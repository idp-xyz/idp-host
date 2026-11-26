package integration

// KB8 against real PostgreSQL: the HOST's knowledge tools (D65), the question rewrite (D66), the citation counter of a run (D67)
// and the member's credential for the CMI (D68). The CMI is tests/fixtures/fakecmi, which follows docs/adr/kb8-cmi-api.md.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"idp-host/adapters/store/postgres"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-host/adapters/identity"
	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/api"
	"idp-host/internal/authorization"
	"idp-host/internal/cmiclient"
	"idp-host/internal/credential"
	"idp-host/internal/harness"
	"idp-host/internal/model"
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
			cmiExchanges.Add(1)
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
	// the question rewrite is made by the API's own harness (the worker's is another object), as main wires it
	e.host.CMI, e.host.CMIAuth = client, api.GatewayAuth{Exchanger: x}
	e.host.Rewriter = &harness.Harness{Providers: e.harness.Providers, Router: model.NewRouter(), Log: e.harness.Log}
	e.harness.CMI, e.harness.CMIRefresh = client, x.Refresh
	// the people who use the HOST as themselves; erin is one (carol and bob hold only host-user, as a service account may)
	e.host.MemberRoles = []string{"ws-member"}
	tok, err := identity.DevToken([]byte(devSecret), profile, "erin", e.tenantA, []string{authorization.RoleUser, "ws-member"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	e.tokens["erin"] = tok
	if e.tokens["frank"], err = identity.DevToken([]byte(devSecret), profile, "frank", e.tenantA, []string{authorization.RoleUser, "ws-member"}, time.Hour); err != nil {
		t.Fatal(err)
	}
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

// cmiExchanges counts the token exchanges for the CMI's audience.
var cmiExchanges atomic.Int32

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
	e, fc := knowledgeEnv(t, handbookDoc(), fakecmi.Doc{ID: "doc-carol", Title: "Carol's notes", Source: "personal", KB: "personal-carol", Visible: []string{"erin"},
		Blocks: []fakecmi.Block{{Text: "returns: carol keeps her own notes on returns"}}})

	t.Run("a run with several packages: member identity, events, continuous numbers, the first package stays on the run", func(t *testing.T) {
		e.publish("librarian", knowledgeAgent(agent.KnowledgeTools, ""))
		e.model.Script = fakemodel.DefaultScript
		r := e.must(202, "POST", "/v1/agents/librarian/runs", "erin", runBody("kb-1", "know-dig: returns | fuel surcharge"), "Idempotency-Key", "kb1")
		id := r["runId"].(string)
		// the credential made at creation is kept with the run, sealed
		present, sealed := e.credentialColumn(id)
		if !present || strings.Contains(string(sealed), "member:erin") {
			t.Fatalf("the member's CMI credential is kept sealed: present %v", present)
		}
		e.drive()
		got := e.must(200, "GET", "/v1/runs/"+id, "erin", nil)
		if got["status"] != run.StatusCompleted {
			t.Fatalf("%v", got)
		}
		if present, _ := e.credentialColumn(id); present {
			t.Error("the credential is cleared when the run ends")
		}
		resolved := e.eventsOf(id, "erin", agui.CustomContextResolved)
		pk := e.eventsOf(id, "erin", agui.CustomContextPackage)
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
			if req.Bearer != "member:erin" && !strings.HasSuffix(req.Path, "/verify") { // D24's check of the first package is the HOST's own, as before
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
		msgs := e.must(200, "GET", "/v1/threads/kb-1/messages", "erin", nil)
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
		r := e.must(202, "POST", "/v1/agents/librarian-b/runs", "frank", runBody("kb-2", "show me carol's notes"), "Idempotency-Key", "kb2")
		e.drive()
		calls := e.must(200, "GET", "/v1/runs/"+r["runId"].(string)+"/tool-calls", "frank", nil)["toolCalls"].([]any)
		if c := calls[0].(map[string]any); c["state"] != run.CallFailed || !strings.Contains(fmt.Sprint(c["result"]), "not found or not visible") {
			t.Fatalf("%v", c)
		}
		if reqs := fc.RequestsTo("/v1/context/read"); len(reqs) == 0 || reqs[len(reqs)-1].Bearer != "member:frank" {
			t.Errorf("%+v", reqs)
		}
	})

	t.Run("a release that does not turn the tools on offers none", func(t *testing.T) {
		d := knowledgeAgent(nil, "")
		e.publish("plain-context", d)
		r := e.must(202, "POST", "/v1/agents/plain-context/runs", "erin", runBody("kb-3", "returns policy"), "Idempotency-Key", "kb3")
		if present, _ := e.credentialColumn(r["runId"].(string)); present { // before the run ends and clears it
			t.Error("a run that cannot use the tools keeps no member credential")
		}
		e.drive()
		for _, name := range e.model.LastTools() {
			if strings.HasPrefix(name, "knowledge__") {
				t.Errorf("offered %s", name)
			}
		}
		// a release without the tools sees the text it saw before KB8: the first package's entries are not numbered
		var seen string
		for _, m := range e.model.Last() {
			if strings.HasPrefix(m.Content, "CMI ") {
				seen += m.Content
			}
		}
		if !strings.Contains(seen, "CMI knowledge from") || strings.Contains(seen, "CMI [") {
			t.Errorf("%q", seen)
		}
		// a published release names the tools it turns on, and the API refuses an unknown one
		bad := knowledgeAgent([]string{"knowledge__delete"}, "")
		e.must(422, "POST", "/api/v1/agents", "alice", map[string]any{"name": "bad-tools", "displayName": "x", "definition": bad})
	})

	t.Run("a run whose tools the caller executes gets none and keeps no credential", func(t *testing.T) {
		body := runBody("kb-4", "returns?")
		body["forwardedProps"] = map[string]any{"idp.host/toolExecution": "caller"}
		r := e.must(202, "POST", "/v1/agents/librarian/runs", "erin", body, "Idempotency-Key", "kb4")
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
		r := e.must(202, "POST", "/v1/agents/screened/runs", "erin", runBody("kb-5", "the deal"), "Idempotency-Key", "kb5")
		e.drive()
		id := r["runId"].(string)
		w := e.eventsOf(id, "erin", agui.CustomContextWithheld)
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
		raw, _ := json.Marshal(e.eventsOf(id, "erin", agui.CustomContextPackage))
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
	r := e.must(202, "POST", "/v1/agents/librarian/runs", "erin", runBody("kb-hand", "returns"), "Idempotency-Key", "kbh")
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
	calls := e.must(200, "GET", "/v1/runs/"+id+"/tool-calls", "erin", nil)["toolCalls"].([]any)
	if len(calls) != 2 || calls[1].(map[string]any)["state"] != run.CallExecuting {
		t.Fatalf("setup: the find should rest as EXECUTING: %v", calls)
	}
	e.resumes(id, c1)
	if got := e.must(200, "GET", "/v1/runs/"+id, "erin", nil); got["status"] != run.StatusCompleted {
		t.Fatalf("%v", got)
	}
	resolved := e.eventsOf(id, "erin", agui.CustomContextResolved)
	pk := e.eventsOf(id, "erin", agui.CustomContextPackage)
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
	if evs := e.eventsOf(id, "erin", agui.CustomToolInterrupted); len(evs) != 1 || evs[0]["resent"] != true || evs[0]["tool"] != "knowledge__find" {
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
		if used, rewriteTokens := e.tokensUsed(id), e.stepTokens(id); used <= 0 || used != int64(rewriteTokens) {
			t.Errorf("the run's own cap counts the rewrite: tokens_used %d, step 0 %d", used, rewriteTokens)
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
		calls, resolves, exchanges := rewrites(), len(fc.RequestsTo("/v1/context/resolve")), cmiExchanges.Load()
		if again := e.must(200, "POST", "/v1/agents/rewriter/runs", "carol", first, "Idempotency-Key", "rw1"); again["runId"] != id {
			t.Fatal("replay returned another run")
		}
		if cmiExchanges.Load() != exchanges {
			t.Error("a replay exchanged a token before it was found")
		}
		if rewrites() != calls || len(fc.RequestsTo("/v1/context/resolve")) != resolves {
			t.Errorf("a replay made a model call or a retrieval: %d %d", rewrites(), len(fc.RequestsTo("/v1/context/resolve")))
		}
		// the stored run: step 0 holds the rewrite's tokens, so that the tenant's budget counts them
		if steps := e.stepTokens(id); steps <= 0 {
			t.Errorf("the rewrite's tokens are not the run's: %d", steps)
		}
	})

	t.Run("the earlier turns are the thread's: a follow-up that carries only the new message is rewritten too", func(t *testing.T) {
		e.model.Rewrite = func(history []fakemodel.Message, q string) fakemodel.Turn {
			if len(history) < 2 || !strings.Contains(history[0].Content, "return rules") {
				t.Errorf("the model was not shown the stored turns: %+v", history)
			}
			return fakemodel.Turn{Text: "What is the second return rule?"}
		}
		msg := func(id, text string) map[string]any {
			return map[string]any{"threadId": "rw-hist", "messages": []map[string]any{{"id": id, "role": "user", "content": text}}, "tools": []any{}, "context": []any{}}
		}
		e.must(202, "POST", "/v1/agents/rewriter/runs", "carol", msg("h1", "what are the return rules"), "Idempotency-Key", "rwh1")
		e.drive()
		before := rewrites()
		e.must(202, "POST", "/v1/agents/rewriter/runs", "carol", msg("h2", "and the second one?"), "Idempotency-Key", "rwh2")
		if rewrites() != before+1 || lastResolve()["query"] != "What is the second return rule?" {
			t.Fatalf("%d %d %v", before, rewrites(), lastResolve()["query"])
		}
	})

	t.Run("a request the thread would refuse makes no model call", func(t *testing.T) {
		msg := func(thread, id, text string, earlier bool) map[string]any {
			ms := []map[string]any{}
			if earlier {
				ms = append(ms, map[string]any{"id": id + "e", "role": "user", "content": "what are the return rules"}, map[string]any{"id": id + "f", "role": "assistant", "content": "two rules"})
			}
			ms = append(ms, map[string]any{"id": id, "role": "user", "content": text})
			return map[string]any{"threadId": thread, "messages": ms, "tools": []any{}, "context": []any{}}
		}
		// a busy thread: the first run is queued and not driven
		e.must(202, "POST", "/v1/agents/rewriter/runs", "carol", msg("pf-busy", "b1", "first", false), "Idempotency-Key", "pf1")
		before, resolves := rewrites(), len(fc.RequestsTo("/v1/context/resolve"))
		e.must(409, "POST", "/v1/agents/rewriter/runs", "carol", msg("pf-busy", "b2", "and the second one?", true), "Idempotency-Key", "pf2")
		// another user posts to Carol's thread, with earlier turns of his own making
		e.must(403, "POST", "/v1/agents/rewriter/runs", "erin", msg("pf-busy", "b3", "and the third one?", true), "Idempotency-Key", "pf3")
		if rewrites() != before || len(fc.RequestsTo("/v1/context/resolve")) != resolves {
			t.Errorf("a refused request made a model call or a retrieval: %d -> %d", before, rewrites())
		}
		// and the history of a thread that is not the caller's is never the request's messages
		if _, err := e.store.ThreadMessages(context.Background(), e.tenantA, "erin", "pf-busy"); !errors.Is(err, postgres.ErrThreadNotOwned) {
			t.Errorf("another user's thread: %v", err)
		}
		e.drive()
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

	t.Run("the tenant's blocked patterns apply to the rewrite", func(t *testing.T) {
		e.must(200, "PUT", "/api/v1/policy", "alice", map[string]any{"blockedPatterns": []string{"TENANTBAN"}})
		defer e.must(200, "PUT", "/api/v1/policy", "alice", map[string]any{"blockedPatterns": []string{}})
		e.model.Rewrite = func([]fakemodel.Message, string) fakemodel.Turn {
			return fakemodel.Turn{Text: "What is the TENANTBAN rule?"}
		}
		r := e.must(202, "POST", "/v1/agents/rewriter/runs", "carol", turns("and the eighth one?", true), "Idempotency-Key", "rw7")
		e.drive()
		rw := e.eventsOf(r["runId"].(string), "carol", agui.CustomContextRewrite)
		if lastResolve()["query"] != "and the eighth one?" || len(rw) != 1 || rw[0]["reason"] != "guard" {
			t.Fatalf("%v %v", lastResolve()["query"], rw)
		}
	})

	t.Run("the daily budget is asked before the model is: an exhausted tenant makes no rewrite call", func(t *testing.T) {
		e.must(200, "PUT", "/api/v1/policy", "alice", map[string]any{"maxConcurrentRuns": 0, "tokensPerDay": 1})
		defer e.must(200, "PUT", "/api/v1/policy", "alice", map[string]any{"maxConcurrentRuns": 0, "tokensPerDay": 0})
		before := rewrites()
		st, out := e.call("POST", "/v1/agents/rewriter/runs", "carol", turns("and the ninth one?", true), "Idempotency-Key", "rw8")
		if st != 429 || !strings.Contains(fmt.Sprint(out), "budget") || rewrites() != before {
			t.Fatalf("%d %v rewrites %d -> %d", st, out, before, rewrites())
		}
	})
}

// stepTokens is what the run's step 0 (the question rewrite) was charged.
func (e *env) stepTokens(id string) int {
	e.t.Helper()
	conn, err := pgx.Connect(context.Background(), e.ownerURL)
	if err != nil {
		e.t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var n int
	if err := conn.QueryRow(context.Background(), `SELECT coalesce(sum(prompt_tokens + completion_tokens), 0) FROM run_steps WHERE run_id = $1 AND step = 0`, id).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) tokensUsed(id string) int64 {
	e.t.Helper()
	conn, err := pgx.Connect(context.Background(), e.ownerURL)
	if err != nil {
		e.t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var n int64
	if err := conn.QueryRow(context.Background(), `SELECT tokens_used FROM runs WHERE id = $1`, id).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) credentialRow(id string) (own bool, ref string) {
	e.t.Helper()
	conn, err := pgx.Connect(context.Background(), e.ownerURL)
	if err != nil {
		e.t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var sealed []byte
	var r *string
	if err := conn.QueryRow(context.Background(), `SELECT cmi_credential, cmi_credential_ref FROM runs WHERE id = $1`, id).Scan(&sealed, &r); err != nil {
		e.t.Fatal(err)
	}
	if r != nil {
		ref = *r
	}
	return len(sealed) > 0, ref
}

func offered(e *env) bool {
	for _, n := range e.model.LastTools() {
		if strings.HasPrefix(n, "knowledge__") {
			return true
		}
	}
	return false
}

// D68: only a member gets a member credential and the tools: not a principal that holds host-user alone (a service account), not a
// peer that comes in through A2A whatever roles it holds, nobody when no member roles are configured.
func TestOnlyMembersGetTheKnowledgeToolsOnPostgres(t *testing.T) {
	e, _ := knowledgeEnv(t, handbookDoc())
	e.publish("librarian", knowledgeAgent(agent.KnowledgeTools, ""))
	e.model.Script = fakemodel.DefaultScript

	// a member
	r := e.must(202, "POST", "/v1/agents/librarian/runs", "erin", runBody("om-1", "returns?"), "Idempotency-Key", "om1")
	if own, _ := e.credentialRow(r["runId"].(string)); !own {
		t.Error("a member's run keeps the credential")
	}
	e.drive()
	if !offered(e) {
		t.Error("a member is offered the tools")
	}
	// a principal that holds host-user and is not a member (carol; a service account that is a CMI writer would be the same)
	r = e.must(202, "POST", "/v1/agents/librarian/runs", "carol", runBody("om-2", "returns policy"), "Idempotency-Key", "om2")
	if own, _ := e.credentialRow(r["runId"].(string)); own {
		t.Error("a non-member's run keeps a credential")
	}
	e.drive()
	if offered(e) {
		t.Error("a non-member is offered the tools")
	}
	// numbering follows the credential, not the release: a run that has no tools in reach sees the text it saw before KB8
	var seen string
	for _, m := range e.model.Last() {
		if strings.HasPrefix(m.Content, "CMI ") {
			seen += m.Content
		}
	}
	if !strings.Contains(seen, "CMI knowledge from") || strings.Contains(seen, "CMI [") {
		t.Errorf("a run without a credential numbered its first package: %q", seen)
	}
	// an A2A peer holding the member role: not offered either
	msg := map[string]any{"kind": "message", "role": "user", "messageId": "om-a2a", "contextId": "om-3", "parts": []map[string]any{{"kind": "text", "text": "returns?"}}}
	res := e.must(200, "POST", "/a2a/agents/librarian", "erin", map[string]any{"jsonrpc": "2.0", "id": "1", "method": "message/send", "params": map[string]any{"message": msg}})
	id := res["result"].(map[string]any)["id"].(string)
	if own, _ := e.credentialRow(id); own {
		t.Error("an A2A run keeps a member credential")
	}
	e.drive()
	if offered(e) {
		t.Error("an A2A run is offered the tools")
	}
	// no member roles configured: nobody
	e.host.MemberRoles = nil
	r = e.must(202, "POST", "/v1/agents/librarian/runs", "erin", runBody("om-4", "returns?"), "Idempotency-Key", "om4")
	if own, _ := e.credentialRow(r["runId"].(string)); own {
		t.Error("with no member roles nobody keeps a credential")
	}
	e.drive()
	if offered(e) {
		t.Error("with no member roles nobody is offered the tools")
	}
}

// D68: a credential that cannot be opened, and one cleared by a cancel; a sub-agent uses the root's and holds no copy; the runs
// that share a credential renew it once.
func TestTheMemberCredentialOfARunOnPostgres(t *testing.T) {
	e, fc := knowledgeEnv(t, handbookDoc())
	e.publish("librarian", knowledgeAgent(agent.KnowledgeTools, ""))
	e.model.Script = func(msgs []fakemodel.Message, tools []string) fakemodel.Turn {
		if msgs[len(msgs)-1].Role == "tool" {
			return fakemodel.Turn{Text: "done"}
		}
		return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "knowledge__search", Args: `{"query":"returns"}`}}}
	}

	t.Run("cleared when the run is canceled", func(t *testing.T) {
		r := e.must(202, "POST", "/v1/agents/librarian/runs", "erin", runBody("mc-1", "returns?"), "Idempotency-Key", "mc1")
		id := r["runId"].(string)
		if own, _ := e.credentialRow(id); !own {
			t.Fatal("no credential")
		}
		e.must(200, "POST", "/v1/runs/"+id+"/cancel", "erin", nil)
		if own, _ := e.credentialRow(id); own {
			t.Error("a canceled run keeps its credential")
		}
	})

	t.Run("a credential that cannot be opened: the run completes without the tools", func(t *testing.T) {
		r := e.must(202, "POST", "/v1/agents/librarian/runs", "erin", runBody("mc-2", "returns?"), "Idempotency-Key", "mc2")
		id := r["runId"].(string)
		conn, err := pgx.Connect(context.Background(), e.ownerURL)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(context.Background())
		if _, err := conn.Exec(context.Background(), `UPDATE runs SET cmi_credential = '\x0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20'::bytea WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		before := len(fc.RequestsTo("/v1/context/resolve"))
		e.drive()
		if got := e.must(200, "GET", "/v1/runs/"+id, "erin", nil); got["status"] != run.StatusCompleted {
			t.Fatalf("%v", got)
		}
		if offered(e) || len(fc.RequestsTo("/v1/context/resolve")) != before {
			t.Error("the tools were offered or the CMI was called with a credential nobody could open")
		}
	})

	t.Run("kept when the first retrieval fails", func(t *testing.T) {
		d := knowledgeAgent(agent.KnowledgeTools, "")
		e.publish("librarian-down", d)
		fc.FailWith["/v1/context/resolve"] = 503
		r := e.must(202, "POST", "/v1/agents/librarian-down/runs", "erin", runBody("mc-0", "returns?"), "Idempotency-Key", "mc0")
		if own, _ := e.credentialRow(r["runId"].(string)); !own {
			t.Error("a skipped retrieval cost the run its credential")
		}
		e.drive()
	})

	t.Run("a grandchild behind a child without the tools has the credential", func(t *testing.T) {
		leaf := knowledgeAgent(agent.KnowledgeTools, "")
		e.publish("leaf-k", leaf)
		mid := definition("", nil, nil)
		mid.Limits = agent.DefaultLimits
		mid.Limits.MaxSubagentDepth = 2
		mid.Subagents = []agent.SubagentRef{{Name: "leaf-k", Description: "Looks things up."}}
		e.publish("mid-k", mid)
		top := definition("", nil, nil)
		top.Subagents = []agent.SubagentRef{{Name: "mid-k", Description: "Delegates."}}
		e.publish("top-k", top)
		e.model.Script = func(msgs []fakemodel.Message, tools []string) fakemodel.Turn {
			last := msgs[len(msgs)-1]
			has := func(n string) bool {
				for _, t := range tools {
					if t == n {
						return true
					}
				}
				return false
			}
			switch {
			case last.Role == "tool":
				return fakemodel.Turn{Text: "done"}
			case has("agent__mid-k"):
				return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "agent__mid-k", Args: `{"input":"go on"}`}}}
			case has("agent__leaf-k"):
				return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "agent__leaf-k", Args: `{"input":"find returns"}`}}}
			case has("knowledge__search"):
				return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "knowledge__search", Args: `{"query":"returns"}`}}}
			}
			return fakemodel.Turn{Text: "nothing"}
		}
		r := e.must(202, "POST", "/v1/agents/top-k/runs", "erin", runBody("mc-4", "go"), "Idempotency-Key", "mc4")
		root := r["runId"].(string)
		before := len(fc.RequestsTo("/v1/context/resolve"))
		var ids []string
		for i := 0; i < 2; i++ { // the root and the middle run each rest while their child runs
			if ok, err := e.worker.Once(context.Background()); err != nil || !ok {
				t.Fatal(ok, err)
			}
			parent := root
			if i == 1 {
				parent = ids[0]
			}
			kids := e.must(200, "GET", "/v1/runs/"+parent+"/subruns", "erin", nil)["runs"].([]any)
			ids = append(ids, kids[0].(map[string]any)["runId"].(string))
			if own, ref := e.credentialRow(ids[i]); own || ref != root {
				t.Fatalf("level %d: holds a copy (%v) or points at %q, not the root", i+1, own, ref)
			}
		}
		e.drive()
		if got := e.must(200, "GET", "/v1/runs/"+root, "erin", nil); got["status"] != run.StatusCompleted {
			t.Fatalf("%v", got)
		}
		reqs := fc.RequestsTo("/v1/context/resolve")
		if len(reqs) != before+1 || reqs[len(reqs)-1].Bearer != "member:erin" {
			t.Errorf("the leaf's search was not made as the member: %+v", reqs[before:])
		}
	})

	t.Run("a sub-agent points at the root's credential and the runs that share it renew it once", func(t *testing.T) {
		e.publish("helper-k", knowledgeAgent(agent.KnowledgeTools, ""))
		lead := definition("", nil, nil)
		lead.Subagents = []agent.SubagentRef{{Name: "helper-k", Description: "Looks things up."}}
		e.publish("lead-k", lead)
		e.model.Script = func(msgs []fakemodel.Message, tools []string) fakemodel.Turn {
			last := msgs[len(msgs)-1]
			has := func(n string) bool {
				for _, t := range tools {
					if t == n {
						return true
					}
				}
				return false
			}
			switch {
			case last.Role == "tool":
				return fakemodel.Turn{Text: "done: " + last.Content[:min(40, len(last.Content))]}
			case has("agent__helper-k"):
				return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "agent__helper-k", Args: `{"input":"find returns"}`}}}
			case has("knowledge__search"):
				return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "knowledge__search", Args: `{"query":"returns"}`}}}
			}
			return fakemodel.Turn{Text: "nothing"}
		}
		r := e.must(202, "POST", "/v1/agents/lead-k/runs", "erin", runBody("mc-3", "go"), "Idempotency-Key", "mc3")
		root := r["runId"].(string)
		if own, _ := e.credentialRow(root); !own {
			t.Fatal("the root keeps the credential for a sub-agent that has the tools, though its own release has none")
		}
		if ok, err := e.worker.Once(context.Background()); err != nil || !ok {
			t.Fatal(ok, err)
		}
		child := e.must(200, "GET", "/v1/runs/"+root+"/subruns", "erin", nil)["runs"].([]any)[0].(map[string]any)["runId"].(string)
		if own, ref := e.credentialRow(child); own || ref != root {
			t.Fatalf("the child holds a copy (%v) or points elsewhere (%q)", own, ref)
		}
		// single-flight renewal (D69): four at once, one call to the authorization server, no row lock held across it
		cl, err := e.store.Claim(context.Background(), "w-renew", time.Minute)
		if err != nil || cl == nil {
			t.Fatal(err, cl)
		}
		seen, err := e.store.CMICredential(context.Background(), cl)
		if err != nil || seen == nil {
			t.Fatal(err, seen)
		}
		var renewals atomic.Int32
		var wg sync.WaitGroup
		results := make([]string, 4)
		for i := range results {
			wg.Add(1)
			go func() {
				defer wg.Done()
				tok, err := e.store.RenewCMICredential(context.Background(), cl, seen, func(_ context.Context, refresh string) (*credential.Token, error) {
					renewals.Add(1)
					time.Sleep(200 * time.Millisecond)
					return &credential.Token{AccessToken: "member:erin-renewed", RefreshToken: refresh, ExpiresAt: time.Now().Add(time.Hour)}, nil
				})
				if err == nil {
					results[i] = tok.AccessToken
				}
			}()
		}
		wg.Wait()
		if renewals.Load() != 1 {
			t.Errorf("the refresh token was renewed %d times", renewals.Load())
		}
		for _, got := range results { // the one that renewed has the new token; the others use the one that still worked, or the new one
			if got != "member:erin-renewed" && got != seen.AccessToken {
				t.Errorf("%v", results)
			}
		}
		if cur, err := e.store.CMICredential(context.Background(), cl); err != nil || cur.AccessToken != "member:erin-renewed" {
			t.Errorf("the renewed credential is stored: %v %v", cur, err)
		}
		// a renewal in flight holds no lock: the child's heartbeat and a cancel of the root go through meanwhile, and a credential that
		// was cleared meanwhile is not brought back
		cur, _ := e.store.CMICredential(context.Background(), cl)
		inCall, unblock := make(chan struct{}), make(chan struct{})
		done := make(chan error, 1)
		go func() {
			_, err := e.store.RenewCMICredential(context.Background(), cl, cur, func(_ context.Context, refresh string) (*credential.Token, error) {
				close(inCall)
				<-unblock
				return &credential.Token{AccessToken: "member:erin-late", RefreshToken: refresh, ExpiresAt: time.Now().Add(time.Hour)}, nil
			})
			done <- err
		}()
		<-inCall
		quick := func(what string, f func() error) {
			t.Helper()
			finished := make(chan error, 1)
			go func() { finished <- f() }()
			select {
			case err := <-finished:
				if err != nil {
					t.Errorf("%s: %v", what, err)
				}
			case <-time.After(3 * time.Second):
				t.Errorf("%s was kept waiting by a renewal in flight", what)
			}
		}
		quick("the child's heartbeat", func() error { return e.store.Heartbeat(context.Background(), cl, time.Minute) })
		quick("the cancel of the root", func() error { _, err := e.store.CancelRun(context.Background(), e.tenantA, "erin", root); return err })
		close(unblock)
		if err := <-done; err == nil {
			t.Error("a credential cleared while it was being renewed was restored")
		}
		if own, _ := e.credentialRow(root); own {
			t.Error("the root's credential came back")
		}
	})
}

// D69: an idempotent replay is answered before the gateway's catalogue is read and before a credential is exchanged.
func TestAReplayIsFoundBeforeAnythingIsListedOrExchangedOnPostgres(t *testing.T) {
	e, _ := knowledgeEnv(t, handbookDoc())
	d := definition("edge", []string{"sum"}, nil)
	d.Context = agent.ContextPolicy{CMI: true, Tools: agent.KnowledgeTools}
	e.publish("replayer", d)
	body := runBody("rp-1", "returns policy")
	r := e.must(202, "POST", "/v1/agents/replayer/runs", "erin", body, "Idempotency-Key", "rp1")
	listed, exchanged := len(e.mcp.Bearers()), cmiExchanges.Load()
	again := e.must(200, "POST", "/v1/agents/replayer/runs", "erin", body, "Idempotency-Key", "rp1")
	if again["runId"] != r["runId"] {
		t.Fatal("replay returned another run")
	}
	if len(e.mcp.Bearers()) != listed || cmiExchanges.Load() != exchanged {
		t.Errorf("a replay listed the gateway (%d -> %d) or exchanged a token (%d -> %d)", listed, len(e.mcp.Bearers()), exchanged, cmiExchanges.Load())
	}
	// the same key with another request is a conflict, found just as early
	other := runBody("rp-1", "another question")
	e.must(409, "POST", "/v1/agents/replayer/runs", "erin", other, "Idempotency-Key", "rp1")
	if len(e.mcp.Bearers()) != listed || cmiExchanges.Load() != exchanged {
		t.Error("a conflicting request listed the gateway or exchanged a token")
	}
	e.drive()
}
