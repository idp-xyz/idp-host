package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"idp-host/internal/run"
)

// D57 against real PostgreSQL: the traceparent of the request that creates a run is stored with it; the worker's turn
// continues that trace; each model call and the tool call are its children; the gateway and (when listed) the model
// provider are sent the context of the call's span, the provider not otherwise; the run's representation does not grow.
func TestARunsTurnContinuesTheTraceOfTheRequestThatCreatedItOnPostgres(t *testing.T) {
	e := newEnv(t)
	exp := tracetest.NewInMemoryExporter()
	e.harness.Tracer = sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)).Tracer("it-worker")
	e.harness.TraceProviders = map[string]bool{} // "fake" is not listed: it is an external provider by default
	e.publish("assistant", definition("edge", []string{"sum", "echo"}, nil))
	const tid, sid = "0af7651916cd43dd8448eb211c80319c", "b7ad6b7169203331"
	r := e.must(202, "POST", "/v1/agents/assistant/runs", "carol", runBody("thr-trace", "please sum"), "Idempotency-Key", "tr-1", "Traceparent", "00-"+tid+"-"+sid+"-01")
	id := r["runId"].(string)
	if r["traceId"] != tid {
		t.Errorf("the run's traceId %v", r["traceId"])
	}
	if _, has := r["traceparent"]; has {
		t.Errorf("the run's representation carries the traceparent")
	}
	e.drive()
	if got := e.must(200, "GET", "/v1/runs/"+id, "carol", nil); got["status"] != run.StatusCompleted {
		t.Fatalf("%v %v", got["status"], got["failureCode"])
	}
	ctx := context.Background()
	c, err := pgx.Connect(ctx, e.ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	var stored string
	if err := c.QueryRow(ctx, `SELECT traceparent FROM runs WHERE id = $1`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	// the request's own span is the one the middleware made (the header's span is its parent), so the stored parent is not sid
	if !strings.HasPrefix(stored, "00-"+tid+"-") || strings.Contains(stored, "-"+sid+"-") {
		t.Errorf("stored traceparent %q", stored)
	}
	var turn, models, tools int
	var turnSpan string
	for _, s := range exp.GetSpans() {
		if s.SpanContext.TraceID().String() != tid {
			t.Errorf("span %s is in trace %s", s.Name, s.SpanContext.TraceID())
		}
		switch s.Name {
		case "idp.run.turn":
			turn++
			turnSpan = s.SpanContext.SpanID().String()
			if "00-"+tid+"-"+s.Parent.SpanID().String()+"-01" != stored {
				t.Errorf("the turn's parent %s is not the stored span of %q", s.Parent.SpanID(), stored)
			}
		case "idp.model.call":
			models++
		case "idp.tool.call":
			tools++
		}
	}
	if turn != 1 || models != 2 || tools != 1 {
		t.Errorf("%d turn, %d model, %d tool spans", turn, models, tools)
	}
	for _, s := range exp.GetSpans() {
		if s.Name != "idp.run.turn" && s.Parent.SpanID().String() != turnSpan {
			t.Errorf("%s is not a child of the turn", s.Name)
		}
	}
	if calls := e.mcp.Traceparents(); len(calls) != 1 || !strings.HasPrefix(calls[0], "00-"+tid+"-") {
		t.Errorf("tools/call traceparents %q", calls)
	}
	for _, tp := range e.model.Traceparents() {
		if tp != "" {
			t.Errorf("the model provider that is not listed was sent %q", tp)
		}
	}
	// the provider listed: it is sent the context of the model call
	e.harness.TraceProviders = map[string]bool{"fake": true}
	e.must(202, "POST", "/v1/agents/assistant/runs", "carol", runBody("thr-trace-2", "hello"), "Idempotency-Key", "tr-2", "Traceparent", "00-"+tid+"-"+sid+"-01")
	e.drive()
	sent := 0
	for _, tp := range e.model.Traceparents() {
		if strings.HasPrefix(tp, "00-"+tid+"-") {
			sent++
		}
	}
	if sent != 1 {
		t.Errorf("%d requests to the listed provider carried the trace: %q", sent, e.model.Traceparents())
	}
}
