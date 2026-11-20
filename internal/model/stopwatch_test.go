package model

import (
	"testing"
	"time"
)

// stepClock returns 0, 10 ms, 20 ms, ... on successive readings, so a test says exactly which reading each mark took.
func stepClock() func() time.Time {
	base := time.Unix(1000, 0)
	n := 0
	return func() time.Time { t := base.Add(time.Duration(n) * 10 * time.Millisecond); n++; return t }
}

func ms(d *time.Duration) any {
	if d == nil {
		return nil
	}
	return d.Milliseconds()
}

func TestAStopwatchSaysWhenTheFirstOutputCameAndNeverGuesses(t *testing.T) {
	w := NewStopwatch(stepClock())
	w.Sent()   // 0
	w.Output() // 10: reasoning came first
	w.Output() // ignored: only the first counts
	w.Text()   // 20
	w.Text()   // ignored
	w.Done()   // 30
	got := w.Timing()
	if ms(got.FirstOutput) != int64(10) || ms(got.FirstText) != int64(20) || ms(got.AfterFirst) != int64(20) {
		t.Fatalf("%v %v %v", ms(got.FirstOutput), ms(got.FirstText), ms(got.AfterFirst))
	}
	// text first: one reading serves both
	w = NewStopwatch(stepClock())
	w.Sent()
	w.Text() // 10
	w.Done() // 20
	got = w.Timing()
	if ms(got.FirstOutput) != int64(10) || ms(got.FirstText) != int64(10) || ms(got.AfterFirst) != int64(10) {
		t.Fatalf("%v %v %v", ms(got.FirstOutput), ms(got.FirstText), ms(got.AfterFirst))
	}
	// no text at all (a tool call): the first text is absent, not the first output
	w = NewStopwatch(stepClock())
	w.Sent()
	w.Output()
	w.Done()
	if got = w.Timing(); got.FirstText != nil || got.FirstOutput == nil {
		t.Fatalf("%+v", got)
	}
	// never output (a non-streamed answer): everything is absent, not the total
	w = NewStopwatch(stepClock())
	w.Sent()
	w.Done()
	if got = w.Timing(); got.FirstOutput != nil || got.FirstText != nil || got.AfterFirst != nil {
		t.Fatalf("%+v", got)
	}
	// a stream that failed after its first output has no end
	w = NewStopwatch(stepClock())
	w.Sent()
	w.Output()
	if got = w.Timing(); got.FirstOutput == nil || got.AfterFirst != nil {
		t.Fatalf("%+v", got)
	}
	// not sent: nothing, and a nil watch is safe
	w = NewStopwatch(stepClock())
	w.Output()
	if got = w.Timing(); got.FirstOutput != nil {
		t.Fatalf("%+v", got)
	}
	var nilw *Stopwatch
	nilw.Sent()
	nilw.Output()
	nilw.Text()
	nilw.Done()
	_ = nilw.Timing()
}
