package harness

// D71: a web tool that lists an outputSchema and answers with a structure valid against it is rendered from the structure; idp-web's text
// is never read. Anything else goes the older way and is recorded.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"idp-host/internal/agui"
	"idp-host/internal/run"
	"idp-host/tests/fixtures/fakemcp"
	"idp-host/tests/fixtures/fakemodel"
)

const outSearchSchema = `{"type": "object", "properties": {"citations": {"type": "array", "items": {"type": "object", "properties": {"n": {"type": "integer", "minimum": 1}, "title": {"type": "string"}, "url": {"type": "string", "minLength": 1}, "ref": {"type": "string", "minLength": 1, "maxLength": 4096}, "published": {"type": "string"}, "excerpt": {"type": "string"}}, "required": ["n", "title", "url", "ref"], "additionalProperties": false}}}, "required": ["citations"], "additionalProperties": false}`
const outReadSchema = `{"type": "object", "properties": {"citations": {"type": "array", "items": {"type": "object", "properties": {"n": {"type": "integer", "minimum": 1}, "title": {"type": "string"}, "url": {"type": "string", "minLength": 1}, "finalUrl": {"type": "string"}, "fetchedAt": {"type": "string"}, "rendered": {"type": "boolean"}, "sha256": {"type": "string", "pattern": "^[0-9a-f]{64}$"}, "ref": {"type": "string", "minLength": 1, "maxLength": 4096}, "text": {"type": "string"}, "start": {"type": "integer", "minimum": 0}, "end": {"type": "integer", "minimum": 0}, "total": {"type": "integer", "minimum": 0}, "nextOffset": {"type": "integer", "minimum": 0}, "truncated": {"type": "boolean"}, "notice": {"type": "string", "enum": ["no_text", "offset_past_end"]}, "pdf": {"type": "object", "properties": {"pages": {"type": "integer", "minimum": 0}, "pagesRead": {"type": "integer", "minimum": 0}, "cut": {"type": "boolean"}}, "required": ["pages", "pagesRead", "cut"], "additionalProperties": false}}, "required": ["n", "title", "url", "fetchedAt", "sha256", "text", "start", "end", "total", "truncated"], "additionalProperties": false}}}, "required": ["citations"], "additionalProperties": false}`

// idpWebD34 installs web_search and web_fetch as idp-web lists them after its D34. text is what the tool's own text says (the HOST must
// not read it: it carries idp-web's numbers and a marker); structure builds the structured result.
func idpWebD34(e *env, search func(args map[string]any) any, read func(args map[string]any) any) {
	const marker = "IDPWEB-TEXT-MARKER [77]"
	e.mcp.Tools = append(e.mcp.Tools,
		withSchema(fakemcp_tool("web_search", `{"type":"object","properties":{"query":{"type":"string"}}}`, true, func(_ string, args map[string]any) (string, any, bool) {
			return marker, search(args), false
		}), outSearchSchema),
		withSchema(fakemcp_tool("web_fetch", `{"type":"object","properties":{"url":{"type":"string"},"offset":{"type":"integer"}}}`, true, func(_ string, args map[string]any) (string, any, bool) {
			return marker, read(args), false
		}), outReadSchema))
	e.h.Gateways["edge"].ExternalTools = []string{"web_*"}
}

func withSchema(t fakemcp.Tool, schema string) fakemcp.Tool {
	t.OutputSchema = schema
	return t
}

func citesOf(items ...map[string]any) map[string]any {
	list := make([]any, len(items))
	for i, it := range items {
		list[i] = it
	}
	return map[string]any{"citations": list}
}

