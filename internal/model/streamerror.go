package model

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxDetail bounds the text of Error.Detail, in characters (D50).
const MaxDetail = 300

// maxDetailField bounds each of the provider's fields before they are joined.
const maxDetailField = 160

// CleanDetail makes provider-controlled text fit to record: control and other non-printing characters (newlines
// included) become spaces, runs of spaces collapse, invalid UTF-8 is dropped, and the result is cut to max characters
// with an ellipsis.
func CleanDetail(s string, max int) string {
	var b strings.Builder
	space := true
	n := 0
	for _, r := range s {
		if r == utf8.RuneError || unicode.IsControl(r) || !unicode.IsPrint(r) {
			r = ' '
		}
		if r == ' ' {
			if space {
				continue
			}
			space = true
		} else {
			space = false
		}
		if n >= max {
			return strings.TrimRight(b.String(), " ") + "…"
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimRight(b.String(), " ")
}

// DecodeError is the error of a stream chunk that is not what the wire format says it is; where says which chunk, in
// words of the adapter's own, never the payload.
func DecodeError(where string) *Error {
	return &Error{Code: "decode", Detail: CleanDetail(where, MaxDetail)}
}

// ClassifyStreamError turns the error object a provider sent inside a stream into an Error (D50). raw is the value of
// the "error" key: OpenAI-compatible servers send {message, type, code}, Anthropic {type, message}, Google
// {code, message, status}; a bare string is taken as the message. The class decides the code and whether the error is
// retryable; a message that says the request does not fit the context window is the D40 error. The result's Detail
// carries the provider's type, code and message, cleaned and bounded.
func ClassifyStreamError(raw json.RawMessage) *Error {
	var f struct {
		Message json.RawMessage `json:"message"`
		Type    json.RawMessage `json:"type"`
		Code    json.RawMessage `json:"code"`
		Status  json.RawMessage `json:"status"`
	}
	var msg, typ, code, status string
	if json.Unmarshal(raw, &f) == nil {
		msg, typ, code, status = scalar(f.Message), scalar(f.Type), scalar(f.Code), scalar(f.Status)
	} else {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			msg = s
		}
	}
	var parts []string
	for _, kv := range [][2]string{{"type", typ}, {"code", code}, {"status", status}, {"message", msg}} {
		if v := CleanDetail(kv[1], maxDetailField); v != "" {
			parts = append(parts, kv[0]+"="+v)
		}
	}
	detail := CleanDetail(strings.Join(parts, " "), MaxDetail)
	if detail == "" {
		detail = "the provider sent an error event without fields"
	}
	e := &Error{Detail: detail}
	// the class comes from the provider's type, code and status; only the context-window question reads the message
	tokens := strings.ToLower(typ + " " + code + " " + status)
	num, _ := strconv.Atoi(strings.TrimSpace(code))
	switch {
	case containsAny(strings.ToLower(msg+" "+tokens), contextMarkers):
		e.Code = CodeContextLength
	case containsAny(tokens, []string{"insufficient_quota", "billing", "invalid_api_key", "authentication", "permission"}):
		e.Code = CodeProviderError
	case num == 503 || num == 529 || containsAny(tokens, []string{"overloaded", "unavailable"}):
		e.Code, e.Retryable = CodeProviderOverloaded, true
	case num == 429 || containsAny(tokens, []string{"rate_limit", "rate limit", "too_many_requests", "resource_exhausted"}):
		e.Code, e.Retryable = CodeProviderRateLimit, true
	case num >= 500 || containsAny(tokens, []string{"server_error", "api_error", "internal", "timeout", "deadline_exceeded"}):
		e.Code, e.Retryable = CodeProviderServer, true
	default:
		e.Code = CodeProviderError
	}
	return e
}

// scalar reads a JSON string or number as text; anything else is empty.
func scalar(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}

func containsAny(text string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}
