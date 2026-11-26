package harness

// D47: what happens to a tool call that was in progress when the worker stopped, and to the assistant message that was
// stored without the records of its calls. The tests kill a first worker at a chosen write (crashStore) and let a second
// Run on the same store be the worker that resumes the run, as the real store does once the lease of the first expires.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"idp-host/internal/a2a"
	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/run"
	"idp-host/tests/fixtures/fakemodel"
)

// crashStore is a store whose worker dies at a chosen write: from then on nothing is stored, as when the process is gone
// (the real store refuses a stale claim the same way: run.ErrFenced). Nothing is stored by the write it dies at, except
// for splitTurn, which reproduces what data written before D47 looks like.
type crashStore struct {
	*memStore
	dead         atomic.Bool
	dieOnState   string // UpdateToolCall to this state kills the worker (the write is lost)
	dieOnRecords bool   // RecordToolCalls kills the worker (the write is lost)
	dieOnTurn    bool   // AppendAssistantTurn kills the worker (the write is lost)
	// splitTurn: AppendAssistantTurn stores the assistant message with the events of the model's step, as the two writes
	// before D47 did, and the worker dies before the second write: the message has no tool_call rows and the events of the
	// decisions (denied, confirmation required) are not on the stream.
	splitTurn bool
	// splitAnswer: AnswerToolCall stores the call's final state and the worker dies before the tool message is written.
	splitAnswer bool
	// dieOnToolMessage: the worker dies at the write of a tool message (AppendMessages of a tool message, or AnswerToolCall,
	// which is that write and the call's state together); nothing of that write is stored.
	dieOnToolMessage bool
	// D49: the worker dies at the answer of a call to this tool, or at the check of the citations at the end of the run.
	dieOnToolAnswer string
	dieOnCheck      bool
}

func (c *crashStore) CheckSources(ctx context.Context, cl *run.Claim, answer string) error {
	if c.dead.Load() || c.dieOnCheck {
		c.dead.Store(true)
		return run.ErrFenced
	}
	return c.memStore.CheckSources(ctx, cl, answer)
}

func (c *crashStore) UpdateToolCall(ctx context.Context, cl *run.Claim, id, state, result string, isErr bool, ev []agui.Event) error {
	if c.dead.Load() || (c.dieOnState != "" && state == c.dieOnState) {
		c.dead.Store(true)
		return run.ErrFenced
	}
	return c.memStore.UpdateToolCall(ctx, cl, id, state, result, isErr, ev)
}

func (c *crashStore) AnswerToolCall(ctx context.Context, cl *run.Claim, id, state, result string, isErr bool, msg agui.Message, src *run.SourceBatch, ev []agui.Event) error {
	if c.dead.Load() || c.dieOnToolMessage || (c.dieOnState != "" && state == c.dieOnState) || c.answerKilled(id) {
		c.dead.Store(true)
		return run.ErrFenced
	}
	if c.splitAnswer {
		// what two transactions did before D47: the call's state is stored, the worker dies before its tool message
		err := c.memStore.UpdateToolCall(ctx, cl, id, state, result, isErr, nil)
		c.dead.Store(true)
		if err != nil {
			return err
		}
		return run.ErrFenced
	}
	return c.memStore.AnswerToolCall(ctx, cl, id, state, result, isErr, msg, src, ev)
}

var errSplit = errors.New("split answer")

