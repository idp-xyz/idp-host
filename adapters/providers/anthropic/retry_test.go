package anthropic

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"idp-host/internal/agui"
	"idp-host/internal/model"
)

func idleProvider(t *testing.T, url string, idle time.Duration) *Provider {
	t.Helper()
	hc, _ := model.NewHTTPClient(10*time.Second, "")
	p, err := New(Config{Name: "p", BaseURL: url, HTTP: hc, IdleTimeout: idle, AllowPlainHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// D45: the Retry-After of a rate-limit answer travels from the HTTP response to the error the harness reads.
func TestARetryAfterOfTheProviderReachesTheError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(429)
	}))
	defer srv.Close()
	_, err := idleProvider(t, srv.URL, 0).Complete(context.Background(), model.Request{Model: "m", Messages: []agui.Message{{ID: "u", Role: agui.RoleUser, Content: "hi"}}}, nil)
	var me *model.Error
	if !errors.As(err, &me) || me.Status != 429 || !me.Retryable || me.RetryAfter != 7*time.Second {
		t.Fatalf("%v", err)
	}
}

// D46: a stream that sends the headers and then nothing is cut after the idle timeout as a retryable transport error that
// says it was idle; a caller that cancels is not told it was idle.
func TestAStreamThatStopsSendingFailsAsAnIdleTransportError(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	start := time.Now()
	_, err := idleProvider(t, srv.URL, 200*time.Millisecond).Complete(context.Background(), model.Request{Model: "m", Messages: []agui.Message{{ID: "u", Role: agui.RoleUser, Content: "hi"}}}, nil)
	var me *model.Error
	if !errors.As(err, &me) || me.Code != "transport" || !me.Retryable || !me.Idle {
		t.Fatalf("%v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v", d)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = idleProvider(t, srv.URL, 5*time.Second).Complete(ctx, model.Request{Model: "m", Messages: []agui.Message{{ID: "u", Role: agui.RoleUser, Content: "hi"}}}, nil)
	if !errors.As(err, &me) || me.Code != "canceled" || me.Idle {
		t.Fatalf("%v", err)
	}
}
