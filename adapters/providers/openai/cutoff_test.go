package openai

import (
	"testing"

	"idp-host/internal/model"
)

// D55: an answer stopped by the output limit (finish_reason "length") is a cut-off answer even when it carries tool calls,
// and whether the cut arguments happen to be valid JSON or not. The adapter used to say "tool_calls" for the first
// (the harness then ran the call) and fail with "schema" for the second (the member was told nothing of the cut).
func TestACutOffAnswerWithToolCallsIsReportedAsCutOff(t *testing.T) {
	for _, c := range []struct{ name, args string }{
		{"arguments cut in the middle of a string", `{"a":1,"b":"hel`},
		{"arguments that parse though the answer was cut", `{"a":1,"b":2}`},
	} {
		chunk := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"sum","arguments":` + quote(c.args) + `}}]}}]}`
		p := provider(t, serve(t, "text/event-stream", chunk, `{"choices":[{"index":0,"finish_reason":"length","delta":{}}]}`), true)
		resp, _, err := ask(t, p)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if resp.StopReason != model.StopLength || len(resp.Message.ToolCalls) != 1 {
			t.Errorf("%s: stop %q with %d tool call(s), want %q with the call kept for the host to count", c.name, resp.StopReason, len(resp.Message.ToolCalls), model.StopLength)
		}
	}
	// the same arguments of an answer that ended by itself are still a schema error (unchanged)
	p := provider(t, serve(t, "text/event-stream", `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"sum","arguments":"{\"a\":"}}]}}]}`,
		`{"choices":[{"index":0,"finish_reason":"tool_calls","delta":{}}]}`), true)
	if _, _, err := ask(t, p); err == nil {
		t.Errorf("invalid arguments of a finished answer were accepted")
	}
}

func quote(s string) string {
	b := []byte{'"'}
	for _, r := range s {
		if r == '"' || r == '\\' {
			b = append(b, '\\')
		}
		b = append(b, byte(r))
	}
	return string(append(b, '"'))
}
