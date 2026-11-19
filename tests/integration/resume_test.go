package integration

// D47 against real PostgreSQL, the real lease and the real Claim: what happens to a tool call that was in progress when
// the worker stopped, and to an assistant message stored without the records of its calls, and the worker's grace period.

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"idp-host/adapters/store/postgres"
	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/run"
	"idp-host/internal/worker"
	"idp-host/tests/fixtures/fakemodel"
)

// firstWorkerDiesInTheCall publishes an agent, starts a run and lets worker 1 (a 300 ms lease) claim it and die while the
// gateway is executing the model's call to "sum" (its context is cancelled while the handler is blocked inside the call;
// the handler still finishes afterwards, so the gateway completed the call and its result was lost). The call rests as
// EXECUTING. It returns the run's id and the claim worker 1 held.
func (e *env) firstWorkerDiesInTheCall(name, thread string) (string, *run.Claim) {
	e.t.Helper()
	e.publish(name, definition("edge", []string{"sum", "echo"}, map[string]string{"echo": agent.ConfirmConfirm}))
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	var first atomic.Bool
	first.Store(true)
	e.mcp.Tools[0].Handle = func(map[string]any) (string, bool) {
		started <- struct{}{}
		if first.CompareAndSwap(true, false) {
			<-release
		}
		return "3", false
	}
	r := e.must(202, "POST", "/v1/agents/"+name+"/runs", "carol", runBody(thread, "please sum"), "Idempotency-Key", thread)
	id := r["runId"].(string)
	ctx := context.Background()
	c1, err := e.store.Claim(ctx, "w1", 300*time.Millisecond)
	if err != nil || c1 == nil {
		e.t.Fatal(err, c1)
	}
	ctx1, kill := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- e.harness.Run(ctx1, c1) }()
	<-started
	kill() // worker 1 dies with the call in flight
	<-done
	close(release) // the gateway finishes the call it was given
	calls := e.must(200, "GET", "/v1/runs/"+id+"/tool-calls", "carol", nil)["toolCalls"].([]any)
	if len(calls) != 1 || calls[0].(map[string]any)["state"] != run.CallExecuting {
		e.t.Fatalf("setup: the call should rest as EXECUTING: %v", calls)
	}
	return id, c1
}

// resumes: the lease of the first worker runs out, another worker claims the run and runs the harness.
func (e *env) resumes(id string, c1 *run.Claim) {
	e.t.Helper()
	time.Sleep(400 * time.Millisecond)
	c2, err := e.store.Claim(context.Background(), "w2", time.Minute)
	if err != nil || c2 == nil || c2.RunID != id || c2.Epoch != c1.Epoch+1 {
		e.t.Fatalf("%v %+v", err, c2)
	}
	if err := e.harness.Run(context.Background(), c2); err != nil && !errors.Is(err, run.ErrFenced) {
		e.t.Fatalf("worker 2: %v", err)
	}
}

// interrupted returns the idp.host/tool.interrupted payloads of the run's stream.
func (e *env) interrupted(id string) []map[string]any {
	e.t.Helper()
	_, events, _ := e.sse("GET", "/v1/runs/"+id+"/events", "carol", nil)
	var out []map[string]any
	for _, ev := range events {
		if ev["name"] == agui.CustomToolInterrupted {
			out = append(out, ev["value"].(map[string]any))
		}
	}
	return out
}

