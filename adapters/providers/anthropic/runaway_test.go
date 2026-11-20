package anthropic

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
		_, _ = fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":100}}}\n\n"+
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tu_1\",\"name\":\"sum\",\"input\":{}}}\n\n")
		for r.Context().Err() == nil {
			n, err := fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"again \"}}\n\n")
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
	p, err := New(Config{Name: "p", BaseURL: srv.URL, HTTP: hc, AllowPlainHTTP: true, MaxStreamBytes: limit})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Complete(context.Background(), model.Request{Model: "m", MaxOutputTokens: 100, Messages: []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: "hi"}}}, nil)
	var me *model.Error
	if !errors.As(err, &me) || me.Code != model.CodeOutputRunaway || !me.Retryable {
		t.Fatalf("%v %+v", err, me)
	}
	if got := sent.Load(); got < limit || got > 4<<20 {
		t.Errorf("the writer got to %d bytes, want %d.. and far below 8 MiB", got, limit)
	}
}

// stop_reason max_tokens is StopLength and output_tokens are the generated tokens the host compares with the limit it sent.
func TestAMaxTokensStopCarriesTheGeneratedTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":75814}}}\n\n"+
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"again \"}}\n\n"+
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":32768}}\n\n")
	}))
	defer srv.Close()
	resp, err := idleProvider(t, srv.URL, 0).Complete(context.Background(), model.Request{Model: "m", MaxOutputTokens: 32768, Messages: []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: "hi"}}}, nil)
	if err != nil || resp.StopReason != model.StopLength || resp.Usage.CompletionTokens != 32768 || resp.Usage.PromptTokens != 75814 {
		t.Fatalf("%v %+v", err, resp)
	}
}
