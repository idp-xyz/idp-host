package harness

// D66: the question rewrite is a managed model call of the harness; whatever goes wrong with it, the original question is the query.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/cmiclient"
	"idp-host/internal/model"
	"idp-host/tests/fixtures/fakemodel"
)

func followUp() cmiclient.RewriteInput {
	return cmiclient.RewriteInput{Question: "and the second one?", History: []agui.Message{
		{Role: agui.RoleUser, Content: "what are the return rules for acme"}, {Role: agui.RoleAssistant, Content: "There are two rules: 30 days, and unopened."}}}
}

func rewriteOf(t *testing.T, e *env, d agent.Definition) cmiclient.RewriteOutcome {
	t.Helper()
	return e.h.RewriteQuestion(context.Background(), d, followUp())
}

func eventValue(ev agui.Event) map[string]any { return ev["value"].(map[string]any) }

func TestTheRewriteIsOneGovernedLowEffortCallWhoseAnswerBecomesTheQuery(t *testing.T) {
	e := setup(t)
	e.model.Rewrite = func(history []fakemodel.Message, q string) fakemodel.Turn {
		if len(history) != 2 || history[0].Role != "user" || history[1].Role != "assistant" || q != "and the second one?" {
			t.Errorf("the model was shown %+v and %q", history, q)
		}
		return fakemodel.Turn{Text: "  What is the second return rule for acme?  "}
	}
	out := rewriteOf(t, e, def())
	v := eventValue(out.Event)
	if out.Query != "What is the second return rule for acme?" || out.Event["name"] != agui.CustomContextRewrite ||
		v["outcome"] != cmiclient.RewriteRewritten || v["original"] != "and the second one?" || v["query"] != out.Query || v["mode"] != "model" || v["provider"] != "fake" || v["model"] != "fake-1" {
		t.Fatalf("%q %v", out.Query, v)
	}
	if _, ok := v["completionTokens"]; !ok {
		t.Errorf("the use is accounted for: %v", v)
	}
	if got := e.model.Efforts(); len(got) != 1 || got[0] != "low" {
		t.Errorf("thinking effort: %v", got)
	}
	if e.model.LastMaxTokens() != cmiclient.RewriteMaxOutput {
		t.Errorf("output limit %d", e.model.LastMaxTokens())
	}
	req := e.model.Last()
	if len(req) != 2 || !strings.HasPrefix(req[0].Content, cmiclient.RewriteMarker) || req[1].Role != "user" || len(e.model.LastTools()) != 0 {
		t.Errorf("%+v tools %v", req, e.model.LastTools())
	}
}

func TestTheRewriteFallsBackToTheOriginalWhateverGoesWrong(t *testing.T) {
	cases := []struct {
		name   string
		turn   fakemodel.Turn
		reason string
		fix    func(*env)
	}{
		{"empty answer", fakemodel.Turn{Text: "   "}, "empty", nil},
		{"two lines", fakemodel.Turn{Text: "The second rule?\nBecause you asked."}, "multiline", nil},
		{"too long", fakemodel.Turn{Text: strings.Repeat("a long question ", 30)}, "too_long", nil},
		{"cut by the output limit", fakemodel.Turn{Text: "What is the second return rule for", CutOff: true}, "too_long", nil},
		{"the model fails", fakemodel.Turn{Status: 500}, "error", nil},
		{"a blocked pattern", fakemodel.Turn{Text: "What is the SECRET return rule?"}, "guard", nil},
	}
	for _, c := range cases {
		e := setup(t)
		e.model.Rewrite = func([]fakemodel.Message, string) fakemodel.Turn { return c.turn }
		d := def()
		if c.reason == "guard" {
			d.Guards.Output.Patterns = []string{"SECRET"}
		}
		out := rewriteOf(t, e, d)
		v := eventValue(out.Event)
		if out.Query != "and the second one?" || v["outcome"] != cmiclient.RewriteFallback || v["reason"] != c.reason || v["original"] != "and the second one?" || v["query"] != "and the second one?" {
			t.Errorf("%s: %q %v", c.name, out.Query, v)
		}
	}
	// the answer that repeats the question is "unchanged", not a failure
	e := setup(t)
	e.model.Rewrite = func(_ []fakemodel.Message, q string) fakemodel.Turn { return fakemodel.Turn{Text: q} }
	if v := eventValue(rewriteOf(t, e, def()).Event); v["outcome"] != cmiclient.RewriteUnchanged {
		t.Errorf("%v", v)
	}
	// no provider to ask
	e = setup(t)
	d := def()
	d.Model.Provider = "nowhere"
	if out := rewriteOf(t, e, d); out.Query != "and the second one?" || eventValue(out.Event)["reason"] != "no_candidate" && eventValue(out.Event)["reason"] != "error" {
		t.Errorf("%q %v", out.Query, out.Event)
	}
}

