package harness

// D61: a tool of the platform's bridge answers that the member has not authorized the third-party service. The HOST reads the
// marker only from a tool the gateway marked external, only as a connection id, writes one idp.host/authorization.required
// with the call's answer, and gives the model nothing but the tool's own text.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/run"
	"idp-host/tests/fixtures/fakemcp"
	"idp-host/tests/fixtures/fakemodel"
)

const authText = "authorization required: the member has not connected this service yet"

// spyStore remembers the events of every AnswerToolCall, so that a test can say what was written together with an answer.
type spyStore struct {
	*memStore
	answers []spyAnswer
}

type spyAnswer struct {
	id     string
	events []agui.Event
}

func (s *spyStore) AnswerToolCall(ctx context.Context, c *run.Claim, id, state, result string, isError bool, msg agui.Message, src *run.SourceBatch, events []agui.Event) error {
	err := s.memStore.AnswerToolCall(ctx, c, id, state, result, isError, msg, src, events)
	if err == nil {
		s.answers = append(s.answers, spyAnswer{id, events})
	}
	return err
}

// bridgeTool is a read-only tool that fails with the marker in structuredContent; external says whether the gateway marks it.
func bridgeTool(name string, external bool, marker any) fakemcp.Tool {
	t := fakemcp_tool(name, `{"type":"object"}`, true, func(string, map[string]any) (string, any, bool) {
		return authText, map[string]any{run.AuthorizationRequiredKey: marker}, true
	})
	if external {
		t.Meta = map[string]any{"io.idp.telos/external": true}
	}
	return t
}

func authDef() agent.Definition {
	d := def()
	d.Tools.Allow = []string{"bridge_issues", "bridge_named", "plain_issues"}
	d.Confirmation = agent.ConfirmationPolicy{Default: agent.ConfirmAuto}
	return d
}

func authRun(t *testing.T, e *env, tools ...fakemcp.Tool) (*memStore, func() string) {
	t.Helper()
	e.mcp.Tools = append(e.mcp.Tools, tools...)
	bodies := recordBodies(t, e)
	return newWebRun(authDef(), "list my issues", "run_1", "tok", run.Input{ExternalTools: true}), bodies
}

func authEvents(m *memStore) []map[string]any { return eventNamed(m, agui.CustomAuthorizationRequired) }

func TestAnExternalToolThatSaysTheMemberHasNotAuthorizedIsAnnouncedOnceWithItsConnection(t *testing.T) {
	e := setup(t)
	m, bodies := authRun(t, e, bridgeTool("bridge_issues", true, map[string]any{"connection": "linear-1"}))
	spy := &spyStore{memStore: m}
	e.h.Store = spy
	e.model.Script = seq(call("bridge_issues", `{}`), fakemodel.Turn{Text: "you need to connect Linear"})
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if m.status != run.StatusCompleted {
		t.Fatalf("the run goes on: %s %s", m.status, m.failCode)
	}
	evs := authEvents(m)
	if len(evs) != 1 {
		t.Fatalf("want one event, got %v", evs)
	}
	id := spy.answers[0].id
	want := map[string]any{"toolCallId": id, "tool": "bridge_issues", "connection": "linear-1"}
	if fmt.Sprint(evs[0]) != fmt.Sprint(want) {
		t.Errorf("event %v, want %v", evs[0], want)
	}
	// the same write as the call's answer: the one AnswerToolCall carries the event and the TOOL_CALL_RESULT
	if len(spy.answers) != 1 {
		t.Fatalf("%d answers", len(spy.answers))
	}
	var names []string
	for _, ev := range spy.answers[0].events {
		if v, ok := ev["value"].(map[string]any); ev["type"] == agui.EventCustom && ok {
			names = append(names, fmt.Sprintf("%v %v", ev["name"], v["connection"]))
		} else {
			names = append(names, fmt.Sprint(ev["type"]))
		}
	}
	if fmt.Sprint(names) != fmt.Sprintf("[%s linear-1 %s]", agui.CustomAuthorizationRequired, agui.EventToolCallResult) {
		t.Errorf("events written with the answer: %v", names)
	}
	// the model reads the tool's own text, wrapped as any result of an external tool, and nothing is added
	sent := bodies()
	if !strings.Contains(sent, authText) {
		t.Errorf("the model should read the tool's text: %s", sent)
	}
	for _, bad := range []string{"linear-1", "authorizationRequired", "connection"} {
		if strings.Contains(sent, bad) {
			t.Errorf("a model request contains %q", bad)
		}
	}
	if c := m.calls[id]; c.State != run.CallFailed || !c.IsError {
		t.Errorf("the call is an error: %+v", c)
	}
}

