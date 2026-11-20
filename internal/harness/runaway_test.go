package harness

// Tests of D60: an answer that does not end. The model double streams an endless repetition (tool-call arguments that grow
// without end, or text); the OpenAI adapter's stream limit is injected small so that the test measures bytes and time in
// kilobytes and milliseconds, not in the 8 MiB of production.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"idp-host/adapters/providers/openai"
	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/ctxwin"
	"idp-host/internal/model"
	"idp-host/internal/run"
)

// streamLimit is the stream limit the tests inject; slack is how far past it the double may have written before the client
// has read the chunk that crossed the limit and gone away (it writes without waiting for the reader, so this varies from run to run). Both are far
// below the 8 MiB of production.
const (
	streamLimit = 128 << 10
	slack       = 3 << 20
)

// addLimited is addProvider with the adapter's stream limit injected.
func addLimited(t *testing.T, e *env, name string, limit int, handle func(n int, w http.ResponseWriter, r *http.Request) bool) *atomic.Int32 {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handle != nil && handle(int(hits.Add(1)), w, r) {
			return
		}
		if handle == nil {
			hits.Add(1)
		}
		e.model.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	hc, _ := model.NewHTTPClient(5*time.Second, "")
	p, err := openai.New(openai.Config{Name: name, BaseURL: srv.URL + "/v1", HTTP: hc, UsageInStream: true, MaxStreamBytes: limit})
	if err != nil {
		t.Fatal(err)
	}
	var all []model.Provider
	for _, n := range e.h.Providers.Names() {
		old, _ := e.h.Providers.Get(n)
		all = append(all, old)
	}
	e.h.Providers = model.NewRegistry(append(all, p)...)
	return &hits
}

