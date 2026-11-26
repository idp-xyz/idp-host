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
	"unicode/utf8"

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
	observability.InjectUnlessDenied(ctx, req.Header) // the gateway is ours; a call to an external tool is made without it (D57)
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
	// OutputSchema is the JSON Schema the tool declares for the structuredContent of its results (MCP 2025-06-18 and later), as the
	// gateway listed it. The HOST pins it for the turn and renders a web tool's result from the structure only when the structure is
	// valid against it (D71). Nothing else reads it.
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	// Annotations are the gateway's hints about the tool as it lists it now (MCP tool annotations). The HOST reads
	// readOnlyHint and idempotentHint to decide whether a call interrupted mid-way may be sent again (D47); a tool that
	// lists none is treated as neither.
	Annotations ToolAnnotations `json:"annotations,omitzero"`
	// External is the gateway's statement that the tool sends the caller's content outside the enterprise: the tool
	// object's _meta carries ExternalMetaKey with the JSON boolean true (D51). It is read from that one key only, and only
	// when it is exactly true; a missing key, any other value and every other key of _meta count for nothing, since the
	// other keys come from the upstream. ListTools sets it; a Tool decoded any other way is not external.
	External bool `json:"-"`
	// Meta is the raw _meta as the gateway sent it. ListTools reads ExternalMetaKey from it and clears it, so that nothing
	// of an upstream's own _meta travels further.
	Meta json.RawMessage `json:"_meta,omitempty"`
}

// ExternalMetaKey is the key of a tool's _meta with which the gateway marks a tool of an external upstream (idp-mcp
// ADR-0001 D50 to D53; HOST ADR-0001 D51).
const ExternalMetaKey = "io.idp.telos/external"

// externalMarker reports whether a tool's _meta marks it external: an object whose ExternalMetaKey is the JSON boolean
// true. Anything else, including a _meta that is not an object, is false.
func externalMarker(meta json.RawMessage) bool {
	var m map[string]json.RawMessage
	if len(meta) == 0 || json.Unmarshal(meta, &m) != nil {
		return false
	}
	return string(bytes.TrimSpace(m[ExternalMetaKey])) == "true"
}

// ToolAnnotations are the hints of a tool the HOST uses; an absent hint is false, as in the MCP specification.
type ToolAnnotations struct {
	ReadOnlyHint   bool `json:"readOnlyHint,omitempty"`
	IdempotentHint bool `json:"idempotentHint,omitempty"`
}

// SafeToRepeat reports whether the tool says that calling it again with the same arguments does no harm: it only reads,
// or it is idempotent (D47). It is the gateway's statement, pinned in its catalog, not a measurement.
func (t Tool) SafeToRepeat() bool { return t.Annotations.ReadOnlyHint || t.Annotations.IdempotentHint }

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
		for i := range res.Tools {
			t := &res.Tools[i]
			if t.Name == "" {
				return nil, errors.New("tools/list entry without a name")
			}
			t.External, t.Meta = externalMarker(t.Meta), nil
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
	MimeType string `json:"mimeType,omitempty"` // image, audio
	URI      string `json:"uri,omitempty"`      // resource_link
	Name     string `json:"name,omitempty"`     // resource_link
	// Resource is the embedded resource of a "resource" block (D62): its uri, type and text sit inside this object.
	Resource *EmbeddedResource `json:"resource,omitempty"`
}

// EmbeddedResource is what a "resource" block carries: text, or a base64 blob. The blob is not kept: only how many bytes it
// holds, because it is never shown to a model (D62).
type EmbeddedResource struct {
	URI       string
	MimeType  string
	Text      string
	IsBlob    bool
	BlobBytes int
}

