package gemini

import (
	"testing"

	"idp-host/internal/model"
)

// D55: a candidate that finished with MAX_TOKENS is a cut-off answer also when it carries a functionCall. Gemini sends the
// arguments as an object, so a cut one is either missing fields or an unusable value; either way it is not a call to run.
func TestACutOffAnswerWithAFunctionCallIsReportedAsCutOff(t *testing.T) {
	for _, c := range []struct{ name, args string }{
		{"complete object", `{"a":1,"b":2}`},
		{"partial object", `{"a":1}`},
		{"null arguments", `null`},
	} {
		p := provider(t, serve(t, `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"sum","args":`+c.args+`}}]},"finishReason":"MAX_TOKENS"}]}`))
		resp, _, err := ask(t, p)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if resp.StopReason != model.StopLength || len(resp.Message.ToolCalls) != 1 {
			t.Errorf("%s: stop %q with %d tool call(s), want %q with the call kept for the host to count", c.name, resp.StopReason, len(resp.Message.ToolCalls), model.StopLength)
		}
	}
}
