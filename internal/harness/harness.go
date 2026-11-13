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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"idp-host/internal/a2a"
	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/canonical"
	"idp-host/internal/cmiclient"
	"idp-host/internal/credential"
	"idp-host/internal/guard"
	"idp-host/internal/mcpclient"
	"idp-host/internal/model"
	"idp-host/internal/run"
)

// Store is what the harness needs from the run store. Every method is fenced by the claim.
type Store interface {
	Context(ctx context.Context, c *run.Claim) (*run.Context, error)
	AppendEvents(ctx context.Context, c *run.Claim, events []agui.Event) error
	AppendMessages(ctx context.Context, c *run.Claim, msgs []agui.Message, events []agui.Event) error
	StartStep(ctx context.Context, c *run.Claim, maxSteps int, maxTokensPerRun int64, provider, model string) (step int, cancelRequested bool, err error)
	FinishStep(ctx context.Context, c *run.Claim, st run.Step, events []agui.Event) error
	RecordToolCalls(ctx context.Context, c *run.Claim, calls []run.ToolCall, events []agui.Event) error
	UpdateToolCall(ctx context.Context, c *run.Claim, id, state, result string, isError bool, events []agui.Event) error
	Finish(ctx context.Context, c *run.Claim, status, failureCode, failureMessage string, events []agui.Event) error
	Credential(ctx context.Context, c *run.Claim) (string, error)
	FileTickets(ctx context.Context, c *run.Claim) (*run.FileTickets, error) // the member's file tickets (D37)
	// the run's delegated credential with its refresh token, and its renewal (D38)
	RunCredential(ctx context.Context, c *run.Claim) (*credential.Token, error)
	SaveCredential(ctx context.Context, c *run.Claim, tok *credential.Token) error
	// sub-agent runs (D22)
	CreateChildRun(ctx context.Context, c *run.Claim, in run.ChildRunInput) (*run.Run, error)
	ChildOutcome(ctx context.Context, c *run.Claim, childID string) (*run.ChildOutcome, error)
}

// Gateway is one configured MCP Infrastructure gateway.
type Gateway struct {
	Name     string
	Endpoint string
	Client   *mcpclient.Client
	// StaticBearer, when set, is used instead of the run's delegated credential (development).
	StaticBearer string
	// Refresh renews a session-bound delegated credential (D38); nil when the gateway's credential is not renewable.
	Refresh func(ctx context.Context, refreshToken string) (*credential.Token, error)
}

// credentialLeeway: a delegated credential this close to its expiry is renewed before a gateway call.
const credentialLeeway = time.Minute

// Metrics receives what the harness observes; nil disables.
type Metrics interface {
	StepFinished(provider, model, outcome string, latency time.Duration, usage model.Usage)
	ToolCall(gateway, outcome string, latency time.Duration)
	RunFinished(status string)
	Guard(kind, outcome string) // kind: input | output; outcome: flagged | blocked | redacted | classifier_error
}

// Harness runs claims.
type Harness struct {
	Store     Store
	Providers *model.Registry
	Gateways  map[string]*Gateway
	Metrics   Metrics
	Log       *slog.Logger
	Now       func() time.Time
	// Classifiers are the external output classifiers of the HOST configuration, by name (D28).
	Classifiers map[string]guard.Classifier
	// Router orders model candidates (D29); nil means a fresh router per harness.
	Router *model.Router
	// A2A delegates to remote agents (D30); nil disables remote sub-agents.
	A2A *a2a.Client
	// CMI verifies (and, with a static key, refreshes) the run's context package at each step (CMI D24);
	// nil disables. CMIKey is the HOST's own cmi-reader credential; without one a stale package can
	// only be reported or fail the run, not refreshed.
	CMI    *cmiclient.Client
	CMIKey string
	// CMIToken is the HOST's own credential for the CMI, minted and refreshed by itself (D36); set
	// instead of CMIKey, which is a token that stops working when it expires.
	CMIToken func(context.Context) (string, error)
	// Remember appends an episodic memory to the CMI when a run of a CMI-context agent completes
	// (D33); nil disables. The write happens after the terminal transition and never affects the run.
	Remember *cmiclient.Remember
	memWG    sync.WaitGroup

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
	if h.Router == nil {
		h.Router = model.NewRouter()
	}
}

type turn struct {
	contextVerified bool // the CMI package was checked (and found stale or unanswerable) once already (D24)
	h               *Harness
	ctx             context.Context
	claim           *run.Claim
	rc              *run.Context
	def             agent.Definition
	messages        []agui.Message
	gateway         *Gateway
	tools           []model.ToolSpec
	frontend        map[string]bool
	visible         map[string]bool
	subagents       map[string]agent.EmbeddedSubagent    // by tool name (D22)
	resources       map[string]map[string]agent.Resource // on-demand skill resources by skill and name (D23)
	waiting         bool                                 // a delegated call's child run has not ended yet
	output          *guard.Output                        // the tenant's and the release's output filter (D28)
	flagged         map[string]bool                      // untrusted content already reported in this turn (D27)
	fileProps       map[string]map[string]string         // gateway tool -> property -> fileAttachment | fileDelivery (D37)
	cred            *credential.Token                    // the run's delegated gateway credential, as last loaded or renewed (D38)
	tickets         *run.FileTickets                     // loaded at the first file-taking call
}

// Formats a gateway tool's input schema gives a property that takes a member's file (D37): the model writes
// attachment:N or output there, and the HOST puts the run's ticket in its place just before the call.
const (
	fileAttachment = "idp-attachment"
	fileDelivery   = "idp-delivery"
)