// UnmarshalJSON reads uri, mimeType, text and blob; the blob's base64 is measured and dropped.
func (e *EmbeddedResource) UnmarshalJSON(b []byte) error {
	var raw struct {
		URI      string  `json:"uri"`
		MimeType string  `json:"mimeType"`
		Text     *string `json:"text"`
		Blob     *string `json:"blob"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*e = EmbeddedResource{URI: raw.URI, MimeType: raw.MimeType}
	switch {
	case raw.Text != nil:
		e.Text = *raw.Text
	case raw.Blob != nil:
		e.IsBlob = true
		n := len(*raw.Blob)
		e.BlobBytes = n / 4 * 3
		for i := n - 1; i >= 0 && i >= n-2 && (*raw.Blob)[i] == '='; i-- {
			e.BlobBytes--
		}
	}
	return nil
}

// CallResult is a tools/call result.
type CallResult struct {
	Content           []ContentBlock  `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

// capped is a string builder that keeps at most max bytes (plus what finishing a rune needs) and counts all it was given, so
// that a large resource text is cut as it is rendered and not copied whole (D62).
type capped struct {
	sb    strings.Builder
	max   int // 0: keep everything
	total int
}

func (c *capped) WriteString(s string) {
	c.total += len(s)
	if c.max <= 0 {
		c.sb.WriteString(s)
		return
	}
	if room := c.max + utf8.UTFMax - c.sb.Len(); room > 0 {
		if room > len(s) {
			room = len(s)
		}
		c.sb.WriteString(s[:room])
	}
}

// Text renders the result for the model: text blocks concatenated, non-text blocks named, and the structured content when
// there is no text. An embedded text resource is its header line "[resource <uri> <mimeType>]" and its text; a blob is only
// named with its size (D62). Bounded to limit bytes (D40) as a whole.
func (r *CallResult) Text(limit int) string {
	c := r.render(limit)
	s := c.sb.String()
	if limit > 0 && c.total > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + fmt.Sprintf("\n[truncated: %d of %d bytes shown; narrow the output (head, grep, a summary) to see the part you need]", cut, c.total)
	}
	return s
}

// TextLen is how many bytes Text would give without a limit, which is what the limit is compared with: whether a limited Text is cut.
func (r *CallResult) TextLen() int { return r.render(1).total }

func (r *CallResult) render(limit int) *capped {
	c := &capped{max: limit}
	for _, b := range r.Content {
		switch b.Type {
		case "text":
			c.WriteString(b.Text)
		case "image", "audio":
			c.WriteString(fmt.Sprintf("[%s %s]", b.Type, b.MimeType))
		case "resource_link":
			if b.Name != "" {
				c.WriteString(fmt.Sprintf("[resource_link %s %s]", b.URI, b.Name))
			} else {
				c.WriteString(fmt.Sprintf("[resource_link %s]", b.URI))
			}
		case "resource":
			switch e := b.Resource; {
			case e == nil:
				c.WriteString("[resource]")
			case e.IsBlob:
				c.WriteString(fmt.Sprintf("[resource %s %s, %d bytes, binary content not shown]", e.URI, e.MimeType, e.BlobBytes))
			default:
				c.WriteString(fmt.Sprintf("[resource %s %s]\n", e.URI, e.MimeType))
				c.WriteString(e.Text)
			}
		default:
			c.WriteString(fmt.Sprintf("[%s]", b.Type))
		}
	}
	if c.total == 0 && len(r.StructuredContent) > 0 {
		c.WriteString(string(r.StructuredContent))
	}
	return c
}

// CallTool calls one tool with JSON arguments.
func (c *Client) CallTool(ctx context.Context, endpoint, bearer, name string, args json.RawMessage) (res *CallResult, err error) {
	ctx, end := observability.StartToolCall(ctx, name) // D57: a span per call when the run's turn is traced
	defer func() { end(toolOutcome(res, err)) }()
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
	var out CallResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, errors.New("malformed tools/call result")
	}
	if out.Content == nil {
		out.Content = []ContentBlock{}
	}
	return &out, nil
}

// toolOutcome is the outcome word of a tools/call for its span: what happened, never what was said.
func toolOutcome(res *CallResult, err error) string {
	var rpc *RPCError
	switch {
	case err == nil && res != nil && res.IsError:
		return "tool_error"
	case err == nil:
		return "ok"
	case Unauthorized(err):
		return "unauthorized"
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		return "interrupted"
	case errors.As(err, &rpc):
		return "rpc_error"
	}
	return "error"
}
