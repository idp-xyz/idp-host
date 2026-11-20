package integration

// D53 against real PostgreSQL: the read-only calls of one step run at the same time, their answers are written in the order
// of the calls, and a cancel, a stop and a worker that dies reach every call of a group. The gateway is the fixture playing
// idp-web (read-only web_fetch with citations).

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"idp-host/internal/agui"
	"idp-host/internal/api"
	"idp-host/internal/run"
	"idp-host/tests/fixtures/fakemcp"
	"idp-host/tests/fixtures/fakemodel"
)

// pageNumber is the number the fixture gives a page: the digits its address ends with.
func pageNumber(u string) int {
	n, _ := strconv.Atoi(u[strings.LastIndex(u, "/")+1:])
	return n
}

// groupEnv publishes an agent whose model asks for web_fetch of four pages in its first step, on the gateway "delegated"
// (the fixture of idp-web), and returns the pages' addresses. gate, when set, is called by the gateway for every page.
func groupEnv(e *env, gate func(n int)) []string {
	e.mcpDeleg.RequireBearer = ""
	e.harness.Gateways["delegated"].ExternalTools = []string{"web_*"}
	e.host.Files = api.FilePolicy{Local: map[string]bool{"fake": true}, ExternalTools: map[string][]string{"delegated": {"web_*"}}}
	e.mcpDeleg.Tools = append(e.mcpDeleg.Tools, fakemcp.Tool{Name: "web_fetch", Description: "fetch", InputSchema: `{"type":"object","properties":{"url":{"type":"string"}}}`,
		Annotations: map[string]any{"readOnlyHint": true}, Handle: func(map[string]any) (string, bool) { return "", false },
		HandleAs: func(_ string, args map[string]any) (string, any, bool) {
			u, _ := args["url"].(string)
			if gate != nil {
				gate(pageNumber(u))
			}
			n := pageNumber(u)
			return fmt.Sprintf("[%d] page %d", n, n), map[string]any{"citations": []any{map[string]any{"n": n, "title": "page", "url": u}}}, false
		}})
	var urls []string
	var calls []fakemodel.Call
	for i := 1; i <= 4; i++ {
		u := fmt.Sprintf("https://x.example/%d", i)
		urls = append(urls, u)
		calls = append(calls, fakemodel.Call{Name: "web_fetch", Args: `{"url":"` + u + `"}`})
	}
	e.model.Script = func(messages []fakemodel.Message, _ []string) fakemodel.Turn {
		if messages[len(messages)-1].Role == "tool" {
			return fakemodel.Turn{Text: "done"}
		}
		return fakemodel.Turn{ToolCalls: calls}
	}
	e.publish("webber", definition("delegated", []string{"web_fetch"}, nil))
	return urls
}

// toolMessageIDs are the call ids of the thread's tool messages, in the thread's order.
func (e *env) toolMessageIDs(thread string) []string {
	e.t.Helper()
	var out []string
	for _, m := range e.must(200, "GET", "/v1/threads/"+thread+"/messages", "carol", nil)["messages"].([]any) {
		if msg := m.(map[string]any); msg["role"] == "tool" {
			out = append(out, msg["toolCallId"].(string))
		}
	}
	return out
}

func (e *env) callStates(runID string) []string {
	e.t.Helper()
	var out []string
	for _, c := range e.must(200, "GET", "/v1/runs/"+runID+"/tool-calls", "carol", nil)["toolCalls"].([]any) {
		out = append(out, c.(map[string]any)["state"].(string))
	}
	return out
}

func wantIDs(n int) string {
	var out []string
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("call_1_%d", i))
	}
	return fmt.Sprint(out)
}

func (e *env) startWeb(thread string) string {
	r := e.must(202, "POST", "/v1/agents/webber/runs", "carol", runBody(thread, "fetch four pages"), "Idempotency-Key", thread)
	return r["runId"].(string)
}

// Four web reads of one step run together (the gateway holds each for 400 ms: the run takes about that long, not 1.6 s), the
// thread holds their answers in the order of the calls, and all four pages are among the run's sources.
func TestTheReadOnlyCallsOfAStepRunTogetherOnPostgres(t *testing.T) {
	e := newEnv(t)
	urls := groupEnv(e, nil)
	for i := range e.mcpDeleg.Tools {
		e.mcpDeleg.Tools[i].Delay = 400 * time.Millisecond
	}
	id := e.startWeb("thr-par-1")
	start := time.Now()
	e.drive()
	took := time.Since(start)
	if got := e.must(200, "GET", "/v1/runs/"+id, "carol", nil); got["status"] != run.StatusCompleted {
		t.Fatalf("%v", got)
	}
	t.Logf("four reads of 400 ms: the run took %v, %d at once at the gateway", took.Round(10*time.Millisecond), e.mcpDeleg.MaxInFlight())
	if e.mcpDeleg.MaxInFlight() != 4 || took > 1500*time.Millisecond {
		t.Errorf("%d calls at once, the run took %v", e.mcpDeleg.MaxInFlight(), took)
	}
	if got := fmt.Sprint(e.toolMessageIDs("thr-par-1")); got != wantIDs(4) {
		t.Errorf("tool messages %s, want %s", got, wantIDs(4))
	}
	srcs, _ := sourcesOfRun(e, "carol", id)
	if fmt.Sprint(addresses(srcs)) != fmt.Sprint(urls) {
		t.Errorf("sources %v, want %v", addresses(srcs), urls)
	}
}