var attachmentRE = regexp.MustCompile(`^attachment:([1-9][0-9]?)$`)

// guardBlock ends a model call whose output the guard refused.
type guardBlock struct {
	rule   string
	detail string
}

func (g *guardBlock) Error() string { return "output guard: " + g.rule }

func (t *turn) guardMetric(kind, outcome string) {
	if t.h.Metrics != nil {
		t.h.Metrics.Guard(kind, outcome)
	}
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
	if run.Overdue(rc.Run.DeadlineAt, h.Now()) {
		// D35: a run claimed after its deadline does nothing more, not even a call its owner confirmed
		return h.fail(ctx, c, run.FailTimeout, "the run exceeded its time limit")
	}
	t := &turn{h: h, ctx: ctx, claim: c, rc: rc, def: rc.Release.Effective(), messages: rc.Messages, frontend: map[string]bool{}, visible: map[string]bool{},
		subagents: map[string]agent.EmbeddedSubagent{}, resources: rc.Release.OnDemandResources()}
	if rc.Run.Input.ToolExecution == run.ToolExecutionCaller {
		// ADR-0001 D19: the caller executes every tool. No gateway tools are offered (the release's
		// and its skills' alike), no sub-agents and no resources, and the run is one model call: a
		// final answer, or proposals the caller executes and answers in its next run.
		t.def.Tools = agent.ToolPolicy{Allow: []string{}}
		t.def.Limits.MaxSteps = 1
		t.resources = nil
	} else if rc.Run.Depth < t.def.Limits.MaxSubagentDepth {
		for _, sub := range rc.Release.Subagents {
			t.subagents[agent.SubagentToolName(sub.Name)] = sub
		}
	}
	if m := rc.Run.Input.MaxOutputTokens; m > 0 && (t.def.Model.MaxOutputTokens == 0 || m < t.def.Model.MaxOutputTokens) {
		t.def.Model.MaxOutputTokens = m
	}
	t.flagged = map[string]bool{}
	out, err := guard.NewOutput(rc.BlockedPatterns, t.def.Guards.Output.Patterns, t.def.Guards.Output.Redact)
	if err != nil {
		return h.fail(ctx, c, run.FailInternal, "output guard: "+err.Error())
	}
	t.output = out
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
	if t.waiting {
		return h.Store.Finish(ctx, c, run.StatusAwaitingSubagent, "", "", nil)
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
		bearer, err := t.gatewayBearer(false)
		if err != nil {
			return t.credentialFailure(nil, err)
		}
		lctx, cancel := context.WithTimeout(t.ctx, h.ToolTimeout)
		tools, err := gw.Client.ListTools(lctx, gw.Endpoint, bearer)
		cancel()
		if mcpclient.Unauthorized(err) { // D38: renewed once; a credential the gateway still refuses ends the run
			if bearer, err = t.gatewayBearer(true); err != nil {
				return t.credentialFailure(nil, err)
			}
			lctx, cancel := context.WithTimeout(t.ctx, h.ToolTimeout)
			tools, err = gw.Client.ListTools(lctx, gw.Endpoint, bearer)
			cancel()
			if mcpclient.Unauthorized(err) {
				return t.credentialFailure(nil, refusedCredential())
			}
		}
		if err != nil {
			return h.fail(t.ctx, t.claim, run.FailGateway, "tools/list failed: "+sanitize(err))
		}
		for _, tool := range tools {
			if !t.def.Tools.Allows(tool.Name) {
				continue
			}
			if agent.Reserved(tool.Name) {
				h.Log.Warn("gateway tool uses a reserved prefix and is ignored", "tool", tool.Name)
				continue
			}
			t.visible[tool.Name] = true
			if props := fileProperties(tool.InputSchema); len(props) > 0 {
				if t.fileProps == nil {
					t.fileProps = map[string]map[string]string{}
				}
				t.fileProps[tool.Name] = props
			}
			desc := tool.Description
			if desc == "" {
				desc = tool.Title
			}
			t.tools = append(t.tools, model.ToolSpec{Name: tool.Name, Description: desc, Parameters: tool.InputSchema})
		}
	}
	for _, ft := range t.rc.Run.Input.Tools {
		if t.visible[ft.Name] || agent.Reserved(ft.Name) {
			h.Log.Warn("frontend tool shadows a gateway or HOST tool and is ignored", "tool", ft.Name)
			delete(t.frontend, ft.Name)
			continue
		}
		t.tools = append(t.tools, model.ToolSpec{Name: ft.Name, Description: ft.Description, Parameters: ft.Parameters})
	}
	// HOST tools (D22, D23): sub-agents and on-demand skill resources, in release order
	for _, ref := range t.rc.Release.Definition.Subagents {
		name := agent.SubagentToolName(ref.Name)
		if _, ok := t.subagents[name]; !ok {
			continue
		}
		t.tools = append(t.tools, model.ToolSpec{Name: name, Description: ref.Description,
			Parameters: json.RawMessage(`{"type":"object","properties":{"input":{"type":"string","description":"The task for the agent, with everything it needs to know."}},"required":["input"],"additionalProperties":false}`)})
	}
	if len(t.resources) > 0 {
		var names []string
		for _, s := range t.rc.Release.Skills {
			for _, r := range s.Definition.Resources {
				if r.OnDemand {
					names = append(names, s.Name+"/"+r.Name)
				}
			}
		}
		t.tools = append(t.tools, model.ToolSpec{Name: agent.ResourceReadTool, Description: "Read a reference document published with one of your skills. Available: " + strings.Join(names, ", ") + ".",
			Parameters: json.RawMessage(`{"type":"object","properties":{"skill":{"type":"string"},"name":{"type":"string"}},"required":["skill","name"],"additionalProperties":false}`)})
	}
	return nil
}

