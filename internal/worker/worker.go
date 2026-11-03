// Package worker claims runs and drives them through the harness (ADR-0001 D3, D11): a pool of
// goroutines polls the store, each claim is held with a heartbeat for as long as the harness
// works, and a claim that is lost (fenced) stops the harness at the next write.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"idp-host/internal/harness"
	"idp-host/internal/run"
)

// Store is what the worker needs beyond the harness.
type Store interface {
	Claim(ctx context.Context, workerID string, lease time.Duration) (*run.Claim, error)
	Heartbeat(ctx context.Context, c *run.Claim, lease time.Duration) error
	ExpireConfirmations(ctx context.Context) (int, error)
}

// Worker is the pool.
type Worker struct {
	ID          string
	Store       Store
	Harness     *harness.Harness
	Concurrency int
	Lease       time.Duration
	Poll        time.Duration
	Log         *slog.Logger

	busy sync.WaitGroup
}

// Run polls until ctx ends; it returns after every in-flight run has released its claim.
func (w *Worker) Run(ctx context.Context) {
	if w.Concurrency <= 0 {
		w.Concurrency = 2
	}
	if w.Lease == 0 {
		w.Lease = 30 * time.Second
	}
	if w.Poll == 0 {
		w.Poll = 500 * time.Millisecond
	}
	if w.Log == nil {
		w.Log = slog.Default()
	}
	var wg sync.WaitGroup
	for i := 0; i < w.Concurrency; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			w.loop(ctx, n == 0)
		}(i)
	}
	wg.Wait()
}

func (w *Worker) loop(ctx context.Context, sweeper bool) {
	lastSweep := time.Time{}
	for ctx.Err() == nil {
		if sweeper && time.Since(lastSweep) > 5*time.Second {
			if n, err := w.Store.ExpireConfirmations(ctx); err != nil && ctx.Err() == nil {
				w.Log.Warn("confirmation sweep failed", "error", err)
			} else if n > 0 {
				w.Log.Info("expired confirmations requeued runs", "runs", n)
			}
			lastSweep = time.Now()
		}
		c, err := w.Store.Claim(ctx, w.ID, w.Lease)
		if err != nil {
			if ctx.Err() == nil {
				w.Log.Warn("claim failed", "error", err)
				sleep(ctx, w.Poll)
			}
			continue
		}
		if c == nil {
			sleep(ctx, w.Poll)
			continue
		}
		w.serve(ctx, c)
	}
}

// Once claims and serves at most one run (tests, idp-host worker --once).
func (w *Worker) Once(ctx context.Context) (bool, error) {
	if w.Lease == 0 {
		w.Lease = 30 * time.Second
	}
	if w.Log == nil {
		w.Log = slog.Default()
	}
	c, err := w.Store.Claim(ctx, w.ID, w.Lease)
	if err != nil || c == nil {
		return false, err
	}
	w.serve(ctx, c)
	return true, nil
}

func (w *Worker) serve(ctx context.Context, c *run.Claim) {
	hctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(w.Lease / 3)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if err := w.Store.Heartbeat(hctx, c, w.Lease); err != nil {
					if errors.Is(err, run.ErrFenced) {
						w.Log.Warn("claim lost, stopping", "run", c.RunID, "epoch", c.Epoch)
						cancel()
						return
					}
					w.Log.Warn("heartbeat failed", "run", c.RunID, "error", err)
				}
			}
		}
	}()
	start := time.Now()
	err := w.Harness.Run(hctx, c)
	close(stop)
	switch {
	case err == nil:
		w.Log.Info("run turn finished", "run", c.RunID, "tenant", c.TenantID, "duration", time.Since(start).Round(time.Millisecond))
	case errors.Is(err, run.ErrFenced):
		w.Log.Warn("run turn fenced", "run", c.RunID, "epoch", c.Epoch)
	default:
		var f *run.Failure
		if errors.As(err, &f) {
			w.Log.Info("run failed", "run", c.RunID, "code", f.Code, "message", f.Message)
		} else {
			w.Log.Error("run turn aborted", "run", c.RunID, "error", err)
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
