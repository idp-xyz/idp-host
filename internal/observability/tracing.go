package observability

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Tracing (M4): the OpenTelemetry SDK with a small OTLP/HTTP JSON exporter of our own. The official
// OTLP exporters pull in gRPC and protobuf; OTLP defines a JSON encoding over HTTP that collectors
// accept, so a few dozen lines keep the supply chain and the scratch image small.
//
// Standard environment: OTEL_EXPORTER_OTLP_TRACES_ENDPOINT (full URL) or OTEL_EXPORTER_OTLP_ENDPOINT
// (base; /v1/traces appended), OTEL_EXPORTER_OTLP_HEADERS ("k=v,k2=v2"), OTEL_EXPORTER_OTLP_CERTIFICATE
// (CA file), OTEL_SERVICE_NAME. Without an endpoint spans are still created - trace ids correlate
// audit records and logs - but nothing is exported. Spans never carry arguments, tokens or subjects.

// OTLPExporter exports spans as OTLP/HTTP JSON.
type OTLPExporter struct {
	Endpoint string
	Headers  map[string]string
	HTTP     *http.Client
}

type kv struct {
	Key   string         `json:"key"`
	Value map[string]any `json:"value"`
}

func anyValue(v attribute.Value) map[string]any {
	switch v.Type() {
	case attribute.BOOL:
		return map[string]any{"boolValue": v.AsBool()}
	case attribute.INT64:
		return map[string]any{"intValue": strconv.FormatInt(v.AsInt64(), 10)} // int64 is a string in OTLP JSON
	case attribute.FLOAT64:
		return map[string]any{"doubleValue": v.AsFloat64()}
	case attribute.STRINGSLICE:
		var vals []map[string]any
		for _, s := range v.AsStringSlice() {
			vals = append(vals, map[string]any{"stringValue": s})
		}
		return map[string]any{"arrayValue": map[string]any{"values": vals}}
	}
	return map[string]any{"stringValue": v.Emit()}
}

func kvs(attrs []attribute.KeyValue) []kv {
	out := make([]kv, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, kv{Key: string(a.Key), Value: anyValue(a.Value)})
	}
	return out
}

// OTLP status codes differ from the Go API's (Unset 0, Error 1, Ok 2).
func statusCode(c codes.Code) int {
	switch c {
	case codes.Ok:
		return 1
	case codes.Error:
		return 2
	}
	return 0
}

func nanos(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }

// Encode renders spans as an OTLP ExportTraceServiceRequest in JSON.
func Encode(spans []sdktrace.ReadOnlySpan) ([]byte, error) {
	type scopeKey struct{ name, version string }
	resources := map[string]map[scopeKey][]map[string]any{}
	resourceAttrs := map[string][]kv{}
	var order []string
	for _, s := range spans {
		rk := s.Resource().Encoded(attribute.DefaultEncoder())
		if _, ok := resources[rk]; !ok {
			resources[rk] = map[scopeKey][]map[string]any{}
			resourceAttrs[rk] = kvs(s.Resource().Attributes())
			order = append(order, rk)
		}
		sc := s.InstrumentationScope()
		tid, sid := s.SpanContext().TraceID(), s.SpanContext().SpanID()
		sp := map[string]any{
			"traceId":           hex.EncodeToString(tid[:]),
			"spanId":            hex.EncodeToString(sid[:]),
			"name":              s.Name(),
			"kind":              int(s.SpanKind()), // same numbering as OTLP
			"startTimeUnixNano": nanos(s.StartTime()),
			"endTimeUnixNano":   nanos(s.EndTime()),
			"attributes":        kvs(s.Attributes()),
			"status":            map[string]any{"code": statusCode(s.Status().Code), "message": s.Status().Description},
		}
		if s.Parent().IsValid() {
			pid := s.Parent().SpanID()
			sp["parentSpanId"] = hex.EncodeToString(pid[:])
		}
		if ts := s.SpanContext().TraceState().String(); ts != "" {
			sp["traceState"] = ts
		}
		var events []map[string]any
		for _, e := range s.Events() {
			events = append(events, map[string]any{"name": e.Name, "timeUnixNano": nanos(e.Time), "attributes": kvs(e.Attributes)})
		}
		if events != nil {
			sp["events"] = events
		}
		key := scopeKey{sc.Name, sc.Version}
		resources[rk][key] = append(resources[rk][key], sp)
	}
	var rs []map[string]any
	for _, rk := range order {
		var scopes []map[string]any
		for k, sps := range resources[rk] {
			scopes = append(scopes, map[string]any{"scope": map[string]any{"name": k.name, "version": k.version}, "spans": sps})
		}
		rs = append(rs, map[string]any{"resource": map[string]any{"attributes": resourceAttrs[rk]}, "scopeSpans": scopes})
	}
	return json.Marshal(map[string]any{"resourceSpans": rs})
}

