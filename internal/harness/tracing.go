package harness

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"idp-host/internal/agent"
	"idp-host/internal/model"
	"idp-host/internal/observability"
	"idp-host/internal/run"
)

// Spans of a run's turn (D57). With Harness.Tracer nil nothing here allocates or records: every function returns at its
// first line, and no request carries a trace context. Attributes are identifiers, counts, timings and outcome words; never a
// prompt, an argument, a result, a token or a subject.

var (
	noTurnEnd  = func(error) {}
	noModelEnd = func(string, *model.Response, model.Timing) {}
)

// startTurn continues the trace of the request that created the run (its stored traceparent, or its trace id for a run
// created before the traceparent was kept) with the span of this turn.
func (h *Harness) startTurn(ctx context.Context, rc *run.Context) (context.Context, func(error)) {
	if h.Tracer == nil {
		return ctx, noTurnEnd
	}
	ctx = observability.ContinueTrace(ctx, rc.Run.Traceparent, rc.Run.TraceID)
	ctx, span := h.Tracer.Start(ctx, "idp.run.turn", trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(attribute.String("idp.run.id", rc.Run.ID), attribute.Int("idp.run.depth", rc.Run.Depth), attribute.Int("idp.run.step_count", rc.Run.StepCount)))
	return ctx, func(err error) {
		outcome := "ok"
		var f *run.Failure
		switch {
		case err == nil:
		case errors.As(err, &f):
			outcome = "failed"
			span.SetAttributes(attribute.String("idp.run.failure_code", f.Code))
			span.SetStatus(codes.Error, f.Code)
		case errors.Is(err, run.ErrFenced):
			outcome = "fenced"
		case errors.Is(err, errStopped):
			outcome = "stopped"
		default:
			outcome = "error"
			span.SetStatus(codes.Error, "error")
		}
		span.SetAttributes(attribute.String("idp.turn.outcome", outcome))
		span.End()
	}
}

// toolTrace is what the gateway client needs to trace this turn's tool calls (observability.StartToolCall).
func (t *turn) toolTrace() *observability.ToolTrace {
	return &observability.ToolTrace{Tracer: t.h.Tracer, RunID: t.claim.RunID, Gateway: t.def.Tools.Gateway, Step: func() int { return t.step },
		External: func(tool string) bool { return t.external[tool] }}
}

// startModelSpan starts the span of one request to a model provider and says, on the context the request is made with,
// whether the provider may be sent the trace context: only a provider the HOST's configuration lists (TraceProviders), which
// by default is a provider declared external: false (D44, D57).
func (t *turn) startModelSpan(ctx context.Context, step, attempt int, ref agent.ModelRef) (context.Context, func(outcome string, resp *model.Response, timing model.Timing)) {
	if t.h.Tracer == nil {
		return ctx, noModelEnd
	}
	ctx, span := t.h.Tracer.Start(ctx, "idp.model.call", trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(
		attribute.String("idp.run.id", t.claim.RunID), attribute.Int("idp.step", step), attribute.Int("idp.model.attempt", attempt),
		attribute.String("idp.model.provider", ref.Provider), attribute.String("idp.model.name", ref.Model)))
	if t.h.TraceProviders[ref.Provider] {
		ctx = observability.WithPropagation(ctx)
	}
	return ctx, func(outcome string, resp *model.Response, timing model.Timing) {
		span.SetAttributes(attribute.String("idp.model.outcome", outcome))
		if outcome != "ok" {
			span.SetStatus(codes.Error, outcome)
		}
		if resp != nil {
			span.SetAttributes(attribute.String("idp.model.stop_reason", resp.StopReason), attribute.Int("idp.model.prompt_tokens", resp.Usage.PromptTokens),
				attribute.Int("idp.model.completion_tokens", resp.Usage.CompletionTokens))
			if resp.Usage.CachedTokens != nil {
				span.SetAttributes(attribute.Int("idp.model.cached_tokens", *resp.Usage.CachedTokens))
			}
		}
		for key, d := range map[string]*time.Duration{"idp.model.first_output_ms": timing.FirstOutput, "idp.model.first_text_ms": timing.FirstText, "idp.model.after_first_ms": timing.AfterFirst} {
			if d != nil {
				span.SetAttributes(attribute.Float64(key, float64(*d)/float64(time.Millisecond)))
			}
		}
		span.End()
	}
}

// startRewriteSpan is startModelSpan for the question rewrite (D66), which has no run yet: the span is a child of the request's.
func (h *Harness) startRewriteSpan(ctx context.Context, ref agent.ModelRef) (context.Context, func(outcome string, resp *model.Response)) {
	if h.Tracer == nil {
		return ctx, func(string, *model.Response) {}
	}
	ctx, span := h.Tracer.Start(ctx, "idp.model.call", trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(
		attribute.String("idp.model.purpose", "context.rewrite"), attribute.String("idp.model.provider", ref.Provider), attribute.String("idp.model.name", ref.Model)))
	if h.TraceProviders[ref.Provider] {
		ctx = observability.WithPropagation(ctx)
	}
	return ctx, func(outcome string, resp *model.Response) {
		span.SetAttributes(attribute.String("idp.model.outcome", outcome))
		if outcome != "ok" {
			span.SetStatus(codes.Error, outcome)
		}
		if resp != nil {
			span.SetAttributes(attribute.Int("idp.model.prompt_tokens", resp.Usage.PromptTokens), attribute.Int("idp.model.completion_tokens", resp.Usage.CompletionTokens))
		}
		span.End()
	}
}