// endless streams, as fast as the client reads, an answer that never ends: "args" is a tool call whose arguments grow
// without end, "text" an endless repetition of one sentence. sent counts the bytes written; the stream stops when the client
// goes away.
func endless(kind string, sent *atomic.Int64, onlyAttempt int) func(int, http.ResponseWriter, *http.Request) bool {
	return func(n int, w http.ResponseWriter, r *http.Request) bool {
		if onlyAttempt > 0 && n != onlyAttempt {
			return false
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		write := func(s string) bool {
			k, err := fmt.Fprint(w, s)
			sent.Add(int64(k))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return err == nil && r.Context().Err() == nil
		}
		if kind == "args" {
			if !write(`data: {"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"sum","arguments":"{\"a\":\""}}]}}]}` + "\n\n") {
				return true
			}
		}
		for {
			var chunk string
			if kind == "args" {
				chunk = `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"the same words again "}}]}}]}` + "\n\n"
			} else {
				chunk = `data: {"choices":[{"index":0,"delta":{"content":"The same sentence again. "}}]}` + "\n\n"
			}
			if !write(chunk) {
				return true
			}
		}
	}
}

// The stream that never ends is stopped at the stream limit. The second request answers normally: the run completes, the
// first attempt is on the stream as an output_runaway with the retry, and the tool call it was writing was never run.
func TestAnEndlessToolCallStreamIsARunawayAndIsRetriedOnce(t *testing.T) {
	e := setup(t)
	var sent atomic.Int64
	hits := addLimited(t, e, "loopy", streamLimit, endless("args", &sent, 1))
	fast(e, "loopy", 3, 0)
	m := newMem(onlyModel("loopy"), user("hello"), run.Input{})
	e.h.Store = m
	start := time.Now()
	err := e.h.Run(context.Background(), claim)
	took := time.Since(start)
	if err != nil || m.status != run.StatusCompleted || hits.Load() != 2 {
		t.Fatalf("%v %s %s, %d requests, %s", err, m.status, m.failCode, hits.Load(), took)
	}
	if got := sent.Load(); got < streamLimit || got > streamLimit+slack {
		t.Errorf("the endless stream was cut after %d bytes, want %d..%d", got, streamLimit, streamLimit+slack)
	}
	if took > 10*time.Second {
		t.Errorf("took %s", took)
	}
	if len(e.mcp.Calls()) != 0 {
		t.Errorf("the tool call of the runaway answer reached the gateway: %v", e.mcp.Calls())
	}
	if ev := eventNamed(m, agui.CustomModelError); len(ev) != 1 || ev[0]["code"] != "output_runaway" || ev[0]["outcome"] != "retry" {
		t.Errorf("model.error %v", ev)
	}
	if ev := eventNamed(m, agui.CustomModelRetry); len(ev) != 1 || ev[0]["code"] != "output_runaway" {
		t.Errorf("model.retry %v", ev)
	}
	textWellFormed(t, m)
}

// A runaway that is endless again at the second request ends the run, and the message names output_runaway; two requests
// were made (the cap of a runaway is two, whatever attempts allows), not three.
func TestARunawayTwiceFailsTheRunAndSaysSo(t *testing.T) {
	e := setup(t)
	var sent atomic.Int64
	hits := addLimited(t, e, "loopy", streamLimit, endless("args", &sent, 0))
	fast(e, "loopy", 3, 0)
	m := newMem(onlyModel("loopy"), user("hello"), run.Input{})
	e.h.Store = m
	_ = e.h.Run(context.Background(), claim)
	if m.status != run.StatusFailed || m.failCode != run.FailModel || hits.Load() != 2 {
		t.Fatalf("%s %s, %d requests", m.status, m.failCode, hits.Load())
	}
	if !strings.Contains(m.failMessage, "output_runaway") || strings.Contains(m.failMessage, "decode") {
		t.Errorf("failure message %q", m.failMessage)
	}
	if len(m.steps) != 1 || m.steps[0].ErrorCode != "output_runaway" {
		t.Errorf("steps %+v", m.steps)
	}
	if len(e.mcp.Calls()) != 0 {
		t.Errorf("gateway calls %v", e.mcp.Calls())
	}
}

// Endless text has been streamed to the member: it is not taken back by a second attempt (D45-3), so the run fails at once,
// as output_runaway, and the text the member saw is closed.
func TestAnEndlessTextStreamIsARunawayAndIsNotRetried(t *testing.T) {
	e := setup(t)
	var sent atomic.Int64
	hits := addLimited(t, e, "loopy", streamLimit, endless("text", &sent, 0))
	fast(e, "loopy", 3, 0)
	m := newMem(onlyModel("loopy"), user("hello"), run.Input{})
	e.h.Store = m
	_ = e.h.Run(context.Background(), claim)
	if m.status != run.StatusFailed || hits.Load() != 1 || len(m.steps) != 1 || m.steps[0].ErrorCode != "output_runaway" {
		t.Fatalf("%s, %d requests, %+v", m.status, hits.Load(), m.steps)
	}
	if !strings.Contains(m.failMessage, "output_runaway") {
		t.Errorf("failure message %q", m.failMessage)
	}
	if got := sent.Load(); got < streamLimit || got > streamLimit+slack {
		t.Errorf("cut after %d bytes", got)
	}
	textWellFormed(t, m)
}

// scripted is a provider whose n-th answer (1-based) is made by a function; it keeps the requests it was sent.
type scripted struct {
	name   string
	mu     sync.Mutex
	reqs   []model.Request
	answer func(n int, onDelta func(model.Delta)) (*model.Response, error)
}

func (s *scripted) Name() string { return s.name }
func (s *scripted) Complete(_ context.Context, req model.Request, onDelta func(model.Delta)) (*model.Response, error) {
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	n := len(s.reqs)
	s.mu.Unlock()
	return s.answer(n, onDelta)
}
func (s *scripted) requests() []model.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.Request(nil), s.reqs...)
}

const sumCall = `{"a":1,"b":2}`

