package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-host/adapters/providers/openai"
	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/model"
	"idp-host/internal/run"
)

// D56 on real PostgreSQL: the first candidate's key is refused (401), the fallback answers; the step row says who failed
// with what (codes only) and who answered, and the run's stream carries the same.
func TestTheStepRowSaysWhichCandidateFailedAndWhichAnsweredOnPostgres(t *testing.T) {
	e := newEnv(t)
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided: sk-secret-1234"}}`))
	}))
	t.Cleanup(dead.Close)
	hc, _ := model.NewHTTPClient(5*time.Second, "")
	p, err := openai.New(openai.Config{Name: "dead", BaseURL: dead.URL + "/v1", HTTP: hc})
	if err != nil {
		t.Fatal(err)
	}
	fake, _ := e.harness.Providers.Get("fake")
	e.harness.Providers = model.NewRegistry(fake, p)
	d := definition("edge", []string{"sum", "echo"}, nil)
	d.Model = agent.ModelProfile{Provider: "dead", Model: "m1", Fallbacks: []agent.ModelRef{{Provider: "fake", Model: "fake-1"}}}
	e.publish("assistant", d)
	r := e.must(202, "POST", "/v1/agents/assistant/runs", "carol", runBody("thr-fb", "hello"), "Idempotency-Key", "fb-1")
	id := r["runId"].(string)
	e.drive()
	if got := e.must(200, "GET", "/v1/runs/"+id, "carol", nil); got["status"] != run.StatusCompleted {
		t.Fatalf("%v %v", got["status"], got["failureCode"])
	}
	ctx := context.Background()
	c, err := pgx.Connect(ctx, e.ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	var provider, mdl string
	var code *string
	var failed []byte
	if err := c.QueryRow(ctx, `SELECT provider, model, error_code, failed_candidates FROM run_steps WHERE run_id = $1 AND step = 1`, id).Scan(&provider, &mdl, &code, &failed); err != nil {
		t.Fatal(err)
	}
	var f []run.CandidateFailure
	if err := json.Unmarshal(failed, &f); err != nil || len(f) != 1 || f[0] != (run.CandidateFailure{Provider: "dead", Model: "m1", Code: "status_401"}) {
		t.Errorf("failed_candidates %s %v", failed, err)
	}
	if provider != "fake" || mdl != "fake-1" || code != nil {
		t.Errorf("the step row: %s/%s %v", provider, mdl, code)
	}
	// a step nobody failed has NULL there
	var n int
	_ = c.QueryRow(ctx, `SELECT count(*) FROM run_steps WHERE failed_candidates IS NOT NULL`).Scan(&n)
	if n != 1 {
		t.Errorf("%d step rows with failed candidates", n)
	}
	events, err := e.store.Events(ctx, e.tenantA, "", id, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var sawError, sawUsage bool
	if all, _ := json.Marshal(events); strings.Contains(string(all), "sk-secret") || strings.Contains(string(failed), "sk-secret") {
		t.Errorf("the provider's words are in the stored events or the step row")
	}
	for _, ev := range events {
		v, _ := ev.Payload["value"].(map[string]any)
		switch ev.Payload["name"] {
		case agui.CustomModelError:
			sawError = v["provider"] == "dead" && v["code"] == "status_401" && v["outcome"] == "next_candidate"
		case agui.CustomUsage:
			fc, _ := v["failedCandidates"].([]any)
			sawUsage = v["provider"] == "fake" && len(fc) == 1
		}
	}
	if !sawError || !sawUsage {
		t.Errorf("model.error %v, model.usage with failedCandidates %v", sawError, sawUsage)
	}
}
