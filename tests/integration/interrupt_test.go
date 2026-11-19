package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-host/internal/agui"
	"idp-host/internal/run"
	"idp-host/tests/fixtures/fakemodel"
)

// stepCodes reads the error codes of the run's steps straight from the table.
func (e *env) stepCodes(runID string) []string {
	e.t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, e.ownerURL)
	if err != nil {
		e.t.Fatal(err)
	}
	defer c.Close(ctx)
	rows, err := c.Query(ctx, `SELECT coalesce(error_code, '') FROM run_steps WHERE run_id = $1 ORDER BY step`, runID)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, code)
	}
	return out
}

// textClosed reports whether every TEXT_MESSAGE_START of the run's stored events has its TEXT_MESSAGE_END.
func (e *env) textClosed(runID string) (started int, closed bool) {
	e.t.Helper()
	events, err := e.store.Events(context.Background(), e.tenantA, "", runID, 0, 1000)
	if err != nil {
		e.t.Fatal(err)
	}
	open := map[string]bool{}
	for _, ev := range events {
		id, _ := ev.Payload["messageId"].(string)
		switch ev.Type {
		case agui.EventTextMessageStart:
			started++
			open[id] = true
		case agui.EventTextMessageEnd:
			delete(open, id)
		}
	}
	return started, len(open) == 0
}

