package model

import (
	"net/http"
	"testing"
	"time"
)

// D56: whose problem a non-retryable provider error is, as the host classifies it from the status, the provider's error
// code or type, and for a quota or a policy refusal the body (which is read to classify and never kept).
func TestWhoseFaultAProviderErrorIs(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		name      string
		status    int
		body      string
		fault     string
		retryable bool
	}{
		{"401", 401, "", FaultCandidate, false},
		{"402", 402, "", FaultCandidate, false},
		{"403", 403, "", FaultCandidate, false},
		{"404", 404, "", FaultCandidate, false},
		{"413", 413, "", FaultCandidate, false},
		{"400 plain", 400, `{"error":{"message":"bad"}}`, "", false},
		{"422", 422, "", "", false},
		{"429 rate limit", 429, `{"error":{"code":"rate_limit_exceeded"}}`, "", true},
		{"429 out of quota", 429, `{"error":{"code":"insufficient_quota"}}`, FaultCandidate, false},
		{"400 credit", 400, `{"error":{"message":"Your credit balance is too low to access the Anthropic API."}}`, FaultCandidate, false},
		{"400 content policy", 400, `{"error":{"code":"content_policy_violation"}}`, FaultPolicy, false},
		{"400 content filter", 400, `{"error":{"code":"content_filter"}}`, FaultPolicy, false},
		{"500", 500, "", "", true},
		{"503 with a policy word is still the provider's outage", 503, `content_filter`, "", true},
	} {
		e := ClassifyHTTP(c.status, http.Header{}, []byte(c.body), now)
		if e.Fault != c.fault || e.Retryable != c.retryable {
			t.Errorf("%s: fault %q retryable %v, want %q %v", c.name, e.Fault, e.Retryable, c.fault, c.retryable)
		}
	}
	// the context window is a size, not a fault: it has its own code and the harness weighs the windows
	if e := ClassifyHTTP(400, http.Header{}, []byte(`maximum context length`), now); e.Code != CodeContextLength || e.Fault != "" {
		t.Errorf("%+v", e)
	}
}

func TestWhoseFaultAStreamErrorIs(t *testing.T) {
	for _, c := range []struct{ name, raw, fault string }{
		{"insufficient_quota", `{"type":"insufficient_quota","code":"insufficient_quota"}`, FaultCandidate},
		{"authentication_error", `{"type":"authentication_error","message":"x"}`, FaultCandidate},
		{"not_found_error", `{"type":"not_found_error"}`, FaultCandidate},
		{"gemini PERMISSION_DENIED", `{"code":403,"status":"PERMISSION_DENIED"}`, FaultCandidate},
		{"gemini NOT_FOUND", `{"code":404,"status":"NOT_FOUND"}`, FaultCandidate},
		{"content_filter", `{"type":"invalid_request_error","code":"content_filter"}`, FaultPolicy},
		{"invalid_request_error", `{"type":"invalid_request_error","message":"bad"}`, ""},
		{"overloaded", `{"type":"overloaded_error"}`, ""},
	} {
		if e := ClassifyStreamError([]byte(c.raw)); e.Fault != c.fault {
			t.Errorf("%s: fault %q, want %q", c.name, e.Fault, c.fault)
		}
	}
}
