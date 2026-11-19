package gemini

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
		{"input token count", `{"error":{"code":400,"message":"The input token count (1100000) exceeds the maximum number of tokens allowed (1048576).","status":"INVALID_ARGUMENT"}}`},
	} {
		// not streamed: HTTP 400
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			_, _ = w.Write([]byte(c.body))
		}))
		_, err := provider(t, srv).Complete(context.Background(), hi, nil)
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
		_, err = provider(t, srv).Complete(context.Background(), hi, nil)
		srv.Close()
		if !errors.As(err, &me) || me.Code != model.CodeContextLength || me.Retryable {
			t.Errorf("%s, in the stream: %v", c.name, err)
		}
	}
	// a 400 about something else is not the context window
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"code":400,"message":"Invalid JSON payload received. Unknown name \\"foo\\"","status":"INVALID_ARGUMENT"}}`))
	}))
	defer srv.Close()
	_, err := provider(t, srv).Complete(context.Background(), hi, nil)
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
	body, err := provider(t, httptest.NewServer(http.NotFoundHandler())).body(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(body)
	var wire struct {
		Contents []struct {
			Parts []struct {
				FunctionCall *struct {
					Args map[string]any `json:"args"` // an object, as functionCall.args requires
				} `json:"functionCall"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("the request body is not valid for functionCall.args: %v", err)
	}
	var got map[string]any
	for _, c := range wire.Contents {
		for _, p := range c.Parts {
			if p.FunctionCall != nil {
				got = p.FunctionCall.Args
			}
		}
	}
	if got["language"] != "python" || got["n"] != float64(3) || !strings.Contains(got["code"].(string), "The call was made with the whole value") {
		t.Errorf("%v", got)
	}
}