// hangs is a provider that does not answer until it is told to stop.
type hangs struct{}

func (hangs) Name() string { return "hangs" }
func (hangs) Complete(ctx context.Context, _ model.Request, _ func(model.Delta)) (*model.Response, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestARewriteThatTakesTooLongIsGivenUp(t *testing.T) {
	e := setup(t)
	e.h.Providers = model.NewRegistry(hangs{})
	e.h.RewriteTimeout = 50 * time.Millisecond
	d := def()
	d.Model.Provider = "hangs"
	start := time.Now()
	out := rewriteOf(t, e, d)
	if time.Since(start) > 2*time.Second || out.Query != "and the second one?" || eventValue(out.Event)["reason"] != "timeout" {
		t.Fatalf("%v %q %v", time.Since(start), out.Query, out.Event)
	}
}

func TestTheNextCandidateAnswersWhenTheFirstOneFails(t *testing.T) {
	e := setup(t)
	d := def()
	d.Model = agent.ModelProfile{Provider: "down", Model: "x", Fallbacks: []agent.ModelRef{{Provider: "fake", Model: "fake-1"}}}
	e.model.Rewrite = func([]fakemodel.Message, string) fakemodel.Turn {
		return fakemodel.Turn{Text: "the second rule for acme?"}
	}
	out := rewriteOf(t, e, d)
	if v := eventValue(out.Event); out.Query != "the second rule for acme?" || v["provider"] != "fake" {
		t.Fatalf("%q %v", out.Query, v)
	}
}

func TestThePriorTurnsOfARequest(t *testing.T) {
	u := func(s string) agui.Message { return agui.Message{Role: agui.RoleUser, Content: s} }
	a := func(s string) agui.Message { return agui.Message{Role: agui.RoleAssistant, Content: s} }
	if _, ok := cmiclient.PriorTurns([]agui.Message{u("hello")}); ok {
		t.Error("a first message has no prior turns")
	}
	if _, ok := cmiclient.PriorTurns([]agui.Message{{Role: agui.RoleDeveloper, Content: "x"}, u("hello")}); ok {
		t.Error("a developer message is not a turn")
	}
	if _, ok := cmiclient.PriorTurns([]agui.Message{a("   "), u("hello")}); ok {
		t.Error("an empty message is not a turn")
	}
	in, ok := cmiclient.PriorTurns([]agui.Message{u("one"), a("two"), {Role: agui.RoleTool, Content: "tool"}, u("three")})
	if !ok || in.Question != "three" || len(in.History) != 2 || in.History[0].Content != "one" {
		t.Errorf("%+v %v", in, ok)
	}
	var long []agui.Message
	for i := 0; i < 10; i++ {
		long = append(long, u(strings.Repeat("x", i+1)), a("y"))
	}
	in, _ = cmiclient.PriorTurns(append(long, u("last")))
	if len(in.History) != cmiclient.RewriteMaxHistory || in.History[len(in.History)-1].Content != "y" {
		t.Errorf("%d", len(in.History))
	}
	// the prompt cuts a long earlier message
	p := cmiclient.RewriteInput{Question: "q", History: []agui.Message{u(strings.Repeat("é", 2000))}}.Prompt()
	if strings.Count(p, "é") != cmiclient.RewriteMessageChars || !strings.HasSuffix(p, "Last question:\nq") {
		t.Errorf("%d", len(p))
	}
	for _, c := range []struct{ in, q, why string }{{"a b", "a b", ""}, {" a ", "a", ""}, {"", "", "empty"}, {"a\r\nb", "", "multiline"}, {strings.Repeat("字", 301), "", "too_long"}, {strings.Repeat("字", 300), strings.Repeat("字", 300), ""}} {
		if q, why := cmiclient.CleanRewrite(c.in); q != c.q || why != c.why {
			t.Errorf("%q: %q %q", c.in, q, why)
		}
	}
}

type stepCounter struct {
	mu       sync.Mutex
	outcomes []string
}

func (s *stepCounter) StepFinished(_, _, outcome string, _ time.Duration, _ model.Usage) {
	s.mu.Lock()
	s.outcomes = append(s.outcomes, outcome)
	s.mu.Unlock()
}
func (*stepCounter) ToolCall(string, string, time.Duration) {}
func (*stepCounter) RunFinished(string)                     {}
func (*stepCounter) Guard(string, string)                   {}

func TestTheTenantsBlockedPatternsApplyToTheRewriteAndItsUseIsCounted(t *testing.T) {
	e := setup(t)
	sc := &stepCounter{}
	e.h.Metrics = sc
	e.model.Rewrite = func([]fakemodel.Message, string) fakemodel.Turn {
		return fakemodel.Turn{Text: "What is the TENANTBAN rule?"}
	}
	in := followUp()
	in.BlockedPatterns = []string{"TENANTBAN"}
	out := e.h.RewriteQuestion(context.Background(), def(), in)
	v := eventValue(out.Event)
	if out.Query != in.Question || v["reason"] != "guard" || out.Usage == nil || out.Usage.Provider != "fake" || out.Usage.CompletionTokens == 0 {
		t.Fatalf("%q %v %+v", out.Query, v, out.Usage)
	}
	if len(sc.outcomes) != 1 || sc.outcomes[0] != "rewrite" {
		t.Errorf("the step metric counts the call: %v", sc.outcomes)
	}
}

func TestTheRewriteDoesNotFeedTheRouter(t *testing.T) {
	e := setup(t)
	e.h.Router = model.NewRouter()
	d := def()
	d.Model.Routing = &agent.Routing{Strategy: agent.RouteLatency, Candidates: []agent.Candidate{{Provider: "fake", Model: "fake-1"}}}
	before, _ := e.h.Router.Order(d.Model, model.Needs{})
	e.model.Rewrite = func([]fakemodel.Message, string) fakemodel.Turn { return fakemodel.Turn{Status: 500} }
	e.h.RewriteQuestion(context.Background(), d, followUp())
	after, decisions := e.h.Router.Order(d.Model, model.Needs{})
	if len(before) != len(after) || fmt.Sprint(decisions) != fmt.Sprint(decisionsOf(e, d)) {
		t.Fatalf("%v", decisions)
	}
	for _, dec := range decisions {
		if dec.Reason != "ordered" && dec.Score != 0 {
			t.Errorf("the router learned from a rewrite: %+v", dec)
		}
	}
}

func decisionsOf(e *env, d agent.Definition) []model.Decision {
	_, ds := model.NewRouter().Order(d.Model, model.Needs{})
	return ds
}

func TestACanceledRewriteIsNotATimeoutAndNoProviderIsNoCandidate(t *testing.T) {
	e := setup(t)
	e.h.Providers = model.NewRegistry(hangs{})
	d := def()
	d.Model.Provider = "hangs"
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	out := e.h.RewriteQuestion(ctx, d, followUp())
	if v := eventValue(out.Event); v["reason"] != "canceled" {
		t.Errorf("%v", v)
	}
	d.Model.Provider = "nowhere"
	if v := eventValue(e.h.RewriteQuestion(context.Background(), d, followUp()).Event); v["reason"] != "no_candidate" {
		t.Errorf("%v", v)
	}
}

// failsWithUsage reports what it used and then fails: the run is still charged for it (D69).
type failsWithUsage struct{}

func (failsWithUsage) Name() string { return "fails" }
func (failsWithUsage) Complete(context.Context, model.Request, func(model.Delta)) (*model.Response, error) {
	return &model.Response{Usage: model.Usage{PromptTokens: 40, CompletionTokens: 5}}, &model.Error{Code: "server_error"}
}

func TestACandidateThatFailedAfterReportingUsageIsStillChargedFor(t *testing.T) {
	e := setup(t)
	e.h.Providers = model.NewRegistry(failsWithUsage{})
	d := def()
	d.Model.Provider = "fails"
	out := e.h.RewriteQuestion(context.Background(), d, followUp())
	if out.Usage == nil || out.Usage.PromptTokens != 40 || out.Usage.CompletionTokens != 5 || eventValue(out.Event)["outcome"] != cmiclient.RewriteFallback {
		t.Fatalf("%+v %v", out.Usage, out.Event)
	}
}
