package harness

// Tests of D57: the spans of a run's turn, what they carry, and where the trace context is sent.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"idp-host/internal/agent"
	"idp-host/internal/model"
	"idp-host/internal/observability"
	"idp-host/internal/run"
	"idp-host/tests/fixtures/fakemodel"
)

const (
	reqTrace = "0af7651916cd43dd8448eb211c80319c"
	reqSpan  = "b7ad6b7169203331"
)

// traced gives the harness a tracer that records into memory.
func traced(e *env) *tracetest.InMemoryExporter {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	e.h.Tracer = tp.Tracer("test")
	return exp
}

// memFromRequest is a run created by a request whose trace context is the one above.
func memFromRequest(d agent.Definition, text string, in run.Input) *memStore {
	m := newMem(d, user(text), in)
	m.rc.Run.TraceID = reqTrace
	m.rc.Run.Traceparent = "00-" + reqTrace + "-" + reqSpan + "-01"
	return m
}

func byName(spans tracetest.SpanStubs, name string) tracetest.SpanStubs {
	var out tracetest.SpanStubs
	for _, s := range spans {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

func attr(s tracetest.SpanStub, key string) (attribute.Value, bool) {
	for _, a := range s.Attributes {
		if string(a.Key) == key {
			return a.Value, true
		}
	}
	return attribute.Value{}, false
}

// A run with two steps and a tool call: the turn continues the request's trace, each model call and the tool call are its
// children, and the attributes are what an operator looks for.
func TestTheSpanTreeOfARunWithTwoStepsAndAToolCall(t *testing.T) {
	e := setup(t)
	exp := traced(e)
	m := memFromRequest(def(), "please sum", run.Input{})
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil || m.status != run.StatusCompleted {
		t.Fatalf("%v %s", err, m.status)
	}
	spans := exp.GetSpans()
	turn, models, tools := byName(spans, "idp.run.turn"), byName(spans, "idp.model.call"), byName(spans, "idp.tool.call")
	if len(turn) != 1 || len(models) != 2 || len(tools) != 1 || len(spans) != 4 {
		t.Fatalf("%d turn, %d model, %d tool spans of %d", len(turn), len(models), len(tools), len(spans))
	}
	if turn[0].SpanContext.TraceID().String() != reqTrace || turn[0].Parent.SpanID().String() != reqSpan || !turn[0].Parent.IsRemote() {
		t.Errorf("the turn does not continue the request's trace: trace %s, parent %s", turn[0].SpanContext.TraceID(), turn[0].Parent.SpanID())
	}
	for _, s := range append(models, tools...) {
		if s.SpanContext.TraceID().String() != reqTrace || s.Parent.SpanID() != turn[0].SpanContext.SpanID() {
			t.Errorf("%s is not a child of the turn: %s", s.Name, s.Parent.SpanID())
		}
	}
	str := func(s tracetest.SpanStub, k string) string { v, _ := attr(s, k); return v.AsString() }
	num := func(s tracetest.SpanStub, k string) int64 { v, _ := attr(s, k); return v.AsInt64() }
	if str(turn[0], "idp.run.id") != "run_1" || str(turn[0], "idp.turn.outcome") != "ok" {
		t.Errorf("turn attributes %v", turn[0].Attributes)
	}
	for i, s := range models {
		if str(s, "idp.run.id") != "run_1" || num(s, "idp.step") != int64(i+1) || num(s, "idp.model.attempt") != 1 || str(s, "idp.model.provider") != "fake" ||
			str(s, "idp.model.name") != "fake-1" || str(s, "idp.model.outcome") != "ok" || num(s, "idp.model.prompt_tokens") == 0 {
			t.Errorf("model span %d attributes %v", i+1, s.Attributes)
		}
		// D54: the streamed answer has a first output and a first text
		for _, k := range []string{"idp.model.first_output_ms", "idp.model.first_text_ms", "idp.model.after_first_ms"} {
			if _, ok := attr(s, k); !ok && i == 1 {
				t.Errorf("model span %d lacks %s: %v", i+1, k, s.Attributes)
			}
		}
	}
	if str(models[0], "idp.model.stop_reason") != model.StopToolCalls || str(models[1], "idp.model.stop_reason") != model.StopEnd {
		t.Errorf("stop reasons %v %v", models[0].Attributes, models[1].Attributes)
	}
	if v, _ := attr(tools[0], "idp.tool.external"); str(tools[0], "idp.run.id") != "run_1" || str(tools[0], "idp.tool.name") != "sum" || str(tools[0], "idp.tool.gateway") != "edge" ||
		str(tools[0], "idp.tool.outcome") != "ok" || num(tools[0], "idp.step") != 1 || v.AsBool() {
		t.Errorf("tool span attributes %v", tools[0].Attributes)
	}
	if tools[0].Status.Code != 0 || models[0].Status.Code != 0 {
		t.Errorf("a span of a call that worked has an error status")
	}
}

// Nothing of the conversation is in a span: not the message, not the arguments, not the result, not the instructions.
func TestSpansCarryNoPromptArgumentsOrResults(t *testing.T) {
	e := setup(t)
	exp := traced(e)
	m := memFromRequest(def(), "please sum", run.Input{})
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	args := e.mcp.Calls()[0].Args
	rawArgs, _ := json.Marshal(args)
	var sink strings.Builder
	for _, s := range exp.GetSpans() {
		sink.WriteString(s.Name + " " + s.Status.Description + "\n")
		for _, a := range s.Attributes {
			sink.WriteString(string(a.Key) + "=" + a.Value.Emit() + "\n")
		}
		for _, ev := range s.Events {
			sink.WriteString(ev.Name + "\n")
		}
	}
	all := sink.String()
	for _, secret := range []string{"please sum", "help", string(rawArgs), "Echo", "=tok\n", "alice"} {
		if strings.Contains(all, secret) {
			t.Errorf("a span carries %q:\n%s", secret, all)
		}
	}
	for _, key := range []string{"prompt\n", "arguments", "result", "content", "token", "subject", "owner"} {
		if strings.Contains(strings.ToLower(all), "."+key) || strings.Contains(strings.ToLower(all), key+"=") {
			t.Errorf("an attribute named %q", key)
		}
	}
}

// A run created before the traceparent was kept has its trace id: the turn is in that trace.
func TestARunWithOnlyATraceIdJoinsThatTrace(t *testing.T) {
	e := setup(t)
	exp := traced(e)
	m := memFromRequest(def(), "hello", run.Input{})
	m.rc.Run.Traceparent = ""
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if turn := byName(exp.GetSpans(), "idp.run.turn"); len(turn) != 1 || turn[0].SpanContext.TraceID().String() != reqTrace || turn[0].Parent.SpanID().String() == reqSpan {
		t.Errorf("%+v", turn)
	}
	// a run with neither starts a trace of its own
	m = newMem(def(), user("hello"), run.Input{})
	e.h.Store = m
	exp.Reset()
	_ = e.h.Run(context.Background(), claim)
	if turn := byName(exp.GetSpans(), "idp.run.turn"); len(turn) != 1 || !turn[0].SpanContext.TraceID().IsValid() || turn[0].Parent.IsValid() {
		t.Errorf("%+v", turn)
	}
}

// A turn that ends failed says why, with the code and no message.
func TestAFailedModelCallAndTurnAreMarkedWithCodesOnly(t *testing.T) {
	e := setup(t)
	exp := traced(e)
	addProvider(t, e, "p1", 0, body(401, `{"error":{"message":"Incorrect API key provided: sk-secret-1234"}}`))
	m := memFromRequest(onlyModel("p1"), "hello", run.Input{})
	e.h.Store = m
	_ = e.h.Run(context.Background(), claim)
	spans := exp.GetSpans()
	models, turn := byName(spans, "idp.model.call"), byName(spans, "idp.run.turn")
	if len(models) != 1 || models[0].Status.Code != 1 || models[0].Status.Description != "status_401" {
		t.Fatalf("%+v", models)
	}
	if v, _ := attr(turn[0], "idp.run.failure_code"); len(turn) != 1 || v.AsString() != run.FailModel || turn[0].Status.Code != 1 {
		t.Errorf("%+v", turn)
	}
	var sink strings.Builder
	for _, s := range spans {
		sink.WriteString(s.Status.Description)
		for _, a := range s.Attributes {
			sink.WriteString(a.Value.Emit())
		}
	}
	if strings.Contains(sink.String(), "sk-secret") {
		t.Errorf("the provider's words are in a span")
	}
}

// capture records the traceparent of each request to a provider and lets the fake model answer.
type capture struct {
	mu  sync.Mutex
	got []string
}

func (c *capture) handle(_ int, _ http.ResponseWriter, r *http.Request) bool {
	c.mu.Lock()
	c.got = append(c.got, r.Header.Get("Traceparent"))
	c.mu.Unlock()
	return false
}

func (c *capture) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.got...)
}

