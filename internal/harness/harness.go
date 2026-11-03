// Package harness is the HOST's reasoning loop (ADR-0001 D4): under a worker's claim it builds the
// model request from the release and the thread, calls the model, turns the model's tool calls
// into gateway calls (after the release's allow list and confirmation policy), records every step,
// message, tool call and usage as events, and ends the turn as completed, failed, canceled or
// waiting for the owner. It never executes a tool itself and never holds business state.
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/canonical"
	"idp-host/internal/mcpclient"
	"idp-host/internal/model"
	"idp-host/internal/run"
)

// Store is what the harness needs from the run store. Every method is fenced by the claim.
type Store interface {
	Context(ctx context.Context, c *run.Claim) (*run.Context, error)
	AppendEvents(ctx context.Context, c *run.Claim, events []agui.Event) error
	AppendMessages(ctx context.Context, c *run.Claim, msgs []agui.Message, events []agui.Event) error
	StartStep(ctx context.Context, c *run.Claim, maxSteps int, provider, model string) (step int, cancelRequested bool, err error)
	FinishStep(ctx context.Context, c *run.Claim, st run.Step, events []agui.Event) error
	RecordToolCalls(ctx context.Context, c *run.Claim, calls []run.ToolCall, events []agui.Event) error
	UpdateToolCall(ctx context.Context, c *run.Claim, id, state, result string, isError bool, events []agui.Event) error
	Finish(ctx context.Context, c *run.Claim, status, failureCode, failureMessage string, events []agui.Event) error
	Credential(ctx context.Context, c *run.Claim) (string, error)
}

// Gateway is one configured MCP Infrastructure gateway.
type Gateway struct {
	Name     string
	Endpoint string
	Client   *mcpclient.Client
	// StaticBearer, when set, is used instead of the run's delegated credential (development).
	StaticBearer string
}

// Metrics receives what the harness observes; nil disables.
type Metrics interface {
	StepFinished(provider, model, outcome string, latency time.Duration, usage model.Usage)
	ToolCall(gateway, outcome string, latency time.Duration)
	RunFinished(status string)
}

// Harness runs claims.
type Harness struct {
	Store     Store
	Providers *model.Registry
	Gateways  map[string]*Gateway
	Metrics   Metrics
	Log       *slog.Logger
	Now       func() time.Time

	DeltaFlush      time.Duration // how often streamed text is persisted (default 50 ms)
	ToolTimeout     time.Duration // per tool call (default 60 s)
	ToolResultLimit int           // bytes of tool output handed to the model (default 64 KiB)
	NewMessageID    func() string
}

func (h *Harness) defaults() {
	if h.DeltaFlush == 0 {
		h.DeltaFlush = 50 * time.Millisecond
	}
	if h.ToolTimeout == 0 {
		h.ToolTimeout = 60 * time.Second
	}
	if h.ToolResultLimit == 0 {
		h.ToolResultLimit = 64 << 10
	}
	if h.NewMessageID == nil {
		h.NewMessageID = model.NewMessageID
	}
	if h.Now == nil {
		h.Now = time.Now
	}
	if h.Log == nil {
		h.Log = slog.Default()
	}
}

type turn struct {
	h        *Harness
	ctx      context.Context
	claim    *run.Claim
	rc       *run.Context
	def      agent.Definition
	messages []agui.Message
	gateway  *Gateway
	bearer   string
	tools    []model.ToolSpec
	frontend map[string]bool
	visible  map[string]bool
}

// Run executes one claim to its next resting state. It returns run.ErrFenced when the claim was
// lost; any other error means the run was finished as failed (or the failure could not be stored).
func (h *Harness) Run(ctx context.Context, c *run.Claim) error {
	h.defaults()
	rc, err := h.Store.Context(ctx, c)
	if err != nil {
		if errors.Is(err, run.ErrFenced) {
			return err
		}
		return h.fail(ctx, c, run.FailReleaseGone, "the run's release cannot be loaded: "+err.Error())
	}
	t := &turn{h: h, ctx: ctx, claim: c, rc: rc, def: rc.Release.Definition, messages: rc.Messages, frontend: map[string]bool{}, visible: map[string]bool{}}
	for _, ft := range rc.Run.Input.Tools {
		t.frontend[ft.Name] = true
	}
	// a previous turn's confirmations
	for _, tc := range rc.ToolCalls {
		if tc.State == run.CallPendingConfirmation {
			return h.Store.Finish(ctx, c, run.StatusAwaitingInput, "", "", nil) // nothing changed; keep waiting
		}
	}
	if err := t.prepareTools(); err != nil {
		return err
	}
	if err := t.resolveDecided(); err != nil {
		return err
	}
	return t.loop()
}