// finished is an answer that ended by itself; cutAt is an answer the output limit stopped while it wrote a tool call.
func finished(text string) *model.Response {
	return &model.Response{StopReason: model.StopEnd, Usage: model.Usage{PromptTokens: 1000, CompletionTokens: 20},
		Message: agui.Message{Role: agui.RoleAssistant, Content: text}}
}
func cutAt(prompt, completion int) *model.Response {
	return &model.Response{StopReason: model.StopLength, Usage: model.Usage{PromptTokens: prompt, CompletionTokens: completion},
		Message: agui.Message{Role: agui.RoleAssistant, ToolCalls: []agui.ToolCall{{ID: "c1", Type: "function", Function: agui.FunctionCall{Name: "sum", Arguments: sumCall}}}}}
}

// runawayEnv is a harness whose only model is the scripted provider, with a context window of 131,072 tokens in the host's
// configuration (the case D60 is about: the window is known and far from full).
func runawayEnv(t *testing.T, answer func(n int, onDelta func(model.Delta)) (*model.Response, error)) (*env, *scripted, *memStore) {
	t.Helper()
	e := setup(t)
	p := &scripted{name: "fake", answer: answer}
	e.h.Providers = model.NewRegistry(p)
	e.h.ModelContext = map[string]ctxwin.Settings{"fake/fake-1": {Window: 131072}}
	m := newMem(def(), user("please sum 1 and 2"), run.Input{})
	e.h.Store = m
	return e, p, m
}

// D60 (a): an output limit is always sent, and who set it decides which: the release's or the run's when lower, else the
// operator's entry for the model, else the default; the operator's value is a ceiling a release cannot raise (the default is
// not the operator's word); and the window's remainder caps all of them.
func TestAnOutputLimitIsAlwaysSent(t *testing.T) {
	for _, c := range []struct {
		name              string
		release, operator int
		runOption         int
		window            int
		want              int // 0: below the window's remainder, see wantBelow
		wantBelow         int
		unknown           bool // the host's configuration does not give the model's window
		none              bool // no limit is sent
	}{
		{name: "window unknown, nothing explicit: no limit is sent", unknown: true, none: true},
		{name: "window unknown, the operator's value", unknown: true, operator: 8000, want: 8000},
		{name: "window unknown, the release's value", unknown: true, release: 4000, want: 4000},
		{name: "window unknown, the release's value above the default", unknown: true, release: 40000, want: 40000},
		{name: "window unknown, the run's value", unknown: true, runOption: 1000, none: false, want: 1000},
		{name: "nothing sets one: the default", want: model.DefaultMaxOutputTokens},
		{name: "the operator's value", operator: 8000, want: 8000},
		{name: "the release's lower value", operator: 8000, release: 4000, want: 4000},
		{name: "the release cannot exceed the operator's", operator: 8000, release: 20000, want: 8000},
		{name: "no operator value: the release's higher value stands", release: 40000, want: 40000},
		{name: "the run's lower value", operator: 8000, release: 4000, runOption: 1000, want: 1000},
		{name: "the window's remainder caps the default", window: 20000, wantBelow: 20000},
		{name: "a window that leaves room changes nothing", window: 131072, operator: 8000, want: 8000},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, p, m := runawayEnv(t, func(int, func(model.Delta)) (*model.Response, error) { return finished("3"), nil })
			if c.window == 0 && !c.unknown {
				c.window = 131072
			}
			e.h.ModelContext = map[string]ctxwin.Settings{"fake/fake-1": {Window: c.window}}
			e.h.ModelOutput = map[string]model.OutputPolicy{"fake/fake-1": {MaxOutputTokens: c.operator}}
			d := def()
			d.Model.MaxOutputTokens = c.release
			m = newMem(d, user("please sum 1 and 2"), run.Input{MaxOutputTokens: c.runOption})
			e.h.Store = m
			if err := e.h.Run(context.Background(), claim); err != nil || m.status != run.StatusCompleted {
				t.Fatalf("%v %s", err, m.status)
			}
			got := p.requests()[0].MaxOutputTokens
			switch {
			case c.none && got != 0:
				t.Errorf("max output tokens %d, want none sent (the window is not known and nothing is explicit)", got)
			case c.want > 0 && got != c.want:
				t.Errorf("max output tokens %d, want %d", got, c.want)
			case c.wantBelow > 0 && (got <= 256 || got >= c.wantBelow):
				t.Errorf("max output tokens %d, want the remainder of a window of %d (more than 256, less than the window)", got, c.wantBelow)
			}
		})
	}
}