// waitStatus polls the run through the API until it has the status or the limit passes.
func (e *env) waitStatus(runID, status string, limit time.Duration) map[string]any {
	e.t.Helper()
	end := time.Now().Add(limit)
	for {
		got := e.must(200, "GET", "/v1/runs/"+runID, "carol", nil)
		if got["status"] == status || time.Now().After(end) {
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// D46 on real PostgreSQL: the owner cancels a RUNNING run (the store sets cancel_requested) while a worker holds it with its
// heartbeat running and the model is in the middle of a call. The run ends CANCELED within a few seconds of the cancel,
// not when the model answers (4 s); the step says canceled; the lease is released. The worker's lease is 3 s here, so its
// heartbeat (every second) runs throughout and must not get in the way.
func TestACancelOfARunningRunReachesTheModelCallThroughTheStoreOnPostgres(t *testing.T) {
	e := newEnv(t)
	e.worker.Lease = 3 * time.Second
	e.publish("assistant", definition("edge", []string{"sum", "echo"}, nil))
	e.model.Script = func(m []fakemodel.Message, tools []string) fakemodel.Turn {
		time.Sleep(4 * time.Second)
		return fakemodel.DefaultScript(m, tools)
	}
	r := e.must(202, "POST", "/v1/agents/assistant/runs", "carol", runBody("thr-cancel-store", "hello"), "Idempotency-Key", "cancel-store")
	id := r["runId"].(string)
	served := make(chan struct{})
	go func() { defer close(served); _, _ = e.worker.Once(context.Background()) }()
	if got := e.waitStatus(id, run.StatusRunning, 3*time.Second); got["status"] != run.StatusRunning {
		t.Fatalf("the run did not start: %v", got)
	}
	time.Sleep(300 * time.Millisecond) // inside the model call
	cancelAt := time.Now()
	if _, err := e.store.CancelRun(context.Background(), e.tenantA, "carol", id); err != nil {
		t.Fatal(err)
	}
	<-served
	took := time.Since(cancelAt)
	got := e.must(200, "GET", "/v1/runs/"+id, "carol", nil)
	if got["status"] != run.StatusCanceled || took > 3500*time.Millisecond {
		t.Fatalf("status %v %v after %v", got["status"], got["failureCode"], took)
	}
	if codes := e.stepCodes(id); len(codes) != 1 || codes[0] != "canceled" {
		t.Errorf("step error codes %v", codes)
	}
}

// D46 through the HTTP cancel endpoint: the owner cancels a run whose gateway tool call is in progress (5 s). The call's row
// ends FAILED with a message that says it was interrupted, the thread carries the tool's answer, and the run ends CANCELED.
func TestACancelThroughTheEndpointInterruptsAToolCallOnPostgres(t *testing.T) {
	e := newEnv(t)
	e.worker.Lease = 3 * time.Second
	e.publish("assistant", definition("edge", []string{"sum", "echo"}, nil))
	started := make(chan struct{}, 2)
	e.mcp.Tools[0].Handle = func(map[string]any) (string, bool) {
		started <- struct{}{}
		time.Sleep(4 * time.Second)
		return "3", false
	}
	r := e.must(202, "POST", "/v1/agents/assistant/runs", "carol", runBody("thr-cancel-http", "please sum"), "Idempotency-Key", "cancel-http")
	id := r["runId"].(string)
	served := make(chan struct{})
	go func() { defer close(served); _, _ = e.worker.Once(context.Background()) }()
	<-started
	time.Sleep(200 * time.Millisecond)
	cancelAt := time.Now()
	if got := e.must(200, "POST", "/v1/runs/"+id+"/cancel", "carol", nil); got["status"] != run.StatusRunning && got["status"] != run.StatusCanceled {
		t.Fatalf("cancel: %v", got)
	}
	<-served
	if took := time.Since(cancelAt); took > 3500*time.Millisecond {
		t.Errorf("the run ended %v after the cancel", took)
	}
	got := e.must(200, "GET", "/v1/runs/"+id, "carol", nil)
	if got["status"] != run.StatusCanceled {
		t.Fatalf("status %v %v", got["status"], got["failureCode"])
	}
	calls := e.must(200, "GET", "/v1/runs/"+id+"/tool-calls", "carol", nil)["toolCalls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("%v", calls)
	}
	c := calls[0].(map[string]any)
	if c["state"] != run.CallFailed || !strings.Contains(c["result"].(string), "interrupted") {
		t.Errorf("the interrupted call: %v", c)
	}
	events, _ := e.store.Events(context.Background(), e.tenantA, "", id, 0, 1000)
	var answered, ended bool
	for _, ev := range events {
		answered = answered || ev.Type == agui.EventToolCallResult
		ended = ev.Type == agui.EventRunError && ev.Payload["code"] == "CANCELED"
	}
	if !answered || !ended {
		t.Errorf("tool call answered %v, log ends with the cancel %v", answered, ended)
	}
}

// D46 on real PostgreSQL: the run's deadline (RunTimeoutSeconds 1, the least a release may say) reaches a model call that
// takes 4 s: the run fails RUN_TIMEOUT within about a second of the deadline and the step says deadline.
func TestTheDeadlineReachesAModelCallOnPostgres(t *testing.T) {
	e := newEnv(t)
	e.worker.Lease = 3 * time.Second
	d := definition("edge", []string{"sum"}, nil)
	d.Limits.RunTimeoutSeconds = 1
	e.publish("assistant", d)
	e.model.Script = func(m []fakemodel.Message, tools []string) fakemodel.Turn {
		time.Sleep(4 * time.Second)
		return fakemodel.DefaultScript(m, tools)
	}
	r := e.must(202, "POST", "/v1/agents/assistant/runs", "carol", runBody("thr-deadline", "hello"), "Idempotency-Key", "deadline")
	id := r["runId"].(string)
	start := time.Now()
	if ok, err := e.worker.Once(context.Background()); err != nil || !ok {
		t.Fatal(ok, err)
	}
	got := e.must(200, "GET", "/v1/runs/"+id, "carol", nil)
	if got["status"] != run.StatusFailed || got["failureCode"] != run.FailTimeout || time.Since(start) > 3*time.Second {
		t.Fatalf("status %v %v after %v", got["status"], got["failureCode"], time.Since(start))
	}
	if codes := e.stepCodes(id); len(codes) != 1 || codes[0] != "deadline" {
		t.Errorf("step error codes %v", codes)
	}
}