func (h *Harness) fail(ctx context.Context, c *run.Claim, code, msg string) error {
	if h.Metrics != nil {
		h.Metrics.RunFinished(run.StatusFailed)
	}
	if err := h.Store.Finish(ctx, c, run.StatusFailed, code, msg, []agui.Event{agui.RunError(msg, code)}); err != nil {
		return err
	}
	return &run.Failure{Code: code, Message: msg}
}

// prepareTools resolves the gateway credential and the tool catalog the model may use.
func (t *turn) prepareTools() error {
	h := t.h
	if len(t.def.Tools.Allow) > 0 {
		gw := h.Gateways[t.def.Tools.Gateway]
		if gw == nil {
			return h.fail(t.ctx, t.claim, run.FailGateway, "gateway "+t.def.Tools.Gateway+" is not configured")
		}
		t.gateway = gw
		bearer := gw.StaticBearer
		if bearer == "" {
			var err error
			if bearer, err = h.Store.Credential(t.ctx, t.claim); err != nil {
				var f *run.Failure
				if errors.As(err, &f) {
					return h.fail(t.ctx, t.claim, f.Code, f.Message)
				}
				return err
			}
		}
		t.bearer = bearer
		lctx, cancel := context.WithTimeout(t.ctx, h.ToolTimeout)
		tools, err := gw.Client.ListTools(lctx, gw.Endpoint, bearer)
		cancel()
		if err != nil {
			return h.fail(t.ctx, t.claim, run.FailGateway, "tools/list failed: "+sanitize(err))
		}
		for _, tool := range tools {
			if !t.def.Tools.Allows(tool.Name) {
				continue
			}
			t.visible[tool.Name] = true
			desc := tool.Description
			if desc == "" {
				desc = tool.Title
			}
			t.tools = append(t.tools, model.ToolSpec{Name: tool.Name, Description: desc, Parameters: tool.InputSchema})
		}
	}
	for _, ft := range t.rc.Run.Input.Tools {
		if t.visible[ft.Name] {
			h.Log.Warn("frontend tool shadows a gateway tool and is ignored", "tool", ft.Name)
			delete(t.frontend, ft.Name)
			continue
		}
		t.tools = append(t.tools, model.ToolSpec{Name: ft.Name, Description: ft.Description, Parameters: ft.Parameters})
	}
	return nil
}

