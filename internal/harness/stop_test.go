package harness

// D47: what a harness does when the worker is told to stop (run.WithStop): the call in progress finishes, nothing new is
// started, and the run is handed over at the next boundary with everything it stored consistent. Nothing here sends a real
// signal: the test closes the stop channel, as the worker does, and cancels the context where it means the end of the grace
// period. A second Run on the same store is the worker that takes the run over.

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"idp-host/internal/agui"
	"idp-host/internal/model"
	"idp-host/internal/run"
	"idp-host/tests/fixtures/fakemodel"
)

// runStoppable starts a Run that can be told to stop (close stop) and have its grace period end (cancel).
func runStoppable(h *Harness) (stop func(), cancel context.CancelFunc, done <-chan runEnd) {
	ch := make(chan struct{})
	ctx, cancel := context.WithCancel(run.WithStop(context.Background(), ch))
	var once atomic.Bool
	return func() {
		if once.CompareAndSwap(false, true) {
			close(ch)
		}
	}, cancel, runIn(h, ctx)
}

// twoSums: the model asks for two sums in one step and then answers.
func twoSums(messages []fakemodel.Message, _ []string) fakemodel.Turn {
	if spoken(messages).Role == "tool" {
		return fakemodel.Turn{Text: "done"}
	}
	return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "sum", Args: `{"a":1,"b":2}`}, {Name: "sum", Args: `{"a":3,"b":4}`}}}
}

// toolMessages are the ids of the calls the thread has an answer for.
func toolMessages(m *memStore) map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]bool{}
	for _, msg := range m.rc.Messages {
		if msg.Role == agui.RoleTool {
			out[msg.ToolCallID] = true
		}
	}
	return out
}

func within(t *testing.T, d time.Duration, done <-chan runEnd, what string) runEnd {
	t.Helper()
	select {
	case end := <-done:
		return end
	case <-time.After(d):
		t.Fatalf("%s: Run did not return within %v", what, d)
		return runEnd{}
	}
}

// secondWorker resumes the run with no stop, as the worker that takes it over does.
func secondWorker(t *testing.T, e *env, m *memStore) {
	t.Helper()
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatalf("second worker: %v", err)
	}
}

// D47: told to stop while a tool call runs, the harness lets that call finish, starts no other (the gateway and the model
// see no new request), gives the claim up at once, and leaves the run RUNNING; the next worker executes the rest and every
// tool has been executed exactly once.
func TestAStopWhileAToolCallRunsLetsItFinishStartsNothingAndHandsTheRunOver(t *testing.T) {
	e := setup(t)
	e.model.Script = twoSums
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	e.mcp.Tools[0].Handle = func(map[string]any) (string, bool) { started <- struct{}{}; <-release; return "3", false }
	m := newMem(def(), user("please sum twice"), run.Input{})
	e.h.Store = m
	stop, cancel, done := runStoppable(e.h)
	defer cancel()
	<-started
	stop()
	close(release) // the call in progress finishes after the stop
	end := within(t, 5*time.Second, done, "stop during a tool call")
	if end.err != nil {
		t.Fatalf("the hand-over is not an error: %v", end.err)
	}
	if n := len(e.mcp.Calls()); n != 1 {
		t.Errorf("the gateway received %d calls; the call in progress finishes and no other starts (want 1)", n)
	}
	if n := e.model.Calls(); n != 1 {
		t.Errorf("the model saw %d requests; want 1", n)
	}
	if c := m.calls["call_1_0"]; c.State != run.CallDone || !toolMessages(m)["call_1_0"] {
		t.Errorf("the call in progress should be DONE and answered: %+v", c)
	}
	if c := m.calls["call_1_1"]; c.State != run.CallConfirmed || toolMessages(m)["call_1_1"] {
		t.Errorf("the second call should rest CONFIRMED, not started: %+v", c)
	}
	if m.released != 1 || m.status != run.StatusRunning {
		t.Errorf("the claim should be given up once and the run left RUNNING: released %d, status %s", m.released, m.status)
	}
	secondWorker(t, e, m)
	if n := len(e.mcp.Calls()); n != 2 || m.status != run.StatusCompleted || m.last().Content != "done" {
		t.Errorf("after the hand-over: the gateway received %d calls (want 2, each once); status %s; last %q", n, m.status, m.last().Content)
	}
	if c := m.calls["call_1_1"]; c.State != run.CallDone {
		t.Errorf("the second call: %+v", c)
	}
}

