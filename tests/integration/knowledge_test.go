package integration

// KB8 against real PostgreSQL: the HOST's knowledge tools (D65), the question rewrite (D66), the citation counter of a run (D67)
// and the member's credential for the CMI (D68). The CMI is tests/fixtures/fakecmi, which follows docs/adr/kb8-cmi-api.md.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/api"
	"idp-host/internal/cmiclient"
	"idp-host/internal/credential"
	"idp-host/internal/run"
	"idp-host/tests/fixtures/fakecmi"
	"idp-host/tests/fixtures/fakemodel"
)

func handbookDoc() fakecmi.Doc {
	return fakecmi.Doc{ID: "doc-1", Title: "Returns policy", Source: "ops-handbook", KB: "ops", DocType: "policy",
		Blocks: []fakecmi.Block{
			{Text: "Returns are accepted within 30 days of delivery.", Locator: fakecmi.Locator{Page: 1, Headings: []string{"Returns"}}},
			{Text: "Refunds are paid within 14 days of the return.", Locator: fakecmi.Locator{Page: 2, Headings: []string{"Returns", "Refunds"}}},
			{Text: "The fuel surcharge is 12.5 percent of the freight.", Locator: fakecmi.Locator{Page: 3, Headings: []string{"Pricing"}}}}}
}

// knowledgeEnv wires the fake CMI into the API and the harness, reached by delegation as a real deployment reaches it: the
// caller's token is exchanged for one of the member's own ("member:<sub>"), session-bound, and the fake CMI takes the subject from it.
func knowledgeEnv(t *testing.T, docs ...fakecmi.Doc) (*env, *fakecmi.Server) {
	t.Helper()
	e := newEnv(t)
	fc := fakecmi.New(docs...)
	cmi := httptest.NewServer(fc.Handler())
	t.Cleanup(cmi.Close)
	var refreshes atomic.Int32
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.PostForm.Get("grant_type") {
		case "urn:ietf:params:oauth:grant-type:token-exchange":
			if r.PostForm.Get("audience") != "cmi" {
				http.Error(w, `{"error":"invalid_target"}`, 400)
				return
			}
			parts := strings.Split(r.PostForm.Get("subject_token"), ".")
			raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
			var claims struct {
				Sub string `json:"sub"`
			}
			_ = json.Unmarshal(raw, &claims)
			_, _ = fmt.Fprintf(w, `{"access_token":"member:%s","expires_in":3600,"refresh_token":"rt-%s"}`, claims.Sub, claims.Sub)
		case "refresh_token":
			refreshes.Add(1)
			sub := strings.TrimPrefix(r.PostForm.Get("refresh_token"), "rt-")
			_, _ = fmt.Fprintf(w, `{"access_token":"member:%s","expires_in":3600,"refresh_token":"rt-%s"}`, sub, sub)
		default:
			http.Error(w, `{"error":"unsupported_grant_type"}`, 400)
		}
	}))
	t.Cleanup(idp.Close)
	x := &credential.Exchanger{TokenEndpoint: idp.URL, ClientID: "idp-host", ClientSecret: "s", Audience: "cmi", SessionBound: true, HTTP: idp.Client()}
	client := &cmiclient.Client{Endpoint: cmi.URL, HTTP: cmi.Client()}
	e.host.CMI, e.host.CMIAuth, e.host.Rewriter = client, api.GatewayAuth{Exchanger: x}, e.harness
	e.harness.CMI, e.harness.CMIRefresh = client, x.Refresh
	// the HOST has credentials of its own for the CMI; the tools must never present them (D68), and the cleanup fails the test if one did
	e.harness.CMIKey, e.host.CMIKey = "host-service-key", "host-service-key"
	e.harness.CMIToken = func(context.Context) (string, error) { return "host-service-key", nil }
	t.Cleanup(func() {
		for _, r := range fc.Requests() {
			if strings.Contains(r.Bearer, "host-service-key") && !strings.HasSuffix(r.Path, "/verify") {
				t.Errorf("the HOST's own credential was presented to %s", r.Path)
			}
		}
	})
	e.host.Files = api.FilePolicy{Local: map[string]bool{"fake": true}} // the model is the enterprise's own: nothing is screened unless a test says so
	return e, fc
}

