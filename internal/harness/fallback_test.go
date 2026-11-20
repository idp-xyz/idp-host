package harness

// Tests of D56: which non-retryable errors of one candidate move the step to the next candidate, and which end it.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/ctxwin"
	"idp-host/internal/run"
)

// body answers a status with a JSON body.
func body(code int, text string) func(int, http.ResponseWriter, *http.Request) bool {
	return func(_ int, w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = fmt.Fprint(w, text)
		return true
	}
}

// streamError answers 200 and an error event inside the stream (D50).
func streamErr(text string) func(int, http.ResponseWriter, *http.Request) bool {
	return func(_ int, w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = fmt.Fprint(w, "data: "+text+"\n\n")
		return true
	}
}

// primaryThenFake: the first candidate "p1" answers as handle says; the fallback is the fake model that answers well.
func primaryThenFake(t *testing.T, handle func(int, http.ResponseWriter, *http.Request) bool) (*env, *memStore, func() int) {
	t.Helper()
	e := setup(t)
	hits := addProvider(t, e, "p1", 0, handle)
	fast(e, "p1", 3, 0)
	d := onlyModel("p1")
	d.Model.Fallbacks = []agent.ModelRef{{Provider: "fake", Model: "fake-1"}}
	m := newMem(d, user("hello"), run.Input{})
	e.h.Store = m
	return e, m, func() int { return int(hits.Load()) }
}