// AnswerCited is AnswerToolCall's crash points for an answer the HOST numbers (D70): the answer is made, then the worker dies instead of
// writing it (or writes the call's state only, when the test splits the answer as D47's predecessor did).
func (c *crashStore) AnswerCited(ctx context.Context, cl *run.Claim, id string, cite run.CiteFunc) error {
	var split run.Answer
	err := c.memStore.AnswerCited(ctx, cl, id, func(first int, tree *run.TreeNumbers) (run.Answer, error) {
		a, err := cite(first, tree)
		if err != nil {
			return a, err
		}
		if c.dead.Load() || c.dieOnToolMessage || (c.dieOnState != "" && a.State == c.dieOnState) || c.answerKilled(id) {
			c.dead.Store(true)
			return run.Answer{}, run.ErrFenced
		}
		if c.splitAnswer {
			split = a
			return run.Answer{}, errSplit
		}
		return a, nil
	})
	if errors.Is(err, errSplit) {
		err = c.memStore.UpdateToolCall(ctx, cl, id, split.State, split.Result, split.IsError, nil)
		c.dead.Store(true)
		if err != nil {
			return err
		}
		return run.ErrFenced
	}
	return err
}

func (c *crashStore) RecordToolCalls(ctx context.Context, cl *run.Claim, calls []run.ToolCall, ev []agui.Event) error {
	if c.dead.Load() || c.dieOnRecords {
		c.dead.Store(true)
		return run.ErrFenced
	}
	return c.memStore.RecordToolCalls(ctx, cl, calls, ev)
}

func (c *crashStore) AppendAssistantTurn(ctx context.Context, cl *run.Claim, msg agui.Message, calls []run.ToolCall, ev []agui.Event) error {
	if c.dead.Load() || c.dieOnTurn {
		c.dead.Store(true)
		return run.ErrFenced
	}
	if c.splitTurn {
		var kept []agui.Event
		for _, e := range ev {
			if n, _ := e["name"].(string); n != agui.CustomToolDenied && n != agui.CustomConfirmationRequired {
				kept = append(kept, e)
			}
		}
		err := c.memStore.AppendMessages(ctx, cl, []agui.Message{msg}, kept)
		c.dead.Store(true)
		if err != nil {
			return err
		}
		return run.ErrFenced
	}
	return c.memStore.AppendAssistantTurn(ctx, cl, msg, calls, ev)
}

func (c *crashStore) AppendMessages(ctx context.Context, cl *run.Claim, msgs []agui.Message, ev []agui.Event) error {
	if c.dead.Load() || (c.dieOnToolMessage && len(msgs) > 0 && msgs[0].Role == agui.RoleTool) {
		c.dead.Store(true)
		return run.ErrFenced
	}
	return c.memStore.AppendMessages(ctx, cl, msgs, ev)
}

func (c *crashStore) Finish(ctx context.Context, cl *run.Claim, status, code, msg string, ev []agui.Event) error {
	if c.dead.Load() {
		return run.ErrFenced
	}
	return c.memStore.Finish(ctx, cl, status, code, msg, ev)
}

// interruptions are the idp.host/tool.interrupted events of the run's stream.
func interruptions(m *memStore) []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []map[string]any
	for _, e := range m.events {
		if e.Type() == agui.EventCustom && e["name"] == agui.CustomToolInterrupted {
			out = append(out, e["value"].(map[string]any))
		}
	}
	return out
}

func countEvents(m *memStore, typ string) int {
	n := 0
	for _, t := range m.types() {
		if t == typ {
			n++
		}
	}
	return n
}

// toolResultSeen records what the model reads in the last tool message it is shown.
func toolResultSeen(e *env) *atomic.Value {
	var seen atomic.Value
	e.model.Script = func(messages []fakemodel.Message, tools []string) fakemodel.Turn {
		if n := len(messages); n > 0 && messages[n-1].Role == "tool" {
			seen.Store(messages[n-1].Content)
		}
		return fakemodel.DefaultScript(messages, tools)
	}
	return &seen
}

