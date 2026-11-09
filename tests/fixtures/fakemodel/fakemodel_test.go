package fakemodel

import (
	"strings"
	"testing"
)

func TestSandboxFlow(t *testing.T) {
	tools := []string{"sandbox__sandbox_create", "sandbox__sandbox_run", "minimal__echo"}
	user := Message{Role: "user", Content: "run python: print(6*7)"}
	t1 := DefaultScript([]Message{user}, tools)
	if len(t1.ToolCalls) != 1 || t1.ToolCalls[0].Name != "sandbox__sandbox_create" {
		t.Fatalf("first turn: %+v", t1)
	}
	id := strings.Repeat("ab", 16)
	created := Message{Role: "tool", Content: "workspace " + id + " created, expires 2026-11-06T12:00:00Z"}
	t2 := DefaultScript([]Message{user, {Role: "assistant", ToolCalls: t1.ToolCalls}, created}, tools)
	if len(t2.ToolCalls) != 1 || t2.ToolCalls[0].Name != "sandbox__sandbox_run" ||
		!strings.Contains(t2.ToolCalls[0].Args, `"workspaceId":"`+id+`"`) || !strings.Contains(t2.ToolCalls[0].Args, `print(6*7)`) {
		t.Fatalf("second turn: %+v", t2)
	}
	ran := Message{Role: "tool", Content: "exit code 0\n--- stdout ---\n42\n"}
	if t3 := DefaultScript([]Message{user, created, ran}, tools); !strings.HasPrefix(t3.Text, "Result: exit code 0") {
		t.Fatalf("third turn: %+v", t3)
	}
	// without a sandbox offered the request is just echoed
	if got := DefaultScript([]Message{user}, []string{"minimal__echo"}); len(got.ToolCalls) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestDefaultScriptExtractsFactsFromTheUsersMessageOnly(t *testing.T) {
	prompt := "KNOWN FACTS (key = value):\nfavourite_colour = red\n\nAt most 5 facts.\n\nUSER MESSAGE:\nActually my favourite colour is now Teal. I live in Chengdu."
	turn := DefaultScript([]Message{{Role: "user", Content: prompt}}, nil)
	want := `{"facts":[{"key":"favourite_colour","value":"teal","statement":"The user's favourite colour is teal"},{"key":"city","value":"chengdu","statement":"The user's city is chengdu"}]}`
	if turn.Text != want {
		t.Fatalf("%s", turn.Text)
	}
	if got := DefaultScript([]Message{{Role: "user", Content: "KNOWN FACTS (key = value):\nfavourite_colour = red\n\nUSER MESSAGE:\nhello"}}, nil).Text; got != `{"facts":[]}` {
		t.Fatalf("%s", got)
	}
}