// D47: a NON-idempotent tool call that was EXECUTING when the worker died is not sent again: the gateway received it once,
// the model reads that its outcome is unknown, the stream says so and the run completes.
func TestANonIdempotentToolCallThatWasExecutingWhenTheWorkerDiedIsNotSentAgainOnPostgres(t *testing.T) {
	e := newEnv(t)
	id, c1 := e.firstWorkerDiesInTheCall("assistant", "thr-r1a")
	e.resumes(id, c1)
	got := e.must(200, "GET", "/v1/runs/"+id, "carol", nil)
	if n := len(e.mcp.Calls()); n != 1 {
		t.Errorf("the gateway received tools/call sum %d times; want exactly 1 (run status %v)", n, got["status"])
	}
	if got["status"] != run.StatusCompleted {
		t.Errorf("run status %v, want completed: %v", got["status"], got)
	}
	calls := e.must(200, "GET", "/v1/runs/"+id+"/tool-calls", "carol", nil)["toolCalls"].([]any)
	if c := calls[0].(map[string]any); c["state"] != run.CallFailed || !strings.Contains(c["result"].(string), "outcome is unknown") {
		t.Errorf("the call should be FAILED with an outcome-unknown error: %v", c)
	}
	if evs := e.interrupted(id); len(evs) != 1 || evs[0]["resent"] != false || evs[0]["reason"] != "worker_stopped" {
		t.Errorf("want one idp.host/tool.interrupted {resent: false, reason: worker_stopped}: %v", evs)
	}
}

// D47: a read-only tool call that was EXECUTING when the worker died is sent again and the run completes with its result.
func TestAReadOnlyToolCallThatWasExecutingWhenTheWorkerDiedIsSentAgainOnPostgres(t *testing.T) {
	e := newEnv(t)
	e.mcp.Tools[0].Annotations = map[string]any{"readOnlyHint": true}
	id, c1 := e.firstWorkerDiesInTheCall("assistant", "thr-r1ro")
	e.resumes(id, c1)
	got := e.must(200, "GET", "/v1/runs/"+id, "carol", nil)
	if n := len(e.mcp.Calls()); n != 2 {
		t.Errorf("the gateway received tools/call sum %d times; want 2 (the second is the repetition)", n)
	}
	if got["status"] != run.StatusCompleted {
		t.Errorf("run status %v, want completed: %v", got["status"], got)
	}
	if evs := e.interrupted(id); len(evs) != 1 || evs[0]["resent"] != true {
		t.Errorf("want one idp.host/tool.interrupted {resent: true}: %v", evs)
	}
}

// legacyWrite stores the assistant message of a step as the two separate writes did before D47 and then dies before the
// second one: the thread ends with an assistant message whose tool calls have no tool_call rows.
type legacyWrite struct {
	*postgres.Store
}

func (l legacyWrite) AppendAssistantTurn(ctx context.Context, c *run.Claim, msg agui.Message, _ []run.ToolCall, ev []agui.Event) error {
	var kept []agui.Event
	for _, e := range ev {
		if n, _ := e["name"].(string); n != agui.CustomToolDenied && n != agui.CustomConfirmationRequired {
			kept = append(kept, e)
		}
	}
	if err := l.Store.AppendMessages(ctx, c, []agui.Message{msg}, kept); err != nil {
		return err
	}
	return run.ErrFenced
}

