package harness

import (
	"fmt"
	"math"

	"idp-host/internal/agent"
	"idp-host/internal/model"
)

// The output limit of a request and the answer that does not end (ADR-0001 D60).

// outputSlack is the margin the model server keeps besides the output limit (Strata rejects prompt + max_tokens + 8 > context).
const outputSlack = 8

// runawayFloor is the smallest limit the window's clamp leaves (D52 reserves the same 256 tokens for the answer).
const runawayFloor = 256

// outLimit is the output limit of one request and what set it.
type outLimit struct {
	tokens  int    // sent as the provider's max_tokens
	source  string // "release", "operator", "default" or "window"
	clamped bool   // what is left of the window is smaller than the limit would have been: the window sets it
	window  int    // the candidate's context window from the host's configuration, 0 when it says nothing
}

// outputLimit is the limit of a request to ref: the release's or the run's value when lower, otherwise the operator's entry
// for the model, otherwise the default (model.OutputPolicy.Limit); then never more than what the window leaves after the
// request, by the corrected estimate with D52's 5 percent margin. A model whose window the host does not know gets only an
// explicit limit (the release's, the run's or the operator's), unclamped; with none, no limit is sent (tokens 0), as before D60.
func (t *turn) outputLimit(ref agent.ModelRef) outLimit {
	l := outLimit{window: t.window(ref)}
	l.tokens, l.source = t.h.ModelOutput[ref.Provider+"/"+ref.Model].Limit(t.def.Model.MaxOutputTokens)
	if l.window == 0 && l.source == "default" {
		// the window is not known: nothing says the default fits it, and a server that refuses prompt + max_tokens beyond its
		// context (Strata does) would fail conversations that used to finish. Only an explicit limit is sent.
		l.tokens = 0
		return l
	}
	if l.window > 0 {
		room := max(l.window-int(math.Ceil(t.viewFixed*1.05))-outputSlack, runawayFloor)
		if room < l.tokens {
			l.tokens, l.source, l.clamped = room, "window", true
		}
	}
	return l
}

// cutIsRunaway decides what a finish reason length means (D60). It is D40's case, the window, when the limit was what the
// window left; when the prompt and the limit together came within a sixteenth of the window; or when the server stopped
// short of the limit it was given (its own context was full, not the limit: fewer than nine tenths of it generated). It is a
// runaway when the answer reached the limit with the window to spare, or, where the server did not say how many tokens it
// generated, when the window is known and was not nearly full.
func (t *turn) cutIsRunaway(l outLimit, u model.Usage) bool {
	switch {
	case l.tokens == 0:
		return false // no limit was sent: a cut is the server's own (D40)
	case l.clamped:
		return false
	case l.window > 0 && u.PromptTokens+l.tokens > l.window-l.window/16:
		return false
	case u.CompletionTokens > 0:
		return u.CompletionTokens*10 >= l.tokens*9
	}
	return l.window > 0
}

// runawayNote is what the failure message of an answer that did not end says.
type runawayNote struct {
	limit    outLimit
	tokens   int  // what the last attempt generated, 0 when the server did not say
	bytes    bool // the host's bound of the stream stopped it, not the output limit
	attempts int
}

func (n runawayNote) message() string {
	what := fmt.Sprintf("it reached the output limit of %d tokens (set by the %s) with room left in the context window", n.limit.tokens, n.limit.source)
	if n.bytes {
		what = fmt.Sprintf("it went on past the %d MiB this host reads from one answer", model.MaxResponse>>20)
	}
	return fmt.Sprintf("model call failed: %s: the model's answer did not end: %s (%d request(s) in the step); the answer was dropped and none of its tool calls was run", model.CodeOutputRunaway, what, n.attempts)
}