// firstWorkerDiesInTheCall runs a worker that dies while the gateway handles the model's call to "sum": either after the
// gateway finished it and before the result was stored, or while the gateway is still running it. The call rests as
// EXECUTING in the returned store, which is ready for the next worker. The gateway's handler answers "3".
func firstWorkerDiesInTheCall(t *testing.T, e *env, mode string) *memStore {
	t.Helper()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var started atomic.Int32
	firstStarted := make(chan struct{})
	e.mcp.Tools[0].Handle = func(map[string]any) (string, bool) {
		if started.Add(1) == 1 {
			close(firstStarted)
			if mode == "gateway still running" {
				<-release
			}
		}
		return "3", false
	}
	m := newMem(def(), user("please sum"), run.Input{})
	cs := &crashStore{memStore: m}
	e.h.Store = cs
	ctx1, kill := context.WithCancel(context.Background())
	defer kill()
	if mode == "gateway finished, result lost" {
		cs.dieOnState = run.CallDone
	}
	done := runIn(e.h, ctx1)
	<-firstStarted
	if mode == "gateway still running" {
		cs.dead.Store(true) // the process is gone: the connection to the gateway drops, nothing is stored any more
		kill()
	}
	if end := <-done; !errors.Is(end.err, run.ErrFenced) && !errors.Is(end.err, context.Canceled) {
		t.Logf("first worker ended with %v", end.err)
	}
	if c := m.calls["call_1_0"]; c == nil || c.State != run.CallExecuting {
		t.Fatalf("setup: the call should rest as EXECUTING, got %+v", c)
	}
	e.h.Store = m // the next worker claims the run (the lease having expired) and resumes it
	return m
}

var interruptedModes = []string{"gateway finished, result lost", "gateway still running"}

// D47: a gateway call that was EXECUTING when the worker died is sent again only when the tool, as the gateway lists it
// now, is read-only or idempotent and the operator did not name it in neverResendTools; every other call is answered to the
// model as interrupted with an unknown outcome and the run goes on. The event says which happened.
func TestAToolCallThatWasExecutingWhenTheWorkerDiedIsSentAgainOnlyWhenRepeatingIsHarmless(t *testing.T) {
	for _, tc := range []struct {
		name        string
		annotations map[string]any
		neverResend []string
		vanished    bool // the gateway no longer lists the tool when the run is resumed
		resent      bool
	}{
		{name: "a tool with no annotation is not sent again"},
		{name: "a tool annotated neither read-only nor idempotent is not sent again", annotations: map[string]any{"readOnlyHint": false, "idempotentHint": false}},
		{name: "a read-only tool is sent again", annotations: map[string]any{"readOnlyHint": true}, resent: true},
		{name: "an idempotent tool is sent again", annotations: map[string]any{"idempotentHint": true}, resent: true},
		{name: "neverResendTools stops a read-only tool being sent again", annotations: map[string]any{"readOnlyHint": true}, neverResend: []string{"su*"}},
		{name: "neverResendTools stops an idempotent tool being sent again", annotations: map[string]any{"idempotentHint": true}, neverResend: []string{"sum"}},
		{name: "a pattern of neverResendTools that does not match the tool changes nothing", annotations: map[string]any{"readOnlyHint": true}, neverResend: []string{"echo"}, resent: true},
		{name: "a read-only tool the gateway no longer lists is not sent again", annotations: map[string]any{"readOnlyHint": true}, vanished: true},
	} {
		for _, mode := range interruptedModes {
			t.Run(tc.name+" ("+mode+")", func(t *testing.T) {
				e := setup(t)
				e.mcp.Tools[0].Annotations = tc.annotations
				e.h.Gateways["edge"].NeverResendTools = tc.neverResend
				seen := toolResultSeen(e)
				m := firstWorkerDiesInTheCall(t, e, mode)
				if tc.vanished {
					e.mcp.Tools = e.mcp.Tools[1:]
				}
				if err := e.h.Run(context.Background(), claim); err != nil {
					t.Fatalf("second worker: %v", err)
				}
				want := 1
				if tc.resent {
					want = 2
				}
				if n := len(e.mcp.Calls()); n != want {
					t.Errorf("the gateway received tools/call sum %d times; want %d", n, want)
				}
				if m.status != run.StatusCompleted {
					t.Errorf("run status %s (%s), want completed", m.status, m.failCode)
				}
				evs := interruptions(m)
				if len(evs) != 1 || evs[0]["resent"] != tc.resent || evs[0]["reason"] != "worker_stopped" || evs[0]["toolCallId"] != "call_1_0" || evs[0]["tool"] != "sum" {
					t.Errorf("want one idp.host/tool.interrupted {resent: %v, reason: worker_stopped}, got %v", tc.resent, evs)
				}
				read, _ := seen.Load().(string)
				if tc.resent {
					if read != "3" || m.last().Content != "Result: 3" {
						t.Errorf("the model read %q and answered %q, want the tool's result", read, m.last().Content)
					}
					return
				}
				for _, part := range []string{"interrupted", "may or may not have completed it", "outcome is unknown", "Check whether it was done before doing it again"} {
					if !strings.Contains(read, part) {
						t.Errorf("the model read %q, which lacks %q", read, part)
					}
				}
				if c := m.calls["call_1_0"]; c.State != run.CallFailed || !c.IsError {
					t.Errorf("the call should be FAILED as an error, got %+v", c)
				}
			})
		}
	}
}

