package observability

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func recorded(t *testing.T) ([]sdktrace.ReadOnlySpan, trace.SpanContext, trace.SpanContext) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp),
		sdktrace.WithResource(sdkresource.NewSchemaless(attribute.String("service.name", "svc"))))
	tr := tp.Tracer("idp-host/test", trace.WithInstrumentationVersion("9"))
	ctx, parent := tr.Start(context.Background(), "mcp tools/call", trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(attribute.String("mcp.method", "tools/call"), attribute.Int("n", 7), attribute.Bool("ok", true),
			attribute.StringSlice("roles", []string{"a", "b"})))
	_, child := tr.Start(ctx, "upstream tools/call", trace.WithSpanKind(trace.SpanKindClient))
	child.SetStatus(codes.Error, "UNKNOWN")
	child.AddEvent("retry", trace.WithAttributes(attribute.Int("attempt", 2)))
	child.End()
	parent.End()
	stubs := exp.GetSpans()
	var out []sdktrace.ReadOnlySpan
	for _, s := range stubs {
		out = append(out, s.Snapshot())
	}
	return out, parent.SpanContext(), child.SpanContext()
}

func TestEncodeProducesOTLPJSON(t *testing.T) {
	spans, parent, child := recorded(t)
	b, err := Encode(spans)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		ResourceSpans []struct {
			Resource   struct{ Attributes []kv }
			ScopeSpans []struct {
				Scope struct{ Name, Version string }
				Spans []map[string]any
			}
		}
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.ResourceSpans) != 1 || doc.ResourceSpans[0].Resource.Attributes[0].Key != "service.name" {
		t.Fatalf("resource: %s", b)
	}
	ss := doc.ResourceSpans[0].ScopeSpans
	if len(ss) != 1 || ss[0].Scope.Name != "idp-host/test" || ss[0].Scope.Version != "9" || len(ss[0].Spans) != 2 {
		t.Fatalf("scope: %s", b)
	}
	byName := map[string]map[string]any{}
	for _, sp := range ss[0].Spans {
		byName[sp["name"].(string)] = sp
	}
	p, c := byName["mcp tools/call"], byName["upstream tools/call"]
	if p["traceId"] != parent.TraceID().String() || p["spanId"] != parent.SpanID().String() || c["parentSpanId"] != parent.SpanID().String() || c["spanId"] != child.SpanID().String() {
		t.Fatalf("ids: parent %v child %v", p, c)
	}
	if p["kind"].(float64) != 2 || c["kind"].(float64) != 3 { // OTLP: SERVER=2, CLIENT=3
		t.Fatalf("kinds %v %v", p["kind"], c["kind"])
	}
	if c["status"].(map[string]any)["code"].(float64) != 2 || p["status"].(map[string]any)["code"].(float64) != 0 {
		t.Fatalf("status %v %v", c["status"], p["status"])
	}
	attrs := map[string]any{}
	for _, a := range p["attributes"].([]any) {
		m := a.(map[string]any)
		attrs[m["key"].(string)] = m["value"]
	}
	if attrs["n"].(map[string]any)["intValue"] != "7" || attrs["ok"].(map[string]any)["boolValue"] != true ||
		attrs["mcp.method"].(map[string]any)["stringValue"] != "tools/call" ||
		len(attrs["roles"].(map[string]any)["arrayValue"].(map[string]any)["values"].([]any)) != 2 {
		t.Fatalf("attributes %v", attrs)
	}
	if ev := c["events"].([]any)[0].(map[string]any); ev["name"] != "retry" || !strings.HasSuffix(ev["timeUnixNano"].(string), "") || c["startTimeUnixNano"].(string) == "" {
		t.Fatalf("events %v", c["events"])
	}
}