// D47: told to stop while the model call runs, the harness lets it finish and stores the message with its calls; no call is
// started, the claim is given up, and the next worker executes the calls the model returned exactly once.
func TestAStopWhileAModelCallRunsStoresItsAnswerStartsNoToolCallAndHandsTheRunOver(t *testing.T) {
	e := setup(t)
	gate := make(chan struct{})
	reached := make(chan struct{})
	var first atomic.Bool
	first.Store(true)
	e.model.Script = func(messages []fakemodel.Message, tools []string) fakemodel.Turn {
		if first.CompareAndSwap(true, false) {
			close(reached)
			<-gate
		}
		return twoSums(messages, tools)
	}
	m := newMem(def(), user("please sum twice"), run.Input{})
	e.h.Store = m
	stop, cancel, done := runStoppable(e.h)
	defer cancel()
	<-reached
	stop()
	close(gate)
	if end := within(t, 5*time.Second, done, "stop during a model call"); end.err != nil {
		t.Fatalf("the hand-over is not an error: %v", end.err)
	}
	if n := len(e.mcp.Calls()); n != 0 {
		t.Errorf("the gateway received %d calls; want none", n)
	}
	if last := m.rc.Messages[len(m.rc.Messages)-1]; len(last.ToolCalls) != 2 || len(m.calls) != 2 {
		t.Errorf("the message and the records of its calls should be stored: %+v / %d records", last, len(m.calls))
	}
	for id, c := range m.calls {
		if c.State != run.CallConfirmed {
			t.Errorf("%s should rest CONFIRMED, not started: %+v", id, c)
		}
	}
	if m.released != 1 || m.status != run.StatusRunning {
		t.Errorf("released %d, status %s", m.released, m.status)
	}
	secondWorker(t, e, m)
	if n := len(e.mcp.Calls()); n != 2 || m.status != run.StatusCompleted {
		t.Errorf("after the hand-over: the gateway received %d calls (want 2); status %s", n, m.status)
	}
	if n := e.model.Calls(); n != 2 {
		t.Errorf("the model saw %d requests; want 2 (one per worker)", n)
	}
}

// D47: told to stop during the wait between two attempts at the model (D45), the wait ends, no further request is made, the
// step's row is closed as stopped and the run is handed over.
func TestAStopDuringTheWaitBetweenModelAttemptsEndsTheWaitAndHandsTheRunOver(t *testing.T) {
	e := setup(t)
	hits := addProvider(t, e, "flaky", 0, status(503, ""))
	e.h.Retry = map[string]model.RetryPolicy{"flaky": {Attempts: 3, Backoff: 20 * time.Second, MaxBackoff: 20 * time.Second}}
	m := newMem(onlyModel("flaky"), user("hello"), run.Input{})
	e.h.Store = m
	stop, cancel, done := runStoppable(e.h)
	defer cancel()
	time.Sleep(300 * time.Millisecond) // the first attempt failed and the wait began
	stopAt := time.Now()
	stop()
	end := within(t, 5*time.Second, done, "stop during a retry wait")
	if end.err != nil || end.at.Sub(stopAt) > 2*time.Second {
		t.Errorf("err %v after %v", end.err, end.at.Sub(stopAt))
	}
	if hits.Load() != 1 {
		t.Errorf("the provider saw %d requests; the retry must not be made (want 1)", hits.Load())
	}
	if m.released != 1 || m.status != run.StatusRunning || m.steps[0].ErrorCode != "stopped" {
		t.Errorf("released %d, status %s, step %+v", m.released, m.status, m.steps[0])
	}
}

// D47: past the grace period (the context ends) what is still in progress is cut as before: the call stays EXECUTING, the
// claim is not given up (the lease runs out as after a crash) and Run answers run.ErrFenced.
func TestPastTheGracePeriodACallInProgressIsCutAsBefore(t *testing.T) {
	e := setup(t)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	e.mcp.Tools[0].Handle = func(map[string]any) (string, bool) { started <- struct{}{}; <-release; return "3", false }
	m := newMem(def(), user("please sum"), run.Input{})
	e.h.Store = m
	stop, cancel, done := runStoppable(e.h)
	<-started
	stop()
	cancel() // the grace period is over
	end := within(t, 5*time.Second, done, "end of the grace period")
	if !errors.Is(end.err, run.ErrFenced) {
		t.Errorf("Run returned %v; want run.ErrFenced", end.err)
	}
	if c := m.calls["call_1_0"]; c.State != run.CallExecuting || m.released != 0 {
		t.Errorf("the call should rest EXECUTING and the claim be kept: %+v, released %d", c, m.released)
	}
}

