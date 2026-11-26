// Package model is the provider-neutral model interface (ADR-0001 D9): one request shape, one
// response shape, streaming text deltas, tool calls and usage. Providers (adapters/providers/*) are
// registered by name; the release's model profile selects one. The transport policy for every
// provider lives here too: HTTPS unless loopback, no redirects, no environment proxy, bounded
// bodies and durations, sanitized errors.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"idp-host/internal/agui"
	"idp-host/internal/transport"
)

// MaxResponse bounds a buffered (non-streaming) provider response.
const MaxResponse = 8 << 20

// StreamLimit is the bound of a streamed answer in bytes: the adapter's own setting, or MaxResponse when that is zero.
func StreamLimit(configured int) int {
	if configured > 0 {
		return configured
	}
	return MaxResponse
}

// ToolSpec is a tool offered to the model.
type ToolSpec struct {
	Name        string
	Description string
	Parameters  json.RawMessage // JSON Schema object
}

// Request is one model call.
type Request struct {
	Model           string
	Instructions    string // system prompt
	Messages        []agui.Message
	Tools           []ToolSpec
	Temperature     *float64
	MaxOutputTokens int
	// PresencePenalty, when set, is sent where the provider documents the field (OpenAI-compatible presence_penalty); the
	// other adapters leave it out. The host sets it only for the retry of a runaway answer (D60).
	PresencePenalty *float64
	// Effort is how hard the model should think (D63-2): "", EffortOff, EffortLow, EffortMedium or EffortHigh. Empty leaves the
	// provider's own default. An adapter that cannot map it ignores it. Only the OpenAI-compatible adapter maps it so far
	// (reasoning_effort); the question rewrite of D66 is its only user.
	Effort string
	// Watch, when set, is told by the adapter when the request was sent and when the answer first produced output (D54).
	// An adapter that streams calls it; one that does not leaves the timing absent. Never nil-checked by the caller.
	Watch *Stopwatch
}

// Thinking efforts of Request.Effort.
const (
	EffortOff    = "off"
	EffortLow    = "low"
	EffortMedium = "medium"
	EffortHigh   = "high"
)

// Delta is a streamed fragment.
type Delta struct {
	Text      string // assistant text
	Reasoning string // provider reasoning text, if it exposes any (never shown to tools)
}

// Usage is the token accounting of one call.
//
// PromptTokens is ALWAYS the whole prompt the model server holds for the request, cached part included (D54): D52's
// correction compares it with the estimate of the whole request. A provider that reports the uncached part apart
// (Anthropic's input_tokens) is added up by its adapter.
//
// The pointer fields are what the provider said about reuse and processing (D54); nil is "it did not say", never zero.
type Usage struct {
	PromptTokens     int `json:"promptTokens"`
	CompletionTokens int `json:"completionTokens"`
	// CachedTokens is the part of the prompt served from the provider's cache: OpenAI-compatible
	// usage.prompt_tokens_details.cached_tokens, Anthropic cache_read_input_tokens, Gemini cachedContentTokenCount.
	CachedTokens *int `json:"cachedTokens,omitempty"`
	// CacheCreationTokens is what the provider wrote to its cache for this request (Anthropic
	// cache_creation_input_tokens).
	CacheCreationTokens *int `json:"cacheCreationTokens,omitempty"`
	// Server is the model server's own account of the call (llama.cpp's timings object).
	Server *ServerTimings `json:"server,omitempty"`
}

// ServerTimings is what a llama.cpp style server reports in its timings object: prompt_n tokens processed (not reused),
// cache_n reused, predicted_n generated, and the milliseconds spent on the first and the last.
type ServerTimings struct {
	PromptN     *int     `json:"promptN,omitempty"`
	PromptMs    *float64 `json:"promptMs,omitempty"`
	CacheN      *int     `json:"cacheN,omitempty"`
	PredictedN  *int     `json:"predictedN,omitempty"`
	PredictedMs *float64 `json:"predictedMs,omitempty"`
}

// Any reports whether the server said anything.
func (s *ServerTimings) Any() bool {
	return s != nil && (s.PromptN != nil || s.PromptMs != nil || s.CacheN != nil || s.PredictedN != nil || s.PredictedMs != nil)
}

// Stop reasons.
const (
	StopEnd       = "end"        // the model finished its answer
	StopToolCalls = "tool_calls" // the model requested tool calls
	StopLength    = "length"     // output limit reached
	StopOther     = "other"
)

// Response is the completed call.
type Response struct {
	Message    agui.Message // role assistant; content and/or tool calls
	StopReason string
	Usage      Usage
	Reasoning  string
}

// Provider completes requests. Implementations must be safe for concurrent use.
type Provider interface {
	Name() string
	// Complete runs one call. onDelta (may be nil) receives streamed fragments; the returned message
	// is the whole assistant turn.
	Complete(ctx context.Context, req Request, onDelta func(Delta)) (*Response, error)
}

// Registry maps provider names to providers.
type Registry struct{ byName map[string]Provider }

// NewRegistry builds a registry.
func NewRegistry(providers ...Provider) *Registry {
	r := &Registry{byName: map[string]Provider{}}
	for _, p := range providers {
		r.byName[p.Name()] = p
	}
	return r
}