// D60 (b), (c): the output limit stopped an answer that was writing a tool call, with the window far from full. It is an
// output_runaway, not D40's cut-off: it is retried on the same candidate, with sampling that is not the first attempt's, and
// the run completes when the second answer is normal. None of the cut-off call is run, and the attempt is on the stream with
// its generated tokens.
func TestAnAnswerCutAtTheLimitWithRoomInTheWindowIsRetriedAndTheRunCompletes(t *testing.T) {
	e, p, m := runawayEnv(t, func(n int, _ func(model.Delta)) (*model.Response, error) {
		if n == 1 {
			return cutAt(1000, model.DefaultMaxOutputTokens), nil
		}
		return finished("3"), nil
	})
	if err := e.h.Run(context.Background(), claim); err != nil || m.status != run.StatusCompleted {
		t.Fatalf("%v %s %s", err, m.status, m.failMessage)
	}
	reqs := p.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests", len(reqs))
	}
	if reqs[0].Temperature != nil || reqs[0].PresencePenalty != nil {
		t.Errorf("the first request changed the sampling: %v %v", reqs[0].Temperature, reqs[0].PresencePenalty)
	}
	if reqs[1].Temperature == nil || *reqs[1].Temperature != model.DefaultRetryTemperature || reqs[1].PresencePenalty == nil || *reqs[1].PresencePenalty != model.DefaultRetryPresencePenalty {
		t.Errorf("the retry kept the first attempt's sampling: %v %v", reqs[1].Temperature, reqs[1].PresencePenalty)
	}
	if reqs[1].MaxOutputTokens != model.DefaultMaxOutputTokens {
		t.Errorf("the retry's limit %d", reqs[1].MaxOutputTokens)
	}
	if len(e.mcp.Calls()) != 0 || len(m.calls) != 0 || toolCallEvents(m) != 0 {
		t.Errorf("the cut-off call reached a tool: %v, %d records, %d events", e.mcp.Calls(), len(m.calls), toolCallEvents(m))
	}
	if ev := eventNamed(m, agui.CustomModelCutOff); len(ev) != 0 {
		t.Errorf("a runaway is not D55's cut-off event: %v", ev)
	}
	if ev := eventNamed(m, agui.CustomModelError); len(ev) != 1 || ev[0]["code"] != model.CodeOutputRunaway || ev[0]["outcome"] != "retry" || ev[0]["completionTokens"] != model.DefaultMaxOutputTokens || ev[0]["limit"] != model.DefaultMaxOutputTokens {
		t.Errorf("model.error %v", ev)
	}
	ev := eventNamed(m, agui.CustomModelRetry)
	if len(ev) != 1 || ev[0]["code"] != model.CodeOutputRunaway || ev[0]["of"] != model.MaxRunawayAttempts || ev[0]["temperature"] != model.DefaultRetryTemperature || ev[0]["waitMs"] != int64(0) {
		t.Errorf("model.retry %v", ev)
	}
	usage := eventNamed(m, agui.CustomUsage)
	if len(usage) != 1 || usage[0]["attempts"] != 2 {
		t.Fatalf("usage %v", usage)
	}
	at, _ := usage[0]["attemptTimings"].([]map[string]any)
	if len(at) != 2 || at[0]["outcome"] != model.CodeOutputRunaway || at[0]["completionTokens"] != model.DefaultMaxOutputTokens || at[1]["outcome"] != "ok" {
		t.Errorf("attempt timings %v", usage[0]["attemptTimings"])
	}
	if len(m.steps) != 1 || m.steps[0].ErrorCode != "" || m.steps[0].CompletionTokens != 20 {
		t.Errorf("steps %+v", m.steps)
	}
	if d := danglingCalls(m.rc.Messages); len(d) != 0 {
		t.Errorf("dangling calls %v", d)
	}
}

