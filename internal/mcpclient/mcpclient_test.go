package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"idp-host/tests/fixtures/fakemcp"
)

func TestDiscoverListAndCall(t *testing.T) {
	for _, stream := range []bool{false, true} {
		fake := fakemcp.New()
		fake.StreamResults = stream
		fake.RequireBearer = "tok"
		srv := httptest.NewServer(fake.Handler())
		c := &Client{HTTP: NewHTTPClient(5*time.Second, nil), Info: ClientInfo{Name: "idp-host", Version: "test"}}
		ctx := context.Background()
		if _, err := c.Discover(ctx, srv.URL+"/mcp", "wrong"); err == nil {
			t.Fatal("bad bearer accepted")
		} else if he := new(HTTPError); !errors.As(err, &he) || he.Status != 401 {
			t.Fatalf("expected HTTP 401, got %v", err)
		}
		d, err := c.Discover(ctx, srv.URL+"/mcp", "tok")
		if err != nil || d.ResultType != "complete" {
			t.Fatal(err, d)
		}
		tools, err := c.ListTools(ctx, srv.URL+"/mcp", "tok")
		if err != nil || len(tools) != 4 || tools[0].Name != "sum" || len(tools[0].InputSchema) == 0 {
			t.Fatal(err, tools)
		}
		res, err := c.CallTool(ctx, srv.URL+"/mcp", "tok", "sum", json.RawMessage(`{"a":1,"b":2}`))
		if err != nil || res.IsError || res.Text(0) != "3" {
			t.Fatalf("%+v %v", res, err)
		}
		res, err = c.CallTool(ctx, srv.URL+"/mcp", "tok", "fail", nil)
		if err != nil || !res.IsError || res.Text(0) != "it failed" {
			t.Fatalf("%+v %v", res, err)
		}
		_, err = c.CallTool(ctx, srv.URL+"/mcp", "tok", "nope", nil)
		var rpc *RPCError
		if !errors.As(err, &rpc) || rpc.Code != -32602 {
			t.Fatalf("%v", err)
		}
		if calls := fake.Calls(); len(calls) != 3 || calls[0].Bearer != "tok" {
			t.Fatalf("%+v", calls)
		}
		srv.Close()
	}
}

func TestResultText(t *testing.T) {
	r := &CallResult{Content: []ContentBlock{{Type: "text", Text: "a"}, {Type: "image", MimeType: "image/png"}, {Type: "resource_link", URI: "x://y"}}}
	if r.Text(0) != "a[image image/png][resource_link x://y]" {
		t.Fatal(r.Text(0))
	}
	r = &CallResult{StructuredContent: json.RawMessage(`{"k":1}`)}
	if r.Text(0) != `{"k":1}` || !strings.HasPrefix(r.Text(3), `{"k`+"\n[truncated: 3 of 7 bytes shown") {
		t.Fatal(r.Text(0), r.Text(3))
	}
	if _, err := CheckEndpoint("https://u:p@gw/mcp"); err == nil {
		t.Fatal("credentials in endpoint accepted")
	}
}

