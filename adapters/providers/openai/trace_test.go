package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"idp-host/internal/agui"
	"idp-host/internal/model"
	"idp-host/internal/observability"
)

// D57: the provider is sent the trace context only when the harness marked the call: a request that carries a span but no
// mark (a provider outside the enterprise) has no traceparent header.
func TestTheTraceContextIsSentOnlyWhenTheCallIsMarked(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Traceparent")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()
	base := observability.ContinueTrace(context.Background(), "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01", "")
	req := model.Request{Model: "m", Messages: []agui.Message{{ID: "u", Role: agui.RoleUser, Content: "hi"}}}
	for _, c := range []struct {
		ctx  context.Context
		want string
	}{
		{base, ""},
		{observability.WithoutPropagation(base), ""},
		{observability.WithPropagation(base), "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"},
	} {
		got = "unset"
		if _, err := idleProvider(t, srv.URL, 0).Complete(c.ctx, req, nil); err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("traceparent %q, want %q", got, c.want)
		}
	}
}
