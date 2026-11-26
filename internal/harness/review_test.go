package harness

// The independent review of D70-D76: what a run that numbers citations lets a tool, a document or a remote agent say, how the same passage
// keeps its number, and what the model sends back about a heading it read.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"idp-host/internal/a2a"
	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/run"
	"idp-host/tests/fixtures/fakecmi"
	"idp-host/tests/fixtures/fakemodel"
)

func TestARunThatNumbersNeutralisesWhatEveryToolReturnsAndNothingElse(t *testing.T) {
	e, _ := knowEnv(t, handbook())
	e.mcp.Tools = append(e.mcp.Tools,
		fakemcp_tool("readme", `{"type":"object"}`, true, func(string, map[string]any) (string, any, bool) { return "README: see [2] and [1, 3]", nil, false }),
		fakemcp_tool("web_search", `{"type":"object"}`, true, func(string, map[string]any) (string, any, bool) { return "no citations here, but [3] is", nil, false }),
		fakemcp_tool("flaky", `{"type":"object"}`, true, func(string, map[string]any) (string, any, bool) { return "it failed [2]", nil, true }),
		fakemcp_tool("quiet", `{"type":"object"}`, true, func(string, map[string]any) (string, any, bool) { return "no numbers at all", nil, false }))
	e.h.Gateways["edge"].ExternalTools = []string{"web_*"}
	d := knowDef()
	d.Tools = webDef().Tools
	d.Tools.Allow = []string{"readme", "web_search", "flaky", "quiet"}
	e.model.Script = seq(call("knowledge__search", `{"query":"returns"}`), call("readme", `{}`), call("web_search", `{}`), call("flaky", `{}`), call("quiet", `{}`), fakemodel.Turn{Text: "see [2]"})
	m := newWebRun(d, "go", "run_1", "tok", run.Input{ExternalTools: true, ContextCeiling: "restricted", ContextTrigger: "external-tools"})
	runKnow(t, e, m)
	msgs := toolMessages2(m)
	if len(msgs) != 5 {
		t.Fatalf("%d tool messages", len(msgs))
	}
	if !strings.Contains(msgs[0].Content, "[2] Returns policy") { // knowledge source 2 exists
		t.Fatalf("%q", msgs[0].Content)
	}
	for i, want := range []string{"README: see ⟦2⟧ and ⟦1, 3⟧", "no citations here, but ⟦3⟧ is", "it failed ⟦2⟧"} {
		if !strings.Contains(msgs[i+1].Content, want) {
			t.Errorf("tool %d: %q lacks %q", i+1, msgs[i+1].Content, want)
		}
	}
	if msgs[4].Content != "no numbers at all" {
		t.Errorf("a result with nothing to neutralise is as it was: %q", msgs[4].Content)
	}
}

func TestARemoteAgentsAnswerIsNeutralisedAndALocalSubagentsIsNot(t *testing.T) {
	srv := httptest.NewServer(nil)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			fmt.Fprintf(w, `{"url":%q}`, srv.URL+"/rpc")
			return
		}
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":"1","result":{"kind":"message","messageId":"x","role":"agent","parts":[{"kind":"text","text":"the remote says [2] and [9]"}]}}`)
	})
	t.Cleanup(srv.Close)
	e := setup(t)
	e.h.A2A = &a2a.Client{HTTP: srv.Client()}
	d := def()
	d.Subagents = []agent.SubagentRef{{Name: "remote", Description: "R.", A2A: &agent.A2ARef{CardURL: srv.URL + "/card"}}, {Name: "helper", Description: "H."}}
	d.Normalize()
	e.model.Script = seq(fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "agent__remote", Args: `{"input":"x"}`}, {Name: "agent__helper", Args: `{"input":"y"}`}}}, fakemodel.Turn{Text: "done"})
	m := newMem(d, user("go"), run.Input{ContextNumbered: true}) // the run numbers: its first package was
	m.rc.Release = &agent.Document{Definition: d, Subagents: []agent.EmbeddedSubagent{{Name: "remote", A2A: &agent.A2ARef{CardURL: srv.URL + "/card"}}, {Name: "helper", AgentID: "a-helper", Version: 1, Digest: "sha256:" + strings.Repeat("ab", 32)}}}
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	var childID string
	for _, c := range m.calls {
		if c.Tool == "agent__helper" {
			childID = c.ChildRunID
		}
	}
	m.childEnds(childID, run.StatusCompleted, "the child says [3]", "")
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range m.calls {
		got[c.Tool] = c.Result
	}
	if !strings.HasPrefix(got["agent__remote"], "the remote says ⟦2⟧ and ⟦9⟧") {
		t.Errorf("a remote agent's numbers are not this thread's: %q", got["agent__remote"])
	}
	if got["agent__helper"] != "the child says [3]" {
		t.Errorf("a local sub-agent's numbers are the thread's own and stay: %q", got["agent__helper"])
	}
}

