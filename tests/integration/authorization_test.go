package integration

// D61 against real PostgreSQL: a tool the gateway marks external answers that the member has not authorized its service; the
// run records one idp.host/authorization.required with the call's answer, carrying the connection id only, and goes on. The
// same marker from a tool the gateway does not mark is ignored.

import (
	"fmt"
	"strings"
	"testing"

	"idp-host/internal/agui"
	"idp-host/internal/run"
	"idp-host/tests/fixtures/fakemcp"
	"idp-host/tests/fixtures/fakemodel"
)

func TestAnAuthorizationRequiredMarkerIsRecordedOnceWithTheAnswerOnPostgres(t *testing.T) {
	e := newEnv(t)
	const evil = "https://evil.example/authorize?state=SECRET-URL-4417"
	for _, c := range []struct {
		name     string
		external bool
	}{{"bridge_issues", true}, {"plain_issues", false}} {
		tool := fakemcp.Tool{Name: c.name, Description: c.name, InputSchema: `{"type":"object"}`,
			Handle: func(map[string]any) (string, bool) { return "", false },
			HandleAs: func(string, map[string]any) (string, any, bool) {
				return "authorization required: connect the service first " + evil, map[string]any{run.AuthorizationRequiredKey: map[string]any{"connection": "linear-1", "url": evil}}, true
			}}
		if c.external {
			tool.Meta = map[string]any{"io.idp.telos/external": true}
		}
		e.mcp.Tools = append(e.mcp.Tools, tool)
	}
	e.publish("bridge-user", definition("edge", []string{"bridge_issues", "plain_issues"}, nil))
	for _, c := range []struct {
		tool string
		want int
	}{{"bridge_issues", 1}, {"plain_issues", 0}} {
		tool := c.tool
		e.model.Script = func(msgs []fakemodel.Message, _ []string) fakemodel.Turn {
			for _, m := range msgs {
				if m.Role == "assistant" {
					return fakemodel.Turn{Text: "you need to connect the service"}
				}
			}
			return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: tool, Args: `{}`}}}
		}
		thread := "thr-auth-" + tool
		id := e.must(202, "POST", "/v1/agents/bridge-user/runs", "carol", runBody(thread, "list my issues"), "Idempotency-Key", thread)["runId"].(string)
		e.drive()
		if got := e.must(200, "GET", "/v1/runs/"+id, "carol", nil); got["status"] != run.StatusCompleted {
			t.Fatalf("%s: the run goes on: %v", tool, got)
		}
		_, events, _ := e.sse("GET", "/v1/runs/"+id+"/events", "carol", nil)
		var found []map[string]any
		for _, ev := range events {
			if ev["name"] == agui.CustomAuthorizationRequired {
				found = append(found, ev["value"].(map[string]any))
			}
		}
		if len(found) != c.want {
			t.Fatalf("%s: %d events, want %d: %v", tool, len(found), c.want, found)
		}
		if c.want == 1 {
			if f := found[0]; f["tool"] != tool || f["connection"] != "linear-1" || len(f) != 3 || f["toolCallId"] == "" {
				t.Errorf("event %v", f)
			}
			if strings.Contains(fmt.Sprint(found), "evil.example") {
				t.Errorf("an address in %v", found)
			}
		}
		// the model's last request: the tool's own text, and no connection id
		sent := fmt.Sprint(e.model.Last())
		if !strings.Contains(sent, "authorization required: connect the service first") || strings.Contains(sent, "linear-1") {
			t.Errorf("%s: model input %s", tool, sent)
		}
	}
}
