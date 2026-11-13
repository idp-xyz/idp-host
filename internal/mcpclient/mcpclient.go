// Package mcpclient is the HOST's MCP client toward the MCP Infrastructure gateway (ADR-0001 D1):
// MCP 2026-07-28 modern, stateless Streamable HTTP - one POST per request with the _meta envelope
// and the Mcp-* headers, a JSON or single-message SSE answer. Every call carries the credential of
// the caller the run acts for, so the gateway authorizes and audits the real principal.
package mcpclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/otel/propagation"

	"idp-host/internal/observability"
	"idp-host/internal/transport"
)

// Protocol constants (MCP 2026-07-28).
const (
	ProtocolVersion        = "2026-07-28"
	MetaProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
	MetaClientInfo         = "io.modelcontextprotocol/clientInfo"
	MetaClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
	MetaServerInfo         = "io.modelcontextprotocol/serverInfo"
	HeaderProtocolVersion  = "MCP-Protocol-Version"
	HeaderMethod           = "Mcp-Method"
	HeaderName             = "Mcp-Name"
)

// MaxResponse bounds a buffered response.
const MaxResponse = 8 << 20

// ClientInfo is what the HOST announces as clientInfo.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// NewHTTPClient returns the client for gateway calls: no environment proxy, no redirects, bounded
// header wait, optional extra CA and client certificate.
func NewHTTPClient(timeout time.Duration, tlsCfg *tls.Config) *http.Client {
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: timeout,
		DisableCompression:    true,
	}
	if tlsCfg != nil {
		tr.TLSClientConfig = tlsCfg.Clone()
	}
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// CheckEndpoint validates a gateway MCP endpoint: http(s) URL without credentials, query or fragment.
func CheckEndpoint(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("gateway endpoint must be an http(s) URL without credentials, query or fragment")
	}
	return u.String(), nil
}

// Client sends MCP requests.
type Client struct {
	HTTP *http.Client
	Info ClientInfo
}

// RPCError is a JSON-RPC error answered by the server.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
	// HTTPStatus is the status the error came with (a gateway answers an unacceptable credential with 401 and a
	// JSON-RPC body).
	HTTPStatus int `json:"-"`
}

// Error implements error.
func (e *RPCError) Error() string { return fmt.Sprintf("MCP error %d: %s", e.Code, e.Message) }

// HTTPError is a non-200 answer without a JSON-RPC body (401, 403, 5xx).
type HTTPError struct{ Status int }

// Error implements error.
func (e *HTTPError) Error() string { return fmt.Sprintf("gateway HTTP %d", e.Status) }

// Unauthorized reports whether the gateway refused the credential (HTTP 401), with or without a JSON-RPC body.
func Unauthorized(err error) bool {
	var he *HTTPError
	var re *RPCError
	return errors.As(err, &he) && he.Status == http.StatusUnauthorized || errors.As(err, &re) && re.HTTPStatus == http.StatusUnauthorized
}

// NamedParam maps methods to the param carried in Mcp-Name.
var NamedParam = map[string]string{"tools/call": "name", "prompts/get": "name", "resources/read": "uri"}

func (c *Client) meta() map[string]any {
	return map[string]any{MetaProtocolVersion: ProtocolVersion, MetaClientInfo: c.Info, MetaClientCapabilities: map[string]any{}}
}

