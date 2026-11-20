package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"idp-host/internal/agui"
	"idp-host/internal/model"
	"idp-host/internal/run"
	"idp-host/tests/fixtures/fakemodel"
)

// D60 on real PostgreSQL: a model whose answers stop at the output limit (it says it generated as many tokens as the host's
// explicit limit; the window is not configured) with room fails the run as output_runaway after two requests, the step row says so, the
// stored stream carries the model.error and model.retry events, the cut-off call was not run and not stored, and the next run
// on the thread is accepted.
func TestARunawayFailsTheRunAsOutputRunawayAndTheThreadStaysUsableOnPostgres(t *testing.T) {
	e := newEnv(t)
	e.publish("assistant", definition("edge", []string{"sum", "echo"}, nil))
	// no window is configured, so the limit must be explicit for a cut to be told from the server's own (D60)
	e.harness.ModelOutput = map[string]model.OutputPolicy{"fake/fake-1": {MaxOutputTokens: model.DefaultMaxOutputTokens}}
	e.model.Script = func([]fakemodel.Message, []string) fakemodel.Turn {
		return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "sum", Args: `{"a":1,"b":2}`}}, CutOff: true, Completion: model.DefaultMaxOutputTokens}
	}
	r := e.must(202, "POST", "/v1/agents/assistant/runs", "carol", runBody("thr-loop", "please sum"), "Idempotency-Key", "loop-1")
	id := r["runId"].(string)
	e.drive()
	got := e.must(200, "GET", "/v1/runs/"+id, "carol", nil)
	if got["status"] != run.StatusFailed || got["failureCode"] != run.FailModel {
		t.Fatalf("%v %v", got["status"], got["failureCode"])
	}
	if msg, _ := got["failureMessage"].(string); !strings.Contains(msg, "output_runaway") || !strings.Contains(msg, "2 request(s)") {
		t.Errorf("the failure message does not say why: %q", msg)
	}
	if n := len(e.mcp.Calls()); n != 0 {
		t.Errorf("%d tool call(s) reached the gateway", n)
	}
	ctx := context.Background()
	c, err := pgx.Connect(ctx, e.ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	var code, stop string
	var completion int
	if err := c.QueryRow(ctx, `SELECT error_code, stop_reason, completion_tokens FROM run_steps WHERE run_id = $1 AND step = 1`, id).Scan(&code, &stop, &completion); err != nil {
		t.Fatal(err)
	}
	if code != model.CodeOutputRunaway || stop != model.StopLength || completion != model.DefaultMaxOutputTokens {
		t.Errorf("step row: %q %q %d", code, stop, completion)
	}
	_, evs, _ := e.sse("GET", "/v1/runs/"+id+"/events", "carol", nil)
	var errorsSeen, retries, cutoffs int
	for _, ev := range evs {
		v, _ := ev["value"].(map[string]any)
		switch ev["name"] {
		case agui.CustomModelError:
			errorsSeen++
			if v["code"] != model.CodeOutputRunaway {
				t.Errorf("model.error %v", v)
			}
		case agui.CustomModelRetry:
			retries++
		case agui.CustomModelCutOff:
			cutoffs++
		}
	}
	if errorsSeen != 2 || retries != 1 || cutoffs != 0 {
		t.Errorf("%d model.error, %d model.retry, %d model.cutoff events", errorsSeen, retries, cutoffs)
	}
	if calls := e.must(200, "GET", "/v1/runs/"+id+"/tool-calls", "carol", nil)["toolCalls"].([]any); len(calls) != 0 {
		t.Errorf("call records %v", calls)
	}
	e.model.Script = fakemodel.DefaultScript
	r = e.must(202, "POST", "/v1/agents/assistant/runs", "carol", runBody("thr-loop", "hello again"), "Idempotency-Key", "loop-2")
	e.drive()
	if got := e.must(200, "GET", "/v1/runs/"+r["runId"].(string), "carol", nil); got["status"] != run.StatusCompleted {
		t.Errorf("the next run on the thread: %v %v", got["status"], got["failureCode"])
	}
}
