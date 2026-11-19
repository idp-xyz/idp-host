//go:build repro

package harness

// These reproduce defects whose fix is not decided yet (R1: what to do with a tool call that was in progress when the
// worker stopped, and the two separate writes of an assistant message and its tool calls; R4: an answer cut off at the
// output limit). They fail today on purpose and are not part of `make test`. Run them with
// `go test -tags repro ./internal/harness/ ./tests/integration/`. The helpers they share with the tests of D45 and D46
// (runIn, setup, newMem) are in the untagged files.

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"idp-host/internal/agui"
	"idp-host/internal/run"
	"idp-host/tests/fixtures/fakemodel"
)

// crashStore is a store whose worker dies at a chosen write: from then on nothing is stored, as when the process is gone
// (the real store refuses a stale claim the same way: run.ErrFenced). Nothing is stored by the write it dies at.
type crashStore struct {
	*memStore
	dead         atomic.Bool
	dieOnState   string // UpdateToolCall to this state kills the worker (the write is lost)
	dieOnRecords bool   // RecordToolCalls kills the worker (the write is lost)
}

func (c *crashStore) UpdateToolCall(ctx context.Context, cl *run.Claim, id, state, result string, isErr bool, ev []agui.Event) error {
	if c.dead.Load() || (c.dieOnState != "" && state == c.dieOnState) {
		c.dead.Store(true)
		return run.ErrFenced
	}
	return c.memStore.UpdateToolCall(ctx, cl, id, state, result, isErr, ev)
}

func (c *crashStore) RecordToolCalls(ctx context.Context, cl *run.Claim, calls []run.ToolCall, ev []agui.Event) error {
	if c.dead.Load() || c.dieOnRecords {
		c.dead.Store(true)
		return run.ErrFenced
	}
	return c.memStore.RecordToolCalls(ctx, cl, calls, ev)
}

func (c *crashStore) AppendMessages(ctx context.Context, cl *run.Claim, msgs []agui.Message, ev []agui.Event) error {
	if c.dead.Load() {
		return run.ErrFenced
	}
	return c.memStore.AppendMessages(ctx, cl, msgs, ev)
}

func (c *crashStore) Finish(ctx context.Context, cl *run.Claim, status, code, msg string, ev []agui.Event) error {
	if c.dead.Load() {
		return run.ErrFenced
	}
	return c.memStore.Finish(ctx, cl, status, code, msg, ev)
}

// R1(a): a worker dies while a gateway tool call is EXECUTING; the next worker that claims the run must not execute the
// call again (ADR-0001 D3: tool calls are written before they run and are not repeated). Setup: a model that calls "sum"
// once; the first worker dies (a) after the gateway finished the call, before the result was stored, and (b) while the
// gateway is still running it. A second Run on the same store is the next worker. Expects: the gateway received "sum"
// exactly once and the run completed. Reproduction, not a fix.
func TestAToolCallThatWasExecutingWhenTheWorkerDiedIsNotExecutedAgain(t *testing.T) {
	for _, tc := range []string{"gateway finished, result lost", "gateway still running"} {
		t.Run(tc, func(t *testing.T) {
			e := setup(t)
			var started atomic.Int32
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			firstStarted := make(chan struct{})
			e.mcp.Tools[0].Handle = func(map[string]any) (string, bool) {
				if started.Add(1) == 1 {
					close(firstStarted)
					if tc == "gateway still running" {
						<-release
					}
				}
				return "3", false
			}
			m := newMem(def(), user("please sum"), run.Input{})
			cs := &crashStore{memStore: m}
			e.h.Store = cs
			ctx1, kill := context.WithCancel(context.Background())
			defer kill()
			if tc == "gateway finished, result lost" {
				cs.dieOnState = run.CallDone
			}
			done := runIn(e.h, ctx1)
			<-firstStarted
			if tc == "gateway still running" {
				cs.dead.Store(true) // the process is gone: the connection to the gateway drops, nothing is stored any more
				kill()
			}
			if end := <-done; !errors.Is(end.err, run.ErrFenced) && !errors.Is(end.err, context.Canceled) {
				t.Logf("first worker ended with %v", end.err)
			}
			if c := m.calls["call_1_0"]; c == nil || c.State != run.CallExecuting {
				t.Fatalf("setup: the call should rest as EXECUTING, got %+v", c)
			}
			// the next worker claims the run (the lease having expired) and resumes it
			e.h.Store = m
			if err := e.h.Run(context.Background(), claim); err != nil {
				t.Fatalf("second worker: %v", err)
			}
			if n := len(e.mcp.Calls()); n != 1 {
				t.Errorf("the gateway received tools/call sum %d times; want exactly 1 (final status %s)", n, m.status)
			}
			if m.status != run.StatusCompleted {
				t.Errorf("run status %s (%s), want completed", m.status, m.failCode)
			}
		})
	}
}

