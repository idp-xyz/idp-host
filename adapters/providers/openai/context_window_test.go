package openai

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
		{"the message of OpenAI and vLLM", `{"error":{"message":"This model's maximum context length is 128000 tokens. However, your messages resulted in 130000 tokens.","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`},
		{"llama.cpp", `{"error":{"code":400,"message":"the request exceeds the available context size (131072 tokens)","type":"exceed_context_size_error","n_prompt_tokens":140000,"n_ctx":131072}}`},
	} {
		// not streamed: HTTP 400
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			_, _ = w.Write([]byte(c.body))
		}))
		_, err := provider(t, srv, true).Complete(context.Background(), hi, nil)
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
			_, _ = w.Write([]byte(`data: ` + c.body + "\n\n"))
		}))
		_, err = provider(t, srv, true).Complete(context.Background(), hi, nil)
		srv.Close()
		if !errors.As(err, &me) || me.Code != model.CodeContextLength || me.Retryable {
			t.Errorf("%s, in the stream: %v", c.name, err)
		}
	}
	// a 400 about something else is not the context window
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid schema for function","type":"invalid_request_error","code":"invalid_function_parameters"}}`))
	}))
	defer srv.Close()
	_, err := provider(t, srv, true).Complete(context.Background(), hi, nil)
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
	b, err := json.Marshal((&Provider{cfg: Config{MaxTokensField: "max_tokens"}}).body(req, true))
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Messages []struct {
			ToolCalls []struct {
				Function struct{ Name, Arguments string } `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	for _, m := range wire.Messages {
		for _, c := range m.ToolCalls {
			if err := json.Unmarshal([]byte(c.Function.Arguments), &got); err != nil { // arguments are a JSON string for this wire form
				t.Fatalf("the arguments on the wire are not JSON: %v", err)
			}
		}
	}
	if got["language"] != "python" || got["n"] != float64(3) || !strings.Contains(got["code"].(string), "The call was made with the whole value") {
		t.Errorf("%v", got)
	}
}