// D47: the HOST cannot see or undo what a remote agent did, and a second child run is not harmless either, so a sub-agent
// call that was EXECUTING when the worker died (a remote one in practice; a local one is DELEGATED and the state should not
// exist) is not sent again: no request to the remote agent, no child run, and the model reads that its outcome is unknown.
func TestASubagentCallThatWasExecutingWhenTheWorkerDiedIsNotSentAgain(t *testing.T) {
	for _, remoteAgent := range []bool{true, false} {
		name := "local"
		if remoteAgent {
			name = "remote"
		}
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			var hits atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(500) }))
			t.Cleanup(remote.Close)
			e.h.A2A = &a2a.Client{HTTP: remote.Client()}
			seen := toolResultSeen(e)
			d := def()
			sub := agent.EmbeddedSubagent{Name: "helper", AgentID: "a-helper", Version: 1, Digest: "sha256:" + strings.Repeat("ab", 32)}
			ref := agent.SubagentRef{Name: "helper", Description: "A helper."}
			if remoteAgent {
				sub = agent.EmbeddedSubagent{Name: "helper", A2A: &agent.A2ARef{CardURL: remote.URL + "/card"}}
				ref.A2A = sub.A2A
			}
			d.Subagents = []agent.SubagentRef{ref}
			d.Normalize()
			tool := agent.SubagentToolName("helper")
			msgs := append(user("please delegate"), agui.Message{ID: "a1", Role: agui.RoleAssistant,
				ToolCalls: []agui.ToolCall{{ID: "call_r", Type: "function", Function: agui.FunctionCall{Name: tool, Arguments: `{"input":"x"}`}}}})
			m := newMem(d, msgs, run.Input{})
			m.rc.Release = &agent.Document{Definition: d, Subagents: []agent.EmbeddedSubagent{sub}}
			m.rc.Run.StepCount = 1
			m.calls["call_r"] = &run.ToolCall{RunID: "run_1", ID: "call_r", Step: 1, MessageID: "a1", Tool: tool, Arguments: `{"input":"x"}`, State: run.CallExecuting}
			e.h.Store = m
			if err := e.h.Run(context.Background(), claim); err != nil {
				t.Fatal(err)
			}
			if hits.Load() != 0 || len(m.children) != 0 {
				t.Errorf("the remote agent was reached %d times and %d child runs were created; want none", hits.Load(), len(m.children))
			}
			if read, _ := seen.Load().(string); !strings.Contains(read, "outcome is unknown") {
				t.Errorf("the model read %q", read)
			}
			if evs := interruptions(m); len(evs) != 1 || evs[0]["resent"] != false || evs[0]["tool"] != tool {
				t.Errorf("want one idp.host/tool.interrupted with resent false, got %v", evs)
			}
			if m.status != run.StatusCompleted {
				t.Errorf("run status %s (%s), want completed", m.status, m.failCode)
			}
		})
	}
}