// fileProperties returns the top-level properties of a tool's input schema that take a member's file.
func fileProperties(schema json.RawMessage) map[string]string {
	var s struct {
		Properties map[string]struct {
			Format string `json:"format"`
		} `json:"properties"`
	}
	if json.Unmarshal(schema, &s) != nil {
		return nil
	}
	var out map[string]string
	for name, p := range s.Properties {
		if p.Format == fileAttachment || p.Format == fileDelivery {
			if out == nil {
				out = map[string]string{}
			}
			out[name] = p.Format
		}
	}
	return out
}

// fileArguments puts the run's tickets in place of attachment:N and output in a file-taking call's arguments (D37). It
// returns the arguments to send, whether the call delivers a file, and a message for the model when the call cannot be
// made. The recorded call, the confirmation and the event log keep what the model wrote.
func (t *turn) fileArguments(tc run.ToolCall) (json.RawMessage, bool, string, error) {
	props := t.fileProps[tc.Tool]
	if len(props) == 0 {
		return json.RawMessage(tc.Arguments), false, "", nil
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
		return json.RawMessage(tc.Arguments), false, "", nil // the gateway reports malformed arguments
	}
	in := t.rc.Run.Input
	delivers := false
	for name, format := range props {
		raw, ok := args[name]
		if !ok {
			continue
		}
		var v string
		if json.Unmarshal(raw, &v) != nil {
			return nil, false, name + " must be a string", nil
		}
		if t.tickets == nil && (len(in.Attachments) > 0 || in.Delivery) {
			tk, err := t.h.Store.FileTickets(t.ctx, t.claim)
			if err != nil {
				return nil, false, "", err
			}
			if tk == nil {
				tk = &run.FileTickets{}
			}
			t.tickets = tk
		}
		var ticket string
		switch format {
		case fileAttachment:
			m := attachmentRE.FindStringSubmatch(v)
			if m == nil {
				return nil, false, fmt.Sprintf("%s must be attachment:N, one of the files attached to this request (%d attached)", name, len(in.Attachments)), nil
			}
			n, _ := strconv.Atoi(m[1])
			if n > len(in.Attachments) || t.tickets == nil || n > len(t.tickets.Read) {
				return nil, false, fmt.Sprintf("there is no %s: %d files are attached to this request", v, len(in.Attachments)), nil
			}
			ticket = t.tickets.Read[n-1]
		case fileDelivery:
			if v != "output" {
				return nil, false, name + " must be output", nil
			}
			if !in.Delivery || t.tickets == nil || t.tickets.Write == "" {
				return nil, false, "files cannot be delivered from this request", nil
			}
			ticket, delivers = t.tickets.Write, true
		}
		args[name], _ = json.Marshal(ticket)
	}
	out, err := json.Marshal(args)
	return out, delivers, "", err
}

// delivered reads what a delivering call returned (structuredContent.deliveredFile) as the file.delivered event.
func delivered(tc run.ToolCall, res *mcpclient.CallResult) (agui.Event, bool) {
	var s struct {
		File *struct {
			Ref       string `json:"ref"`
			Name      string `json:"name"`
			MediaType string `json:"mediaType"`
			Size      int64  `json:"size"`
			SHA256    string `json:"sha256"`
		} `json:"deliveredFile"`
	}
	if json.Unmarshal(res.StructuredContent, &s) != nil || s.File == nil || s.File.Ref == "" {
		return agui.Event{}, false
	}
	f := s.File
	return agui.Custom(agui.CustomFileDelivered, map[string]any{"toolCallId": tc.ID, "ref": f.Ref, "name": f.Name, "mediaType": f.MediaType, "size": f.Size, "sha256": f.SHA256}), true
}

// gatewayBearer returns the bearer for the gateway (D38): the static one, or the run's delegated credential, renewed with
// its refresh token when it expires within credentialLeeway (or always, when force). A credential that has expired
// and cannot be renewed, or a session that has ended, is a *run.Failure: the run ends with it.
func (t *turn) gatewayBearer(force bool) (string, error) {
	gw, h := t.gateway, t.h
	if gw.StaticBearer != "" {
		return gw.StaticBearer, nil
	}
	if t.cred == nil {
		tok, err := h.Store.RunCredential(t.ctx, t.claim)
		if err != nil {
			return "", err
		}
		t.cred = tok
	}
	now := h.Now()
	valid := t.cred.ExpiresAt.IsZero() || now.Before(t.cred.ExpiresAt)
	if !force && (t.cred.ExpiresAt.IsZero() || t.cred.ExpiresAt.Sub(now) > credentialLeeway) {
		return t.cred.AccessToken, nil
	}
	if gw.Refresh == nil || t.cred.RefreshToken == "" {
		if valid && !force {
			return t.cred.AccessToken, nil
		}
		return "", &run.Failure{Code: run.FailCredential, Message: "the delegated credential expired at " + t.cred.ExpiresAt.UTC().Format(time.RFC3339) +
			" and cannot be renewed: send the request again"}
	}
	rctx, cancel := context.WithTimeout(t.ctx, 15*time.Second)
	tok, err := gw.Refresh(rctx, t.cred.RefreshToken)
	cancel()
	if err != nil {
		if errors.Is(err, credential.ErrSessionEnded) {
			return "", &run.Failure{Code: run.FailCredential, Message: "the user's session has ended (signed out, idle too long or at its maximum length), " +
				"so the run can no longer act for them: sign in and send the request again"}
		}
		if valid && !force { // the authorization server did not answer: the current credential still works for now
			h.Log.Warn("renewing the delegated credential failed", "run", t.claim.RunID, "error", err)
			return t.cred.AccessToken, nil
		}
		return "", &run.Failure{Code: run.FailCredential, Message: "the delegated credential could not be renewed: " + err.Error()}
	}
	if err := h.Store.SaveCredential(t.ctx, t.claim, tok); err != nil {
		return "", err
	}
	t.cred = tok
	return tok.AccessToken, nil
}

