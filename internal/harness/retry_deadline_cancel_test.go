package harness

// The tests of D45 and D46 that began as reproductions (R2: a retryable error of the only configured model; R3: the
// deadline and a cancel reaching a call in progress) and the helpers the other tests of the two decisions share. Each
// asserts what a user expects. The reproductions whose fix is not decided are in repro_pending_test.go (build tag repro).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"idp-host/adapters/providers/openai"
	"idp-host/internal/model"
	"idp-host/internal/run"
	"idp-host/tests/fixtures/fakemodel"
)

// runIn starts h.Run in the background and returns a channel with its result and the time it ended.
type runEnd struct {
	err error
	at  time.Time
}

func runIn(h *Harness, ctx context.Context) <-chan runEnd {
	done := make(chan runEnd, 1)
	go func() { err := h.Run(ctx, claim); done <- runEnd{err, time.Now()} }()
	return done
}

// proxiedProvider puts a front server before the fake model: fail(n) decides the n-th request (1-based); false passes
// it on to the fake model. It records when each request arrived.
func proxiedProvider(t *testing.T, e *env, fail func(n int, w http.ResponseWriter, r *http.Request) bool) (arrivals func() []time.Time) {
	t.Helper()
	var mu sync.Mutex
	var times []time.Time
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		times = append(times, time.Now())
		n := len(times)
		mu.Unlock()
		if fail(n, w, r) {
			return
		}
		e.model.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	hc, _ := model.NewHTTPClient(5*time.Second, "")
	p, err := openai.New(openai.Config{Name: "fake", BaseURL: front.URL + "/v1", HTTP: hc, UsageInStream: true})
	if err != nil {
		t.Fatal(err)
	}
	e.h.Providers = model.NewRegistry(p)
	return func() []time.Time { mu.Lock(); defer mu.Unlock(); return append([]time.Time(nil), times...) }
}

// R2: a single configured model answers one retryable failure (429 with Retry-After: 1, 503, a dropped connection) and
// then works. Expects: the run survives (is completed) and, for the 429, the second request is not made before the
// Retry-After the provider asked for. Setup: no fallbacks, the OpenAI adapter against a front server. Reproduction, not a fix.
func TestARunSurvivesOneRetryableErrorOfTheOnlyConfiguredModel(t *testing.T) {
	cases := []struct {
		name string
		fail func(n int, w http.ResponseWriter, r *http.Request) bool
	}{
		{"429 with Retry-After 1s", func(n int, w http.ResponseWriter, r *http.Request) bool {
			if n > 1 {
				return false
			}
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			return true
		}},
		{"503", func(n int, w http.ResponseWriter, r *http.Request) bool {
			if n > 1 {
				return false
			}
			w.WriteHeader(503)
			return true
		}},
		{"dropped connection", func(n int, w http.ResponseWriter, r *http.Request) bool {
			if n > 1 {
				return false
			}
			c, _, _ := w.(http.Hijacker).Hijack()
			c.Close()
			return true
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t)
			arrivals := proxiedProvider(t, e, c.fail)
			m := newMem(def(), user("hello"), run.Input{})
			e.h.Store = m
			err := e.h.Run(context.Background(), claim)
			times := arrivals()
			if m.status != run.StatusCompleted {
				t.Errorf("run status %s code %q (err %v) after %d model request(s); want completed", m.status, m.failCode, err, len(times))
			}
			if strings.HasPrefix(c.name, "429") && len(times) >= 2 && times[1].Sub(times[0]) < 900*time.Millisecond {
				t.Errorf("retried after %v, before the Retry-After of 1s", times[1].Sub(times[0]))
			}
		})
	}
}

// awaitEnd waits for the run to end within limit; when it does not, it reports how long it really took (up to diag more)
// and then stops the run so the test can finish.
func awaitEnd(t *testing.T, done <-chan runEnd, start time.Time, limit, diag time.Duration, stop context.CancelFunc, what string) (runEnd, bool) {
	t.Helper()
	select {
	case end := <-done:
		return end, true
	case <-time.After(time.Until(start.Add(limit))):
	}
	t.Errorf("%s: the run had not ended %v after the reference point", what, limit)
	select {
	case end := <-done:
		t.Logf("%s: it ended %v after the reference point", what, end.at.Sub(start).Round(100*time.Millisecond))
		return end, false
	case <-time.After(diag):
		t.Logf("%s: still running %v after the reference point; stopping it", what, limit+diag)
		stop()
		return <-done, false
	}
}

