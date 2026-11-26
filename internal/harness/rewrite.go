package harness

import (
	"context"
	"errors"
	"time"

	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/cmiclient"
	"idp-host/internal/guard"
	"idp-host/internal/model"
)

// DefaultRewriteTimeout bounds the whole rewrite call, every candidate included (D66).
const DefaultRewriteTimeout = 8 * time.Second

// RewriteQuestion is the managed model call of the question rewrite (D66, D63-1(b)): the release's own model profile decides
// who answers (the router orders the candidates as for a step), the call has an output limit of cmiclient.RewriteMaxOutput
// tokens, a low thinking effort and Harness.RewriteTimeout for everything, each candidate is tried once (no retry: a slow
// rewrite is worse than none), the tenant's and the release's output patterns are applied to the answer, the span is made the same
// way a step's request is, and the call is counted by the step metric. The router is not told how it went: its numbers order the
// candidates for a run's steps. Its use is in the event (provider, model, tokens, how long) and in Usage, which the API records
// as step 0 of the run, so that the tenant's budget counts it. Any failure, a refused answer included, returns the original question and says why in the event; the
// rewrite never fails a run. Safe for concurrent use: it changes nothing of the harness.
func (h *Harness) RewriteQuestion(ctx context.Context, def agent.Definition, in cmiclient.RewriteInput) cmiclient.RewriteOutcome {
	now := h.Now
	if now == nil {
		now = time.Now
	}
	fallback := func(reason string, extra map[string]any) cmiclient.RewriteOutcome {
		return cmiclient.RewriteOutcome{Query: in.Question, Event: cmiclient.RewriteEvent(cmiclient.RewriteFallback, in.Question, in.Question, reason, extra)}
	}
	timeout := h.RewriteTimeout
	if timeout <= 0 {
		timeout = DefaultRewriteTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	prompt := in.Prompt()
	needs := model.Needs{EstimatedInput: model.EstimateTokens(cmiclient.RewriteInstructions, prompt), MaxOutputTokens: cmiclient.RewriteMaxOutput}
	var refs []agent.ModelRef
	if h.Router != nil {
		refs, _ = h.Router.Order(def.Model, needs)
	} else {
		refs = append([]agent.ModelRef{{Provider: def.Model.Provider, Model: def.Model.Model}}, def.Model.Fallbacks...)
	}
	if len(refs) == 0 || h.Providers == nil {
		return fallback("no_candidate", nil)
	}
	asked := 0
	var spent model.Usage                                                                                    // what the candidates that failed after reporting usage used: the run is charged for it too
	filter, err := guard.NewOutput(in.BlockedPatterns, def.Guards.Output.Patterns, def.Guards.Output.Redact) // D28: the tenant's patterns and the release's
	if err != nil {
		filter = nil
	}
	zero := 0.0
	reason := "error"
	for _, ref := range refs {
		p, ok := h.Providers.Get(ref.Provider)
		if !ok {
			continue
		}
		asked++
		mctx, endSpan := h.startRewriteSpan(ctx, ref)
		start := now()
		resp, err := p.Complete(mctx, model.Request{Model: ref.Model, Instructions: cmiclient.RewriteInstructions,
			Messages:    []agui.Message{{ID: "rewrite", Role: agui.RoleUser, Content: prompt}},
			Temperature: &zero, MaxOutputTokens: cmiclient.RewriteMaxOutput, Effort: model.EffortLow, Watch: model.NewStopwatch(now)}, func(model.Delta) {})
		took := now().Sub(start)
		// the router is not told how long a rewrite took or whether it failed: its numbers order the candidates for a run's steps,
		// and a 128-token call says nothing about them
		if err != nil {
			if resp != nil {
				spent.PromptTokens += resp.Usage.PromptTokens
				spent.CompletionTokens += resp.Usage.CompletionTokens
			}
			endSpan("error", nil)
			if h.Metrics != nil {
				h.Metrics.StepFinished(ref.Provider, ref.Model, "rewrite_error", took, model.Usage{})
			}
			switch {
			case errors.Is(ctx.Err(), context.DeadlineExceeded):
				reason = "timeout"
			case ctx.Err() != nil:
				reason = "canceled" // the caller went away
			default:
				continue // the next candidate
			}
			break
		}
		endSpan("ok", resp)
		if h.Metrics != nil {
			h.Metrics.StepFinished(ref.Provider, ref.Model, "rewrite", took, resp.Usage)
		}
		usage := &cmiclient.RewriteUsage{Provider: ref.Provider, Model: ref.Model, PromptTokens: spent.PromptTokens + resp.Usage.PromptTokens, CompletionTokens: spent.CompletionTokens + resp.Usage.CompletionTokens, LatencyMs: took.Milliseconds()}
		extra := map[string]any{"provider": ref.Provider, "model": ref.Model, "promptTokens": resp.Usage.PromptTokens, "completionTokens": resp.Usage.CompletionTokens, "latencyMs": took.Milliseconds()}
		withUsage := func(o cmiclient.RewriteOutcome) cmiclient.RewriteOutcome { o.Usage = usage; return o }
		if resp.StopReason == model.StopLength {
			return withUsage(fallback("too_long", extra)) // cut by the output limit: not a finished question
		}
		q, why := cmiclient.CleanRewrite(resp.Message.Content)
		if why == "" && filter.Check(q) != "" {
			why = "guard"
		}
		if why != "" {
			return withUsage(fallback(why, extra))
		}
		if q == in.Question {
			return withUsage(cmiclient.RewriteOutcome{Query: in.Question, Event: cmiclient.RewriteEvent(cmiclient.RewriteUnchanged, in.Question, in.Question, "", extra)})
		}
		return withUsage(cmiclient.RewriteOutcome{Query: q, Event: cmiclient.RewriteEvent(cmiclient.RewriteRewritten, in.Question, q, "", extra)})
	}
	if asked == 0 {
		reason = "no_candidate" // no candidate of the profile is a provider this HOST has
	}
	out := fallback(reason, nil)
	if spent.PromptTokens+spent.CompletionTokens > 0 {
		out.Usage = &cmiclient.RewriteUsage{Provider: def.Model.Provider, Model: def.Model.Model, PromptTokens: spent.PromptTokens, CompletionTokens: spent.CompletionTokens}
	}
	return out
}