// Get returns the named provider.
func (r *Registry) Get(name string) (Provider, bool) {
	p, ok := r.byName[name]
	return p, ok
}

// Names lists the registered providers, sorted.
func (r *Registry) Names() []string {
	var out []string
	for n := range r.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// CodeContextLength is the code of a request the provider refused because it does not fit the model's context
// window (D40).
const CodeContextLength = "context_length"

// CodeOutputRunaway is the code of an answer that did not end before a limit of this host stopped it (D60): the output
// limit (the provider's finish reason length, with the context window not nearly full) or the bound of the stream.
const CodeOutputRunaway = "output_runaway"

// Error is a provider failure safe to record and show: a code, never the provider's raw body.
type Error struct {
	Code      string // transport, status, context_length, provider_* (D50), decode, schema, canceled
	Status    int    // HTTP status when Code is "status" or "context_length"
	Retryable bool
	// RetryAfter is how long the provider asked the caller to wait before the next request (the Retry-After header of
	// an HTTP answer, D45); zero when it did not ask.
	RetryAfter time.Duration
	// Idle marks a transport error that is a stream which sent nothing for the provider's idle timeout (D46).
	Idle bool
	// Detail says what the provider or the decoder said, bounded and cleaned (D50): the provider's type, code and
	// message for an error it reported inside a stream, or where an undecodable chunk was. It is provider-controlled
	// text: it goes to the host log only, never to an event, the failure message or the step row.
	Detail string
	// Fault says whose problem a non-retryable error is (D56): FaultCandidate for what belongs to the one provider or
	// model that was asked (its key, its model name, its quota, its size limit), FaultPolicy for a content refusal.
	// Empty is the request's own problem, or one this host cannot attribute: it would fail at every candidate.
	Fault string
}

// The values of Error.Fault.
const (
	FaultCandidate = "candidate" // the next candidate may answer: another key, another model name, another quota
	FaultPolicy    = "policy"    // the provider's content policy refused this content: it is not offered to another provider
)

// Error implements error.
func (e *Error) Error() string {
	switch e.Code {
	case "status":
		return fmt.Sprintf("model provider answered HTTP %d", e.Status)
	case CodeContextLength:
		return "the request does not fit the model's context window"
	case CodeOutputRunaway:
		return "the model's answer did not end: " + CodeOutputRunaway
	case "transport":
		if e.Idle {
			return "model provider error: the stream stopped sending data"
		}
	}
	return "model provider error: " + e.Code
}

// The codes of an error the provider reported inside a stream that began with HTTP 200 (D50). The first three are
// transient and retried under D45's rule; ProviderError is every other class.
const (
	CodeProviderOverloaded = "provider_overloaded" // overloaded_error, 503, 529, UNAVAILABLE
	CodeProviderRateLimit  = "provider_rate_limit" // rate_limit_error, 429, RESOURCE_EXHAUSTED
	CodeProviderServer     = "provider_server"     // api_error, server_error, 5xx, INTERNAL, timeouts
	CodeProviderError      = "provider_error"      // any other class: not retried
)

// IsProviderStreamCode reports whether code is one of the D50 codes.
func IsProviderStreamCode(code string) bool {
	switch code {
	case CodeProviderOverloaded, CodeProviderRateLimit, CodeProviderServer, CodeProviderError:
		return true
	}
	return false
}

// CheckEndpoint validates a provider base URL: http(s), a host, no credentials, query or fragment;
// plain http only to loopback addresses unless allowPlainHTTP (development topologies where the
// model runs on a private network).
func CheckEndpoint(raw string, allowPlainHTTP bool) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("invalid endpoint URL")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("endpoint must be an http(s) URL without credentials, query or fragment")
	}
	if u.Scheme == "http" && !allowPlainHTTP {
		host := u.Hostname()
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return "", errors.New("plain http endpoints must be loopback addresses")
		}
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}

// NewHTTPClient builds the client every provider uses: no proxy from the environment, no
// redirects, bounded header wait; the overall deadline comes from the request context.
func NewHTTPClient(headerTimeout time.Duration, caFile string) (*http.Client, error) {
	tlsCfg, err := transport.ClientTLS(caFile, "", "")
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       tlsCfg,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: headerTimeout,
		DisableCompression:    true,
	}
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}, nil
}

// Classify turns an HTTP status into a provider error.
func Classify(status int) *Error {
	e := &Error{Code: "status", Status: status, Retryable: status == 408 || status == 429 || status >= 500}
	switch status {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusNotFound, http.StatusRequestEntityTooLarge:
		// that provider's key, its payment, its permission for the model, its model name or route, its body limit (D56)
		e.Fault = FaultCandidate
	}
	return e
}

// ClassifyHTTP is ClassifyBody for a response whose headers are at hand: the error also carries the Retry-After the
// provider sent with a retryable status (D45), read at now.
func ClassifyHTTP(status int, header http.Header, body []byte, now time.Time) *Error {
	e := ClassifyBody(status, body)
	if e.Retryable {
		e.RetryAfter, _ = ParseRetryAfter(header.Get("Retry-After"), now)
	}
	return e
}

