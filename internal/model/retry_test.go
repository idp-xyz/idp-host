package model

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// D45: a Retry-After is read as seconds or as an HTTP date; a date in the past is no wait, and a value that is neither is
// ignored, so that a provider cannot make the host wait for a string it cannot read.
func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"7", 7 * time.Second, true},
		{" 0 ", 0, true},
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second, true},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0, true},
		{"99999999999", 24 * time.Hour, true},
		{"-5", 0, false},
		{"soon", 0, false},
		{"", 0, false},
	} {
		got, ok := ParseRetryAfter(c.in, now)
		if got != c.want || ok != c.ok {
			t.Errorf("%q: %v %v, want %v %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// D45: only a retryable status carries the provider's Retry-After into the error.
func TestClassifyHTTPCarriesRetryAfter(t *testing.T) {
	h := http.Header{"Retry-After": {"12"}}
	if e := ClassifyHTTP(429, h, nil, time.Now()); !e.Retryable || e.RetryAfter != 12*time.Second {
		t.Errorf("%+v", e)
	}
	if e := ClassifyHTTP(400, h, nil, time.Now()); e.Retryable || e.RetryAfter != 0 {
		t.Errorf("%+v", e)
	}
}

// D45: the waits double from the first one, are capped, keep between half and all of the computed wait, a longer
// Retry-After replaces them, and one above the cap is refused.
func TestRetryPolicyWait(t *testing.T) {
	low := RetryPolicy{Backoff: time.Second, MaxBackoff: 20 * time.Second, MaxRetryAfter: time.Minute, Rand: func() float64 { return 0 }}
	high := low
	high.Rand = func() float64 { return 0.999999 }
	for retry, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 6: 20 * time.Second, 30: 20 * time.Second} {
		if w, ok := high.Wait(retry, 0); !ok || w < want-time.Millisecond || w > want {
			t.Errorf("retry %d with full jitter: %v %v, want about %v", retry, w, ok, want)
		}
		if w, ok := low.Wait(retry, 0); !ok || w != want/2 {
			t.Errorf("retry %d with least jitter: %v %v, want %v", retry, w, ok, want/2)
		}
	}
	if w, ok := low.Wait(1, 30*time.Second); !ok || w != 30*time.Second {
		t.Errorf("a longer Retry-After must win: %v %v", w, ok)
	}
	if w, ok := high.Wait(1, 100*time.Millisecond); !ok || w < 900*time.Millisecond {
		t.Errorf("a shorter Retry-After must not shorten the wait: %v %v", w, ok)
	}
	if w, ok := low.Wait(1, 60*time.Second); !ok || w != time.Minute {
		t.Errorf("a Retry-After at the cap is waited for: %v %v", w, ok)
	}
	if _, ok := low.Wait(1, 61*time.Second); ok {
		t.Errorf("a Retry-After above the cap must be refused")
	}
	d := RetryPolicy{}.WithDefaults()
	if d.Attempts != 3 || d.Backoff != time.Second || d.MaxBackoff != 20*time.Second || d.MaxRetryAfter != time.Minute {
		t.Errorf("defaults %+v", d)
	}
}

// D46: a body that goes quiet is cut after the idle timeout and says so; one that keeps sending is not.
func TestIdleWatchCutsASilentBody(t *testing.T) {
	pr, pw := io.Pipe()
	w := NewIdleWatch(150*time.Millisecond, func() { _ = pw.CloseWithError(context.Canceled) })
	defer w.Stop()
	r := w.Wrap(pr)
	go func() {
		for i := 0; i < 4; i++ { // four chunks, each inside the timeout: the watch must not fire
			time.Sleep(80 * time.Millisecond)
			_, _ = pw.Write([]byte("x"))
		}
	}()
	buf := make([]byte, 1)
	for i := 0; i < 4; i++ {
		if _, err := r.Read(buf); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
	}
	if w.Fired() {
		t.Fatal("fired while data was arriving")
	}
	start := time.Now()
	if _, err := r.Read(buf); err == nil {
		t.Fatal("read after the silence succeeded")
	}
	if !w.Fired() || time.Since(start) > time.Second {
		t.Fatalf("fired %v after %v", w.Fired(), time.Since(start))
	}
	parent := context.Background()
	if e := w.StreamError(parent); e.Code != "transport" || !e.Retryable || !e.Idle {
		t.Errorf("%+v", e)
	}
	done, stop := context.WithCancel(context.Background())
	stop()
	if e := w.StreamError(done); e.Code != "canceled" {
		t.Errorf("a cancelled caller is not an idle stream: %+v", e)
	}
	if !strings.Contains((&Error{Code: "transport", Idle: true}).Error(), "stopped sending") {
		t.Error("the message of an idle error must say what happened")
	}
}
