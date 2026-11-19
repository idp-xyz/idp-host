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
}

// Delta is a streamed fragment.
type Delta struct {
	Text      string // assistant text
	Reasoning string // provider reasoning text, if it exposes any (never shown to tools)
}

// Usage is the token accounting of one call.
type Usage struct {
	PromptTokens     int `json:"promptTokens"`
	CompletionTokens int `json:"completionTokens"`
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
}

// Error implements error.
func (e *Error) Error() string {
	switch e.Code {
	case "status":
		return fmt.Sprintf("model provider answered HTTP %d", e.Status)
	case CodeContextLength:
		return "the request does not fit the model's context window"
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
	return &Error{Code: "status", Status: status, Retryable: status == 408 || status == 429 || status >= 500}
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

// contextMarkers is how OpenAI-compatible servers name a request that does not fit the model's context window:
// OpenAI's error code, the message of OpenAI and vLLM, llama.cpp's error type and its message, and the message of
// the Strata server the platform is tried with ("prompt (N tokens) + max tokens (M) exceeds the context (W)").
var contextMarkers = []string{"context_length_exceeded", "maximum context length", "exceed_context_size_error", "exceeds the available context size", "exceeds the context", "prompt is too long"}

// ClassifyBody is Classify for a refusal whose body was read (D40): a 400 that says the request does not fit the
// model's context window gets its own code, because the caller can do something about that one. Only the code
// leaves this function; the body is neither kept nor shown.
func ClassifyBody(status int, body []byte) *Error {
	if status == http.StatusBadRequest {
		text := strings.ToLower(string(body))
		for _, marker := range contextMarkers {
			if strings.Contains(text, marker) {
				return &Error{Code: CodeContextLength, Status: status}
			}
		}
	}
	return Classify(status)
}
