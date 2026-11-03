// Package integration runs the HOST in process - API, store, worker and harness - against a real
// PostgreSQL, with a deterministic model and a fake gateway. Set IDP_TEST_PG_OWNER_URL to a
// superuser URL of a scratch cluster (make test-integration).
package integration

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-host/adapters/identity"
	"idp-host/adapters/providers/openai"
	"idp-host/adapters/store/postgres"
	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/api"
	"idp-host/internal/authorization"
	"idp-host/internal/credential"
	"idp-host/internal/harness"
	"idp-host/internal/mcpclient"
	"idp-host/internal/model"
	"idp-host/internal/observability"
	"idp-host/internal/run"
	"idp-host/internal/worker"
	"idp-host/tests/fixtures/fakemcp"
	"idp-host/tests/fixtures/fakemodel"
)

func scratchDB(t *testing.T) (ownerURL, serviceURL string) {
	t.Helper()
	base := os.Getenv("IDP_TEST_PG_OWNER_URL")
	if base == "" {
		t.Skip("IDP_TEST_PG_OWNER_URL not set (make test-integration)")
	}
	ctx := context.Background()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "idp_host_it_" + hex.EncodeToString(b)
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), base)
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			c.Close(context.Background())
		}
	})
	u, _ := url.Parse(base)
	u.Path = "/" + name
	ownerURL = u.String()
	if _, err := postgres.Migrate(ctx, ownerURL); err != nil {
		t.Fatal(err)
	}
	if err := postgres.EnsureLogin(ctx, ownerURL, "idp_host_it_svc", "it-service-password"); err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("idp_host_it_svc", "it-service-password")
	return ownerURL, u.String()
}

type env struct {
	t        *testing.T
	store    *postgres.Store
	api      *httptest.Server
	worker   *worker.Worker
	model    *fakemodel.Server
	mcp      *fakemcp.Server
	mcpDeleg *fakemcp.Server
	tokens   map[string]string
	tenantA  string
	tenantB  string
}

var profile = identity.Profile{Issuer: "idp-host-it", Audience: "idp-host", TenantClaim: "tenant_id", RolesClaim: "roles"}

const devSecret = "integration-test-secret-at-least-32-bytes-long"

func newEnv(t *testing.T) *env {
	t.Helper()
	ownerURL, serviceURL := scratchDB(t)
	ctx := context.Background()
	tenantA, err := postgres.SeedTenant(ctx, ownerURL, "", "acme")
	if err != nil {
		t.Fatal(err)
	}
	tenantB, _ := postgres.SeedTenant(ctx, ownerURL, "", "globex")
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	box, _ := credential.NewBox(key)
	store, err := postgres.Open(ctx, serviceURL, box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := postgres.Open(ctx, ownerURL, box); err == nil {
		t.Fatal("the superuser connection must be refused")
	}

	fm := fakemodel.New()
	ms := httptest.NewServer(fm.Handler())
	t.Cleanup(ms.Close)
	hc, _ := model.NewHTTPClient(10*time.Second, "")
	prov, err := openai.New(openai.Config{Name: "fake", BaseURL: ms.URL + "/v1", HTTP: hc, UsageInStream: true})
	if err != nil {
		t.Fatal(err)
	}
	fx := fakemcp.New()
	fx.RequireBearer = "static-key"
	xs := httptest.NewServer(fx.Handler())
	t.Cleanup(xs.Close)
	fx2 := fakemcp.New()
	xs2 := httptest.NewServer(fx2.Handler())
	t.Cleanup(xs2.Close)

	tokens := map[string]string{}
	for name, tenant := range map[string]string{"alice": tenantA, "bob": tenantA, "carol": tenantA, "dave": tenantB} {
		roles := map[string][]string{"alice": {authorization.RoleAdmin}, "bob": {authorization.RoleApprover, authorization.RoleAdmin}, "carol": {authorization.RoleUser}, "dave": {authorization.RoleUser, authorization.RoleAdmin}}[name]
		tok, err := identity.DevToken([]byte(devSecret), profile, name, tenant, roles, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		tokens[name] = tok
	}
	// a fake authorization server: client credentials for the HOST, token exchange for users
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("client_id") != "idp-host" || r.PostForm.Get("client_secret") != "host-secret" {
			http.Error(w, `{"error":"invalid_client"}`, 401)
			return
		}
		switch r.PostForm.Get("grant_type") {
		case "client_credentials":
			_, _ = w.Write([]byte(`{"access_token":"host-actor","expires_in":300}`))
		case "urn:ietf:params:oauth:grant-type:token-exchange":
			if r.PostForm.Get("actor_token") != "host-actor" || r.PostForm.Get("audience") != "gw" || r.PostForm.Get("subject_token") == "" {
				http.Error(w, `{"error":"invalid_request"}`, 400)
				return
			}
			_, _ = fmt.Fprintf(w, `{"access_token":"delegated:%s","expires_in":300}`, r.PostForm.Get("subject_token"))
		default:
			http.Error(w, `{"error":"unsupported_grant_type"}`, 400)
		}
	}))
	t.Cleanup(idp.Close)
	fx2.RequireBearer = "delegated:" + tokens["carol"]

	verifier, _ := identity.NewDevVerifier(profile, []byte(devSecret))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// metrics and tracing wrap the ResponseWriter: the stream must still flush through them
	a := &api.API{Store: store, Auth: verifier, Log: log, Providers: []string{"fake"}, Version: "it", StreamPoll: 20 * time.Millisecond, Metrics: observability.NewMetrics(),
		Gateways: map[string]api.GatewayAuth{"edge": {}, "delegated": {ActorToken: true,
			Exchanger: &credential.Exchanger{TokenEndpoint: idp.URL, ClientID: "idp-host", ClientSecret: "host-secret", Audience: "gw", HTTP: idp.Client()}}}}
	tp, _, err := observability.Setup("idp-host-it", "it", log)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(observability.HTTPMiddleware(tp, "idp-host/it", a.Handler()))
	t.Cleanup(srv.Close)
	mc := func(u string) *harness.Gateway {
		return &harness.Gateway{Endpoint: u + "/mcp", Client: &mcpclient.Client{HTTP: mcpclient.NewHTTPClient(5*time.Second, nil), Info: mcpclient.ClientInfo{Name: "idp-host", Version: "it"}}}
	}
	edge := mc(xs.URL)
	edge.Name, edge.StaticBearer = "edge", "static-key"
	deleg := mc(xs2.URL)
	deleg.Name = "delegated"
	h := &harness.Harness{Store: store, Providers: model.NewRegistry(prov), Gateways: map[string]*harness.Gateway{"edge": edge, "delegated": deleg}, Log: log, DeltaFlush: time.Millisecond}
	w := &worker.Worker{ID: "it-worker", Store: store, Harness: h, Lease: 30 * time.Second, Log: log}
	return &env{t: t, store: store, api: srv, worker: w, model: fm, mcp: fx, mcpDeleg: fx2, tokens: tokens, tenantA: tenantA, tenantB: tenantB}
}

