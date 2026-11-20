package harness

// D53: the calls of one step that only read run at the same time. Every test here uses the gateway fixture with annotated
// tools that can be held back, and its record of when each call began and ended. In memory; the interruption and resume cases
// on PostgreSQL are in tests/integration/parallel_test.go.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/credential"
	"idp-host/internal/run"
	"idp-host/tests/fixtures/fakemcp"
	"idp-host/tests/fixtures/fakemodel"
)

// installTools adds to the gateway fixture: read (readOnlyHint), write (idempotentHint only: it may be repeated, it may not
// run beside another call), plain (no annotations), ask (read-only, but the release's policy has the member confirm it) and
// bad (read-only, reports an error for the argument fail). Every call takes id and ms (how long the gateway holds it back).
func installTools(e *env) {
	hold := func(args map[string]any) time.Duration {
		ms, _ := args["ms"].(float64)
		return time.Duration(ms) * time.Millisecond
	}
	answer := func(prefix string) func(map[string]any) (string, bool) {
		return func(args map[string]any) (string, bool) {
			id, _ := args["id"].(string)
			if f, _ := args["fail"].(bool); f {
				return prefix + " " + id + " failed", true
			}
			return prefix + " " + id, false
		}
	}
	schema := `{"type":"object","properties":{"id":{"type":"string"},"ms":{"type":"number"},"fail":{"type":"boolean"}}}`
	ro := map[string]any{"readOnlyHint": true}
	e.mcp.Tools = append(e.mcp.Tools,
		fakemcp.Tool{Name: "read", InputSchema: schema, Annotations: ro, Handle: answer("read"), DelayFor: hold},
		fakemcp.Tool{Name: "write", InputSchema: schema, Annotations: map[string]any{"idempotentHint": true}, Handle: answer("write"), DelayFor: hold},
		fakemcp.Tool{Name: "plain", InputSchema: schema, Handle: answer("plain"), DelayFor: hold},
		fakemcp.Tool{Name: "ask", InputSchema: schema, Annotations: ro, Handle: answer("ask"), DelayFor: hold},
	)
}

func pdef() agent.Definition {
	d := def()
	d.Tools.Allow = []string{agent.AllowAll}
	d.Confirmation = agent.ConfirmationPolicy{Default: agent.ConfirmAuto, Tools: map[string]string{"ask": agent.ConfirmConfirm}}
	return d
}

// rd, wr and pl are calls of the model: a tool, an id for the call to be told by, and how long the gateway holds it back.
func rd(id string, ms int) fakemodel.Call { return tcall("read", id, ms) }
func wr(id string, ms int) fakemodel.Call { return tcall("write", id, ms) }
func pl(id string, ms int) fakemodel.Call { return tcall("plain", id, ms) }
func tcall(tool, id string, ms int) fakemodel.Call {
	return fakemodel.Call{Name: tool, Args: fmt.Sprintf(`{"id":%q,"ms":%d}`, id, ms)}
}

// asking: the model asks for these calls in its first step and answers "done" once it has the answers.
func asking(calls ...fakemodel.Call) fakemodel.Script {
	return func(messages []fakemodel.Message, _ []string) fakemodel.Turn {
		if spoken(messages).Role == "tool" {
			return fakemodel.Turn{Text: "done"}
		}
		return fakemodel.Turn{ToolCalls: calls}
	}
}

func pSetup(t *testing.T, calls ...fakemodel.Call) (*env, *memStore) {
	t.Helper()
	e := setup(t)
	installTools(e)
	e.model.Script = asking(calls...)
	m := newMem(pdef(), user("go"), run.Input{})
	e.h.Store = m
	return e, m
}

// threadOrder is the call ids of the tool messages of the thread, in the order the thread holds them.
func threadOrder(m *memStore) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, msg := range m.rc.Messages {
		if msg.Role == agui.RoleTool {
			out = append(out, msg.ToolCallID)
		}
	}
	return out
}

// resultOrder is the call ids of the TOOL_CALL_RESULT events, in the order the log holds them.
func resultOrder(m *memStore) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, ev := range m.events {
		if ev.Type() == agui.EventToolCallResult {
			out = append(out, ev["toolCallId"].(string))
		}
	}
	return out
}

func answerOf(m *memStore, callID string) agui.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, msg := range m.rc.Messages {
		if msg.Role == agui.RoleTool && msg.ToolCallID == callID {
			return msg
		}
	}
	return agui.Message{}
}

func callState(m *memStore, id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[id].State
}

func callIDs(n int) []string {
	var out []string
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("call_1_%d", i))
	}
	return out
}