func refusedCredential() error {
	return &run.Failure{Code: run.FailCredential, Message: "the gateway refuses the run's delegated credential even after renewing it"}
}

// credentialFailure ends the run for a credential it cannot use (D38), answering the call it was about to make (if any)
// with the reason; an error that is not a *run.Failure is returned as it is.
func (t *turn) credentialFailure(tc *run.ToolCall, err error) error {
	var f *run.Failure
	if !errors.As(err, &f) {
		return err
	}
	if tc != nil {
		if aerr := t.answer(*tc, run.CallFailed, "", f.Message); aerr != nil {
			return aerr
		}
	}
	return t.h.fail(t.ctx, t.claim, f.Code, f.Message)
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
		case run.CallDelegated:
			if err := t.collect(tc); err != nil {
				return err
			}
		}
	}
	return nil
}

// collect answers a delegated call from its child run when that run has ended; otherwise the turn
// keeps waiting.
func (t *turn) collect(tc run.ToolCall) error {
	out, err := t.h.Store.ChildOutcome(t.ctx, t.claim, tc.ChildRunID)
	if err != nil {
		return err
	}
	if !run.Terminal(out.Status) {
		t.waiting = true
		return nil
	}
	name := strings.TrimPrefix(tc.Tool, agent.SubagentToolPrefix)
	switch out.Status {
	case run.StatusCompleted:
		return t.answer(tc, run.CallDone, out.Output, "")
	case run.StatusCanceled:
		return t.answer(tc, run.CallFailed, "", "sub-agent "+name+" was canceled")
	default:
		return t.answer(tc, run.CallFailed, "", fmt.Sprintf("sub-agent %s failed: %s: %s", name, out.FailureCode, out.FailureMessage))
	}
}

// delegate creates the child run of a sub-agent call. The call rests as DELEGATED; a store refusal
// (quota, missing release) becomes the tool's error result and the model continues. A remote
// (A2A) agent is called synchronously instead: the answer is the tool result.
func (t *turn) delegate(tc run.ToolCall) error {
	sub := t.subagents[tc.Tool]
	var args struct {
		Input string `json:"input"`
	}
	if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil || strings.TrimSpace(args.Input) == "" {
		return t.answer(tc, run.CallFailed, "", "the input argument is required")
	}
	if sub.A2A != nil {
		if t.h.A2A == nil {
			return t.answer(tc, run.CallFailed, "", "remote agents are not enabled on this host")
		}
		if err := t.h.Store.UpdateToolCall(t.ctx, t.claim, tc.ID, run.CallExecuting, "", false, []agui.Event{agui.Custom(agui.CustomSubagentStarted, map[string]any{"toolCallId": tc.ID, "agent": sub.Name, "remote": sub.A2A.CardURL})}); err != nil {
			return err
		}
		cctx, cancel := context.WithTimeout(t.ctx, a2a.Deadline(t.h.Now(), t.rc.Run.DeadlineAt))
		start := t.h.Now()
		res, err := t.h.A2A.Send(cctx, sub.A2A.CardURL, run.ChildRunID(t.rc.Run.ID, tc.ID), "m_"+tc.ID, args.Input)
		cancel()
		if t.h.Metrics != nil {
			outcome := "ok"
			if err != nil {
				outcome = "error"
			}
			t.h.Metrics.ToolCall("a2a:"+sub.Name, outcome, t.h.Now().Sub(start))
		}
		if err != nil {
			if t.ctx.Err() != nil {
				return run.ErrFenced
			}
			return t.answer(tc, run.CallFailed, "", "remote agent "+sub.Name+": "+sanitize(err))
		}
		return t.answer(tc, run.CallDone, res.Text, "")
	}
	if _, err := t.h.Store.CreateChildRun(t.ctx, t.claim, run.ChildRunInput{ToolCallID: tc.ID, Agent: sub, Input: args.Input, Gateway: t.def.Tools.Gateway}); err != nil {
		if errors.Is(err, run.ErrFenced) || t.ctx.Err() != nil {
			return err
		}
		var ref *run.Refusal
		if errors.As(err, &ref) {
			return t.answer(tc, run.CallFailed, "", "delegation refused: "+ref.Message)
		}
		return err
	}
	t.waiting = true
	return nil
}

// readResource answers a resource__read call from the release's own content.
func (t *turn) readResource(tc run.ToolCall) error {
	var args struct {
		Skill string `json:"skill"`
		Name  string `json:"name"`
	}
	if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
		return t.answer(tc, run.CallFailed, "", "skill and name are required")
	}
	r, ok := t.resources[args.Skill][args.Name]
	if !ok {
		return t.answer(tc, run.CallFailed, "", fmt.Sprintf("no on-demand resource %s/%s", args.Skill, args.Name))
	}
	return t.answer(tc, run.CallDone, r.Content, "")
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