func sanitize(err error) string {
	var rpc *mcpclient.RPCError
	var he *mcpclient.HTTPError
	switch {
	case errors.As(err, &rpc):
		return fmt.Sprintf("MCP error %d: %s", rpc.Code, rpc.Message)
	case errors.As(err, &he):
		return he.Error()
	}
	s := err.Error()
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// resolveDecided executes confirmed calls and answers rejected or expired ones.
func (t *turn) resolveDecided() error {
	for _, tc := range t.rc.ToolCalls {
		switch tc.State {
		case run.CallConfirmed, run.CallExecuting:
			if _, err := t.execute(tc); err != nil {
				return err
			}
		case run.CallRejected, run.CallExpired:
			reason := "the user rejected this tool call"
			if tc.State == run.CallExpired {
				reason = "the confirmation request expired"
			}
			if err := t.answer(tc, tc.State, "", reason); err != nil {
				return err
			}
		}
	}
	return nil
}

// answer appends the tool message for a call and marks the call resolved.
func (t *turn) answer(tc run.ToolCall, state, result, errText string) error {
	msg := agui.Message{ID: "tool_" + tc.ID, Role: agui.RoleTool, ToolCallID: tc.ID, Content: result, Error: errText}
	content := result
	if errText != "" {
		if content == "" {
			content = "error: " + errText
		}
	}
	if err := t.h.Store.UpdateToolCall(t.ctx, t.claim, tc.ID, state, content, errText != "", nil); err != nil {
		return err
	}
	if err := t.h.Store.AppendMessages(t.ctx, t.claim, []agui.Message{msg}, []agui.Event{agui.ToolCallResult(msg.ID, tc.ID, content)}); err != nil {
		return err
	}
	t.messages = append(t.messages, msg)
	return nil
}

// execute calls the gateway for a confirmed or automatic call and records the result.
func (t *turn) execute(tc run.ToolCall) (bool, error) {
	h := t.h
	if t.gateway == nil {
		return false, t.answer(tc, run.CallFailed, "", "no gateway configured")
	}
	if err := h.Store.UpdateToolCall(t.ctx, t.claim, tc.ID, run.CallExecuting, "", false, nil); err != nil {
		return false, err
	}
	start := h.Now()
	cctx, cancel := context.WithTimeout(t.ctx, h.ToolTimeout)
	res, err := t.gateway.Client.CallTool(cctx, t.gateway.Endpoint, t.bearer, tc.Tool, json.RawMessage(tc.Arguments))
	cancel()
	outcome := "ok"
	if err != nil {
		outcome = "error"
		if h.Metrics != nil {
			h.Metrics.ToolCall(t.gateway.Name, outcome, h.Now().Sub(start))
		}
		return false, t.answer(tc, run.CallFailed, "", "tool call failed: "+sanitize(err))
	}
	text := res.Text(h.ToolResultLimit)
	if res.IsError {
		outcome = "tool_error"
	}
	if h.Metrics != nil {
		h.Metrics.ToolCall(t.gateway.Name, outcome, h.Now().Sub(start))
	}
	if res.IsError {
		return false, t.answer(tc, run.CallFailed, text, "the tool reported an error")
	}
	return true, t.answer(tc, run.CallDone, text, "")
}

type streamer struct {
	mu      sync.Mutex
	t       *turn
	msgID   string
	started bool
	buf     strings.Builder
	last    time.Time
	err     error
	cancel  context.CancelFunc
}

func (s *streamer) delta(d model.Delta) {
	if d.Text == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return
	}
	s.buf.WriteString(d.Text)
	if s.t.h.Now().Sub(s.last) >= s.t.h.DeltaFlush {
		s.flushLocked()
	}
}

func (s *streamer) flushLocked() {
	if s.buf.Len() == 0 || s.err != nil {
		return
	}
	var events []agui.Event
	if !s.started {
		events = append(events, agui.TextMessageStart(s.msgID, agui.RoleAssistant))
		s.started = true
	}
	events = append(events, agui.TextMessageContent(s.msgID, s.buf.String()))
	s.buf.Reset()
	s.last = s.t.h.Now()
	if err := s.t.h.Store.AppendEvents(s.t.ctx, s.t.claim, events); err != nil {
		s.err = err
		s.cancel()
	}
}

func (s *streamer) finish() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushLocked()
	return s.err
}

// loop runs model steps until the run rests.
func (t *turn) loop() error {
	h := t.h
	for {
		if !t.rc.Run.DeadlineAt.IsZero() && !h.Now().Before(t.rc.Run.DeadlineAt) {
			return h.fail(t.ctx, t.claim, run.FailTimeout, "the run exceeded its time limit")
		}
		primary := t.def.Model
		step, cancelRequested, err := h.Store.StartStep(t.ctx, t.claim, t.def.Limits.MaxSteps, primary.Provider, primary.Model)
		if errors.Is(err, run.ErrStepLimit) {
			return h.fail(t.ctx, t.claim, run.FailStepLimit, fmt.Sprintf("the run reached its limit of %d steps", t.def.Limits.MaxSteps))
		}
		if err != nil {
			return err
		}
		if cancelRequested {
			return t.canceled()
		}
		resp, st, err := t.callModel(step)
		if err != nil {
			if errors.Is(err, run.ErrFenced) || t.ctx.Err() != nil {
				return run.ErrFenced
			}
			_ = h.Store.FinishStep(t.ctx, t.claim, st, nil)
			if h.Metrics != nil {
				h.Metrics.StepFinished(st.Provider, st.Model, "error", st.Latency, model.Usage{})
			}
			return h.fail(t.ctx, t.claim, run.FailModel, "model call failed: "+st.ErrorCode)
		}
		if h.Metrics != nil {
			h.Metrics.StepFinished(st.Provider, st.Model, st.StopReason, st.Latency, resp.Usage)
		}
		events := []agui.Event{}
		if resp.Message.Content != "" {
			events = append(events, agui.TextMessageEnd(resp.Message.ID))
		}
		for _, call := range resp.Message.ToolCalls {
			events = append(events, agui.ToolCallStart(call.ID, call.Function.Name, resp.Message.ID), agui.ToolCallArgs(call.ID, call.Function.Arguments), agui.ToolCallEnd(call.ID))
		}
		events = append(events, agui.Custom(agui.CustomUsage, map[string]any{"step": step, "provider": st.Provider, "model": st.Model,
			"promptTokens": resp.Usage.PromptTokens, "completionTokens": resp.Usage.CompletionTokens, "latencyMs": st.Latency.Milliseconds()}),
			agui.StepFinished(run.StepName(step)))
		if err := h.Store.FinishStep(t.ctx, t.claim, st, nil); err != nil {
			return err
		}
		if err := h.Store.AppendMessages(t.ctx, t.claim, []agui.Message{resp.Message}, events); err != nil {
			return err
		}
		t.messages = append(t.messages, resp.Message)
		if len(resp.Message.ToolCalls) == 0 {
			return t.complete()
		}
		rest, err := t.dispatch(step, resp.Message)
		if err != nil {
			return err
		}
		if rest != "" {
			return nil
		}
	}
}