func knowledgeAgent(tools []string, rewrite string) agent.Definition {
	d := definition("", nil, nil)
	d.Context = agent.ContextPolicy{CMI: true, Purpose: "assistant", Tools: tools, Rewrite: rewrite}
	return d
}

// streamOf returns the run's events by name (the last of each name) and all of the named one, in order.
func (e *env) eventsOf(id, who, name string) []map[string]any {
	e.t.Helper()
	_, events, _ := e.sse("GET", "/v1/runs/"+id+"/events", who, nil)
	var out []map[string]any
	for _, ev := range events {
		if ev["name"] == name {
			out = append(out, ev["value"].(map[string]any))
		}
	}
	return out
}

func (e *env) credentialColumn(id string) (present bool, sealed []byte) {
	e.t.Helper()
	conn, err := pgx.Connect(context.Background(), e.ownerURL)
	if err != nil {
		e.t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if err := conn.QueryRow(context.Background(), `SELECT cmi_credential FROM runs WHERE id = $1`, id).Scan(&sealed); err != nil {
		e.t.Fatal(err)
	}
	return len(sealed) > 0, sealed
}

func TestKnowledgeToolsOnPostgres(t *testing.T) {
	e, fc := knowledgeEnv(t, handbookDoc(), fakecmi.Doc{ID: "doc-carol", Title: "Carol's notes", Source: "personal", KB: "personal-carol", Visible: []string{"carol"},
		Blocks: []fakecmi.Block{{Text: "returns: carol keeps her own notes on returns"}}})

	t.Run("a run with several packages: member identity, events, continuous numbers, the first package stays on the run", func(t *testing.T) {
		e.publish("librarian", knowledgeAgent(agent.KnowledgeTools, ""))
		e.model.Script = fakemodel.DefaultScript
		r := e.must(202, "POST", "/v1/agents/librarian/runs", "carol", runBody("kb-1", "know-dig: returns | fuel surcharge"), "Idempotency-Key", "kb1")
		id := r["runId"].(string)
		// the credential made at creation is kept with the run, sealed
		present, sealed := e.credentialColumn(id)
		if !present || strings.Contains(string(sealed), "member:carol") {
			t.Fatalf("the member's CMI credential is kept sealed: present %v", present)
		}
		e.drive()
		got := e.must(200, "GET", "/v1/runs/"+id, "carol", nil)
		if got["status"] != run.StatusCompleted {
			t.Fatalf("%v", got)
		}
		if present, _ := e.credentialColumn(id); present {
			t.Error("the credential is cleared when the run ends")
		}
		resolved := e.eventsOf(id, "carol", agui.CustomContextResolved)
		pk := e.eventsOf(id, "carol", agui.CustomContextPackage)
		// the first retrieval matched the three blocks and carol's note (4 entries), numbered 1-4; the tools go on from 5
		if len(resolved) != 1 || resolved[0]["packageId"] != got["contextPackageId"] || resolved[0]["firstIndex"] != float64(1) || resolved[0]["numbered"] != float64(4) || resolved[0]["query"] != "know-dig: returns | fuel surcharge" {
			t.Fatalf("%v %v", resolved, got["contextPackageId"])
		}
		if len(pk) != 2 || pk[0]["tool"] != "knowledge__search" || pk[0]["firstIndex"] != float64(5) || pk[0]["itemCount"] != float64(4) || pk[0]["query"] != "returns" ||
			pk[1]["tool"] != "knowledge__find" || pk[1]["firstIndex"] != float64(9) || pk[1]["itemCount"] != float64(1) || pk[1]["documentId"] != "doc-1" || pk[1]["text"] != "fuel surcharge" || pk[0]["packageId"] == resolved[0]["packageId"] {
			t.Fatalf("%v", pk)
		}
		// every CMI call was made as the member, and the knowledge tools reached the three endpoints they should
		for _, req := range fc.Requests() {
			if req.Bearer != "member:carol" && !strings.HasSuffix(req.Path, "/verify") { // D24's check of the first package is the HOST's own, as before
				t.Errorf("%s was called as %q", req.Path, req.Bearer)
			}
		}
		paths := []string{}
		for _, req := range fc.Requests() {
			if strings.HasSuffix(req.Path, "/verify") {
				continue
			}
			paths = append(paths, req.Path)
		}
		if strings.Join(paths, " ") != "/v1/context/resolve /v1/context/resolve /v1/context/find" {
			t.Errorf("%v", paths)
		}
		// the answer cites the number of the last find, which the model read
		var answer string
		for _, m := range e.model.Last() {
			if m.Role == "tool" && strings.Contains(m.Content, "knowledge__find") {
				answer = m.Content
			}
		}
		if !strings.Contains(answer, "[9] Returns policy") || !strings.Contains(answer, `<untrusted source="tool_result">`) {
			t.Errorf("%q", answer)
		}
		msgs := e.must(200, "GET", "/v1/threads/kb-1/messages", "carol", nil)
		if !strings.Contains(fmt.Sprint(msgs), "Answer: returns") {
			t.Errorf("%v", msgs)
		}
	})

	t.Run("another member does not see a personal document: not found or not visible", func(t *testing.T) {
		e.publish("librarian-b", knowledgeAgent(agent.KnowledgeTools, ""))
		e.model.Script = func(msgs []fakemodel.Message, tools []string) fakemodel.Turn {
			if last := msgs[len(msgs)-1]; last.Role == "tool" {
				return fakemodel.Turn{Text: "result: " + last.Content}
			}
			return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "knowledge__read", Args: `{"documentId":"doc-carol"}`}}}
		}
		defer func() { e.model.Script = fakemodel.DefaultScript }()
		r := e.must(202, "POST", "/v1/agents/librarian-b/runs", "bob", runBody("kb-2", "show me carol's notes"), "Idempotency-Key", "kb2")
		e.drive()
		calls := e.must(200, "GET", "/v1/runs/"+r["runId"].(string)+"/tool-calls", "bob", nil)["toolCalls"].([]any)
		if c := calls[0].(map[string]any); c["state"] != run.CallFailed || !strings.Contains(fmt.Sprint(c["result"]), "not found or not visible") {
			t.Fatalf("%v", c)
		}
		if reqs := fc.RequestsTo("/v1/context/read"); len(reqs) == 0 || reqs[len(reqs)-1].Bearer != "member:bob" {
			t.Errorf("%+v", reqs)
		}
	})

	t.Run("a release that does not turn the tools on offers none", func(t *testing.T) {
		d := knowledgeAgent(nil, "")
		e.publish("plain-context", d)
		r := e.must(202, "POST", "/v1/agents/plain-context/runs", "carol", runBody("kb-3", "returns?"), "Idempotency-Key", "kb3")
		e.drive()
		for _, name := range e.model.LastTools() {
			if strings.HasPrefix(name, "knowledge__") {
				t.Errorf("offered %s", name)
			}
		}
		if present, _ := e.credentialColumn(r["runId"].(string)); present {
			t.Error("a run that cannot use the tools keeps no member credential")
		}
		// a published release names the tools it turns on, and the API refuses an unknown one
		bad := knowledgeAgent([]string{"knowledge__delete"}, "")
		e.must(422, "POST", "/api/v1/agents", "alice", map[string]any{"name": "bad-tools", "displayName": "x", "definition": bad})
	})

	t.Run("a run whose tools the caller executes gets none and keeps no credential", func(t *testing.T) {
		body := runBody("kb-4", "returns?")
		body["forwardedProps"] = map[string]any{"idp.host/toolExecution": "caller"}
		r := e.must(202, "POST", "/v1/agents/librarian/runs", "carol", body, "Idempotency-Key", "kb4")
		if present, _ := e.credentialColumn(r["runId"].(string)); present {
			t.Error("kept a credential")
		}
		e.drive()
		for _, name := range e.model.LastTools() {
			if strings.HasPrefix(name, "knowledge__") {
				t.Errorf("offered %s", name)
			}
		}
	})

	t.Run("an external model or tool: the ceiling screens the tool results and the stream says so", func(t *testing.T) {
		e.host.Files = api.FilePolicy{} // no provider is declared the enterprise's own: the model may be outside
		defer func() { e.host.Files = api.FilePolicy{Local: map[string]bool{"fake": true}} }()
		secret := fakecmi.Doc{ID: "doc-deal", Title: "Deal", Source: "finance", KB: "fin", Blocks: []fakecmi.Block{
			{Text: "deal: the open price list", Sensitivity: "internal"}, {Text: "deal: BODY-CONFIDENTIAL margin is 31 percent", Sensitivity: "confidential"}}}
		fc.Add(secret)
		e.publish("screened", knowledgeAgent(agent.KnowledgeTools, ""))
		e.model.Script = func(msgs []fakemodel.Message, tools []string) fakemodel.Turn {
			if msgs[len(msgs)-1].Role == "tool" {
				return fakemodel.Turn{Text: "done"}
			}
			return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "knowledge__find", Args: `{"documentId":"doc-deal","text":"deal:"}`}}}
		}
		defer func() { e.model.Script = fakemodel.DefaultScript }()
		before := len(e.model.History())
		r := e.must(202, "POST", "/v1/agents/screened/runs", "carol", runBody("kb-5", "the deal"), "Idempotency-Key", "kb5")
		e.drive()
		id := r["runId"].(string)
		w := e.eventsOf(id, "carol", agui.CustomContextWithheld)
		var tool map[string]any
		for _, v := range w {
			if v["source"] == "knowledge_tool" {
				tool = v
			}
		}
		if tool == nil || tool["count"] != float64(1) || tool["tool"] != "knowledge__find" || tool["ceiling"] != "internal" || tool["trigger"] != "external-model" {
			t.Fatalf("%v", w)
		}
		for _, turn := range e.model.History()[before:] {
			for _, m := range turn {
				if strings.Contains(m.Content, "BODY-CONFIDENTIAL") {
					t.Fatalf("the model was given the confidential passage: %q", m.Content)
				}
			}
		}
		raw, _ := json.Marshal(e.eventsOf(id, "carol", agui.CustomContextPackage))
		if strings.Contains(string(raw), "BODY-CONFIDENTIAL") {
			t.Error("the event repeats what was withheld")
		}
	})
}

