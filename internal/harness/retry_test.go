package harness

// Tests of D45 (the model provider is retried before the run gives up or moves on) and of the part of D46 that reaches the
// model call (the idle timeout of a stream, the deadline and a cancel during a wait or a call). The reproductions R2 and R3
// in repro_loop_test.go say what the user expects; these say what each control does and what it must not do.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"idp-host/adapters/providers/openai"
	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/model"
	"idp-host/internal/run"
	"idp-host/tests/fixtures/fakemodel"
)

// addProvider registers an OpenAI-compatible provider whose n-th request (1-based) is first offered to handle; when it
// returns false the fake model answers. It returns the count of requests.
func addProvider(t *testing.T, e *env, name string, idle time.Duration, handle func(n int, w http.ResponseWriter, r *http.Request) bool) *atomic.Int32 {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handle != nil && handle(int(hits.Add(1)), w, r) {
			return
		}
		if handle == nil {
			hits.Add(1)
		}
		e.model.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	hc, _ := model.NewHTTPClient(5*time.Second, "")
	p, err := openai.New(openai.Config{Name: name, BaseURL: srv.URL + "/v1", HTTP: hc, UsageInStream: true, IdleTimeout: idle})
	if err != nil {
		t.Fatal(err)
	}
	var all []model.Provider
	for _, n := range e.h.Providers.Names() {
		old, _ := e.h.Providers.Get(n)
		all = append(all, old)
	}
	e.h.Providers = model.NewRegistry(append(all, p)...)
	return &hits
}

func status(code int, retryAfter string) func(int, http.ResponseWriter, *http.Request) bool {
	return func(_ int, w http.ResponseWriter, _ *http.Request) bool {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(code)
		return true
	}
}

// fast makes the waits toward a provider milliseconds long, with a Retry-After cap of maxRetryAfter.
func fast(e *env, provider string, attempts int, maxRetryAfter time.Duration) {
	if e.h.Retry == nil {
		e.h.Retry = map[string]model.RetryPolicy{}
	}
	e.h.Retry[provider] = model.RetryPolicy{Attempts: attempts, Backoff: 5 * time.Millisecond, MaxBackoff: 20 * time.Millisecond, MaxRetryAfter: maxRetryAfter}
}

func onlyModel(provider string) agent.Definition {
	d := def()
	d.Model = agent.ModelProfile{Provider: provider, Model: "fake-1"}
	return d
}

// textWellFormed checks the TEXT_MESSAGE events the way a client following the stream reads them: every message that was
// started is closed, nothing is sent for a message that is not open, and no message is started twice.
func textWellFormed(t *testing.T, m *memStore) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	open := map[string]bool{}
	for i, ev := range m.events {
		id, _ := ev["messageId"].(string)
		switch ev.Type() {
		case agui.EventTextMessageStart:
			if open[id] {
				t.Errorf("event %d: message %s started twice", i, id)
			}
			open[id] = true
		case agui.EventTextMessageContent:
			if !open[id] {
				t.Errorf("event %d: content for message %s, which is not open", i, id)
			}
		case agui.EventTextMessageEnd:
			if !open[id] {
				t.Errorf("event %d: end of message %s, which is not open", i, id)
			}
			delete(open, id)
		}
	}
	if len(open) > 0 {
		t.Errorf("messages left open in the event log: %v", open)
	}
}

func startedTexts(m *memStore) int {
	n := 0
	for _, ev := range m.events {
		if ev.Type() == agui.EventTextMessageStart {
			n++
		}
	}
	return n
}

