package integration

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"idp-host/internal/agui"
	"idp-host/internal/run"
)

type stepTiming struct {
	firstOutput, firstText, afterFirst, promptMs, predictedMs *float64
	cached, creation, promptN, cacheN, predictedN             *int
	latency                                                   int64
}

// stepTiming reads the D54 columns of the run's only step straight from the table.
func (e *env) stepTiming(runID string) stepTiming {
	e.t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, e.ownerURL)
	if err != nil {
		e.t.Fatal(err)
	}
	defer c.Close(ctx)
	var s stepTiming
	err = c.QueryRow(ctx, `SELECT latency_ms, first_output_ms, first_text_ms, after_first_ms, cached_tokens, cache_creation_tokens,
		server_prompt_n, server_prompt_ms, server_cache_n, server_predicted_n, server_predicted_ms FROM run_steps WHERE run_id = $1 AND step = 1`, runID).
		Scan(&s.latency, &s.firstOutput, &s.firstText, &s.afterFirst, &s.cached, &s.creation, &s.promptN, &s.promptMs, &s.cacheN, &s.predictedN, &s.predictedMs)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

// D54 on real PostgreSQL: the step row carries the time to first output and what the server said about reuse, and NULL (not 0)
// where it said nothing; the model.usage event of the stored stream carries the same.
func TestTheStepRowCarriesTheTimingAndWhatTheServerSaidOnPostgres(t *testing.T) {
	e := newEnv(t)
	e.publish("assistant", definition("edge", []string{"sum", "echo"}, nil))
	one := func(thread, key string) string {
		r := e.must(202, "POST", "/v1/agents/assistant/runs", "carol", runBody(thread, "hello"), "Idempotency-Key", key)
		id := r["runId"].(string)
		if _, err := e.worker.Once(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := e.must(200, "GET", "/v1/runs/"+id, "carol", nil); got["status"] != run.StatusCompleted {
			t.Fatalf("%v %v", got["status"], got["failureCode"])
		}
		return id
	}
	e.model.LlamaTimings = true
	id := one("thr-timing-1", "timing-1")
	s := e.stepTiming(id)
	if s.firstOutput == nil || s.firstText == nil || s.afterFirst == nil || *s.firstOutput > float64(s.latency)+1 || *s.firstOutput > *s.firstText {
		t.Errorf("timing %v %v %v latency %d", s.firstOutput, s.firstText, s.afterFirst, s.latency)
	}
	if s.cached == nil || s.cacheN == nil || *s.cached != *s.cacheN || s.promptN == nil || s.promptMs == nil || *s.promptMs != 12.5 || s.predictedN == nil || *s.predictedN != 7 || s.predictedMs == nil || *s.predictedMs != 70.25 {
		t.Errorf("reuse %v %v %v %v", s.cached, s.cacheN, s.promptN, s.promptMs)
	}
	if s.creation != nil {
		t.Errorf("cache creation was not reported: %d", *s.creation)
	}
	events, err := e.store.Events(context.Background(), e.tenantA, "", id, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.Type == agui.EventCustom && ev.Payload["name"] == agui.CustomUsage {
			v, _ := ev.Payload["value"].(map[string]any)
			srv, _ := v["server"].(map[string]any)
			found = v["firstOutputMs"] != nil && v["cachedTokens"] != nil && srv["promptMs"] == 12.5
		}
	}
	if !found {
		t.Errorf("the stored model.usage event lacks the new fields")
	}
	e.model.LlamaTimings = false
	s = e.stepTiming(one("thr-timing-2", "timing-2"))
	if s.firstOutput == nil || s.cached != nil || s.creation != nil || s.promptN != nil || s.promptMs != nil || s.cacheN != nil || s.predictedN != nil || s.predictedMs != nil {
		t.Errorf("a server that says nothing leaves NULL: %+v", s)
	}
}
