package anthropic

// D54: the time to first output and the cache fields of the usage object. The fixtures follow the documentation of the
// Messages API streaming events and of prompt caching: message_start carries usage {input_tokens, cache_creation_input_tokens,
// cache_read_input_tokens, output_tokens}; message_delta carries the final output_tokens. input_tokens is only the part of
// the prompt after the last cache breakpoint.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"idp-host/internal/agui"
	"idp-host/internal/model"
)

func stepClock() func() time.Time {
	base := time.Unix(1000, 0)
	n := 0
	return func() time.Time { t := base.Add(time.Duration(n) * 10 * time.Millisecond); n++; return t }
}

func ms(d *time.Duration) any {
	if d == nil {
		return nil
	}
	return d.Milliseconds()
}

func serve(t *testing.T, events ...[2]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e[0], e[1])
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func ask(t *testing.T, p *Provider) (*model.Response, model.Timing, error) {
	t.Helper()
	w := model.NewStopwatch(stepClock())
	resp, err := p.Complete(context.Background(), model.Request{Model: "m", Watch: w,
		Messages: []agui.Message{{ID: "u", Role: agui.RoleUser, Content: "hi"}}}, nil)
	return resp, w.Timing(), err
}

func start(usage string) [2]string {
	return [2]string{"message_start", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","content":[],"usage":` + usage + `}}`}
}

var (
	textStart = [2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`}
	textDelta = [2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`}
	blockStop = [2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`}
	msgStop   = [2]string{"message_stop", `{"type":"message_stop"}`}
)

func msgDelta(usage string) [2]string {
	return [2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":` + usage + `}`}
}

// A cached request: input_tokens is the uncached part only. D52's correction compares the usage with the estimate of the whole
// request, so PromptTokens must be the whole prompt: input + cache read + cache creation. Without this a cached prompt would
// pull the factor to its floor, 0.7, at every step.
func TestPromptTokensIsTheWholePromptWhenPartOfItWasCached(t *testing.T) {
	p := provider(t, serve(t,
		start(`{"input_tokens":50,"cache_creation_input_tokens":100,"cache_read_input_tokens":4000,"output_tokens":1}`),
		textStart, textDelta, blockStop, msgDelta(`{"output_tokens":12}`), msgStop))
	resp, tm, err := ask(t, p)
	if err != nil {
		t.Fatal(err)
	}
	u := resp.Usage
	if u.PromptTokens != 4150 || u.CompletionTokens != 12 || u.CachedTokens == nil || *u.CachedTokens != 4000 || u.CacheCreationTokens == nil || *u.CacheCreationTokens != 100 {
		t.Errorf("usage %+v", u)
	}
	// Sent 0; the text block start carries no text and is not output; the first text delta is 10; Done 20
	if ms(tm.FirstOutput) != int64(10) || ms(tm.FirstText) != int64(10) || ms(tm.AfterFirst) != int64(10) {
		t.Errorf("timing %v %v %v", ms(tm.FirstOutput), ms(tm.FirstText), ms(tm.AfterFirst))
	}
}

// message_delta may repeat the input counts as running totals (documentation of the streaming events): the last word wins.
func TestAMessageDeltaMayRestateTheInputCounts(t *testing.T) {
	p := provider(t, serve(t,
		start(`{"input_tokens":10,"cache_read_input_tokens":0,"output_tokens":1}`),
		textStart, textDelta, blockStop, msgDelta(`{"input_tokens":10,"cache_read_input_tokens":900,"output_tokens":5}`), msgStop))
	resp, _, err := ask(t, p)
	if err != nil || resp.Usage.PromptTokens != 910 || *resp.Usage.CachedTokens != 900 {
		t.Fatalf("%v %+v", err, resp.Usage)
	}
}

// A stream without cache fields: the cache numbers are absent, and the prompt is input_tokens as before. A tool call only:
// its start is the first output and there is no first text.
func TestAStreamWithoutCacheFieldsLeavesThemAbsent(t *testing.T) {
	p := provider(t, serve(t,
		start(`{"input_tokens":77,"output_tokens":1}`),
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"parcel__sum","input":{}}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`},
		blockStop, msgDelta(`{"output_tokens":9}`), msgStop))
	resp, tm, err := ask(t, p)
	if err != nil {
		t.Fatal(err)
	}
	u := resp.Usage
	if u.PromptTokens != 77 || u.CachedTokens != nil || u.CacheCreationTokens != nil || u.Server != nil {
		t.Errorf("usage %+v", u)
	}
	if ms(tm.FirstOutput) != int64(10) || tm.FirstText != nil || tm.AfterFirst == nil {
		t.Errorf("timing %v %v %v", ms(tm.FirstOutput), ms(tm.FirstText), ms(tm.AfterFirst))
	}
}

// Thinking comes before the text: the first output is the thinking, the first text later.
func TestThinkingIsOutputButNotText(t *testing.T) {
	p := provider(t, serve(t,
		start(`{"input_tokens":5,"output_tokens":1}`),
		[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hm"}}`},
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`},
		[2]string{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"x"}}`},
		msgDelta(`{"output_tokens":3}`), msgStop))
	_, tm, err := ask(t, p)
	if err != nil || ms(tm.FirstOutput) != int64(10) || ms(tm.FirstText) != int64(20) {
		t.Fatalf("%v %v %v", err, ms(tm.FirstOutput), ms(tm.FirstText))
	}
}

// A retry (D45): each attempt has its own watch; a 529 before any output has no first output.
func TestEachAttemptHasItsOwnTiming(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(529)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range [][2]string{start(`{"input_tokens":1}`), textStart, textDelta, msgDelta(`{"output_tokens":1}`), msgStop} {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e[0], e[1])
		}
	}))
	defer srv.Close()
	p := provider(t, srv)
	_, t1, err := ask(t, p)
	if err == nil || t1.FirstOutput != nil {
		t.Fatalf("first: %v %v", err, ms(t1.FirstOutput))
	}
	_, t2, err := ask(t, p)
	if err != nil || ms(t2.FirstOutput) != int64(10) {
		t.Fatalf("second: %v %v", err, ms(t2.FirstOutput))
	}
}
