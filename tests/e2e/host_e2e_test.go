// Package e2e drives the compose topology from outside (deploy/e2e.sh): the HOST image, the real
// idp-mcp registry and gateway, real MCP SDK upstreams and a model endpoint. Nothing is mocked
// inside the containers.
package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"idp-host/adapters/identity"
	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/authorization"
	"idp-host/internal/run"
)

type env struct {
	t                       *testing.T
	host, registry, metrics string
	hostTenant, hostSecret  string
	mcpTenant, mcpSecret    string
	modelName               string
	live                    bool
	http                    *http.Client
}

func setup(t *testing.T) *env {
	e := &env{t: t, host: os.Getenv("IDP_E2E_HOST"), registry: os.Getenv("IDP_E2E_REGISTRY"), metrics: os.Getenv("IDP_E2E_METRICS"),
		hostTenant: os.Getenv("IDP_E2E_HOST_TENANT"), hostSecret: os.Getenv("IDP_E2E_HOST_SECRET"), mcpTenant: os.Getenv("IDP_E2E_MCP_TENANT"),
		mcpSecret: os.Getenv("IDP_E2E_MCP_SECRET"), modelName: os.Getenv("IDP_E2E_MODEL_NAME"), live: os.Getenv("IDP_E2E_MODEL") == "live",
		http: &http.Client{Timeout: 5 * time.Minute, Transport: &http.Transport{Proxy: nil}}}
	if e.host == "" || e.hostTenant == "" || e.hostSecret == "" {
		t.Skip("IDP_E2E_HOST not set (deploy/e2e.sh)")
	}
	return e
}

var hostProfile = identity.Profile{Issuer: "idp-host-dev", Audience: "idp-host", TenantClaim: "tenant_id", RolesClaim: "roles"}
var mcpProfile = identity.Profile{Issuer: "idp-mcp-dev", Audience: "idp-mcp-registry", TenantClaim: "tenant_id", RolesClaim: "roles"}

func (e *env) token(sub string, roles ...string) string {
	tok, err := identity.DevToken([]byte(e.hostSecret), hostProfile, sub, e.hostTenant, roles, time.Hour)
	if err != nil {
		e.t.Fatal(err)
	}
	return tok
}

func (e *env) mcpToken(sub string, roles ...string) string {
	tok, _ := identity.DevToken([]byte(e.mcpSecret), mcpProfile, sub, e.mcpTenant, roles, time.Hour)
	return tok
}

func (e *env) do(base, tok, method, path string, body any, headers ...string) (int, map[string]any, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, base+path, rd)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := e.http.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, raw
}

func (e *env) api(tok, method, path string, body any, want int, headers ...string) map[string]any {
	e.t.Helper()
	st, out, raw := e.do(e.host, tok, method, path, body, headers...)
	if st != want {
		e.t.Fatalf("%s %s: want %d, got %d %s", method, path, want, st, raw)
	}
	return out
}

func (e *env) sse(tok, path string, body any) ([]string, []map[string]any, http.Header) {
	e.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", e.host+path, strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := e.http.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		e.t.Fatalf("POST %s: %d %s", path, resp.StatusCode, raw)
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
		_ = json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev)
		types = append(types, fmt.Sprint(ev["type"]))
		events = append(events, ev)
	}
	return types, events, resp.Header
}