// ExportSpans implements sdktrace.SpanExporter.
func (e *OTLPExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if len(spans) == 0 {
		return nil
	}
	body, err := Encode(spans)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range e.Headers {
		req.Header.Set(k, v)
	}
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("OTLP export: HTTP %d", resp.StatusCode)
	}
	return nil
}

// Shutdown implements sdktrace.SpanExporter.
func (e *OTLPExporter) Shutdown(context.Context) error { return nil }

// ExporterFromEnv builds the exporter from the standard environment, or nil when no endpoint is set.
func ExporterFromEnv(httpClient *http.Client) *OTLPExporter {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
	if endpoint == "" {
		if base := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); base != "" {
			endpoint = strings.TrimSuffix(base, "/") + "/v1/traces"
		}
	}
	if endpoint == "" {
		return nil
	}
	headers := map[string]string{}
	for _, pair := range strings.Split(os.Getenv("OTEL_EXPORTER_OTLP_HEADERS"), ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(pair), "="); ok && k != "" {
			headers[k] = v
		}
	}
	return &OTLPExporter{Endpoint: endpoint, Headers: headers, HTTP: httpClient}
}

// NewTracerProvider returns an SDK tracer provider for service (OTEL_SERVICE_NAME overrides), batching
// to exp when it is not nil.
func NewTracerProvider(service, version string, exp sdktrace.SpanExporter) *sdktrace.TracerProvider {
	if v := os.Getenv("OTEL_SERVICE_NAME"); v != "" {
		service = v
	}
	res := sdkresource.NewSchemaless(attribute.String("service.name", service), attribute.String("service.version", version))
	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res), sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample()))}
	if exp != nil {
		opts = append(opts, sdktrace.WithBatcher(exp, sdktrace.WithBatchTimeout(2*time.Second)))
	}
	return sdktrace.NewTracerProvider(opts...)
}

// Propagator is W3C Trace Context (traceparent, tracestate).
var Propagator = propagation.TraceContext{}

// Install makes tp the global provider and W3C Trace Context the global propagator.
func Install(tp *sdktrace.TracerProvider) {
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(Propagator)
}

// TraceID returns the hex trace id of the span in ctx ("" if none).
func TraceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.TraceID().IsValid() {
		return ""
	}
	return sc.TraceID().String()
}

// Setup builds the tracer provider for a binary from the environment and installs it globally. The
// returned function flushes and shuts the provider down. httpClient is used for OTLP export
// (OTEL_EXPORTER_OTLP_CERTIFICATE adds a CA for an https collector).
func Setup(service, version string, log *slog.Logger) (*sdktrace.TracerProvider, func(context.Context) error, error) {
	tlsCfg, err := clientTLSFromEnv()
	if err != nil {
		return nil, nil, err
	}
	exp := ExporterFromEnv(&http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil, TLSClientConfig: tlsCfg}})
	tp := NewTracerProvider(service, version, exporterOrNil(exp))
	Install(tp)
	if exp != nil {
		log.Info("tracing: OTLP/HTTP export enabled", "endpoint", exp.Endpoint)
	}
	return tp, tp.Shutdown, nil
}

func exporterOrNil(e *OTLPExporter) sdktrace.SpanExporter {
	if e == nil {
		return nil
	}
	return e
}

func clientTLSFromEnv() (*tls.Config, error) {
	ca := os.Getenv("OTEL_EXPORTER_OTLP_CERTIFICATE")
	if ca == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(ca)
	if err != nil {
		return nil, fmt.Errorf("OTEL_EXPORTER_OTLP_CERTIFICATE: %w", err)
	}
	roots, _ := x509.SystemCertPool()
	if roots == nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("OTEL_EXPORTER_OTLP_CERTIFICATE: no PEM certificates in %s", ca)
	}
	return &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, nil
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) { w.status = code; w.ResponseWriter.WriteHeader(code) }

// Unwrap lets http.ResponseController reach the underlying writer (flushing, hijacking).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// HTTPMiddleware starts a server span per request, named after the matched route pattern once the
// mux has resolved it, and records the response status. Incoming trace context is continued.
func HTTPMiddleware(tp trace.TracerProvider, scope string, next http.Handler) http.Handler {
	tracer := tp.Tracer(scope)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := Propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := tracer.Start(ctx, r.Method+" "+r.URL.Path, trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attribute.String("http.request.method", r.Method)))
		defer span.End()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		r = r.WithContext(ctx)
		next.ServeHTTP(sw, r)
		if r.Pattern != "" {
			span.SetName(r.Pattern)
		}
		span.SetAttributes(attribute.Int("http.response.status_code", sw.status))
		if sw.status >= 500 {
			span.SetStatus(codes.Error, http.StatusText(sw.status))
		}
	})
}
