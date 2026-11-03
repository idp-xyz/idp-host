// Package live is the real-model gate: it runs the two provider adapters against the operator's
// endpoints (nothing is mocked) and checks streaming, tool calling and usage. Skipped unless the
// endpoints are given:
//
//	IDP_LIVE_OPENAI_URL      OpenAI-compatible base URL (.../v1)
//	IDP_LIVE_ANTHROPIC_URL   Anthropic-style base URL (without /v1)
//	IDP_LIVE_MODEL           model id for both
//	IDP_LIVE_API_KEY         optional
package live

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"idp-host/adapters/providers/anthropic"
	"idp-host/adapters/providers/openai"
	"idp-host/internal/agui"
	"idp-host/internal/model"
)

var sumTool = model.ToolSpec{Name: "sum_weights", Description: "Sum package weights in kg.",
	Parameters: json.RawMessage(`{"type":"object","properties":{"weights_kg":{"type":"array","items":{"type":"number"}}},"required":["weights_kg"]}`)}

func providers(t *testing.T) []model.Provider {
	t.Helper()
	mdl := os.Getenv("IDP_LIVE_MODEL")
	if mdl == "" {
		t.Skip("IDP_LIVE_MODEL not set")
	}
	hc, _ := model.NewHTTPClient(2*time.Minute, "")
	var out []model.Provider
	if u := os.Getenv("IDP_LIVE_OPENAI_URL"); u != "" {
		p, err := openai.New(openai.Config{Name: "live-openai", BaseURL: u, APIKey: os.Getenv("IDP_LIVE_API_KEY"), HTTP: hc, UsageInStream: true})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	if u := os.Getenv("IDP_LIVE_ANTHROPIC_URL"); u != "" {
		p, err := anthropic.New(anthropic.Config{Name: "live-anthropic", BaseURL: u, APIKey: os.Getenv("IDP_LIVE_API_KEY"), HTTP: hc})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		t.Skip("no live endpoint configured")
	}
	return out
}

func TestLiveModel(t *testing.T) {
	mdl := os.Getenv("IDP_LIVE_MODEL")
	for _, p := range providers(t) {
		t.Run(p.Name(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			temp := 0.0
			var deltas strings.Builder
			resp, err := p.Complete(ctx, model.Request{Model: mdl, Instructions: "Answer in one short sentence.", Temperature: &temp, MaxOutputTokens: 200,
				Messages: []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: "What is the capital of France?"}}}, func(d model.Delta) { deltas.WriteString(d.Text) })
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.ToLower(resp.Message.Content), "paris") || deltas.String() != resp.Message.Content || resp.StopReason != model.StopEnd {
				t.Fatalf("content %q deltas %q stop %s", resp.Message.Content, deltas.String(), resp.StopReason)
			}
			if resp.Usage.CompletionTokens == 0 {
				t.Errorf("no usage reported: %+v", resp.Usage)
			}
			t.Logf("plain: %q usage=%+v reasoning=%d chars", resp.Message.Content, resp.Usage, len(resp.Reasoning))

			resp, err = p.Complete(ctx, model.Request{Model: mdl, Instructions: "Use the tool for arithmetic.", Temperature: &temp, MaxOutputTokens: 300, Tools: []model.ToolSpec{sumTool},
				Messages: []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: "What is 12.5 kg plus 0.75 kg? Use the tool."}}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StopReason != model.StopToolCalls || len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].Function.Name != "sum_weights" {
				t.Fatalf("expected a tool call: %+v", resp.Message)
			}
			var args map[string]any
			if err := json.Unmarshal([]byte(resp.Message.ToolCalls[0].Function.Arguments), &args); err != nil || args["weights_kg"] == nil {
				t.Fatalf("arguments %s", resp.Message.ToolCalls[0].Function.Arguments)
			}
			t.Logf("tool call: %s %s", resp.Message.ToolCalls[0].Function.Name, resp.Message.ToolCalls[0].Function.Arguments)

			// the tool result goes back and the model answers
			call := resp.Message.ToolCalls[0]
			resp2, err := p.Complete(ctx, model.Request{Model: mdl, Instructions: "Use the tool for arithmetic.", Temperature: &temp, MaxOutputTokens: 200, Tools: []model.ToolSpec{sumTool},
				Messages: []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: "What is 12.5 kg plus 0.75 kg? Use the tool."}, resp.Message,
					{ID: "t1", Role: agui.RoleTool, ToolCallID: call.ID, Content: `{"total_kg": 13.25}`}}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(resp2.Message.Content, "13.25") || resp2.StopReason != model.StopEnd {
				t.Fatalf("final answer %q stop %s", resp2.Message.Content, resp2.StopReason)
			}
			t.Logf("final: %q", resp2.Message.Content)
		})
	}
}
