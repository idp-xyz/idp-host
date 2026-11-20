package anthropic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"idp-host/internal/agui"
	"idp-host/internal/model"
)

// D55: a stream that stops with max_tokens or model_context_window_exceeded is a cut-off answer, also when a tool_use block
// was being written: its input may be cut in the middle (invalid JSON) or happen to be complete.
func TestACutOffAnswerWithAToolUseIsReportedAsCutOff(t *testing.T) {
	for _, stop := range []string{"max_tokens", "model_context_window_exceeded"} {
		for _, c := range []struct{ name, partial string }{
			{"cut in a string", `{\"a\":1,\"b\":\"hel`},
			{"parses", `{\"a\":1,\"b\":2}`},
		} {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":100}}}\n\n" +
					"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tu_1\",\"name\":\"sum\",\"input\":{}}}\n\n" +
					"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"" + c.partial + "\"}}\n\n" +
					"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"" + stop + "\"},\"usage\":{\"output_tokens\":900}}\n\n"))
			}))
			resp, err := idleProvider(t, srv.URL, 0).Complete(context.Background(), model.Request{Model: "m", Messages: []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: "hi"}}}, nil)
			srv.Close()
			if err != nil {
				t.Errorf("%s, %s: %v", stop, c.name, err)
				continue
			}
			if resp.StopReason != model.StopLength || len(resp.Message.ToolCalls) != 1 {
				t.Errorf("%s, %s: stop %q with %d tool call(s), want %q with the call kept for the host to count", stop, c.name, resp.StopReason, len(resp.Message.ToolCalls), model.StopLength)
			}
		}
	}
}