func (t *turn) complete() error {
	if t.h.Metrics != nil {
		t.h.Metrics.RunFinished(run.StatusCompleted)
	}
	return t.h.Store.Finish(t.ctx, t.claim, run.StatusCompleted, "", "", []agui.Event{agui.RunFinished(t.rc.Run.ThreadID, t.rc.Run.ID, agui.OutcomeSuccess, nil)})
}

func (t *turn) canceled() error {
	if t.h.Metrics != nil {
		t.h.Metrics.RunFinished(run.StatusCanceled)
	}
	return t.h.Store.Finish(t.ctx, t.claim, run.StatusCanceled, "CANCELED", "canceled by the owner", []agui.Event{agui.RunError("canceled by the owner", "CANCELED")})
}

// callModel runs one model call with fallbacks, streaming text into the event log.
func (t *turn) callModel(step int) (*model.Response, run.Step, error) {
	h := t.h
	refs := append([]agent.ModelRef{{Provider: t.def.Model.Provider, Model: t.def.Model.Model}}, t.def.Model.Fallbacks...)
	st := run.Step{Step: step, Provider: refs[0].Provider, Model: refs[0].Model}
	var ctxMsgs []agui.Message
	for _, c := range t.rc.Run.Input.Context {
		if c.Value != "" {
			ctxMsgs = append(ctxMsgs, agui.Message{ID: "ctx", Role: agui.RoleDeveloper, Content: c.Description + "\n" + c.Value})
		}
	}
	msgs := append(ctxMsgs, t.messages...)
	var lastErr error
	for i, ref := range refs {
		p, ok := h.Providers.Get(ref.Provider)
		if !ok {
			lastErr = &model.Error{Code: "unknown_provider"}
			continue
		}
		st.Provider, st.Model = ref.Provider, ref.Model
		msgID := h.NewMessageID()
		mctx, cancel := context.WithCancel(t.ctx)
		s := &streamer{t: t, msgID: msgID, last: h.Now(), cancel: cancel}
		start := h.Now()
		resp, err := p.Complete(mctx, model.Request{Model: ref.Model, Instructions: t.def.Instructions, Messages: msgs, Tools: t.tools,
			Temperature: t.def.Model.Temperature, MaxOutputTokens: t.def.Model.MaxOutputTokens}, s.delta)
		cancel()
		st.Latency = h.Now().Sub(start)
		if ferr := s.finish(); ferr != nil {
			return nil, st, ferr // fenced while streaming
		}
		if err == nil {
			resp.Message.ID = msgID
			if resp.Message.Content != "" && !s.started { // non-streaming provider: emit the text now
				if err := h.Store.AppendEvents(t.ctx, t.claim, []agui.Event{agui.TextMessageStart(msgID, agui.RoleAssistant), agui.TextMessageContent(msgID, resp.Message.Content)}); err != nil {
					return nil, st, err
				}
			}
			st.PromptTokens, st.CompletionTokens, st.StopReason = resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.StopReason
			return resp, st, nil
		}
		lastErr = err
		var me *model.Error
		if errors.As(err, &me) {
			st.ErrorCode = me.Code
			if me.Code == "status" {
				st.ErrorCode = fmt.Sprintf("status_%d", me.Status)
			}
			if me.Code == "canceled" {
				return nil, st, err
			}
			if !me.Retryable && me.Code != "transport" {
				break
			}
		} else {
			st.ErrorCode = "error"
		}
		if i < len(refs)-1 {
			h.Log.Warn("model provider failed, trying fallback", "provider", ref.Provider, "code", st.ErrorCode)
		}
	}
	if st.ErrorCode == "" {
		st.ErrorCode = "error"
	}
	return nil, st, lastErr
}