// drive lets the worker serve every claimable run.
func (e *env) drive() {
	e.t.Helper()
	for i := 0; i < 50; i++ {
		ok, err := e.worker.Once(context.Background())
		if err != nil {
			e.t.Fatal(err)
		}
		if !ok {
			return
		}
	}
	e.t.Fatal("worker never ran out of work")
}

func (e *env) call(method, path, who string, body any, headers ...string) (int, map[string]any) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, e.api.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+e.tokens[who])
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			e.t.Fatalf("%s %s: %d non-JSON body %s", method, path, resp.StatusCode, raw)
		}
	}
	return resp.StatusCode, out
}

func (e *env) must(status int, method, path, who string, body any, headers ...string) map[string]any {
	e.t.Helper()
	st, out := e.call(method, path, who, body, headers...)
	if st != status {
		e.t.Fatalf("%s %s as %s: want %d, got %d %v", method, path, who, status, st, out)
	}
	return out
}

// sse reads an event stream to its end and returns the event types and payloads.
func (e *env) sse(method, path, who string, body any) ([]string, []map[string]any, http.Header) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, e.api.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+e.tokens[who])
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		e.t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, raw)
	}
	var types []string
	var events []map[string]any
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev); err != nil {
			e.t.Fatalf("bad event line %q", line)
		}
		types = append(types, ev["type"].(string))
		events = append(events, ev)
	}
	return types, events, resp.Header
}

func definition(gateway string, allow []string, confirm map[string]string) agent.Definition {
	return agent.Definition{Instructions: "You are a test agent.", Model: agent.ModelProfile{Provider: "fake", Model: "fake-1"},
		Tools: agent.ToolPolicy{Gateway: gateway, Allow: allow}, Confirmation: agent.ConfirmationPolicy{Default: agent.ConfirmAuto, Tools: confirm}}
}

func (e *env) publish(name string, def agent.Definition) {
	e.t.Helper()
	e.must(201, "POST", "/api/v1/agents", "alice", map[string]any{"name": name, "displayName": name, "definition": def})
	rel := e.must(201, "POST", "/api/v1/agents/"+name+"/releases", "alice", nil)
	e.must(403, "POST", "/api/v1/releases/"+rel["releaseId"].(string)+"/approve", "alice", nil) // four eyes
	e.must(200, "POST", "/api/v1/releases/"+rel["releaseId"].(string)+"/approve", "bob", nil)
}

func runBody(thread, text string) map[string]any {
	id := "u-" + hex.EncodeToString([]byte(text))
	if len(id) > 100 {
		id = id[:100]
	}
	return map[string]any{"threadId": thread, "messages": []map[string]any{{"id": id, "role": "user", "content": text}}, "tools": []any{}, "context": []any{}}
}

