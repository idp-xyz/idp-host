//go:build repro

package integration

// These reproduce defects whose fix is not decided yet (R1: what to do with a tool call that was in progress when the
// worker stopped), they fail today on purpose and are not part of `make test-integration`. Run them with
// `go test -tags repro ./internal/harness/ ./tests/integration/` (the integration one needs IDP_TEST_PG_OWNER_URL).

import (
	"context"
	"errors"
	"testing"
	"time"

	"idp-host/internal/agent"
	"idp-host/internal/run"
)

// R1(a) against real PostgreSQL, the real lease and the real Claim: a worker dies while the gateway is executing a tool
// call (the call rests as EXECUTING); the lease expires and another worker claims the run. Expects (ADR-0001 D3: tool calls
// are not repeated): the gateway received "sum" exactly once and the run completed. Setup: worker 1 claims with a 300 ms
// lease, its context is cancelled while the gateway handler is blocked inside the call (the process is gone; the handler
// still finishes afterwards, so the gateway completed the call and its result was lost); worker 2 claims after the lease
// expired and runs the harness. Reproduction, not a fix.
func TestAToolCallThatWasExecutingWhenTheWorkerDiedIsNotExecutedAgainOnPostgres(t *testing.T) {
	e := newEnv(t)
	e.publish("assistant", definition("edge", []string{"sum", "echo"}, map[string]string{"echo": agent.ConfirmConfirm}))
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	first := true
	e.mcp.Tools[0].Handle = func(map[string]any) (string, bool) {
		started <- struct{}{}
		if first {
			first = false
			<-release
		}
		return "3", false
	}
	r := e.must(202, "POST", "/v1/agents/assistant/runs", "carol", runBody("thr-r1a", "please sum"), "Idempotency-Key", "r1a")
	id := r["runId"].(string)

	ctx := context.Background()
	c1, err := e.store.Claim(ctx, "w1", 300*time.Millisecond)
	if err != nil || c1 == nil {
		t.Fatal(err, c1)
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
		t.Fatalf("setup: the call should rest as EXECUTING: %v", calls)
	}

	time.Sleep(400 * time.Millisecond) // the lease runs out
	c2, err := e.store.Claim(ctx, "w2", time.Minute)
	if err != nil || c2 == nil || c2.RunID != id || c2.Epoch != c1.Epoch+1 {
		t.Fatalf("%v %+v", err, c2)
	}
	if err := e.harness.Run(ctx, c2); err != nil && !errors.Is(err, run.ErrFenced) {
		t.Fatalf("worker 2: %v", err)
	}
	got := e.must(200, "GET", "/v1/runs/"+id, "carol", nil)
	if n := len(e.mcp.Calls()); n != 1 {
		t.Errorf("the gateway received tools/call sum %d times; want exactly 1 (run status %v)", n, got["status"])
	}
	if got["status"] != run.StatusCompleted {
		t.Errorf("run status %v, want completed: %v", got["status"], got)
	}
}