// stalledStream answers 200 text/event-stream with one chunk and then sends nothing until released.
func stalledStream(t *testing.T, e *env) {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = fmt.Fprint(w, `data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"},"finish_reason":null}]}`+"\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	hc, _ := model.NewHTTPClient(5*time.Second, "")
	p, err := openai.New(openai.Config{Name: "fake", BaseURL: srv.URL + "/v1", HTTP: hc, UsageInStream: true})
	if err != nil {
		t.Fatal(err)
	}
	e.h.Providers = model.NewRegistry(p)
}

// slowModel makes the fake model answer after d (before any byte is sent).
func slowModel(e *env, d time.Duration) {
	e.model.Script = func(messages []fakemodel.Message, tools []string) fakemodel.Turn {
		time.Sleep(d)
		return fakemodel.DefaultScript(messages, tools)
	}
}

// slowTool makes the gateway's "sum" take d and signal when it starts.
func slowTool(e *env, d time.Duration) <-chan struct{} {
	started := make(chan struct{}, 8)
	e.mcp.Tools[0].Handle = func(map[string]any) (string, bool) {
		started <- struct{}{}
		time.Sleep(d)
		return "3", false
	}
	return started
}

// R3 (deadline): the run's deadline passes while a call is in progress. Expects: the run ends as failed RUN_TIMEOUT within
// 3 s of the deadline (D35: a run that exceeded its time limit does nothing more). Setup: a run with a 1 s deadline and
// (a) a model that takes 4 s to answer, (b) a model whose stream stalls after the first chunk and never ends, (c) a
// gateway tool that takes 4 s with the tool timeout set to 5 s (the default is 60 s). Reproduction, not a fix.
func TestARunEndsWithinAFewSecondsOfItsDeadlineWhateverItIsWaitingFor(t *testing.T) {
	for _, tc := range []string{"model answers after 4s", "model stream stalls", "tool takes 4s"} {
		t.Run(tc, func(t *testing.T) {
			e := setup(t)
			e.h.ToolTimeout = 5 * time.Second
			input := "hello"
			switch tc {
			case "model answers after 4s":
				slowModel(e, 4*time.Second)
			case "model stream stalls":
				stalledStream(t, e)
			case "tool takes 4s":
				slowTool(e, 4*time.Second)
				input = "please sum"
			}
			m := newMem(def(), user(input), run.Input{})
			deadline := time.Now().Add(time.Second)
			m.rc.Run.DeadlineAt = deadline
			e.h.Store = m
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			done := runIn(e.h, ctx)
			end, inTime := awaitEnd(t, done, deadline, 3*time.Second, 5*time.Second, stop, tc)
			var f *run.Failure
			if inTime && !(errors.As(end.err, &f) && f.Code == run.FailTimeout && m.status == run.StatusFailed && m.failCode == run.FailTimeout) {
				t.Errorf("ended %v after the deadline with status %s code %q err %v; want failed RUN_TIMEOUT", end.at.Sub(deadline).Round(100*time.Millisecond), m.status, m.failCode, end.err)
			}
			if !inTime {
				t.Logf("final state: status %q code %q err %v", m.status, m.failCode, end.err)
			}
		})
	}
}

// R3 (cancel): the owner cancels a RUNNING run (the store sets cancel_requested; the memStore reads it at StartStep as the
// real store does). Expects: the run ends as canceled within 3 s of the cancel. Setup: the cancel arrives 300 ms into
// (a) a model call that takes 4 s, (b) a tool call that takes 4 s. Reproduction, not a fix.
func TestACancelledRunEndsWithinAFewSecondsWhateverItIsWaitingFor(t *testing.T) {
	for _, tc := range []string{"model call of 4s", "tool call of 4s"} {
		t.Run(tc, func(t *testing.T) {
			e := setup(t)
			input := "hello"
			var started <-chan struct{}
			if tc == "model call of 4s" {
				slowModel(e, 4*time.Second)
			} else {
				started = slowTool(e, 4*time.Second)
				input = "please sum"
			}
			m := newMem(def(), user(input), run.Input{})
			e.h.Store = m
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			done := runIn(e.h, ctx)
			if started != nil {
				<-started
			}
			time.Sleep(300 * time.Millisecond)
			m.mu.Lock()
			m.rc.Run.CancelRequested = true // what CancelRun does to a RUNNING run
			m.mu.Unlock()
			cancelAt := time.Now()
			end, inTime := awaitEnd(t, done, cancelAt, 3*time.Second, 5*time.Second, stop, tc)
			if inTime && m.status != run.StatusCanceled {
				t.Errorf("ended %v after the cancel with status %s code %q err %v; want canceled", end.at.Sub(cancelAt).Round(100*time.Millisecond), m.status, m.failCode, end.err)
			}
			if !inTime {
				t.Logf("final state: status %q code %q err %v", m.status, m.failCode, end.err)
			}
		})
	}
}
