package harness

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/cmiclient"
	"idp-host/internal/guard"
	"idp-host/internal/run"
)

func eventNamed(m *memStore, name string) []map[string]any {
	var out []map[string]any
	for _, ev := range m.events {
		if ev["name"] == name {
			out = append(out, ev["value"].(map[string]any))
		}
	}
	return out
}

// D44: a tool the HOST's configuration names always waits for the member, whatever the release's confirmation policy says;
// the release here says auto for it.
func TestHostConfigurationForcesConfirmationOfNamedTools(t *testing.T) {
	e := setup(t)
	e.h.Gateways["edge"].ConfirmTools = []string{"su*"}
	m := newMem(def(), user("please sum"), run.Input{})
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if m.status != run.StatusAwaitingInput || len(e.mcp.Calls()) != 0 {
		t.Fatalf("%s %v", m.status, e.mcp.Calls())
	}
	var pending *run.ToolCall
	for _, c := range m.calls {
		pending = c
	}
	if pending.State != run.CallPendingConfirmation || pending.Tool != "sum" {
		t.Fatalf("%+v", pending)
	}
	if req := eventNamed(m, agui.CustomConfirmationRequired); len(req) != 1 || req[0]["requiredBy"] != "host" {
		t.Fatalf("%v", req)
	}
	// a tool no pattern names follows the release
	e = setup(t)
	e.h.Gateways["edge"].ConfirmTools = []string{"web__*"}
	m = newMem(def(), user("please sum"), run.Input{})
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if m.status != run.StatusCompleted || len(e.mcp.Calls()) != 1 {
		t.Fatalf("%s %v", m.status, e.mcp.Calls())
	}
}

// D44: a release that says toolResults off is run with wrap when the run has external tools, and the stream says so once;
// without external tools "off" stays off.
func TestToolResultsAreWrappedWhenTheRunHasExternalToolsWhateverTheReleaseSays(t *testing.T) {
	off := def()
	off.Guards.Input = agent.InputGuard{ToolResults: guard.ModeOff, Context: guard.ModeOff}
	e := setup(t)
	m := newMem(off, user("please sum"), run.Input{ExternalTools: true})
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	var toolSeen string
	for _, msg := range e.model.Last() {
		if msg.Role == "tool" {
			toolSeen = msg.Content
		}
	}
	if !strings.HasPrefix(toolSeen, `<untrusted source="tool_result">`) {
		t.Fatalf("not wrapped: %q", toolSeen)
	}
	raised := eventNamed(m, agui.CustomInputGuardRaised)
	if len(raised) != 1 || raised[0]["configured"] != "off" || raised[0]["applied"] != "wrap" {
		t.Fatalf("%v", raised)
	}
	// a stored tool message stays raw
	if last := m.rc.Messages[len(m.rc.Messages)-2]; strings.Contains(last.Content, "<untrusted") {
		t.Fatalf("%+v", last)
	}
	// without external tools: off is off
	e = setup(t)
	m = newMem(off, user("please sum"), run.Input{})
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if len(eventNamed(m, agui.CustomInputGuardRaised)) != 0 || strings.Contains(strings.Join(m.types(), ","), agui.CustomGuardFlagged) {
		t.Fatal("nothing should be wrapped")
	}
}

// refreshCMI answers a resolve with a public, a confidential and an unlabelled entry and says the package is stale.
func refreshCMI(t *testing.T) *cmiclient.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/verify") {
			_, _ = fmt.Fprint(w, `{"packageId":"pkg_1","stale":true,"staleItems":["k1"],"redacted":false}`)
			return
		}
		it := func(id, level string) string {
			s := ""
			if level != "" {
				s = `"sensitivity":"` + level + `",`
			}
			return `{"kind":"knowledge","id":"` + id + `","source":"handbook","trust":"authoritative","ref":"r` + id + `","excerpt":"BODY-` + id + `",` + s + `"observedAt":"2026-11-01T00:00:00Z","tokens":3}`
		}
		_, _ = fmt.Fprint(w, `{"packageId":"pkg_2","digest":"sha256:`+strings.Repeat("ab", 32)+`","package":{"items":[`+it("1", "public")+`,`+it("2", "confidential")+`,`+it("3", "")+`],"budget":{"usedTokens":9}}}`)
	}))
	t.Cleanup(srv.Close)
	return &cmiclient.Client{Endpoint: srv.URL, HTTP: srv.Client()}
}

func contextSeen(e *env) string {
	var seen []string
	for _, msg := range e.model.Last() {
		if msg.Role == "developer" || msg.Role == "system" {
			seen = append(seen, msg.Content)
		}
	}
	return strings.Join(seen, "\n---\n")
}