// What the retry changes is the operator's to set, per model; "keepSampling" repeats the request, and a temperature the
// release sets is kept.
func TestTheRetrySamplingIsTheOperatorsAndTheReleasesTemperatureIsKept(t *testing.T) {
	half := 0.2
	for _, c := range []struct {
		name         string
		policy       model.RunawayRetry
		release      *float64
		wantTemp     *float64
		wantPresence *float64
	}{
		{name: "the operator's values", policy: model.RunawayRetry{Temperature: 0.9, PresencePenalty: 1.5}, wantTemp: ptr(0.9), wantPresence: ptr(1.5)},
		{name: "the release's temperature stays", release: &half, wantTemp: &half, wantPresence: ptr(model.DefaultRetryPresencePenalty)},
		{name: "kept as it was", policy: model.RunawayRetry{Keep: true, Temperature: 0.9}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, p, m := runawayEnv(t, func(n int, _ func(model.Delta)) (*model.Response, error) {
				if n == 1 {
					return cutAt(1000, model.DefaultMaxOutputTokens), nil
				}
				return finished("3"), nil
			})
			e.h.ModelOutput = map[string]model.OutputPolicy{"fake/fake-1": {Retry: c.policy}}
			d := def()
			d.Model.Temperature = c.release
			m = newMem(d, user("sum"), run.Input{})
			e.h.Store = m
			if err := e.h.Run(context.Background(), claim); err != nil || m.status != run.StatusCompleted {
				t.Fatalf("%v %s", err, m.status)
			}
			r := p.requests()[1]
			same := func(a, b *float64) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }
			if !same(r.Temperature, c.wantTemp) || !same(r.PresencePenalty, c.wantPresence) {
				t.Errorf("the retry sent temperature %v and presence penalty %v", deref(r.Temperature), deref(r.PresencePenalty))
			}
			if c.policy.Keep && len(eventNamed(m, agui.CustomModelRetry)[0]) != 0 && eventNamed(m, agui.CustomModelRetry)[0]["temperature"] != nil {
				t.Errorf("the retry event claims a change of sampling that was not made")
			}
		})
	}
}

