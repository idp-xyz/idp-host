package agui

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMessageContentForms(t *testing.T) {
	var m Message
	if err := json.Unmarshal([]byte(`{"id":"m1","role":"user","content":"hello"}`), &m); err != nil || m.Content != "hello" {
		t.Fatal(err, m)
	}
	if err := json.Unmarshal([]byte(`{"id":"m2","role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`), &m); err != nil || m.Content != "ab" {
		t.Fatal(err, m)
	}
	if err := json.Unmarshal([]byte(`{"id":"m3","role":"user","content":[{"type":"image","url":"x"}]}`), &m); err == nil {
		t.Fatal("image part accepted")
	}
	if err := json.Unmarshal([]byte(`{"id":"m4","role":"assistant","content":null,"toolCalls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}`), &m); err != nil || m.ToolCalls[0].Function.Name != "f" {
		t.Fatal(err, m)
	}
	b, _ := json.Marshal(m)
	if strings.Contains(string(b), `"content"`) {
		t.Fatalf("empty content must be omitted: %s", b)
	}
}

func TestValidation(t *testing.T) {
	bad := []string{
		`{"id":"","role":"user","content":"x"}`,
		`{"id":"m","role":"owner","content":"x"}`,
		`{"id":"m","role":"tool","content":"x"}`,
		`{"id":"m","role":"user","content":"x","toolCallId":"c"}`,
		`{"id":"m","role":"assistant","toolCalls":[{"id":"c","type":"function","function":{"name":"f","arguments":"not json"}}]}`,
	}
	for _, s := range bad {
		var m Message
		if err := json.Unmarshal([]byte(s), &m); err == nil && m.Validate() == nil {
			t.Errorf("accepted: %s", s)
		}
	}
	in := RunAgentInput{ThreadID: "thr 1"}
	if in.Validate() == nil {
		t.Fatal("bad thread id accepted")
	}
	in = RunAgentInput{Messages: []Message{{ID: "a", Role: RoleUser, Content: "x"}, {ID: "a", Role: RoleUser, Content: "y"}}}
	if in.Validate() == nil {
		t.Fatal("duplicate ids accepted")
	}
	in = RunAgentInput{Tools: []Tool{{Name: "t"}, {Name: "t"}}}
	if in.Validate() == nil {
		t.Fatal("duplicate tools accepted")
	}
	in = RunAgentInput{ThreadID: "thr-1", RunID: "run_1", Messages: []Message{{ID: "a", Role: RoleUser, Content: "x"}}, Tools: []Tool{{Name: "ui", Parameters: json.RawMessage(`{"type":"object"}`)}}}
	if err := in.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestEvents(t *testing.T) {
	e := RunFinished("t", "r", OutcomeInterrupt, []map[string]any{{"kind": "confirmation"}})
	if e.Type() != EventRunFinished || e["outcome"].(map[string]any)["type"] != OutcomeInterrupt {
		t.Fatalf("%v", e)
	}
	if _, ok := RunStarted("t", "r")["timestamp"]; !ok {
		t.Fatal("timestamp")
	}
	if ToolCallResult("m", "c", "ok")["role"] != RoleTool {
		t.Fatal("tool result role")
	}
	b, _ := json.Marshal(MessagesSnapshot(nil))
	if !strings.Contains(string(b), `"messages":[]`) {
		t.Fatalf("%s", b)
	}
}
