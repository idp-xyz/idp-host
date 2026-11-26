package cmiclient

import (
	"context"
	"strings"
	"unicode/utf8"

	"idp-host/internal/agent"
	"idp-host/internal/agui"
)

// D66: the question of a thread that has earlier turns is rewritten by the release's own model, as one sentence that does not
// lean on the earlier turns, before the first retrieval. The model call is the harness's managed one (D63-1); this file is
// what the API and the harness agree on: what is asked, how the answer is checked and what the event says.

// Limits of the rewrite. The answer is a query, not an answer: it is short, one line, and checked before it is used.
const (
	RewriteMarker       = "idp.host/context.rewrite" // the first line of the instructions; test doubles recognise the call by it
	RewriteMaxHistory   = 6                          // earlier messages shown to the model, newest last
	RewriteMessageChars = 600                        // each earlier message is cut to this many characters
	RewriteQuestionMax  = 4096                       // the question as it is sent to the CMI
	RewriteQueryMax     = 300                        // the rewritten question, in characters: longer is not a question
	RewriteMaxOutput    = 128                        // output tokens the call may use
)

// RewriteInstructions is what the model is told.
const RewriteInstructions = RewriteMarker + "\n" +
	"You rewrite the last question of a conversation as one self-contained question for a search of company documents. " +
	"Use the earlier messages only to resolve what the last question refers to (\"that one\", \"the second item\", \"what about the other\"). " +
	"Keep the language of the question and every name, number and term in it. Do not answer it and do not add facts. " +
	"If the question already stands on its own, repeat it unchanged. " +
	"Reply with the question only: one line, no quotes, no label, no explanation."

// Rewrite outcomes of the event (D66).
const (
	RewriteRewritten = "rewritten" // the model's question is the query
	RewriteUnchanged = "unchanged" // the model repeated the question: the query is the original
	RewriteFallback  = "fallback"  // the call or its answer failed a check: the query is the original, and reason says why
)

// RewriteInput is what the API hands to the rewriter.
type RewriteInput struct {
	Question string         // the last user message
	History  []agui.Message // the earlier user and assistant messages, oldest first
}

// RewriteOutcome is what the rewriter hands back. Query is never empty: it is the original when the rewrite did not work.
type RewriteOutcome struct {
	Query string
	Event agui.Event // idp.host/context.rewrite, written with the run's other context events
}

// Rewriter makes the managed model call of the rewrite; the harness implements it.
type Rewriter interface {
	RewriteQuestion(ctx context.Context, def agent.Definition, in RewriteInput) RewriteOutcome
}

// PriorTurns splits a request's messages into the last user message (the question) and what came before it: user and assistant
// messages with text, oldest first, the newest RewriteMaxHistory of them. ok is false when there is no question or nothing
// before it, which is when no rewrite is made.
func PriorTurns(msgs []agui.Message) (in RewriteInput, ok bool) {
	last := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == agui.RoleUser && msgs[i].Content != "" {
			last = i
			break
		}
	}
	if last < 0 {
		return RewriteInput{}, false
	}
	for _, m := range msgs[:last] {
		if (m.Role == agui.RoleUser || m.Role == agui.RoleAssistant) && strings.TrimSpace(m.Content) != "" {
			in.History = append(in.History, m)
		}
	}
	if len(in.History) == 0 {
		return RewriteInput{}, false
	}
	if len(in.History) > RewriteMaxHistory {
		in.History = in.History[len(in.History)-RewriteMaxHistory:]
	}
	in.Question = Cut(msgs[last].Content, RewriteQuestionMax)
	return in, true
}

// Prompt is the one user message of the rewrite call: the earlier messages, each cut, then the question.
func (in RewriteInput) Prompt() string {
	var b strings.Builder
	b.WriteString("Earlier messages:\n")
	for _, m := range in.History {
		text := strings.Join(strings.Fields(m.Content), " ")
		if utf8.RuneCountInString(text) > RewriteMessageChars {
			text = string([]rune(text)[:RewriteMessageChars]) + "…"
		}
		b.WriteString(string(m.Role) + ": " + text + "\n")
	}
	b.WriteString("\nLast question:\n" + in.Question)
	return b.String()
}

// CleanRewrite checks the model's answer. It returns the query and "" when the answer is usable, else "" and why: empty, multiline
// or too_long. Nothing is repaired: a model that wrote an explanation, a label or two lines is not guessed at, the original is used.
func CleanRewrite(raw string) (string, string) {
	q := strings.TrimSpace(raw)
	if q == "" {
		return "", "empty"
	}
	if strings.ContainsAny(q, "\r\n") {
		return "", "multiline"
	}
	if utf8.RuneCountInString(q) > RewriteQueryMax {
		return "", "too_long"
	}
	return q, ""
}

// RewriteEvent builds idp.host/context.rewrite. extra is merged in (provider, model, usage, latencyMs).
func RewriteEvent(outcome, original, query, reason string, extra map[string]any) agui.Event {
	v := map[string]any{"mode": agent.RewriteModel, "outcome": outcome, "original": Cut(original, RewriteQuestionMax), "query": query}
	if reason != "" {
		v["reason"] = reason
	}
	for k, x := range extra {
		v[k] = x
	}
	return agui.Custom(agui.CustomContextRewrite, v)
}