// D45: the same candidate is asked at most Attempts times, the waits between the requests are reported on the stream, and
// the run then fails as before. With one configured model, three requests in all by default.
func TestRequestsToOneCandidateAreBoundedAndEachWaitIsReported(t *testing.T) {
	e := setup(t)
	hits := addProvider(t, e, "flaky", 0, status(503, ""))
	fast(e, "flaky", 0, 0) // the defaults: 3 attempts
	m := newMem(onlyModel("flaky"), user("hello"), run.Input{})
	e.h.Store = m
	err := e.h.Run(context.Background(), claim)
	var f *run.Failure
	if !errors.As(err, &f) || f.Code != run.FailModel || m.status != run.StatusFailed || m.steps[0].ErrorCode != "status_503" {
		t.Fatalf("%v %s %+v", err, m.status, m.steps)
	}
	if hits.Load() != 3 {
		t.Errorf("%d requests, want 3", hits.Load())
	}
	retries := eventNamed(m, agui.CustomModelRetry)
	if len(retries) != 2 || retries[0]["attempt"] != 1 || retries[1]["attempt"] != 2 || retries[0]["of"] != 3 || retries[0]["code"] != "status_503" || retries[0]["provider"] != "flaky" {
		t.Errorf("retry events %v", retries)
	}
	if m.rc.Run.StepCount != 1 {
		t.Errorf("%d steps for one model call", m.rc.Run.StepCount)
	}
	// Attempts 1 means never retry
	fast(e, "flaky", 1, 0)
	hits.Store(0)
	m = newMem(onlyModel("flaky"), user("hello"), run.Input{})
	e.h.Store = m
	_ = e.h.Run(context.Background(), claim)
	if hits.Load() != 1 || len(eventNamed(m, agui.CustomModelRetry)) != 0 {
		t.Errorf("attempts 1: %d requests, retry events %v", hits.Load(), eventNamed(m, agui.CustomModelRetry))
	}
}

// D45: the answer carries how many requests the step needed, so that a run that survived a bad minute can be told from one
// that did not have one.
func TestTheUsageEventSaysHowManyRequestsTheStepMade(t *testing.T) {
	e := setup(t)
	addProvider(t, e, "flaky", 0, func(n int, w http.ResponseWriter, r *http.Request) bool {
		if n <= 2 {
			w.WriteHeader(502)
			return true
		}
		return false
	})
	fast(e, "flaky", 3, 0)
	m := newMem(onlyModel("flaky"), user("hello"), run.Input{})
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil || m.status != run.StatusCompleted {
		t.Fatalf("%v %s", err, m.status)
	}
	if u := eventNamed(m, agui.CustomUsage); len(u) != 1 || u[0]["attempts"] != 3 {
		t.Errorf("usage %v", u)
	}
}

// D45: a failed attempt returned no usage and costs no tokens, and it must not use up one of the run's steps: a run that
// may take one step survives a 503 and answers.
func TestAFailedAttemptConsumesNoStepAndNoTokens(t *testing.T) {
	e := setup(t)
	addProvider(t, e, "flaky", 0, func(n int, w http.ResponseWriter, r *http.Request) bool {
		if n == 1 {
			w.WriteHeader(503)
			return true
		}
		return false
	})
	fast(e, "flaky", 3, 0)
	d := onlyModel("flaky")
	d.Limits.MaxSteps = 1
	m := newMem(d, user("hello"), run.Input{})
	e.h.Store = m
	err := e.h.Run(context.Background(), claim)
	if err != nil || m.status != run.StatusCompleted {
		t.Fatalf("%v %s %s", err, m.status, m.failCode)
	}
	if m.rc.Run.StepCount != 1 || len(m.steps) != 1 {
		t.Errorf("%d steps counted, %d rows", m.rc.Run.StepCount, len(m.steps))
	}
	if u := eventNamed(m, agui.CustomUsage); len(u) != 1 || m.steps[0].PromptTokens == 0 || u[0]["promptTokens"] != m.steps[0].PromptTokens {
		t.Errorf("usage %v", u)
	}
	if st := m.steps[0]; st.ErrorCode != "" {
		t.Errorf("the step must carry the answer's usage only: %+v", st)
	}
}

