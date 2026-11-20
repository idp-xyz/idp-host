package model

import (
	"sync"
	"time"
)

// Stopwatch times one model attempt for D54. The adapter calls Sent just before it hands the request to the HTTP client,
// Output when the first fragment of ANY kind arrives (text, reasoning, tool-call name or arguments; never an empty
// role-only chunk), Text when the first text fragment arrives, and Done when the stream ends. A watch that was never told
// Output (a non-streamed answer, a stream that carried nothing, a failure before the first fragment) reports those
// times as absent: an unknown time is never replaced by the total. Every method is safe on a nil watch.
type Stopwatch struct {
	mu                             sync.Mutex
	now                            func() time.Time
	sent, firstOut, firstText, end time.Time
}

// NewStopwatch makes a watch on the given clock (the harness's, so that tests control it).
func NewStopwatch(now func() time.Time) *Stopwatch { return &Stopwatch{now: now} }

func (w *Stopwatch) mark(field func(*Stopwatch) *time.Time, needSent bool) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	t := field(w)
	if !t.IsZero() || (needSent && w.sent.IsZero()) {
		return
	}
	*t = w.now()
}

// Sent marks the moment the request is sent (the first call counts).
func (w *Stopwatch) Sent() { w.mark(func(w *Stopwatch) *time.Time { return &w.sent }, false) }

// Output marks the first output of any kind.
func (w *Stopwatch) Output() { w.mark(func(w *Stopwatch) *time.Time { return &w.firstOut }, true) }

// Text marks the first text fragment (and counts as output).
func (w *Stopwatch) Text() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sent.IsZero() || !w.firstText.IsZero() {
		return
	}
	t := w.now() // one reading for both: the first text fragment is also the first output unless something came before it
	w.firstText = t
	if w.firstOut.IsZero() {
		w.firstOut = t
	}
}

// Done marks the end of the stream.
func (w *Stopwatch) Done() { w.mark(func(w *Stopwatch) *time.Time { return &w.end }, true) }

// Timing is what a watch measured; nil is absent.
type Timing struct {
	FirstOutput *time.Duration // from the request being sent to the first output of any kind
	FirstText   *time.Duration // from the request being sent to the first text fragment
	AfterFirst  *time.Duration // from the first output to the end of the stream
}

// Timing reads the watch.
func (w *Stopwatch) Timing() Timing {
	var t Timing
	if w == nil {
		return t
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sent.IsZero() || w.firstOut.IsZero() {
		return t
	}
	d := w.firstOut.Sub(w.sent)
	t.FirstOutput = &d
	if !w.firstText.IsZero() {
		d := w.firstText.Sub(w.sent)
		t.FirstText = &d
	}
	if !w.end.IsZero() {
		d := w.end.Sub(w.firstOut)
		t.AfterFirst = &d
	}
	return t
}