// The model provider is sent the context of the model call's span when the host's configuration lists it, and nothing
// otherwise: a provider that is not listed (the default of one outside the enterprise) gets no traceparent at all, though
// the run is traced and the spans are recorded.
func TestTheTraceContextIsSentToAListedProviderOnly(t *testing.T) {
	for _, c := range []struct {
		name   string
		listed map[string]bool
		sent   bool
	}{
		{"listed (declared external: false, or the operator says so)", map[string]bool{"wire": true}, true},
		{"not listed (external by default)", map[string]bool{"other": true}, false},
		{"listed as false", map[string]bool{"wire": false}, false},
		{"no list", nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t)
			exp := traced(e)
			e.h.TraceProviders = c.listed
			cap := &capture{}
			addProvider(t, e, "wire", 0, cap.handle)
			m := memFromRequest(onlyModel("wire"), "hello", run.Input{})
			e.h.Store = m
			if err := e.h.Run(context.Background(), claim); err != nil || m.status != run.StatusCompleted {
				t.Fatalf("%v %s", err, m.status)
			}
			got := cap.all()
			if len(got) != 1 {
				t.Fatalf("%d requests", len(got))
			}
			models := byName(exp.GetSpans(), "idp.model.call")
			if len(models) != 1 {
				t.Fatalf("%d model spans", len(models))
			}
			if !c.sent {
				if got[0] != "" {
					t.Errorf("the provider was sent %q", got[0])
				}
				return
			}
			want := "00-" + reqTrace + "-" + models[0].SpanContext.SpanID().String() + "-01"
			if got[0] != want {
				t.Errorf("traceparent %q, want %q", got[0], want)
			}
		})
	}
}