func sItem(n int, u string, extra map[string]any) map[string]any {
	m := map[string]any{"n": n, "title": "title " + u, "url": u, "ref": "ref-" + u}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func rItem(n int, u, text string, extra map[string]any) map[string]any {
	m := map[string]any{"n": n, "title": "title " + u, "url": u, "fetchedAt": "2026-11-26T08:00:00Z", "sha256": strings.Repeat("b", 64), "ref": "ref-" + u,
		"text": text, "start": 0, "end": len(text), "total": len(text), "truncated": false}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func numbered(m *memStore) []map[string]any { return eventsNamed(m, agui.CustomCitationsNumbered) }

func TestAWebResultWithAValidStructureIsRenderedFromItAndIdpWebsTextIsNeverRead(t *testing.T) {
	e := setup(t)
	idpWebD34(e,
		func(map[string]any) any {
			return citesOf(sItem(7, "https://x.example/a", map[string]any{"excerpt": "an excerpt [3]", "published": "2026-11-01"}), sItem(9, "https://x.example/b", nil))
		},
		func(map[string]any) any { return citesOf(rItem(9, "https://x.example/b", "the page of b", nil)) })
	e.model.Script = seq(call("web_search", `{"query":"q"}`), call("web_fetch", `{"url":"https://x.example/b"}`), fakemodel.Turn{Text: "see [1] [2] and [77]"})
	m := newWebRun(webDef(), "go", "run_1", "tok", run.Input{ExternalTools: true})
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	msgs := toolMessages2(m)
	if !strings.HasPrefix(msgs[0].Content, "[1] title https://x.example/a\naddress: https://x.example/a\nref: ref-https://x.example/a\npublished: 2026-11-01\nexcerpt: an excerpt ⟦3⟧") ||
		!strings.Contains(msgs[0].Content, "\n\n[2] title https://x.example/b") {
		t.Errorf("%q", msgs[0].Content)
	}
	// a read of the searched address keeps its HOST number
	if !strings.HasPrefix(msgs[1].Content, "[2] title https://x.example/b (bytes 0–13 of 13)") || !strings.Contains(msgs[1].Content, "\n\nthe page of b") {
		t.Errorf("%q", msgs[1].Content)
	}
	for _, c := range msgs {
		if strings.Contains(c.Content, "MARKER") || strings.Contains(c.Content, "[7]") || strings.Contains(c.Content, "[9]") {
			t.Errorf("idp-web's text or numbers reached the model: %q", c.Content)
		}
	}
	ev := numbered(m)
	if len(ev) != 2 || ev[0]["render"] != "structured" || ev[1]["render"] != "structured" || ev[1]["taken"] != 0.0 {
		t.Errorf("%v", ev)
	}
	src := sourcesOf(m)
	if fmt.Sprint(ns(src)) != "[1 2]" || src[1].SHA256 == "" || src[1].FetchedAt == "" || src[1].WebN != 9 {
		t.Errorf("%+v", src)
	}
	if ev := finalEvent(t, m); fmt.Sprint(ev["unverifiedCitations"]) != "[77]" {
		t.Errorf("%v", ev)
	}
}

func TestAnInvalidStructureFallsBackToTheTextAndIsRecorded(t *testing.T) {
	for name, structure := range map[string]any{
		"a required field missing": citesOf(map[string]any{"n": 7, "title": "t", "url": "https://x.example/a"}),
		"a wrong type":             citesOf(sItem(7, "https://x.example/a", map[string]any{"excerpt": 5})),
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			e.mcp.Tools = append(e.mcp.Tools, withSchema(fakemcp_tool("web_search", `{"type":"object"}`, true, func(string, map[string]any) (string, any, bool) {
				return "[7] page\naddress: https://x.example/a\nref: r\nexcerpt: from the text", structure, false
			}), outSearchSchema))
			e.h.Gateways["edge"].ExternalTools = []string{"web_*"}
			e.model.Script = seq(call("web_search", `{}`), fakemodel.Turn{Text: "done"})
			m := newWebRun(webDef(), "go", "run_1", "tok", run.Input{ExternalTools: true})
			e.h.Store = m
			if err := e.h.Run(context.Background(), claim); err != nil {
				t.Fatal(err)
			}
			ev := numbered(m)
			if len(ev) != 1 || ev[0]["render"] != "text" || ev[0]["structure"] != "schema_violation" {
				t.Fatalf("%v", ev)
			}
			if got := toolMessages2(m)[0].Content; !strings.HasPrefix(got, "[1] page\naddress: https://x.example/a") || !strings.Contains(got, "excerpt: from the text") {
				t.Errorf("the text path: %q", got)
			}
		})
	}
}

func TestAToolThatListsNoSchemaGoesTheTextWayAndIsRecordedAsSuch(t *testing.T) {
	e := setup(t)
	installWeb(e, map[string][]string{"q": {"https://x.example/1"}})
	e.model.Script = seq(call("web_search", `{"query":"q"}`), fakemodel.Turn{Text: "done"})
	m := newWebRun(webDef(), "go", "run_1", "tok", run.Input{ExternalTools: true})
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if ev := numbered(m); len(ev) != 1 || ev[0]["render"] != "text" || ev[0]["structure"] != "no_output_schema" {
		t.Errorf("%v", ev)
	}
}

func TestAnEmptyStructuredSearchIsAnswered(t *testing.T) {
	e := setup(t)
	idpWebD34(e, func(map[string]any) any { return citesOf() }, nil)
	e.model.Script = seq(call("web_search", `{"query":"q"}`), fakemodel.Turn{Text: "nothing found"})
	m := newWebRun(webDef(), "go", "run_1", "tok", run.Input{ExternalTools: true})
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if got := toolMessages2(m)[0].Content; got != "no results" || m.counter() != 1 || len(eventsNamed(m, agui.CustomCitationsUnnumbered)) != 0 {
		t.Errorf("%q", got)
	}
}

