package worker

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"idp-host/internal/run"
)

// claimsOnce hands out one run and then none, and counts the claims asked for.
type claimsOnce struct {
	handed atomic.Bool
	asked  atomic.Int32
}

func (s *claimsOnce) Claim(context.Context, string, time.Duration) (*run.Claim, error) {
	s.asked.Add(1)
	if s.handed.CompareAndSwap(false, true) {
		return &run.Claim{TenantID: "t", RunID: "run_1", WorkerID: "w", Epoch: 1}, nil
	}
	return nil, nil
}
func (s *claimsOnce) Heartbeat(context.Context, *run.Claim, time.Duration) error { return nil }
func (s *claimsOnce) ExpireConfirmations(context.Context) (int, error)           { return 0, nil }

// slowRun is a run turn that works until `finish` is closed, noting whether its context was cut short first.
type slowRun struct {
	started chan struct{}
	finish  chan struct{}
	cut     atomic.Bool
	cutAt   atomic.Int64 // unix nanoseconds
	ended   atomic.Bool
}

func newSlowRun() *slowRun {
	return &slowRun{started: make(chan struct{}), finish: make(chan struct{})}
}

func (r *slowRun) Run(ctx context.Context, _ *run.Claim) error {
	close(r.started)
	select {
	case <-r.finish:
		r.ended.Store(true)
		return nil
	case <-ctx.Done():
		r.cut.Store(true)
		r.cutAt.Store(time.Now().UnixNano())
		return run.ErrFenced
	}
}

func startWorker(t *testing.T, st Store, r Runner, grace time.Duration) (stop func(), exited <-chan struct{}) {
	t.Helper()
	w := &Worker{ID: "w", Store: st, Harness: r, Concurrency: 1, Lease: time.Minute, Poll: 5 * time.Millisecond, StopGrace: grace, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	t.Cleanup(cancel)
	return cancel, done
}

// D47: told to stop while a run is in progress, the worker claims nothing more but lets the run finish within the grace
// period: its context is not cut short, so a tool call in progress is not interrupted.
func TestAWorkerToldToStopLetsTheRunInProgressFinishWithinTheGracePeriod(t *testing.T) {
	st, r := &claimsOnce{}, newSlowRun()
	stop, exited := startWorker(t, st, r, 5*time.Second)
	<-r.started
	stop()
	time.Sleep(150 * time.Millisecond) // well past the moment the run would have been cut short without a grace period
	if r.cut.Load() {
		t.Fatal("the run was cut short although the grace period had not passed")
	}
	select {
	case <-exited:
		t.Fatal("the worker returned while a run was still in progress")
	default:
	}
	asked := st.asked.Load()
	time.Sleep(50 * time.Millisecond)
	if st.asked.Load() != asked {
		t.Error("the worker went on claiming runs after it was told to stop")
	}
	close(r.finish)
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("the worker did not return after the run finished")
	}
	if !r.ended.Load() || r.cut.Load() {
		t.Errorf("the run should have finished by itself: ended %v, cut short %v", r.ended.Load(), r.cut.Load())
	}
}

// D47: past the grace period the run is cut short and the worker returns: shutdown is not blocked beyond it.
func TestAWorkerCutsTheRunShortWhenTheGracePeriodIsOver(t *testing.T) {
	st, r := &claimsOnce{}, newSlowRun()
	stop, exited := startWorker(t, st, r, 200*time.Millisecond)
	<-r.started
	stoppedAt := time.Now()
	stop()
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("the worker did not return after the grace period")
	}
	if !r.cut.Load() {
		t.Fatal("the run was not cut short")
	}
	if d := time.Duration(r.cutAt.Load() - stoppedAt.UnixNano()); d < 190*time.Millisecond || d > time.Second {
		t.Errorf("the run was cut short %v after the stop; want about the 200ms grace period", d)
	}
}

// Without a grace period (the default of the type) the run is cut short at once, as before D47.
func TestAWorkerWithoutAGracePeriodCutsTheRunShortAtOnce(t *testing.T) {
	st, r := &claimsOnce{}, newSlowRun()
	stop, exited := startWorker(t, st, r, 0)
	<-r.started
	stop()
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("the worker did not return")
	}
	if !r.cut.Load() {
		t.Error("the run was not cut short")
	}
}

// obeyingRun hands the run over as soon as it is told to stop, as the harness does at its next boundary.
type obeyingRun struct {
	started chan struct{}
	cut     atomic.Bool
	handed  atomic.Bool
}

func (r *obeyingRun) Run(ctx context.Context, _ *run.Claim) error {
	close(r.started)
	select {
	case <-run.StopChan(ctx):
		r.handed.Store(ctx.Err() == nil)
		return nil
	case <-ctx.Done():
		r.cut.Store(true)
		return run.ErrFenced
	}
}

// D47: the worker tells a run in progress that it was told to stop (run.WithStop) without cancelling it, so the run can
// stop at its next boundary and hand itself over long before the grace period is over.
func TestAWorkerTellsTheRunInProgressToStopWithoutCancellingIt(t *testing.T) {
	st, r := &claimsOnce{}, &obeyingRun{started: make(chan struct{})}
	stop, exited := startWorker(t, st, r, time.Minute)
	<-r.started
	at := time.Now()
	stop()
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("the worker did not return after the run handed itself over")
	}
	if !r.handed.Load() || r.cut.Load() || time.Since(at) > 2*time.Second {
		t.Errorf("handed over %v, cut short %v after %v", r.handed.Load(), r.cut.Load(), time.Since(at))
	}
}
