package model

import "testing"

// D40: a 400 that says the request does not fit the context window is told apart from any other refusal, in the
// wording of the servers behind the OpenAI-compatible adapter; nothing else changes its classification.
func TestClassifyBody(t *testing.T) {
	for _, c := range []struct {
		name      string
		status    int
		body      string
		code      string
		retryable bool
	}{
		{"OpenAI", 400, `{"error":{"message":"This model's maximum context length is 128000 tokens. However, your messages resulted in 131310 tokens.","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`, CodeContextLength, false},
		{"vLLM", 400, `{"object":"error","message":"This model's maximum context length is 32768 tokens. However, you requested 40000 tokens in the messages, Please reduce the length of the messages.","type":"BadRequestError","param":null,"code":400}`, CodeContextLength, false},
		{"llama.cpp", 400, `{"error":{"code":400,"message":"the request exceeds the available context size, try increasing it","type":"exceed_context_size_error","n_prompt_tokens":131310,"n_ctx":131072}}`, CodeContextLength, false},
		// as the model server of the reference host answered on 2026-11-17 (Strata 0.1.27)
		{"Strata", 400, `{"error": {"type": "invalid_request_error", "message": "prompt (1008941 tokens) + max tokens (1) exceeds the context (131072); requests are never truncated"}}`, CodeContextLength, false},
		{"another 400", 400, `{"error":{"message":"Invalid schema for function 'sum'","type":"invalid_request_error","code":"invalid_function_parameters"}}`, "status", false},
		{"a 400 without a body", 400, ``, "status", false},
		{"not a 400", 500, `{"error":{"message":"maximum context length"}}`, "status", true},
		{"rate limit", 429, `{"error":{"code":"rate_limit_exceeded"}}`, "status", true},
	} {
		e := ClassifyBody(c.status, []byte(c.body))
		if e.Code != c.code || e.Status != c.status || e.Retryable != c.retryable {
			t.Errorf("%s: %+v", c.name, e)
		}
	}
}