// D47: a call cut short by the deadline or a cancel (D46) says so in the same event, with its own reason.
func TestACallCutShortByACancelIsAnnouncedWithItsReason(t *testing.T) {
	e := setup(t)
	started := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	e.mcp.Tools[0].Handle = func(map[string]any) (string, bool) { close(started); <-release; return "3", false }
	m := newMem(def(), user("please sum"), run.Input{})
	e.h.Store = m
	e.h.CancelPoll = 10 * time.Millisecond
	done := runIn(e.h, context.Background())
	<-started
	m.mu.Lock()
	m.rc.Run.CancelRequested = true
	m.mu.Unlock()
	<-done
	if evs := interruptions(m); len(evs) != 1 || evs[0]["resent"] != false || evs[0]["reason"] != "canceled" {
		t.Errorf("want one idp.host/tool.interrupted {resent: false, reason: canceled}, got %v", evs)
	}
}

// D47: the assistant message and the records of its tool calls are one write, so a worker that dies at that write leaves
// nothing behind: no assistant message whose tool calls nothing records and nothing answers. The next worker asks the model
// again and the tool runs once. (The model double refuses, as OpenAI does, a thread whose tool calls nothing answers.)
func TestAWorkerThatDiesWhenTheAssistantMessageIsStoredLeavesNoCallsWithoutRecords(t *testing.T) {
	e := setup(t)
	e.model.Script = refusesUnansweredToolCalls
	m := newMem(def(), user("please sum"), run.Input{})
	// the worker dies at the write of the message with its calls, and also, were they two writes, at the second
	cs := &crashStore{memStore: m, dieOnTurn: true, dieOnRecords: true}
	e.h.Store = cs
	if end := <-runIn(e.h, context.Background()); !errors.Is(end.err, run.ErrFenced) {
		t.Fatalf("setup: the first worker should have died when the assistant message was stored: %v", end.err)
	}
	if last := m.rc.Messages[len(m.rc.Messages)-1]; last.Role == agui.RoleAssistant && len(last.ToolCalls) > 0 && len(m.calls) == 0 {
		t.Fatalf("the thread ends with an assistant message with tool calls that nothing records: %+v", last)
	}
	e.h.Store = m
	err := e.h.Run(context.Background(), claim)
	if n := len(e.mcp.Calls()); n != 1 || m.status != run.StatusCompleted {
		t.Errorf("tool executed %d times (want 1); run status %s code %q err %v; tool_call rows %d; model requests %d", n, m.status, m.failCode, err, len(m.calls), e.model.Calls())
	}
}

// D47: data written before the single transaction can end with an assistant message whose tool calls have no rows. The
// next worker creates the rows as not started and runs the calls as dispatch would have: each tool once, and a client
// following the stream sees each TOOL_CALL once.
func TestAnAssistantMessageStoredWithoutItsToolCallRecordsIsRepairedOnResume(t *testing.T) {
	e := setup(t)
	m := newMem(def(), user("please sum"), run.Input{})
	cs := &crashStore{memStore: m, splitTurn: true}
	e.h.Store = cs
	if end := <-runIn(e.h, context.Background()); !errors.Is(end.err, run.ErrFenced) {
		t.Fatalf("setup: the first worker should have died after the message: %v", end.err)
	}
	if last := m.rc.Messages[len(m.rc.Messages)-1]; len(last.ToolCalls) != 1 || len(m.calls) != 0 {
		t.Fatalf("setup: want an assistant message with one tool call and no rows, got %+v / %d rows", last, len(m.calls))
	}
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if n := len(e.mcp.Calls()); n != 1 {
		t.Errorf("the gateway received tools/call sum %d times; want exactly 1", n)
	}
	if c := m.calls["call_1_0"]; c == nil || c.State != run.CallDone {
		t.Errorf("the repaired call should be DONE, got %+v", c)
	}
	if m.status != run.StatusCompleted || m.last().Content != "Result: 3" {
		t.Errorf("run status %s (%s), last %+v", m.status, m.failCode, m.last())
	}
	if n := countEvents(m, agui.EventToolCallStart); n != 1 {
		t.Errorf("the stream has %d TOOL_CALL_START events; want 1 (no duplicates)", n)
	}
	if len(interruptions(m)) != 0 {
		t.Errorf("a call that was never sent is not an interruption: %v", interruptions(m))
	}
}

