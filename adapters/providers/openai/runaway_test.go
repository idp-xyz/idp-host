package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"idp-host/internal/agui"
	"idp-host/internal/model"
)

// D60: a stream longer than the host's bound is an answer that does not end: output_runaway, retryable under the host's cap
// of two requests, not a chunk the host cannot read (decode). The bound is injected small here; the adapter stops reading,
// so the server's endless writer is stopped after a few hundred kilobytes, not at 8 MiB.
func TestAStreamLongerThanTheLimitIsARunawayNotADecodeError(t *testing.T) {
	var sent atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for r.Context().Err() == nil {
			n, err := fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"again and again "}}]}}]}`+"\n\n")
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
	p, err := New(Config{Name: "fake", BaseURL: srv.URL + "/v1", HTTP: hc, MaxStreamBytes: limit})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = p.Complete(context.Background(), model.Request{Model: "m", Messages: []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: "hi"}}}, nil)
	took := time.Since(start)
	var me *model.Error
	if !errors.As(err, &me) || me.Code != model.CodeOutputRunaway || !me.Retryable || me.Detail == "" {
		t.Fatalf("%v %+v", err, me)
	}
	if got := sent.Load(); got < limit || got > 4<<20 {
		t.Errorf("the writer got to %d bytes before the adapter stopped reading, want %d.. and far below 8 MiB", got, limit)
	}
	if took > 10*time.Second {
		t.Errorf("took %s", took)
	}
}

// A server that ignores stream: true and answers one JSON body longer than the bound is the same.
func TestABodyLongerThanTheLimitIsARunaway(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"`+strings.Repeat("again ", 20000)+`"}}]}`)
	}))
	defer srv.Close()
	hc, _ := model.NewHTTPClient(10*time.Second, "")
	p, _ := New(Config{Name: "fake", BaseURL: srv.URL + "/v1", HTTP: hc, MaxStreamBytes: 64 << 10})
	_, err := p.Complete(context.Background(), model.Request{Model: "m", Messages: []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: "hi"}}}, nil)
	var me *model.Error
	if !errors.As(err, &me) || me.Code != model.CodeOutputRunaway {
		t.Fatalf("%v", err)
	}
}

// The adapter reports what the host needs to tell a runaway from a full window: finish reason length as StopLength, and the
// tokens the server says it generated (usage.completion_tokens).
func TestALengthStopCarriesTheGeneratedTokens(t *testing.T) {
	p := provider(t, serve(t, "text/event-stream", `{"choices":[{"index":0,"delta":{"content":"again "}}]}`,
		`{"choices":[{"index":0,"finish_reason":"length","delta":{}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":75814,"completion_tokens":32768}}`), true)
	resp, _, err := ask(t, p)
	if err != nil || resp.StopReason != model.StopLength || resp.Usage.CompletionTokens != 32768 || resp.Usage.PromptTokens != 75814 {
		t.Fatalf("%v %+v", err, resp)
	}
}

// The output limit is always the host's to send, and the presence penalty goes out only when the host sets it (the retry of a
// runaway answer).
func TestTheLimitAndThePresencePenaltyAreSent(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"finish_reason\":\"stop\",\"delta\":{\"content\":\"ok\"}}]}\n\n")
	}))
	defer srv.Close()
	p := provider(t, srv, false)
	pen, temp := 0.5, 0.7
	for _, c := range []struct {
		name string
		req  model.Request
		pen  any
		temp any
	}{
		{"neither", model.Request{Model: "m", MaxOutputTokens: 32768}, nil, nil},
		{"no limit set: none sent", model.Request{Model: "m"}, nil, nil},
		{"both", model.Request{Model: "m", MaxOutputTokens: 32768, Temperature: &temp, PresencePenalty: &pen}, 0.5, 0.7},
	} {
		body = nil
		c.req.Messages = []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: "hi"}}
		if _, err := p.Complete(context.Background(), c.req, nil); err != nil {
			t.Fatal(err)
		}
		wantMax := any(float64(32768))
		if c.req.MaxOutputTokens == 0 {
			wantMax = nil
		}
		if body["max_tokens"] != wantMax || body["presence_penalty"] != c.pen || body["temperature"] != c.temp {
			t.Errorf("%s: %v", c.name, body)
		}
	}
}
