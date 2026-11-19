package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/authorization"
	"idp-host/internal/cmiclient"
	"idp-host/internal/run"
)

func docWithTools(provider string, gateway string, allow ...string) *agent.Document {
	d := docWith(agent.ModelProfile{Provider: provider})
	d.Definition.Tools = agent.ToolPolicy{Gateway: gateway, Allow: allow}
	return d
}

// D44: what makes a run "external" is a model outside the enterprise or a tool that sends content out; the tool side
// reads the release's effective tools and the patterns of the release's own gateway.
func TestTheTriggerIsAnExternalModelOrAnExternalTool(t *testing.T) {
	p := FilePolicy{Local: map[string]bool{"onprem": true}, ExternalTools: map[string][]string{"edge": {"web__*"}}}
	for name, c := range map[string]struct {
		doc  *agent.Document
		want string
		tool bool
	}{
		"own model, no tools":                    {docWithTools("onprem", ""), "", false},
		"own model, an internal tool":            {docWithTools("onprem", "edge", "parcel__get"), "", false},
		"own model, an external tool":            {docWithTools("onprem", "edge", "web__web_search"), "external-tools", true},
		"own model, every tool":                  {docWithTools("onprem", "edge", agent.AllowAll), "external-tools", true},
		"external model, no tools":               {docWithTools("cloud", ""), "external-model", false},
		"external model and external tool":       {docWithTools("cloud", "edge", "web__web_fetch"), "external-model+external-tools", true},
		"the patterns belong to another gateway": {docWithTools("onprem", "other", "web__web_search"), "", false},
	} {
		if got := p.outward(context.Background(), "t", c.doc, nil).Trigger; got != c.want || p.hasExternalTools(c.doc) != c.tool {
			t.Errorf("%s: %q %v", name, got, p.hasExternalTools(c.doc))
		}
	}
	// a skill brings its tools into the effective policy
	d := docWithTools("onprem", "edge", "parcel__get")
	d.Definition.Skills = []agent.SkillRef{{Name: "research"}}
	d.Skills = []agent.EmbeddedSkill{{Name: "research", Definition: agent.SkillDefinition{Tools: []string{"web__web_search"}}}}
	if !p.hasExternalTools(d) {
		t.Error("a skill's external tool was not seen")
	}
}

// D44: files above the ceiling are withheld from a run that may call an external tool even when its model is the
// enterprise's own (the D43 rule, with the second trigger).
func TestFilesAboveTheCeilingAreWithheldFromRunsWithExternalTools(t *testing.T) {
	atts := []run.Attachment{{Name: "a.txt", Sensitivity: "internal"}, {Name: "b.txt", Sensitivity: "confidential"}}
	p := FilePolicy{Local: map[string]bool{"onprem": true}, ExternalTools: map[string][]string{"edge": {"web__*"}}}
	kept, withheld, _ := p.screen("t1", p.outward(context.Background(), "t1", docWithTools("onprem", "edge", "web__web_search"), nil), atts)
	if names(kept) != "a.txt" || names(withheld) != "b.txt" {
		t.Fatalf("%s | %s", names(kept), names(withheld))
	}
	if kept, withheld, _ = p.screen("t1", p.outward(context.Background(), "t1", docWithTools("onprem", "edge", "parcel__get"), nil), atts); len(withheld) != 0 || len(kept) != 2 {
		t.Fatalf("an internal tool changes nothing: %s | %s", names(kept), names(withheld))
	}
}

func cmiServer(t *testing.T, items string) *cmiclient.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"packageId":"pkg_1","digest":"sha256:`+strings.Repeat("ab", 32)+`","package":{"items":[`+items+`],"budget":{"usedTokens":9}}}`)
	}))
	t.Cleanup(srv.Close)
	return &cmiclient.Client{Endpoint: srv.URL, HTTP: srv.Client()}
}

func cmiItem(id, level string) string {
	sens := ""
	if level != "-" {
		sens = `"sensitivity":"` + level + `",`
	}
	return `{"kind":"knowledge","id":"` + id + `","source":"handbook","trust":"authoritative","ref":"r` + id + `","title":"T` + id + `","excerpt":"BODY-` + id + `",` + sens + `"observedAt":"2026-11-01T00:00:00Z","tokens":3}`
}

func resolveWith(t *testing.T, items string, input run.Input) (run.Input, []agui.Event) {
	t.Helper()
	a := &API{CMI: cmiServer(t, items), CMIKey: "k", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	doc := &agent.Document{}
	doc.Definition.Context = agent.ContextPolicy{CMI: true}
	in := agui.RunAgentInput{Messages: []agui.Message{{ID: "u", Role: agui.RoleUser, Content: "question"}}}
	input.Context = []agui.Context{{Description: "caller", Value: "mine"}}
	r := httptest.NewRequest("POST", "/", nil)
	_, events, err := a.resolveContext(r, authorization.Principal{Tenant: "t1"}, doc, in, &input)
	if err != nil {
		t.Fatal(err)
	}
	return input, events
}

// D44: a run that is screened is given the CMI entries at or below the ceiling and a note of the rest; "items" is what
// the CMI returned and "appended" what was put at the tail of the context; the withheld event carries the details. A run
// that is not screened gets every entry, whatever its level.
func TestResolveContextScreensCMIEntriesOfAnExternalRun(t *testing.T) {
	items := strings.Join([]string{cmiItem("1", "internal"), cmiItem("2", "confidential"), cmiItem("3", "-"), cmiItem("4", "public")}, ",")
	input, events := resolveWith(t, items, run.Input{ContextCeiling: "internal", ContextTrigger: "external-tools"})
	var texts []string
	for _, c := range input.Context {
		texts = append(texts, c.Description+"|"+c.Value)
	}
	all := strings.Join(texts, "\n")
	if len(input.Context) != 4 || input.Context[0].Description != "caller" || strings.Contains(all, "BODY-2") || strings.Contains(all, "BODY-3") ||
		!strings.Contains(all, "BODY-1") || !strings.Contains(all, "BODY-4") || input.Context[3].Description != cmiclient.ContextCMIWithheld {
		t.Fatalf("%q", texts)
	}
	if len(events) != 2 || events[0]["name"] != agui.CustomContextResolved || events[1]["name"] != agui.CustomContextWithheld {
		t.Fatalf("%v", events)
	}
	res := events[0]["value"].(map[string]any)
	if res["items"] != 4 || res["appended"] != 3 {
		t.Fatalf("items is what the CMI returned, appended what the run got: %v", res)
	}
	w := events[1]["value"].(map[string]any)
	by := w["byLevel"].(map[string]int)
	if w["count"] != 2 || w["ceiling"] != "internal" || w["trigger"] != "external-tools" || by["confidential"] != 1 || by["unknown"] != 1 {
		t.Fatalf("%v", w)
	}
	// not screened: everything is given, no withheld event
	input, events = resolveWith(t, items, run.Input{})
	if len(input.Context) != 5 || len(events) != 1 || events[0]["value"].(map[string]any)["appended"] != 4 {
		t.Fatalf("%d %v", len(input.Context), events)
	}
	// screened, nothing above the ceiling: no note, no event
	input, events = resolveWith(t, cmiItem("1", "public"), run.Input{ContextCeiling: "internal", ContextTrigger: "external-model"})
	if len(input.Context) != 2 || len(events) != 1 {
		t.Fatalf("%d %v", len(input.Context), events)
	}
}
