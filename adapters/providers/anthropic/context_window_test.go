package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"idp-host/internal/agui"
	"idp-host/internal/ctxwin"
	"idp-host/internal/model"
)

// D52: what this provider documents for a request that does not fit the model's context window is the D40 error, whether
// the refusal comes as an HTTP 400 or as an error event inside a stream that began with 200; any other 400 stays a bare
// status.
func TestContextWindowErrorsAreRecognisedStreamedAndNot(t *testing.T) {
	hi := model.Request{Model: "m", Messages: []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: "hi"}}}
	for _, c := range []struct{ name, body string }{
		{"prompt is too long", `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 215000 tokens > 200000 maximum"}}`},
		{"input and max_tokens", `{"type":"error","error":{"type":"invalid_request_error","message":"input length and ` + "`max_tokens`" + ` exceed context limit: 188240 + 21333 > 200000, decrease input length or ` + "`max_tokens`" + ` and try again"}}`},
	} {
		// not streamed: HTTP 400
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			_, _ = w.Write([]byte(c.body))
		}))
		_, err := idleProvider(t, srv.URL, 0).Complete(context.Background(), hi, nil)
		srv.Close()
		var me *model.Error
		if !errors.As(err, &me) || me.Code != model.CodeContextLength || me.Status != 400 || me.Retryable {
			t.Errorf("%s, HTTP 400: %v", c.name, err)
		}
		if err != nil && strings.Contains(err.Error(), "maximum") {
			t.Errorf("%s: the provider's words are in the error: %v", c.name, err)
		}
		// streamed: the same error as an event in a 200 stream
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			_, _ = w.Write([]byte("event: error\ndata: " + c.body + "\n\n"))
		}))
		_, err = idleProvider(t, srv.URL, 0).Complete(context.Background(), hi, nil)
		srv.Close()
		if !errors.As(err, &me) || me.Code != model.CodeContextLength || me.Retryable {
			t.Errorf("%s, in the stream: %v", c.name, err)
		}
	}
	// a 400 about something else is not the context window
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"messages.0.content: Input should be a valid list"}}`))
	}))
	defer srv.Close()
	_, err := idleProvider(t, srv.URL, 0).Complete(context.Background(), hi, nil)
	var me *model.Error
	if !errors.As(err, &me) || me.Code != "status" || me.Status != 400 {
		t.Errorf("another 400: %v", err)
	}
}

// D52: arguments shortened by the harness are a valid request for this provider: the wire form still carries a JSON
// object, with the keys of the call and the marker that says the call was made with the whole value.
func TestShortenedArgumentsMakeAValidRequest(t *testing.T) {
	long := strings.Repeat("print(\"price\")  # 价格 <b>\n", 400)
	args, _ := json.Marshal(map[string]any{"language": "python", "code": long, "n": 3})
	short, ok := ctxwin.ShortenArguments(string(args), 600)
	if !ok {
		t.Fatal("not shortened")
	}
	req := model.Request{Model: "m", Messages: []agui.Message{
		{ID: "u1", Role: agui.RoleUser, Content: "go"},
		{ID: "a1", Role: agui.RoleAssistant, ToolCalls: []agui.ToolCall{{ID: "c1", Type: "function", Function: agui.FunctionCall{Name: "run", Arguments: short}}}},
		{ID: "t1", Role: agui.RoleTool, ToolCallID: "c1", Content: "ok"},
	}}
	body, err := idleProvider(t, "http://127.0.0.1:1", 0).body(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(body)
	var wire struct {
		Messages []struct {
			Content []struct {
				Type  string         `json:"type"`
				Input map[string]any `json:"input"` // an object, as the Messages API requires
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("the request body is not valid for the tool_use input: %v", err)
	}
	var got map[string]any
	for _, m := range wire.Messages {
		for _, c := range m.Content {
			if c.Type == "tool_use" {
				got = c.Input
			}
		}
	}
	if got["language"] != "python" || got["n"] != float64(3) || !strings.Contains(got["code"].(string), "The call was made with the whole value") {
		t.Errorf("%v", got)
	}
}

// D52: a stream that ends with stop_reason model_context_window_exceeded is an answer the window cut off, as max_tokens is
// (D40 fails such a run instead of completing it).
func TestAStreamThatRanOutOfWindowEndsAsCutOff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":199000}}}\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"I was saying\"}}\n\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"model_context_window_exceeded\"},\"usage\":{\"output_tokens\":900}}\n\n"))
	}))
	defer srv.Close()
	resp, err := idleProvider(t, srv.URL, 0).Complete(context.Background(), model.Request{Model: "m", Messages: []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: "hi"}}}, nil)
	if err != nil || resp.StopReason != model.StopLength {
		t.Fatalf("%v %+v", err, resp)
	}
}
