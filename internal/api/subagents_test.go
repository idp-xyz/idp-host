package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/run"
)

// tree is a set of releases a test resolves sub-agents from, keyed by digest.
type tree map[string]*agent.Document

func (t tree) release(_ context.Context, _, _, digest string) (*agent.Document, error) {
	if d, ok := t[digest]; ok {
		return d, nil
	}
	return nil, errors.New("not found")
}

// node is a release of provider/tools that delegates to the named sub-agents (embedded with the digest "d-<name>").
func node(provider, gateway string, allow []string, limit int, subs ...string) *agent.Document {
	d := docWithTools(provider, gateway, allow...)
	d.Definition.Limits.MaxSubagentDepth = limit
	for _, s := range subs {
		d.Definition.Subagents = append(d.Definition.Subagents, agent.SubagentRef{Name: s, Description: s})
		d.Subagents = append(d.Subagents, agent.EmbeddedSubagent{Name: s, AgentID: "a-" + s, Version: 1, Digest: "d-" + s})
	}
	return d
}

func remote(d *agent.Document, name string) *agent.Document {
	d.Definition.Subagents = append(d.Definition.Subagents, agent.SubagentRef{Name: name, Description: name, A2A: &agent.A2ARef{CardURL: "https://peer/" + name}})
	d.Subagents = append(d.Subagents, agent.EmbeddedSubagent{Name: name, A2A: &agent.A2ARef{CardURL: "https://peer/" + name}})
	return d
}

// D48: a run is outward when its own release is (D43, D44) or when a sub-agent it can delegate to is, at any depth D22
// lets it reach, or a remote agent is referenced, or a sub-agent cannot be judged. The parent then never sees data above
// the ceiling, so it cannot write it into a task.
func TestARunIsOutwardWhenAReachableSubagentOrARemoteAgentIs(t *testing.T) {
	p := FilePolicy{Local: map[string]bool{"onprem": true}, ExternalTools: map[string][]string{"edge": {"web__*"}}}
	cases := map[string]struct {
		root  *agent.Document
		subs  tree
		trig  string
		viaIn string // what the reasons must contain; "" when none are expected
	}{
		"no sub-agents": {node("onprem", "edge", []string{"echo"}, 0), nil, "", ""},
		"sub-agent with its own model and internal tools (the control)": {node("onprem", "edge", []string{"echo"}, 0, "calc"),
			tree{"d-calc": node("onprem", "edge", []string{"sum"}, 0)}, "", ""},
		"sub-agent with an outward tool":   {node("onprem", "edge", []string{"echo"}, 0, "web"), tree{"d-web": node("onprem", "edge", []string{"web__search"}, 0)}, "subagent", "sub-agent web: may call an external tool"},
		"sub-agent with an external model": {node("onprem", "edge", []string{"echo"}, 0, "ask"), tree{"d-ask": node("cloud", "edge", nil, 0)}, "subagent", "sub-agent ask: may use an external model (cloud)"},
		"two levels down": {node("onprem", "edge", []string{"echo"}, 1, "mid"),
			tree{"d-mid": node("onprem", "edge", nil, 2, "web"), "d-web": node("onprem", "edge", []string{"web__search"}, 0)}, "subagent", "sub-agent mid/web: may call an external tool"},
		"two levels down, but the middle one may not delegate (D22: cannot be reached)": {node("onprem", "edge", []string{"echo"}, 3, "mid"),
			tree{"d-mid": node("onprem", "edge", nil, 1, "web"), "d-web": node("onprem", "edge", []string{"web__search"}, 0)}, "", ""},
		"three levels down, within every limit": {node("onprem", "edge", []string{"echo"}, 1, "mid"),
			tree{"d-mid": node("onprem", "edge", nil, 2, "low"), "d-low": node("onprem", "edge", nil, 3, "web"), "d-web": node("onprem", "edge", []string{"web__search"}, 0)}, "subagent", "sub-agent mid/low/web: may call an external tool"},
		"a remote agent":                           {remote(node("onprem", "edge", []string{"echo"}, 0), "partner"), nil, "external-model+subagent", "remote agent partner"},
		"a remote agent two levels down":           {node("onprem", "edge", nil, 2, "mid"), tree{"d-mid": remote(node("onprem", "edge", nil, 2), "partner")}, "subagent", "remote agent mid/partner"},
		"a sub-agent whose release cannot be read": {node("onprem", "edge", nil, 0, "ghost"), tree{}, "subagent", "sub-agent ghost: its release cannot be read"},
		"own outward tool and a sub-agent":         {node("onprem", "edge", []string{"web__fetch"}, 0, "web"), tree{"d-web": node("onprem", "edge", []string{"web__search"}, 0)}, "external-tools+subagent", "sub-agent web"},
		"the operator no longer lists the tool":    {node("onprem", "edge", []string{"echo"}, 0, "web"), tree{"d-web": node("onprem", "edge", []string{"fetch__page"}, 0)}, "", ""},
	}
	for name, c := range cases {
		pol := p
		pol.Release = c.subs.release
		o := pol.outward(context.Background(), "t", c.root, nil)
		if o.Trigger != c.trig || (c.viaIn == "") != (len(o.Via) == 0) || !strings.Contains(strings.Join(o.Via, "\n"), c.viaIn) {
			t.Errorf("%s: trigger %q via %q", name, o.Trigger, o.Via)
		}
	}
	// no way to read releases at all: unknown, so outward
	if o := p.outward(context.Background(), "t", node("onprem", "edge", nil, 0, "calc"), nil); o.Trigger != "subagent" {
		t.Errorf("without a resolver: %q", o.Trigger)
	}
}