// The gateway is sent the context of the tool call's span; a call to a tool the host knows to be external is made without
// it (idp-mcp D51 forbids it and the gateway enforces that; the host does not hand it the header either), though the call
// has its span.
func TestTheTraceContextIsSentToTheGatewayExceptForAnExternalTool(t *testing.T) {
	for _, external := range []bool{false, true} {
		e := setup(t)
		exp := traced(e)
		in := run.Input{}
		if external {
			e.h.Gateways["edge"].ExternalTools = []string{"sum"}
			in.ExternalTools = true
		}
		m := memFromRequest(def(), "please sum", in)
		e.h.Store = m
		if err := e.h.Run(context.Background(), claim); err != nil || m.status != run.StatusCompleted {
			t.Fatalf("external %v: %v %s", external, err, m.status)
		}
		tools := byName(exp.GetSpans(), "idp.tool.call")
		got := e.mcp.Traceparents()
		if len(tools) != 1 || len(got) != 1 {
			t.Fatalf("external %v: %d tool spans, %d calls", external, len(tools), len(got))
		}
		if v, _ := attr(tools[0], "idp.tool.external"); v.AsBool() != external {
			t.Errorf("external %v: attributes %v", external, tools[0].Attributes)
		}
		if external {
			if got[0] != "" {
				t.Errorf("a call to an external tool carried %q", got[0])
			}
			continue
		}
		if want := "00-" + reqTrace + "-" + tools[0].SpanContext.SpanID().String() + "-01"; got[0] != want {
			t.Errorf("traceparent %q, want %q", got[0], want)
		}
	}
}

// Without a Tracer there is no span, no header to a provider (listed or not) and no header to the gateway, and the helpers
// that would start one allocate nothing.
func TestWithoutATracerNothingIsRecordedOrSent(t *testing.T) {
	e := setup(t)
	e.h.TraceProviders = map[string]bool{"wire": true}
	cap := &capture{}
	addProvider(t, e, "wire", 0, cap.handle)
	m := memFromRequest(onlyModel("wire"), "hello", run.Input{})
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if got := cap.all(); len(got) != 1 || got[0] != "" {
		t.Errorf("provider requests %q", got)
	}
	m = memFromRequest(def(), "please sum", run.Input{})
	e.h.Store = m
	e.h.Tracer = nil
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if got := e.mcp.Traceparents(); len(got) != 1 || got[0] != "" {
		t.Errorf("gateway requests %q", got)
	}
	ctx := context.Background()
	tr := &turn{h: e.h, claim: claim}
	rc := &run.Context{}
	ref := agent.ModelRef{Provider: "wire", Model: "m"}
	if n := testing.AllocsPerRun(100, func() {
		c1, end1 := e.h.startTurn(ctx, rc)
		c2, end2 := tr.startModelSpan(c1, 1, 1, ref)
		end2("ok", nil, model.Timing{})
		end1(nil)
		_ = c2
	}); n != 0 {
		t.Errorf("%v allocations on the path without a tracer", n)
	}
	if n := testing.AllocsPerRun(100, func() {
		_, end := observability.StartToolCall(ctx, "sum")
		end("ok")
	}); n != 0 {
		t.Errorf("%v allocations for a tool call that is not traced", n)
	}
}

// A sub-agent's run continues the trace from the span of the turn that delegated.
func TestADelegatedRunIsGivenTheTurnsTraceContext(t *testing.T) {
	e := setup(t)
	exp := traced(e)
	d := def()
	d.Subagents = []agent.SubagentRef{{Name: "helper", Description: "A helper."}}
	d.Normalize()
	m := memFromRequest(d, "please delegate", run.Input{})
	m.rc.Release = &agent.Document{Definition: d, Subagents: []agent.EmbeddedSubagent{{Name: "helper", AgentID: "a-helper", Version: 1, Digest: "sha256:" + strings.Repeat("ab", 32)}}}
	e.model.Script = func([]fakemodel.Message, []string) fakemodel.Turn {
		return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: agent.SubagentToolName("helper"), Args: `{"input":"hi"}`}}}
	}
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	turn := byName(exp.GetSpans(), "idp.run.turn")
	if len(m.inputs) != 1 || len(turn) != 1 {
		t.Fatalf("%d child runs, %d turns", len(m.inputs), len(turn))
	}
	for _, in := range m.inputs {
		if want := "00-" + reqTrace + "-" + turn[0].SpanContext.SpanID().String() + "-01"; in.Traceparent != want {
			t.Errorf("child traceparent %q, want %q", in.Traceparent, want)
		}
	}
}