// A worker that dies in a knowledge call: another one sends the call again (it only reads), and the numbers go on from what the
// run's events say, with nothing numbered twice.
func TestKnowledgeNumbersSurviveAHandOverOnPostgres(t *testing.T) {
	e, fc := knowledgeEnv(t, handbookDoc())
	e.publish("librarian", knowledgeAgent(agent.KnowledgeTools, ""))
	e.model.Script = func(msgs []fakemodel.Message, tools []string) fakemodel.Turn {
		n := 0
		for _, m := range msgs {
			if m.Role == "assistant" {
				n++
			}
		}
		switch n {
		case 0:
			return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "knowledge__search", Args: `{"query":"refunds"}`}}}
		case 1:
			return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "knowledge__find", Args: `{"documentId":"doc-1","text":"fuel"}`}}}
		}
		return fakemodel.Turn{Text: "done"}
	}
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	var mu sync.Mutex
	finds := 0
	fc.Hold = func(path string) {
		if path != "/v1/context/find" {
			return
		}
		mu.Lock()
		finds++
		first := finds == 1
		mu.Unlock()
		if first {
			started <- struct{}{}
			<-release
		}
	}
	r := e.must(202, "POST", "/v1/agents/librarian/runs", "carol", runBody("kb-hand", "returns"), "Idempotency-Key", "kbh")
	id := r["runId"].(string)
	ctx := context.Background()
	c1, err := e.store.Claim(ctx, "w1", 300*time.Millisecond)
	if err != nil || c1 == nil {
		t.Fatal(err, c1)
	}
	ctx1, kill := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- e.harness.Run(ctx1, c1) }()
	<-started // worker 1 has made the first package (the search) and is in the find
	kill()
	<-done
	close(release)
	calls := e.must(200, "GET", "/v1/runs/"+id+"/tool-calls", "carol", nil)["toolCalls"].([]any)
	if len(calls) != 2 || calls[1].(map[string]any)["state"] != run.CallExecuting {
		t.Fatalf("setup: the find should rest as EXECUTING: %v", calls)
	}
	e.resumes(id, c1)
	if got := e.must(200, "GET", "/v1/runs/"+id, "carol", nil); got["status"] != run.StatusCompleted {
		t.Fatalf("%v", got)
	}
	resolved := e.eventsOf(id, "carol", agui.CustomContextResolved)
	pk := e.eventsOf(id, "carol", agui.CustomContextPackage)
	// "returns" matched the three blocks (1-3); the search for "refunds" matched one (4); the find, sent again by worker 2, is 5
	if resolved[0]["numbered"] != float64(3) || len(pk) != 2 || pk[0]["firstIndex"] != float64(4) || pk[0]["itemCount"] != float64(1) || pk[1]["firstIndex"] != float64(5) || pk[1]["itemCount"] != float64(1) {
		t.Fatalf("%v %v", resolved, pk)
	}
	var seen string
	for _, m := range e.model.Last() {
		if m.Role == "tool" {
			seen += m.Content
		}
	}
	if !strings.Contains(seen, "[4] Returns policy") || !strings.Contains(seen, "[5] Returns policy") || strings.Count(seen, "[5] Returns policy") != 1 {
		t.Errorf("%q", seen)
	}
	if evs := e.interrupted(id); len(evs) != 1 || evs[0]["resent"] != true || evs[0]["tool"] != "knowledge__find" {
		t.Errorf("the stream says the find was sent again: %v", evs)
	}
}

