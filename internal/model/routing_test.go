package model

import (
	"testing"
	"time"

	"idp-host/internal/agent"
)

func TestRouterOrdersByStrategy(t *testing.T) {
	r := NewRouter()
	plain := agent.ModelProfile{Provider: "a", Model: "m1", Fallbacks: []agent.ModelRef{{Provider: "b", Model: "m2"}}}
	order, ds := r.Order(plain, Needs{Tools: true})
	if len(order) != 2 || order[0].Provider != "a" || ds[0].Reason != "ordered" {
		t.Fatalf("%v %v", order, ds)
	}
	cands := []agent.Candidate{
		{Provider: "a", Model: "m1", CostPer1kInput: 3, CostPer1kOutput: 15, Capabilities: []string{"tools", "reasoning", "vision"}},
		{Provider: "cheap", Model: "small", CostPer1kInput: 0.1, CostPer1kOutput: 0.4, Capabilities: []string{"tools"}, MaxContextTokens: 8000},
		{Provider: "chat", Model: "notools", CostPer1kInput: 0.05, CostPer1kOutput: 0.1, Capabilities: []string{"json"}},
	}
	cost := plain
	cost.Routing = &agent.Routing{Strategy: agent.RouteCost, Candidates: cands}
	order, ds = r.Order(cost, Needs{Tools: true, EstimatedInput: 1000, MaxOutputTokens: 500})
	// the no-tools candidate is dropped; the cheap one wins; the undescribed fallback b costs nothing and comes first
	if order[0].Provider != "b" || order[1].Provider != "cheap" || order[2].Provider != "a" || len(order) != 3 {
		t.Fatalf("%v", order)
	}
	if ds[len(ds)-1].Provider != "chat" || ds[len(ds)-1].Reason != "dropped: no tools capability" {
		t.Fatalf("%v", ds)
	}
	// too large for the small context window
	order, _ = r.Order(cost, Needs{Tools: true, EstimatedInput: 9000, MaxOutputTokens: 500})
	for _, ref := range order {
		if ref.Provider == "cheap" {
			t.Fatal("cheap kept despite its context window")
		}
	}
	// latency: unobserved candidates keep their order; a slow or failing one sinks
	lat := plain
	lat.Routing = &agent.Routing{Strategy: agent.RouteLatency, Candidates: cands[:2]}
	order, _ = r.Order(lat, Needs{})
	if order[0].Provider != "a" {
		t.Fatalf("%v", order)
	}
	for i := 0; i < 3; i++ {
		r.Observe(agent.ModelRef{Provider: "a", Model: "m1"}, 900*time.Millisecond, i == 2)
		r.Observe(agent.ModelRef{Provider: "cheap", Model: "small"}, 100*time.Millisecond, false)
	}
	order, ds = r.Order(lat, Needs{})
	if order[0].Provider != "b" || order[1].Provider != "cheap" || order[2].Provider != "a" || ds[2].Score < 900 {
		t.Fatalf("%v %v", order, ds)
	}
	// capability: the most capable first
	capability := plain
	capability.Routing = &agent.Routing{Strategy: agent.RouteCapability, Candidates: cands}
	order, _ = r.Order(capability, Needs{})
	if order[0].Provider != "a" || order[1].Provider != "cheap" {
		t.Fatalf("%v", order)
	}
	if s := r.Stats(); s["a/m1"].Calls != 3 || s["a/m1"].Errors != 1 {
		t.Fatalf("%+v", s["a/m1"])
	}
	if EstimateTokens("abcd", "efgh") != 2 || StrategyName(cost) != "cost" || StrategyName(plain) != "ordered" {
		t.Fatal("helpers")
	}
}
