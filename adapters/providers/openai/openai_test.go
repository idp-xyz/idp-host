package openai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"idp-host/internal/agui"
	"idp-host/internal/model"
	"idp-host/tests/fixtures/fakemodel"
)

func provider(t *testing.T, srv *httptest.Server, usage bool) *Provider {
	t.Helper()
	hc, _ := model.NewHTTPClient(10*time.Second, "")
	p, err := New(Config{Name: "fake", BaseURL: srv.URL + "/v1", HTTP: hc, UsageInStream: usage, NewMessageID: func() string { return "msg_1" }})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

var sumTool = model.ToolSpec{Name: "parcel__sum", Description: "adds", Parameters: json.RawMessage(`{"type":"object","properties":{"a":{"type":"number"},"b":{"type":"number"}}}`)}

func TestStreamingTextAndUsage(t *testing.T) {
	fake := fakemodel.New()
	srv := httptest.NewServer(fake.Handler())
	defer srv.Close()
	p := provider(t, srv, true)
	var deltas strings.Builder
	resp, err := p.Complete(context.Background(), model.Request{Model: "fake-1", Instructions: "be brief",
		Messages: []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: "hello there"}}}, func(d model.Delta) { deltas.WriteString(d.Text) })
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Content != "Echo: hello there" || deltas.String() != resp.Message.Content || resp.StopReason != model.StopEnd {
		t.Fatalf("%+v deltas=%q", resp, deltas.String())
	}
	if resp.Usage.PromptTokens != 20 || resp.Usage.CompletionTokens != 7 || resp.Message.ID != "msg_1" {
		t.Fatalf("usage %+v id %s", resp.Usage, resp.Message.ID)
	}
	last := fake.Last()
	if last[0].Role != "system" || last[0].Content != "be brief" || last[1].Role != "user" {
		t.Fatalf("instructions not sent as system message: %+v", last)
	}
}

func TestToolCallsAndReasoning(t *testing.T) {
	fake := fakemodel.New()
	srv := httptest.NewServer(fake.Handler())
	defer srv.Close()
	p := provider(t, srv, false)
	resp, err := p.Complete(context.Background(), model.Request{Model: "fake-1", Tools: []model.ToolSpec{sumTool},
		Messages: []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: "please sum"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != model.StopToolCalls || len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].Function.Name != "parcel__sum" ||
		resp.Message.ToolCalls[0].Function.Arguments != `{"a":1,"b":2}` || resp.Message.ToolCalls[0].ID == "" {
		t.Fatalf("%+v", resp.Message)
	}
	if resp.Reasoning != "need the tool" {
		t.Fatalf("reasoning %q", resp.Reasoning)
	}
	// the assistant turn and the tool result go back in the OpenAI shape
	resp2, err := p.Complete(context.Background(), model.Request{Model: "fake-1", Tools: []model.ToolSpec{sumTool},
		Messages: []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: "please sum"}, resp.Message,
			{ID: "t1", Role: agui.RoleTool, ToolCallID: resp.Message.ToolCalls[0].ID, Content: "3"}}}, nil)
	if err != nil || resp2.Message.Content != "Result: 3" {
		t.Fatalf("%+v %v", resp2, err)
	}
	last := fake.Last()
	if last[1].Role != "assistant" || len(last[1].ToolCalls) != 1 || last[2].Role != "tool" || last[2].ToolCallID != resp.Message.ToolCalls[0].ID {
		t.Fatalf("%+v", last)
	}
}

func TestNonStreamingServer(t *testing.T) {
	fake := fakemodel.New()
	// a server that answers application/json although stream was requested
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		body["stream"] = false
		b, _ := json.Marshal(body)
		r2, _ := http.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(b)))
		fake.Handler().ServeHTTP(w, r2)
	}))
	defer srv.Close()
	p := provider(t, srv, true)
	resp, err := p.Complete(context.Background(), model.Request{Model: "fake-1", Tools: []model.ToolSpec{sumTool},
		Messages: []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: "sum"}}}, nil)
	if err != nil || len(resp.Message.ToolCalls) != 1 || resp.Usage.CompletionTokens != 7 {
		t.Fatalf("%+v %v", resp, err)
	}
}

func TestErrorsAreSanitized(t *testing.T) {
	fake := fakemodel.New()
	srv := httptest.NewServer(fake.Handler())
	defer srv.Close()
	p := provider(t, srv, true)
	_, err := p.Complete(context.Background(), model.Request{Model: "fake-1", Messages: []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: "fail: now"}}}, nil)
	var me *model.Error
	if !errors.As(err, &me) || me.Code != "status" || me.Status != 500 || !me.Retryable || strings.Contains(err.Error(), "scripted") {
		t.Fatalf("%v", err)
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, srv.URL, http.StatusFound) }))
	defer redirect.Close()
	p = provider(t, redirect, true)
	if _, err := p.Complete(context.Background(), model.Request{Model: "fake-1", Messages: []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: "x"}}}, nil); !errors.As(err, &me) || me.Status != 302 {
		t.Fatalf("redirect followed or misreported: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Complete(ctx, model.Request{Model: "fake-1", Messages: []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: "x"}}}, nil); !errors.As(err, &me) || me.Code != "canceled" {
		t.Fatalf("%v", err)
	}
}

func TestEndpointPolicy(t *testing.T) {
	hc, _ := model.NewHTTPClient(time.Second, "")
	for _, u := range []string{"http://models.example/v1", "https://u:p@models.example/v1", "ftp://x/v1", "https://models.example/v1?x=1"} {
		if _, err := New(Config{Name: "x", BaseURL: u, HTTP: hc}); err == nil {
			t.Errorf("%s accepted", u)
		}
	}
	for _, u := range []string{"http://127.0.0.1:8080/v1", "http://localhost:8080/v1/", "https://models.example/v1"} {
		if _, err := New(Config{Name: "x", BaseURL: u, HTTP: hc}); err != nil {
			t.Errorf("%s refused: %v", u, err)
		}
	}
}