func TestOTLPExporterPostsJSONWithHeaders(t *testing.T) {
	var got []byte
	var auth, ct string
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		auth, ct = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		w.WriteHeader(status)
	}))
	defer srv.Close()
	spans, _, _ := recorded(t)
	e := &OTLPExporter{Endpoint: srv.URL + "/v1/traces", Headers: map[string]string{"Authorization": "Bearer t"}, HTTP: srv.Client()}
	if err := e.ExportSpans(context.Background(), spans); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer t" || ct != "application/json" || !strings.Contains(string(got), `"resourceSpans"`) {
		t.Fatalf("%s %s %.80s", auth, ct, got)
	}
	status = 500
	if err := e.ExportSpans(context.Background(), spans); err == nil {
		t.Fatal("5xx not reported")
	}
	got = nil
	if err := e.ExportSpans(context.Background(), nil); err != nil || got != nil {
		t.Fatal("empty batch must not be sent")
	}
}

func TestExporterFromEnv(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	if ExporterFromEnv(http.DefaultClient) != nil {
		t.Fatal("exporter without an endpoint")
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318/")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "x-key=abc, x-other=d=e")
	e := ExporterFromEnv(http.DefaultClient)
	if e.Endpoint != "http://collector:4318/v1/traces" || e.Headers["x-key"] != "abc" || e.Headers["x-other"] != "d=e" {
		t.Fatalf("%+v", e)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "https://c/custom")
	if ExporterFromEnv(http.DefaultClient).Endpoint != "https://c/custom" {
		t.Fatal("traces endpoint must win")
	}
}

func TestTraceIDAndPropagation(t *testing.T) {
	tp := NewTracerProvider("svc", "1", nil)
	ctx, span := tp.Tracer("x").Start(context.Background(), "s")
	defer span.End()
	if TraceID(ctx) != span.SpanContext().TraceID().String() || TraceID(context.Background()) != "" {
		t.Fatal(TraceID(ctx))
	}
	h := http.Header{}
	Propagator.Inject(ctx, propagation.HeaderCarrier(h))
	tp2 := h.Get("traceparent")
	if !strings.HasPrefix(tp2, "00-"+TraceID(ctx)+"-"+span.SpanContext().SpanID().String()+"-01") {
		t.Fatal(tp2)
	}
	back := Propagator.Extract(context.Background(), propagation.HeaderCarrier(h))
	if TraceID(back) != TraceID(ctx) {
		t.Fatal("round trip")
	}
	bad := http.Header{"Traceparent": {"00-00000000000000000000000000000000-b7ad6b7169203331-01"}, "Tracestate": {"v=1"}}
	if TraceID(Propagator.Extract(context.Background(), propagation.HeaderCarrier(bad))) != "" {
		t.Fatal("invalid traceparent accepted")
	}
}

func TestHTTPMiddlewareNamesSpansAfterTheRouteAndRecordsStatus(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/servers/{id}", func(w http.ResponseWriter, r *http.Request) {
		if TraceID(r.Context()) == "" {
			t.Error("handler has no trace context")
		}
		w.WriteHeader(http.StatusTeapot)
	})
	mux.HandleFunc("GET /boom", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	srv := httptest.NewServer(HTTPMiddleware(tp, "test", mux))
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/servers/abc", nil)
	req.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	resp, err := srv.Client().Do(req)
	if err != nil || resp.StatusCode != http.StatusTeapot {
		t.Fatal(err, resp.StatusCode)
	}
	resp2, _ := srv.Client().Get(srv.URL + "/boom")
	resp2.Body.Close()
	spans := exp.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("%d spans", len(spans))
	}
	byName := map[string]tracetest.SpanStub{}
	for _, s := range spans {
		byName[s.Name] = s
	}
	s, ok := byName["GET /api/v1/servers/{id}"]
	if !ok || s.SpanContext.TraceID().String() != "0af7651916cd43dd8448eb211c80319c" || s.Parent.SpanID().String() != "b7ad6b7169203331" {
		t.Fatalf("route span: %v", spans)
	}
	if byName["GET /boom"].Status.Code != codes.Error {
		t.Fatalf("5xx not marked: %v", byName["GET /boom"].Status)
	}
}