// span is when the gateway had the call with this id argument.
func span(t *testing.T, e *env, id string) fakemcp.Call {
	t.Helper()
	for _, c := range e.mcp.Calls() {
		if c.Args["id"] == id {
			return c
		}
	}
	t.Fatalf("the gateway never received a call with id %q", id)
	return fakemcp.Call{}
}

func overlap(a, b fakemcp.Call) bool { return a.Start.Before(b.End) && b.Start.Before(a.End) }

func same(a, b []string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

// Three read-only calls of two seconds each, asked for in one step, take about two seconds and not six; the thread holds
// their answers in the order of the calls.
func TestThreeReadOnlyCallsOfTwoSecondsFinishInAboutTwoSeconds(t *testing.T) {
	e, m := pSetup(t, rd("a", 2000), rd("b", 2000), rd("c", 2000))
	start := time.Now()
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	t.Logf("three read-only calls of 2 s: the run took %v; gateway: at most %d calls at once", took.Round(10*time.Millisecond), e.mcp.MaxInFlight())
	if took < 2*time.Second || took > 3500*time.Millisecond {
		t.Errorf("the run took %v; three calls of 2 s together should take about 2 s (one after the other: 6 s)", took)
	}
	if e.mcp.MaxInFlight() != 3 {
		t.Errorf("at most %d calls were in progress at the gateway at once, want 3", e.mcp.MaxInFlight())
	}
	if m.status != run.StatusCompleted || !same(threadOrder(m), callIDs(3)) {
		t.Errorf("status %s, tool messages in the order %v", m.status, threadOrder(m))
	}
	for i, id := range []string{"a", "b", "c"} {
		if got := answerOf(m, fmt.Sprintf("call_1_%d", i)).Content; got != "read "+id {
			t.Errorf("call %d answered %q", i, got)
		}
	}
}

// The calls finish in another order than they were asked for (the last is the quickest): the thread and the event log hold
// the answers in the order of the calls all the same, and the call that is ready first is not written before the ones
// before it.
func TestAnswersAreWrittenInTheOrderOfTheCallsWhateverOrderTheyFinishIn(t *testing.T) {
	e, m := pSetup(t, rd("a", 600), rd("b", 300), rd("c", 50), rd("d", 150))
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	a, c := span(t, e, "a"), span(t, e, "c")
	if !c.End.Before(a.End) {
		t.Fatalf("the test needs the quick call to finish first: %v %v", a.End, c.End)
	}
	if !same(threadOrder(m), callIDs(4)) || !same(resultOrder(m), callIDs(4)) {
		t.Errorf("thread %v, event log %v; want the order of the calls %v", threadOrder(m), resultOrder(m), callIDs(4))
	}
}

// With the setting at 4 or at 1 a recorded step leaves the same thread and the same event log, and both are the ones the
// code before D53 left (testdata/sequential_step.golden, written by that code).
func TestTheSettingAtOneAndTheGroupLeaveTheThreadAndEventLogOfTheSequentialPath(t *testing.T) {
	var transcripts [2]string
	for i, parallel := range []int{1, 4} {
		e, m := goldenRun(t)
		e.h.Gateways["edge"].ParallelCalls = parallel
		if err := e.h.Run(context.Background(), claim); err != nil {
			t.Fatal(err)
		}
		transcripts[i] = transcript(m)
		if parallel == 4 && e.mcp.MaxInFlight() < 2 {
			t.Errorf("the group did not run at once (%d in flight): the comparison says nothing", e.mcp.MaxInFlight())
		}
		if parallel == 1 && e.mcp.MaxInFlight() != 1 {
			t.Errorf("with the setting at 1, %d calls were in flight", e.mcp.MaxInFlight())
		}
	}
	golden := readGolden(t)
	if transcripts[0] != golden {
		t.Errorf("with the setting at 1 the thread and the event log differ from the sequential path's:\n%s\nwant\n%s", transcripts[0], golden)
	}
	if transcripts[1] != golden {
		t.Errorf("with the group the thread and the event log differ from the sequential path's:\n%s\nwant\n%s", transcripts[1], golden)
	}
}

// read, write, read: the write is the only call of its group, between two groups of one; nothing runs beside it. Two writes
// that are only idempotent never run together, and a tool without annotations is never beside another call.
func TestACallThatMayWriteRunsAloneBetweenTwoGroupsOfOne(t *testing.T) {
	e, m := pSetup(t, rd("a", 200), wr("w", 200), rd("b", 200))
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	a, w, b := span(t, e, "a"), span(t, e, "w"), span(t, e, "b")
	if overlap(a, w) || overlap(w, b) || overlap(a, b) || e.mcp.MaxInFlight() != 1 {
		t.Errorf("the calls ran beside each other: in flight at most %d", e.mcp.MaxInFlight())
	}
	if !same(threadOrder(m), callIDs(3)) {
		t.Errorf("%v", threadOrder(m))
	}
}

func TestIdempotentIsNotReadOnlyTwoIdempotentWritesNeverRunTogether(t *testing.T) {
	e, m := pSetup(t, wr("w1", 150), wr("w2", 150), wr("w3", 150))
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if e.mcp.MaxInFlight() != 1 || m.status != run.StatusCompleted {
		t.Errorf("%d idempotent writes were in flight at once; status %s", e.mcp.MaxInFlight(), m.status)
	}
}

func TestAToolWithoutAnnotationsIsNeverRunBesideAnotherCall(t *testing.T) {
	e, m := pSetup(t, rd("a", 300), pl("p", 300), rd("b", 300), rd("c", 300))
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	a, p, b, c := span(t, e, "a"), span(t, e, "p"), span(t, e, "b"), span(t, e, "c")
	if overlap(a, p) || overlap(p, b) || overlap(p, c) {
		t.Errorf("the tool without annotations ran beside another call")
	}
	if !overlap(b, c) {
		t.Errorf("the two reads after it should have run together")
	}
	if !same(threadOrder(m), callIDs(4)) {
		t.Errorf("%v", threadOrder(m))
	}
}

// The operator's sequentialTools keeps a read-only tool from running beside another call; neverResendTools does not (it is
// about repeating a call after a crash, and says nothing about running together).
func TestSequentialToolsKeepAReadOnlyToolAlone(t *testing.T) {
	e, _ := pSetup(t, rd("a", 300), rd("b", 300), rd("c", 300))
	e.h.Gateways["edge"].SequentialTools = []string{"re*"}
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if e.mcp.MaxInFlight() != 1 {
		t.Errorf("%d calls of a tool named in sequentialTools were in flight at once", e.mcp.MaxInFlight())
	}
	e2, _ := pSetup(t, rd("a", 300), rd("b", 300), rd("c", 300))
	e2.h.Gateways["edge"].NeverResendTools = []string{"read"}
	if err := e2.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if e2.mcp.MaxInFlight() != 3 {
		t.Errorf("neverResendTools must not keep a read-only tool from running beside others: %d in flight", e2.mcp.MaxInFlight())
	}
}

// The bound: eight calls and a bound of four have at most four in flight at the gateway, and the other four wait for a place.
func TestNoMoreCallsThanTheBoundAreInFlight(t *testing.T) {
	var calls []fakemodel.Call
	for i := 0; i < 8; i++ {
		calls = append(calls, rd(fmt.Sprint("r", i), 300))
	}
	e, m := pSetup(t, calls...)
	e.h.Gateways["edge"].ParallelCalls = 4
	start := time.Now()
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if got := e.mcp.MaxInFlight(); got != 4 {
		t.Errorf("%d calls were in flight at once; want 4 (the bound), not 8 and not fewer", got)
	}
	if took := time.Since(start); took < 550*time.Millisecond {
		t.Errorf("8 calls of 300 ms with a bound of 4 took %v: two rounds are 600 ms", took)
	}
	if !same(threadOrder(m), callIDs(8)) {
		t.Errorf("%v", threadOrder(m))
	}
}

// A call that needs the member's confirmation splits the group and parks the step as it always did: the reads before and
// after it run (in two groups), it waits, and the turn ends AWAITING_INPUT.
func TestACallThatNeedsConfirmationSplitsTheGroupAndParksTheStep(t *testing.T) {
	e, m := pSetup(t, rd("a", 300), rd("b", 300), tcall("ask", "q", 300), rd("c", 300), rd("d", 300))
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if m.status != run.StatusAwaitingInput {
		t.Fatalf("status %s, want AWAITING_INPUT", m.status)
	}
	if e.mcp.MaxInFlight() != 2 {
		t.Errorf("%d calls in flight; the two groups of two must not run together (4)", e.mcp.MaxInFlight())
	}
	a, b, c, d := span(t, e, "a"), span(t, e, "b"), span(t, e, "c"), span(t, e, "d")
	if !overlap(a, b) || !overlap(c, d) || overlap(a, c) || overlap(b, d) {
		t.Errorf("a and b, and c and d, run together, the two pairs one after the other")
	}
	if callState(m, "call_1_2") != run.CallPendingConfirmation || answerOf(m, "call_1_2").ID != "" {
		t.Errorf("the call that needs the member should wait, unanswered: %s", callState(m, "call_1_2"))
	}
	if want := []string{"call_1_0", "call_1_1", "call_1_3", "call_1_4"}; !same(threadOrder(m), want) {
		t.Errorf("%v", threadOrder(m))
	}
	for _, c := range e.mcp.Calls() {
		if c.Name == "ask" {
			t.Errorf("the call that needs confirmation was sent")
		}
	}
}

// One call failing (the tool reports an error, the gateway refuses the arguments) leaves the others answered with their own
// results, in order.
func TestOneCallFailingLeavesTheOthersAnswered(t *testing.T) {
	e, m := pSetup(t, rd("a", 100), fakemodel.Call{Name: "read", Args: `{"id":"b","ms":100,"fail":true}`}, rd("c", 100), fakemodel.Call{Name: "read", Args: `{"id":"d","ms":"x"}`}, rd("e", 100))
	e.mcp.Tools[len(e.mcp.Tools)-4].Refuse = func(args map[string]any) string {
		if _, bad := args["ms"].(string); bad {
			return "ms must be a number"
		}
		return ""
	}
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if m.status != run.StatusCompleted || !same(threadOrder(m), callIDs(5)) {
		t.Fatalf("%s %v", m.status, threadOrder(m))
	}
	want := []string{run.CallDone, run.CallFailed, run.CallDone, run.CallFailed, run.CallDone}
	for i, w := range want {
		if got := callState(m, fmt.Sprintf("call_1_%d", i)); got != w {
			t.Errorf("call %d is %s, want %s", i, got, w)
		}
	}
	if c := answerOf(m, "call_1_2").Content; c != "read c" {
		t.Errorf("a sibling's answer: %q", c)
	}
	if c := answerOf(m, "call_1_3"); !strings.Contains(c.Error, "ms must be a number") {
		t.Errorf("the refusal's reason should reach the model: %+v", c)
	}
	if e.mcp.MaxInFlight() < 3 {
		t.Errorf("the calls did not run together (%d)", e.mcp.MaxInFlight())
	}
}

// A call refused by D51's rule and a call the policy denies are answered in their place without holding up the others, and
// do not split the group.
func TestADeniedCallIsAnsweredInItsPlaceAndDoesNotHoldUpTheOthers(t *testing.T) {
	e, m := pSetup(t, rd("a", 300), fakemodel.Call{Name: "marked", Args: `{"id":"m"}`}, rd("b", 300), fakemodel.Call{Name: "nowhere", Args: `{}`}, rd("c", 300))
	e.mcp.Tools = append(e.mcp.Tools, fakemcp.Tool{Name: "marked", InputSchema: `{"type":"object"}`, Annotations: map[string]any{"readOnlyHint": true},
		Meta: map[string]any{"io.idp.telos/external": true}, Handle: func(map[string]any) (string, bool) { return "sent out", false }})
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if !same(threadOrder(m), callIDs(5)) {
		t.Errorf("%v", threadOrder(m))
	}
	if callState(m, "call_1_1") != run.CallDenied || callState(m, "call_1_3") != run.CallDenied {
		t.Errorf("%s %s", callState(m, "call_1_1"), callState(m, "call_1_3"))
	}
	if !strings.Contains(answerOf(m, "call_1_1").Error, "not run") || !strings.Contains(answerOf(m, "call_1_3").Error, "not available") {
		t.Errorf("%+v %+v", answerOf(m, "call_1_1"), answerOf(m, "call_1_3"))
	}
	if e.mcp.MaxInFlight() != 3 {
		t.Errorf("the three reads should run together past the denied calls: %d", e.mcp.MaxInFlight())
	}
	for _, c := range e.mcp.Calls() {
		if c.Name == "marked" {
			t.Errorf("the refused call was sent")
		}
	}
}

// ---- interruption: the deadline, a cancel and a stop reach every call of a group ----

// A cancel while a group runs: every call in flight is recorded as interrupted, the calls that had no place yet and the call
// after the group as not run; the thread answers all of them in call order and the run ends canceled within a second or so.
func TestACancelDuringAGroupReachesEveryCall(t *testing.T) {
	var calls []fakemodel.Call
	for i := 0; i < 6; i++ {
		calls = append(calls, rd(fmt.Sprint("r", i), 5000))
	}
	calls = append(calls, wr("w", 100))
	e, m := pSetup(t, calls...)
	e.h.Gateways["edge"].ParallelCalls = 4
	e.h.CancelPoll = 20 * time.Millisecond
	done := runIn(e.h, context.Background())
	waitFor(t, func() bool { return e.mcp.MaxInFlight() == 4 })
	m.mu.Lock()
	m.rc.Run.CancelRequested = true
	m.mu.Unlock()
	cancelAt := time.Now()
	end := within(t, 4*time.Second, done, "a cancel during a group")
	t.Logf("the run ended %v after the cancel", end.at.Sub(cancelAt).Round(10*time.Millisecond))
	if m.status != run.StatusCanceled {
		t.Errorf("status %s (%v)", m.status, end.err)
	}
	if !same(threadOrder(m), callIDs(7)) {
		t.Errorf("every call must be answered, in order: %v", threadOrder(m))
	}
	for i := 0; i < 7; i++ {
		id := fmt.Sprintf("call_1_%d", i)
		msg := answerOf(m, id)
		switch {
		case i < 4 && !strings.Contains(msg.Error, "interrupted: the owner canceled the run while this call was running"):
			t.Errorf("call %d (in flight): %+v", i, msg)
		case i >= 4 && !strings.Contains(msg.Error, "not run: the owner canceled the run before this call started"):
			t.Errorf("call %d (not started): %+v", i, msg)
		}
		if callState(m, id) != run.CallFailed {
			t.Errorf("call %d is %s", i, callState(m, id))
		}
	}
	if n := len(interruptions(m)); n != 4 {
		t.Errorf("%d idp.host/tool.interrupted events, want 4 (one per call in flight)", n)
	}
	if n := len(e.mcp.Calls()); n != 4 {
		t.Errorf("the gateway received %d calls, want 4: nothing starts after the cancel", n)
	}
}

// The deadline passes while a group runs: the same, and the run is failed RUN_TIMEOUT.
func TestTheDeadlineDuringAGroupReachesEveryCall(t *testing.T) {
	e, m := pSetup(t, rd("a", 5000), rd("b", 5000), rd("c", 5000), wr("w", 100))
	m.rc.Run.DeadlineAt = time.Now().Add(500 * time.Millisecond)
	start := time.Now()
	err := e.h.Run(context.Background(), claim)
	var f *run.Failure
	if !asFailure(err, &f) || f.Code != run.FailTimeout || m.status != run.StatusFailed || time.Since(start) > 3*time.Second {
		t.Fatalf("%v %s after %v", err, m.status, time.Since(start))
	}
	if !same(threadOrder(m), callIDs(4)) {
		t.Errorf("%v", threadOrder(m))
	}
	for i := 0; i < 3; i++ {
		if msg := answerOf(m, fmt.Sprintf("call_1_%d", i)); !strings.Contains(msg.Error, "interrupted: the run's time limit passed while this call was running") {
			t.Errorf("call %d: %+v", i, msg)
		}
	}
	if msg := answerOf(m, "call_1_3"); !strings.Contains(msg.Error, "not run: the run's time limit passed before this call started") {
		t.Errorf("%+v", msg)
	}
}

// A stop while a group runs: the calls in flight finish and are answered, no other starts, the claim is given up and the run
// left RUNNING; the next worker runs the rest, and every call was executed exactly once, answered once, in order.
func TestAStopDuringAGroupLetsTheCallsInFlightFinishAndStartsNoOther(t *testing.T) {
	e, m := pSetup(t, rd("a", 0), rd("b", 0), rd("c", 0), rd("d", 0))
	e.h.Gateways["edge"].ParallelCalls = 2
	release := make(chan struct{})
	var started atomic.Int32
	for i := range e.mcp.Tools {
		if e.mcp.Tools[i].Name == "read" {
			h := e.mcp.Tools[i].Handle
			e.mcp.Tools[i].Handle = func(args map[string]any) (string, bool) {
				started.Add(1)
				<-release
				return h(args)
			}
		}
	}
	stop, cancel, done := runStoppable(e.h)
	defer cancel()
	waitFor(t, func() bool { return started.Load() == 2 })
	stop()
	time.Sleep(100 * time.Millisecond) // the launcher is waiting for a place: it must not take one
	close(release)
	if end := within(t, 5*time.Second, done, "a stop during a group"); end.err != nil {
		t.Fatalf("the hand-over is not an error: %v", end.err)
	}
	if n := len(e.mcp.Calls()); n != 2 {
		t.Errorf("the gateway received %d calls; the two in flight finish and no other starts", n)
	}
	for i, want := range []string{run.CallDone, run.CallDone, run.CallConfirmed, run.CallConfirmed} {
		if got := callState(m, fmt.Sprintf("call_1_%d", i)); got != want {
			t.Errorf("call %d is %s, want %s", i, got, want)
		}
	}
	if !same(threadOrder(m), callIDs(2)) || m.released != 1 || m.status != run.StatusRunning {
		t.Errorf("thread %v, released %d, status %s", threadOrder(m), m.released, m.status)
	}
	secondWorker(t, e, m)
	if n := len(e.mcp.Calls()); n != 4 || m.status != run.StatusCompleted || !same(threadOrder(m), callIDs(4)) {
		t.Errorf("after the hand-over: %d gateway calls (want 4, each once), status %s, thread %v", n, m.status, threadOrder(m))
	}
}

// After the grace period the calls in flight are cut short: nothing is written for them (the claim is the next worker's),
// they rest EXECUTING, and the worker returns.
func TestTheEndOfTheGracePeriodCutsEveryCallOfAGroupShort(t *testing.T) {
	e, m := pSetup(t, rd("a", 5000), rd("b", 5000))
	stop, cancel, done := runStoppable(e.h)
	defer cancel()
	waitFor(t, func() bool { return e.mcp.MaxInFlight() == 2 })
	stop()
	cancel()
	end := within(t, 3*time.Second, done, "the end of the grace period")
	if !asFatal(end.err) {
		t.Errorf("%v", end.err)
	}
	for _, id := range []string{"call_1_0", "call_1_1"} {
		if callState(m, id) != run.CallExecuting {
			t.Errorf("%s is %s, want EXECUTING", id, callState(m, id))
		}
	}
	if len(threadOrder(m)) != 0 {
		t.Errorf("nothing is written for a call cut short by the end of the grace period: %v", threadOrder(m))
	}
}

// ---- a worker dying during a group, another resuming ----

// dying fences the claim at the n-th answer of a call, as a worker that dies there does: that write and every later one
// is lost.
type dying struct {
	*memStore
	at, n int
}

func (d *dying) AnswerToolCall(ctx context.Context, c *run.Claim, id, state, result string, isErr bool, msg agui.Message, src *run.SourceBatch, ev []agui.Event) error {
	d.mu.Lock()
	d.n++
	if d.n >= d.at {
		d.fenced = true
	}
	d.mu.Unlock()
	return d.memStore.AnswerToolCall(ctx, c, id, state, result, isErr, msg, src, ev)
}

// Context lists the calls that are left by call id, highest first: the real store lists them by step, creation time (one
// instant for the calls of a step) and id, which is not the order of the calls.
func (d *dying) Context(ctx context.Context, c *run.Claim) (*run.Context, error) {
	rc, err := d.memStore.Context(ctx, c)
	if rc != nil {
		sort.Slice(rc.ToolCalls, func(i, j int) bool { return rc.ToolCalls[i].ID > rc.ToolCalls[j].ID })
	}
	return rc, err
}

// The worker dies while the answers of a group are being written: the first calls are answered, the others rest EXECUTING.
// The worker that resumes sends the read-only ones again; every call is answered once in the thread, in order, no tool
// message twice, and the sources are not duplicated.
func TestAWorkerDyingDuringAGroupAndAnotherResumingAnswerEveryCallExactlyOnce(t *testing.T) {
	e := setup(t)
	installWeb(e, nil)
	var urls []string
	var calls []fakemodel.Call
	for i := 1; i <= 4; i++ {
		u := fmt.Sprintf("https://x.example/%d", i)
		urls = append(urls, u)
		calls = append(calls, fakemodel.Call{Name: "web_fetch", Args: `{"url":"` + u + `"}`})
	}
	e.model.Script = asking(calls...)
	for i := range e.mcp.Tools {
		e.mcp.Tools[i].Delay = 200 * time.Millisecond
	}
	m := newWebRun(webDef(), "fetch four pages", "run_1", "tok", run.Input{ExternalTools: true})
	d := &dying{memStore: m, at: 3}
	e.h.Store = d
	if err := e.h.Run(context.Background(), claim); !asFatal(err) {
		t.Fatalf("the first worker should be fenced: %v", err)
	}
	// the third answer was the write that was lost with the worker: calls 0 and 1 are answered, 2 and 3 are EXECUTING
	if got := threadOrder(m); !same(got, callIDs(2)) {
		t.Fatalf("after the death: tool messages %v, want the first two", got)
	}
	for i, want := range []string{run.CallDone, run.CallDone, run.CallExecuting, run.CallExecuting} {
		if got := callState(m, fmt.Sprintf("call_1_%d", i)); got != want {
			t.Fatalf("call %d is %s, want %s", i, got, want)
		}
	}
	if got := len(sourcesOf(m)); got != 2 {
		t.Fatalf("the answered calls recorded %d sources, want 2", got)
	}
	m.mu.Lock()
	m.fenced = false
	m.mu.Unlock()
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatalf("the worker that resumes: %v", err)
	}
	if m.status != run.StatusCompleted || !same(threadOrder(m), callIDs(4)) || !same(resultOrder(m), callIDs(4)) {
		t.Errorf("status %s, thread %v, events %v: want every call once, in order", m.status, threadOrder(m), resultOrder(m))
	}
	got := sourcesOf(m)
	if len(got) != 4 || fmt.Sprint(ns(got)) != "[1 2 3 4]" {
		t.Errorf("sources %v: each page once", ns(got))
	}
	if n := len(e.mcp.Calls()); n != 6 {
		t.Errorf("the gateway received %d calls: 4 and the 2 that were in progress again", n)
	}
	if ev := interruptions(m); len(ev) != 2 || ev[0]["resent"] != true || ev[1]["resent"] != true {
		t.Errorf("%v", ev)
	}
}

