package observability

import (
	"context"
	"net/http"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

const (
	tid = "0af7651916cd43dd8448eb211c80319c"
	sid = "b7ad6b7169203331"
)

// D57: the stored traceparent is the remote parent of the worker's spans; a trace id alone puts them in the same trace; a
// value that is not a traceparent is ignored.
func TestContinueTrace(t *testing.T) {
	ctx := ContinueTrace(context.Background(), "00-"+tid+"-"+sid+"-01", "")
	sc := trace.SpanContextFromContext(ctx)
	if sc.TraceID().String() != tid || sc.SpanID().String() != sid || !sc.IsRemote() || !sc.IsSampled() {
		t.Errorf("%+v", sc)
	}
	ctx = ContinueTrace(context.Background(), "", tid)
	if sc := trace.SpanContextFromContext(ctx); sc.TraceID().String() != tid || !sc.SpanID().IsValid() {
		t.Errorf("trace id only: %+v", sc)
	}
	for _, bad := range [][2]string{{"", ""}, {"garbage", ""}, {"garbage", "nothex"}, {"", "00000000000000000000000000000000"}} {
		if sc := trace.SpanContextFromContext(ContinueTrace(context.Background(), bad[0], bad[1])); sc.IsValid() {
			t.Errorf("%q accepted: %+v", bad, sc)
		}
	}
	if got := Traceparent(ContinueTrace(context.Background(), "00-"+tid+"-"+sid+"-01", "")); got != "00-"+tid+"-"+sid+"-01" {
		t.Errorf("round trip %q", got)
	}
	if Traceparent(context.Background()) != "" {
		t.Errorf("a context without a span has a traceparent")
	}
}

// The two policies: the gateway gets the context unless the call is marked, a provider only when the call is marked.
func TestInjectPolicies(t *testing.T) {
	ctx := ContinueTrace(context.Background(), "00-"+tid+"-"+sid+"-01", "")
	for _, c := range []struct {
		name         string
		ctx          context.Context
		gateway, llm bool
	}{
		{"unmarked", ctx, true, false},
		{"allowed", WithPropagation(ctx), true, true},
		{"denied", WithoutPropagation(ctx), false, false},
		{"allowed then denied", WithoutPropagation(WithPropagation(ctx)), false, false},
	} {
		g, p := http.Header{}, http.Header{}
		InjectUnlessDenied(c.ctx, g)
		InjectIfAllowed(c.ctx, p)
		if (g.Get("Traceparent") != "") != c.gateway || (p.Get("Traceparent") != "") != c.llm {
			t.Errorf("%s: gateway %q provider %q", c.name, g.Get("Traceparent"), p.Get("Traceparent"))
		}
	}
	// no span: nothing to send under any policy
	g := http.Header{}
	InjectUnlessDenied(context.Background(), g)
	InjectIfAllowed(WithPropagation(context.Background()), g)
	if len(g) != 0 {
		t.Errorf("%v", g)
	}
}

func TestStartToolCall(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tr := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)).Tracer("t")
	step := 3
	tt := &ToolTrace{Tracer: tr, RunID: "run_1", Gateway: "edge", Step: func() int { return step }, External: func(tool string) bool { return tool == "web" }}
	base := WithToolTrace(ContinueTrace(context.Background(), "00-"+tid+"-"+sid+"-01", ""), tt)
	for _, tool := range []string{"sum", "web"} {
		ctx, end := StartToolCall(base, tool)
		h := http.Header{}
		InjectUnlessDenied(ctx, h)
		if got := h.Get("Traceparent") != ""; got == (tool == "web") {
			t.Errorf("%s: header %q", tool, h.Get("Traceparent"))
		}
		end("tool_error")
	}
	spans := exp.GetSpans()
	if len(spans) != 2 || spans[0].Status.Description != "tool_error" || spans[1].Parent.SpanID().String() != sid {
		t.Errorf("%+v", spans)
	}
}
