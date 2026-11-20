package openai

// D54: the time to first output, what the provider says about reuse of the prompt, and llama.cpp's timings object. The
// fixtures are written from the documentation: OpenAI's chat completion chunk (usage.prompt_tokens_details.cached_tokens, in
// the last chunk when stream_options.include_usage is set) and llama.cpp's server (tools/server: usage_json_oaicompat and the
// timings object it adds to the last chunk, with no request option).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"idp-host/internal/agui"
	"idp-host/internal/model"
)

// stepClock returns 0, 10 ms, 20 ms ... on successive readings: Sent, first output, first text and Done each take one.
func stepClock() func() time.Time {
	base := time.Unix(1000, 0)
	n := 0
	return func() time.Time { t := base.Add(time.Duration(n) * 10 * time.Millisecond); n++; return t }
}

func serve(t *testing.T, contentType string, chunks ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		for _, c := range chunks {
			if contentType == "text/event-stream" {
				fmt.Fprintf(w, "data: %s\n\n", c)
			} else {
				fmt.Fprint(w, c)
			}
			w.(http.Flusher).Flush()
		}
		if contentType == "text/event-stream" {
			fmt.Fprint(w, "data: [DONE]\n\n")
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

func ms(d *time.Duration) any {
	if d == nil {
		return nil
	}
	return d.Milliseconds()
}

const llamaChunks = `{"choices":[{"index":0,"delta":{"role":"assistant","content":null}}],"object":"chat.completion.chunk"}
{"choices":[{"index":0,"delta":{"reasoning_content":"think"}}]}
{"choices":[{"index":0,"delta":{"content":"Hello"}}]}
{"choices":[{"index":0,"finish_reason":"stop","delta":{}}]}
{"choices":[],"usage":{"completion_tokens":35,"prompt_tokens":237,"total_tokens":272,"prompt_tokens_details":{"cached_tokens":236}},"timings":{"cache_n":236,"prompt_n":1,"prompt_ms":30.958,"prompt_per_token_ms":30.958,"prompt_per_second":32.3,"predicted_n":35,"predicted_ms":661.064,"predicted_per_token_ms":18.9,"predicted_per_second":52.9}}`

// A stream whose last chunk carries usage and timings. The first chunk has a role and no content: it is not output.
func TestAStreamWithUsageAndTimingsIsReadWholeAndTimed(t *testing.T) {
	chunks := strings.Split(llamaChunks, "\n")
	p := provider(t, serve(t, "text/event-stream", chunks...), true)
	resp, tm, err := ask(t, p)
	if err != nil {
		t.Fatal(err)
	}
	// Sent 0, first output (the reasoning) 10, first text 20, Done 30
	if ms(tm.FirstOutput) != int64(10) || ms(tm.FirstText) != int64(20) || ms(tm.AfterFirst) != int64(20) {
		t.Errorf("timing %v %v %v", ms(tm.FirstOutput), ms(tm.FirstText), ms(tm.AfterFirst))
	}
	u := resp.Usage
	if u.PromptTokens != 237 || u.CompletionTokens != 35 || u.CachedTokens == nil || *u.CachedTokens != 236 {
		t.Errorf("usage %+v", u)
	}
	s := u.Server
	if s == nil || *s.PromptN != 1 || *s.CacheN != 236 || *s.PredictedN != 35 || *s.PromptMs != 30.958 || *s.PredictedMs != 661.064 {
		t.Errorf("server timings %+v", s)
	}
	if u.CacheCreationTokens != nil {
		t.Errorf("OpenAI says nothing about cache writes: %v", *u.CacheCreationTokens)
	}
}

// A stream without usage and without timings (a server that sends neither): the numbers are absent, not zero. The answer is a
// tool call, so there is no text: the first output is the call, the first text stays absent.
func TestAStreamWithoutUsageOrTimingsLeavesThemAbsent(t *testing.T) {
	p := provider(t, serve(t, "text/event-stream",
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"parcel__sum","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}`,
		`{"choices":[{"index":0,"finish_reason":"tool_calls","delta":{}}]}`), false)
	resp, tm, err := ask(t, p)
	if err != nil {
		t.Fatal(err)
	}
	if ms(tm.FirstOutput) != int64(10) || tm.FirstText != nil || ms(tm.AfterFirst) != int64(10) {
		t.Errorf("timing %v %v %v", ms(tm.FirstOutput), ms(tm.FirstText), ms(tm.AfterFirst))
	}
	u := resp.Usage
	if u.CachedTokens != nil || u.CacheCreationTokens != nil || u.Server != nil || u.PromptTokens != 0 {
		t.Errorf("usage %+v", u)
	}
}

// usage with cached_tokens: 0 is a measurement (nothing reused), and prompt_tokens_details missing is not.
func TestZeroCachedTokensIsKnownAndMissingIsNot(t *testing.T) {
	p := provider(t, serve(t, "text/event-stream",
		`{"choices":[{"index":0,"delta":{"content":"a"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":0}}}`), true)
	resp, _, err := ask(t, p)
	if err != nil || resp.Usage.CachedTokens == nil || *resp.Usage.CachedTokens != 0 {
		t.Fatalf("%v %+v", err, resp.Usage)
	}
	p = provider(t, serve(t, "text/event-stream",
		`{"choices":[{"index":0,"delta":{"content":"a"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":1}}`), true)
	resp, _, err = ask(t, p)
	if err != nil || resp.Usage.CachedTokens != nil {
		t.Fatalf("%v %+v", err, resp.Usage)
	}
}

// A non-streamed answer: the first output is unknown and is reported absent, never as the total. Usage and timings of the
// body are still read.
func TestANonStreamedAnswerHasNoFirstOutput(t *testing.T) {
	body := `{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Hello"}}],
	 "usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":64}},
	 "timings":{"cache_n":64,"prompt_n":36,"prompt_ms":12.5,"predicted_n":5,"predicted_ms":50}}`
	p := provider(t, serve(t, "application/json", body), true)
	resp, tm, err := ask(t, p)
	if err != nil {
		t.Fatal(err)
	}
	if tm.FirstOutput != nil || tm.FirstText != nil || tm.AfterFirst != nil {
		t.Errorf("a non-streamed answer has no first output: %v %v %v", ms(tm.FirstOutput), ms(tm.FirstText), ms(tm.AfterFirst))
	}
	if resp.Message.Content != "Hello" || *resp.Usage.CachedTokens != 64 || *resp.Usage.Server.PromptN != 36 || *resp.Usage.Server.CacheN != 64 {
		t.Errorf("%+v", resp)
	}
}

// A retry (D45) makes a new request with a new watch: the failed attempt (503 before any output) has no first output, the
// second its own. The harness records the step's from the successful one.
func TestEachAttemptHasItsOwnTiming(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(503)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	p := provider(t, srv, false)
	_, t1, err := ask(t, p)
	if err == nil || t1.FirstOutput != nil {
		t.Fatalf("first attempt: %v %v", err, ms(t1.FirstOutput))
	}
	_, t2, err := ask(t, p)
	if err != nil || ms(t2.FirstOutput) != int64(10) {
		t.Fatalf("second attempt: %v %v", err, ms(t2.FirstOutput))
	}
}

// llama.cpp sends timings with no usage when stream_options.include_usage is off: they are read all the same, and the
// prompt tokens stay unknown (0) rather than being made up from them.
func TestTimingsWithoutUsageAreStillRead(t *testing.T) {
	p := provider(t, serve(t, "text/event-stream",
		`{"choices":[{"index":0,"delta":{"content":"a"}}]}`,
		`{"choices":[{"index":0,"finish_reason":"stop","delta":{}}],"timings":{"cache_n":5,"prompt_n":7,"prompt_ms":3,"predicted_n":1,"predicted_ms":2}}`), false)
	resp, _, err := ask(t, p)
	if err != nil || resp.Usage.Server == nil || *resp.Usage.Server.CacheN != 5 || resp.Usage.PromptTokens != 0 || resp.Usage.CachedTokens != nil {
		t.Fatalf("%v %+v", err, resp.Usage)
	}
}
