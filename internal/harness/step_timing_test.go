package harness

// D54: what a step records of its timing and of what the provider said about reuse, in the model.usage event and in the
// step the store receives; and that D52's correction sees the whole prompt whatever the provider's split.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"idp-host/adapters/providers/anthropic"
	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/ctxwin"
	"idp-host/internal/model"
	"idp-host/internal/run"
	"idp-host/tests/fixtures/fakemodel"
)

func TestAStepRecordsItsTimeToFirstOutputAndWhatTheServerSaid(t *testing.T) {
	e := setup(t)
	e.model.LlamaTimings = true
	m := newMem(def(), user("hello"), run.Input{})
	e.h.Store = m
	if err := e.h.Run(ctxBackground(), claim); err != nil || m.status != run.StatusCompleted {
		t.Fatalf("%v %s", err, m.status)
	}
	st := m.steps[0]
	if st.FirstOutput == nil || st.FirstText == nil || st.AfterFirst == nil || *st.FirstOutput > st.Latency || *st.FirstOutput > *st.FirstText {
		t.Errorf("timing %v %v %v latency %v", st.FirstOutput, st.FirstText, st.AfterFirst, st.Latency)
	}
	// the fake reports 10 prompt tokens a message, of which 6 reused and 4 processed
	if st.CachedTokens == nil || *st.CachedTokens != st.PromptTokens*6/10 || st.Server == nil || *st.Server.CacheN != *st.CachedTokens || *st.Server.PromptN != st.PromptTokens*4/10 || *st.Server.PromptMs != 12.5 || *st.Server.PredictedMs != 70.25 {
		t.Errorf("reuse %v %+v", st.CachedTokens, st.Server)
	}
	if st.CacheCreationTokens != nil {
		t.Errorf("cache creation was not reported")
	}
	u := eventNamed(m, agui.CustomUsage)[0]
	srv, _ := u["server"].(map[string]any)
	if u["firstOutputMs"] == nil || u["firstTextMs"] == nil || u["afterFirstMs"] == nil || u["cachedTokens"] != *st.CachedTokens || srv["cacheN"] != *st.CachedTokens || srv["promptN"] != *st.Server.PromptN || srv["promptMs"] != 12.5 {
		t.Errorf("event %v", u)
	}
	if u["cacheCreationTokens"] != nil || u["attemptTimings"] != nil {
		t.Errorf("nothing to say about cache writes, one attempt: %v", u)
	}
}

// A provider that says nothing about reuse: those fields are absent from the event and the step, never 0.
func TestWithoutWhatTheProviderSaysTheFieldsAreAbsent(t *testing.T) {
	e := setup(t)
	m := newMem(def(), user("hello"), run.Input{})
	e.h.Store = m
	if err := e.h.Run(ctxBackground(), claim); err != nil || m.status != run.StatusCompleted {
		t.Fatalf("%v %s", err, m.status)
	}
	st := m.steps[0]
	if st.CachedTokens != nil || st.CacheCreationTokens != nil || st.Server != nil || st.FirstOutput == nil {
		t.Errorf("%+v", st)
	}
	u := eventNamed(m, agui.CustomUsage)[0]
	for _, k := range []string{"cachedTokens", "cacheCreationTokens", "server", "attemptTimings"} {
		if _, ok := u[k]; ok {
			t.Errorf("%s must be absent: %v", k, u)
		}
	}
}

// D45 with D54: every request of the step has its own timing and outcome in the event; the step's own is the successful
// request's.
func TestEachRetryHasItsOwnTimingAndTheStepsIsTheSuccessfulOnes(t *testing.T) {
	e := setup(t)
	addProvider(t, e, "flaky", 0, func(n int, w http.ResponseWriter, r *http.Request) bool {
		if n <= 2 {
			w.WriteHeader(502)
			return true
		}
		time.Sleep(30 * time.Millisecond) // the third request is slow to answer
		return false
	})
	fast(e, "flaky", 3, 0)
	m := newMem(onlyModel("flaky"), user("hello"), run.Input{})
	e.h.Store = m
	if err := e.h.Run(ctxBackground(), claim); err != nil || m.status != run.StatusCompleted {
		t.Fatalf("%v %s", err, m.status)
	}
	u := eventNamed(m, agui.CustomUsage)[0]
	log, _ := u["attemptTimings"].([]map[string]any)
	if len(log) != 3 {
		t.Fatalf("%v", u)
	}
	for i, want := range []string{"status_502", "status_502", "ok"} {
		if log[i]["outcome"] != want || log[i]["attempt"] != i+1 {
			t.Errorf("attempt %d: %v", i+1, log[i])
		}
	}
	if log[0]["firstOutputMs"] != nil || log[1]["firstOutputMs"] != nil || log[2]["firstOutputMs"] == nil {
		t.Errorf("only the successful attempt had output: %v", log)
	}
	if log[2]["firstOutputMs"].(int64) < 30 || u["firstOutputMs"] != log[2]["firstOutputMs"] || u["afterFirstMs"] != log[2]["afterFirstMs"] {
		t.Errorf("the step's timing is the successful attempt's: %v", u)
	}
	if st := m.steps[0]; st.FirstOutput == nil || st.FirstOutput.Milliseconds() != log[2]["firstOutputMs"] {
		t.Errorf("step %+v", st)
	}
}

// D52 with D54: Anthropic reports input_tokens as the uncached part. The adapter adds the cached part back, so the correction
// factor the harness derives is the same whether or not the prompt was cached. Before this, a cached prompt (here a tenth
// of it uncached) pulled the factor to its floor at once.
func TestD52sCorrectionSeesTheWholePromptWhenAnthropicSplitsIt(t *testing.T) {
	factors := func(cache bool) []float64 {
		e := setup(t)
		withWindow(e, ctxwin.Settings{Window: 131072})
		fm := fakemodel.New()
		fm.AnthropicCache = cache
		fm.AnthropicTokensPerMessage = 400
		fm.Script = func(msgs []fakemodel.Message, _ []string) fakemodel.Turn { // three more calls after the eight of the thread
			n := 0
			for _, m := range msgs {
				if m.Role == "tool" {
					n++
				}
			}
			if n < 11 {
				return call("sum", fmt.Sprintf(`{"a":%d,"b":2}`, n))
			}
			return fakemodel.Turn{Text: "done"}
		}
		srv := httptest.NewServer(fm.Handler())
		t.Cleanup(srv.Close)
		hc, _ := model.NewHTTPClient(5*time.Second, "")
		p, err := anthropic.New(anthropic.Config{Name: "fake", BaseURL: srv.URL, HTTP: hc})
		if err != nil {
			t.Fatal(err)
		}
		e.h.Providers = model.NewRegistry(p)
		d := def()
		d.Model = agent.ModelProfile{Provider: "fake", Model: "fake-1"}
		m := runWith(t, e, d, recordedLikeThread(8), run.Input{})
		var out []float64
		for _, ev := range viewEvents(m) {
			out = append(out, ev["factor"].(float64))
		}
		return out
	}
	whole, split := factors(false), factors(true)
	if len(whole) < 3 || len(whole) != len(split) {
		t.Fatalf("%v %v", whole, split)
	}
	for i := range whole {
		if whole[i] != split[i] {
			t.Fatalf("the factor depends on how the provider splits the prompt: whole %v split %v", whole, split)
		}
	}
	if whole[len(whole)-1] == whole[0] {
		t.Fatalf("the factor never moved, so the test shows nothing: %v", whole)
	}
}

func ctxBackground() context.Context { return context.Background() }