// maxRetryAfter bounds what a Retry-After header can say, so that a hostile or broken value cannot overflow a duration.
const maxRetryAfter = 24 * time.Hour

// ParseRetryAfter reads a Retry-After header value (RFC 9110: a number of seconds or an HTTP date) as the time to wait
// from now. A date in the past is no wait; a value that is neither is ignored (false).
func ParseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseUint(v, 10, 63); err == nil {
		return min(time.Duration(min(secs, uint64(maxRetryAfter/time.Second)))*time.Second, maxRetryAfter), true
	}
	if at, err := http.ParseTime(v); err == nil {
		return min(max(at.Sub(now), 0), maxRetryAfter), true
	}
	return 0, false
}

// IdleWatch bounds the silence of a response body (D46): when no byte arrives for the idle timeout, it cancels the
// request, so a model that stopped answering in the middle of a stream cannot hold a run (and its lease) for ever.
type IdleWatch struct {
	timer *time.Timer
	idle  time.Duration
	fired atomic.Bool
}

// NewIdleWatch starts the watch; cancel is called once, when the silence exceeds idle. A zero idle watches nothing.
func NewIdleWatch(idle time.Duration, cancel context.CancelFunc) *IdleWatch {
	w := &IdleWatch{idle: idle}
	if idle > 0 {
		w.timer = time.AfterFunc(idle, func() { w.fired.Store(true); cancel() })
	}
	return w
}

// Wrap returns r with every successful read restarting the idle timer.
func (w *IdleWatch) Wrap(r io.Reader) io.Reader { return idleReader{w, r} }

// Stop ends the watch (the response was read to its end or abandoned).
func (w *IdleWatch) Stop() {
	if w.timer != nil {
		w.timer.Stop()
	}
}

// Fired reports whether the watch cancelled the request.
func (w *IdleWatch) Fired() bool { return w.fired.Load() }

// StreamError is the error of a body read or request that failed: canceled when the caller's context ended, an
// idle transport error when the watch fired, a plain retryable transport error otherwise.
func (w *IdleWatch) StreamError(ctx context.Context) *Error {
	if ctx.Err() != nil {
		return &Error{Code: "canceled"}
	}
	return &Error{Code: "transport", Retryable: true, Idle: w.Fired()}
}

type idleReader struct {
	w *IdleWatch
	r io.Reader
}

func (i idleReader) Read(p []byte) (int, error) {
	n, err := i.r.Read(p)
	if n > 0 && i.w.timer != nil {
		i.w.timer.Reset(i.w.idle)
	}
	return n, err
}

// contextMarkers is how model servers name a request that does not fit the model's context window:
//   - OpenAI-compatible: OpenAI's error code, the message of OpenAI and vLLM, llama.cpp's error type and its message, and
//     the message of the Strata server the platform is tried with ("prompt (N tokens) + max tokens (M) exceeds the context (W)");
//   - Anthropic (HTTP 400, error type invalid_request_error): "prompt is too long: N tokens > M maximum", and, for models
//     that count the output limit too, "input length and `max_tokens` exceed context limit: N + M > W" (D52);
//   - Gemini (HTTP 400, status INVALID_ARGUMENT): "The input token count (N) exceeds the maximum number of tokens allowed (M)" (D52).
var contextMarkers = []string{"context_length_exceeded", "maximum context length", "exceed_context_size_error", "exceeds the available context size", "exceeds the context", "prompt is too long",
	"exceed context limit", "exceeds the maximum number of tokens allowed"}

// ClassifyBody is Classify for a refusal whose body was read (D40): a 400 that says the request does not fit the
// model's context window gets its own code, because the caller can do something about that one. Only the code
// leaves this function; the body is neither kept nor shown.
func ClassifyBody(status int, body []byte) *Error {
	text := strings.ToLower(string(body))
	if status == http.StatusBadRequest {
		for _, marker := range contextMarkers {
			if strings.Contains(text, marker) {
				return &Error{Code: CodeContextLength, Status: status}
			}
		}
	}
	e := Classify(status)
	switch status {
	case http.StatusBadRequest, http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests:
		if containsAny(text, quotaMarkers) {
			// the account has no more to spend: waiting does not help (a 429 of this kind is not the rate limit), and
			// another candidate has its own account (D56)
			e.Retryable, e.Fault = false, FaultCandidate
		}
	}
	if status == http.StatusBadRequest && e.Fault == "" && containsAny(text, policyMarkers) {
		e.Fault = FaultPolicy
	}
	return e
}

// quotaMarkers are how providers say the account is out of quota or credit: OpenAI's error code (also in the 429 body that
// looks like a rate limit), its billing limit, and Anthropic's 400 message. Only the code and the status are read for the
// class; the body is not kept.
var quotaMarkers = []string{"insufficient_quota", "exceeded your current quota", "billing_hard_limit_reached", "credit balance is too low"}

// policyMarkers are how providers say their content policy refused the request: the error code of OpenAI and Azure.
var policyMarkers = []string{"content_policy_violation", "content_filter", "responsibleaipolicyviolation"}
