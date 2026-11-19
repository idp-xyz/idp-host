package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClassifyStreamError(t *testing.T) {
	cases := []struct {
		name, raw, code string
		retry           bool
	}{
		{"openai server error, numeric code", `{"code":500,"message":"boom","type":"server_error"}`, CodeProviderServer, true},
		{"anthropic overloaded", `{"type":"overloaded_error","message":"Overloaded"}`, CodeProviderOverloaded, true},
		{"anthropic api_error", `{"type":"api_error","message":"Internal server error"}`, CodeProviderServer, true},
		{"anthropic rate limit", `{"type":"rate_limit_error","message":"slow down"}`, CodeProviderRateLimit, true},
		{"gemini unavailable", `{"code":503,"message":"The model is overloaded","status":"UNAVAILABLE"}`, CodeProviderOverloaded, true},
		{"gemini resource exhausted", `{"code":429,"message":"quota","status":"RESOURCE_EXHAUSTED"}`, CodeProviderRateLimit, true},
		{"openai quota is not transient", `{"message":"You exceeded your current quota","type":"insufficient_quota","code":"insufficient_quota"}`, CodeProviderError, false},
		{"openai context code", `{"message":"too long","type":"invalid_request_error","code":"context_length_exceeded"}`, CodeContextLength, false},
		{"llama.cpp context", `{"code":400,"message":"the request exceeds the available context size","type":"exceed_context_size_error"}`, CodeContextLength, false},
		{"anthropic prompt too long", `{"type":"invalid_request_error","message":"prompt is too long: 300000 tokens > 200000 maximum"}`, CodeContextLength, false},
		{"anything else", `{"type":"invalid_request_error","message":"bad field"}`, CodeProviderError, false},
		{"a bare string", `"it broke"`, CodeProviderError, false},
		{"no fields", `{}`, CodeProviderError, false},
	}
	for _, c := range cases {
		e := ClassifyStreamError(json.RawMessage(c.raw))
		if e.Code != c.code || e.Retryable != c.retry || e.Detail == "" {
			t.Errorf("%s: %+v", c.name, e)
		}
	}
	if e := ClassifyStreamError(json.RawMessage(`{"type":"overloaded_error","message":"Overloaded"}`)); !strings.Contains(e.Detail, "overloaded_error") || !strings.Contains(e.Detail, "Overloaded") {
		t.Errorf("the provider's words are lost: %q", e.Detail)
	}
}

func TestStreamErrorDetailIsBoundedAndClean(t *testing.T) {
	msg := "line1\nline2\x00\x1b[31mred‮ " + strings.Repeat("x", 5000)
	raw, _ := json.Marshal(map[string]any{"type": strings.Repeat("t", 1000), "code": 502, "message": msg})
	e := ClassifyStreamError(raw)
	if n := len([]rune(e.Detail)); n > MaxDetail+1 {
		t.Errorf("detail has %d characters", n)
	}
	for _, r := range e.Detail {
		if r < ' ' || r == 0x7f || r == 0x202e {
			t.Fatalf("control character %U in %q", r, e.Detail)
		}
	}
	if !strings.HasSuffix(e.Detail, "…") {
		t.Errorf("not marked as cut: %q", e.Detail[len(e.Detail)-10:])
	}
	if got := CleanDetail("a\n\n  b\tc\xff", 50); got != "a b c" {
		t.Errorf("%q", got)
	}
	if d := DecodeError("a stream chunk is not JSON"); d.Code != "decode" || d.Retryable || d.Detail == "" {
		t.Errorf("%+v", d)
	}
}
