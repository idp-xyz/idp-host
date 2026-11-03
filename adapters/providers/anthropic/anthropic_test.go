package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"idp-host/internal/agui"
	"idp-host/internal/model"
	"idp-host/tests/fixtures/fakemodel"
)

type capture struct {
	mu   sync.Mutex
	body []byte
	next http.Handler
}

func (c *capture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.body = b
	c.mu.Unlock()
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(strings.NewReader(string(b)))
	c.next.ServeHTTP(w, r2)
}

func provider(t *testing.T, srv *httptest.Server) *Provider {
	t.Helper()
	hc, _ := model.NewHTTPClient(10*time.Second, "")
	p, err := New(Config{Name: "fake", BaseURL: srv.URL, HTTP: hc, NewMessageID: func() string { return "msg_1" }})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

var sumTool = model.ToolSpec{Name: "parcel__sum", Description: "adds", Parameters: json.RawMessage(`{"type":"object"}`)}

func TestStreamingTextToolsAndUsage(t *testing.T) {
	fake := fakemodel.New()
	cap := &capture{next: fake.Handler()}
	srv := httptest.NewServer(cap)
	defer srv.Close()
	p := provider(t, srv)
	var deltas strings.Builder
	resp, err := p.Complete(context.Background(), model.Request{Model: "fake-1", Instructions: "be brief",
		Messages: []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: "hello there"}}}, func(d model.Delta) { deltas.WriteString(d.Text) })
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Content != "Echo: hello there" || deltas.String() != resp.Message.Content || resp.StopReason != model.StopEnd ||
		resp.Usage.PromptTokens != 20 || resp.Usage.CompletionTokens != 7 {
		t.Fatalf("%+v", resp)
	}
	var sent map[string]any
	_ = json.Unmarshal(cap.body, &sent)
	if sent["system"] != "be brief" || sent["max_tokens"] != float64(DefaultMaxTokens) || sent["stream"] != true {
		t.Fatalf("%v", sent)
	}

	resp, err = p.Complete(context.Background(), model.Request{Model: "fake-1", Tools: []model.ToolSpec{sumTool},
		Messages: []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: "please sum"}}}, nil)
	if err != nil || resp.StopReason != model.StopToolCalls || len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].Function.Arguments != `{"a":1,"b":2}` ||
		resp.Reasoning != "need the tool" {
		t.Fatalf("%+v %v", resp, err)
	}
	call := resp.Message.ToolCalls[0]
	// two tool results for one assistant turn must be merged into ONE user message (alternation rule)
	second := agui.Message{ID: "a2", Role: agui.RoleAssistant, ToolCalls: []agui.ToolCall{call, {ID: "c2", Type: "function", Function: agui.FunctionCall{Name: "parcel__sum", Arguments: `{"a":3}`}}}}
	resp2, err := p.Complete(context.Background(), model.Request{Model: "fake-1", Tools: []model.ToolSpec{sumTool},
		Messages: []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: "please sum"}, second,
			{ID: "t1", Role: agui.RoleTool, ToolCallID: call.ID, Content: "3"}, {ID: "t2", Role: agui.RoleTool, ToolCallID: "c2", Error: "boom"}}}, nil)
	if err != nil || resp2.Message.Content != "Result: boom" {
		t.Fatalf("%+v %v", resp2, err)
	}
	_ = json.Unmarshal(cap.body, &sent)
	msgs := sent["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("expected user, assistant, user; got %d: %v", len(msgs), msgs)
	}
	last := msgs[2].(map[string]any)
	blocks := last["content"].([]any)
	if last["role"] != "user" || len(blocks) != 2 || blocks[1].(map[string]any)["is_error"] != true {
		t.Fatalf("%v", last)
	}
	asst := msgs[1].(map[string]any)["content"].([]any)
	if len(asst) != 2 || asst[0].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("%v", asst)
	}
}

func TestErrorsAndPolicy(t *testing.T) {
	fake := fakemodel.New()
	srv := httptest.NewServer(fake.Handler())
	defer srv.Close()
	p := provider(t, srv)
	_, err := p.Complete(context.Background(), model.Request{Model: "fake-1", Messages: []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: "fail: x"}}}, nil)
	var me *model.Error
	if !errors.As(err, &me) || me.Status != 500 {
		t.Fatalf("%v", err)
	}
	if _, err := p.Complete(context.Background(), model.Request{Model: "fake-1", Messages: []agui.Message{{ID: "a", Role: agui.RoleAssistant, Content: "x"}}}, nil); !errors.As(err, &me) || me.Code != "schema" {
		t.Fatalf("conversation not starting with user accepted: %v", err)
	}
	hc, _ := model.NewHTTPClient(time.Second, "")
	if _, err := New(Config{Name: "x", BaseURL: "http://api.example", HTTP: hc}); err == nil {
		t.Fatal("plain http to a non-loopback host accepted")
	}
}
