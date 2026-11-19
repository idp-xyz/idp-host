package api

import (
	"context"
	"strings"
	"testing"

	"idp-host/internal/agent"
	"idp-host/internal/run"
)

func docWith(m agent.ModelProfile, subs ...agent.SubagentRef) *agent.Document {
	d := &agent.Document{}
	d.Definition.Model = m
	d.Definition.Subagents = subs
	return d
}

func names(atts []run.Attachment) string {
	var out []string
	for _, a := range atts {
		out = append(out, a.Name)
	}
	return strings.Join(out, ",")
}

// D43: what a release may send content to decides whether its runs get the files above the ceiling. A provider is
// external unless the operator declared it the enterprise's own; so are a fallback, a routing candidate and a remote
// A2A subagent, because the run's content can reach any of them.
func TestFilesAboveTheCeilingAreWithheldFromExternalModels(t *testing.T) {
	atts := []run.Attachment{{Name: "public.txt", Sensitivity: "public"}, {Name: "plain.txt"}, {Name: "internal.txt", Sensitivity: "internal"},
		{Name: "confidential.txt", Sensitivity: "confidential"}, {Name: "restricted.txt", Sensitivity: "restricted"}}
	local := FilePolicy{Local: map[string]bool{"onprem": true}}
	for name, c := range map[string]struct {
		policy   FilePolicy
		doc      *agent.Document
		kept     string
		withheld string
		external string
	}{
		"external model, default ceiling": {local, docWith(agent.ModelProfile{Provider: "cloud"}),
			"public.txt,plain.txt,internal.txt", "confidential.txt,restricted.txt", "cloud"},
		"the enterprise's own model": {local, docWith(agent.ModelProfile{Provider: "onprem"}),
			"public.txt,plain.txt,internal.txt,confidential.txt,restricted.txt", "", ""},
		"own model with an external fallback": {local, docWith(agent.ModelProfile{Provider: "onprem", Fallbacks: []agent.ModelRef{{Provider: "cloud"}}}),
			"public.txt,plain.txt,internal.txt", "confidential.txt,restricted.txt", "cloud"},
		"own model with an external routing candidate": {local, docWith(agent.ModelProfile{Provider: "onprem",
			Routing: &agent.Routing{Candidates: []agent.Candidate{{Provider: "onprem"}, {Provider: "cloud-b"}, {Provider: "cloud-a"}}}}),
			"public.txt,plain.txt,internal.txt", "confidential.txt,restricted.txt", "cloud-a,cloud-b"},
		"own model with a remote A2A subagent": {local, docWith(agent.ModelProfile{Provider: "onprem"},
			agent.SubagentRef{Name: "local-helper"}, agent.SubagentRef{Name: "partner", A2A: &agent.A2ARef{CardURL: "https://partner.example/card.json"}}),
			"public.txt,plain.txt,internal.txt", "confidential.txt,restricted.txt", "a2a:https://partner.example/card.json"},
		"nothing declared local": {FilePolicy{}, docWith(agent.ModelProfile{Provider: "onprem"}),
			"public.txt,plain.txt,internal.txt", "confidential.txt,restricted.txt", "onprem"},
		"ceiling public": {FilePolicy{Ceiling: func(string) string { return "public" }}, docWith(agent.ModelProfile{Provider: "cloud"}),
			"public.txt", "plain.txt,internal.txt,confidential.txt,restricted.txt", "cloud"},
		"ceiling restricted": {FilePolicy{Ceiling: func(string) string { return "restricted" }}, docWith(agent.ModelProfile{Provider: "cloud"}),
			"public.txt,plain.txt,internal.txt,confidential.txt,restricted.txt", "", "cloud"},
	} {
		kept, withheld, _ := c.policy.screen("t1", c.policy.outward(context.Background(), "t1", c.doc, nil), atts)
		if names(kept) != c.kept || names(withheld) != c.withheld {
			t.Errorf("%s: kept %s, withheld %s", name, names(kept), names(withheld))
		}
		if got := strings.Join(c.policy.externalModels(c.doc), ","); got != c.external {
			t.Errorf("%s: external models %q", name, got)
		}
	}
	// the ceiling is the tenant's
	perTenant := FilePolicy{Ceiling: func(tenant string) string {
		if tenant == "t-high" {
			return "confidential"
		}
		return "internal"
	}}
	if kept, _, ceiling := perTenant.screen("t-high", perTenant.outward(context.Background(), "t-high", docWith(agent.ModelProfile{Provider: "cloud"}), nil), atts); names(kept) != "public.txt,plain.txt,internal.txt,confidential.txt" || ceiling != "confidential" {
		t.Errorf("%s %s", names(kept), ceiling)
	}
	if kept, _, ceiling := perTenant.screen("t-other", perTenant.outward(context.Background(), "t-other", docWith(agent.ModelProfile{Provider: "cloud"}), nil), atts); names(kept) != "public.txt,plain.txt,internal.txt" || ceiling != "internal" {
		t.Errorf("%s %s", names(kept), ceiling)
	}
	// the model is told names and levels, never anything that could be a reference to the file
	note := withheldNote([]run.Attachment{{Ref: "file-9", Name: "并购方案.docx", Sensitivity: "restricted", SHA256: "abc", Ticket: "a.b.c"}}, "internal")
	if !strings.Contains(note, "并购方案.docx") || !strings.Contains(note, "restricted") || !strings.Contains(note, "internal") || strings.Contains(note, "file-9") || strings.Contains(note, "a.b.c") {
		t.Errorf("%s", note)
	}
}