func ptr(f float64) *float64 { return &f }
func deref(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

// Two runaways in a row end the run with a message that names output_runaway and the limit; no third request is made
// (attempts allows three, a runaway two); the step keeps what the last attempt generated; nothing was run.
func TestARunawayTwiceFailsTheRunWithAClearMessage(t *testing.T) {
	e, p, m := runawayEnv(t, func(int, func(model.Delta)) (*model.Response, error) {
		return cutAt(1000, model.DefaultMaxOutputTokens), nil
	})
	fast(e, "fake", 3, 0)
	_ = e.h.Run(context.Background(), claim)
	if m.status != run.StatusFailed || m.failCode != run.FailModel || len(p.requests()) != 2 {
		t.Fatalf("%s %s, %d requests", m.status, m.failCode, len(p.requests()))
	}
	for _, want := range []string{"output_runaway", "32768 tokens", "default", "none of its tool calls was run"} {
		if !strings.Contains(m.failMessage, want) {
			t.Errorf("failure message %q lacks %q", m.failMessage, want)
		}
	}
	if len(m.steps) != 1 || m.steps[0].ErrorCode != model.CodeOutputRunaway || m.steps[0].CompletionTokens != model.DefaultMaxOutputTokens || m.steps[0].StopReason != model.StopLength {
		t.Errorf("steps %+v", m.steps)
	}
	if ev := eventNamed(m, agui.CustomModelError); len(ev) != 2 || ev[1]["outcome"] != "stop" || ev[0]["outcome"] != "retry" {
		t.Errorf("model.error %v", ev)
	}
	if len(e.mcp.Calls()) != 0 || len(m.calls) != 0 || len(danglingCalls(m.rc.Messages)) != 0 {
		t.Errorf("calls %v %d %v", e.mcp.Calls(), len(m.calls), danglingCalls(m.rc.Messages))
	}
}

// The runaway cap is two even where the provider's retry policy allows one request only.
func TestARunawayIsNotRetriedWhereTheRetryPolicyAllowsOneRequest(t *testing.T) {
	e, p, m := runawayEnv(t, func(int, func(model.Delta)) (*model.Response, error) {
		return cutAt(1000, model.DefaultMaxOutputTokens), nil
	})
	fast(e, "fake", 1, 0)
	_ = e.h.Run(context.Background(), claim)
	if m.status != run.StatusFailed || len(p.requests()) != 1 {
		t.Fatalf("%s, %d requests", m.status, len(p.requests()))
	}
}

// Text the member has read is not taken back (D45-3): a runaway that streamed text is not retried; the run fails as an
// output_runaway and the text that was shown is closed.
func TestARunawayThatStreamedTextIsNotRetried(t *testing.T) {
	e, p, m := runawayEnv(t, func(_ int, onDelta func(model.Delta)) (*model.Response, error) {
		onDelta(model.Delta{Text: "The same sentence again. "})
		r := cutAt(1000, model.DefaultMaxOutputTokens)
		r.Message.Content = "The same sentence again. "
		return r, nil
	})
	_ = e.h.Run(context.Background(), claim)
	if m.status != run.StatusFailed || len(p.requests()) != 1 || len(m.steps) != 1 || m.steps[0].ErrorCode != model.CodeOutputRunaway {
		t.Fatalf("%s, %d requests, %+v", m.status, len(p.requests()), m.steps)
	}
	if !strings.Contains(m.failMessage, "output_runaway") {
		t.Errorf("failure message %q", m.failMessage)
	}
	textWellFormed(t, m)
}

// A runaway of reasoning only (no text reaches the member) is retried like a tool call is.
func TestARunawayThatOnlyReasonedIsRetried(t *testing.T) {
	e, p, m := runawayEnv(t, func(n int, onDelta func(model.Delta)) (*model.Response, error) {
		if n == 1 {
			onDelta(model.Delta{Reasoning: "thinking again "})
			r := cutAt(1000, model.DefaultMaxOutputTokens)
			r.Message.ToolCalls = nil
			return r, nil
		}
		return finished("3"), nil
	})
	if err := e.h.Run(context.Background(), claim); err != nil || m.status != run.StatusCompleted || len(p.requests()) != 2 {
		t.Fatalf("%v %s, %d requests", err, m.status, len(p.requests()))
	}
}

// D60 (d): where the window is what stopped the answer, D40 stands: one request, no retry, the D40 message and, with tool
// calls, D55's event; the step has no error code (the answer was received) and nothing is called output_runaway.
func TestACutAnswerWhereTheWindowIsFullIsStillD40(t *testing.T) {
	for _, c := range []struct {
		name             string
		window           int
		prompt, produced int
		bigInstructions  bool // the host's estimate of the request is far above what the server counts
	}{
		{name: "the limit is what the window left (clamped)", window: 20000, prompt: 1000, produced: 19000},
		{name: "the clamp alone decides (the server counts far fewer prompt tokens than the estimate)", window: 20000, prompt: 100, produced: 16000, bigInstructions: true},
		{name: "the prompt and the limit leave less than a sixteenth of the window", window: 60000, prompt: 30000, produced: model.DefaultMaxOutputTokens},
		{name: "the server stopped short of the limit it was given", window: 131072, prompt: 1000, produced: 7},
		{name: "the window is not known, the server stopped short", window: 0, prompt: 1000, produced: 7},
		{name: "the window is not known and the server does not say how much it generated", window: 0, prompt: 0, produced: 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, p, m := runawayEnv(t, func(int, func(model.Delta)) (*model.Response, error) { return cutAt(c.prompt, c.produced), nil })
			e.h.ModelContext = map[string]ctxwin.Settings{"fake/fake-1": {Window: c.window}}
			if c.bigInstructions {
				d := def()
				d.Instructions = strings.Repeat("word ", 2000)
				m = newMem(d, user("sum"), run.Input{})
				e.h.Store = m
			}
			fast(e, "fake", 3, 0)
			_ = e.h.Run(context.Background(), claim)
			if m.status != run.StatusFailed || m.failCode != run.FailModel || len(p.requests()) != 1 {
				t.Fatalf("%s %s, %d requests", m.status, m.failCode, len(p.requests()))
			}
			if !strings.Contains(m.failMessage, "the model's answer was cut off: its context window or output limit is full") || strings.Contains(m.failMessage, "output_runaway") {
				t.Errorf("failure message %q", m.failMessage)
			}
			if len(m.steps) != 1 || m.steps[0].ErrorCode != "" || m.steps[0].StopReason != model.StopLength {
				t.Errorf("steps %+v", m.steps)
			}
			if ev := eventNamed(m, agui.CustomModelCutOff); len(ev) != 1 {
				t.Errorf("model.cutoff %v", ev)
			}
			if len(eventNamed(m, agui.CustomModelError)) != 0 || len(e.mcp.Calls()) != 0 {
				t.Errorf("model.error %v, gateway calls %v", eventNamed(m, agui.CustomModelError), e.mcp.Calls())
			}
		})
	}
}