// ---- sources ----

// Two calls that return citations at the same moment (they wait for each other at the gateway) both reach the run's sources.
func TestTheSourcesOfCallsThatReturnAtTheSameMomentAreAllRecorded(t *testing.T) {
	e := setup(t)
	w := installWeb(e, map[string][]string{
		"alpha": {"https://x.example/1", "https://x.example/2"},
		"beta":  {"https://x.example/2", "https://x.example/3"},
		"gamma": {"https://x.example/4"},
	})
	w.gather = newBarrier(3)
	e.model.Script = asking(
		fakemodel.Call{Name: "web_search", Args: `{"query":"alpha"}`},
		fakemodel.Call{Name: "web_search", Args: `{"query":"beta"}`},
		fakemodel.Call{Name: "web_search", Args: `{"query":"gamma"}`})
	m := newWebRun(webDef(), "look", "run_1", "tok", run.Input{ExternalTools: true})
	e.h.Store = m
	start := time.Now()
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("the three searches never met at the gateway: they did not run together")
	}
	var got []string
	for _, s := range sourcesOf(m) {
		got = append(got, s.URL)
	}
	if len(got) != 4 {
		t.Errorf("sources %v: want the four pages, once each", got)
	}
}

// ---- the credential ----

// Every call of a group is refused with the credential the HOST believed valid: it is renewed once, and each call is made
// again with the renewed one; no call renews it again, and no sibling is left with the old token.
func TestACredentialRefusedForAWholeGroupIsRenewedOnce(t *testing.T) {
	e, m := pSetup(t, rd("a", 100), rd("b", 100), rd("c", 100), rd("d", 100))
	e.mcp.CallBearer = "tok-2"
	r := &renewer{next: func(string) (*credential.Token, error) {
		time.Sleep(200 * time.Millisecond) // the other calls have been refused and wait by now
		return &credential.Token{AccessToken: "tok-2", ExpiresAt: time.Now().Add(time.Hour), RefreshToken: "rt-2"}, nil
	}}
	e.h.Gateways["edge"].Refresh = r.refresh
	m.credExp, m.refresh = time.Now().Add(time.Hour), "rt-1"
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if m.status != run.StatusCompleted || !same(threadOrder(m), callIDs(4)) {
		t.Fatalf("%s %v", m.status, threadOrder(m))
	}
	r.mu.Lock()
	renewals := len(r.calls)
	r.mu.Unlock()
	if renewals != 1 || len(m.saved) != 1 {
		t.Errorf("the credential was renewed %d times and stored %d times; want once", renewals, len(m.saved))
	}
	for _, c := range e.mcp.Calls() {
		if c.Bearer != "tok-2" {
			t.Errorf("a call reached the tool with %q", c.Bearer)
		}
	}
	if n := len(e.mcp.Calls()); n != 4 {
		t.Errorf("the gateway ran %d calls, want 4", n)
	}
}

