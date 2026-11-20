package gemini

// D54: the time to first output and usageMetadata.cachedContentTokenCount. Fixtures follow the documentation of
// streamGenerateContent (alt=sse): every chunk is a GenerateContentResponse, usageMetadata in the last one, and
// promptTokenCount includes the cached content ("When cachedContent is set, this is still the total effective prompt size").

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

func serve(t *testing.T, chunks ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
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

// The last chunk carries usageMetadata with cachedContentTokenCount; promptTokenCount is the whole prompt, so it is not
// added to (D52's correction reads the whole).
func TestCachedContentTokenCountIsReadAndThePromptStaysWhole(t *testing.T) {
	p := provider(t, serve(t,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hel"}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"lo"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3000,"cachedContentTokenCount":2800,"candidatesTokenCount":4,"totalTokenCount":3004}}`))
	resp, tm, err := ask(t, p)
	if err != nil {
		t.Fatal(err)
	}
	u := resp.Usage
	if u.PromptTokens != 3000 || u.CompletionTokens != 4 || u.CachedTokens == nil || *u.CachedTokens != 2800 || u.CacheCreationTokens != nil || u.Server != nil {
		t.Errorf("usage %+v", u)
	}
	if ms(tm.FirstOutput) != int64(10) || ms(tm.FirstText) != int64(10) || ms(tm.AfterFirst) != int64(10) {
		t.Errorf("timing %v %v %v", ms(tm.FirstOutput), ms(tm.FirstText), ms(tm.AfterFirst))
	}
}

// No usageMetadata at all, and an answer that is only a function call.
func TestAStreamWithoutUsageLeavesTheCacheAbsent(t *testing.T) {
	p := provider(t, serve(t,
		`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"parcel__sum","args":{"a":1}}}]},"finishReason":"STOP"}]}`))
	resp, tm, err := ask(t, p)
	if err != nil || resp.Usage.CachedTokens != nil || resp.Usage.PromptTokens != 0 {
		t.Fatalf("%v %+v", err, resp.Usage)
	}
	if ms(tm.FirstOutput) != int64(10) || tm.FirstText != nil {
		t.Errorf("timing %v %v", ms(tm.FirstOutput), ms(tm.FirstText))
	}
	// usageMetadata without the cached count: absent, not 0
	p = provider(t, serve(t, `{"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":1}}`))
	resp, _, err = ask(t, p)
	if err != nil || resp.Usage.CachedTokens != nil || resp.Usage.PromptTokens != 9 {
		t.Fatalf("%v %+v", err, resp.Usage)
	}
}

// A thought part comes first: it is output, not text.
func TestAThoughtIsOutputButNotText(t *testing.T) {
	p := provider(t, serve(t,
		`{"candidates":[{"content":{"parts":[{"text":"hm","thought":true}]}}]}`,
		`{"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"STOP"}]}`))
	_, tm, err := ask(t, p)
	if err != nil || ms(tm.FirstOutput) != int64(10) || ms(tm.FirstText) != int64(20) {
		t.Fatalf("%v %v %v", err, ms(tm.FirstOutput), ms(tm.FirstText))
	}
}

// A retry (D45): each attempt has its own watch.
func TestEachAttemptHasItsOwnTiming(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(503)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}]}\n\n")
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