// D48: the walk ends on a cycle and on a chain deeper than D22 allows, and reads a bounded number of releases; what it
// cannot finish judging counts as outward.
func TestTheWalkOverSubagentsTerminatesAndIsBounded(t *testing.T) {
	p := FilePolicy{Local: map[string]bool{"onprem": true}}
	// a release that names itself (a digest cycle that D22 forbids at compile time, but a store could hold one)
	loop := tree{"d-a": node("onprem", "edge", nil, 5, "a")}
	p.Release = loop.release
	if o := p.outward(context.Background(), "t", node("onprem", "edge", nil, 5, "a"), nil); o.Trigger != "" {
		t.Errorf("a cycle of harmless releases is not outward: %q %q", o.Trigger, o.Via)
	}
	// a chain longer than the deepest nesting D22 allows, every release claiming a limit above the largest D22 allows (a store could hold one)
	chain := tree{}
	for i := 0; i < 12; i++ {
		chain[fmt.Sprintf("d-n%d", i)] = node("onprem", "edge", nil, 9, fmt.Sprintf("n%d", i+1))
	}
	p.Release = chain.release
	o := p.outward(context.Background(), "t", node("onprem", "edge", nil, 9, "n0"), nil)
	if o.Trigger != "subagent" || !strings.Contains(strings.Join(o.Via, "\n"), "too many levels or releases") {
		t.Errorf("a chain beyond the depth limit: %q %q", o.Trigger, o.Via)
	}
	// a wide tree: the reads stop at the bound
	wide := tree{}
	var names []string
	for i := 0; i < 20; i++ {
		n := fmt.Sprintf("w%d", i)
		names = append(names, n)
	}
	for _, n := range names {
		wide["d-"+n] = node("onprem", "edge", nil, 5, names...)
	}
	reads := 0
	p.Release = func(ctx context.Context, tenant, id, digest string) (*agent.Document, error) {
		reads++
		return wide.release(ctx, tenant, id, digest)
	}
	o = p.outward(context.Background(), "t", node("onprem", "edge", nil, 5, names...), nil)
	if reads > maxSubagentLoads || o.Trigger != "subagent" {
		t.Errorf("%d reads, %q", reads, o.Trigger)
	}
}

// D48: because the parent is outward, its attachments are screened with the same rule as a release with an external
// model, and the event says which sub-agent made it so (a name and a reason, no content).
func TestFilesAboveTheCeilingAreWithheldFromAParentWhoseSubagentIsOutward(t *testing.T) {
	atts := []run.Attachment{{Name: "a.txt", Sensitivity: "internal"}, {Name: "b.txt", Sensitivity: "confidential"}}
	p := FilePolicy{Local: map[string]bool{"onprem": true}, ExternalTools: map[string][]string{"edge": {"web__*"}},
		Release: tree{"d-web": node("onprem", "edge", []string{"web__search"}, 0), "d-calc": node("onprem", "edge", []string{"sum"}, 0)}.release}
	o := p.outward(context.Background(), "t", node("onprem", "edge", []string{"echo"}, 0, "web"), nil)
	kept, withheld, ceiling := p.screen("t", o, atts)
	if names(kept) != "a.txt" || names(withheld) != "b.txt" {
		t.Fatalf("%s | %s", names(kept), names(withheld))
	}
	ev := withheldEvents(withheld, ceiling, o)
	v := ev[0]["value"].(map[string]any)
	if v["trigger"] != "subagent" || fmt.Sprint(v["via"]) != "[sub-agent web: may call an external tool]" || strings.Contains(fmt.Sprint(v), "confidential content") {
		t.Fatalf("%v", v)
	}
	if kept, withheld, _ = p.screen("t", p.outward(context.Background(), "t", node("onprem", "edge", []string{"echo"}, 0, "calc"), nil), atts); len(withheld) != 0 || len(kept) != 2 {
		t.Fatalf("the control: %s | %s", names(kept), names(withheld))
	}
}

// D48: the CMI entries of a parent whose sub-agent is outward are screened, and the event carries the reason.
func TestCMIEntriesAreScreenedForAParentWhoseSubagentIsOutward(t *testing.T) {
	items := strings.Join([]string{cmiItem("1", "internal"), cmiItem("2", "confidential")}, ",")
	input, events := resolveWith(t, items, run.Input{ContextCeiling: "internal", ContextTrigger: agui.ScreenSubagent, ContextVia: []string{"sub-agent web: may call an external tool"}})
	for _, c := range input.Context {
		if strings.Contains(c.Value, "BODY-2") {
			t.Fatalf("%v", c)
		}
	}
	w := events[len(events)-1]["value"].(map[string]any)
	if events[len(events)-1]["name"] != agui.CustomContextWithheld || w["trigger"] != "subagent" || fmt.Sprint(w["via"]) != "[sub-agent web: may call an external tool]" {
		t.Fatalf("%v", events)
	}
}
