package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
	if r.Text(0) != `{"k":1}` || r.Text(3) != `{"k`+"\n[truncated]" {
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