// D47: a worker told to stop before it did anything hands the run over without a single call.
func TestAStopBeforeAnythingWasDoneHandsTheRunOverAtOnce(t *testing.T) {
	e := setup(t)
	m := newMem(def(), user("please sum"), run.Input{})
	e.h.Store = m
	stop, cancel, done := runStoppable(e.h)
	defer cancel()
	stop()
	_ = within(t, 5*time.Second, done, "stop at the start")
	if n := len(e.mcp.Bearers()); n != 0 || e.model.Calls() != 0 {
		t.Errorf("the gateway saw %d requests (not even tools/list) and the model %d; want none", n, e.model.Calls())
	}
	if m.released != 1 || m.status != run.StatusRunning {
		t.Errorf("released %d, status %s", m.released, m.status)
	}
}

// D47: calls recorded as denied are answered even when the worker was told to stop (nothing is started by it), and the
// calls that were not started stay for the next worker: no call is left without an answer or a way to get one.
func TestAStopAnswersTheDeniedCallsOfTheStepAndLeavesTheOthersForTheNextWorker(t *testing.T) {
	e := setup(t)
	gate := make(chan struct{})
	reached := make(chan struct{})
	var first atomic.Bool
	first.Store(true)
	e.model.Script = func(messages []fakemodel.Message, tools []string) fakemodel.Turn {
		if spoken(messages).Role == "tool" {
			return fakemodel.Turn{Text: "done"}
		}
		if first.CompareAndSwap(true, false) {
			close(reached)
			<-gate
		}
		return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "nope", Args: `{}`}, {Name: "sum", Args: `{"a":1,"b":2}`}}}
	}
	m := newMem(def(), user("please sum"), run.Input{})
	e.h.Store = m
	stop, cancel, done := runStoppable(e.h)
	defer cancel()
	<-reached
	stop()
	close(gate)
	_ = within(t, 5*time.Second, done, "stop")
	if got := toolMessages(m); !got["call_1_0"] || got["call_1_1"] {
		t.Errorf("the denied call should be answered and the other not: %v", got)
	}
	secondWorker(t, e, m)
	if got := toolMessages(m); !got["call_1_0"] || !got["call_1_1"] || len(e.mcp.Calls()) != 1 || m.status != run.StatusCompleted {
		t.Errorf("tool messages %v, gateway calls %d, status %s", got, len(e.mcp.Calls()), m.status)
	}
}

// D47: a call recorded as denied whose answer was not written (the worker died first) is answered by the next worker, with
// the reason stored with the record.
func TestADeniedCallThatWasNotAnsweredIsAnsweredByTheNextWorkerWithItsReason(t *testing.T) {
	e := setup(t)
	e.model.Script = func(messages []fakemodel.Message, tools []string) fakemodel.Turn {
		if spoken(messages).Role == "tool" {
			return fakemodel.Turn{Text: "done"}
		}
		return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "nope", Args: `{}`}}}
	}
	m := newMem(def(), user("please"), run.Input{})
	e.h.Store = &crashStore{memStore: m, dieOnState: run.CallDenied}
	if end := <-runIn(e.h, context.Background()); !errors.Is(end.err, run.ErrFenced) {
		t.Fatalf("setup: %v", end.err)
	}
	if c := m.calls["call_1_0"]; c.State != run.CallDenied || toolMessages(m)["call_1_0"] {
		t.Fatalf("setup: the call should be recorded as denied and unanswered: %+v", c)
	}
	secondWorker(t, e, m)
	var content string
	answers := 0
	for _, msg := range m.rc.Messages {
		if msg.Role == agui.RoleTool && msg.ToolCallID == "call_1_0" {
			content = msg.Content + msg.Error
			answers++
		}
	}
	if !strings.Contains(content, "tool not available to this agent") || answers != 1 || m.status != run.StatusCompleted {
		t.Errorf("tool message %q (%d of them), status %s", content, answers, m.status)
	}
}