// Only the gateway's own statement that a tool is external makes its marker count: a tool that is not external, and one the
// operator's patterns name but the gateway does not mark, are not read.
func TestTheMarkerOfAToolThatTheGatewayDoesNotMarkExternalIsIgnored(t *testing.T) {
	e := setup(t)
	m, _ := authRun(t, e,
		bridgeTool("plain_issues", false, map[string]any{"connection": "linear-1"}),
		bridgeTool("bridge_named", false, map[string]any{"connection": "linear-1"}))
	e.h.Gateways["edge"].ExternalTools = []string{"bridge_named"} // the operator's pattern, not the gateway's marker
	e.h.Store = m
	e.model.Script = seq(call("plain_issues", `{}`), call("bridge_named", `{}`), fakemodel.Turn{Text: "done"})
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if m.status != run.StatusCompleted || len(m.calls) != 2 {
		t.Fatalf("%s %s %d calls", m.status, m.failCode, len(m.calls))
	}
	if evs := authEvents(m); len(evs) != 0 {
		t.Errorf("events %v", evs)
	}
}

func TestAConnectionIdThatIsNotAShortPatternIsIgnored(t *testing.T) {
	bad := map[string]any{
		"a url":             "https://evil.example/authorize?x=1",
		"a slash":           "a/b",
		"a space":           "linear 1",
		"empty":             "",
		"too long":          strings.Repeat("a", run.MaxConnectionID+1),
		"a number":          7,
		"an object":         map[string]any{"id": "x"},
		"no connection key": nil,
	}
	for name, conn := range bad {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			marker := map[string]any{"connection": conn}
			if conn == nil {
				marker = map[string]any{"url": "https://evil.example/authorize"}
			}
			m, _ := authRun(t, e, bridgeTool("bridge_issues", true, marker))
			e.h.Store = m
			e.model.Script = seq(call("bridge_issues", `{}`), fakemodel.Turn{Text: "done"})
			if err := e.h.Run(context.Background(), claim); err != nil {
				t.Fatal(err)
			}
			if m.status != run.StatusCompleted || len(authEvents(m)) != 0 {
				t.Errorf("%s: events %v", m.status, authEvents(m))
			}
		})
	}
	t.Run("the longest id is accepted", func(t *testing.T) {
		e := setup(t)
		id := strings.Repeat("A", run.MaxConnectionID-3) + "-_9"
		m, _ := authRun(t, e, bridgeTool("bridge_issues", true, map[string]any{"connection": id}))
		e.h.Store = m
		e.model.Script = seq(call("bridge_issues", `{}`), fakemodel.Turn{Text: "done"})
		if err := e.h.Run(context.Background(), claim); err != nil {
			t.Fatal(err)
		}
		if evs := authEvents(m); len(evs) != 1 || evs[0]["connection"] != id {
			t.Errorf("%v", evs)
		}
	})
}

// A URL in the marker or in the text is never put in an event: the event has the id only, and the Workspace builds its link.
func TestAnAddressInTheMarkerOrTheTextIsNeverInAnEvent(t *testing.T) {
	e := setup(t)
	const evil = "https://evil.example/oauth/authorize?state=SECRET-URL-4417"
	marker := map[string]any{"connection": "linear-1", "url": evil, "authorizationUrl": evil}
	tool := fakemcp_tool("bridge_issues", `{"type":"object"}`, true, func(string, map[string]any) (string, any, bool) {
		return "connect at " + evil, map[string]any{run.AuthorizationRequiredKey: marker}, true
	})
	tool.Meta = map[string]any{"io.idp.telos/external": true}
	m, _ := authRun(t, e, tool)
	e.h.Store = m
	e.model.Script = seq(call("bridge_issues", `{}`), fakemodel.Turn{Text: "done"})
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if len(authEvents(m)) != 1 {
		t.Fatalf("events %v", authEvents(m))
	}
	for _, ev := range m.events {
		if ev["name"] == agui.CustomAuthorizationRequired && strings.Contains(fmt.Sprint(ev), "evil.example") {
			t.Errorf("an address in %v", ev)
		}
	}
}

// D47: a worker that dies at the answer loses the event with it, and the next worker (the call is read-only, so it is sent
// again) writes it once; a worker that resumes a run whose call is answered does not write it again.
func TestAResumedRunWritesTheAuthorizationEventOnceAndReplayDoesNotRepeatIt(t *testing.T) {
	e := setup(t)
	m, _ := authRun(t, e, bridgeTool("bridge_issues", true, map[string]any{"connection": "linear-1"}))
	cs := &crashStore{memStore: m, dieOnToolAnswer: "bridge_issues"}
	e.h.Store = cs
	e.model.Script = seq(call("bridge_issues", `{}`), fakemodel.Turn{Text: "done"})
	if err := e.h.Run(context.Background(), claim); err == nil {
		t.Fatal("the first worker should have lost its claim")
	}
	if n := len(authEvents(m)); n != 0 {
		t.Fatalf("the lost write left %d events", n)
	}
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if m.status != run.StatusCompleted || len(authEvents(m)) != 1 {
		t.Fatalf("%s: events %v", m.status, authEvents(m))
	}
	// a worker that runs it once more on a thread that is answered finds nothing to do and writes nothing
	m.status, m.rc.Run.Status = run.StatusRunning, run.StatusRunning
	e.h.Store = m
	_ = e.h.Run(context.Background(), claim)
	if n := len(authEvents(m)); n != 1 {
		t.Errorf("%d events after a replay", n)
	}
}
