package harness

// D51: a tool the gateway marks as external (its _meta carries "io.idp.telos/external": true) is an outward tool for the
// harness exactly like one the operator lists: its results are wrapped whatever the release says, its citations are
// recorded, a child that may call it is judged outward, and, when the run was created without screening its context, a
// call of it is refused instead of being sent with context nobody screened.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/guard"
	"idp-host/internal/run"
	"idp-host/tests/fixtures/fakemodel"
)

const markerKey = "io.idp.telos/external"

// markWeb installs the web tools without any operator pattern and lists web_search and web_fetch with the given _meta.
func markWeb(e *env, meta map[string]any) {
	installWeb(e, map[string][]string{"q": {"https://a.example/p", "https://b.example/q"}})
	e.h.Gateways["edge"].ExternalTools = nil // the operator lists nothing: the marker alone makes the tools outward
	for i := range e.mcp.Tools {
		if strings.HasPrefix(e.mcp.Tools[i].Name, "web_") {
			e.mcp.Tools[i].Meta = meta
		}
	}
}

func webDefOff() agent.Definition {
	d := webDef()
	d.Guards.Input = agent.InputGuard{ToolResults: guard.ModeOff, Context: guard.ModeOff}
	return d
}

func toolMessage(e *env) string {
	for _, msg := range e.model.Last() {
		if msg.Role == "tool" {
			return msg.Content
		}
	}
	return ""
}

func runWeb(t *testing.T, e *env, in run.Input) *memStore {
	t.Helper()
	e.model.Script = seq(call("web_search", `{"query":"q"}`), fakemodel.Turn{Text: "done [1]"})
	m := newWebRun(webDefOff(), "find it", "run_1", "tok", in)
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if m.status != run.StatusCompleted {
		t.Fatalf("%s %s", m.status, m.failCode)
	}
	return m
}

// A marked tool behaves like one the operator listed: results wrapped although the release says off (and the stream says
// which tools the marker is the reason for), citations recorded. Without the marker neither happens.
func TestAGatewayMarkedToolIsOutwardLikeOneTheOperatorListed(t *testing.T) {
	e := setup(t)
	markWeb(e, map[string]any{markerKey: true})
	m := runWeb(t, e, run.Input{ExternalTools: true})
	if !strings.HasPrefix(toolMessage(e), `<untrusted source="tool_result">`) {
		t.Fatalf("not wrapped: %q", toolMessage(e))
	}
	raised := eventNamed(m, agui.CustomInputGuardRaised)
	if len(raised) != 1 || fmt.Sprint(raised[0]["marked"]) != "[web_fetch web_search]" {
		t.Fatalf("%v", raised)
	}
	if got := sourcesOf(m); len(got) != 2 {
		t.Fatalf("the citations of a marked tool are recorded: %+v", got)
	}
	// the control: the same tools unmarked and unlisted are not outward
	e = setup(t)
	markWeb(e, nil)
	m = runWeb(t, e, run.Input{})
	if strings.Contains(toolMessage(e), "<untrusted") || len(eventNamed(m, agui.CustomInputGuardRaised)) != 0 || len(sourcesOf(m)) != 0 {
		t.Fatalf("an unmarked tool is not outward: %q %+v", toolMessage(e), sourcesOf(m))
	}
}

// D51 (5): only the boolean true under that exact key counts; no other value, no other key, no _meta that is not an object.
func TestOnlyTheBooleanTrueUnderTheKeyMarksATool(t *testing.T) {
	for name, meta := range map[string]map[string]any{
		"the string true":         {markerKey: "true"},
		"the number 1":            {markerKey: 1},
		"null":                    {markerKey: nil},
		"false":                   {markerKey: false},
		"an object":               {markerKey: map[string]any{"value": true}},
		"an array":                {markerKey: []any{true}},
		"another key":             {"io.idp.telos/internal": true, "external": true},
		"the key in another case": {"IO.IDP.TELOS/EXTERNAL": true},
	} {
		e := setup(t)
		markWeb(e, meta)
		// created as not outward: a tool that does not count is called as before, and nothing of it is outward
		m := runWeb(t, e, run.Input{})
		if strings.Contains(toolMessage(e), "<untrusted") || len(sourcesOf(m)) != 0 || len(e.mcp.Calls()) == 0 || len(eventNamed(m, agui.CustomToolDenied)) != 0 {
			t.Errorf("%s: counted as external: %q %+v calls %d", name, toolMessage(e), sourcesOf(m), len(e.mcp.Calls()))
		}
	}
}