func TestContinuingAPageWithNextOffsetAndTheNoticesAreShownToTheModel(t *testing.T) {
	e := setup(t)
	page := strings.Repeat("p", 50)
	idpWebD34(e, nil, func(args map[string]any) any {
		off, _ := args["offset"].(float64)
		switch {
		case off == 0:
			return citesOf(rItem(4, "https://x.example/long", page, map[string]any{"total": 120, "end": 50, "truncated": true, "nextOffset": 50}))
		case off == 50:
			return citesOf(rItem(4, "https://x.example/long", page, map[string]any{"start": 50, "end": 100, "total": 120, "truncated": true, "nextOffset": 100}))
		}
		return citesOf(rItem(4, "https://x.example/long", "", map[string]any{"start": 120, "end": 120, "total": 120, "notice": "offset_past_end"}))
	})
	e.model.Script = seq(call("web_fetch", `{"url":"https://x.example/long"}`), call("web_fetch", `{"url":"https://x.example/long","offset":50}`), call("web_fetch", `{"url":"https://x.example/long","offset":500}`), fakemodel.Turn{Text: "done"})
	m := newWebRun(webDef(), "go", "run_1", "tok", run.Input{ExternalTools: true})
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	msgs := toolMessages2(m)
	if !strings.Contains(msgs[0].Content, "(bytes 0–50 of 120; the rest: web_fetch with offset 50)") || !strings.Contains(msgs[1].Content, "(bytes 50–100 of 120; the rest: web_fetch with offset 100)") ||
		!strings.Contains(msgs[2].Content, "(nothing at this offset: the page text is shorter)") {
		t.Errorf("%q", msgs)
	}
	for _, mm := range msgs {
		if !strings.HasPrefix(mm.Content, "[1] ") { // one address, one number, however many parts are read
			t.Errorf("%q", mm.Content)
		}
	}
	if m.counter() != 2 || len(sourcesOf(m)) != 1 {
		t.Errorf("counter %d, sources %d", m.counter(), len(sourcesOf(m)))
	}
}

func TestAMaximumSizeReadGoesThroughD40AsTheTextTheModelIsGiven(t *testing.T) {
	e := setup(t)
	page := strings.Repeat("0123456789abcdef", (12<<10)/16) // idp-web's page part: 12 KiB
	idpWebD34(e, nil, func(map[string]any) any {
		return citesOf(rItem(4, "https://x.example/big", page, map[string]any{"total": 40000, "end": len(page), "truncated": true, "nextOffset": len(page)}))
	})
	// the raw result is about twice the size (the text content repeats the page): it reaches the HOST whole and is not what D40 measures
	raw := "[4] title\n\n" + page + page[:4000] // the text content repeats the page: with the structure, twice the size
	e.mcp.Tools[len(e.mcp.Tools)-1].HandleAs = func(string, map[string]any) (string, any, bool) {
		return raw, citesOf(rItem(4, "https://x.example/big", page, map[string]any{"total": 40000, "end": len(page), "truncated": true, "nextOffset": len(page)})), false
	}
	e.model.Script = seq(call("web_fetch", `{"url":"https://x.example/big"}`), fakemodel.Turn{Text: "done"})
	m := newWebRun(webDef(), "go", "run_1", "tok", run.Input{ExternalTools: true})
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	got := toolMessages2(m)[0].Content
	if !strings.Contains(got, page) || strings.Contains(got, "[truncated") || len(got) > 16<<10 || !strings.HasPrefix(got, "[1] ") {
		t.Errorf("a full-size read reaches the model whole under D40's 16 KiB: %d bytes", len(got))
	}
	// and under a smaller limit the model's text is cut, whatever the size of the raw result
	e2 := setup(t)
	e2.h.ToolResultLimit = 4 << 10
	idpWebD34(e2, nil, func(map[string]any) any {
		return citesOf(rItem(4, "https://x.example/big", page, map[string]any{"total": 40000, "end": len(page), "truncated": true, "nextOffset": len(page)}))
	})
	e2.model.Script = seq(call("web_fetch", `{"url":"https://x.example/big"}`), fakemodel.Turn{Text: "done"})
	m2 := newWebRun(webDef(), "go", "run_1", "tok", run.Input{ExternalTools: true})
	e2.h.Store = m2
	if err := e2.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if got := toolMessages2(m2)[0].Content; len(got) > 4<<10 || !strings.Contains(got, "the rest: web_fetch with offset") || !strings.HasPrefix(got, "[1] ") {
		t.Errorf("%d bytes", len(got))
	}
}