// R1(b): the assistant message with tool calls is stored in one transaction (AppendMessages) and the tool_call rows in
// another (RecordToolCalls). The worker dies between the two. Expects: the next worker either runs the tool once or
// does not leave the thread with an assistant message whose tool calls nothing answers; the run completes with the
// tool's result. The model double behaves like OpenAI and refuses (HTTP 400) a thread in which an assistant message with
// tool calls is not followed by tool messages. Reproduction, not a fix.
func TestACrashBetweenTheAssistantMessageAndItsToolCallRecordsDoesNotLoseTheToolCalls(t *testing.T) {
	e := setup(t)
	e.model.Script = func(messages []fakemodel.Message, tools []string) fakemodel.Turn {
		body := messages
		if n := len(body); n > 0 && strings.HasPrefix(body[n-1].Content, "Budget:") {
			body = body[:n-1]
		}
		for i, msg := range body {
			if msg.Role == "assistant" && len(msg.ToolCalls) > 0 && (i+1 >= len(body) || body[i+1].Role != "tool") {
				return fakemodel.Turn{Status: 400, ErrorCode: "invalid_request_error"} // tool_calls must be followed by tool messages
			}
		}
		return fakemodel.DefaultScript(messages, tools)
	}
	m := newMem(def(), user("please sum"), run.Input{})
	cs := &crashStore{memStore: m, dieOnRecords: true}
	e.h.Store = cs
	end := <-runIn(e.h, context.Background())
	if !errors.Is(end.err, run.ErrFenced) {
		t.Fatalf("setup: the first worker should have died at RecordToolCalls: %v", end.err)
	}
	last := m.rc.Messages[len(m.rc.Messages)-1]
	if last.Role != agui.RoleAssistant || len(last.ToolCalls) != 1 || len(m.calls) != 0 {
		t.Fatalf("setup: want an assistant message with one tool call and no tool_call rows, got %+v / %d rows", last, len(m.calls))
	}
	e.h.Store = m // the next worker
	err := e.h.Run(context.Background(), claim)
	if n := len(e.mcp.Calls()); n != 1 || m.status != run.StatusCompleted {
		t.Errorf("tool executed %d times (want 1); run status %s code %q err %v; tool_call rows %d; model requests %d",
			n, m.status, m.failCode, err, len(m.calls), e.model.Calls())
	}
}

// R4: the model's answer is cut off at the output limit (finish_reason "length", no tool calls). Expects: the run does not
// end as failed MODEL_ERROR (it completes with what was said, or the model is asked to continue). Setup: the fake model
// answers a partial text with finish_reason length. Reproduction, not a fix.
func TestAnAnswerCutOffAtTheOutputLimitDoesNotFailTheRun(t *testing.T) {
	e := setup(t)
	e.model.Script = func([]fakemodel.Message, []string) fakemodel.Turn {
		return fakemodel.Turn{Text: "The first three of the five parts of the report are", CutOff: true}
	}
	m := newMem(def(), user("write the report"), run.Input{})
	e.h.Store = m
	err := e.h.Run(context.Background(), claim)
	if m.status == run.StatusFailed {
		t.Errorf("run failed with %s after %d model call(s): %v; the partial answer %q was stored and streamed", m.failCode, e.model.Calls(), err, m.last().Content)
	}
}