// D51 (3): the gateway marks a tool now that a run, created without screening its context, has: the call is refused with a
// clear error before it reaches the gateway, no confirmation is asked, and the run goes on; the model is told.
func TestAToolThatIsMarkedButWasNotWhenTheRunWasCreatedIsRefused(t *testing.T) {
	e := setup(t)
	markWeb(e, map[string]any{markerKey: true})
	d := webDefOff()
	d.Confirmation = agent.ConfirmationPolicy{Default: agent.ConfirmConfirm} // a refused call asks nobody
	e.model.Script = seq(call("web_search", `{"query":"q"}`), fakemodel.Turn{Text: "I could not search."})
	m := newWebRun(d, "find it", "run_1", "tok", run.Input{})
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if m.status != run.StatusCompleted || len(e.mcp.Calls()) != 0 || len(sourcesOf(m)) != 0 {
		t.Fatalf("%s: calls %v", m.status, e.mcp.Calls())
	}
	denied := eventNamed(m, agui.CustomToolDenied)
	if len(denied) != 1 || denied[0]["tool"] != "web_search" || denied[0]["marked"] != true || !strings.Contains(fmt.Sprint(denied[0]["reason"]), "without its context being screened") {
		t.Fatalf("%v", denied)
	}
	if len(eventNamed(m, agui.CustomConfirmationRequired)) != 0 {
		t.Fatal("a call that is refused is not put to the member")
	}
	if got := toolMessage(e); !strings.Contains(got, "not run:") {
		t.Fatalf("the model was not told: %q", got)
	}
	// the same run, created screened (for a model outside the enterprise, say): the call is made, and its results are wrapped
	e = setup(t)
	markWeb(e, map[string]any{markerKey: true})
	m = runWeb(t, e, run.Input{ContextCeiling: "internal", ContextTrigger: agui.ScreenExternalModel})
	if len(e.mcp.Calls()) == 0 || len(eventNamed(m, agui.CustomToolDenied)) != 0 || !strings.HasPrefix(toolMessage(e), "<untrusted") || len(sourcesOf(m)) != 2 {
		t.Fatalf("a screened run may call it: calls %v, %q", e.mcp.Calls(), toolMessage(e))
	}
	// and a tool the operator lists keeps its behaviour: it is not refused, only a marked one is
	e = setup(t)
	markWeb(e, map[string]any{markerKey: true})
	e.h.Gateways["edge"].ExternalTools = []string{"web_*"}
	runWeb(t, e, run.Input{})
	if len(e.mcp.Calls()) == 0 {
		t.Fatal("a tool the operator lists is not refused by the marker")
	}
}

// D51 (4): a child that may call a marked tool is judged outward by the store, from the patterns the harness hands it.
func TestChildRunsAreCreatedWithTheToolsTheGatewayMarks(t *testing.T) {
	for name, c := range map[string]struct {
		meta map[string]any
		want string
	}{"marked": {map[string]any{markerKey: true}, "[sum]"}, "not marked": {nil, "[]"}, "marker that is not true": {map[string]any{markerKey: "yes"}, "[]"}} {
		e := setup(t)
		e.mcp.Tools[0].Meta = c.meta // sum
		e.model.Script = delegating
		m := newMem(def(), user("please delegate"), run.Input{})
		m.rc.Release = subagentRelease(1)
		e.h.Store = m
		if err := e.h.Run(context.Background(), claim); err != nil {
			t.Fatal(err)
		}
		if len(m.inputs) == 0 {
			t.Fatalf("%s: no child run was created", name)
		}
		for _, in := range m.inputs {
			if got := fmt.Sprint(in.ExternalTools["edge"]); got != c.want {
				t.Errorf("%s: patterns %s, want %s", name, got, c.want)
			}
		}
	}
	if got := escapePattern(`a*b?c[d\e`); got != `a\*b\?c\[d\\e` {
		t.Errorf("%s", got)
	}
}
