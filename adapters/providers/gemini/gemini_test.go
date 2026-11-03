package gemini

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
	p, err := New(Config{Name: "fake", BaseURL: srv.URL, HTTP: hc, APIKey: "k", NewMessageID: func() string { return "msg_1" }})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

var sumTool = model.ToolSpec{Name: "parcel__sum", Description: "adds", Parameters: json.RawMessage(`{"type":"object","properties":{"a":{"type":"number"}}}`)}

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
		resp.Usage.PromptTokens != 20 || resp.Usage.CompletionTokens != 7 || resp.Message.ID != "msg_1" {
		t.Fatalf("%+v deltas=%q", resp, deltas.String())
	}
	var sent map[string]any
	_ = json.Unmarshal(cap.body, &sent)
	if sent["systemInstruction"] == nil || len(sent["contents"].([]any)) != 1 {
		t.Fatalf("%v", sent)
	}

	resp, err = p.Complete(context.Background(), model.Request{Model: "fake-1", Tools: []model.ToolSpec{sumTool},
		Messages: []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: "please sum"}}}, nil)
	if err != nil || resp.StopReason != model.StopToolCalls || len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].Function.Name != "parcel__sum" ||
		resp.Message.ToolCalls[0].Function.Arguments != `{"a":1,"b":2}` || resp.Message.ToolCalls[0].ID == "" || resp.Reasoning != "need the tool" {
		t.Fatalf("%+v %v", resp, err)
	}
	call := resp.Message.ToolCalls[0]
	// the result goes back as a functionResponse carrying the function's name
	resp2, err := p.Complete(context.Background(), model.Request{Model: "fake-1", Tools: []model.ToolSpec{sumTool},
		Messages: []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: "please sum"}, resp.Message,
			{ID: "t1", Role: agui.RoleTool, ToolCallID: call.ID, Content: "3"}}}, nil)
	if err != nil || resp2.Message.Content != "Result: 3" {
		t.Fatalf("%+v %v", resp2, err)
	}
	_ = json.Unmarshal(cap.body, &sent)
	contents := sent["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("expected user, model, user; got %v", contents)
	}
	fr := contents[2].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)
	if fr["name"] != "parcel__sum" || fr["response"].(map[string]any)["result"] != 3.0 {
		t.Fatalf("%v", fr)
	}
	if _, ok := sent["tools"].([]any)[0].(map[string]any)["functionDeclarations"]; !ok {
		t.Fatalf("%v", sent["tools"])
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
	// a tool message answering an unknown call cannot be mapped to a function name
	if _, err := p.Complete(context.Background(), model.Request{Model: "fake-1", Messages: []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: "x"},
		{ID: "t1", Role: agui.RoleTool, ToolCallID: "nope", Content: "3"}}}, nil); !errors.As(err, &me) || me.Code != "schema" {
		t.Fatalf("%v", err)
	}
	hc, _ := model.NewHTTPClient(time.Second, "")
	if _, err := New(Config{Name: "x", BaseURL: "http://api.example", HTTP: hc}); err == nil {
		t.Fatal("plain http to a non-loopback host accepted")
	}
}
