package gemini

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"idp-host/internal/agui"
	"idp-host/internal/model"
)

// D60: a stream longer than the host's bound is an output_runaway, not decode (see the OpenAI adapter's test).
func TestAStreamLongerThanTheLimitIsARunawayNotADecodeError(t *testing.T) {
	var sent atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for r.Context().Err() == nil {
			n, err := fmt.Fprint(w, `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"again and again "}]}}]}`+"\n\n")
			sent.Add(int64(n))
			w.(http.Flusher).Flush()
			if err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	hc, _ := model.NewHTTPClient(10*time.Second, "")
	const limit = 64 << 10
	p, err := New(Config{Name: "fake", BaseURL: srv.URL, HTTP: hc, APIKey: "k", MaxStreamBytes: limit})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Complete(context.Background(), model.Request{Model: "m", Messages: []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: "hi"}}}, nil)
	var me *model.Error
	if !errors.As(err, &me) || me.Code != model.CodeOutputRunaway || !me.Retryable {
		t.Fatalf("%v %+v", err, me)
	}
	if got := sent.Load(); got < limit || got > 4<<20 {
		t.Errorf("the writer got to %d bytes, want %d.. and far below 8 MiB", got, limit)
	}
}

// finishReason MAX_TOKENS is StopLength and candidatesTokenCount the generated tokens.
func TestAMaxTokensStopCarriesTheGeneratedTokens(t *testing.T) {
	p := provider(t, serve(t, `{"candidates":[{"content":{"role":"model","parts":[{"text":"again "}]},"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":75814,"candidatesTokenCount":32768}}`))
	resp, _, err := ask(t, p)
	if err != nil || resp.StopReason != model.StopLength || resp.Usage.CompletionTokens != 32768 || resp.Usage.PromptTokens != 75814 {
		t.Fatalf("%v %+v", err, resp)
	}
}