// D53 and D47: the worker dies when the first call of a group is answered and the others are in flight (EXECUTING). The one
// that resumes sends the three read-only calls again; every call is answered once in the thread, in order, each page is a
// source once, and the stream says three calls were repeated.
func TestAWorkerThatDiesDuringAGroupAndAnotherThatResumesAnswerEveryCallOnceOnPostgres(t *testing.T) {
	e := newEnv(t)
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	groupEnv(e, func(n int) {
		if n != 1 {
			started <- struct{}{}
			<-release
		}
	})
	id := e.startWeb("thr-par-die")
	ctx := context.Background()
	c1, err := e.store.Claim(ctx, "w1", 2*time.Second)
	if err != nil || c1 == nil || c1.RunID != id {
		t.Fatal(err, c1)
	}
	ctx1, kill := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- e.harness.Run(ctx1, c1) }()
	for i := 0; i < 3; i++ {
		<-started
	}
	for { // the first call is answered; the three others rest EXECUTING
		if s := e.callStates(id); fmt.Sprint(s) == fmt.Sprint([]string{run.CallDone, run.CallExecuting, run.CallExecuting, run.CallExecuting}) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	kill() // worker 1 dies with three calls in flight
	<-done
	close(release)
	if got := fmt.Sprint(e.toolMessageIDs("thr-par-die")); got != "[call_1_0]" {
		t.Fatalf("what the first worker stored: %s", got)
	}
	time.Sleep(2100 * time.Millisecond) // its lease
	c2, err := e.store.Claim(ctx, "w2", time.Minute)
	if err != nil || c2 == nil || c2.RunID != id {
		t.Fatalf("%v %+v", err, c2)
	}
	if err := e.harness.Run(ctx, c2); err != nil {
		t.Fatal(err)
	}
	if got := e.must(200, "GET", "/v1/runs/"+id, "carol", nil); got["status"] != run.StatusCompleted {
		t.Fatalf("%v", got)
	}
	if got := fmt.Sprint(e.toolMessageIDs("thr-par-die")); got != wantIDs(4) {
		t.Errorf("tool messages %s: every call once, in order", got)
	}
	for _, s := range e.callStates(id) {
		if s != run.CallDone {
			t.Errorf("%v", e.callStates(id))
		}
	}
	srcs, _ := sourcesOfRun(e, "carol", id)
	if len(srcs) != 4 {
		t.Errorf("sources lost or doubled: %v", addresses(srcs))
	}
	if n := len(e.mcpDeleg.Calls()); n != 7 {
		t.Errorf("the gateway received %d calls: 4 and the 3 that were in progress again", n)
	}
	if evs := e.interrupted(id); len(evs) != 3 {
		t.Errorf("%v", evs)
	} else {
		for _, ev := range evs {
			if ev["resent"] != true || ev["reason"] != "worker_stopped" {
				t.Errorf("%v", ev)
			}
		}
	}
}

// D53 and D46: the owner cancels a run whose four calls are in flight (the gateway holds them for 5 s): every call is
// recorded as interrupted, the thread answers all four in order, and the run ends CANCELED within a few seconds.
func TestACancelDuringAGroupReachesEveryCallOnPostgres(t *testing.T) {
	e := newEnv(t)
	e.worker.Lease = 3 * time.Second
	e.harness.CancelPoll = 50 * time.Millisecond
	groupEnv(e, nil)
	for i := range e.mcpDeleg.Tools {
		e.mcpDeleg.Tools[i].Delay = 5 * time.Second
	}
	id := e.startWeb("thr-par-cancel")
	served := make(chan struct{})
	go func() { defer close(served); _, _ = e.worker.Once(context.Background()) }()
	for e.mcpDeleg.MaxInFlight() < 4 {
		time.Sleep(10 * time.Millisecond)
	}
	cancelAt := time.Now()
	e.must(200, "POST", "/v1/runs/"+id+"/cancel", "carol", nil)
	<-served
	if took := time.Since(cancelAt); took > 3*time.Second {
		t.Errorf("the run ended %v after the cancel", took)
	}
	if got := e.must(200, "GET", "/v1/runs/"+id, "carol", nil); got["status"] != run.StatusCanceled {
		t.Fatalf("%v", got)
	}
	for _, s := range e.callStates(id) {
		if s != run.CallFailed {
			t.Errorf("%v", e.callStates(id))
		}
	}
	if got := fmt.Sprint(e.toolMessageIDs("thr-par-cancel")); got != wantIDs(4) {
		t.Errorf("tool messages %s", got)
	}
	for _, m := range e.must(200, "GET", "/v1/threads/thr-par-cancel/messages", "carol", nil)["messages"].([]any) {
		if msg := m.(map[string]any); msg["role"] == "tool" && !strings.Contains(fmt.Sprint(msg["error"]), "interrupted: the owner canceled the run") {
			t.Errorf("%v", msg)
		}
	}
	if evs := e.interrupted(id); len(evs) != 4 {
		t.Errorf("%v", evs)
	}
}

