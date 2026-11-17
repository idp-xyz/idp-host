package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"idp-host/internal/agui"
	"idp-host/internal/run"
	"idp-host/tests/fixtures/fakemodel"
)

// D40: the model reads its last tool results whole and older long ones as head and tail; skill resources and short
// results stay as they are; the stored conversation is not changed.
func TestOldToolResultsAreShortenedForTheModel(t *testing.T) {
	e := setup(t)
	e.h.KeepToolResults, e.h.OldToolResultBytes = 2, 300
	big := strings.Repeat("0123456789", 500)
	msgs := []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: "hello"}}
	add := func(i int, tool, content string) {
		id := fmt.Sprintf("c%d", i)
		msgs = append(msgs, agui.Message{ID: "a" + id, Role: agui.RoleAssistant, ToolCalls: []agui.ToolCall{{ID: id, Type: "function", Function: agui.FunctionCall{Name: tool, Arguments: "{}"}}}},
			agui.Message{ID: "tool_" + id, Role: agui.RoleTool, ToolCallID: id, Content: content})
	}
	add(1, "resource__read", "REFERENCE "+big)
	add(2, "sum", "OLD-HEAD "+big+" OLD-TAIL")
	add(3, "sum", "short old result")
	add(4, "sum", "RECENT-A "+big)
	add(5, "sum", "RECENT-B "+big)
	msgs = append(msgs, agui.Message{ID: "u2", Role: agui.RoleUser, Content: "hello again"})
	m := newMem(def(), msgs, run.Input{})
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	// what the model was sent, found by a marker in it: the input guard wraps every tool result (D27)
	sent := func(marker string) string {
		t.Helper()
		for _, x := range e.model.Last() {
			if x.Role == "tool" && strings.Contains(x.Content, marker) {
				return x.Content
			}
		}
		t.Fatalf("no tool result with %q reached the model", marker)
		return ""
	}
	old := sent("OLD-HEAD")
	if len(old) > 500 || !strings.Contains(old, "bytes of this earlier result are not shown") || !strings.Contains(old, "OLD-TAIL") {
		t.Fatalf("the old result reached the model as %d bytes: %.120q", len(old), old)
	}
	for _, k := range []string{"REFERENCE", "RECENT-A", "RECENT-B"} {
		if got := sent(k); len(got) < 5000 || strings.Contains(got, "are not shown") {
			t.Errorf("%s was shortened to %d bytes", k, len(got))
		}
	}
	if got := sent("short old result"); len(got) > 100 || strings.Contains(got, "are not shown") {
		t.Errorf("%q", got)
	}
	for _, x := range m.rc.Messages {
		if x.ID == "tool_c2" && len(x.Content) < 5000 {
			t.Fatal("the stored conversation was shortened")
		}
	}
}

// D40: a request the provider refuses because it does not fit the model's context window fails the run with that
// reason and what to do about it; any other 400 stays a bare status.
func TestRequestTooLongForTheModelSaysSo(t *testing.T) {
	for _, c := range []struct{ providerCode, stepCode, message string }{
		{"context_length_exceeded", "context_length", "no longer fits the model's context window; start a new conversation"},
		{"invalid_tool_schema", "status_400", "model call failed: status_400"},
	} {
		e := setup(t)
		e.model.Script = func([]fakemodel.Message, []string) fakemodel.Turn {
			return fakemodel.Turn{Status: 400, ErrorCode: c.providerCode}
		}
		m := newMem(def(), user("hello"), run.Input{})
		e.h.Store = m
		err := e.h.Run(context.Background(), claim)
		var f *run.Failure
		if !errors.As(err, &f) || f.Code != run.FailModel || !strings.Contains(f.Message, c.message) || m.status != run.StatusFailed || m.steps[0].ErrorCode != c.stepCode {
			t.Fatalf("%s: %v %s %+v", c.providerCode, err, m.status, m.steps)
		}
	}
}

// D40: an answer the model could not finish (finish reason length, no tool call) fails the run with the reason.
func TestCutOffAnswerIsNotACompletedRun(t *testing.T) {
	e := setup(t)
	e.model.Script = func([]fakemodel.Message, []string) fakemodel.Turn {
		return fakemodel.Turn{Text: "Let me look at the next", CutOff: true}
	}
	m := newMem(def(), user("hello"), run.Input{})
	e.h.Store = m
	err := e.h.Run(context.Background(), claim)
	var f *run.Failure
	if !errors.As(err, &f) || f.Code != run.FailModel || !strings.Contains(f.Message, "cut off") || m.status != run.StatusFailed {
		t.Fatalf("%v %s", err, m.status)
	}
}
