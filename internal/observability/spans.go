package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Spans of the worker (ADR-0001 D57). A run is created by a request and served by a worker, possibly another process and
// later: the request's trace context is stored with the run (the traceparent, and the trace id the run always carried), and
// the worker's turn continues that trace. Spans carry identifiers and outcomes only: never prompts, arguments, results,
// tokens or subjects.

// Traceparent returns the W3C traceparent of the span in ctx, "" when there is none. It is what a run stores of the request
// that created it.
func Traceparent(ctx context.Context) string {
	c := propagation.MapCarrier{}
	Propagator.Inject(ctx, c)
	return c.Get("traceparent")
}

// ContinueTrace returns ctx with the stored trace context of a run as the remote parent of the spans that follow: the
// traceparent when there is one, otherwise (a run created before the traceparent was stored) the trace id with a parent span
// id of its own, so that the spans are at least in the request's trace. Neither valid: ctx unchanged, a new trace begins.
func ContinueTrace(ctx context.Context, traceparent, traceID string) context.Context {
	if traceparent != "" {
		out := Propagator.Extract(ctx, propagation.MapCarrier{"traceparent": traceparent})
		if trace.SpanContextFromContext(out).IsValid() {
			return out
		}
	}
	tid, err := trace.TraceIDFromHex(traceID)
	if err != nil || !tid.IsValid() {
		return ctx
	}
	var sid trace.SpanID
	_, _ = rand.Read(sid[:])
	if !sid.IsValid() {
		sid[7] = 1
	}
	return trace.ContextWithRemoteSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled, Remote: true}))
}

type propagationKey struct{}

// The two policies of sending the trace context out, by who is asked. A request to the enterprise's own gateway carries it
// unless the call is marked otherwise (WithoutPropagation): the gateway is infrastructure of ours, and it enforces the rule
// of idp-mcp D51 that nothing is passed on to an external upstream. A request to a model provider carries it only when
// the caller says so (WithPropagation): a provider outside the enterprise is a third party, and the traceparent identifies
// our request to it (D57).
type propagationMode int

const (
	modeDefault propagationMode = iota
	modeAllow
	modeDeny
)

// WithPropagation marks the requests made with ctx as allowed to carry the trace context to a provider (IfAllowed).
func WithPropagation(ctx context.Context) context.Context {
	return context.WithValue(ctx, propagationKey{}, modeAllow)
}

// WithoutPropagation marks the requests made with ctx as not to carry the trace context (UnlessDenied).
func WithoutPropagation(ctx context.Context) context.Context {
	return context.WithValue(ctx, propagationKey{}, modeDeny)
}

func mode(ctx context.Context) propagationMode {
	m, _ := ctx.Value(propagationKey{}).(propagationMode)
	return m
}

// InjectUnlessDenied adds the trace context of ctx to h unless the call was marked WithoutPropagation.
func InjectUnlessDenied(ctx context.Context, h http.Header) {
	if mode(ctx) != modeDeny {
		Propagator.Inject(ctx, propagation.HeaderCarrier(h))
	}
}

// InjectIfAllowed adds the trace context of ctx to h only when the call was marked WithPropagation.
func InjectIfAllowed(ctx context.Context, h http.Header) {
	if mode(ctx) == modeAllow {
		Propagator.Inject(ctx, propagation.HeaderCarrier(h))
	}
}

// ToolTrace says how the tool calls made with a context are traced (D57). The harness puts it on the context of a run's
// turn; the gateway client starts the span of each tools/call from it. Without one, calls are not traced and nothing
// is allocated for them.
type ToolTrace struct {
	Tracer   trace.Tracer
	RunID    string
	Gateway  string
	Step     func() int             // the step in progress
	External func(tool string) bool // the tool sends content outside the enterprise: the call does not carry the trace context
}

type toolTraceKey struct{}

// WithToolTrace returns ctx carrying tt.
func WithToolTrace(ctx context.Context, tt *ToolTrace) context.Context {
	return context.WithValue(ctx, toolTraceKey{}, tt)
}

func noToolEnd(string) {}

// StartToolCall starts the span of one gateway tool call, a child of the span in ctx, and returns the context to make the
// request with and the function that ends the span with its outcome (ok, tool_error, unauthorized, rpc_error, interrupted
// or error). The span has the tool's name and no arguments or result. A call to a tool the host knows to be external is
// made without the trace context.
func StartToolCall(ctx context.Context, tool string) (context.Context, func(outcome string)) {
	tt, _ := ctx.Value(toolTraceKey{}).(*ToolTrace)
	if tt == nil || tt.Tracer == nil {
		return ctx, noToolEnd
	}
	external := tt.External != nil && tt.External(tool)
	attrs := []attribute.KeyValue{attribute.String("idp.run.id", tt.RunID), attribute.String("idp.tool.name", tool),
		attribute.String("idp.tool.gateway", tt.Gateway), attribute.Bool("idp.tool.external", external)}
	if tt.Step != nil {
		attrs = append(attrs, attribute.Int("idp.step", tt.Step()))
	}
	ctx, span := tt.Tracer.Start(ctx, "idp.tool.call", trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	if external {
		ctx = WithoutPropagation(ctx)
	}
	return ctx, func(outcome string) {
		span.SetAttributes(attribute.String("idp.tool.outcome", outcome))
		if outcome != "ok" {
			span.SetStatus(codes.Error, outcome)
		}
		span.End()
	}
}

// Exporting reports whether the environment names a trace collector (the standard OTEL_EXPORTER_OTLP_* variables). Spans
// of the worker are made only then (D57): without a collector nobody reads them.
func Exporting() bool { return ExporterFromEnv(nil) != nil }

// RandomSpanID is for tests that need a span id.
func RandomSpanID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