// D47: a thread that ends with an assistant message whose tool calls have no rows (written before the single
// transaction) is repaired on resume: the rows are created, the tool runs once, and the stream has each TOOL_CALL once.
func TestAnAssistantMessageWithoutToolCallRowsIsRepairedOnResumeOnPostgres(t *testing.T) {
	e := newEnv(t)
	e.publish("assistant", definition("edge", []string{"sum", "echo"}, map[string]string{"echo": agent.ConfirmConfirm}))
	r := e.must(202, "POST", "/v1/agents/assistant/runs", "carol", runBody("thr-legacy", "please sum"), "Idempotency-Key", "legacy")
	id := r["runId"].(string)
	c1, err := e.store.Claim(context.Background(), "w1", 300*time.Millisecond)
	if err != nil || c1 == nil {
		t.Fatal(err, c1)
	}
	e.harness.Store = legacyWrite{e.store}
	if err := e.harness.Run(context.Background(), c1); !errors.Is(err, run.ErrFenced) {
		t.Fatalf("setup: worker 1 should have died after the message: %v", err)
	}
	e.harness.Store = e.store
	if calls := e.must(200, "GET", "/v1/runs/"+id+"/tool-calls", "carol", nil)["toolCalls"].([]any); len(calls) != 0 {
		t.Fatalf("setup: the message should have no rows: %v", calls)
	}
	e.resumes(id, c1)
	got := e.must(200, "GET", "/v1/runs/"+id, "carol", nil)
	if n := len(e.mcp.Calls()); n != 1 || got["status"] != run.StatusCompleted {
		t.Errorf("the gateway received tools/call sum %d times (want 1); run status %v: %v", n, got["status"], got)
	}
	if calls := e.must(200, "GET", "/v1/runs/"+id+"/tool-calls", "carol", nil)["toolCalls"].([]any); len(calls) != 1 || calls[0].(map[string]any)["state"] != run.CallDone {
		t.Errorf("the repaired call should be DONE: %v", calls)
	}
	types, _, _ := e.sse("GET", "/v1/runs/"+id+"/events", "carol", nil)
	n := 0
	for _, ty := range types {
		if ty == agui.EventToolCallStart {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the stream has %d TOOL_CALL_START events; want 1: %v", n, types)
	}
}

// graceWorker is a worker pool of one on the env's store and harness, with a short lease, polling fast.
func (e *env) graceWorker(grace time.Duration) *worker.Worker {
	return &worker.Worker{ID: "grace", Store: e.store, Harness: e.harness, Concurrency: 1, Lease: 300 * time.Millisecond, Poll: 20 * time.Millisecond, StopGrace: grace}
}

// D47: a worker told to stop while a tool call is running lets it finish within the grace period: the gateway executes the
// call once, the run is not left EXECUTING and completes.
func TestAWorkerToldToStopLetsTheToolCallInProgressFinishOnPostgres(t *testing.T) {
	e := newEnv(t)
	e.publish("assistant", definition("edge", []string{"sum", "echo"}, nil))
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	e.mcp.Tools[0].Handle = func(map[string]any) (string, bool) { started <- struct{}{}; <-release; return "3", false }
	r := e.must(202, "POST", "/v1/agents/assistant/runs", "carol", runBody("thr-grace", "please sum"), "Idempotency-Key", "grace")
	id := r["runId"].(string)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	exited := make(chan struct{})
	go func() { defer close(exited); e.graceWorker(10 * time.Second).Run(ctx) }()
	<-started
	stop()                             // SIGTERM
	time.Sleep(600 * time.Millisecond) // longer than the lease: the heartbeat keeps the claim during the grace period
	close(release)
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker did not return after the call finished")
	}
	// the call finished within the grace period and the run was handed over after it (it is not left EXECUTING); the next
	// worker takes it at once and the model's second step completes it
	calls := e.must(200, "GET", "/v1/runs/"+id+"/tool-calls", "carol", nil)["toolCalls"].([]any)
	if len(calls) != 1 || calls[0].(map[string]any)["state"] != run.CallDone {
		t.Fatalf("the call should be DONE, not left EXECUTING: %v", calls)
	}
	c2, err := e.store.Claim(context.Background(), "w2", time.Minute)
	if err != nil || c2 == nil || c2.RunID != id {
		t.Fatalf("the run should be claimable at once: %v %+v", err, c2)
	}
	if err := e.harness.Run(context.Background(), c2); err != nil {
		t.Fatal(err)
	}
	got := e.must(200, "GET", "/v1/runs/"+id, "carol", nil)
	if n := len(e.mcp.Calls()); n != 1 || got["status"] != run.StatusCompleted {
		t.Errorf("the gateway received %d calls (want 1); run status %v: %v", n, got["status"], got)
	}
	if got["status"] != run.StatusCompleted {
		return // the event stream of a run that did not end does not end either
	}
	if evs := e.interrupted(id); len(evs) != 0 {
		t.Errorf("nothing was interrupted: %v", evs)
	}
}

