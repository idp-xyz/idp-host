package harness

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"idp-host/adapters/providers/openai"
	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/mcpclient"
	"idp-host/internal/model"
	"idp-host/internal/run"
	"idp-host/tests/fixtures/fakemcp"
	"idp-host/tests/fixtures/fakemodel"
)

// memStore is an in-memory Store with the fencing and resolution semantics of the real one.
type memStore struct {
	mu       sync.Mutex
	rc       run.Context
	events   []agui.Event
	calls    map[string]*run.ToolCall
	steps    []run.Step
	status   string
	failCode string
	cred     string
	fenced   bool
	fenceAt  int // fence after this many event appends (0 = never)
	appends  int
}

func newMem(def agent.Definition, msgs []agui.Message, input run.Input) *memStore {
	def.Normalize()
	return &memStore{calls: map[string]*run.ToolCall{}, cred: "tok", status: run.StatusRunning,
		rc: run.Context{Run: run.Run{ID: "run_1", ThreadID: "thr_1", Owner: "alice", Status: run.StatusRunning, Input: input, DeadlineAt: time.Now().Add(time.Minute)},
			Release: &agent.Document{Definition: def}, Messages: msgs},
		events: []agui.Event{agui.RunStarted("thr_1", "run_1")}}
}

func (m *memStore) check() error {
	if m.fenced {
		return run.ErrFenced
	}
	return nil
}

func (m *memStore) Context(context.Context, *run.Claim) (*run.Context, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(); err != nil {
		return nil, err
	}
	rc := m.rc
	rc.Messages = append([]agui.Message(nil), m.rc.Messages...)
	rc.ToolCalls = nil
	for _, c := range m.calls {
		if c.FinishedAt.IsZero() {
			rc.ToolCalls = append(rc.ToolCalls, *c)
		}
	}
	return &rc, nil
}

func (m *memStore) appendEvents(events []agui.Event) error {
	if err := m.check(); err != nil {
		return err
	}
	m.events = append(m.events, events...)
	m.appends++
	if m.fenceAt > 0 && m.appends >= m.fenceAt {
		m.fenced = true
	}
	return nil
}

func (m *memStore) AppendEvents(_ context.Context, _ *run.Claim, events []agui.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.appendEvents(events)
}

func (m *memStore) AppendMessages(_ context.Context, _ *run.Claim, msgs []agui.Message, events []agui.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(); err != nil {
		return err
	}
	m.rc.Messages = append(m.rc.Messages, msgs...)
	return m.appendEvents(events)
}

func (m *memStore) StartStep(_ context.Context, _ *run.Claim, maxSteps int, _ int64, provider, mdl string) (int, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(); err != nil {
		return 0, false, err
	}
	if m.rc.Run.StepCount >= maxSteps {
		return 0, false, run.ErrStepLimit
	}
	m.rc.Run.StepCount++
	m.steps = append(m.steps, run.Step{Step: m.rc.Run.StepCount, Provider: provider, Model: mdl})
	return m.rc.Run.StepCount, m.rc.Run.CancelRequested, m.appendEvents([]agui.Event{agui.StepStarted(run.StepName(m.rc.Run.StepCount))})
}

func (m *memStore) FinishStep(_ context.Context, _ *run.Claim, st run.Step, events []agui.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(); err != nil {
		return err
	}
	m.steps[len(m.steps)-1] = st
	return m.appendEvents(events)
}

func (m *memStore) RecordToolCalls(_ context.Context, _ *run.Claim, calls []run.ToolCall, events []agui.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(); err != nil {
		return err
	}
	for i := range calls {
		c := calls[i]
		m.calls[c.ID] = &c
	}
	return m.appendEvents(events)
}

func (m *memStore) UpdateToolCall(_ context.Context, _ *run.Claim, id, state, result string, isError bool, events []agui.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(); err != nil {
		return err
	}
	c := m.calls[id]
	c.State, c.Result, c.IsError = state, result, isError
	switch state {
	case run.CallDone, run.CallFailed, run.CallDenied, run.CallRejected, run.CallExpired:
		c.FinishedAt = time.Now()
	}
	return m.appendEvents(events)
}

