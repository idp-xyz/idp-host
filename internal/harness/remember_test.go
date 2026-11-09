package harness

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/cmiclient"
	"idp-host/internal/credential"
	"idp-host/internal/memfacts"
	"idp-host/internal/model"
	"idp-host/internal/run"
)

type cmiSpy struct {
	mu     sync.Mutex
	bodies []string
	auths  []string
	idems  []string
	status int
}

func newCMISpy(t *testing.T, status int) (*cmiSpy, *cmiclient.Client) {
	t.Helper()
	s := &cmiSpy{status: status}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.bodies, s.auths, s.idems = append(s.bodies, string(b)), append(s.auths, r.Header.Get("Authorization")), append(s.idems, r.Header.Get("Idempotency-Key"))
		s.mu.Unlock()
		w.WriteHeader(s.status)
	}))
	t.Cleanup(srv.Close)
	return s, &cmiclient.Client{Endpoint: srv.URL, HTTP: srv.Client()}
}

// countMetrics is a harness.Metrics that also counts memory writes by outcome.
type countMetrics struct {
	mu       sync.Mutex
	outcomes []string
}

func (m *countMetrics) StepFinished(string, string, string, time.Duration, model.Usage) {}
func (m *countMetrics) ToolCall(string, string, time.Duration)                          {}
func (m *countMetrics) RunFinished(string)                                              {}
func (m *countMetrics) Guard(string, string)                                            {}
func (m *countMetrics) MemoryWrite(outcome string) {
	m.mu.Lock()
	m.outcomes = append(m.outcomes, outcome)
	m.mu.Unlock()
}

func rememberer(c *cmiclient.Client) *cmiclient.Remember {
	return &cmiclient.Remember{Client: c, Source: "idp-host", Sensitivity: "internal", TTL: 30 * 24 * time.Hour, MaxChars: 1500,
		Key: func() (string, error) { return "writer-key", nil }, Backoff: time.Millisecond}
}

func cmiAgent() agent.Definition {
	d := def()
	d.Tools = agent.ToolPolicy{Allow: []string{}}
	d.Context = agent.ContextPolicy{CMI: true}
	return d
}

func runRemembered(t *testing.T, e *env, d agent.Definition, msgs []agui.Message, mutate func(*memStore)) *memStore {
	t.Helper()
	m := newMem(d, msgs, run.Input{})
	m.rc.Release.AgentName = "support"
	if mutate != nil {
		mutate(m)
	}
	e.h.Store = m
	e.h.Run(context.Background(), claim)
	e.h.WaitMemory()
	return m
}

func TestRememberAfterACompletedRun(t *testing.T) {
	e := setup(t)
	spy, c := newCMISpy(t, 201)
	e.h.Remember = rememberer(c)
	m := runRemembered(t, e, cmiAgent(), user("你好，世界"), nil)
	if m.status != run.StatusCompleted {
		t.Fatal(m.status)
	}
	if len(spy.bodies) != 1 || spy.auths[0] != "Bearer writer-key" || spy.idems[0] != "host-run-run_1" {
		t.Fatalf("%+v", spy)
	}
	for _, want := range []string{`"scope":{"kind":"subject","id":"alice"}`, `"kind":"episodic"`, `The user said: 你好，世界\nThe assistant answered: Echo: 你好，世界`, `"agent":"support"`, `"threadId":"thr_1"`, `"ref":"run:run_1"`, `"recordedBy":"idp-host"`} {
		if !strings.Contains(spy.bodies[0], want) {
			t.Errorf("missing %s in %s", want, spy.bodies[0])
		}
	}
}

func TestRememberNotForOtherRuns(t *testing.T) {
	spyRun := func(name string, d agent.Definition, msgs []agui.Message, remember bool, mutate func(*memStore)) {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			spy, c := newCMISpy(t, 201)
			if remember {
				e.h.Remember = rememberer(c)
			}
			m := runRemembered(t, e, d, msgs, mutate)
			t.Logf("run ended %s", m.status)
			if len(spy.bodies) != 0 {
				t.Fatalf("written: %v", spy.bodies)
			}
		})
	}
	noCMI := cmiAgent()
	noCMI.Context = agent.ContextPolicy{}
	spyRun("agent without context.cmi", noCMI, user("hi"), true, nil)
	spyRun("remember disabled", cmiAgent(), user("hi"), false, nil)
	bad := cmiAgent()
	bad.Model.Provider = "nonexistent"
	spyRun("failed run", bad, user("hi"), true, nil)
	spyRun("canceled run", cmiAgent(), user("hi"), true, func(m *memStore) { m.rc.Run.CancelRequested = true })
	spyRun("no owner subject", cmiAgent(), user("hi"), true, func(m *memStore) { m.rc.Run.Owner = "" })
	spyRun("sub-agent run", cmiAgent(), user("hi"), true, func(m *memStore) { m.rc.Run.ParentRunID = "run_parent" })
	spyRun("evaluation run", cmiAgent(), user("hi"), true, func(m *memStore) { m.rc.Run.Purpose = run.PurposeEvaluation })
	spyRun("blank user message", cmiAgent(), user("   "), true, nil)
}

