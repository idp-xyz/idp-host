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
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
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

// Error is a provider failure safe to record and show: a code, never the provider's raw body.
type Error struct {
	Code      string // transport, status, decode, schema, canceled
	Status    int    // HTTP status when Code is "status"
	Retryable bool
}

// Error implements error.
func (e *Error) Error() string {
	if e.Code == "status" {
		return fmt.Sprintf("model provider answered HTTP %d", e.Status)
	}
	return "model provider error: " + e.Code
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