// D47: the repair applies the confirmation policy, as dispatch does: a call the release makes wait for the member is
// recorded as waiting, not executed, and the run waits for the member.
func TestARepairedToolCallStillWaitsForTheConfirmationTheReleaseRequires(t *testing.T) {
	e := setup(t)
	e.model.Script = func(messages []fakemodel.Message, tools []string) fakemodel.Turn {
		if spoken(messages).Role == "tool" {
			return fakemodel.Turn{Text: "done"}
		}
		return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "echo", Args: `{"text":"hi"}`}}}
	}
	m := newMem(def(), user("please echo"), run.Input{})
	e.h.Store = &crashStore{memStore: m, splitTurn: true}
	if end := <-runIn(e.h, context.Background()); !errors.Is(end.err, run.ErrFenced) {
		t.Fatalf("setup: %v", end.err)
	}
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if c := m.calls["call_1_0"]; c == nil || c.State != run.CallPendingConfirmation {
		t.Errorf("the repaired call should wait for confirmation, got %+v", c)
	}
	if n := len(e.mcp.Calls()); n != 0 || m.status != run.StatusAwaitingInput {
		t.Errorf("the gateway received %d calls; run status %s; want 0 and awaiting input", n, m.status)
	}
	if n := countEvents(m, "CUSTOM:"+agui.CustomConfirmationRequired); n != 1 {
		t.Errorf("the stream has %d confirmation.required events; want 1", n)
	}
}

// refusesUnansweredToolCalls is the model double behaving like OpenAI: it refuses (HTTP 400) a thread in which an assistant
// message with tool calls is not followed by a tool message for each of them.
func refusesUnansweredToolCalls(messages []fakemodel.Message, tools []string) fakemodel.Turn {
	body := messages
	if n := len(body); n > 0 && strings.HasPrefix(body[n-1].Content, "Budget:") {
		body = body[:n-1]
	}
	for i, msg := range body {
		if msg.Role != "assistant" || len(msg.ToolCalls) == 0 {
			continue
		}
		answers := 0
		for _, later := range body[i+1:] {
			if later.Role != "tool" {
				break
			}
			answers++
		}
		if answers < len(msg.ToolCalls) {
			return fakemodel.Turn{Status: 400, ErrorCode: "invalid_request_error"} // tool_calls must be followed by a tool message each
		}
	}
	return fakemodel.DefaultScript(messages, tools)
}

// finishedWithoutItsMessage returns the calls in a final state whose tool message is not in the thread.
func finishedWithoutItsMessage(m *memStore) []string {
	answered := toolMessages(m)
	var out []string
	for id, c := range m.calls {
		if finalState(c.State) && !answered[id] {
			out = append(out, id)
		}
	}
	return out
}

