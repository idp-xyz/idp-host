package harness

import (
	"context"
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
// rewrite is worse than none), the release's output patterns are applied to the answer, the router learns how each
// attempt went, and the span is made the same way a step's request is. Its use is accounted for in the event: provider, model,
// tokens, how long. Any failure, a refused answer included, returns the original question and says why in the event; the
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
	filter, err := guard.NewOutput(nil, def.Guards.Output.Patterns, def.Guards.Output.Redact)
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
		mctx, endSpan := h.startRewriteSpan(ctx, ref)
		start := now()
		resp, err := p.Complete(mctx, model.Request{Model: ref.Model, Instructions: cmiclient.RewriteInstructions,
			Messages:    []agui.Message{{ID: "rewrite", Role: agui.RoleUser, Content: prompt}},
			Temperature: &zero, MaxOutputTokens: cmiclient.RewriteMaxOutput, Effort: model.EffortLow, Watch: model.NewStopwatch(now)}, func(model.Delta) {})
		took := now().Sub(start)
		if h.Router != nil {
			h.Router.Observe(ref, took, err != nil && ctx.Err() == nil)
		}
		if err != nil {
			endSpan("error", nil)
			if ctx.Err() != nil {
				reason = "timeout"
				break
			}
			continue // the next candidate
		}
		endSpan("ok", resp)
		extra := map[string]any{"provider": ref.Provider, "model": ref.Model, "promptTokens": resp.Usage.PromptTokens, "completionTokens": resp.Usage.CompletionTokens, "latencyMs": took.Milliseconds()}
		if resp.StopReason == model.StopLength {
			return fallback("too_long", extra) // cut by the output limit: not a finished question
		}
		q, why := cmiclient.CleanRewrite(resp.Message.Content)
		if why == "" && filter.Check(q) != "" {
			why = "guard"
		}
		if why != "" {
			return fallback(why, extra)
		}
		if q == in.Question {
			return cmiclient.RewriteOutcome{Query: in.Question, Event: cmiclient.RewriteEvent(cmiclient.RewriteUnchanged, in.Question, in.Question, "", extra)}
		}
		return cmiclient.RewriteOutcome{Query: q, Event: cmiclient.RewriteEvent(cmiclient.RewriteRewritten, in.Question, q, "", extra)}
	}
	return fallback(reason, nil)
}