func (m *memStore) Finish(_ context.Context, _ *run.Claim, status, code, _ string, events []agui.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(); err != nil {
		return err
	}
	m.status, m.failCode = status, code
	m.rc.Run.Status = status
	return m.appendEvents(events)
}

func (m *memStore) Credential(context.Context, *run.Claim) (string, error) {
	if m.cred == "" {
		return "", &run.Failure{Code: run.FailCredential, Message: "no credential"}
	}
	return m.cred, nil
}

func (m *memStore) types() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, e := range m.events {
		t := e.Type()
		if t == agui.EventCustom {
			t += ":" + e["name"].(string)
		}
		out = append(out, t)
	}
	return out
}

func (m *memStore) last() agui.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rc.Messages[len(m.rc.Messages)-1]
}

type env struct {
	model *fakemodel.Server
	mcp   *fakemcp.Server
	h     *Harness
}

func setup(t *testing.T) *env {
	t.Helper()
	fm := fakemodel.New()
	ms := httptest.NewServer(fm.Handler())
	t.Cleanup(ms.Close)
	fx := fakemcp.New()
	xs := httptest.NewServer(fx.Handler())
	t.Cleanup(xs.Close)
	hc, _ := model.NewHTTPClient(5*time.Second, "")
	p, err := openai.New(openai.Config{Name: "fake", BaseURL: ms.URL + "/v1", HTTP: hc, UsageInStream: true})
	if err != nil {
		t.Fatal(err)
	}
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	t.Cleanup(down.Close)
	dp, _ := openai.New(openai.Config{Name: "down", BaseURL: down.URL + "/v1", HTTP: hc})
	gw := &Gateway{Name: "edge", Endpoint: xs.URL + "/mcp", Client: &mcpclient.Client{HTTP: mcpclient.NewHTTPClient(5*time.Second, nil), Info: mcpclient.ClientInfo{Name: "idp-host", Version: "test"}}}
	h := &Harness{Providers: model.NewRegistry(p, dp), Gateways: map[string]*Gateway{"edge": gw}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DeltaFlush: time.Nanosecond}
	return &env{model: fm, mcp: fx, h: h}
}

func def() agent.Definition {
	return agent.Definition{Instructions: "help", Model: agent.ModelProfile{Provider: "fake", Model: "fake-1"},
		Tools:        agent.ToolPolicy{Gateway: "edge", Allow: []string{"sum", "echo"}},
		Confirmation: agent.ConfirmationPolicy{Default: agent.ConfirmAuto, Tools: map[string]string{"echo": agent.ConfirmConfirm}}}
}

func user(text string) []agui.Message {
	return []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: text}}
}

var claim = &run.Claim{TenantID: "t", RunID: "run_1", WorkerID: "w", Epoch: 1}

func TestPlainAnswer(t *testing.T) {
	e := setup(t)
	m := newMem(def(), user("hello"), run.Input{})
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if m.status != run.StatusCompleted || m.last().Content != "Echo: hello" || m.last().Role != agui.RoleAssistant {
		t.Fatalf("%s %+v", m.status, m.last())
	}
	want := []string{"RUN_STARTED", "STEP_STARTED", "TEXT_MESSAGE_START", "TEXT_MESSAGE_CONTENT", "TEXT_MESSAGE_END", "CUSTOM:idp.host/model.usage", "STEP_FINISHED", "RUN_FINISHED"}
	got := m.types()
	// content may be split into several TEXT_MESSAGE_CONTENT events; collapse them
	var collapsed []string
	for _, g := range got {
		if len(collapsed) > 0 && g == "TEXT_MESSAGE_CONTENT" && collapsed[len(collapsed)-1] == g {
			continue
		}
		collapsed = append(collapsed, g)
	}
	if strings.Join(collapsed, ",") != strings.Join(want, ",") {
		t.Fatalf("events %v", got)
	}
	if m.steps[0].PromptTokens == 0 || m.steps[0].StopReason != model.StopEnd || m.steps[0].Provider != "fake" {
		t.Fatalf("%+v", m.steps[0])
	}
	// the gateway was consulted for the catalog with the run's credential, nothing was called
	if b := e.mcp.Bearers(); len(b) != 1 || b[0] != "tok" || len(e.mcp.Calls()) != 0 {
		t.Fatalf("%v %v", b, e.mcp.Calls())
	}
}

