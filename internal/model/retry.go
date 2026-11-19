package model

import (
	"math/rand/v2"
	"time"
)

// Defaults of the retry policy toward one provider (D45). They need no configuration: three requests in all per candidate,
// waits growing from one second and capped at twenty, a Retry-After honoured up to a minute.
const (
	DefaultAttempts      = 3
	DefaultBackoff       = time.Second
	DefaultMaxBackoff    = 20 * time.Second
	DefaultMaxRetryAfter = time.Minute
	DefaultIdleTimeout   = time.Minute // D46: silence tolerated between two chunks of a stream
)

// RetryPolicy is what the harness does when a candidate model answers with a retryable error before it has streamed
// anything (D45). The operator sets it on the provider's entry of the host configuration; zero fields take the defaults.
type RetryPolicy struct {
	Attempts      int           // requests in all per candidate, the first included (1 = never retry)
	Backoff       time.Duration // the wait before the first retry; it doubles for each further retry
	MaxBackoff    time.Duration // a computed wait never exceeds this
	MaxRetryAfter time.Duration // a provider asking for longer than this is not waited for: next candidate, or the run fails
	// Rand returns a number in [0, 1) for the jitter; nil uses the process's generator (tests inject a fixed one).
	Rand func() float64
}

// WithDefaults fills the zero fields.
func (p RetryPolicy) WithDefaults() RetryPolicy {
	if p.Attempts <= 0 {
		p.Attempts = DefaultAttempts
	}
	if p.Backoff <= 0 {
		p.Backoff = DefaultBackoff
	}
	if p.MaxBackoff <= 0 {
		p.MaxBackoff = DefaultMaxBackoff
	}
	if p.MaxRetryAfter <= 0 {
		p.MaxRetryAfter = DefaultMaxRetryAfter
	}
	if p.Rand == nil {
		p.Rand = rand.Float64
	}
	return p
}

// Wait is how long to wait before retry number retry (1 for the first retry), given the Retry-After the provider sent
// (zero when it sent none). The computed wait is Backoff doubled for each earlier retry, capped at MaxBackoff, with
// jitter that keeps between half and all of it, so that runs that failed together do not come back together. A longer
// Retry-After replaces it. ok is false when the provider asked for longer than MaxRetryAfter: the caller must not wait.
func (p RetryPolicy) Wait(retry int, retryAfter time.Duration) (wait time.Duration, ok bool) {
	p = p.WithDefaults()
	if retryAfter > p.MaxRetryAfter {
		return 0, false
	}
	d := p.Backoff
	for i := 1; i < retry && d < p.MaxBackoff; i++ {
		d *= 2
	}
	d = min(d, p.MaxBackoff)
	d = time.Duration(float64(d) * (0.5 + 0.5*p.Rand()))
	return max(d, retryAfter), true
}