// D45: a Retry-After longer than the computed wait is honoured (the wait is at least what the provider asked), in seconds
// or as an HTTP date, and the event says how long the host waited.
func TestARetryAfterLongerThanTheBackoffIsWaitedFor(t *testing.T) {
	for _, name := range []string{"seconds", "date"} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			var first, second atomic.Int64
			addProvider(t, e, "limited", 0, func(n int, w http.ResponseWriter, r *http.Request) bool {
				switch n {
				case 1:
					first.Store(time.Now().UnixNano())
					header := "1"
					if name == "date" {
						header = time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat) // whole seconds: 1 to 2 s from now
					}
					w.Header().Set("Retry-After", header)
					w.WriteHeader(429)
					return true
				case 2:
					second.Store(time.Now().UnixNano())
				}
				return false
			})
			fast(e, "limited", 3, 10*time.Second)
			m := newMem(onlyModel("limited"), user("hello"), run.Input{})
			e.h.Store = m
			if err := e.h.Run(context.Background(), claim); err != nil || m.status != run.StatusCompleted {
				t.Fatalf("%v %s", err, m.status)
			}
			gap := time.Duration(second.Load() - first.Load())
			ev := eventNamed(m, agui.CustomModelRetry)
			if len(ev) != 1 || gap < 900*time.Millisecond || ev[0]["waitMs"].(int64) < 900 || ev[0]["retryAfterMs"].(int64) < 900 {
				t.Errorf("waited %v, events %v", gap, ev)
			}
		})
	}
}

// D45: a provider that asks to wait longer than the host's cap is not waited for: with no other candidate the run fails at
// once and the message says what the provider asked; with another candidate that one is tried at once.
func TestARetryAfterBeyondTheCapIsNotWaitedFor(t *testing.T) {
	e := setup(t)
	hits := addProvider(t, e, "limited", 0, status(429, "120"))
	fast(e, "limited", 3, 2*time.Second)
	m := newMem(onlyModel("limited"), user("hello"), run.Input{})
	e.h.Store = m
	start := time.Now()
	err := e.h.Run(context.Background(), claim)
	var f *run.Failure
	if !errors.As(err, &f) || f.Code != run.FailModel || !strings.Contains(f.Message, "asked to wait 120 seconds") || !strings.Contains(f.Message, "longer than the 2 seconds this host waits") {
		t.Fatalf("%v", err)
	}
	if hits.Load() != 1 || time.Since(start) > time.Second || len(eventNamed(m, agui.CustomModelRetry)) != 0 {
		t.Errorf("%d requests in %v", hits.Load(), time.Since(start))
	}
	d := onlyModel("limited")
	d.Model.Fallbacks = []agent.ModelRef{{Provider: "fake", Model: "fake-1"}}
	m = newMem(d, user("hello"), run.Input{})
	e.h.Store = m
	hits.Store(0)
	if err := e.h.Run(context.Background(), claim); err != nil || m.status != run.StatusCompleted || m.steps[0].Provider != "fake" || hits.Load() != 1 {
		t.Fatalf("%v %s %+v requests %d", err, m.status, m.steps, hits.Load())
	}
}

// D45: the next candidate is tried only after the retries of the first are used up, and each candidate has its own policy.
func TestTheNextCandidateIsTriedAfterTheRetriesOfTheFirstAreUsedUp(t *testing.T) {
	e := setup(t)
	hits := addProvider(t, e, "flaky", 0, status(500, ""))
	fast(e, "flaky", 3, 0)
	d := onlyModel("flaky")
	d.Model.Fallbacks = []agent.ModelRef{{Provider: "fake", Model: "fake-1"}}
	m := newMem(d, user("hello"), run.Input{})
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil || m.status != run.StatusCompleted {
		t.Fatalf("%v %s", err, m.status)
	}
	if hits.Load() != 3 || m.steps[0].Provider != "fake" || m.rc.Run.StepCount != 1 {
		t.Errorf("%d requests to the first, answered by %s, %d steps", hits.Load(), m.steps[0].Provider, m.rc.Run.StepCount)
	}
	if u := eventNamed(m, agui.CustomUsage); len(u) != 1 || u[0]["attempts"] != 4 {
		t.Errorf("usage %v", u)
	}
}