func TestAutomaticToolCall(t *testing.T) {
	e := setup(t)
	m := newMem(def(), user("please sum"), run.Input{})
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if m.status != run.StatusCompleted || m.last().Content != "Result: 3" {
		t.Fatalf("%s %+v", m.status, m.last())
	}
	calls := e.mcp.Calls()
	if len(calls) != 1 || calls[0].Name != "sum" || calls[0].Bearer != "tok" || calls[0].Args["a"] != 1.0 {
		t.Fatalf("%+v", calls)
	}
	msgs := m.rc.Messages
	if len(msgs) != 4 || msgs[1].Role != agui.RoleAssistant || len(msgs[1].ToolCalls) != 1 || msgs[2].Role != agui.RoleTool || msgs[2].Content != "3" || msgs[2].ToolCallID != msgs[1].ToolCalls[0].ID {
		t.Fatalf("%+v", msgs)
	}
	if c := m.calls[msgs[1].ToolCalls[0].ID]; c.State != run.CallDone || c.Result != "3" || c.ArgumentsSHA256 == "" {
		t.Fatalf("%+v", c)
	}
	if len(m.steps) != 2 {
		t.Fatalf("steps %d", len(m.steps))
	}
	types := strings.Join(m.types(), ",")
	for _, want := range []string{"TOOL_CALL_START,TOOL_CALL_ARGS,TOOL_CALL_END", "TOOL_CALL_RESULT", "RUN_FINISHED"} {
		if !strings.Contains(types, want) {
			t.Fatalf("missing %s in %s", want, types)
		}
	}
}

func TestConfirmationThenConfirmOrReject(t *testing.T) {
	for _, decision := range []string{run.CallConfirmed, run.CallRejected} {
		e := setup(t)
		m := newMem(def(), user("echo this"), run.Input{})
		e.h.Store = m
		if err := e.h.Run(context.Background(), claim); err != nil {
			t.Fatal(err)
		}
		if m.status != run.StatusAwaitingInput || len(e.mcp.Calls()) != 0 {
			t.Fatalf("%s %v", m.status, e.mcp.Calls())
		}
		var pending *run.ToolCall
		for _, c := range m.calls {
			pending = c
		}
		if pending.State != run.CallPendingConfirmation || pending.ConfirmationExpiresAt.IsZero() || pending.Tool != "echo" {
			t.Fatalf("%+v", pending)
		}
		types := m.types()
		last := m.events[len(m.events)-1]
		if types[len(types)-1] != "RUN_FINISHED" || last["outcome"].(map[string]any)["type"] != agui.OutcomeInterrupt {
			t.Fatalf("%v %v", types, last)
		}
		if !strings.Contains(strings.Join(types, ","), "CUSTOM:idp.host/confirmation.required") {
			t.Fatalf("%v", types)
		}
		// the owner decides (what DecideToolCall does), the worker claims again
		pending.State = decision
		m.status, m.rc.Run.Status = run.StatusRunning, run.StatusRunning
		if err := e.h.Run(context.Background(), claim); err != nil {
			t.Fatal(err)
		}
		if m.status != run.StatusCompleted {
			t.Fatalf("%s", m.status)
		}
		if decision == run.CallConfirmed {
			if len(e.mcp.Calls()) != 1 || e.mcp.Calls()[0].Name != "echo" || m.last().Content != "Result: hi" {
				t.Fatalf("%v %+v", e.mcp.Calls(), m.last())
			}
		} else {
			if len(e.mcp.Calls()) != 0 || !strings.Contains(m.last().Content, "rejected") {
				t.Fatalf("%v %+v", e.mcp.Calls(), m.last())
			}
			if tool := m.rc.Messages[len(m.rc.Messages)-2]; tool.Role != agui.RoleTool || tool.Error == "" {
				t.Fatalf("%+v", tool)
			}
		}
	}
}