func TestRememberExchangeRequiresAFinalAnswerAndAUserMessage(t *testing.T) {
	asst := func(c string, calls ...agui.ToolCall) agui.Message {
		return agui.Message{ID: "a", Role: agui.RoleAssistant, Content: c, ToolCalls: calls}
	}
	u := agui.Message{ID: "u", Role: agui.RoleUser, Content: "question"}
	cases := []struct {
		name string
		msgs []agui.Message
		ok   bool
	}{
		{"answer", []agui.Message{u, asst("answer")}, true},
		{"empty answer", []agui.Message{u, asst("")}, false},
		{"blank answer", []agui.Message{u, asst(" \n")}, false},
		{"tool-call turn (frontend tool) is not an answer", []agui.Message{u, asst("calling", agui.ToolCall{ID: "c"})}, false},
		{"no user message", []agui.Message{asst("answer")}, false},
		{"last message not an assistant's", []agui.Message{u}, false},
		{"empty", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newMem(cmiAgent(), c.msgs, run.Input{})
			h := &Harness{Remember: rememberer(&cmiclient.Client{})}
			tr := &turn{h: h, rc: &run.Context{Run: m.rc.Run, Release: &agent.Document{AgentName: "support"}}, def: cmiAgent(), messages: c.msgs}
			x, ok := tr.exchange()
			if ok != c.ok {
				t.Fatalf("%v", ok)
			}
			if ok && (x.Message != "question" || x.Answer != "answer" || x.Subject != "alice" || x.Agent != "support") {
				t.Fatalf("%+v", x)
			}
		})
	}
}

func TestRememberFailureNeverChangesTheRun(t *testing.T) {
	for _, status := range []int{503, 403} {
		e := setup(t)
		spy, c := newCMISpy(t, status)
		e.h.Remember = rememberer(c)
		mm := &countMetrics{}
		e.h.Metrics = mm
		m := runRemembered(t, e, cmiAgent(), user("hello"), nil)
		if m.status != run.StatusCompleted || m.failCode != "" || m.last().Content != "Echo: hello" {
			t.Fatalf("%d: %s %q", status, m.status, m.failCode)
		}
		if want := map[int]int{503: 4, 403: 1}[status]; len(spy.bodies) != want {
			t.Fatalf("%d: %d attempts, want %d", status, len(spy.bodies), want)
		}
		if want := map[int]string{503: cmiclient.OutcomeFailed, 403: cmiclient.OutcomeRejected}[status]; len(mm.outcomes) != 1 || mm.outcomes[0] != want {
			t.Fatalf("%d: %v", status, mm.outcomes)
		}
	}
}

func TestRememberDoesNotDelayTheTerminalTransition(t *testing.T) {
	e := setup(t)
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release; w.WriteHeader(201) }))
	defer slow.Close()
	defer close(release)
	e.h.Remember = rememberer(&cmiclient.Client{Endpoint: slow.URL, HTTP: slow.Client()})
	m := newMem(cmiAgent(), user("hi"), run.Input{})
	e.h.Store = m
	done := make(chan struct{})
	go func() { e.h.Run(context.Background(), claim); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run waited for the CMI")
	}
	if m.status != run.StatusCompleted {
		t.Fatal(m.status)
	}
}