// D47: past the grace period the call is cut short and the worker returns; the call rests as EXECUTING and whoever
// resumes the run handles it as a call interrupted by a worker (here: not sent again, its outcome unknown).
func TestAWorkerToldToStopCutsTheToolCallShortAfterTheGracePeriodOnPostgres(t *testing.T) {
	e := newEnv(t)
	e.publish("assistant", definition("edge", []string{"sum", "echo"}, nil))
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	var first atomic.Bool
	first.Store(true)
	e.mcp.Tools[0].Handle = func(map[string]any) (string, bool) {
		started <- struct{}{}
		if first.CompareAndSwap(true, false) {
			<-release
		}
		return "3", false
	}
	r := e.must(202, "POST", "/v1/agents/assistant/runs", "carol", runBody("thr-grace2", "please sum"), "Idempotency-Key", "grace2")
	id := r["runId"].(string)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	exited := make(chan struct{})
	go func() { defer close(exited); e.graceWorker(300 * time.Millisecond).Run(ctx) }()
	<-started
	at := time.Now()
	stop()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker blocked shutdown beyond the grace period")
	}
	if d := time.Since(at); d > 3*time.Second {
		t.Errorf("shutdown took %v", d)
	}
	close(release)
	calls := e.must(200, "GET", "/v1/runs/"+id+"/tool-calls", "carol", nil)["toolCalls"].([]any)
	if len(calls) != 1 || calls[0].(map[string]any)["state"] != run.CallExecuting {
		t.Fatalf("the call should rest as EXECUTING: %v", calls)
	}
	time.Sleep(400 * time.Millisecond)
	c2, err := e.store.Claim(context.Background(), "w2", time.Minute)
	if err != nil || c2 == nil || c2.RunID != id {
		t.Fatalf("%v %+v", err, c2)
	}
	if err := e.harness.Run(context.Background(), c2); err != nil {
		t.Fatal(err)
	}
	if n := len(e.mcp.Calls()); n != 1 {
		t.Errorf("the gateway received %d calls; want 1", n)
	}
	if evs := e.interrupted(id); len(evs) != 1 || evs[0]["resent"] != false {
		t.Errorf("want one idp.host/tool.interrupted {resent: false}: %v", evs)
	}
}