// A credential that cannot be renewed ends the run once, with the reason, as it does for one call; the renewal is not tried
// again by each call of the group.
func TestACredentialThatCannotBeRenewedEndsTheRunOnceForAGroup(t *testing.T) {
	e, m := pSetup(t, rd("a", 100), rd("b", 100), rd("c", 100))
	e.mcp.CallBearer = "tok-2"
	r := &renewer{next: func(string) (*credential.Token, error) {
		time.Sleep(100 * time.Millisecond)
		return nil, fmt.Errorf("%w: invalid_grant", credential.ErrSessionEnded)
	}}
	e.h.Gateways["edge"].Refresh = r.refresh
	m.credExp, m.refresh = time.Now().Add(time.Hour), "rt-1"
	err := e.h.Run(context.Background(), claim)
	var f *run.Failure
	if !asFailure(err, &f) || f.Code != run.FailCredential || m.status != run.StatusFailed {
		t.Fatalf("%v %s", err, m.status)
	}
	if len(r.calls) != 1 {
		t.Errorf("the renewal was tried %d times", len(r.calls))
	}
}

// ---- helpers of these tests ----

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("the condition was not met within 5 s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func asFailure(err error, f **run.Failure) bool {
	for err != nil {
		if x, ok := err.(*run.Failure); ok {
			*f = x
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func asFatal(err error) bool { return err == run.ErrFenced }

// What is left of a step after a crash is handled in the order the model asked for it, not in the order of the call ids the
// store lists them by.
func TestWhatIsLeftOfAStepIsHandledInTheOrderOfTheCalls(t *testing.T) {
	msg := agui.Message{ID: "m1", Role: agui.RoleAssistant, ToolCalls: []agui.ToolCall{{ID: "zz"}, {ID: "aa"}, {ID: "mm"}}}
	tt := &turn{rc: &run.Context{Messages: []agui.Message{{ID: "u1", Role: agui.RoleUser}, msg}}}
	got := tt.inCallOrder([]run.ToolCall{{ID: "aa", Step: 1}, {ID: "mm", Step: 1}, {ID: "zz", Step: 1}})
	if got[0].ID != "zz" || got[1].ID != "aa" || got[2].ID != "mm" {
		t.Errorf("%v", got)
	}
}

// Reads that take the member's file go through the group like any other: the tickets are loaded before the calls start, the
// gateway gets the ticket, the model reads a result with the ticket redacted, and the records keep what the model wrote.
func TestReadsThatTakeAMembersFileRunTogetherAndNoTicketReachesTheModel(t *testing.T) {
	e := setup(t)
	echo := func(a map[string]any) (string, bool) { return "saw " + a["source"].(string), false }
	e.mcp.Tools = append(e.mcp.Tools, fakemcp.Tool{Name: "peek", Annotations: map[string]any{"readOnlyHint": true}, Delay: 200 * time.Millisecond, Handle: echo,
		InputSchema: `{"type":"object","properties":{"source":{"type":"string","format":"idp-attachment"}}}`})
	e.model.Script = asking(fakemodel.Call{Name: "peek", Args: `{"source":"attachment:1"}`}, fakemodel.Call{Name: "peek", Args: `{"source":"attachment:1"}`}, fakemodel.Call{Name: "peek", Args: `{"source":"attachment:2"}`})
	d := pdef()
	in := filesInput()
	in.Attachments = append(in.Attachments, in.Attachments[0])
	m := newMem(d, user("go"), in)
	m.fileTickets = &run.FileTickets{Read: []string{readTicket, readTicket + "2"}, Write: writeTicket}
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if e.mcp.MaxInFlight() != 3 || m.status != run.StatusCompleted {
		t.Fatalf("%d in flight, status %s", e.mcp.MaxInFlight(), m.status)
	}
	for _, c := range e.mcp.Calls() {
		if !strings.HasPrefix(fmt.Sprint(c.Args["source"]), readTicket) {
			t.Errorf("the gateway got %v, not a ticket", c.Args["source"])
		}
	}
	if got := answerOf(m, "call_1_0").Content; got != "saw [file ticket]" {
		t.Errorf("the model read %q", got)
	}
	if strings.Contains(fmt.Sprint(m.rc.Messages), "eyJ") {
		t.Errorf("a ticket is in the thread")
	}
}

// D57 with D53: each call of a group has its own tool span, a child of the turn, and the spans overlap in time as the calls do.
func TestEveryCallOfAGroupHasItsOwnToolSpanUnderTheTurn(t *testing.T) {
	e, m := pSetup(t, rd("a", 300), rd("b", 300), rd("c", 300))
	exp := traced(e)
	m.rc.Run.TraceID = reqTrace
	m.rc.Run.Traceparent = "00-" + reqTrace + "-" + reqSpan + "-01"
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	spans := exp.GetSpans()
	turn, tools := byName(spans, "idp.run.turn"), byName(spans, "idp.tool.call")
	if len(turn) != 1 || len(tools) != 3 {
		t.Fatalf("%d turn spans, %d tool spans", len(turn), len(tools))
	}
	for _, s := range tools {
		if s.Parent.SpanID() != turn[0].SpanContext.SpanID() || s.SpanContext.TraceID().String() != reqTrace {
			t.Errorf("a tool span is not a child of the turn: %s", s.Parent.SpanID())
		}
	}
	if !tools[0].StartTime.Before(tools[1].EndTime) || !tools[1].StartTime.Before(tools[0].EndTime) {
		t.Errorf("the tool spans do not overlap")
	}
	if tps := e.mcp.Traceparents(); len(tps) != 3 {
		t.Errorf("%d traceparents reached the gateway", len(tps))
	}
}
