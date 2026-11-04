package model

import (
	"sort"
	"strings"
	"sync"
	"time"

	"idp-host/internal/agent"
)

// Router orders the candidates of a model profile for one call (ADR-0001 D29). It is deterministic
// and in-process: cost from the declared prices, latency from an EWMA of what this process observed,
// capability from the declared capability sets. It never calls anything.
type Router struct {
	mu    sync.Mutex
	stats map[string]*stat
}

type stat struct {
	ewmaMs float64 // exponentially weighted latency
	calls  int
	errors int
	recent []bool // last 20 outcomes (true = error)
}

// NewRouter returns an empty router.
func NewRouter() *Router { return &Router{stats: map[string]*stat{}} }

// Needs describes the call being routed.
type Needs struct {
	Tools           bool // tools are offered, so the model must support tool calls
	EstimatedInput  int  // estimated prompt tokens
	MaxOutputTokens int
}

// Decision is one ordered candidate with the reason for its position.
type Decision struct {
	Provider string  `json:"provider"`
	Model    string  `json:"model"`
	Score    float64 `json:"score"`
	Reason   string  `json:"reason"`
}

func key(r agent.ModelRef) string { return r.Provider + "/" + r.Model }

// Observe records the outcome of a call for the latency strategy.
func (r *Router) Observe(ref agent.ModelRef, latency time.Duration, failed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.stats[key(ref)]
	if s == nil {
		s = &stat{}
		r.stats[key(ref)] = s
	}
	ms := float64(latency.Milliseconds())
	if s.calls == 0 {
		s.ewmaMs = ms
	} else {
		s.ewmaMs = 0.7*s.ewmaMs + 0.3*ms
	}
	s.calls++
	if failed {
		s.errors++
	}
	s.recent = append(s.recent, failed)
	if len(s.recent) > 20 {
		s.recent = s.recent[1:]
	}
}

func (r *Router) snapshot(ref agent.ModelRef) (ewmaMs float64, errorRate float64, known bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.stats[key(ref)]
	if s == nil || s.calls == 0 {
		return 0, 0, false
	}
	bad := 0
	for _, f := range s.recent {
		if f {
			bad++
		}
	}
	return s.ewmaMs, float64(bad) / float64(len(s.recent)), true
}

// Stats reports what the router has observed (for metrics).
func (r *Router) Stats() map[string]struct {
	EWMAMs float64
	Calls  int
	Errors int
} {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]struct {
		EWMAMs float64
		Calls  int
		Errors int
	}{}
	for k, s := range r.stats {
		out[k] = struct {
			EWMAMs float64
			Calls  int
			Errors int
		}{s.ewmaMs, s.calls, s.errors}
	}
	return out
}

// Order returns the attempt order for a profile. Without routing (or with the ordered strategy and
// no candidates) it is the primary followed by the fallbacks, as before D29. With candidates, the
// pool is the primary, the fallbacks and the candidates (deduplicated, candidate metadata applied);
// candidates the call's needs exclude are dropped, then the strategy sorts the rest. The primary
// wins ties under every strategy, so a profile without prices or observations behaves as ordered.
func (r *Router) Order(p agent.ModelProfile, needs Needs) ([]agent.ModelRef, []Decision) {
	base := append([]agent.ModelRef{{Provider: p.Provider, Model: p.Model}}, p.Fallbacks...)
	if p.Routing == nil || len(p.Routing.Candidates) == 0 {
		var ds []Decision
		for i, ref := range base {
			ds = append(ds, Decision{Provider: ref.Provider, Model: ref.Model, Score: float64(i), Reason: "ordered"})
		}
		return base, ds
	}
	meta := map[string]agent.Candidate{}
	for _, c := range p.Routing.Candidates {
		meta[key(c.Ref())] = c
	}
	var pool []agent.ModelRef
	seen := map[string]bool{}
	for _, ref := range append(base, refsOf(p.Routing.Candidates)...) {
		if !seen[key(ref)] {
			seen[key(ref)] = true
			pool = append(pool, ref)
		}
	}
	type scored struct {
		ref   agent.ModelRef
		index int
		score float64
		why   string
	}
	var kept []scored
	var dropped []Decision
	for i, ref := range pool {
		c, declared := meta[key(ref)]
		if declared {
			if needs.Tools && len(c.Capabilities) > 0 && !has(c.Capabilities, agent.CapabilityTools) {
				dropped = append(dropped, Decision{Provider: ref.Provider, Model: ref.Model, Score: -1, Reason: "dropped: no tools capability"})
				continue
			}
			if c.MaxContextTokens > 0 && needs.EstimatedInput+needs.MaxOutputTokens > c.MaxContextTokens {
				dropped = append(dropped, Decision{Provider: ref.Provider, Model: ref.Model, Score: -1, Reason: "dropped: context window too small"})
				continue
			}
		}
		s := scored{ref: ref, index: i}
		switch p.Routing.Strategy {
		case agent.RouteCost:
			in := float64(needs.EstimatedInput) / 1000 * c.CostPer1kInput
			out := float64(max(needs.MaxOutputTokens, 256)) / 1000 * c.CostPer1kOutput
			s.score = in + out
			s.why = "cost"
		case agent.RouteLatency:
			ewma, errRate, known := r.snapshot(ref)
			if known {
				s.score = ewma * (1 + 4*errRate) // a failing candidate is treated as slow
				s.why = "latency"
			} else {
				s.score = 0
				s.why = "latency: unobserved"
			}
		case agent.RouteCapability:
			s.score = -float64(len(c.Capabilities))
			s.why = "capability"
		default:
			s.score = float64(i)
			s.why = "ordered"
		}
		kept = append(kept, s)
	}
	sort.SliceStable(kept, func(a, b int) bool {
		if kept[a].score != kept[b].score {
			return kept[a].score < kept[b].score
		}
		return kept[a].index < kept[b].index
	})
	var order []agent.ModelRef
	var ds []Decision
	for _, s := range kept {
		order = append(order, s.ref)
		ds = append(ds, Decision{Provider: s.ref.Provider, Model: s.ref.Model, Score: s.score, Reason: s.why})
	}
	return order, append(ds, dropped...)
}

func refsOf(cs []agent.Candidate) []agent.ModelRef {
	out := make([]agent.ModelRef, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Ref())
	}
	return out
}

func has(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// EstimateTokens is the router's rough prompt size: about four characters per token.
func EstimateTokens(parts ...string) int {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	return (n + 3) / 4
}

// StrategyName normalizes a strategy for labels.
func StrategyName(p agent.ModelProfile) string {
	if p.Routing == nil || len(p.Routing.Candidates) == 0 {
		return agent.RouteOrdered
	}
	return strings.ToLower(p.Routing.Strategy)
}