// The gateway answers an unacceptable credential with 401 and a JSON-RPC body; both shapes count as a refusal (D38).
func TestUnauthorizedIsRecognized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32600,"message":"Unauthorized"}}`)
	}))
	defer srv.Close()
	c := &Client{HTTP: NewHTTPClient(5*time.Second, nil), Info: ClientInfo{Name: "t", Version: "1"}}
	_, err := c.CallTool(context.Background(), srv.URL, "tok", "x", json.RawMessage(`{}`))
	if !Unauthorized(err) {
		t.Fatalf("not recognized: %v", err)
	}
	if Unauthorized(&RPCError{Code: -32600, Message: "Unauthorized", HTTPStatus: 200}) || Unauthorized(errors.New("x")) || !Unauthorized(&HTTPError{Status: 401}) {
		t.Fatal("misclassified")
	}
}

// D47: ListTools reads readOnlyHint and idempotentHint; a tool that lists neither, or lists them false, is not safe to
// repeat, and unknown annotations are ignored.
func TestListToolsReadsTheAnnotationsThatSayACallMayBeRepeated(t *testing.T) {
	fake := fakemcp.New()
	fake.Tools[0].Annotations = map[string]any{"readOnlyHint": true, "title": "Sum", "openWorldHint": true}
	fake.Tools[1].Annotations = map[string]any{"idempotentHint": true, "destructiveHint": true}
	fake.Tools[2].Annotations = map[string]any{"readOnlyHint": false, "idempotentHint": false}
	srv := httptest.NewServer(fake.Handler())
	defer srv.Close()
	c := &Client{HTTP: NewHTTPClient(5*time.Second, nil), Info: ClientInfo{Name: "idp-host", Version: "test"}}
	tools, err := c.ListTools(context.Background(), srv.URL+"/mcp", "")
	if err != nil || len(tools) != 4 {
		t.Fatal(err, tools)
	}
	for i, want := range []bool{true, true, false, false} { // sum, echo, fail, slow (no annotations)
		if got := tools[i].SafeToRepeat(); got != want {
			t.Errorf("%s: SafeToRepeat %v, want %v (%+v)", tools[i].Name, got, want, tools[i].Annotations)
		}
	}
}

// D51: ListTools reads one thing of a tool's _meta, the key the gateway sets, and only when it is the JSON boolean true;
// the rest of _meta (the upstream's own) is never kept.
func TestListToolsReadsTheExternalMarkerAndNothingElseOfMeta(t *testing.T) {
	fake := fakemcp.New() // sum, echo, fail, slow
	fake.Tools[0].Meta = map[string]any{ExternalMetaKey: true, "vendor/secret": "x"}
	fake.Tools[1].Meta = map[string]any{ExternalMetaKey: "true"}
	fake.Tools[2].Meta = map[string]any{ExternalMetaKey: 1}
	fake.Tools[3].Meta = map[string]any{ExternalMetaKey: map[string]any{"v": true}}
	fake.Tools = append(fake.Tools,
		fakemcp.Tool{Name: "n", InputSchema: `{}`, Meta: map[string]any{ExternalMetaKey: nil}, Handle: fake.Tools[0].Handle},
		fakemcp.Tool{Name: "f", InputSchema: `{}`, Meta: map[string]any{ExternalMetaKey: false}, Handle: fake.Tools[0].Handle},
		fakemcp.Tool{Name: "other", InputSchema: `{}`, Meta: map[string]any{"io.idp.telos/internal": true, "external": true}, Handle: fake.Tools[0].Handle},
		fakemcp.Tool{Name: "none", InputSchema: `{}`, Handle: fake.Tools[0].Handle})
	srv := httptest.NewServer(fake.Handler())
	defer srv.Close()
	c := &Client{HTTP: NewHTTPClient(5*time.Second, nil), Info: ClientInfo{Name: "idp-host", Version: "test"}}
	tools, err := c.ListTools(context.Background(), srv.URL+"/mcp", "")
	if err != nil || len(tools) != 8 {
		t.Fatal(err, tools)
	}
	for i, want := range []bool{true, false, false, false, false, false, false, false} {
		if tools[i].External != want {
			t.Errorf("%s: External %v, want %v", tools[i].Name, tools[i].External, want)
		}
		if tools[i].Meta != nil {
			t.Errorf("%s: the upstream's _meta is kept: %s", tools[i].Name, tools[i].Meta)
		}
	}
	for raw, want := range map[string]bool{`{"io.idp.telos/external":true}`: true, `{"io.idp.telos/external": true }`: true, `"io.idp.telos/external"`: false, `[true]`: false, `true`: false, ``: false, `{}`: false} {
		if got := externalMarker(json.RawMessage(raw)); got != want {
			t.Errorf("%q: %v", raw, got)
		}
	}
}

// D62: an embedded resource is read from its nested object; a blob is only measured; the limit cuts across it.
func TestResultTextOfEmbeddedResources(t *testing.T) {
	var r CallResult
	body := `{"content":[{"type":"text","text":"downloaded (SHA abc)"},
	  {"type":"resource","resource":{"uri":"repo://a/b/README.md","mimeType":"text/markdown","text":"# Title\nbody"}},
	  {"type":"resource","resource":{"uri":"repo://a/b/x.bin","mimeType":"application/octet-stream","blob":"AAECAwQ="}},
	  {"type":"resource_link","uri":"repo://a/c","name":"c"},{"type":"resource_link","uri":"repo://a/d"}]}`
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatal(err)
	}
	want := "downloaded (SHA abc)[resource repo://a/b/README.md text/markdown]\n# Title\nbody" +
		"[resource repo://a/b/x.bin application/octet-stream, 5 bytes, binary content not shown]" +
		"[resource_link repo://a/c c][resource_link repo://a/d]"
	if got := r.Text(0); got != want {
		t.Fatalf("%q", got)
	}
	if strings.Contains(r.Text(0), "AAECAwQ") {
		t.Error("the blob's base64 is shown")
	}
	// the limit counts the header and the text as one, cuts at a rune boundary and says how much there was
	r = CallResult{Content: []ContentBlock{{Type: "resource", Resource: &EmbeddedResource{URI: "u://1", MimeType: "text/plain", Text: strings.Repeat("é", 50)}}}}
	total := len("[resource u://1 text/plain]\n") + 100
	got := r.Text(40)
	if !strings.HasPrefix(got, "[resource u://1 text/plain]\n") || !strings.Contains(got, fmt.Sprintf(" of %d bytes", total)) || !utf8.ValidString(got) {
		t.Fatalf("%q", got)
	}
	if body := got[:strings.Index(got, "\n[truncated")]; len(body) > 40 {
		t.Errorf("%d bytes shown, limit 40", len(body))
	}
	// a resource without its object, and a blob without padding
	r = CallResult{Content: []ContentBlock{{Type: "resource"}}}
	if r.Text(0) != "[resource]" {
		t.Error(r.Text(0))
	}
	var e EmbeddedResource
	_ = json.Unmarshal([]byte(`{"uri":"u","blob":"AAEC"}`), &e)
	if !e.IsBlob || e.BlobBytes != 3 {
		t.Errorf("%+v", e)
	}
}

// D71: a full-size read of idp-web is a part of a page of 12 KiB that is in the result twice (the text content and the structured
// text). Even where every byte is a control character, which JSON writes as six, the result is under 200 KiB, far under what the
// HOST reads (MaxResponse, 8 MiB) and under the gateway's cap on an external answer (1 MiB by default, idp-mcp
// DefaultExternalMaxResult): nothing rejects it, and the limit of the model's text is D40's, applied to what the HOST renders.
func TestAFullSizeReadOfIdpWebIsAccepted(t *testing.T) {
	page := strings.Repeat("\x01", 12<<10)
	fake := fakemcp.New()
	fake.Tools = []fakemcp.Tool{{Name: "web_fetch", InputSchema: `{"type":"object"}`, Handle: func(map[string]any) (string, bool) { return "", false },
		HandleAs: func(string, map[string]any) (string, any, bool) {
			return "[1] t\n\n" + page, map[string]any{"citations": []any{map[string]any{"n": 1, "text": page}}}, false
		}}}
	srv := httptest.NewServer(fake.Handler())
	defer srv.Close()
	c := &Client{HTTP: NewHTTPClient(5*time.Second, nil), Info: ClientInfo{Name: "idp-host", Version: "test"}}
	res, err := c.CallTool(context.Background(), srv.URL+"/mcp", "", "web_fetch", nil)
	if err != nil || res.IsError || len(res.StructuredContent) < 12<<10 || len(res.Text(0)) < 12<<10 {
		t.Fatalf("%v %d", err, len(res.StructuredContent))
	}
	if 2*(12<<10)*6 >= MaxResponse || 2*(12<<10)*6 >= 1<<20 {
		t.Error("the worst case of a full-size read is over a cap")
	}
}