func TestThePassageTheThreadHasNumberedKeepsItsNumberWhenItComesAgain(t *testing.T) {
	e, _ := knowEnv(t, handbook())
	e.model.Script = seq(call("knowledge__search", `{"query":"returns"}`), call("knowledge__search", `{"query":"returns"}`), call("knowledge__find", `{"documentId":"doc-1","text":"fuel"}`), fakemodel.Turn{Text: "done"})
	m := newMem(knowDef(), user("returns"), run.Input{})
	runKnow(t, e, m)
	msgs := toolMessages2(m)
	if len(msgs) != 3 || !strings.Contains(msgs[0].Content, "[1] Returns policy") || !strings.Contains(msgs[0].Content, "[3] Returns policy") {
		t.Fatalf("%q", msgs[0].Content)
	}
	if !strings.Contains(msgs[1].Content, "[1] Returns policy") || !strings.Contains(msgs[1].Content, "[3] Returns policy") || strings.Contains(msgs[1].Content, "[4]") {
		t.Errorf("the same three passages keep their numbers: %q", msgs[1].Content)
	}
	// the page 3 passage found by the find is the one numbered 3
	if !strings.Contains(msgs[2].Content, "[3] Returns policy") || m.counter() != 4 {
		t.Errorf("%q counter %d", msgs[2].Content, m.counter())
	}
	if pk := packages(m); len(pk) != 3 || pk[1]["itemCount"] != 0.0 || pk[1]["shown"] != 3.0 {
		t.Errorf("%v", pk)
	}
}

func TestAHeadingTheModelReadWithBracketsGoesBackToTheCMIAsItWas(t *testing.T) {
	doc := handbook()
	doc.Blocks[1].Locator.Headings = []string{"Annex [2]", "Sheet [A]"}
	e, fc := knowEnv(t, doc)
	e.model.Script = seq(call("knowledge__search", `{"query":"refunds"}`), call("knowledge__read", `{"documentId":"doc-1","locator":{"headings":["Annex ⟦2⟧"]}}`),
		call("knowledge__find", `{"documentId":"doc-1","text":"paid ⟦2⟧"}`), fakemodel.Turn{Text: "done"})
	m := newMem(knowDef(), user("refunds"), run.Input{})
	runKnow(t, e, m)
	if first := toolMessages2(m)[0].Content; !strings.Contains(first, "Annex ⟦2⟧") {
		t.Fatalf("the heading is shown neutralised: %q", first)
	}
	reads, finds := fc.RequestsTo("/v1/context/read"), fc.RequestsTo("/v1/context/find")
	if len(reads) != 1 || len(finds) != 1 {
		t.Fatalf("%d %d", len(reads), len(finds))
	}
	if got := fmt.Sprint(reads[0].Body["locator"]); !strings.Contains(got, "Annex [2]") || strings.Contains(got, "⟦") {
		t.Errorf("the CMI got %s", got)
	}
	if finds[0].Body["text"] != "paid [2]" {
		t.Errorf("%v", finds[0].Body["text"])
	}
}

func TestARefreshOfAFirstPackageThatNumberedNothingStillNumbersAndNeutralises(t *testing.T) {
	d := def()
	d.Context = agent.ContextPolicy{CMI: true, OnStale: "refresh"}
	e := setup(t)
	e.h.CMI, e.h.CMIKey = refreshCMI(t), "k"
	// the first package had no knowledge item (so no entry carries a number) but it was numbered: the flag says so
	m := newMem(d, user("please sum"), run.Input{ContextNumbered: true})
	m.rc.Run.ContextPackageID = "pkg_1"
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	seen := contextSeen(e)
	if !strings.Contains(seen, "CMI [1] knowledge") || !strings.Contains(seen, "CMI [3] knowledge") || m.counter() != 4 {
		t.Errorf("counter %d: %s", m.counter(), seen)
	}
}

func TestAnEmptyListOfCitationsIsASearchThatFoundNothingNotAResultWithoutNumbers(t *testing.T) {
	e := setup(t)
	e.mcp.Tools = append(e.mcp.Tools, fakemcp_tool("web_search", `{"type":"object"}`, true, func(string, map[string]any) (string, any, bool) {
		return "no results", map[string]any{"citations": []any{}}, false
	}))
	e.h.Gateways["edge"].ExternalTools = []string{"web_*"}
	e.model.Script = seq(call("web_search", `{}`), fakemodel.Turn{Text: "nothing"})
	m := newWebRun(webDef(), "go", "run_1", "tok", run.Input{ExternalTools: true})
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if got := toolMessages2(m)[0].Content; got != "no results" || len(eventsNamed(m, agui.CustomCitationsUnnumbered)) != 0 || m.sources.Unnumbered != 0 {
		t.Errorf("%q %d", got, m.sources.Unnumbered)
	}
}

var _ = fakecmi.Doc{}