func TestHostEndToEnd(t *testing.T) {
	e := newEnv(t)

	t.Run("health-without-token", func(t *testing.T) {
		for _, p := range []string{"/healthz", "/readyz", "/version"} {
			resp, err := http.Get(e.api.URL + p)
			if err != nil || resp.StatusCode != 200 {
				t.Fatalf("%s: %v %v", p, err, resp)
			}
			resp.Body.Close()
		}
	})

	t.Run("publish", func(t *testing.T) {
		bad := definition("edge", []string{"bad name"}, nil)
		e.must(422, "POST", "/api/v1/agents", "alice", map[string]any{"name": "broken", "displayName": "x", "definition": bad})
		e.must(403, "POST", "/api/v1/agents", "carol", map[string]any{"name": "nope", "displayName": "x", "definition": definition("", nil, nil)})
		e.publish("assistant", definition("edge", []string{"sum", "echo"}, map[string]string{"echo": agent.ConfirmConfirm}))
		ag := e.must(200, "GET", "/api/v1/agents/assistant", "alice", nil)
		if ag["currentReleaseId"] == nil {
			t.Fatalf("%v", ag)
		}
		rels := e.must(200, "GET", "/api/v1/agents/assistant/releases", "alice", nil)["releases"].([]any)
		if len(rels) != 1 || rels[0].(map[string]any)["status"] != "APPROVED" || rels[0].(map[string]any)["digest"] == nil {
			t.Fatalf("%v", rels)
		}
		env := e.must(200, "GET", "/api/v1/releases/"+rels[0].(map[string]any)["releaseId"].(string)+"/envelope", "alice", nil)
		if env["digest"] == nil || env["document"] == nil {
			t.Fatalf("%v", env)
		}
		if n := len(e.must(200, "GET", "/api/v1/audit", "alice", nil)["events"].([]any)); n < 3 {
			t.Fatalf("audit events %d", n)
		}
		// an unpublished agent cannot run
		e.must(201, "POST", "/api/v1/agents", "alice", map[string]any{"name": "draft", "displayName": "d", "definition": definition("", nil, nil)})
		e.must(409, "POST", "/v1/agents/draft/runs", "carol", runBody("thr-draft", "hi"), "Idempotency-Key", "d1")
	})

	t.Run("plain-run-and-idempotency", func(t *testing.T) {
		r := e.must(202, "POST", "/v1/agents/assistant/runs", "carol", runBody("thr-1", "hello"), "Idempotency-Key", "k1")
		if r["status"] != run.StatusQueued || r["threadId"] != "thr-1" {
			t.Fatalf("%v", r)
		}
		id := r["runId"].(string)
		again := e.must(200, "POST", "/v1/agents/assistant/runs", "carol", runBody("thr-1", "hello"), "Idempotency-Key", "k1")
		if again["runId"] != id {
			t.Fatal("replay returned another run")
		}
		e.must(409, "POST", "/v1/agents/assistant/runs", "carol", runBody("thr-1", "different"), "Idempotency-Key", "k1")
		e.must(409, "POST", "/v1/agents/assistant/runs", "carol", runBody("thr-1", "second turn"), "Idempotency-Key", "k2") // one turn at a time
		e.must(404, "GET", "/v1/runs/"+id, "dave", nil)                                                                     // other tenant
		e.drive()
		got := e.must(200, "GET", "/v1/runs/"+id, "carol", nil)
		if got["status"] != run.StatusCompleted || got["stepCount"] != 1.0 {
			t.Fatalf("%v", got)
		}
		msgs := e.must(200, "GET", "/v1/threads/thr-1/messages", "carol", nil)["messages"].([]any)
		if len(msgs) != 2 || msgs[1].(map[string]any)["content"] != "Echo: hello" {
			t.Fatalf("%v", msgs)
		}
		types, events, _ := e.sse("GET", "/v1/runs/"+id+"/events", "carol", nil)
		if types[0] != agui.EventRunStarted || types[len(types)-1] != agui.EventRunFinished || !strings.Contains(strings.Join(types, ","), "TEXT_MESSAGE_CONTENT") {
			t.Fatalf("%v", types)
		}
		if events[len(events)-1]["outcome"].(map[string]any)["type"] != agui.OutcomeSuccess {
			t.Fatalf("%v", events[len(events)-1])
		}
		page := e.must(200, "GET", "/v1/runs/"+id+"/events?after=0&limit=2", "carol", nil)["events"].([]any)
		if len(page) != 2 {
			t.Fatalf("%v", page)
		}
		// the next turn on the thread now works
		r2 := e.must(202, "POST", "/v1/agents/assistant/runs", "carol", runBody("thr-1", "please sum"), "Idempotency-Key", "k2")
		e.drive()
		got = e.must(200, "GET", "/v1/runs/"+r2["runId"].(string), "carol", nil)
		if got["status"] != run.StatusCompleted || got["stepCount"] != 2.0 {
			t.Fatalf("%v", got)
		}
		calls := e.mcp.Calls()
		if len(calls) != 1 || calls[0].Name != "sum" || calls[0].Bearer != "static-key" {
			t.Fatalf("%+v", calls)
		}
		msgs = e.must(200, "GET", "/v1/threads/thr-1/messages", "carol", nil)["messages"].([]any)
		if last := msgs[len(msgs)-1].(map[string]any); len(msgs) != 6 || last["content"] != "Result: 3" {
			t.Fatalf("%d %v", len(msgs), last)
		}
	})

	t.Run("confirmation", func(t *testing.T) {
		r := e.must(202, "POST", "/v1/agents/assistant/runs", "carol", runBody("thr-2", "echo this"), "Idempotency-Key", "c1")
		id := r["runId"].(string)
		e.drive()
		if got := e.must(200, "GET", "/v1/runs/"+id, "carol", nil); got["status"] != run.StatusAwaitingInput {
			t.Fatalf("%v", got)
		}
		calls := e.must(200, "GET", "/v1/runs/"+id+"/tool-calls", "carol", nil)["toolCalls"].([]any)
		if len(calls) != 1 || calls[0].(map[string]any)["state"] != run.CallPendingConfirmation || calls[0].(map[string]any)["tool"] != "echo" {
			t.Fatalf("%v", calls)
		}
		callID := calls[0].(map[string]any)["toolCallId"].(string)
		types, events, _ := e.sse("GET", "/v1/runs/"+id+"/events", "carol", nil)
		if types[len(types)-1] != agui.EventRunFinished || events[len(events)-1]["outcome"].(map[string]any)["type"] != agui.OutcomeInterrupt {
			t.Fatalf("%v %v", types, events[len(events)-1])
		}
		e.must(404, "POST", "/v1/runs/"+id+"/tool-calls/"+callID+"/decide", "dave", map[string]any{"decision": "confirm"})
		e.must(422, "POST", "/v1/runs/"+id+"/tool-calls/"+callID+"/decide", "carol", map[string]any{"decision": "maybe"})
		dec := e.must(200, "POST", "/v1/runs/"+id+"/tool-calls/"+callID+"/decide", "carol", map[string]any{"decision": "confirm"})
		if dec["state"] != run.CallConfirmed {
			t.Fatalf("%v", dec)
		}
		e.must(409, "POST", "/v1/runs/"+id+"/tool-calls/"+callID+"/decide", "carol", map[string]any{"decision": "confirm"})
		if got := e.must(200, "GET", "/v1/runs/"+id, "carol", nil); got["status"] != run.StatusQueued {
			t.Fatalf("%v", got)
		}
		e.drive()
		got := e.must(200, "GET", "/v1/runs/"+id, "carol", nil)
		msgs := e.must(200, "GET", "/v1/threads/thr-2/messages", "carol", nil)["messages"].([]any)
		if got["status"] != run.StatusCompleted || msgs[len(msgs)-1].(map[string]any)["content"] != "Result: hi" {
			t.Fatalf("%v %v", got, msgs)
		}
		if calls := e.mcp.Calls(); calls[len(calls)-1].Name != "echo" {
			t.Fatalf("%+v", calls)
		}
		// a client that resumes from the interrupt's event id sees the decision, a new RUN_STARTED and the success
		var interruptID int64
		for _, ev := range e.must(200, "GET", "/v1/runs/"+id+"/events", "carol", nil)["events"].([]any) {
			if m := ev.(map[string]any); m["type"] == agui.EventRunFinished && interruptID == 0 {
				interruptID = int64(m["id"].(float64))
			}
		}
		types, events, _ = e.sse("GET", fmt.Sprintf("/v1/runs/%s/events?after=%d", id, interruptID), "carol", nil)
		// the decision, the resume, the confirmed call's result, then the next model step
		if joined := strings.Join(types, ","); !strings.HasPrefix(joined, "CUSTOM,RUN_STARTED,TOOL_CALL_RESULT,STEP_STARTED") || types[len(types)-1] != agui.EventRunFinished ||
			events[len(events)-1]["outcome"].(map[string]any)["type"] != agui.OutcomeSuccess {
			t.Fatalf("%v", types)
		}

		// reject
		r = e.must(202, "POST", "/v1/agents/assistant/runs", "carol", runBody("thr-3", "echo that"), "Idempotency-Key", "c2")
		e.drive()
		calls = e.must(200, "GET", "/v1/runs/"+r["runId"].(string)+"/tool-calls", "carol", nil)["toolCalls"].([]any)
		e.must(200, "POST", "/v1/runs/"+r["runId"].(string)+"/tool-calls/"+calls[0].(map[string]any)["toolCallId"].(string)+"/decide", "carol", map[string]any{"decision": "reject"})
		e.drive()
		msgs = e.must(200, "GET", "/v1/threads/thr-3/messages", "carol", nil)["messages"].([]any)
		if !strings.Contains(msgs[len(msgs)-1].(map[string]any)["content"].(string), "rejected") {
			t.Fatalf("%v", msgs)
		}

		// expiry
		r = e.must(202, "POST", "/v1/agents/assistant/runs", "carol", runBody("thr-4", "echo more"), "Idempotency-Key", "c3")
		e.drive()
		e.store.Now = func() time.Time { return time.Now().Add(time.Hour) }
		n, err := e.store.ExpireConfirmations(context.Background())
		e.store.Now = time.Now
		if err != nil || n != 1 {
			t.Fatal(err, n)
		}
		e.drive()
		got = e.must(200, "GET", "/v1/runs/"+r["runId"].(string), "carol", nil)
		msgs = e.must(200, "GET", "/v1/threads/thr-4/messages", "carol", nil)["messages"].([]any)
		if got["status"] != run.StatusCompleted || !strings.Contains(msgs[len(msgs)-1].(map[string]any)["content"].(string), "expired") {
			t.Fatalf("%v %v", got, msgs)
		}
	})

	t.Run("cancel-queued", func(t *testing.T) {
		r := e.must(202, "POST", "/v1/agents/assistant/runs", "carol", runBody("thr-5", "hello"), "Idempotency-Key", "x1")
		id := r["runId"].(string)
		if got := e.must(200, "POST", "/v1/runs/"+id+"/cancel", "carol", nil); got["status"] != run.StatusCanceled {
			t.Fatalf("%v", got)
		}
		types, _, _ := e.sse("GET", "/v1/runs/"+id+"/events", "carol", nil)
		if types[len(types)-1] != agui.EventRunError {
			t.Fatalf("%v", types)
		}
		e.drive() // nothing to do
		e.must(202, "POST", "/v1/agents/assistant/runs", "carol", runBody("thr-5", "hello again"), "Idempotency-Key", "x2")
		e.drive()
	})

	t.Run("lease-expiry-and-fencing", func(t *testing.T) {
		e.must(202, "POST", "/v1/agents/assistant/runs", "carol", runBody("thr-6", "hello"), "Idempotency-Key", "f1")
		ctx := context.Background()
		c1, err := e.store.Claim(ctx, "w1", 300*time.Millisecond)
		if err != nil || c1 == nil {
			t.Fatal(err, c1)
		}
		if c, _ := e.store.Claim(ctx, "w2", time.Minute); c != nil {
			t.Fatal("a leased run was claimed twice")
		}
		time.Sleep(400 * time.Millisecond)
		c2, err := e.store.Claim(ctx, "w2", time.Minute)
		if err != nil || c2 == nil || c2.RunID != c1.RunID || c2.Epoch != c1.Epoch+1 {
			t.Fatalf("%v %+v %+v", err, c1, c2)
		}
		if err := e.store.AppendEvents(ctx, c1, []agui.Event{agui.Custom("x", nil)}); !errors.Is(err, run.ErrFenced) {
			t.Fatalf("stale worker wrote: %v", err)
		}
		if err := e.store.Heartbeat(ctx, c1, time.Minute); !errors.Is(err, run.ErrFenced) {
			t.Fatalf("%v", err)
		}
		if err := e.store.Heartbeat(ctx, c2, time.Minute); err != nil {
			t.Fatal(err)
		}
		if err := e.worker.Harness.Run(ctx, c2); err != nil {
			t.Fatal(err)
		}
		if got := e.must(200, "GET", "/v1/threads/thr-6/runs", "carol", nil)["runs"].([]any); got[0].(map[string]any)["status"] != run.StatusCompleted {
			t.Fatalf("%v", got)
		}
	})

	t.Run("concurrent-identical-requests", func(t *testing.T) {
		var wg sync.WaitGroup
		ids := make(chan string, 20)
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				st, out := e.call("POST", "/v1/agents/assistant/runs", "carol", runBody("thr-7", "race"), "Idempotency-Key", "race")
				if st == 202 || st == 200 {
					ids <- out["runId"].(string)
				} else {
					ids <- fmt.Sprintf("status-%d", st)
				}
			}()
		}
		wg.Wait()
		close(ids)
		distinct := map[string]bool{}
		for id := range ids {
			distinct[id] = true
		}
		if len(distinct) != 1 {
			t.Fatalf("%v", distinct)
		}
		e.drive()
	})

	t.Run("delegated-gateway", func(t *testing.T) {
		e.publish("delegated-agent", definition("delegated", []string{"sum"}, nil))
		r := e.must(202, "POST", "/v1/agents/delegated-agent/runs", "carol", runBody("thr-8", "please sum"), "Idempotency-Key", "d1")
		e.drive()
		if got := e.must(200, "GET", "/v1/runs/"+r["runId"].(string), "carol", nil); got["status"] != run.StatusCompleted {
			t.Fatalf("%v", got)
		}
		calls := e.mcpDeleg.Calls()
		if len(calls) != 1 || calls[0].Bearer != "delegated:"+e.tokens["carol"] {
			t.Fatalf("the gateway did not receive the delegated token: %+v", calls)
		}
	})

	t.Run("ag-ui", func(t *testing.T) {
		go func() {
			time.Sleep(300 * time.Millisecond)
			_, _ = e.worker.Once(context.Background())
		}()
		in := map[string]any{"threadId": "agui-1", "runId": "agui-run-1", "messages": []map[string]any{{"id": "m1", "role": "user", "content": "please sum"}}, "tools": []any{}, "context": []any{}, "forwardedProps": map[string]any{}}
		types, events, hdr := e.sse("POST", "/ag-ui/agents/assistant", "carol", in)
		if hdr.Get("X-Run-Id") != "agui-run-1" || types[0] != agui.EventRunStarted || types[len(types)-1] != agui.EventRunFinished {
			t.Fatalf("%s %v", hdr.Get("X-Run-Id"), types)
		}
		if events[0]["runId"] != "agui-run-1" || events[0]["threadId"] != "agui-1" {
			t.Fatalf("%v", events[0])
		}
		joined := strings.Join(types, ",")
		for _, want := range []string{"TOOL_CALL_START,TOOL_CALL_ARGS,TOOL_CALL_END", "TOOL_CALL_RESULT", "STEP_FINISHED"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("missing %s in %s", want, joined)
			}
		}
		// a frontend tool ends the run with the call for the client to execute
		go func() {
			time.Sleep(300 * time.Millisecond)
			_, _ = e.worker.Once(context.Background())
		}()
		e.model.Script = func(msgs []fakemodel.Message, tools []string) fakemodel.Turn {
			if msgs[len(msgs)-1].Role == "tool" {
				return fakemodel.Turn{Text: "Thanks: " + msgs[len(msgs)-1].Content}
			}
			return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "ui_pick", Args: `{"choice":1}`}}}
		}
		in = map[string]any{"threadId": "agui-2", "runId": "agui-run-2", "messages": []map[string]any{{"id": "m1", "role": "user", "content": "pick"}},
			"tools": []map[string]any{{"name": "ui_pick", "description": "frontend", "parameters": map[string]any{"type": "object"}}}, "context": []any{}}
		types, events, _ = e.sse("POST", "/ag-ui/agents/assistant", "carol", in)
		if types[len(types)-1] != agui.EventRunFinished || !strings.Contains(strings.Join(types, ","), "TOOL_CALL_START") || len(e.mcp.Calls()) != 3 {
			t.Fatalf("%v %d", types, len(e.mcp.Calls()))
		}
		// the client executed it and sends the whole conversation back with the tool result; the
		// assistant message it echoes carries the id the HOST streamed (parentMessageId)
		var callID, parentID string
		for _, ev := range events {
			if ev["type"] == agui.EventToolCallStart {
				callID, parentID = ev["toolCallId"].(string), ev["parentMessageId"].(string)
			}
		}
		go func() {
			time.Sleep(300 * time.Millisecond)
			_, _ = e.worker.Once(context.Background())
		}()
		in = map[string]any{"threadId": "agui-2", "runId": "agui-run-3", "messages": []map[string]any{
			{"id": "m1", "role": "user", "content": "pick"},
			{"id": parentID, "role": "assistant", "toolCalls": []map[string]any{{"id": callID, "type": "function", "function": map[string]any{"name": "ui_pick", "arguments": `{"choice":1}`}}}},
			{"id": "t1", "role": "tool", "toolCallId": callID, "content": "picked 1"}},
			"tools": []map[string]any{{"name": "ui_pick", "description": "frontend", "parameters": map[string]any{"type": "object"}}}, "context": []any{}}
		types, _, _ = e.sse("POST", "/ag-ui/agents/assistant", "carol", in)
		msgs := e.must(200, "GET", "/v1/threads/agui-2/messages", "carol", nil)["messages"].([]any)
		if types[len(types)-1] != agui.EventRunFinished || msgs[len(msgs)-1].(map[string]any)["content"] != "Thanks: picked 1" || len(msgs) != 4 {
			t.Fatalf("%v %v", types, msgs)
		}
		// a client that re-labels the HOST's assistant message is still not duplicated: the tool call id is known
		e.model.Script = fakemodel.DefaultScript
		go func() {
			time.Sleep(300 * time.Millisecond)
			_, _ = e.worker.Once(context.Background())
		}()
		in = map[string]any{"threadId": "agui-2", "runId": "agui-run-4", "messages": []map[string]any{
			{"id": "m1", "role": "user", "content": "pick"},
			{"id": "relabeled", "role": "assistant", "toolCalls": []map[string]any{{"id": callID, "type": "function", "function": map[string]any{"name": "ui_pick", "arguments": `{"choice":1}`}}}},
			{"id": "t1-again", "role": "tool", "toolCallId": callID, "content": "picked 1"},
			{"id": "m2", "role": "user", "content": "thanks"}}, "tools": []any{}, "context": []any{}}
		types, _, _ = e.sse("POST", "/ag-ui/agents/assistant", "carol", in)
		msgs = e.must(200, "GET", "/v1/threads/agui-2/messages", "carol", nil)["messages"].([]any)
		if types[len(types)-1] != agui.EventRunFinished || len(msgs) != 6 || msgs[4].(map[string]any)["content"] != "thanks" || msgs[5].(map[string]any)["content"] != "Echo: thanks" {
			t.Fatalf("%v %d %v", types, len(msgs), msgs)
		}
		e.model.Script = fakemodel.DefaultScript
	})

	t.Run("governance", func(t *testing.T) {
		pol := e.must(200, "GET", "/api/v1/policy", "alice", nil)
		if pol["default"] != true || pol["maxConcurrentRuns"] != 10.0 {
			t.Fatalf("%v", pol)
		}
		e.must(403, "PUT", "/api/v1/policy", "carol", map[string]any{"maxConcurrentRuns": 1})
		e.must(422, "PUT", "/api/v1/policy", "alice", map[string]any{"maxConcurrentRuns": -1})
		// concurrency: one run in flight blocks a second one on another thread
		e.must(200, "PUT", "/api/v1/policy", "alice", map[string]any{"maxConcurrentRuns": 1})
		first := e.must(202, "POST", "/v1/agents/assistant/runs", "carol", runBody("gov-1", "hello"), "Idempotency-Key", "g1")
		st, out := e.call("POST", "/v1/agents/assistant/runs", "carol", runBody("gov-2", "hello"), "Idempotency-Key", "g2")
		if st != 429 || out["error"].(map[string]any)["code"] != "QUOTA_EXCEEDED" {
			t.Fatalf("%d %v", st, out)
		}
		e.drive()
		if got := e.must(200, "GET", "/v1/runs/"+first["runId"].(string), "carol", nil); got["status"] != run.StatusCompleted {
			t.Fatalf("%v", got)
		}
		e.must(202, "POST", "/v1/agents/assistant/runs", "carol", runBody("gov-2", "hello"), "Idempotency-Key", "g2")
		e.drive()
		usage := e.must(200, "GET", "/api/v1/usage", "alice", nil)
		if usage["activeRuns"] != 0.0 || usage["tokensLast24h"].(float64) <= 0 || usage["runsLast24h"].(float64) < 2 {
			t.Fatalf("%v", usage)
		}
		// daily budget exhausted: new runs are refused
		e.must(200, "PUT", "/api/v1/policy", "alice", map[string]any{"maxConcurrentRuns": 0, "tokensPerDay": 1})
		st, out = e.call("POST", "/v1/agents/assistant/runs", "carol", runBody("gov-3", "hello"), "Idempotency-Key", "g3")
		if st != 429 || !strings.Contains(fmt.Sprint(out), "budget") {
			t.Fatalf("%d %v", st, out)
		}
		// per-run cap: the run fails after its first step consumed the cap
		e.must(200, "PUT", "/api/v1/policy", "alice", map[string]any{"maxConcurrentRuns": 0, "tokensPerDay": 0, "maxTokensPerRun": 5})
		capped := e.must(202, "POST", "/v1/agents/assistant/runs", "carol", runBody("gov-4", "please sum"), "Idempotency-Key", "g4")
		e.drive()
		got := e.must(200, "GET", "/v1/runs/"+capped["runId"].(string), "carol", nil)
		if got["status"] != run.StatusFailed || got["failureCode"] != run.FailBudget || got["stepCount"] != 1.0 {
			t.Fatalf("%v", got)
		}
		// the release's own cap overrides the tenant default
		e.must(200, "PUT", "/api/v1/policy", "alice", map[string]any{"maxConcurrentRuns": 0, "tokensPerDay": 0, "maxTokensPerRun": 5})
		def := definition("edge", []string{"sum", "echo"}, nil)
		def.Limits.MaxTokensPerRun = 100000
		e.publish("roomy", def)
		roomy := e.must(202, "POST", "/v1/agents/roomy/runs", "carol", runBody("gov-5", "please sum"), "Idempotency-Key", "g5")
		e.drive()
		if got := e.must(200, "GET", "/v1/runs/"+roomy["runId"].(string), "carol", nil); got["status"] != run.StatusCompleted {
			t.Fatalf("%v", got)
		}
		e.must(200, "PUT", "/api/v1/policy", "alice", map[string]any{"maxConcurrentRuns": 0, "tokensPerDay": 0, "maxTokensPerRun": 0})
		// the policy is per tenant: the other tenant still has the defaults
		if pol := e.must(200, "GET", "/api/v1/policy", "dave", nil); pol["default"] != true {
			t.Fatalf("%v", pol)
		}
	})

	t.Run("skills", func(t *testing.T) {
		// a skill that brings the echo tool and demands confirmation for it
		e.must(201, "POST", "/api/v1/skills", "alice", map[string]any{"name": "echoing", "displayName": "Echoing", "definition": map[string]any{
			"instructions": "When asked to echo, use the echo tool.", "tools": []string{"echo"}, "confirmation": map[string]string{"echo": "confirm"}}})
		e.must(422, "POST", "/api/v1/skills", "alice", map[string]any{"name": "bad", "displayName": "x", "definition": map[string]any{"instructions": "", "tools": []string{}}})
		// an agent referencing an unpublished skill cannot be compiled
		agentDef := definition("edge", []string{"sum"}, nil)
		agentDef.Skills = []agent.SkillRef{{Name: "echoing"}}
		e.must(201, "POST", "/api/v1/agents", "alice", map[string]any{"name": "skilled", "displayName": "Skilled", "definition": agentDef})
		e.must(409, "POST", "/api/v1/agents/skilled/releases", "alice", nil)
		rel := e.must(201, "POST", "/api/v1/skills/echoing/releases", "alice", nil)
		e.must(403, "POST", "/api/v1/skill-releases/"+rel["releaseId"].(string)+"/approve", "alice", nil)
		sk := e.must(200, "POST", "/api/v1/skill-releases/"+rel["releaseId"].(string)+"/approve", "bob", nil)
		// the agent release embeds the skill with its digest
		ar := e.must(201, "POST", "/api/v1/agents/skilled/releases", "alice", nil)
		e.must(200, "POST", "/api/v1/releases/"+ar["releaseId"].(string)+"/approve", "bob", nil)
		env := e.must(200, "GET", "/api/v1/releases/"+ar["releaseId"].(string)+"/envelope", "alice", nil)
		doc := env["document"].(map[string]any)
		embedded := doc["skills"].([]any)
		if len(embedded) != 1 || embedded[0].(map[string]any)["digest"] != sk["digest"] || embedded[0].(map[string]any)["version"] != 1.0 {
			t.Fatalf("%v", doc["skills"])
		}
		// the run sees the skill's tool and its confirmation rule
		r := e.must(202, "POST", "/v1/agents/skilled/runs", "carol", runBody("skill-1", "echo this"), "Idempotency-Key", "s1")
		e.drive()
		if got := e.must(200, "GET", "/v1/runs/"+r["runId"].(string), "carol", nil); got["status"] != run.StatusAwaitingInput {
			t.Fatalf("%v", got)
		}
		calls := e.must(200, "GET", "/v1/runs/"+r["runId"].(string)+"/tool-calls", "carol", nil)["toolCalls"].([]any)
		e.must(200, "POST", "/v1/runs/"+r["runId"].(string)+"/tool-calls/"+calls[0].(map[string]any)["toolCallId"].(string)+"/decide", "carol", map[string]any{"decision": "confirm"})
		e.drive()
		msgs := e.must(200, "GET", "/v1/threads/skill-1/messages", "carol", nil)["messages"].([]any)
		if msgs[len(msgs)-1].(map[string]any)["content"] != "Result: hi" {
			t.Fatalf("%v", msgs)
		}
		if last := e.model.Last(); !strings.Contains(last[0].Content, "## Skill: echoing") {
			t.Fatalf("skill instructions not merged: %q", last[0].Content)
		}
		// a new skill version does not change the compiled release; a pinned reference stays on its version
		e.must(200, "PATCH", "/api/v1/skills/echoing", "alice", map[string]any{"definition": map[string]any{"instructions": "v2", "tools": []string{"echo"}}})
		rel2 := e.must(201, "POST", "/api/v1/skills/echoing/releases", "alice", nil)
		e.must(200, "POST", "/api/v1/skill-releases/"+rel2["releaseId"].(string)+"/approve", "bob", nil)
		pinned := definition("edge", []string{"sum"}, nil)
		pinned.Skills = []agent.SkillRef{{Name: "echoing", Version: 1}}
		e.must(201, "POST", "/api/v1/agents", "alice", map[string]any{"name": "pinned", "displayName": "Pinned", "definition": pinned})
		pr := e.must(201, "POST", "/api/v1/agents/pinned/releases", "alice", nil)
		if v := pr["document"].(map[string]any)["skills"].([]any)[0].(map[string]any)["version"]; v != 1.0 {
			t.Fatalf("pinned version %v", v)
		}
		cur := e.must(201, "POST", "/api/v1/agents/skilled/releases", "alice", nil)
		if v := cur["document"].(map[string]any)["skills"].([]any)[0].(map[string]any)["version"]; v != 2.0 {
			t.Fatalf("current version %v", v)
		}
		if n := len(e.must(200, "GET", "/api/v1/skills", "alice", nil)["skills"].([]any)); n != 1 {
			t.Fatalf("skills %d", n)
		}
		if n := len(e.must(200, "GET", "/api/v1/skills", "dave", nil)["skills"].([]any)); n != 0 {
			t.Fatalf("skills visible across tenants: %d", n)
		}
	})

	t.Run("tenant-isolation", func(t *testing.T) {
		if ags := e.must(200, "GET", "/api/v1/agents", "dave", nil)["agents"].([]any); len(ags) != 0 {
			t.Fatalf("%v", ags)
		}
		e.must(404, "GET", "/v1/threads/thr-1", "dave", nil)
		e.must(404, "GET", "/v1/threads/thr-1/messages", "dave", nil)
		e.must(404, "POST", "/v1/runs/nope/cancel", "carol", nil)
		// an administrator of the tenant sees a user's run; a plain user does not see another user's
		st, _ := e.call("GET", "/v1/threads/thr-1", "alice", nil)
		if st != 200 {
			t.Fatalf("admin read: %d", st)
		}
	})
}