// D53 and D47: a worker told to stop while a group runs (a bound of two, four calls): the two calls in flight finish, no
// other starts, the run is handed over at once; the next worker runs the other two; each call ran once and is answered once.
func TestAStopDuringAGroupHandsTheRunOverWithoutStartingAnotherCallOnPostgres(t *testing.T) {
	e := newEnv(t)
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	groupEnv(e, func(int) { started <- struct{}{}; <-release })
	e.harness.Gateways["delegated"].ParallelCalls = 2
	id := e.startWeb("thr-par-stop")
	w := e.graceWorker(10 * time.Second)
	w.Lease = time.Minute
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	exited := make(chan struct{})
	go func() { defer close(exited); w.Run(ctx) }()
	<-started
	<-started
	stop() // SIGTERM while two calls are running
	time.Sleep(200 * time.Millisecond)
	close(release)
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker did not return after the calls finished")
	}
	if n := len(e.mcpDeleg.Calls()); n != 2 {
		t.Errorf("the gateway received %d calls; the two in flight finish and no other starts", n)
	}
	if got := fmt.Sprint(e.callStates(id)); got != fmt.Sprint([]string{run.CallDone, run.CallDone, run.CallConfirmed, run.CallConfirmed}) {
		t.Errorf("%s", got)
	}
	c2, err := e.store.Claim(context.Background(), "w2", time.Minute)
	if err != nil || c2 == nil || c2.RunID != id {
		t.Fatalf("the run should be claimable at once: %v %+v", err, c2)
	}
	if err := e.harness.Run(context.Background(), c2); err != nil {
		t.Fatal(err)
	}
	if got := e.must(200, "GET", "/v1/runs/"+id, "carol", nil); got["status"] != run.StatusCompleted {
		t.Fatalf("%v", got)
	}
	if n := len(e.mcpDeleg.Calls()); n != 4 || fmt.Sprint(e.toolMessageIDs("thr-par-stop")) != wantIDs(4) {
		t.Errorf("%d gateway calls (want 4, each once); tool messages %v", n, e.toolMessageIDs("thr-par-stop"))
	}
}

// What protects a run's sources from losing an entry is the run's row lock in the store, taken by every write of an answer:
// two answers written at the same moment, each with sources of its own, are both recorded.
func TestTwoAnswersWrittenAtTheSameMomentBothRecordTheirSourcesOnPostgres(t *testing.T) {
	e := newEnv(t)
	groupEnv(e, nil)
	id := e.startWeb("thr-par-race")
	ctx := context.Background()
	c1, err := e.store.Claim(ctx, "w1", time.Minute)
	if err != nil || c1 == nil || c1.RunID != id {
		t.Fatal(err, c1)
	}
	var calls []run.ToolCall
	for i := 0; i < 8; i++ {
		calls = append(calls, run.ToolCall{RunID: id, ID: fmt.Sprintf("race_%d", i), Step: 1, MessageID: "salvage", Tool: "web_fetch", Arguments: "{}", State: run.CallConfirmed, CreatedAt: time.Now()})
	}
	if err := e.store.RecordToolCalls(ctx, c1, calls, nil); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i, c := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			msg := agui.Message{ID: "tool_" + c.ID, Role: agui.RoleTool, ToolCallID: c.ID, Content: "ok"}
			src := &run.SourceBatch{Items: []run.Source{{N: i + 1, Title: "page", URL: fmt.Sprintf("https://race.example/%d", i+1)}}}
			if err := e.store.AnswerToolCall(ctx, c1, c.ID, run.CallDone, "ok", false, msg, src, []agui.Event{agui.ToolCallResult(msg.ID, c.ID, "ok")}); err != nil {
				t.Errorf("%v", err)
			}
		}()
	}
	wg.Wait()
	srcs, _ := sourcesOfRun(e, "carol", id)
	if len(srcs) != 8 {
		t.Errorf("%d of 8 sources recorded: %v", len(srcs), addresses(srcs))
	}
}
