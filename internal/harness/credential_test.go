package harness

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"idp-host/internal/credential"
	"idp-host/internal/run"
	"idp-host/tests/fixtures/fakemodel"
)

// renewer stands in for the authorization server's refresh grant (D38).
type renewer struct {
	mu    sync.Mutex
	calls []string
	next  func(rt string) (*credential.Token, error)
}

func (r *renewer) refresh(_ context.Context, rt string) (*credential.Token, error) {
	r.mu.Lock()
	r.calls = append(r.calls, rt)
	r.mu.Unlock()
	return r.next(rt)
}

func fresh(token, rt string) func(string) (*credential.Token, error) {
	return func(string) (*credential.Token, error) {
		return &credential.Token{AccessToken: token, ExpiresAt: time.Now().Add(10 * time.Minute), RefreshToken: rt}, nil
	}
}

// A credential about to expire is renewed before the gateway sees it, and the renewal is kept for the next turn.
func TestCredentialRenewedBeforeItExpires(t *testing.T) {
	e := setup(t)
	r := &renewer{next: fresh("tok-2", "rt-2")}
	e.h.Gateways["edge"].Refresh = r.refresh
	m := newMem(def(), user("please sum"), run.Input{})
	m.credExp, m.refresh = time.Now().Add(20*time.Second), "rt-1"
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if m.status != run.StatusCompleted || len(r.calls) != 1 || r.calls[0] != "rt-1" {
		t.Fatalf("%s, renewals %v", m.status, r.calls)
	}
	for _, b := range e.mcp.Bearers() {
		if b != "tok-2" {
			t.Fatalf("the gateway saw %q", b)
		}
	}
	if len(m.saved) != 1 || m.saved[0].AccessToken != "tok-2" || m.refresh != "rt-2" {
		t.Fatalf("the renewal was not kept: %+v %q", m.saved, m.refresh)
	}
}

// The gateway refuses a credential the HOST believed valid: renewed once, the call goes through.
func TestRefusedCredentialRenewedAndCallMadeAgain(t *testing.T) {
	e := setup(t)
	e.mcp.RequireBearer = "tok-2"
	r := &renewer{next: fresh("tok-2", "")}
	e.h.Gateways["edge"].Refresh = r.refresh
	m := newMem(def(), user("please sum"), run.Input{})
	m.credExp, m.refresh = time.Now().Add(time.Hour), "rt-1"
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if m.status != run.StatusCompleted || m.last().Content != "Result: 3" || len(r.calls) != 1 || m.refresh != "rt-1" {
		t.Fatalf("%s %q renewals %v refresh %q", m.status, m.last().Content, r.calls, m.refresh)
	}
}

// The session ends while the run works (the user signed out): the run stops at the next gateway call with the reason,
// instead of handing the model an error it cannot fix, again and again, until the step limit.
func TestRunEndsWhenTheSessionEnds(t *testing.T) {
	e := setup(t)
	clock := time.Now()
	e.h.Now = func() time.Time { return clock }
	r := &renewer{next: func(string) (*credential.Token, error) {
		return nil, errors.Join(credential.ErrSessionEnded, errors.New("invalid_grant"))
	}}
	e.h.Gateways["edge"].Refresh = r.refresh
	modelCalls := 0
	e.model.Script = func(msgs []fakemodel.Message, tools []string) fakemodel.Turn {
		modelCalls++
		clock = clock.Add(5 * time.Minute) // the model thinks for a while: the credential expires meanwhile
		return fakemodel.DefaultScript(msgs, tools)
	}
	m := newMem(def(), user("please sum"), run.Input{})
	m.credExp, m.refresh = clock.Add(3*time.Minute), "rt-1"
	e.h.Store = m
	err := e.h.Run(context.Background(), claim)
	var f *run.Failure
	if !errors.As(err, &f) || f.Code != run.FailCredential || !strings.Contains(f.Message, "session has ended") {
		t.Fatalf("%v", err)
	}
	if m.status != run.StatusFailed || modelCalls != 1 || len(e.mcp.Calls()) != 0 {
		t.Fatalf("%s, model %d, gateway calls %d", m.status, modelCalls, len(e.mcp.Calls()))
	}
	for _, c := range m.calls {
		if c.State != run.CallFailed || !strings.Contains(c.Result, "session has ended") {
			t.Fatalf("the call was not answered with the reason: %+v", c)
		}
	}
}

// Without a refresh token (a gateway not configured sessionBound) an expired credential ends the run as before, and a
// refusal that renewal does not cure ends it too.
func TestCredentialThatCannotBeRenewed(t *testing.T) {
	e := setup(t)
	m := newMem(def(), user("please sum"), run.Input{})
	m.credExp = time.Now().Add(-time.Second)
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err == nil || m.status != run.StatusFailed {
		t.Fatalf("an expired credential was used: %v %s", err, m.status)
	}
	e2 := setup(t)
	e2.mcp.RequireBearer = "something-else"
	r := &renewer{next: fresh("tok-2", "rt-2")}
	e2.h.Gateways["edge"].Refresh = r.refresh
	m2 := newMem(def(), user("please sum"), run.Input{})
	m2.credExp, m2.refresh = time.Now().Add(time.Hour), "rt-1"
	e2.h.Store = m2
	err := e2.h.Run(context.Background(), claim)
	var f *run.Failure
	if !errors.As(err, &f) || f.Code != run.FailCredential || len(r.calls) != 1 || m2.status != run.StatusFailed {
		t.Fatalf("%v %v %s", err, r.calls, m2.status)
	}
}