// execute runs a confirmed or automatic call: a sub-agent delegation, a resource read, or a gateway
// call whose result is recorded.
func (t *turn) execute(tc run.ToolCall) (bool, error) {
	h := t.h
	if _, ok := t.subagents[tc.Tool]; ok {
		return true, t.delegate(tc)
	}
	if tc.Tool == agent.ResourceReadTool && t.resources != nil {
		return true, t.readResource(tc)
	}
	if t.gateway == nil {
		return false, t.answer(tc, run.CallFailed, "", "no gateway configured")
	}
	args, delivers, refusal, err := t.fileArguments(tc)
	if err != nil {
		return false, err
	}
	if refusal != "" {
		return false, t.answer(tc, run.CallFailed, "", refusal)
	}
	if err := h.Store.UpdateToolCall(t.ctx, t.claim, tc.ID, run.CallExecuting, "", false, nil); err != nil {
		return false, err
	}
	bearer, err := t.gatewayBearer(false)
	if err != nil {
		return false, t.credentialFailure(&tc, err)
	}
	start := h.Now()
	cctx, cancel := context.WithTimeout(t.ctx, h.ToolTimeout)
	res, err := t.gateway.Client.CallTool(cctx, t.gateway.Endpoint, bearer, tc.Tool, args)
	cancel()
	if mcpclient.Unauthorized(err) {
		// D38: the gateway refused the credential before it reached the tool, so the call is made again once with a
		// renewed one; a refusal after that ends the run instead of handing the model an error it cannot fix
		if bearer, err = t.gatewayBearer(true); err != nil {
			return false, t.credentialFailure(&tc, err)
		}
		cctx, cancel := context.WithTimeout(t.ctx, h.ToolTimeout)
		res, err = t.gateway.Client.CallTool(cctx, t.gateway.Endpoint, bearer, tc.Tool, args)
		cancel()
		if mcpclient.Unauthorized(err) {
			return false, t.credentialFailure(&tc, refusedCredential())
		}
	}
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
	if delivers {
		if ev, ok := delivered(tc, res); ok {
			if err := h.Store.AppendEvents(t.ctx, t.claim, []agui.Event{ev}); err != nil {
				return false, err
			}
		}
	}
	return true, t.answer(tc, run.CallDone, text, "")
}