// D44: refreshing a stale package applies the screen the run was created under (the ceiling is in its input), replaces
// the note of the first screening instead of adding another, and a run created without a screen gets everything.
func TestRefreshedContextIsScreenedWithTheCeilingRecordedAtCreation(t *testing.T) {
	d := def()
	d.Context = agent.ContextPolicy{CMI: true, OnStale: "refresh"}
	old := []agui.Context{{Description: "caller", Value: "mine"}, {Description: "CMI knowledge from handbook (old)", Value: "OLD-BODY"}, {Description: cmiclient.ContextCMIWithheld, Value: "OLD-NOTE"}}
	e := setup(t)
	e.h.CMI, e.h.CMIKey = refreshCMI(t), "k"
	m := newMem(d, user("please sum"), run.Input{Context: old, ContextCeiling: "internal", ContextTrigger: "external-model"})
	m.rc.Run.ContextPackageID = "pkg_1"
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if m.status != run.StatusCompleted {
		t.Fatalf("%s %s", m.status, m.failCode)
	}
	seen := contextSeen(e)
	for _, want := range []string{"BODY-1", "2 context entries", "1 confidential", "1 unknown", "mine"} {
		if !strings.Contains(seen, want) {
			t.Errorf("the model should see %q: %s", want, seen)
		}
	}
	for _, not := range []string{"BODY-2", "BODY-3", "OLD-BODY", "OLD-NOTE"} {
		if strings.Contains(seen, not) {
			t.Errorf("the model must not see %q: %s", not, seen)
		}
	}
	if w := eventNamed(m, agui.CustomContextWithheld); len(w) != 1 || w[0]["count"] != 2 || w[0]["ceiling"] != "internal" || w[0]["trigger"] != "external-model" {
		t.Fatalf("%v", w)
	}
	if st := eventNamed(m, agui.CustomContextStale); len(st) != 1 || st[0]["refreshedItems"] != 3 || st[0]["refreshedWithheld"] != 2 {
		t.Fatalf("%v", st)
	}
	// created without a screen (no external model, no external tool): the refresh gives everything, as before D44
	e = setup(t)
	e.h.CMI, e.h.CMIKey = refreshCMI(t), "k"
	m = newMem(d, user("please sum"), run.Input{Context: old[:2]})
	m.rc.Run.ContextPackageID = "pkg_1"
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if seen := contextSeen(e); !strings.Contains(seen, "BODY-2") || !strings.Contains(seen, "BODY-3") || len(eventNamed(m, agui.CustomContextWithheld)) != 0 {
		t.Fatalf("%s", seen)
	}
}

// D48: a run that was screened because of a sub-agent is screened the same way when its context is refreshed, and the
// event of the refresh carries the reason recorded at creation.
func TestRefreshedContextOfAParentWithAnOutwardSubagentIsScreenedAndSaysWhy(t *testing.T) {
	d := def()
	d.Context = agent.ContextPolicy{CMI: true, OnStale: "refresh"}
	e := setup(t)
	e.h.CMI, e.h.CMIKey = refreshCMI(t), "k"
	why := []string{"sub-agent web: may call an external tool"}
	m := newMem(d, user("please sum"), run.Input{Context: []agui.Context{{Description: "caller", Value: "mine"}}, ContextCeiling: "internal", ContextTrigger: agui.ScreenSubagent, ContextVia: why})
	m.rc.Run.ContextPackageID = "pkg_1"
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if seen := contextSeen(e); strings.Contains(seen, "BODY-2") || strings.Contains(seen, "BODY-3") || !strings.Contains(seen, "BODY-1") {
		t.Fatalf("%s", seen)
	}
	if w := eventNamed(m, agui.CustomContextWithheld); len(w) != 1 || w[0]["trigger"] != "subagent" || fmt.Sprint(w[0]["via"]) != fmt.Sprint(why) {
		t.Fatalf("%v", w)
	}
}

// D44: the store judges a child's release by the external-tool patterns the harness hands it.
func TestChildRunsAreCreatedWithTheGatewaysExternalToolPatterns(t *testing.T) {
	e := setup(t)
	e.h.Gateways["edge"].ExternalTools = []string{"web__*"}
	e.model.Script = delegating
	m := newMem(def(), user("please delegate"), run.Input{})
	m.rc.Release = subagentRelease(1)
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	for _, in := range m.inputs {
		if got := in.ExternalTools["edge"]; len(got) != 1 || got[0] != "web__*" {
			t.Fatalf("%+v", in.ExternalTools)
		}
		return
	}
	t.Fatal("no child run was created")
}