func TestRememberDelegationUsesTheRunsStoredCredentialBeforeTheRunEnds(t *testing.T) {
	e := setup(t)
	var subject string
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		subject = r.PostForm.Get("subject_token") + "|" + r.PostForm.Get("audience")
		_, _ = w.Write([]byte(`{"access_token":"writer-as-alice","expires_in":300}`))
	}))
	defer tokens.Close()
	spy, c := newCMISpy(t, 201)
	r := rememberer(c)
	r.Key = nil
	r.Exchanger = &credential.Exchanger{TokenEndpoint: tokens.URL, ClientID: "idp-host", ClientSecret: "s", Audience: "idp-cmi", HTTP: tokens.Client()}
	e.h.Remember = r
	runRemembered(t, e, cmiAgent(), user("hello"), nil) // memStore.Credential answers "tok"
	if subject != "tok|idp-cmi" || len(spy.auths) != 1 || spy.auths[0] != "Bearer writer-as-alice" {
		t.Fatalf("%q %v", subject, spy.auths)
	}
}

func TestRememberDelegationWithoutAStoredCredentialWritesNothing(t *testing.T) {
	e := setup(t)
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("no exchange without a subject token") }))
	defer tokens.Close()
	spy, c := newCMISpy(t, 201)
	r := rememberer(c)
	r.Key = nil
	r.Exchanger = &credential.Exchanger{TokenEndpoint: tokens.URL, ClientID: "idp-host", ClientSecret: "s", HTTP: tokens.Client()}
	e.h.Remember = r
	mm := &countMetrics{}
	e.h.Metrics = mm
	m := runRemembered(t, e, cmiAgent(), user("hello"), func(m *memStore) { m.cred = "" })
	if m.status != run.StatusCompleted || len(spy.bodies) != 0 || len(mm.outcomes) != 1 || mm.outcomes[0] != cmiclient.OutcomeNoToken {
		t.Fatalf("%s %v %v", m.status, spy.bodies, mm.outcomes)
	}
}

type factModel struct{ answer string }

func (factModel) Name() string { return "facts" }
func (f factModel) Complete(context.Context, model.Request, func(model.Delta)) (*model.Response, error) {
	r := &model.Response{}
	r.Message.Content = f.answer
	return r, nil
}

func factRememberer(c *cmiclient.Client, mode string) *cmiclient.Remember {
	r := rememberer(c)
	r.Mode = mode
	r.Extractor = &memfacts.Extractor{Provider: factModel{`{"facts":[{"key":"favourite_colour","value":"青色","statement":"用户最喜欢的颜色是青色"}]}`}, Model: "m"}
	return r
}

func TestRememberFactsModeStoresTheFactNotTheTurn(t *testing.T) {
	e := setup(t)
	spy, c := newCMISpy(t, 201)
	mt := &countMetrics{}
	e.h.Metrics = mt
	e.h.Remember = factRememberer(c, "facts")
	runRemembered(t, e, cmiAgent(), user("我最喜欢青色"), nil)
	var posts []string
	for _, b := range spy.bodies {
		if b != "" {
			posts = append(posts, b)
		}
	}
	if len(posts) != 1 || !strings.Contains(posts[0], `"key":"favourite_colour"`) || strings.Contains(posts[0], "The assistant answered") || strings.Contains(posts[0], "Echo:") {
		t.Fatalf("one fact, no turn and no answer: %v", posts)
	}
	if spy.idems[len(spy.idems)-1] != "host-fact-run_1-favourite_colour" || len(mt.outcomes) != 1 || mt.outcomes[0] != "fact_created" {
		t.Fatalf("%v %v", spy.idems, mt.outcomes)
	}
}

func TestRememberBothModeWritesTheTurnAndTheFact(t *testing.T) {
	e := setup(t)
	spy, c := newCMISpy(t, 201)
	mt := &countMetrics{}
	e.h.Metrics = mt
	e.h.Remember = factRememberer(c, "both")
	runRemembered(t, e, cmiAgent(), user("我最喜欢青色"), nil)
	turn, fact := false, false
	for _, b := range spy.bodies {
		turn = turn || strings.Contains(b, "The user said:")
		fact = fact || strings.Contains(b, `"key":"favourite_colour"`)
	}
	if !turn || !fact || len(mt.outcomes) != 2 {
		t.Fatalf("turn %v fact %v outcomes %v", turn, fact, mt.outcomes)
	}
}

func TestRememberFactsExtractionFailureNeverChangesTheRun(t *testing.T) {
	e := setup(t)
	_, c := newCMISpy(t, 201)
	r := factRememberer(c, "facts")
	r.Extractor = &memfacts.Extractor{Provider: factModel{"not json"}, Model: "m"}
	e.h.Remember = r
	m := runRemembered(t, e, cmiAgent(), user("hi"), nil)
	if m.status != run.StatusCompleted {
		t.Fatal(m.status)
	}
}
