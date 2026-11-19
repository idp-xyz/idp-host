package integration

// D51 against real PostgreSQL: a tool the gateway marks as external ("io.idp.telos/external": true in the tool's _meta) is
// outward like one the operator lists, with nothing configured: the run's attachments and CMI context are screened when it is
// created, its results are wrapped, its citations recorded. Without the marker, and with a marker that is not the boolean
// true, nothing is screened; a marker that appears after the run was created refuses the call; a gateway that cannot be read
// when the run is created counts as outward.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/api"
	"idp-host/internal/cmiclient"
	"idp-host/internal/guard"
	"idp-host/internal/run"
)

func TestTheGatewaysExternalMarkerScreensARunOnPostgres(t *testing.T) {
	e := newEnv(t)
	web := &itWeb{pages: map[string][]string{"alpha": {"https://x.example/1", "https://x.example/2"}}}
	web.install(e.mcp)
	e.model.Script = webScript(web)
	setMeta := func(meta map[string]any) {
		for i := range e.mcp.Tools {
			if strings.HasPrefix(e.mcp.Tools[i].Name, "web_") {
				e.mcp.Tools[i].Meta = meta
			}
		}
	}
	marked := map[string]any{"io.idp.telos/external": true}

	// the operator lists nothing and the enterprise's own model is the only one
	e.host.Files = api.FilePolicy{Local: map[string]bool{"fake": true}}
	if len(e.harness.Gateways["edge"].ExternalTools) != 0 {
		t.Fatal("the operator listed external tools")
	}
	items := `[{"kind":"knowledge","id":"k1","source":"h","trust":"authoritative","ref":"h#1","title":"Open","excerpt":"BODY-OPEN","observedAt":"2026-11-01T00:00:00Z","sensitivity":"internal","tokens":3},
	{"kind":"knowledge","id":"k2","source":"h","trust":"authoritative","ref":"h#2","title":"Deal","excerpt":"BODY-DEAL","observedAt":"2026-11-01T00:00:00Z","sensitivity":"confidential","tokens":3}]`
	cmi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"packageId":"pkg_d51","digest":"sha256:`+strings.Repeat("ab", 32)+`","package":{"items":`+items+`,"budget":{"usedTokens":9}}}`)
	}))
	defer cmi.Close()
	client := &cmiclient.Client{Endpoint: cmi.URL, HTTP: cmi.Client()}
	e.host.CMI, e.host.CMIKey = client, "k"
	e.harness.CMI, e.harness.CMIKey = client, "k"
	d := definition("edge", []string{"web_search", "web_fetch"}, nil)
	d.Context = agent.ContextPolicy{CMI: true}
	d.Guards.Input = agent.InputGuard{ToolResults: guard.ModeOff, Context: guard.ModeOff}
	e.publish("partner-web", d)

	att := func(ref, level string) map[string]any {
		return map[string]any{"ref": ref, "name": map[string]string{"file-secret": "deal.txt", "file-plain": "notes.txt"}[ref], "mediaType": "text/plain", "size": 12, "sha256": strings.Repeat("ef", 32), "sensitivity": level,
			"ticket": "eyJhbGciOiJFUzI1NiJ9.eyJraW5kIjoi" + "cmVhZCJ9." + ref}
	}
	start := func(thread string) (id, input string) {
		body := runBody(thread, "alpha look it up")
		body["forwardedProps"] = map[string]any{"idp.host/attachments": []any{att("file-secret", "confidential"), att("file-plain", "internal")}}
		id = e.must(202, "POST", "/v1/agents/partner-web/runs", "carol", body, "Idempotency-Key", thread)["runId"].(string)
		conn, err := pgx.Connect(context.Background(), e.ownerURL)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(context.Background())
		if err := conn.QueryRow(context.Background(), `SELECT input::text FROM runs WHERE id = $1`, id).Scan(&input); err != nil {
			t.Fatal(err)
		}
		return id, input
	}
	eventsOf := func(id string) (map[string]map[string]any, string) {
		_, events, _ := e.sse("GET", "/v1/runs/"+id+"/events", "carol", nil)
		byName := map[string]map[string]any{}
		for _, ev := range events {
			if n, _ := ev["name"].(string); n != "" {
				byName[n], _ = ev["value"].(map[string]any)
			}
		}
		raw, _ := json.Marshal(events)
		return byName, string(raw)
	}
	webCalls := func() int {
		n := 0
		for _, c := range e.mcp.Calls() {
			if strings.HasPrefix(c.Name, "web_") {
				n++
			}
		}
		return n
	}
	sourcesN := func(id string) int {
		got, _ := e.must(200, "GET", "/v1/runs/"+id, "carol", nil)["sources"].([]any)
		return len(got)
	}
	finish := func(id string) {
		t.Helper()
		e.drive()
		if got := e.must(200, "GET", "/v1/runs/"+id, "carol", nil); got["status"] != run.StatusCompleted {
			t.Fatalf("%v", got)
		}
	}
	toolSaw := func(from int) (wrapped, plain int) {
		for _, turn := range e.model.History()[from:] {
			for _, m := range turn {
				if m.Role == "tool" && strings.Contains(m.Content, "a plain title") {
					if strings.HasPrefix(m.Content, "<untrusted") {
						wrapped++
					} else {
						plain++
					}
				}
			}
		}
		return
	}

	// the control: no marker, so nothing is screened and the results are not wrapped
	setMeta(nil)
	before := len(e.model.History())
	id, input := start("thr-m-0")
	if !strings.Contains(input, "file-secret") || strings.Contains(input, api.ContextAttachmentsWithheld) || !strings.Contains(input, "BODY-DEAL") {
		t.Fatalf("an unmarked tool screened the run: %s", input)
	}
	finish(id)
	if w, p := toolSaw(before); w != 0 || p == 0 {
		t.Fatalf("wrapped %d plain %d", w, p)
	}
	if n := sourcesN(id); n != 0 {
		t.Fatalf("an unmarked tool's citations were recorded: %d", n)
	}

	// markers that are not the boolean true count for nothing
	for i, v := range []any{"true", 1, nil, map[string]any{"v": true}} {
		setMeta(map[string]any{"io.idp.telos/external": v})
		if _, input := start(fmt.Sprintf("thr-m-v%d", i)); !strings.Contains(input, "file-secret") || strings.Contains(input, api.ContextAttachmentsWithheld) {
			t.Fatalf("%v counted as a marker: %s", v, input)
		}
	}
	e.drive()

	// the marker: attachments and CMI context are screened at creation, with nothing configured
	setMeta(marked)
	lists := e.mcp.Lists()
	before = len(e.model.History())
	id, input = start("thr-m-1")
	if e.mcp.Lists() != lists+1 {
		t.Errorf("creating the run made %d tools/list calls, want 1", e.mcp.Lists()-lists)
	}
	if strings.Contains(input, "file-secret") || strings.Contains(input, "eyJ") || !strings.Contains(input, "file-plain") || !strings.Contains(input, api.ContextAttachmentsWithheld) ||
		!strings.Contains(input, `"externalTools": true`) {
		t.Fatalf("the stored input: %s", input)
	}
	finish(id)
	ev, raw := eventsOf(id)
	if w := ev[agui.CustomAttachmentWithheld]; w == nil || w["trigger"] != "external-tools" || !strings.Contains(fmt.Sprint(w["via"]), "web_fetch, web_search") {
		t.Fatalf("%s", raw)
	}
	if w := ev[agui.CustomContextWithheld]; w == nil || w["count"] != float64(1) || w["trigger"] != "external-tools" || !strings.Contains(fmt.Sprint(w["via"]), "marked by the gateway") || strings.Contains(raw, "BODY-DEAL") {
		t.Fatalf("%s", raw)
	}
	if w, p := toolSaw(before); w == 0 || p != 0 {
		t.Fatalf("the results were not wrapped: wrapped %d plain %d", w, p)
	}
	if n := sourcesN(id); n != 2 {
		t.Fatalf("the citations of a marked tool were not recorded: %d", n)
	}

	// the stale case: created while the tools were not marked, marked by the time a worker runs them
	setMeta(nil)
	id, input = start("thr-m-2")
	if !strings.Contains(input, "file-secret") {
		t.Fatalf("%s", input)
	}
	setMeta(marked)
	calls := webCalls()
	finish(id)
	if webCalls() != calls {
		t.Fatalf("a call of a tool marked after creation reached the gateway: %+v", e.mcp.Calls())
	}
	if ev, raw = eventsOf(id); ev[agui.CustomToolDenied] == nil || ev[agui.CustomToolDenied]["marked"] != true || !strings.Contains(fmt.Sprint(ev[agui.CustomToolDenied]["reason"]), "without its context being screened") {
		t.Fatalf("%s", raw)
	}

	// the gateway cannot be read when the run is created: fail closed
	setMeta(nil)
	good := e.host.Gateways["edge"]
	bad := good
	bad.Endpoint = "http://127.0.0.1:1/mcp"
	e.host.Gateways["edge"] = bad
	id, input = start("thr-m-3")
	e.host.Gateways["edge"] = good
	if strings.Contains(input, "file-secret") || !strings.Contains(input, api.ContextAttachmentsWithheld) {
		t.Fatalf("a gateway that could not be read did not screen the run: %s", input)
	}
	finish(id)
	if ev, raw = eventsOf(id); !strings.Contains(fmt.Sprint(ev[agui.CustomAttachmentWithheld]["via"]), "could not be read") {
		t.Fatalf("%s", raw)
	}
}