// D45: an error that is not worth another try (a 400) is not retried, as before.
func TestANonRetryableErrorIsNotRetried(t *testing.T) {
	e := setup(t)
	hits := addProvider(t, e, "strict", 0, status(400, ""))
	fast(e, "strict", 3, 0)
	m := newMem(onlyModel("strict"), user("hello"), run.Input{})
	e.h.Store = m
	_ = e.h.Run(context.Background(), claim)
	if hits.Load() != 1 || m.steps[0].ErrorCode != "status_400" {
		t.Errorf("%d requests, %+v", hits.Load(), m.steps)
	}
}

// partialThenDrop streams one text chunk and then drops the connection.
func partialThenDrop(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	_, _ = fmt.Fprint(w, `data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"},"finish_reason":null}]}`+"\n\n")
	w.(http.Flusher).Flush()
	c, _, _ := w.(http.Hijacker).Hijack()
	c.Close()
	return true
}

// D45: text that was streamed is in the event log and cannot be taken back, so a failure after the first text delta is not
// retried; the message that was started is closed. Only the failure before the first delta is (R2's dropped connection).
func TestNoRetryAfterTheFirstStreamedDelta(t *testing.T) {
	e := setup(t)
	hits := addProvider(t, e, "flaky", 0, func(n int, w http.ResponseWriter, r *http.Request) bool { return partialThenDrop(w, r) })
	fast(e, "flaky", 3, 0)
	m := newMem(onlyModel("flaky"), user("hello"), run.Input{})
	e.h.Store = m
	_ = e.h.Run(context.Background(), claim)
	if hits.Load() != 1 || m.status != run.StatusFailed || m.steps[0].ErrorCode != "transport" {
		t.Fatalf("%d requests, status %s, step %+v", hits.Load(), m.status, m.steps)
	}
	if len(eventNamed(m, agui.CustomModelRetry)) != 0 {
		t.Error("a retry was announced")
	}
	if startedTexts(m) != 1 {
		t.Errorf("%d messages started", startedTexts(m))
	}
	textWellFormed(t, m)
}