// The rule as a table: error of the first candidate -> the fallback is asked or not.
func TestTheFallbackIsAskedAfterANonRetryableErrorOnlyWhenTheErrorIsTheCandidatesOwn(t *testing.T) {
	for _, c := range []struct {
		name     string
		handle   func(int, http.ResponseWriter, *http.Request) bool
		next     bool   // the fallback answers
		code     string // the error code of the first candidate as recorded
		requests int    // requests to the first candidate
		fault    string // whose problem the host says it is (the model.error event); "" when it does not say
	}{
		{"401: that provider's key", status(401, ""), true, "status_401", 1, "candidate"},
		{"403: that provider's key", status(403, ""), true, "status_403", 1, "candidate"},
		{"404: that provider's model name", status(404, ""), true, "status_404", 1, "candidate"},
		{"402: payment required", status(402, ""), true, "status_402", 1, "candidate"},
		{"413: that endpoint's body limit", status(413, ""), true, "status_413", 1, "candidate"},
		{"429 insufficient_quota is not retried", body(429, `{"error":{"code":"insufficient_quota","type":"insufficient_quota","message":"You exceeded your current quota"}}`), true, "status_429", 1, "candidate"},
		{"400 credit balance too low", body(400, `{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API."}}`), true, "status_400", 1, "candidate"},
		{"stream error insufficient_quota", streamErr(`{"error":{"type":"insufficient_quota","code":"insufficient_quota","message":"quota"}}`), true, "provider_error", 1, "candidate"},
		{"stream error authentication", streamErr(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`), true, "provider_error", 1, "candidate"},
		{"stream error not found", streamErr(`{"error":{"type":"not_found_error","message":"model: nope"}}`), true, "provider_error", 1, "candidate"},
		{"400: the request itself (tool schema)", body(400, `{"error":{"message":"Invalid schema for function 'sum': 'type' is required","type":"invalid_request_error"}}`), false, "status_400", 1, ""},
		{"422: the request itself", status(422, ""), false, "status_422", 1, ""},
		{"400: content policy refusal", body(400, `{"error":{"code":"content_policy_violation","message":"blocked","type":"invalid_request_error"}}`), false, "status_400", 1, "policy"},
		{"stream error: content filter", streamErr(`{"error":{"type":"invalid_request_error","code":"content_filter","message":"blocked"}}`), false, "provider_error", 1, "policy"},
		{"stream error: other invalid request", streamErr(`{"error":{"type":"invalid_request_error","message":"bad"}}`), false, "provider_error", 1, ""},
		{"retryable 503 is retried, then the fallback (D45 unchanged)", status(503, ""), true, "status_503", 3, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, m, hits := primaryThenFake(t, c.handle)
			err := e.h.Run(context.Background(), claim)
			if c.next {
				if err != nil || m.status != run.StatusCompleted || m.steps[0].Provider != "fake" || e.model.Calls() != 1 {
					t.Fatalf("%v %s answered by %s, fallback asked %d times", err, m.status, m.steps[0].Provider, e.model.Calls())
				}
				if f := m.steps[0].Failed; len(f) != 1 || f[0].Provider != "p1" || f[0].Model != "fake-1" || f[0].Code != c.code {
					t.Errorf("the step does not say who failed with what: %+v", f)
				}
				u := eventNamed(m, agui.CustomUsage)
				if len(u) != 1 || u[0]["provider"] != "fake" {
					t.Errorf("usage %v", u)
				}
				if f, _ := u[0]["failedCandidates"].([]map[string]any); len(f) != 1 || f[0]["provider"] != "p1" || f[0]["code"] != c.code {
					t.Errorf("the usage event does not say who failed: %v", u[0]["failedCandidates"])
				}
				if errs := eventNamed(m, agui.CustomModelError); len(errs) == 0 || errs[len(errs)-1]["provider"] != "p1" || errs[len(errs)-1]["code"] != c.code || errs[len(errs)-1]["outcome"] != "next_candidate" {
					t.Errorf("model.error events %v", errs)
				}
			} else {
				if m.status != run.StatusFailed || e.model.Calls() != 0 || m.steps[0].Provider != "p1" || m.steps[0].ErrorCode != c.code {
					t.Fatalf("%v %s step %+v, fallback asked %d times", err, m.status, m.steps[0], e.model.Calls())
				}
				if errs := eventNamed(m, agui.CustomModelError); len(errs) == 0 || errs[len(errs)-1]["outcome"] != "stop" {
					t.Errorf("model.error events %v", errs)
				}
			}
			if errs := eventNamed(m, agui.CustomModelError); len(errs) > 0 && c.requests == 1 {
				if f, _ := errs[len(errs)-1]["fault"].(string); f != c.fault {
					t.Errorf("the event says the fault is %q, want %q: %v", f, c.fault, errs[len(errs)-1])
				}
			}
			if hits() != c.requests {
				t.Errorf("%d requests to the first candidate, want %d", hits(), c.requests)
			}
		})
	}
}

// A refusal by one provider's content policy is not sent to another provider: the content the fallback would receive is
// the very content that was refused.
func TestARefusedContentIsNotSentToAnotherProvider(t *testing.T) {
	e, m, _ := primaryThenFake(t, body(400, `{"error":{"code":"content_filter","message":"The response was filtered"}}`))
	_ = e.h.Run(context.Background(), claim)
	if e.model.Calls() != 0 || m.status != run.StatusFailed {
		t.Fatalf("fallback asked %d times, %s", e.model.Calls(), m.status)
	}
	if strings.Contains(fmt.Sprint(m.types()), "model.usage") {
		t.Errorf("a step was recorded as answered: %v", m.types())
	}
}

// Every candidate failing with its own fault: the run fails with the last error, and the step names each candidate.
func TestWhenEveryCandidateFailsWithItsOwnFaultTheStepNamesThemAll(t *testing.T) {
	e := setup(t)
	addProvider(t, e, "p1", 0, status(401, ""))
	addProvider(t, e, "p2", 0, status(404, ""))
	d := onlyModel("p1")
	d.Model.Fallbacks = []agent.ModelRef{{Provider: "p2", Model: "fake-1"}}
	m := newMem(d, user("hello"), run.Input{})
	e.h.Store = m
	_ = e.h.Run(context.Background(), claim)
	st := m.steps[0]
	if m.status != run.StatusFailed || st.ErrorCode != "status_404" || len(st.Failed) != 2 || st.Failed[0].Code != "status_401" || st.Failed[1].Provider != "p2" || st.Failed[1].Code != "status_404" {
		t.Fatalf("%s %+v", m.status, st)
	}
	if e.model.Calls() != 0 {
		t.Errorf("the model double was asked")
	}
}

// D40's error with another candidate: asked only when the host knows that candidate's window to be larger.
func TestAContextLengthRefusalMovesOnOnlyToAKnownLargerWindow(t *testing.T) {
	refuse := body(400, `{"error":{"code":"context_length_exceeded","message":"This model's maximum context length is 8192 tokens"}}`)
	for _, c := range []struct {
		name     string
		windows  map[string]int
		next     bool
		wantCode string
	}{
		{"larger known window", map[string]int{"p1/fake-1": 8192, "fake/fake-1": 32768}, true, ""},
		{"equal window", map[string]int{"p1/fake-1": 8192, "fake/fake-1": 8192}, false, "context_length"},
		{"smaller window", map[string]int{"p1/fake-1": 32768, "fake/fake-1": 8192}, false, "context_length"},
		{"the next window is not known", map[string]int{"p1/fake-1": 8192}, false, "context_length"},
		{"the refusing window is not known, the next is", map[string]int{"fake/fake-1": 32768}, true, ""},
		{"no window known", nil, false, "context_length"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, m, hits := primaryThenFake(t, refuse)
			e.h.ModelContext = map[string]ctxwin.Settings{}
			for k, w := range c.windows {
				e.h.ModelContext[k] = ctxwin.Settings{Window: w}
			}
			_ = e.h.Run(context.Background(), claim)
			if hits() != 1 {
				t.Errorf("%d requests to the first candidate", hits())
			}
			if c.next {
				if m.status != run.StatusCompleted || e.model.Calls() != 1 || len(m.steps[0].Failed) != 1 || m.steps[0].Failed[0].Code != "context_length" {
					t.Errorf("%s, fallback asked %d times, %+v", m.status, e.model.Calls(), m.steps[0])
				}
				return
			}
			if m.status != run.StatusFailed || e.model.Calls() != 0 || m.steps[0].ErrorCode != c.wantCode {
				t.Errorf("%s, fallback asked %d times, %+v", m.status, e.model.Calls(), m.steps[0])
			}
		})
	}
}

// With three candidates the one whose window is not larger is skipped, not asked: p1 (8k) refuses, p2 (8k) is skipped, the
// fake (32k) answers.
func TestACandidateWithoutALargerWindowIsSkippedAfterAContextLengthRefusal(t *testing.T) {
	e := setup(t)
	h1 := addProvider(t, e, "p1", 0, body(400, `{"error":{"code":"context_length_exceeded","message":"maximum context length"}}`))
	h2 := addProvider(t, e, "p2", 0, body(400, `{"error":{"code":"context_length_exceeded","message":"maximum context length"}}`))
	e.h.ModelContext = map[string]ctxwin.Settings{"p1/fake-1": {Window: 8192}, "p2/fake-1": {Window: 8192}, "fake/fake-1": {Window: 32768}}
	d := onlyModel("p1")
	d.Model.Fallbacks = []agent.ModelRef{{Provider: "p2", Model: "fake-1"}, {Provider: "fake", Model: "fake-1"}}
	m := newMem(d, user("hello"), run.Input{})
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil || m.status != run.StatusCompleted {
		t.Fatalf("%v %s", err, m.status)
	}
	if h1.Load() != 1 || h2.Load() != 0 || e.model.Calls() != 1 {
		t.Errorf("requests: p1 %d, p2 %d, fake %d", h1.Load(), h2.Load(), e.model.Calls())
	}
}