// D47: the assistant message and the records of its tool calls are one transaction in the real store: a write that is
// refused (here, two calls with the same id) leaves neither the message nor any row.
func TestTheAssistantMessageAndItsToolCallRecordsAreWrittenInOneTransactionOnPostgres(t *testing.T) {
	e := newEnv(t)
	e.publish("assistant", definition("edge", []string{"sum", "echo"}, nil))
	e.must(202, "POST", "/v1/agents/assistant/runs", "carol", runBody("thr-tx", "please sum"), "Idempotency-Key", "tx")
	ctx := context.Background()
	c, err := e.store.Claim(ctx, "w1", time.Minute)
	if err != nil || c == nil {
		t.Fatal(err, c)
	}
	before, err := e.store.Context(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	msg := agui.Message{ID: "asst_tx", Role: agui.RoleAssistant, ToolCalls: []agui.ToolCall{{ID: "c1", Type: "function", Function: agui.FunctionCall{Name: "sum", Arguments: "{}"}}}}
	dup := []run.ToolCall{{RunID: c.RunID, ID: "c1", Step: 1, MessageID: "asst_tx", Tool: "sum", Arguments: "{}", State: run.CallConfirmed},
		{RunID: c.RunID, ID: "c1", Step: 1, MessageID: "asst_tx", Tool: "sum", Arguments: "{}", State: run.CallConfirmed}}
	if err := e.store.AppendAssistantTurn(ctx, c, msg, dup, nil); err == nil {
		t.Fatal("a refused record was accepted")
	}
	after, err := e.store.Context(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Messages) != len(before.Messages) {
		t.Errorf("the message was stored although its records were refused: %d messages before, %d after", len(before.Messages), len(after.Messages))
	}
	if err := e.store.AppendAssistantTurn(ctx, c, msg, dup[:1], nil); err != nil {
		t.Fatal(err)
	}
	if after, err = e.store.Context(ctx, c); err != nil || len(after.Messages) != len(before.Messages)+1 || len(after.ToolCalls) != 1 || len(after.LastCalls) != 1 {
		t.Errorf("want the message and one record together: %v %d messages, %d calls, %v", err, len(after.Messages), len(after.ToolCalls), after.LastCalls)
	}
}

// D47: told to stop while a tool call runs, the worker lets it finish, starts no other call, and gives the claim up at once
// (its lease is a minute: the next Claim must not have to wait for it); the next worker executes the rest and every tool
// has been executed exactly once. The run stays RUNNING for the client, never failed.
func TestAStopHandsTheRunOverAtOnceWithoutStartingAnotherCallOnPostgres(t *testing.T) {
	e := newEnv(t)
	e.publish("assistant", definition("edge", []string{"sum", "echo"}, nil))
	e.model.Script = func(messages []fakemodel.Message, tools []string) fakemodel.Turn {
		if messages[len(messages)-1].Role == "tool" {
			return fakemodel.Turn{Text: "done"}
		}
		return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "sum", Args: `{"a":1,"b":2}`}, {Name: "sum", Args: `{"a":3,"b":4}`}}}
	}
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	var first atomic.Bool
	first.Store(true)
	e.mcp.Tools[0].Handle = func(map[string]any) (string, bool) {
		started <- struct{}{}
		if first.CompareAndSwap(true, false) {
			<-release
		}
		return "3", false
	}
	r := e.must(202, "POST", "/v1/agents/assistant/runs", "carol", runBody("thr-handover", "please sum twice"), "Idempotency-Key", "handover")
	id := r["runId"].(string)
	w := e.graceWorker(10 * time.Second)
	w.Lease = time.Minute
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	exited := make(chan struct{})
	go func() { defer close(exited); w.Run(ctx) }()
	<-started
	stop() // SIGTERM while the first call is running
	time.Sleep(200 * time.Millisecond)
	close(release)
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker did not return after the call finished")
	}
	if n := len(e.mcp.Calls()); n != 1 {
		t.Errorf("the gateway received %d calls; the call in progress finishes and no other starts (want 1)", n)
	}
	if st := e.must(200, "GET", "/v1/runs/"+id, "carol", nil)["status"]; st != run.StatusRunning {
		t.Errorf("the run should stay RUNNING for the client, got %v", st)
	}
	c2, err := e.store.Claim(context.Background(), "w2", time.Minute) // no sleep: the claim was given up
	if err != nil || c2 == nil || c2.RunID != id {
		t.Fatalf("the run should be claimable at once: %v %+v", err, c2)
	}
	if err := e.harness.Run(context.Background(), c2); err != nil {
		t.Fatal(err)
	}
	got := e.must(200, "GET", "/v1/runs/"+id, "carol", nil)
	if n := len(e.mcp.Calls()); n != 2 || got["status"] != run.StatusCompleted {
		t.Errorf("the gateway received %d calls (want 2, each once); run status %v: %v", n, got["status"], got)
	}
	for _, c := range e.must(200, "GET", "/v1/runs/"+id+"/tool-calls", "carol", nil)["toolCalls"].([]any) {
		if c.(map[string]any)["state"] != run.CallDone {
			t.Errorf("%v", c)
		}
	}
}

// legacyAnswer stores a call's final state and dies before its tool message, as the two separate writes could be.
type legacyAnswer struct {
	*postgres.Store
}

func (l legacyAnswer) AnswerToolCall(ctx context.Context, c *run.Claim, id, state, result string, isError bool, _ agui.Message, _ *run.SourceBatch, ev []agui.Event) error {
	if err := l.Store.UpdateToolCall(ctx, c, id, state, result, isError, nil); err != nil {
		return err
	}
	return run.ErrFenced
}