// D45: reasoning and tool-call chunks are not written to the event log, so a stream that fails after them but before any
// text is still retried; the same failure after a text delta (above) is not.
func TestAFailureAfterReasoningButBeforeTextIsStillRetried(t *testing.T) {
	e := setup(t)
	hits := addProvider(t, e, "flaky", 0, func(n int, w http.ResponseWriter, r *http.Request) bool {
		if n > 1 {
			return false
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = fmt.Fprint(w, `data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"thinking"},"finish_reason":null}]}`+"\n\n")
		w.(http.Flusher).Flush()
		c, _, _ := w.(http.Hijacker).Hijack()
		c.Close()
		return true
	})
	fast(e, "flaky", 3, 0)
	m := newMem(onlyModel("flaky"), user("hello"), run.Input{})
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil || m.status != run.StatusCompleted || hits.Load() != 2 {
		t.Fatalf("%v %s %d requests", err, m.status, hits.Load())
	}
}

// D45: a failure after streamed text still lets the next candidate answer (as before), and the abandoned message is closed
// so that the log stays well formed.
func TestTextStreamedByAFailedCandidateIsClosedWhenTheNextCandidateAnswers(t *testing.T) {
	e := setup(t)
	addProvider(t, e, "flaky", 0, func(n int, w http.ResponseWriter, r *http.Request) bool { return partialThenDrop(w, r) })
	fast(e, "flaky", 3, 0)
	d := onlyModel("flaky")
	d.Model.Fallbacks = []agent.ModelRef{{Provider: "fake", Model: "fake-1"}}
	m := newMem(d, user("hello"), run.Input{})
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil || m.status != run.StatusCompleted {
		t.Fatalf("%v %s", err, m.status)
	}
	if startedTexts(m) != 2 {
		t.Errorf("%d messages started", startedTexts(m))
	}
	textWellFormed(t, m)
}

// D45, D46: the wait between two attempts ends when the owner cancels: the run is canceled within a second of the cancel,
// not after the wait, and no further request is made.
func TestTheWaitBetweenAttemptsEndsAtACancel(t *testing.T) {
	e := setup(t)
	hits := addProvider(t, e, "flaky", 0, status(503, ""))
	e.h.Retry = map[string]model.RetryPolicy{"flaky": {Attempts: 3, Backoff: 20 * time.Second, MaxBackoff: 20 * time.Second}}
	m := newMem(onlyModel("flaky"), user("hello"), run.Input{})
	e.h.Store = m
	done := runIn(e.h, context.Background())
	time.Sleep(300 * time.Millisecond)
	m.mu.Lock()
	m.rc.Run.CancelRequested = true
	m.mu.Unlock()
	cancelAt := time.Now()
	end := <-done
	if m.status != run.StatusCanceled || end.at.Sub(cancelAt) > 2500*time.Millisecond || hits.Load() != 1 {
		t.Errorf("status %s after %v, %d requests, err %v", m.status, end.at.Sub(cancelAt), hits.Load(), end.err)
	}
	if m.steps[0].ErrorCode != "canceled" {
		t.Errorf("step %+v", m.steps[0])
	}
}

// D45, D46: the wait between two attempts ends at the run's deadline: the run ends as RUN_TIMEOUT when the deadline
// passes, not when the wait would have ended.
func TestTheWaitBetweenAttemptsEndsAtTheDeadline(t *testing.T) {
	e := setup(t)
	hits := addProvider(t, e, "flaky", 0, status(503, ""))
	e.h.Retry = map[string]model.RetryPolicy{"flaky": {Attempts: 3, Backoff: 20 * time.Second, MaxBackoff: 20 * time.Second}}
	m := newMem(onlyModel("flaky"), user("hello"), run.Input{})
	deadline := time.Now().Add(700 * time.Millisecond)
	m.rc.Run.DeadlineAt = deadline
	e.h.Store = m
	end := <-runIn(e.h, context.Background())
	var f *run.Failure
	if !errors.As(end.err, &f) || f.Code != run.FailTimeout || end.at.Sub(deadline) > time.Second || hits.Load() != 1 {
		t.Errorf("err %v after %v past the deadline, %d requests", end.err, end.at.Sub(deadline), hits.Load())
	}
	if m.steps[0].ErrorCode != "deadline" {
		t.Errorf("step %+v", m.steps[0])
	}
}

// D45: a wait that would end after the deadline is not made when another candidate can be asked now.
func TestAWaitThatWouldOutliveTheDeadlineMovesToTheNextCandidate(t *testing.T) {
	e := setup(t)
	hits := addProvider(t, e, "flaky", 0, status(503, "30"))
	e.h.Retry = map[string]model.RetryPolicy{"flaky": {Attempts: 3, MaxRetryAfter: time.Minute}}
	d := onlyModel("flaky")
	d.Model.Fallbacks = []agent.ModelRef{{Provider: "fake", Model: "fake-1"}}
	m := newMem(d, user("hello"), run.Input{})
	m.rc.Run.DeadlineAt = time.Now().Add(5 * time.Second)
	e.h.Store = m
	start := time.Now()
	if err := e.h.Run(context.Background(), claim); err != nil || m.status != run.StatusCompleted || hits.Load() != 1 || time.Since(start) > 2*time.Second {
		t.Fatalf("%v %s %d requests in %v", err, m.status, hits.Load(), time.Since(start))
	}
}

// stallBeforeTheFirstByte answers the headers and then nothing.
func stallBeforeTheFirstByte(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	w.(http.Flusher).Flush()
	<-r.Context().Done()
	return true
}

// D46: a stream that stalls before any text is cut after the idle timeout, classified as a transport error and therefore
// retried under the D45 rule; when every attempt stalls, the step says idle_timeout.
func TestAStalledStreamIsCutByTheIdleTimeoutAndRetried(t *testing.T) {
	e := setup(t)
	hits := addProvider(t, e, "slow", 150*time.Millisecond, func(n int, w http.ResponseWriter, r *http.Request) bool {
		if n == 1 {
			return stallBeforeTheFirstByte(w, r)
		}
		return false
	})
	fast(e, "slow", 3, 0)
	m := newMem(onlyModel("slow"), user("hello"), run.Input{})
	e.h.Store = m
	start := time.Now()
	if err := e.h.Run(context.Background(), claim); err != nil || m.status != run.StatusCompleted || hits.Load() != 2 || time.Since(start) > 3*time.Second {
		t.Fatalf("%v %s %d requests in %v", err, m.status, hits.Load(), time.Since(start))
	}
	if ev := eventNamed(m, agui.CustomModelRetry); len(ev) != 1 || ev[0]["code"] != "idle_timeout" {
		t.Errorf("%v", ev)
	}

	hits = addProvider(t, e, "dead", 150*time.Millisecond, func(n int, w http.ResponseWriter, r *http.Request) bool { return stallBeforeTheFirstByte(w, r) })
	fast(e, "dead", 2, 0)
	m = newMem(onlyModel("dead"), user("hello"), run.Input{})
	e.h.Store = m
	_ = e.h.Run(context.Background(), claim)
	if m.status != run.StatusFailed || m.steps[0].ErrorCode != "idle_timeout" || hits.Load() != 2 {
		t.Errorf("%s %+v %d requests", m.status, m.steps[0], hits.Load())
	}
}

// D46: a stream that stalls after text was streamed is cut by the idle timeout too, is not retried (D45), and its message
// is closed.
func TestAStreamThatStallsAfterTextIsCutNotRetriedAndClosed(t *testing.T) {
	e := setup(t)
	hits := addProvider(t, e, "slow", 150*time.Millisecond, func(n int, w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = fmt.Fprint(w, `data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"},"finish_reason":null}]}`+"\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		return true
	})
	fast(e, "slow", 3, 0)
	m := newMem(onlyModel("slow"), user("hello"), run.Input{})
	e.h.Store = m
	_ = e.h.Run(context.Background(), claim)
	if m.status != run.StatusFailed || m.steps[0].ErrorCode != "idle_timeout" || hits.Load() != 1 {
		t.Errorf("%s %+v %d requests", m.status, m.steps[0], hits.Load())
	}
	textWellFormed(t, m)
}

// D46: the step's row tells the three ways a call is cut short apart, the text streamed so far is closed, and the run ends
// as the cause says.
func TestAnInterruptedModelCallLeavesTheStepAndTheEventLogConsistent(t *testing.T) {
	cases := []struct {
		name     string
		interupt func(m *memStore)
		status   string
		code     string
		stepCode string
	}{
		{"deadline", func(m *memStore) {}, run.StatusFailed, run.FailTimeout, "deadline"},
		{"cancel", func(m *memStore) {
			m.mu.Lock()
			m.rc.Run.CancelRequested = true
			m.mu.Unlock()
		}, run.StatusCanceled, "CANCELED", "canceled"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t)
			stalledStream(t, e) // one text chunk, then silence
			m := newMem(def(), user("hello"), run.Input{})
			deadline := time.Now().Add(time.Minute)
			if c.name == "deadline" {
				deadline = time.Now().Add(700 * time.Millisecond)
			}
			m.rc.Run.DeadlineAt = deadline
			e.h.Store = m
			done := runIn(e.h, context.Background())
			time.Sleep(300 * time.Millisecond)
			c.interupt(m)
			end := <-done
			if m.status != c.status || m.failCode != c.code {
				t.Fatalf("status %s code %q err %v", m.status, m.failCode, end.err)
			}
			if st := m.steps[0]; st.ErrorCode != c.stepCode || st.Latency <= 0 {
				t.Errorf("step %+v, want error code %s", st, c.stepCode)
			}
			if startedTexts(m) != 1 {
				t.Errorf("%d messages started", startedTexts(m))
			}
			textWellFormed(t, m)
			last := m.events[len(m.events)-1]
			if last.Type() != agui.EventRunError {
				t.Errorf("the log ends with %s", last.Type())
			}
		})
	}
}