func TestDeniedToolAndStepLimit(t *testing.T) {
	e := setup(t)
	e.model.Script = func(msgs []fakemodel.Message, tools []string) fakemodel.Turn {
		if msgs[len(msgs)-1].Role == "tool" {
			return fakemodel.Turn{Text: "Result: " + msgs[len(msgs)-1].Content}
		}
		return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "fail", Args: `{}`}}} // fail is not in the allow list
	}
	m := newMem(def(), user("x"), run.Input{})
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if m.status != run.StatusCompleted || len(e.mcp.Calls()) != 0 || !strings.Contains(m.last().Content, "not available") {
		t.Fatalf("%s %v %+v", m.status, e.mcp.Calls(), m.last())
	}
	if !strings.Contains(strings.Join(m.types(), ","), "CUSTOM:idp.host/tool.denied") {
		t.Fatal(m.types())
	}

	// a model that never stops calling tools hits the step limit
	e.model.Script = func(msgs []fakemodel.Message, tools []string) fakemodel.Turn {
		return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "sum", Args: `{"a":1,"b":1}`}}}
	}
	d := def()
	d.Limits.MaxSteps = 2
	m = newMem(d, user("loop"), run.Input{})
	e.h.Store = m
	err := e.h.Run(context.Background(), claim)
	var f *run.Failure
	if !errors.As(err, &f) || f.Code != run.FailStepLimit || m.status != run.StatusFailed || m.failCode != run.FailStepLimit || len(m.steps) != 2 {
		t.Fatalf("%v %s %s %d", err, m.status, m.failCode, len(m.steps))
	}
	if types := m.types(); types[len(types)-1] != "RUN_ERROR" {
		t.Fatal(types)
	}
}

func TestFrontendTool(t *testing.T) {
	e := setup(t)
	e.model.Script = func(msgs []fakemodel.Message, tools []string) fakemodel.Turn {
		return fakemodel.Turn{Text: "picking", ToolCalls: []fakemodel.Call{{Name: "ui_pick", Args: `{"x":1}`}}}
	}
	m := newMem(def(), user("x"), run.Input{Tools: []agui.Tool{{Name: "ui_pick", Description: "frontend"}}})
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if m.status != run.StatusCompleted || len(e.mcp.Calls()) != 0 || len(m.steps) != 1 {
		t.Fatalf("%s %v %d", m.status, e.mcp.Calls(), len(m.steps))
	}
	for _, c := range m.calls {
		if c.State != run.CallFrontend {
			t.Fatalf("%+v", c)
		}
	}
	if types := m.types(); types[len(types)-1] != "RUN_FINISHED" {
		t.Fatal(types)
	}
}

func TestFallbackAndModelFailure(t *testing.T) {
	e := setup(t)
	d := def()
	d.Model = agent.ModelProfile{Provider: "down", Model: "x", Fallbacks: []agent.ModelRef{{Provider: "fake", Model: "fake-1"}}}
	m := newMem(d, user("hello"), run.Input{})
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if m.status != run.StatusCompleted || m.steps[0].Provider != "fake" || m.last().Content != "Echo: hello" {
		t.Fatalf("%s %+v", m.status, m.steps)
	}
	d.Model.Fallbacks = nil
	m = newMem(d, user("hello"), run.Input{})
	e.h.Store = m
	err := e.h.Run(context.Background(), claim)
	var f *run.Failure
	if !errors.As(err, &f) || f.Code != run.FailModel || m.status != run.StatusFailed || m.steps[0].ErrorCode != "status_503" {
		t.Fatalf("%v %s %+v", err, m.status, m.steps)
	}
}

func TestFencedClaimStopsWithoutFinishing(t *testing.T) {
	e := setup(t)
	m := newMem(def(), user("hello"), run.Input{})
	m.fenceAt = 2
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); !errors.Is(err, run.ErrFenced) {
		t.Fatalf("%v", err)
	}
	if m.status != run.StatusRunning {
		t.Fatalf("a fenced worker must not finish the run: %s", m.status)
	}
}

func TestMissingCredentialFailsTheRun(t *testing.T) {
	e := setup(t)
	m := newMem(def(), user("hello"), run.Input{})
	m.cred = ""
	e.h.Store = m
	err := e.h.Run(context.Background(), claim)
	var f *run.Failure
	if !errors.As(err, &f) || f.Code != run.FailCredential || m.status != run.StatusFailed {
		t.Fatalf("%v %s", err, m.status)
	}
	// an agent without tools needs no credential
	d := def()
	d.Tools = agent.ToolPolicy{}
	m = newMem(d, user("hello"), run.Input{})
	m.cred = ""
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil || m.status != run.StatusCompleted {
		t.Fatal(err, m.status)
	}
}