type streamer struct {
	mu      sync.Mutex
	t       *turn
	msgID   string
	started bool
	buf     strings.Builder
	full    strings.Builder // everything streamed so far (output guard, D28)
	out     *guard.Output
	strict  bool   // release nothing before the final checks
	blocked string // the blocked pattern that ended the stream
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
	if s.err != nil || s.blocked != "" {
		return
	}
	s.full.WriteString(d.Text)
	if rule := s.out.Check(s.full.String()); rule != "" {
		s.blocked = rule
		s.buf.Reset()
		s.cancel()
		return
	}
	if s.strict {
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
	chunk, n := s.out.Redact(s.buf.String())
	if n > 0 {
		s.t.guardMetric("output", "redacted")
	}
	events = append(events, agui.TextMessageContent(s.msgID, chunk))
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
		if run.Overdue(t.rc.Run.DeadlineAt, h.Now()) {
			return h.fail(t.ctx, t.claim, run.FailTimeout, "the run exceeded its time limit")
		}
		primary := t.def.Model
		step, cancelRequested, err := h.Store.StartStep(t.ctx, t.claim, t.def.Limits.MaxSteps, t.def.Limits.MaxTokensPerRun, primary.Provider, primary.Model)
		if errors.Is(err, run.ErrStepLimit) {
			return h.fail(t.ctx, t.claim, run.FailStepLimit, fmt.Sprintf("the run reached its limit of %d steps", t.def.Limits.MaxSteps))
		}
		var f *run.Failure
		if errors.As(err, &f) { // a budget decision (D15)
			return h.fail(t.ctx, t.claim, f.Code, f.Message)
		}
		if err != nil {
			return err
		}
		if cancelRequested {
			return t.canceled()
		}
		if step > 1 {
			if failed, err := t.verifyContext(); err != nil || failed {
				return err
			}
		}
		resp, st, err := t.callModel(step)
		if err != nil {
			if errors.Is(err, run.ErrFenced) || t.ctx.Err() != nil {
				return run.ErrFenced
			}
			var gb *guardBlock
			if errors.As(err, &gb) {
				_ = h.Store.FinishStep(t.ctx, t.claim, st, []agui.Event{agui.Custom(agui.CustomGuardBlocked, map[string]any{"step": step, "rule": gb.rule, "detail": gb.detail})})
				if h.Metrics != nil {
					h.Metrics.StepFinished(st.Provider, st.Model, "guard_blocked", st.Latency, model.Usage{})
				}
				t.guardMetric("output", "blocked")
				return h.fail(t.ctx, t.claim, run.FailGuard, "the output guard refused the answer: "+gb.rule)
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
	x, remember := t.exchange()
	subject := ""
	if remember && t.h.Remember.Exchanger != nil {
		// delegation needs the owner's stored token, which is only readable while the claim is live
		subject, _ = t.h.Store.Credential(t.ctx, t.claim)
	}
	if err := t.h.Store.Finish(t.ctx, t.claim, run.StatusCompleted, "", "", []agui.Event{agui.RunFinished(t.rc.Run.ThreadID, t.rc.Run.ID, agui.OutcomeSuccess, nil)}); err != nil {
		return err
	}
	if remember {
		t.h.remember(x, subject)
	}
	return nil
}

// exchange says whether this turn's run is to be remembered (D33) and what was said: a caller's run
// of an agent with context.cmi, owned by a subject, ending in a final answer (no tool calls) to a
// non-empty last user message. Sub-agent and evaluation runs are not remembered.
func (t *turn) exchange() (cmiclient.Exchange, bool) {
	r := t.h.Remember
	run := t.rc.Run
	if r == nil || !t.def.Context.CMI || run.Owner == "" || run.ParentRunID != "" || run.Purpose != "" || len(t.messages) == 0 {
		return cmiclient.Exchange{}, false
	}
	last := t.messages[len(t.messages)-1]
	if last.Role != agui.RoleAssistant || len(last.ToolCalls) > 0 || strings.TrimSpace(last.Content) == "" {
		return cmiclient.Exchange{}, false
	}
	user := ""
	for i := len(t.messages) - 1; i >= 0; i-- {
		if t.messages[i].Role == agui.RoleUser && strings.TrimSpace(t.messages[i].Content) != "" {
			user = t.messages[i].Content
			break
		}
	}
	if user == "" {
		return cmiclient.Exchange{}, false
	}
	now := time.Now
	if t.h.Now != nil {
		now = t.h.Now
	}
	return cmiclient.Exchange{RunID: run.ID, ThreadID: run.ThreadID, Agent: t.rc.Release.AgentName, Subject: run.Owner, Message: user, Answer: last.Content, At: now()}, true
}

// remember writes the memory off the run's path: a goroutine with its own deadline, so the stream's
// end is not delayed and a failure is only logged (without content) and counted.
func (h *Harness) remember(x cmiclient.Exchange, subject string) {
	h.memWG.Add(1)
	go func() {
		defer h.memWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		count := func(outcome string) {
			if m, ok := h.Metrics.(interface{ MemoryWrite(outcome string) }); ok {
				m.MemoryWrite(outcome)
			}
		}
		token := func(context.Context) (string, error) { return subject, nil }
		if h.Remember.Mode != "facts" { // turns (default) and both: the episodic record of the run (D33)
			outcome, err := h.Remember.Write(ctx, x, token)
			count(outcome)
			if err != nil {
				h.Log.Warn("memory not written", "run", x.RunID, "outcome", outcome, "error", sanitize(err))
			}
		}
		if h.Remember.Mode == "facts" || h.Remember.Mode == "both" { // the facts the user stated (D34)
			outcomes, err := h.Remember.Learn(ctx, x, token)
			for _, o := range outcomes {
				count(o)
			}
			if err != nil {
				h.Log.Warn("facts not learned", "run", x.RunID, "outcomes", outcomes, "error", sanitize(err))
			}
		}
	}()
}

// WaitMemory waits for the memory writes in flight (shutdown and tests).
func (h *Harness) WaitMemory() { h.memWG.Wait() }

func (t *turn) canceled() error {
	if t.h.Metrics != nil {
		t.h.Metrics.RunFinished(run.StatusCanceled)
	}
	return t.h.Store.Finish(t.ctx, t.claim, run.StatusCanceled, "CANCELED", "canceled by the owner", []agui.Event{agui.RunError("canceled by the owner", "CANCELED")})
}

// callModel runs one model call with fallbacks, streaming text into the event log.
func (t *turn) callModel(step int) (*model.Response, run.Step, error) {
	h := t.h
	st := run.Step{Step: step, Provider: t.def.Model.Provider, Model: t.def.Model.Model}
	msgs, err := t.guardedMessages()
	if err != nil {
		return nil, st, err
	}
	// D29: the router decides the attempt order from the call's needs and what it observed
	parts := []string{t.def.Instructions}
	for _, m := range msgs {
		parts = append(parts, m.Content)
	}
	for _, tool := range t.tools {
		parts = append(parts, tool.Description, string(tool.Parameters))
	}
	needs := model.Needs{Tools: len(t.tools) > 0, EstimatedInput: model.EstimateTokens(parts...), MaxOutputTokens: t.def.Model.MaxOutputTokens}
	refs, decisions := h.Router.Order(t.def.Model, needs)
	if len(refs) == 0 {
		st.ErrorCode = "no_candidate"
		return nil, st, &model.Error{Code: "no_candidate"}
	}
	if model.StrategyName(t.def.Model) != agent.RouteOrdered {
		if err := h.Store.AppendEvents(t.ctx, t.claim, []agui.Event{agui.Custom(agui.CustomModelRouted, map[string]any{"step": step, "strategy": model.StrategyName(t.def.Model),
			"estimatedInputTokens": needs.EstimatedInput, "order": decisions})}); err != nil {
			return nil, st, err
		}
	}
	st.Provider, st.Model = refs[0].Provider, refs[0].Model
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
		s := &streamer{t: t, msgID: msgID, last: h.Now(), cancel: cancel, out: t.output, strict: t.def.Guards.Output.Strict}
		start := h.Now()
		resp, err := p.Complete(mctx, model.Request{Model: ref.Model, Instructions: t.def.Instructions, Messages: msgs, Tools: t.tools,
			Temperature: t.def.Model.Temperature, MaxOutputTokens: t.def.Model.MaxOutputTokens}, s.delta)
		cancel()
		st.Latency = h.Now().Sub(start)
		h.Router.Observe(ref, st.Latency, err != nil && s.blocked == "")
		if ferr := s.finish(); ferr != nil {
			return nil, st, ferr // fenced while streaming
		}
		if s.blocked != "" {
			st.ErrorCode = "guard_blocked"
			return nil, st, &guardBlock{rule: s.blocked, detail: "the output matched a blocked pattern while streaming"}
		}
		if err == nil {
			resp.Message.ID = msgID
			st.PromptTokens, st.CompletionTokens, st.StopReason = resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.StopReason
			if gerr := t.guardOutput(resp); gerr != nil {
				st.ErrorCode = "guard_blocked"
				return nil, st, gerr
			}
			if resp.Message.Content != "" && !s.started { // non-streaming provider or strict mode: emit the text now
				if err := h.Store.AppendEvents(t.ctx, t.claim, []agui.Event{agui.TextMessageStart(msgID, agui.RoleAssistant), agui.TextMessageContent(msgID, resp.Message.Content)}); err != nil {
					return nil, st, err
				}
			}
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
		case !t.visible[tc.Tool] && !t.hostTool(tc.Tool):
			tc.State = run.CallDenied
			events = append(events, agui.Custom(agui.CustomToolDenied, map[string]any{"toolCallId": tc.ID, "tool": tc.Tool, "reason": "tool not available to this agent"}))
		case t.def.Confirmation.For(tc.Tool) == agent.ConfirmConfirm:
			tc.State = run.CallPendingConfirmation
			tc.ConfirmationExpiresAt = now.Add(time.Duration(t.def.Limits.ConfirmationTimeoutSeconds) * time.Second)
			if d := t.rc.Run.DeadlineAt; !d.IsZero() && d.Before(tc.ConfirmationExpiresAt) {
				tc.ConfirmationExpiresAt = d // D35: a decision after the run's deadline could not be used
			}
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
	if t.waiting {
		// the stream stays open: no RUN_FINISHED until the sub-agents end and this run resumes
		if err := h.Store.Finish(t.ctx, t.claim, run.StatusAwaitingSubagent, "", "", nil); err != nil {
			return "", err
		}
		return run.StatusAwaitingSubagent, nil
	}
	return "", nil
}

// hostTool reports whether the HOST itself serves the tool in this turn.
func (t *turn) hostTool(name string) bool {
	if _, ok := t.subagents[name]; ok {
		return true
	}
	return name == agent.ResourceReadTool && len(t.resources) > 0
}

// guardedMessages assembles what the model reads this step: the input-guard preamble when wrapping is
// on, the caller's context and the thread, with tool results and context passed through the
// release's input guard (D27). Findings are reported once per turn, on the run's stream.
func (t *turn) guardedMessages() ([]agui.Message, error) {
	in := t.def.Guards.Input
	var msgs []agui.Message
	if in.ToolResults != guard.ModeOff || in.Context != guard.ModeOff {
		msgs = append(msgs, agui.Message{ID: "guard", Role: agui.RoleDeveloper, Content: guard.Preamble})
	}
	var events []agui.Event
	report := func(key, source, id string, findings []guard.Finding, blocked bool) {
		if len(findings) == 0 || t.flagged[key] {
			return
		}
		t.flagged[key] = true
		outcome := "flagged"
		if blocked {
			outcome = "blocked"
		}
		t.guardMetric("input", outcome)
		events = append(events, agui.Custom(agui.CustomGuardFlagged, map[string]any{"source": source, "id": id, "outcome": outcome, "findings": findings}))
	}
	for i, c := range t.rc.Run.Input.Context {
		if c.Value == "" {
			continue
		}
		value, findings := guard.Apply(in.Context, "context", c.Value)
		report(fmt.Sprintf("ctx:%d", i), "context", c.Description, findings, in.Context == guard.ModeBlock)
		msgs = append(msgs, agui.Message{ID: "ctx", Role: agui.RoleDeveloper, Content: c.Description + "\n" + value})
	}
	if note := t.filesNote(); note != "" {
		msgs = append(msgs, agui.Message{ID: "files", Role: agui.RoleDeveloper, Content: note})
	}
	for _, m := range t.messages {
		if m.Role == agui.RoleTool && m.Content != "" && in.ToolResults != guard.ModeOff { // an error-only result is the HOST's own text
			content, findings := guard.Apply(in.ToolResults, "tool_result", m.Content)
			report("tool:"+m.ID, "tool_result", m.ToolCallID, findings, in.ToolResults == guard.ModeBlock)
			m.Content = content
		}
		msgs = append(msgs, m)
	}
	if len(events) > 0 {
		if err := t.h.Store.AppendEvents(t.ctx, t.claim, events); err != nil {
			return nil, err
		}
	}
	return msgs, nil
}

// filesNote tells the model about the member's files and where its own files can go (D37): names, types and sizes,
// never a ticket. It is said only when a gateway tool can take them.
func (t *turn) filesNote() string {
	in := t.rc.Run.Input
	if len(t.fileProps) == 0 || (len(in.Attachments) == 0 && !in.Delivery) {
		return ""
	}
	var takes, delivers bool
	for _, props := range t.fileProps {
		for _, f := range props {
			takes = takes || f == fileAttachment
			delivers = delivers || f == fileDelivery
		}
	}
	var b strings.Builder
	if takes && len(in.Attachments) > 0 {
		b.WriteString("The user attached these files to this request. A tool that takes a file accepts the reference on the left:\n")
		for i, a := range in.Attachments {
			fmt.Fprintf(&b, "attachment:%d  %s  %s  %s\n", i+1, strconv.Quote(a.Name), a.MediaType, humanSize(a.Size))
		}
	}
	if delivers && in.Delivery {
		b.WriteString("To give the user a file, deliver it with target output; they will see it on this request and can download it.\n")
	}
	return strings.TrimSpace(b.String())
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", (n+512)>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

// guardOutput applies the output guard to a completed answer (D28): blocked patterns over the whole
// text, redaction, then the external classifier. It rewrites the message's content in place.
func (t *turn) guardOutput(resp *model.Response) error {
	content := resp.Message.Content
	if content == "" {
		return nil
	}
	if rule := t.output.Check(content); rule != "" {
		return &guardBlock{rule: rule, detail: "the answer matched a blocked pattern"}
	}
	if redacted, n := t.output.Redact(content); n > 0 {
		resp.Message.Content = redacted
		t.guardMetric("output", "redacted")
	}
	og := t.def.Guards.Output
	if og.Classifier == "" {
		return nil
	}
	c := t.h.Classifiers[og.Classifier]
	var verdict guard.Verdict
	var err error
	if c == nil {
		err = fmt.Errorf("%w: classifier %s is not configured on this host", guard.ErrClassifier, og.Classifier)
	} else {
		cctx, cancel := context.WithTimeout(t.ctx, t.h.ToolTimeout)
		verdict, err = c.Classify(cctx, resp.Message.Content)
		cancel()
	}
	if err != nil {
		t.guardMetric("output", "classifier_error")
		if og.OnError == guard.OnErrorBlock {
			return &guardBlock{rule: "classifier:" + og.Classifier, detail: err.Error()}
		}
		t.h.Log.Warn("output classifier failed, answer released (onError=allow)", "classifier", og.Classifier, "error", err)
		return t.h.Store.AppendEvents(t.ctx, t.claim, []agui.Event{agui.Custom(agui.CustomGuardFlagged, map[string]any{"source": "classifier", "id": og.Classifier, "outcome": "error", "detail": err.Error()})})
	}
	if !verdict.Allow {
		return &guardBlock{rule: "classifier:" + og.Classifier, detail: strings.Join(verdict.Categories, ",")}
	}
	return nil
}

// verifyContext re-checks the run's CMI package before a later step (CMI ADR-0001 D24): a stale
// package is reported (continue), replaced by a fresh resolve (refresh, static credential only) or
// fails the run (fail), as the release's context.onStale says. A CMI that cannot answer is reported
// and the step proceeds; the package it drew on was valid when the run started.
// cmiKey is the HOST's own CMI credential: minted on demand when it is configured that way (D36), the
// static key otherwise, and "" when it cannot be obtained (the caller then skips what needs it).
func (h *Harness) cmiKey(ctx context.Context) string {
	if h.CMIToken == nil {
		return h.CMIKey
	}
	tok, err := h.CMIToken(ctx)
	if err != nil {
		return ""
	}
	return tok
}

func (t *turn) verifyContext() (bool, error) {
	pkgID := t.rc.Run.ContextPackageID
	if pkgID == "" || t.h.CMI == nil || t.contextVerified {
		return false, nil
	}
	key := t.h.cmiKey(t.ctx)
	if key == "" {
		return false, nil
	}
	v, err := t.h.CMI.Verify(t.ctx, key, pkgID)
	if err != nil {
		t.contextVerified = true // once per run; the CMI is not asked again
		return false, t.h.Store.AppendEvents(t.ctx, t.claim, []agui.Event{agui.Custom(agui.CustomContextStale, map[string]any{"packageId": pkgID, "verified": false, "reason": sanitize(err)})})
	}
	if !v.Stale {
		return false, nil
	}
	t.contextVerified = true
	mode := t.def.Context.OnStale
	if mode == "" {
		mode = "continue"
	}
	event := map[string]any{"packageId": pkgID, "verified": true, "staleItems": v.StaleItems, "redacted": v.Redacted, "onStale": mode}
	if mode == "fail" {
		if err := t.h.Store.AppendEvents(t.ctx, t.claim, []agui.Event{agui.Custom(agui.CustomContextStale, event)}); err != nil {
			return true, err
		}
		return true, t.h.fail(t.ctx, t.claim, run.FailContext, fmt.Sprintf("the context package %s went stale (%d items)", pkgID, len(v.StaleItems)))
	}
	if mode == "refresh" {
		query := ""
		for i := len(t.rc.Messages) - 1; i >= 0; i-- {
			if m := t.rc.Messages[i]; m.Role == agui.RoleUser && m.Content != "" {
				query = m.Content
				break
			}
		}
		if query != "" {
			req := cmiclient.Request{Query: query, Purpose: t.def.Context.Purpose, Kinds: t.def.Context.Kinds, Sources: t.def.Context.Sources}
			req.Scope = &struct {
				Kind string `json:"kind"`
				ID   string `json:"id,omitempty"`
			}{Kind: "thread", ID: t.rc.Run.ThreadID}
			if pkg, err := t.h.CMI.Resolve(t.ctx, key, req); err == nil {
				kept := t.rc.Run.Input.Context[:0:0]
				for _, c := range t.rc.Run.Input.Context {
					if !strings.HasPrefix(c.Description, "CMI ") {
						kept = append(kept, c)
					}
				}
				t.rc.Run.Input.Context = append(kept, cmiclient.ContextOf(pkg)...)
				event["refreshedPackageId"], event["refreshedItems"] = pkg.PackageID, len(pkg.Items)
			} else {
				event["refreshError"] = sanitize(err)
			}
		}
	}
	return false, t.h.Store.AppendEvents(t.ctx, t.claim, []agui.Event{agui.Custom(agui.CustomContextStale, event)})
}