// A candidate whose answers do not end is left after its two requests; the next candidate answers (D56's loop is the same).
func TestARunawayCandidateIsLeftForTheNextOne(t *testing.T) {
	e, p, m := runawayEnv(t, func(int, func(model.Delta)) (*model.Response, error) {
		return cutAt(1000, model.DefaultMaxOutputTokens), nil
	})
	second := &scripted{name: "fake2", answer: func(int, func(model.Delta)) (*model.Response, error) { return finished("3"), nil }}
	e.h.Providers = model.NewRegistry(p, second)
	d := def()
	d.Model.Fallbacks = []agent.ModelRef{{Provider: "fake2", Model: "fake-2"}}
	m = newMem(d, user("sum"), run.Input{})
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil || m.status != run.StatusCompleted {
		t.Fatalf("%v %s %s", err, m.status, m.failMessage)
	}
	if len(p.requests()) != 2 || len(second.requests()) != 1 {
		t.Fatalf("%d and %d requests", len(p.requests()), len(second.requests()))
	}
	if ev := eventNamed(m, agui.CustomModelError); len(ev) != 2 || ev[1]["outcome"] != "next_candidate" || ev[1]["code"] != model.CodeOutputRunaway {
		t.Errorf("model.error %v", ev)
	}
	if len(m.steps[0].Failed) != 1 || m.steps[0].Failed[0].Code != model.CodeOutputRunaway {
		t.Errorf("failed candidates %+v", m.steps[0].Failed)
	}
}

// Without a known window, an explicit limit still tells a runaway from the server's own cut: the server says how many tokens
// it generated, and the stream bound is a runaway whatever the window.
func TestARunawayIsRecognisedWithoutAKnownWindowWhenTheLimitIsExplicit(t *testing.T) {
	e, p, m := runawayEnv(t, func(n int, _ func(model.Delta)) (*model.Response, error) {
		if n == 1 {
			return cutAt(1000, 8000), nil
		}
		return finished("3"), nil
	})
	e.h.ModelContext = nil
	e.h.ModelOutput = map[string]model.OutputPolicy{"fake/fake-1": {MaxOutputTokens: 8000}}
	if err := e.h.Run(context.Background(), claim); err != nil || m.status != run.StatusCompleted || len(p.requests()) != 2 {
		t.Fatalf("%v %s, %d requests", err, m.status, len(p.requests()))
	}
	if r := p.requests(); r[0].MaxOutputTokens != 8000 || r[1].MaxOutputTokens != 8000 {
		t.Errorf("limits %d %d", r[0].MaxOutputTokens, r[1].MaxOutputTokens)
	}
}