// D47: calls that wait for the member end the turn as always when the worker was told to stop: the run goes to
// AWAITING_INPUT with its interrupt, it is not handed over, and the call that was not started runs after the decision.
func TestAStopDoesNotHideTheConfirmationsOfTheStep(t *testing.T) {
	e := setup(t)
	gate := make(chan struct{})
	reached := make(chan struct{})
	var first atomic.Bool
	first.Store(true)
	e.model.Script = func(messages []fakemodel.Message, tools []string) fakemodel.Turn {
		if first.CompareAndSwap(true, false) {
			close(reached)
			<-gate
		}
		return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "sum", Args: `{"a":1,"b":2}`}, {Name: "echo", Args: `{"text":"hi"}`}}}
	}
	m := newMem(def(), user("please"), run.Input{})
	e.h.Store = m
	stop, cancel, done := runStoppable(e.h)
	defer cancel()
	<-reached
	stop()
	close(gate)
	_ = within(t, 5*time.Second, done, "stop")
	if m.status != run.StatusAwaitingInput || m.released != 0 || len(e.mcp.Calls()) != 0 {
		t.Errorf("status %s, released %d, gateway calls %d; want awaiting input, none, none", m.status, m.released, len(e.mcp.Calls()))
	}
	if c := m.calls["call_1_0"]; c.State != run.CallConfirmed {
		t.Errorf("the automatic call was not started and rests CONFIRMED: %+v", c)
	}
	if n := countEvents(m, agui.EventRunFinished); n != 1 {
		t.Errorf("the stream should carry its interrupt: %d RUN_FINISHED", n)
	}
}

// a Release the worker has no use for in a run it keeps: Run without a stop never gives its claim up.
func TestARunWithoutAStopNeverGivesItsClaimUp(t *testing.T) {
	e := setup(t)
	m := newMem(def(), user("please sum"), run.Input{})
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil || m.released != 0 || m.status != run.StatusCompleted {
		t.Errorf("err %v, released %d, status %s", err, m.released, m.status)
	}
}

// D47: told to stop during the last call of a step, the harness does not start the next model call: the call finishes, its
// answer is stored, and the run is handed over; the next worker makes the second model request.
func TestAStopDuringTheLastCallOfAStepStartsNoNewModelCall(t *testing.T) {
	e := setup(t)
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	e.mcp.Tools[0].Handle = func(map[string]any) (string, bool) { started <- struct{}{}; <-release; return "3", false }
	m := newMem(def(), user("please sum"), run.Input{})
	e.h.Store = m
	stop, cancel, done := runStoppable(e.h)
	defer cancel()
	<-started
	stop()
	close(release)
	_ = within(t, 5*time.Second, done, "stop")
	if n := e.model.Calls(); n != 1 {
		t.Errorf("the model saw %d requests; the second must wait for the next worker", n)
	}
	if c := m.calls["call_1_0"]; c.State != run.CallDone || !toolMessages(m)["call_1_0"] || m.released != 1 || m.status != run.StatusRunning {
		t.Errorf("call %+v, released %d, status %s", c, m.released, m.status)
	}
	secondWorker(t, e, m)
	if n := e.model.Calls(); n != 2 || m.status != run.StatusCompleted || len(e.mcp.Calls()) != 1 {
		t.Errorf("model requests %d, status %s, gateway calls %d", n, m.status, len(e.mcp.Calls()))
	}
}

// D47: told to stop while a model request is in flight and that request fails, the harness neither retries nor asks another
// candidate: the step's row is closed as stopped and the run is handed over.
func TestAStopWhileAModelRequestFailsStartsNoRetry(t *testing.T) {
	e := setup(t)
	e.h.Retry = map[string]model.RetryPolicy{"fake": {Attempts: 1}} // without the stop, a failure here would end the run as MODEL_ERROR
	gate := make(chan struct{})
	reached := make(chan struct{})
	e.model.Script = func([]fakemodel.Message, []string) fakemodel.Turn {
		close(reached)
		<-gate
		return fakemodel.Turn{Status: 503}
	}
	m := newMem(def(), user("hello"), run.Input{})
	e.h.Store = m
	stop, cancel, done := runStoppable(e.h)
	defer cancel()
	<-reached
	stop()
	close(gate)
	end := within(t, 5*time.Second, done, "stop")
	if end.err != nil || e.model.Calls() != 1 || m.released != 1 || m.steps[0].ErrorCode != "stopped" {
		t.Errorf("err %v, model requests %d, released %d, step %+v", end.err, e.model.Calls(), m.released, m.steps[0])
	}
}