// D47: a tool call's final state and its tool message are one write, so a worker that dies at that write leaves no finished
// call without its message. (Reproduction: with two writes, the death between them left the call DONE and the thread with
// an assistant tool call nothing answers, and the model double refused the next request.) The tool ran once, whose result is
// lost: the next worker answers it as interrupted, and the tool is not executed again.
func TestAWorkerThatDiesWhenAToolCallIsAnsweredLeavesNoFinishedCallWithoutItsMessage(t *testing.T) {
	e := setup(t)
	e.model.Script = refusesUnansweredToolCalls
	m := newMem(def(), user("please sum"), run.Input{})
	e.h.Store = &crashStore{memStore: m, dieOnToolMessage: true}
	if end := <-runIn(e.h, context.Background()); !errors.Is(end.err, run.ErrFenced) {
		t.Fatalf("setup: the first worker should have died when the call was answered: %v", end.err)
	}
	if missing := finishedWithoutItsMessage(m); len(missing) > 0 {
		t.Fatalf("calls %v are finished and their tool messages are not in the thread", missing)
	}
	e.h.Store = m
	err := e.h.Run(context.Background(), claim)
	if n := len(e.mcp.Calls()); n != 1 || m.status != run.StatusCompleted {
		t.Errorf("tool executed %d times (want 1); run status %s code %q err %v; model requests %d", n, m.status, m.failCode, err, e.model.Calls())
	}
}

// D47: data written before the single write can hold a call in a final state whose tool message is missing (the two writes
// were separated by a stop). The next worker appends the message from the stored result and the run goes on: the model is
// not refused and the tool is not executed again.
func TestAFinishedToolCallWithoutItsToolMessageIsAnsweredFromItsStoredResultOnResume(t *testing.T) {
	for _, tc := range []struct{ name, state string }{{"DONE", run.CallDone}} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			e.model.Script = refusesUnansweredToolCalls
			m := newMem(def(), user("please sum"), run.Input{})
			e.h.Store = &crashStore{memStore: m, splitAnswer: true}
			if end := <-runIn(e.h, context.Background()); !errors.Is(end.err, run.ErrFenced) {
				t.Fatalf("setup: %v", end.err)
			}
			if missing := finishedWithoutItsMessage(m); len(missing) != 1 {
				t.Fatalf("setup: want one finished call without its message, got %v (%+v)", missing, m.calls)
			}
			e.h.Store = m
			if err := e.h.Run(context.Background(), claim); err != nil {
				t.Fatal(err)
			}
			if n := len(e.mcp.Calls()); n != 1 || m.status != run.StatusCompleted || m.last().Content != "Result: 3" {
				t.Errorf("the gateway received %d calls (want 1); status %s (%s); last %q", n, m.status, m.failCode, m.last().Content)
			}
			if n := countEvents(m, agui.EventToolCallResult); n != 1 {
				t.Errorf("the stream has %d TOOL_CALL_RESULT events; want 1", n)
			}
		})
	}
}

// D47: every final state is repaired the same way: DONE, FAILED (an error result), DENIED, REJECTED and EXPIRED.
func TestEveryFinalStateOfAToolCallWithoutItsMessageIsRepaired(t *testing.T) {
	for _, state := range []string{run.CallDone, run.CallFailed, run.CallDenied, run.CallRejected, run.CallExpired} {
		t.Run(state, func(t *testing.T) {
			e := setup(t)
			e.model.Script = refusesUnansweredToolCalls
			d := def()
			msgs := append(user("please sum"), agui.Message{ID: "a1", Role: agui.RoleAssistant,
				ToolCalls: []agui.ToolCall{{ID: "call_x", Type: "function", Function: agui.FunctionCall{Name: "sum", Arguments: `{"a":1,"b":2}`}}}})
			m := newMem(d, msgs, run.Input{})
			m.rc.Run.StepCount = 1
			result := "3"
			if state != run.CallDone {
				result = "error: it did not work"
			}
			m.calls["call_x"] = &run.ToolCall{RunID: "run_1", ID: "call_x", Step: 1, MessageID: "a1", Tool: "sum", State: state, Result: result, IsError: state != run.CallDone, FinishedAt: time.Now()}
			e.h.Store = m
			if err := e.h.Run(context.Background(), claim); err != nil {
				t.Fatal(err)
			}
			var answer agui.Message
			for _, msg := range m.rc.Messages {
				if msg.Role == agui.RoleTool {
					answer = msg
				}
			}
			if answer.ToolCallID != "call_x" || !strings.Contains(answer.Content, result) || m.status != run.StatusCompleted || len(e.mcp.Calls()) != 0 {
				t.Errorf("answer %+v, status %s (%s), gateway calls %d", answer, m.status, m.failCode, len(e.mcp.Calls()))
			}
		})
	}
}

