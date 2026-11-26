package harness

// The second independent review of D70-D85: the reviewer's probes are the test cases.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/run"
	"idp-host/tests/fixtures/fakecmi"
	"idp-host/tests/fixtures/fakemodel"
)

// A heading the model read neutralised goes back to the CMI exactly as the document wrote it, whatever the brackets were.
func TestAHeadingGoesBackToTheCMIExactlyAsTheDocumentWroteIt(t *testing.T) {
	for _, original := range []string{"Annex 〖2〗", `Annex \[2\]`, "Annex [ 2 ]", "Annex &#91;2&#93;", "Annex [**2**]"} {
		t.Run(original, func(t *testing.T) {
			doc := handbook()
			doc.Blocks[1].Locator.Headings = []string{original}
			e, fc := knowEnv(t, doc)
			shown := "Annex ⟦"
			e.model.Script = func(msgs []fakemodel.Message, _ []string) fakemodel.Turn {
				var last string
				n := 0
				for _, m := range msgs {
					if m.Role == "tool" {
						last = m.Content
						n++
					}
				}
				switch n {
				case 0:
					return call("knowledge__search", `{"query":"refunds"}`)
				case 1:
					// the model writes the heading as it read it: the displayed form of the heading
					i := strings.Index(last, shown)
					j := strings.Index(last[i:], "\n")
					h := last[i : i+j]
					h = strings.TrimSpace(strings.SplitN(h, " > ", 2)[0])
					return call("knowledge__read", fmt.Sprintf(`{"documentId":"doc-1","locator":{"headings":[%q]}}`, h))
				}
				return fakemodel.Turn{Text: "done"}
			}
			m := newMem(knowDef(), user("refunds"), run.Input{})
			runKnow(t, e, m)
			reads := fc.RequestsTo("/v1/context/read")
			if len(reads) != 1 {
				t.Fatalf("%d reads; the model read %q", len(reads), toolMessages2(m)[0].Content)
			}
			hs, _ := reads[0].Body["locator"].(map[string]any)["headings"].([]any)
			if len(hs) != 1 || hs[0] != original {
				t.Errorf("the CMI got %v, the document says %q", hs, original)
			}
		})
	}
}

func TestEveryRunOfAThreadThatNumberedNeutralisesAndChecksEvenWithoutTheTools(t *testing.T) {
	e := setup(t)
	e.mcp.Tools = append(e.mcp.Tools, fakemcp_tool("readme", `{"type":"object"}`, true, func(string, map[string]any) (string, any, bool) { return "README: see [2]", nil, false }))
	d := def()
	d.Tools.Allow = []string{"readme"}
	d.Confirmation = agent.ConfirmationPolicy{Default: agent.ConfirmAuto}
	e.model.Script = seq(call("readme", `{}`), fakemodel.Turn{Text: "the readme says [2], and also [99] and [1]"})
	m := newMem(d, user("go"), run.Input{})
	m.next = 5 // the thread gave 1..4 in earlier runs; this run's release has no knowledge and no outward tool
	m.rc.ThreadNumbered = true
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if got := toolMessages2(m)[0].Content; !strings.HasPrefix(got, "README: see ⟦2⟧") {
		t.Errorf("a thread that numbered neutralises what a tool returns, whatever the release: %q", got)
	}
	// D49: [2] and [1] were given in the thread, [99] never; the run has no list of its own, so what it cited is unlisted
	if ev := finalEvent(t, m); fmt.Sprint(ev["unverifiedCitations"]) != "[99]" || fmt.Sprint(ev["unlistedCitations"]) != "[1 2]" {
		t.Errorf("%v", ev)
	}
	// a run of a thread that numbered nothing reads what it read before
	e2 := setup(t)
	e2.mcp.Tools = e.mcp.Tools
	e2.model.Script = seq(call("readme", `{}`), fakemodel.Turn{Text: "done"})
	m2 := newMem(d, user("go"), run.Input{})
	e2.h.Store = m2
	if err := e2.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if got := toolMessages2(m2)[0].Content; got != "README: see [2]" {
		t.Errorf("%q", got)
	}
}

func TestTheResultOfACallersOwnToolIsNeutralisedInAThreadThatNumbered(t *testing.T) {
	e := setup(t)
	d := def()
	msgs := append(user("look it up"),
		agui.Message{ID: "a1", Role: agui.RoleAssistant, ToolCalls: []agui.ToolCall{{ID: "call_x", Type: "function", Function: agui.FunctionCall{Name: "eer_lookup", Arguments: `{}`}}}},
		agui.Message{ID: "t1", Role: agui.RoleTool, ToolCallID: "call_x", Content: `the caller's tool says [2] and 【3】`})
	for _, numbered := range []bool{true, false} {
		m := newMem(d, msgs, run.Input{Tools: []agui.Tool{{Name: "eer_lookup", Description: "d", Parameters: []byte(`{"type":"object"}`)}}})
		m.rc.ThreadNumbered = numbered
		e.model.Script = seq(fakemodel.Turn{Text: "done"})
		e.h.Store = m
		if err := e.h.Run(context.Background(), claim); err != nil {
			t.Fatal(err)
		}
		var seen string
		for _, mm := range e.model.Last() {
			if mm.Role == "tool" {
				seen += mm.Content
			}
		}
		if numbered != strings.Contains(seen, "⟦2⟧") || numbered != strings.Contains(seen, "⟦3⟧") || (!numbered && !strings.Contains(seen, "says [2] and 【3】")) {
			t.Errorf("numbered=%v: the model read %q", numbered, seen)
		}
	}
}

// Waiting for the credential's lock can be cancelled, and the two credentials of a turn do not wait on each other.
func TestTheLockOfACredentialIsCancellableAndTheTwoCredentialsDoNotShareOne(t *testing.T) {
	tn := &turn{}
	ctx := context.Background()
	if err := tn.credMu.lock(ctx); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := tn.credMu.lock(cctx); err == nil || time.Since(start) > time.Second {
		t.Errorf("a wait for the lock must end with the context: %v after %v", err, time.Since(start))
	}
	// the CMI credential has a lock of its own
	done := make(chan error, 1)
	go func() { done <- tn.cmiMu.lock(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(time.Second):
		t.Error("the CMI credential waited for the gateway credential")
	}
	tn.credMu.unlock()
	tn.cmiMu.unlock()
	if err := tn.credMu.lock(ctx); err != nil { // and it can be taken again
		t.Error(err)
	}
}

var _ = fakecmi.Doc{}
