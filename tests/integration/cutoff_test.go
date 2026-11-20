package integration

import (
	"context"
	"strings"
	"testing"

	"idp-host/internal/agui"
	"idp-host/internal/run"
	"idp-host/tests/fixtures/fakemodel"
)

// D55 on real PostgreSQL: an answer cut off at the output limit while it wrote a tool call that happens to parse runs no
// tool, stores no call and no unanswered tool_calls on the thread, fails the run as D40 says with the event that gives the
// reason; and the next run on the thread is accepted by the model server (the double refuses a thread whose tool calls are
// unanswered, as the real API does).
func TestACutOffAnswerWithAToolCallLeavesAValidThreadOnPostgres(t *testing.T) {
	e := newEnv(t)
	e.publish("assistant", definition("edge", []string{"sum", "echo"}, nil))
	e.model.Script = func([]fakemodel.Message, []string) fakemodel.Turn {
		return fakemodel.Turn{Text: "Adding them.", ToolCalls: []fakemodel.Call{{Name: "sum", Args: `{"a":1,"b":2}`}}, CutOff: true}
	}
	r := e.must(202, "POST", "/v1/agents/assistant/runs", "carol", runBody("thr-cut", "please sum"), "Idempotency-Key", "cut-1")
	id := r["runId"].(string)
	e.drive()
	got := e.must(200, "GET", "/v1/runs/"+id, "carol", nil)
	if got["status"] != run.StatusFailed || got["failureCode"] != run.FailModel {
		t.Fatalf("%v %v", got["status"], got["failureCode"])
	}
	if msg, _ := got["failureMessage"].(string); !strings.Contains(msg, "answer was cut off") || !strings.Contains(msg, "1 tool call(s), which were not run") {
		t.Errorf("the failure message does not say why: %q", msg)
	}
	if n := len(e.mcp.Calls()); n != 0 {
		t.Errorf("%d tool call(s) reached the gateway", n)
	}
	if calls := e.must(200, "GET", "/v1/runs/"+id+"/tool-calls", "carol", nil)["toolCalls"].([]any); len(calls) != 0 {
		t.Errorf("call records %v", calls)
	}
	msgs, err := e.store.ListMessages(context.Background(), e.tenantA, "carol", "thr-cut")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[1].Role != agui.RoleAssistant || msgs[1].Content != "Adding them." || len(msgs[1].ToolCalls) != 0 {
		t.Errorf("thread %+v", msgs)
	}
	types, evs, _ := e.sse("GET", "/v1/runs/"+id+"/events", "carol", nil)
	cut := 0
	for i, ty := range types {
		if ty == agui.EventToolCallStart {
			t.Errorf("a TOOL_CALL_START in the stream")
		}
		if v, _ := evs[i]["value"].(map[string]any); evs[i]["name"] == agui.CustomModelCutOff && v["toolCalls"] == float64(1) {
			cut++
		}
	}
	if cut != 1 {
		t.Errorf("%d cut-off events: %v", cut, types)
	}
	// the next run on the thread
	e.model.Script = fakemodel.DefaultScript
	r = e.must(202, "POST", "/v1/agents/assistant/runs", "carol", runBody("thr-cut", "hello again"), "Idempotency-Key", "cut-2")
	e.drive()
	if got := e.must(200, "GET", "/v1/runs/"+r["runId"].(string), "carol", nil); got["status"] != run.StatusCompleted {
		t.Errorf("the next run on the thread: %v %v", got["status"], got["failureCode"])
	}
}

// With nothing said before the cut call, no assistant message is stored at all (providers refuse an empty one) and the
// thread still takes the next run.
func TestACutOffAnswerWithOnlyAToolCallStoresNoMessageOnPostgres(t *testing.T) {
	e := newEnv(t)
	e.publish("assistant", definition("edge", []string{"sum", "echo"}, nil))
	e.model.Script = func([]fakemodel.Message, []string) fakemodel.Turn {
		return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "sum", Args: `{"a":1,"b":"hel`}}, CutOff: true}
	}
	r := e.must(202, "POST", "/v1/agents/assistant/runs", "carol", runBody("thr-cut2", "please sum"), "Idempotency-Key", "cut2-1")
	e.drive()
	if got := e.must(200, "GET", "/v1/runs/"+r["runId"].(string), "carol", nil); got["status"] != run.StatusFailed || got["failureCode"] != run.FailModel {
		t.Fatalf("%v %v", got["status"], got["failureCode"])
	}
	msgs, _ := e.store.ListMessages(context.Background(), e.tenantA, "carol", "thr-cut2")
	if len(msgs) != 1 || msgs[0].Role != agui.RoleUser {
		t.Errorf("thread %+v", msgs)
	}
	e.model.Script = fakemodel.DefaultScript
	r = e.must(202, "POST", "/v1/agents/assistant/runs", "carol", runBody("thr-cut2", "hello again"), "Idempotency-Key", "cut2-2")
	e.drive()
	if got := e.must(200, "GET", "/v1/runs/"+r["runId"].(string), "carol", nil); got["status"] != run.StatusCompleted {
		t.Errorf("the next run on the thread: %v %v", got["status"], got["failureCode"])
	}
}