// interruptionCount is a Metrics that counts the tool calls found cut short, by resent and reason (D47).
type interruptionCount struct {
	countMetrics
	mu2  sync.Mutex
	seen []string
}

func (m *interruptionCount) ToolInterrupted(resent bool, reason string) {
	m.mu2.Lock()
	m.seen = append(m.seen, fmt.Sprintf("%v/%s", resent, reason))
	m.mu2.Unlock()
}

// D47: each tool call found cut short is counted by whether it was sent again and why it was cut short.
func TestInterruptedToolCallsAreCountedByResentAndReason(t *testing.T) {
	for _, tc := range []struct {
		annotations map[string]any
		want        string
	}{{nil, "false/worker_stopped"}, {map[string]any{"readOnlyHint": true}, "true/worker_stopped"}} {
		e := setup(t)
		e.mcp.Tools[0].Annotations = tc.annotations
		mt := &interruptionCount{}
		e.h.Metrics = mt
		m := firstWorkerDiesInTheCall(t, e, "gateway finished, result lost")
		_ = m
		if err := e.h.Run(context.Background(), claim); err != nil {
			t.Fatal(err)
		}
		if len(mt.seen) != 1 || mt.seen[0] != tc.want {
			t.Errorf("counted %v, want [%s]", mt.seen, tc.want)
		}
	}
}

// answerKilled: for the D49 tests, the worker dies at the answer of a call to dieOnToolAnswer (nothing of that write is
// stored, so the tool's sources are not either).
func (c *crashStore) answerKilled(id string) bool {
	return c.dieOnToolAnswer != "" && c.memStore.calls[id] != nil && c.memStore.calls[id].Tool == c.dieOnToolAnswer
}

// D47-14: in caller mode the thread is the caller's history. It ends with an assistant message whose tool call the
// caller ran and answered with a tool message, and HOST has no record of that call in this run. That is not a half-written
// step: the call is not created or run, the model is asked, and the run ends with its answer. (It was repaired as if its
// worker had died between the message and the records, so the run ended at once without a model call.)
func TestACallerThatAnswersItsOwnToolCallGetsAnswerFromTheModel(t *testing.T) {
	e := setup(t)
	thread := append(user("please sum"),
		agui.Message{ID: "a1", Role: agui.RoleAssistant, ToolCalls: []agui.ToolCall{{ID: "call_x", Type: "function", Function: agui.FunctionCall{Name: "eer_lookup", Arguments: `{"id":"p-1"}`}}}},
		agui.Message{ID: "t1", Role: agui.RoleTool, ToolCallID: "call_x", Content: `{"status":"completed","result":{"sum":42}}`})
	m := newMem(def(), thread, run.Input{ToolExecution: run.ToolExecutionCaller,
		Tools: []agui.Tool{{Name: "eer_lookup", Description: "look up", Parameters: []byte(`{"type":"object"}`)}}})
	m.cred = ""
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if e.model.Calls() != 1 {
		t.Errorf("the model was asked %d times; want once", e.model.Calls())
	}
	if len(m.calls) != 0 || len(e.mcp.Calls()) != 0 {
		t.Errorf("the caller's answered call was created or run: rows %+v, gateway calls %v", m.calls, e.mcp.Calls())
	}
	if m.status != run.StatusCompleted || !strings.HasPrefix(m.last().Content, "Result: ") {
		t.Errorf("run status %s (%s), last %+v", m.status, m.failCode, m.last())
	}
	if n := countEvents(m, agui.EventToolCallStart); n != 0 {
		t.Errorf("%d TOOL_CALL_START events; want none", n)
	}
}