// storeAfter wraps a store to run a hook right after the answer was stored.
type cancelAfterAnswer struct {
	*memStore
}

func (c *cancelAfterAnswer) AppendMessages(ctx context.Context, cl *run.Claim, msgs []agui.Message, ev []agui.Event) error {
	err := c.memStore.AppendMessages(ctx, cl, msgs, ev)
	if err == nil && len(msgs) == 1 && msgs[0].Role == agui.RoleAssistant {
		c.mu.Lock()
		c.rc.Run.CancelRequested = true // the owner cancels right after the answer was stored
		c.mu.Unlock()
	}
	return err
}

// D46, the rule for a cancel during the final model call. Before the answer is stored the cancel wins, even when the
// watcher has not seen it yet (the poll is an hour here): the run ends CANCELED and the answer is not stored. After the
// answer was stored the cancel comes too late: the run completes.
func TestACancelDuringTheFinalModelCallEndsTheRunCanceledUnlessTheAnswerWasStored(t *testing.T) {
	t.Run("before the answer is stored", func(t *testing.T) {
		e := setup(t)
		e.h.CancelPoll = time.Hour // only the check made just before the answer is stored can see it
		slowModel(e, 600*time.Millisecond)
		m := newMem(def(), user("hello"), run.Input{})
		e.h.Store = m
		done := runIn(e.h, context.Background())
		time.Sleep(150 * time.Millisecond)
		m.mu.Lock()
		m.rc.Run.CancelRequested = true
		m.mu.Unlock()
		end := <-done
		if m.status != run.StatusCanceled || m.failCode != "CANCELED" {
			t.Fatalf("status %s code %q err %v", m.status, m.failCode, end.err)
		}
		if got := m.rc.Messages[len(m.rc.Messages)-1]; got.Role != agui.RoleUser {
			t.Errorf("the answer was stored although the cancel came first: %+v", got)
		}
		if m.steps[0].ErrorCode != "canceled" || m.steps[0].PromptTokens == 0 {
			t.Errorf("the step must say it was canceled and keep the tokens the call used: %+v", m.steps[0])
		}
		textWellFormed(t, m)
	})
	t.Run("after the answer is stored", func(t *testing.T) {
		e := setup(t)
		m := newMem(def(), user("hello"), run.Input{})
		e.h.Store = &cancelAfterAnswer{m}
		if err := e.h.Run(context.Background(), claim); err != nil || m.status != run.StatusCompleted || m.last().Content != "Echo: hello" {
			t.Fatalf("%v %s %+v", err, m.status, m.last())
		}
	})
}