// Call sends one request with the caller's bearer credential and returns its result object.
func (c *Client) Call(ctx context.Context, endpoint, bearer, method string, params map[string]any) (json.RawMessage, error) {
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = c.meta()
	name := ""
	if key, ok := NamedParam[method]; ok {
		name, _ = params[key].(string)
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set(HeaderProtocolVersion, ProtocolVersion)
	req.Header.Set(HeaderMethod, method)
	if name != "" {
		req.Header.Set(HeaderName, name)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	observability.Propagator.Inject(ctx, propagation.HeaderCarrier(req.Header))
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gateway unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, err := transport.ReadBounded(resp.Body, MaxResponse)
	if err != nil {
		return nil, fmt.Errorf("gateway response: %w", err)
	}
	msg, err := transport.SingleMessage(resp.Header.Get("Content-Type"), raw)
	if err != nil {
		if resp.StatusCode != http.StatusOK {
			return nil, &HTTPError{Status: resp.StatusCode}
		}
		return nil, fmt.Errorf("gateway %s: %w", method, err)
	}
	var out struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   *RPCError       `json:"error"`
	}
	if err := json.Unmarshal(msg, &out); err != nil || out.JSONRPC != "2.0" {
		return nil, errors.New("malformed JSON-RPC response")
	}
	if out.Error != nil {
		out.Error.HTTPStatus = resp.StatusCode
		return nil, out.Error
	}
	if resp.StatusCode != http.StatusOK || len(out.Result) == 0 || out.Result[0] != '{' {
		return nil, &HTTPError{Status: resp.StatusCode}
	}
	return out.Result, nil
}

// Discovery is what server/discover reports.
type Discovery struct {
	SupportedVersions []string        `json:"supportedVersions"`
	Capabilities      map[string]any  `json:"capabilities"`
	ResultType        string          `json:"resultType"`
	Meta              json.RawMessage `json:"_meta"`
}

// Discover calls server/discover and requires a modern server that offers tools.
func (c *Client) Discover(ctx context.Context, endpoint, bearer string) (*Discovery, error) {
	raw, err := c.Call(ctx, endpoint, bearer, "server/discover", nil)
	if err != nil {
		return nil, err
	}
	var d Discovery
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, errors.New("malformed discovery result")
	}
	ok := false
	for _, v := range d.SupportedVersions {
		if v == ProtocolVersion {
			ok = true
		}
	}
	if !ok {
		return nil, fmt.Errorf("gateway does not support MCP %s (supports %v)", ProtocolVersion, d.SupportedVersions)
	}
	if _, ok := d.Capabilities["tools"]; !ok {
		return nil, errors.New("gateway offers no tools")
	}
	return &d, nil
}

// Tool is a tools/list entry as the gateway exposes it to the caller.
type Tool struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ListTools pages through tools/list.
func (c *Client) ListTools(ctx context.Context, endpoint, bearer string) ([]Tool, error) {
	var all []Tool
	cursor := ""
	for page := 0; page < 20; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.Call(ctx, endpoint, bearer, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var res struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &res); err != nil || res.Tools == nil {
			return nil, errors.New("tools/list result has no tools array")
		}
		for _, t := range res.Tools {
			if t.Name == "" {
				return nil, errors.New("tools/list entry without a name")
			}
		}
		all = append(all, res.Tools...)
		if len(all) > 1000 {
			return nil, errors.New("too many tools")
		}
		if res.NextCursor == "" {
			return all, nil
		}
		cursor = res.NextCursor
	}
	return nil, errors.New("tools/list pagination exceeds 20 pages")
}

// ContentBlock is one block of a tool result.
type ContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	URI      string `json:"uri,omitempty"`
}

// CallResult is a tools/call result.
type CallResult struct {
	Content           []ContentBlock  `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

// Text renders the result for the model: text blocks concatenated, non-text blocks named, and the
// structured content when there is no text. Bounded to limit bytes.
func (r *CallResult) Text(limit int) string {
	var sb strings.Builder
	for _, b := range r.Content {
		switch b.Type {
		case "text":
			sb.WriteString(b.Text)
		case "image", "audio":
			fmt.Fprintf(&sb, "[%s %s]", b.Type, b.MimeType)
		case "resource_link", "resource":
			fmt.Fprintf(&sb, "[%s %s]", b.Type, b.URI)
		default:
			fmt.Fprintf(&sb, "[%s]", b.Type)
		}
	}
	if sb.Len() == 0 && len(r.StructuredContent) > 0 {
		sb.Write(r.StructuredContent)
	}
	s := sb.String()
	if limit > 0 && len(s) > limit {
		s = s[:limit] + "\n[truncated]"
	}
	return s
}

// CallTool calls one tool with JSON arguments.
func (c *Client) CallTool(ctx context.Context, endpoint, bearer, name string, args json.RawMessage) (*CallResult, error) {
	params := map[string]any{"name": name}
	if len(args) > 0 && string(args) != "null" {
		params["arguments"] = args
	} else {
		params["arguments"] = map[string]any{}
	}
	raw, err := c.Call(ctx, endpoint, bearer, "tools/call", params)
	if err != nil {
		return nil, err
	}
	var res CallResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, errors.New("malformed tools/call result")
	}
	if res.Content == nil {
		res.Content = []ContentBlock{}
	}
	return &res, nil
}
