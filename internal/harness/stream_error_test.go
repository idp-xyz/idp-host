package harness

// Tests of D50: an error event inside a model stream that began with HTTP 200 is the provider's error. It is retried under
// D45's rule when its class passes and no text was streamed, it keeps the provider's words for the operator, and the
// member's failure message does not carry them.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"idp-host/internal/agui"
	"idp-host/internal/model"
	"idp-host/internal/run"
)

const streamErrorWords = "Server hiccup SECRET-PROVIDER-WORDS"

// streamError answers 200, streams text first when withText, then an error event with the given class.
func streamError(withText bool, errJSON string) func(int, http.ResponseWriter, *http.Request) bool {
	return func(_ int, w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if withText {
			_, _ = fmt.Fprint(w, `data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"},"finish_reason":null}]}`+"\n\n")
		} else { // reasoning and tool-call fragments are not text the member saw (D45-3)
			_, _ = fmt.Fprint(w, `data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"thinking"},"finish_reason":null}]}`+"\n\n")
		}
		_, _ = fmt.Fprint(w, `data: {"error":`+errJSON+`}`+"\n\n")
		return true
	}
}

var overloaded = `{"message":"` + streamErrorWords + `","type":"server_error","code":500}`

func TestARetryableStreamErrorBeforeTextIsRetried(t *testing.T) {
	e := setup(t)
	hits := addProvider(t, e, "flaky", 0, func(n int, w http.ResponseWriter, r *http.Request) bool {
		if n == 1 {
			return streamError(false, overloaded)(n, w, r)
		}
		return false
	})
	fast(e, "flaky", 3, 0)
	m := newMem(onlyModel("flaky"), user("hello"), run.Input{})
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil || m.status != run.StatusCompleted || hits.Load() != 2 {
		t.Fatalf("%v %s %d requests", err, m.status, hits.Load())
	}
	if r := eventNamed(m, agui.CustomModelRetry); len(r) != 1 || r[0]["code"] != model.CodeProviderServer {
		t.Errorf("retry events %v", r)
	}
}

func TestAStreamErrorAfterTextIsNotRetried(t *testing.T) {
	e := setup(t)
	hits := addProvider(t, e, "flaky", 0, streamError(true, overloaded))
	fast(e, "flaky", 3, 0)
	m := newMem(onlyModel("flaky"), user("hello"), run.Input{})
	e.h.Store = m
	_ = e.h.Run(context.Background(), claim)
	if hits.Load() != 1 || m.status != run.StatusFailed || m.steps[0].ErrorCode != model.CodeProviderServer {
		t.Fatalf("%d requests, %s, %+v", hits.Load(), m.status, m.steps)
	}
	textWellFormed(t, m)
}

func TestANonRetryableStreamErrorFailsAtOnceWithAFixedMessage(t *testing.T) {
	e := setup(t)
	bad := `{"message":"` + streamErrorWords + "\\n\\u001b[31m" + strings.Repeat("y", 3000) + `","type":"invalid_request_error","code":"bad"}`
	hits := addProvider(t, e, "flaky", 0, streamError(false, bad))
	fast(e, "flaky", 3, 0)
	m := newMem(onlyModel("flaky"), user("hello"), run.Input{})
	e.h.Store = m
	err := e.h.Run(context.Background(), claim)
	var f *run.Failure
	if !errors.As(err, &f) || f.Code != run.FailModel || hits.Load() != 1 || m.steps[0].ErrorCode != model.CodeProviderError {
		t.Fatalf("%v %d %+v", err, hits.Load(), m.steps)
	}
	if strings.Contains(f.Message, "SECRET-PROVIDER-WORDS") || !strings.Contains(f.Message, model.CodeProviderError) {
		t.Errorf("failure message: %q", f.Message)
	}
	ev := eventNamed(m, agui.CustomModelError)
	if len(ev) != 1 || ev[0]["code"] != model.CodeProviderError {
		t.Fatalf("model.error events %v", ev)
	}
	if _, has := ev[0]["detail"]; has {
		t.Errorf("the event carries the provider's words: %v", ev[0])
	}
}

// D50: the provider's words reach the host log, with what finds the run from it, and nothing the member or the model reads.
func TestTheProvidersWordsReachOnlyTheHostLog(t *testing.T) {
	e := setup(t)
	var logs bytes.Buffer
	e.h.Log = slog.New(slog.NewTextHandler(&logs, nil))
	secret := "Incorrect API key provided: sk-proj-SECRETKEY1234abcd at http://llm.internal.example:8080 org-ID99"
	bad := `{"message":"` + secret + `","type":"invalid_request_error","code":"invalid_api_key"}`
	addProvider(t, e, "flaky", 0, streamError(false, bad))
	fast(e, "flaky", 3, 0)
	m := newMem(onlyModel("flaky"), user("hello"), run.Input{})
	e.h.Store = m
	err := e.h.Run(context.Background(), claim)
	var f *run.Failure
	if !errors.As(err, &f) {
		t.Fatalf("%v", err)
	}
	const needle = "SECRETKEY1234"
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, ev := range m.events {
		if b, _ := json.Marshal(ev); strings.Contains(string(b), needle) || strings.Contains(string(b), "llm.internal") {
			t.Errorf("event %d carries the provider's words: %s", i, b)
		}
	}
	if b, _ := json.Marshal(m.steps); strings.Contains(string(b), needle) {
		t.Errorf("step rows: %s", b)
	}
	if strings.Contains(f.Message, needle) || strings.Contains(err.Error(), needle) {
		t.Errorf("failure message: %q", f.Message)
	}
	for _, msg := range m.rc.Messages {
		if strings.Contains(msg.Content, needle) {
			t.Errorf("a message of the conversation carries it: %+v", msg)
		}
	}
	line := logs.String()
	for _, want := range []string{needle, "run=" + claim.RunID, "step=1", "attempt=1", "provider=flaky", "model=fake-1", "code=provider_error"} {
		if !strings.Contains(line, want) {
			t.Errorf("the log line lacks %q: %s", want, line)
		}
	}
}

func TestAStreamErrorThatSaysTheContextIsFullIsTheContextWindowError(t *testing.T) {
	e := setup(t)
	hits := addProvider(t, e, "flaky", 0, streamError(false, `{"message":"the request exceeds the available context size","type":"exceed_context_size_error","code":400}`))
	fast(e, "flaky", 3, 0)
	m := newMem(onlyModel("flaky"), user("hello"), run.Input{})
	e.h.Store = m
	err := e.h.Run(context.Background(), claim)
	var f *run.Failure
	if !errors.As(err, &f) || hits.Load() != 1 || m.steps[0].ErrorCode != model.CodeContextLength || !strings.Contains(f.Message, "context window") {
		t.Fatalf("%v %d %+v", err, hits.Load(), m.steps)
	}
}

func TestAnUndecodableChunkIsDecodeWithAnAccountOfWhere(t *testing.T) {
	e := setup(t)
	addProvider(t, e, "flaky", 0, func(_ int, w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = fmt.Fprint(w, "data: {not json SECRET-PAYLOAD\n\n")
		return true
	})
	fast(e, "flaky", 3, 0)
	m := newMem(onlyModel("flaky"), user("hello"), run.Input{})
	e.h.Store = m
	_ = e.h.Run(context.Background(), claim)
	ev := eventNamed(m, agui.CustomModelError)
	if m.steps[0].ErrorCode != "decode" || len(ev) != 1 || strings.Contains(fmt.Sprint(ev[0]["detail"]), "SECRET-PAYLOAD") || ev[0]["detail"] == "" {
		t.Fatalf("%+v %v", m.steps, ev)
	}
}