// dispatch classifies the step's tool calls, executes the automatic ones and decides how the turn
// rests. It returns "" to continue the loop, or the status the run was left in.
func (t *turn) dispatch(step int, msg agui.Message) (string, error) {
	h := t.h
	var records []run.ToolCall
	var events []agui.Event
	var interrupts []map[string]any
	now := h.Now()
	for i, call := range msg.ToolCalls {
		args := call.Function.Arguments
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		canon, err := canonical.Canonical(json.RawMessage(args))
		if err != nil {
			canon = []byte(`{}`)
		}
		digest, _ := canonical.Digest(json.RawMessage(canon))
		tc := run.ToolCall{RunID: t.rc.Run.ID, ID: call.ID, Step: step, MessageID: msg.ID, Tool: call.Function.Name, Arguments: string(canon), ArgumentsSHA256: digest, CreatedAt: now}
		switch {
		case i >= t.def.Limits.MaxToolCallsPerStep:
			tc.State = run.CallDenied
			events = append(events, agui.Custom(agui.CustomToolDenied, map[string]any{"toolCallId": tc.ID, "tool": tc.Tool, "reason": "too many tool calls in one step"}))
		case t.frontend[tc.Tool]:
			tc.State = run.CallFrontend
		case !t.visible[tc.Tool]:
			tc.State = run.CallDenied
			events = append(events, agui.Custom(agui.CustomToolDenied, map[string]any{"toolCallId": tc.ID, "tool": tc.Tool, "reason": "tool not available to this agent"}))
		case t.def.Confirmation.For(tc.Tool) == agent.ConfirmConfirm:
			tc.State = run.CallPendingConfirmation
			tc.ConfirmationExpiresAt = now.Add(time.Duration(t.def.Limits.ConfirmationTimeoutSeconds) * time.Second)
			events = append(events, agui.Custom(agui.CustomConfirmationRequired, map[string]any{"toolCallId": tc.ID, "tool": tc.Tool, "arguments": json.RawMessage(canon), "expiresAt": tc.ConfirmationExpiresAt.UTC().Format(time.RFC3339)}))
			interrupts = append(interrupts, map[string]any{"type": "confirmation", "toolCallId": tc.ID, "tool": tc.Tool, "arguments": json.RawMessage(canon), "expiresAt": tc.ConfirmationExpiresAt.UTC().Format(time.RFC3339)})
		default:
			tc.State = run.CallConfirmed
		}
		records = append(records, tc)
	}
	if err := h.Store.RecordToolCalls(t.ctx, t.claim, records, events); err != nil {
		return "", err
	}
	frontend := false
	for _, tc := range records {
		switch tc.State {
		case run.CallDenied:
			reason := "tool not available to this agent"
			if len(records) > t.def.Limits.MaxToolCallsPerStep {
				reason = "too many tool calls in one step"
			}
			if err := t.answer(tc, run.CallDenied, "", reason); err != nil {
				return "", err
			}
		case run.CallConfirmed:
			if _, err := t.execute(tc); err != nil {
				return "", err
			}
		case run.CallFrontend:
			frontend = true
		}
	}
	if len(interrupts) > 0 {
		if err := h.Store.Finish(t.ctx, t.claim, run.StatusAwaitingInput, "", "", []agui.Event{agui.RunFinished(t.rc.Run.ThreadID, t.rc.Run.ID, agui.OutcomeInterrupt, interrupts)}); err != nil {
			return "", err
		}
		return run.StatusAwaitingInput, nil
	}
	if frontend {
		return run.StatusCompleted, t.complete()
	}
	return "", nil
}