func TestTheQuestionRewriteOnPostgres(t *testing.T) {
	e, fc := knowledgeEnv(t, handbookDoc())
	e.model.Rewrite = func(history []fakemodel.Message, q string) fakemodel.Turn {
		return fakemodel.Turn{Text: "What does the Returns policy say about the second rule?"}
	}
	var threads int
	turns := func(text string, earlier bool) map[string]any {
		threads++
		msgs := []map[string]any{}
		if earlier {
			msgs = append(msgs, map[string]any{"id": "m1", "role": "user", "content": "what are the return rules"}, map[string]any{"id": "m2", "role": "assistant", "content": "There are two rules."})
		}
		msgs = append(msgs, map[string]any{"id": fmt.Sprintf("m3-%d", threads), "role": "user", "content": text})
		return map[string]any{"threadId": fmt.Sprintf("rw-%d", threads), "messages": msgs, "tools": []any{}, "context": []any{}}
	}
	rewrites := func() int {
		n := 0
		for _, turn := range e.model.History() {
			if len(turn) > 0 && strings.HasPrefix(turn[0].Content, cmiclient.RewriteMarker) {
				n++
			}
		}
		return n
	}
	lastResolve := func() map[string]any {
		reqs := fc.RequestsTo("/v1/context/resolve")
		return reqs[len(reqs)-1].Body
	}

	t.Run("on, with earlier turns: the rewritten question is the query and the event says what was used", func(t *testing.T) {
		e.publish("rewriter", knowledgeAgent(nil, agent.RewriteModel))
		first := turns("and the second one?", true)
		r := e.must(202, "POST", "/v1/agents/rewriter/runs", "carol", first, "Idempotency-Key", "rw1")
		id := r["runId"].(string)
		if q := lastResolve()["query"]; q != "What does the Returns policy say about the second rule?" || rewrites() != 1 {
			t.Fatalf("the CMI was asked %q (%d rewrites)", q, rewrites())
		}
		if got := e.model.Efforts(); got[len(got)-1] != "low" && got[0] != "low" {
			t.Errorf("efforts %v", got)
		}
		e.drive()
		rw := e.eventsOf(id, "carol", agui.CustomContextRewrite)
		res := e.eventsOf(id, "carol", agui.CustomContextResolved)
		if len(rw) != 1 || rw[0]["outcome"] != "rewritten" || rw[0]["original"] != "and the second one?" || rw[0]["query"] != "What does the Returns policy say about the second rule?" || rw[0]["mode"] != "model" ||
			len(res) != 1 || res[0]["query"] != rw[0]["query"] {
			t.Fatalf("%v %v", rw, res)
		}
		// the stream has the rewrite before the package
		_, events, _ := e.sse("GET", "/v1/runs/"+id+"/events", "carol", nil)
		order := []string{}
		for _, ev := range events {
			if n, _ := ev["name"].(string); n == agui.CustomContextRewrite || n == agui.CustomContextResolved {
				order = append(order, n)
			}
		}
		if strings.Join(order, ",") != agui.CustomContextRewrite+","+agui.CustomContextResolved {
			t.Errorf("%v", order)
		}
		// the same request replays the same run (the rewrite and the first retrieval are made again before the store finds the replay,
		// as the retrieval always was: D32)
		if again := e.must(200, "POST", "/v1/agents/rewriter/runs", "carol", first, "Idempotency-Key", "rw1"); again["runId"] != id {
			t.Fatal("replay returned another run")
		}
	})

	t.Run("no earlier turns: no model call, the message is the query", func(t *testing.T) {
		before := rewrites()
		e.must(202, "POST", "/v1/agents/rewriter/runs", "carol", turns("the first question", false), "Idempotency-Key", "rw2")
		if rewrites() != before || lastResolve()["query"] != "the first question" {
			t.Fatalf("%d %d %v", before, rewrites(), lastResolve()["query"])
		}
	})

	t.Run("the release switch: off (the default) never calls the model", func(t *testing.T) {
		before := rewrites()
		e.publish("no-rewrite", knowledgeAgent(nil, ""))
		e.must(202, "POST", "/v1/agents/no-rewrite/runs", "carol", turns("and the third one?", true), "Idempotency-Key", "rw3")
		e.publish("off-rewrite", knowledgeAgent(nil, agent.RewriteOff))
		e.must(202, "POST", "/v1/agents/off-rewrite/runs", "carol", turns("and the fourth one??", true), "Idempotency-Key", "rw3b")
		if rewrites() != before || lastResolve()["query"] != "and the fourth one??" {
			t.Fatalf("%d %d %v", before, rewrites(), lastResolve()["query"])
		}
	})

	t.Run("the model fails: the raw message is the query and the event says why", func(t *testing.T) {
		e.model.Rewrite = func([]fakemodel.Message, string) fakemodel.Turn { return fakemodel.Turn{Status: 500} }
		defer func() {
			e.model.Rewrite = func([]fakemodel.Message, string) fakemodel.Turn { return fakemodel.Turn{Text: "x"} }
		}()
		r := e.must(202, "POST", "/v1/agents/rewriter/runs", "carol", turns("and the fifth one?!", true), "Idempotency-Key", "rw4")
		if lastResolve()["query"] != "and the fifth one?!" {
			t.Fatalf("%v", lastResolve()["query"])
		}
		e.drive()
		rw := e.eventsOf(r["runId"].(string), "carol", agui.CustomContextRewrite)
		if len(rw) != 1 || rw[0]["outcome"] != "fallback" || rw[0]["reason"] != "error" || rw[0]["query"] != "and the fifth one?!" || rw[0]["original"] != "and the fifth one?!" {
			t.Fatalf("%v", rw)
		}
	})

	t.Run("an answer that is not one line is refused", func(t *testing.T) {
		e.model.Rewrite = func([]fakemodel.Message, string) fakemodel.Turn {
			return fakemodel.Turn{Text: "Sure! Here is the question:\nWhat is the second rule?"}
		}
		r := e.must(202, "POST", "/v1/agents/rewriter/runs", "carol", turns("and the sixth one?!?", true), "Idempotency-Key", "rw5")
		e.drive()
		rw := e.eventsOf(r["runId"].(string), "carol", agui.CustomContextRewrite)
		if lastResolve()["query"] != "and the sixth one?!?" || len(rw) != 1 || rw[0]["reason"] != "multiline" {
			t.Fatalf("%v %v", lastResolve()["query"], rw)
		}
	})

	t.Run("the CMI is down: the rewrite is not wasted and the stream says the context was skipped", func(t *testing.T) {
		before := rewrites()
		fc.FailWith["/v1/context/resolve"] = 503
		r := e.must(202, "POST", "/v1/agents/rewriter/runs", "carol", turns("and the seventh one?", true), "Idempotency-Key", "rw6")
		e.drive()
		if len(e.eventsOf(r["runId"].(string), "carol", agui.CustomContextSkipped)) != 1 || rewrites() != before+1 {
			t.Fatalf("%d %d", before, rewrites())
		}
		if len(e.eventsOf(r["runId"].(string), "carol", agui.CustomContextRewrite)) != 1 {
			t.Error("the query that was tried is recorded even when the package is not")
		}
	})
}