func (e *env) waitRun(tok, id string, want ...string) map[string]any {
	e.t.Helper()
	var last map[string]any
	for i := 0; i < 300; i++ {
		last = e.api(tok, "GET", "/v1/runs/"+id, nil, 200)
		for _, w := range want {
			if last["status"] == w {
				return last
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	e.t.Fatalf("run %s never reached %v: %v", id, want, last)
	return nil
}

func TestComposeHost(t *testing.T) {
	e := setup(t)
	adm, apr, usr := e.token("alice", authorization.RoleAdmin), e.token("bob", authorization.RoleApprover), e.token("carol", authorization.RoleUser)
	var results []string
	check := func(id string, ok bool, detail ...any) {
		results = append(results, id)
		if !ok {
			t.Errorf("FAIL %s %v", id, detail)
		}
	}

	st, _, _ := e.do(e.host, "", "GET", "/healthz", nil)
	st2, _, _ := e.do(e.host, "", "GET", "/readyz", nil)
	check("health", st == 200 && st2 == 200, st, st2)
	st, _, _ = e.do(e.host, "", "GET", "/v1/sessions", nil)
	st2, _, _ = e.do(e.host, "nope", "GET", "/v1/sessions", nil)
	check("unauthenticated-denied", st == 401 && st2 == 401, st, st2)
	check("providers-listed", fmt.Sprint(e.api(adm, "GET", "/api/v1/providers", nil, 200)["providers"]) == "[local]")

	def := agent.Definition{Instructions: "You are the IDP e2e agent. Use tools when asked.", Model: agent.ModelProfile{Provider: "local", Model: e.modelName, MaxOutputTokens: 400},
		Tools:        agent.ToolPolicy{Gateway: "edge", Allow: []string{"minimal__sum", "minimal__echo"}},
		Confirmation: agent.ConfirmationPolicy{Default: agent.ConfirmAuto, Tools: map[string]string{"minimal__echo": agent.ConfirmConfirm}}}
	e.api(adm, "POST", "/api/v1/agents", map[string]any{"name": "assistant", "displayName": "E2E assistant", "definition": def}, 201)
	rel := e.api(adm, "POST", "/api/v1/agents/assistant/releases", nil, 201)
	e.api(adm, "POST", "/api/v1/releases/"+rel["releaseId"].(string)+"/approve", nil, 403)
	approved := e.api(apr, "POST", "/api/v1/releases/"+rel["releaseId"].(string)+"/approve", nil, 200)
	check("release-published-four-eyes", approved["status"] == "APPROVED" && approved["digest"] != nil, approved)

	// AG-UI: a run that needs the real gateway's sum tool
	prompt := "please sum"
	if e.live {
		prompt = "Please use the tool to sum 1 and 2."
	}
	in := map[string]any{"threadId": "e2e-1", "runId": "e2e-run-1", "messages": []map[string]any{{"id": "m1", "role": "user", "content": prompt}}, "tools": []any{}, "context": []any{}}
	types, events, hdr := e.sse(usr, "/ag-ui/agents/assistant", in)
	joined := strings.Join(types, ",")
	var toolName, toolResult string
	for _, ev := range events {
		switch ev["type"] {
		case agui.EventToolCallStart:
			toolName = fmt.Sprint(ev["toolCallName"])
		case agui.EventToolCallResult:
			toolResult = fmt.Sprint(ev["content"])
		}
	}
	check("agui-stream-shape", hdr.Get("X-Run-Id") == "e2e-run-1" && len(types) > 0 && types[0] == agui.EventRunStarted && types[len(types)-1] == agui.EventRunFinished, types)
	check("agui-tool-call-through-gateway", toolName == "minimal__sum" && (e.live || strings.Contains(toolResult, "3")), toolName, toolResult)
	check("agui-text-streamed", strings.Contains(joined, "TEXT_MESSAGE_CONTENT") && strings.Contains(joined, "STEP_FINISHED"), joined)
	r := e.api(usr, "GET", "/v1/runs/e2e-run-1", nil, 200)
	check("run-completed-with-release", r["status"] == run.StatusCompleted && r["releaseDigest"] == approved["digest"] && r["stepCount"] == 2.0, r)
	msgs := e.api(usr, "GET", "/v1/threads/e2e-1/messages", nil, 200)["messages"].([]any)
	check("thread-has-conversation", len(msgs) == 4 && msgs[2].(map[string]any)["role"] == "tool" && msgs[3].(map[string]any)["role"] == "assistant", len(msgs))

	// REST: a run whose tool call needs the owner's confirmation
	prompt = "echo this"
	if e.live {
		prompt = "Please use the echo tool with the text hi."
	}
	cr := e.api(usr, "POST", "/v1/agents/assistant/runs", map[string]any{"threadId": "e2e-2", "messages": []map[string]any{{"id": "m1", "role": "user", "content": prompt}}, "tools": []any{}, "context": []any{}}, 202, "Idempotency-Key", "e2e-c1")
	rid := cr["runId"].(string)
	waiting := e.waitRun(usr, rid, run.StatusAwaitingInput, run.StatusCompleted, run.StatusFailed)
	calls := e.api(usr, "GET", "/v1/runs/"+rid+"/tool-calls", nil, 200)["toolCalls"].([]any)
	check("confirmation-requested", waiting["status"] == run.StatusAwaitingInput && len(calls) == 1 && calls[0].(map[string]any)["state"] == run.CallPendingConfirmation, waiting, calls)
	if waiting["status"] == run.StatusAwaitingInput {
		callID := calls[0].(map[string]any)["toolCallId"].(string)
		e.api(adm, "POST", "/v1/runs/"+rid+"/tool-calls/"+callID+"/decide", map[string]any{"decision": "confirm"}, 403) // only the owner
		dec := e.api(usr, "POST", "/v1/runs/"+rid+"/tool-calls/"+callID+"/decide", map[string]any{"decision": "confirm"}, 200)
		done := e.waitRun(usr, rid, run.StatusCompleted, run.StatusFailed)
		msgs = e.api(usr, "GET", "/v1/threads/e2e-2/messages", nil, 200)["messages"].([]any)
		tool := msgs[2].(map[string]any)
		check("confirmed-call-executed", dec["state"] == run.CallConfirmed && done["status"] == run.StatusCompleted && tool["role"] == "tool" && strings.Contains(fmt.Sprint(tool["content"]), "hi"), dec, done, tool)
	}

	// the gateway audited the HOST's calls under its service identity (records are shipped in batches)
	if e.registry != "" && e.mcpSecret != "" {
		var st int
		seen := map[string]bool{}
		for i := 0; i < 50 && !(seen["minimal__sum/svc-host/ALLOW"] && seen["minimal__echo/svc-host/ALLOW"]); i++ {
			var inv map[string]any
			st, inv, _ = e.do(e.registry, e.mcpToken("bob", "reviewer"), "GET", "/api/v1/invocations", nil)
			if st == 200 {
				for _, it := range inv["items"].([]any) {
					m := it.(map[string]any)
					seen[fmt.Sprint(m["tool"])+"/"+fmt.Sprint(m["subject"])+"/"+fmt.Sprint(m["decision"])] = true
				}
			}
			time.Sleep(300 * time.Millisecond)
		}
		check("gateway-audited-host-calls", st == 200 && seen["minimal__sum/svc-host/ALLOW"] && seen["minimal__echo/svc-host/ALLOW"], st, seen)
	}
	if e.metrics != "" {
		resp, err := e.http.Get(e.metrics)
		body := ""
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body = string(b)
		}
		check("metrics-exposed", err == nil && strings.Contains(body, "idp_host_runs_finished_total") && strings.Contains(body, "idp_host_model_steps_total") && !strings.Contains(body, "carol"), err)
	}
	t.Logf("compose end-to-end: %d checks", len(results))
}