// D46: a deadline or a cancel that reaches a tool call records the call as FAILED with a message that says it was
// interrupted, answers the calls of the step that did not run (so that the thread is complete), and ends the run as the
// cause says. The two other calls of the step here are a second automatic call and a call the release forbids.
func TestAnInterruptedToolCallIsRecordedAsSuchAndTheRestOfTheStepIsAnswered(t *testing.T) {
	for _, cause := range []string{"deadline", "cancel"} {
		t.Run(cause, func(t *testing.T) {
			e := setup(t)
			started := slowTool(e, 2*time.Second)
			e.model.Script = func(msgs []fakemodel.Message, tools []string) fakemodel.Turn {
				if spoken(msgs).Role == "tool" {
					return fakemodel.Turn{Text: "done"}
				}
				return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "sum", Args: `{"a":1,"b":2}`}, {Name: "sum", Args: `{"a":3,"b":4}`}, {Name: "forbidden", Args: `{}`}}}
			}
			m := newMem(def(), user("go"), run.Input{})
			if cause == "deadline" {
				m.rc.Run.DeadlineAt = time.Now().Add(time.Second)
			}
			e.h.Store = m
			done := runIn(e.h, context.Background())
			<-started
			if cause == "cancel" {
				time.Sleep(200 * time.Millisecond)
				m.mu.Lock()
				m.rc.Run.CancelRequested = true
				m.mu.Unlock()
			}
			begin := time.Now()
			end := <-done
			if cause == "cancel" && (m.status != run.StatusCanceled || end.at.Sub(begin) > 2500*time.Millisecond) {
				t.Fatalf("status %s after %v err %v", m.status, end.at.Sub(begin), end.err)
			}
			if cause == "deadline" && m.status != run.StatusFailed || cause == "deadline" && m.failCode != run.FailTimeout {
				t.Fatalf("status %s code %s err %v", m.status, m.failCode, end.err)
			}
			byID := map[string]*run.ToolCall{}
			for _, c := range m.calls {
				byID[c.ID] = c
			}
			if len(byID) != 3 {
				t.Fatalf("%d tool calls recorded", len(byID))
			}
			interrupted, notRun, denied := 0, 0, 0
			for _, c := range byID {
				switch {
				case c.State == run.CallFailed && strings.HasPrefix(c.Result, "error: interrupted:") && strings.Contains(c.Result, "may or may not"):
					interrupted++
				case c.State == run.CallFailed && strings.Contains(c.Result, "not run:"):
					notRun++
				case c.State == run.CallDenied:
					denied++
				default:
					t.Errorf("call %s is %s %q", c.ID, c.State, c.Result)
				}
				if c.FinishedAt.IsZero() {
					t.Errorf("call %s is not finished", c.ID)
				}
			}
			if interrupted != 1 || notRun != 1 || denied != 1 {
				t.Errorf("interrupted %d, not run %d, denied %d", interrupted, notRun, denied)
			}
			answers := 0
			for _, msg := range m.rc.Messages {
				if msg.Role == agui.RoleTool {
					answers++
				}
			}
			if answers != 3 {
				t.Errorf("%d tool messages for 3 tool calls", answers)
			}
		})
	}
}

// D46: a run that waits for the member's confirmation holds no worker and is not touched by the watch: the turn ends
// AWAITING_INPUT with the confirmation pending, whatever the deadline and the cancel poll say, and the call keeps waiting.
func TestARunWaitingForConfirmationIsNotAffectedByTheWatch(t *testing.T) {
	e := setup(t)
	e.h.CancelPoll = 10 * time.Millisecond
	m := newMem(def(), user("please echo"), run.Input{})
	m.rc.Run.DeadlineAt = time.Now().Add(time.Minute)
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil || m.status != run.StatusAwaitingInput {
		t.Fatalf("%v %s", err, m.status)
	}
	time.Sleep(100 * time.Millisecond) // the watch must have stopped with the turn: nothing more is asked of the store
	var pending int
	for _, c := range m.calls {
		if c.State == run.CallPendingConfirmation {
			pending++
			if c.ConfirmationExpiresAt.After(m.rc.Run.DeadlineAt) {
				t.Errorf("D35: the confirmation outlives the deadline: %v", c.ConfirmationExpiresAt)
			}
		}
	}
	if pending != 1 {
		t.Errorf("%d pending confirmations", pending)
	}
}