// D47: a call in a final state whose tool message was never written is answered from its stored result on resume: the next
// model request is not refused, the tool is not executed again, the run completes.
func TestAFinishedToolCallWithoutItsToolMessageIsRepairedOnResumeOnPostgres(t *testing.T) {
	e := newEnv(t)
	e.publish("assistant", definition("edge", []string{"sum", "echo"}, nil))
	r := e.must(202, "POST", "/v1/agents/assistant/runs", "carol", runBody("thr-legacy-answer", "please sum"), "Idempotency-Key", "legacy-answer")
	id := r["runId"].(string)
	c1, err := e.store.Claim(context.Background(), "w1", 300*time.Millisecond)
	if err != nil || c1 == nil {
		t.Fatal(err, c1)
	}
	e.harness.Store = legacyAnswer{e.store}
	if err := e.harness.Run(context.Background(), c1); !errors.Is(err, run.ErrFenced) {
		t.Fatalf("setup: worker 1 should have died before the tool message: %v", err)
	}
	e.harness.Store = e.store
	if calls := e.must(200, "GET", "/v1/runs/"+id+"/tool-calls", "carol", nil)["toolCalls"].([]any); len(calls) != 1 || calls[0].(map[string]any)["state"] != run.CallDone {
		t.Fatalf("setup: the call should be DONE: %v", calls)
	}
	e.resumes(id, c1)
	got := e.must(200, "GET", "/v1/runs/"+id, "carol", nil)
	if n := len(e.mcp.Calls()); n != 1 || got["status"] != run.StatusCompleted {
		t.Errorf("the gateway received %d calls (want 1); run status %v: %v", n, got["status"], got)
	}
	msgs := e.must(200, "GET", "/v1/threads/thr-legacy-answer/messages", "carol", nil)["messages"].([]any)
	if last := msgs[len(msgs)-1].(map[string]any); last["content"] != "Result: 3" {
		t.Errorf("the run's answer: %v", last)
	}
}

// D47: a call's final state and its tool message are one transaction in the real store: a message that is refused (an id the
// thread already has) leaves the call as it was.
func TestAToolCallsFinalStateAndItsToolMessageAreWrittenInOneTransactionOnPostgres(t *testing.T) {
	e := newEnv(t)
	e.publish("assistant", definition("edge", []string{"sum", "echo"}, nil))
	e.must(202, "POST", "/v1/agents/assistant/runs", "carol", runBody("thr-answer-tx", "please sum"), "Idempotency-Key", "answer-tx")
	ctx := context.Background()
	c, err := e.store.Claim(ctx, "w1", time.Minute)
	if err != nil || c == nil {
		t.Fatal(err, c)
	}
	msg := agui.Message{ID: "asst_a", Role: agui.RoleAssistant, ToolCalls: []agui.ToolCall{{ID: "c1", Type: "function", Function: agui.FunctionCall{Name: "sum", Arguments: "{}"}}}}
	rec := []run.ToolCall{{RunID: c.RunID, ID: "c1", Step: 1, MessageID: "asst_a", Tool: "sum", Arguments: "{}", State: run.CallConfirmed}}
	if err := e.store.AppendAssistantTurn(ctx, c, msg, rec, nil); err != nil {
		t.Fatal(err)
	}
	clash := agui.Message{ID: "asst_a", Role: agui.RoleTool, ToolCallID: "c1", Content: "3"} // the id of a message the thread has
	if err := e.store.AnswerToolCall(ctx, c, "c1", run.CallDone, "3", false, clash, nil, nil); err == nil {
		t.Fatal("a refused message was accepted")
	}
	if got, err := e.store.Context(ctx, c); err != nil || len(got.ToolCalls) != 1 || got.ToolCalls[0].State != run.CallConfirmed {
		t.Errorf("the call must be as it was: %v %+v", err, got.ToolCalls)
	}
	ok := agui.Message{ID: "tool_c1", Role: agui.RoleTool, ToolCallID: "c1", Content: "3"}
	if err := e.store.AnswerToolCall(ctx, c, "c1", run.CallDone, "3", false, ok, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := e.store.Context(ctx, c); err != nil || len(got.ToolCalls) != 0 || len(got.LastCalls) != 1 || got.LastCalls[0].State != run.CallDone || got.Messages[len(got.Messages)-1].ID != "tool_c1" {
		t.Errorf("the call and its message should be there together: %v %+v", err, got.LastCalls)
	}
	// and the claim can be given up: another Claim takes the run at once, and this one is fenced
	if err := e.store.Release(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Heartbeat(ctx, c, time.Minute); !errors.Is(err, run.ErrFenced) {
		t.Errorf("a released claim must be fenced: %v", err)
	}
	if c2, err := e.store.Claim(ctx, "w2", time.Minute); err != nil || c2 == nil || c2.Epoch != c.Epoch+1 {
		t.Errorf("the run should be claimable at once: %v %+v", err, c2)
	}
}
