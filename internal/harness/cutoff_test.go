package harness

// Tests of D55: an answer cut off by the output limit that carries tool calls never reaches a tool.

import (
	"context"
	"strings"
	"testing"

	"idp-host/internal/agui"
	"idp-host/internal/model"
	"idp-host/internal/run"
	"idp-host/tests/fixtures/fakemodel"
)

// danglingCalls lists the tool calls of assistant messages in the thread that no tool message answers: the next request
// of a provider with such a thread is refused (OpenAI: "tool_call_id ... did not have response messages", Anthropic: a
// tool_use without its tool_result).
func danglingCalls(msgs []agui.Message) []string {
	answered := map[string]bool{}
	for _, m := range msgs {
		if m.Role == agui.RoleTool {
			answered[m.ToolCallID] = true
		}
	}
	var out []string
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			if !answered[c.ID] {
				out = append(out, c.ID)
			}
		}
	}
	return out
}

func toolCallEvents(m *memStore) int {
	n := 0
	for _, ty := range m.types() {
		if strings.HasPrefix(ty, "TOOL_CALL") {
			n++
		}
	}
	return n
}

// The model's answer is cut off while it writes a call to "sum". Whether the cut arguments are invalid JSON or happen to be
// a complete object, the tool is not called, no call is stored or streamed, the thread is valid for the next request, the
// run ends as D40 says, and the event says why.
func TestACutOffAnswerWithToolCallsRunsNoTool(t *testing.T) {
	for _, c := range []struct{ name, text, args string }{
		{"arguments cut in a string", "", `{"a":1,"b":"hel`},
		{"arguments that parse", "", `{"a":1,"b":2}`},
		{"text before the call", "Let me add them.", `{"a":1,"b":2}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t)
			e.model.Script = func([]fakemodel.Message, []string) fakemodel.Turn {
				return fakemodel.Turn{Text: c.text, ToolCalls: []fakemodel.Call{{Name: "sum", Args: c.args}}, CutOff: true}
			}
			m := newMem(def(), user("add 1 and 2"), run.Input{})
			e.h.Store = m
			err := e.h.Run(context.Background(), claim)
			var f *run.Failure
			if m.status != run.StatusFailed || m.failCode != run.FailModel {
				t.Fatalf("status %s %s, err %v", m.status, m.failCode, err)
			}
			_ = f
			if n := len(e.mcp.Calls()); n != 0 {
				t.Errorf("%d call(s) reached the gateway: %v", n, e.mcp.Calls())
			}
			if len(m.calls) != 0 || toolCallEvents(m) != 0 {
				t.Errorf("%d call record(s) and %d TOOL_CALL event(s) of a cut-off answer: %v", len(m.calls), toolCallEvents(m), m.types())
			}
			if d := danglingCalls(m.rc.Messages); len(d) != 0 {
				t.Errorf("the stored thread has tool calls nobody answers: %v", d)
			}
			if got := e.model.Calls(); got != 1 {
				t.Errorf("%d model calls", got)
			}
			ev := eventNamed(m, agui.CustomModelCutOff)
			if len(ev) != 1 || ev[0]["toolCalls"] != 1 || ev[0]["finishReason"] != "length" || ev[0]["step"] != 1 {
				t.Errorf("cut-off event %v", ev)
			}
			if c.text == "" {
				if len(m.rc.Messages) != 1 {
					t.Errorf("nothing was said, and the thread has %d messages: %+v", len(m.rc.Messages), m.rc.Messages)
				}
			} else {
				if last := m.last(); last.Role != agui.RoleAssistant || last.Content != c.text || len(last.ToolCalls) != 0 {
					t.Errorf("the text that was said is kept: %+v", last)
				}
			}
			textWellFormed(t, m)
		})
	}
}

// The harness does not rely on the adapter's classification alone: any provider that answers with stop reason length and
// tool calls gets the same treatment.
func TestACutOffAnswerFromAnyProviderRunsNoTool(t *testing.T) {
	e := setup(t)
	e.h.Providers = model.NewRegistry(stub{resp: &model.Response{StopReason: model.StopLength, Message: agui.Message{Role: agui.RoleAssistant,
		ToolCalls: []agui.ToolCall{{ID: "c1", Type: "function", Function: agui.FunctionCall{Name: "sum", Arguments: `{"a":1,"b":2}`}}}}}})
	m := newMem(def(), user("add"), run.Input{})
	e.h.Store = m
	_ = e.h.Run(context.Background(), claim)
	if m.status != run.StatusFailed || len(e.mcp.Calls()) != 0 || len(danglingCalls(m.rc.Messages)) != 0 || len(m.calls) != 0 {
		t.Fatalf("%s, %d gateway calls, %d dangling, %d records", m.status, len(e.mcp.Calls()), len(danglingCalls(m.rc.Messages)), len(m.calls))
	}
}

type stub struct{ resp *model.Response }

func (stub) Name() string { return "fake" }
func (s stub) Complete(_ context.Context, _ model.Request, _ func(model.Delta)) (*model.Response, error) {
	r := *s.resp
	return &r, nil
}

// A finished answer with a call (not cut off) still runs the tool: the rule is about the cut.
func TestAnAnswerThatEndedWithAToolCallStillRunsIt(t *testing.T) {
	e := setup(t)
	m := newMem(def(), user("please sum"), run.Input{})
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if len(e.mcp.Calls()) != 1 || m.status != run.StatusCompleted {
		t.Fatalf("%s %v", m.status, e.mcp.Calls())
	}
}
