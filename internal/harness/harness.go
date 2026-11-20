// Package harness is the HOST's reasoning loop (ADR-0001 D4): under a worker's claim it builds the
// model request from the release and the thread, calls the model, turns the model's tool calls
// into gateway calls (after the release's allow list and confirmation policy), records every step,
// message, tool call and usage as events, and ends the turn as completed, failed, canceled or
// waiting for the owner. It holds no business state. The only calls it makes without the model are
// D41's: at the step limit it delivers the files a run made in a sandbox workspace and never exported.
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"idp-host/internal/a2a"
	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/canonical"
	"idp-host/internal/cmiclient"
	"idp-host/internal/credential"
	"idp-host/internal/ctxwin"
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
	// CancelRequested reads whether the owner asked to cancel the run, without taking it; run.ErrFenced for a lost claim (D46).
	CancelRequested(ctx context.Context, c *run.Claim) (bool, error)
	FinishStep(ctx context.Context, c *run.Claim, st run.Step, events []agui.Event) error
	// AppendAssistantTurn stores a step's assistant message with the records of its tool calls in one transaction (D47).
	AppendAssistantTurn(ctx context.Context, c *run.Claim, msg agui.Message, calls []run.ToolCall, events []agui.Event) error
	RecordToolCalls(ctx context.Context, c *run.Claim, calls []run.ToolCall, events []agui.Event) error
	UpdateToolCall(ctx context.Context, c *run.Claim, id, state, result string, isError bool, events []agui.Event) error
	// AnswerToolCall sets a call's final state and result and appends its tool message, with the events, in one
	// transaction (D47): a stop between the two can no longer leave a finished call whose tool message is missing.
	//
	// D49: src, when not nil, is what the call's result adds to the run's sources. It is written in the same transaction, so
	// that a call is answered once and its sources are recorded once, however often a worker stops and the run is resumed.
	AnswerToolCall(ctx context.Context, c *run.Claim, id, state, result string, isError bool, msg agui.Message, src *run.SourceBatch, events []agui.Event) error
	// CheckSources compares the citation marks of the final answer with the run's sources, stores the result with the run and
	// appends idp.host/run.sources, in one transaction (D49). It does nothing, and writes no event, when the run has no
	// sources and nothing was dropped and the check found no unverified number.
	CheckSources(ctx context.Context, c *run.Claim, answer string) error
	// Release gives the claim up at once instead of letting the lease run out (D47): the run stays RUNNING, as when a worker
	// dies, but the next Claim may take it immediately. Fenced by the claim's epoch like every other write.
	Release(ctx context.Context, c *run.Claim) error
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
	// ExternalTools are patterns of the tools whose calls send content outside the enterprise (D44); the API screens runs
	// by them at creation and a child run is created under them. ConfirmTools are patterns of the tools whose calls
	// always wait for the member, whatever the release's confirmation policy says (D44; platform ADR-0003 D8).
	ExternalTools []string
	ConfirmTools  []string
	// NeverResendTools are patterns of the tools whose call that was in progress when a worker stopped is not sent again
	// by the worker that resumes the run, whatever the gateway's annotations say (D47).
	NeverResendTools []string
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

// interruptionMetrics is implemented by a Metrics that counts tool calls that were cut short (D47).
type interruptionMetrics interface {
	ToolInterrupted(resent bool, reason string)
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

	// Retry is the retry policy toward each provider by name (D45); a provider without an entry gets the defaults.
	Retry map[string]model.RetryPolicy
	// CancelPoll is how often a run in progress asks the store whether its owner asked to cancel it (default 1 s, D46).
	CancelPoll time.Duration

	DeltaFlush  time.Duration // how often streamed text is persisted (default 50 ms)
	ToolTimeout time.Duration // per tool call (default 60 s)
	// D40, D52: ToolResultLimit is the bytes of one tool result handed to the model (default 16 KiB), the model reads the
	// last KeepToolResults tool results whole (default 6), an older one longer than OldToolResultBytes (default 1200) is
	// shown as its head and tail. Left zero, they are derived from the model's context window when the configuration gives
	// one (ModelContext), otherwise they are D40's numbers; a value set here wins over the derived one, a value the operator
	// sets for a model wins over both.
	ToolResultLimit    int
	KeepToolResults    int
	OldToolResultBytes int
	// ModelContext is what the operator says about each model's context window and thresholds (D52), by "provider/model".
	// A model without an entry keeps D40's behaviour exactly.
	ModelContext map[string]ctxwin.Settings
	NewMessageID func() string
}

func (h *Harness) defaults() {
	if h.CancelPoll == 0 {
		h.CancelPoll = time.Second
	}
	if h.DeltaFlush == 0 {
		h.DeltaFlush = 50 * time.Millisecond
	}
	if h.ToolTimeout == 0 {
		h.ToolTimeout = 60 * time.Second
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
	// ctx is the worker's: the claim lives while it does, and every write to the store uses it. callCtx ends when ctx
	// does and also at the run's deadline and when the owner cancels (D46): every call to the model, a gateway, a remote
	// agent or the CMI uses it, so that none of them outlives the run, and what must still be written when it ends (the
	// step, the tool call, the end of the run) can be.
	ctx        context.Context
	callCtx    context.Context
	stopCalls  context.CancelCauseFunc
	attempts   int              // model requests made in the step in progress, retries and fallbacks included (D45)
	attemptLog []map[string]any // what each of those requests took and how it ended (D54)
	askedWait  time.Duration    // the longest Retry-After that was refused in the step in progress (D45)
	askedCap   time.Duration    // what the provider that asked it is waited for, at most
	claim      *run.Claim
	rc         *run.Context
	def        agent.Definition
	messages   []agui.Message
	gateway    *Gateway
	tools      []model.ToolSpec
	frontend   map[string]bool
	visible    map[string]bool
	stop       <-chan struct{}                      // closed when the worker was told to stop (D47); nil when there is no worker
	external   map[string]bool                      // gateway tool -> it sends content outside the enterprise, by the operator's patterns or the gateway's marker, as listed now (D44, D51)
	marked     map[string]bool                      // the part of external that is only the gateway's marker (D51): no pattern of the operator names it
	repeatable map[string]bool                      // gateway tool -> its annotations as listed now say that calling it again does no harm (D47)
	subagents  map[string]agent.EmbeddedSubagent    // by tool name (D22)
	resources  map[string]map[string]agent.Resource // on-demand skill resources by skill and name (D23)
	waiting    bool                                 // a delegated call's child run has not ended yet
	output     *guard.Output                        // the tenant's and the release's output filter (D28)
	flagged    map[string]bool                      // untrusted content already reported in this turn (D27)
	fileProps  map[string]map[string]string         // gateway tool -> property -> fileAttachment | fileDelivery (D37)
	cred       *credential.Token                    // the run's delegated gateway credential, as last loaded or renewed (D38)
	tickets    *run.FileTickets                     // loaded at the first file-taking call
	step       int                                  // the model step in progress (D41)
	salvageN   int                                  // host-initiated calls made while salvaging a file at the step limit
	lastResult *mcpclient.CallResult                // the gateway result of the call execute just finished
	limits     ctxwin.Limits                        // the thresholds of the model this run asks, from its window (D52)
	corr       ctxwin.Correction                    // the estimate's correction by the provider's usage reports (D52)
	lastView   string                               // the items shortened at the previous step, to record only a change
	sticky     map[string]int                       // the limit each item was cut to at an earlier step of this run (D52)
	viewRaw    float64                              // the raw estimate of the request now being sent
	viewFixed  float64                              // ... and corrected
	hint       string                               // what the budget note adds about the context this step
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
		return h.fail(ctx, c, run.FailTimeout, timeoutMessage)
	}
	t := &turn{h: h, ctx: ctx, claim: c, rc: rc, def: rc.Release.Effective(), messages: rc.Messages, stop: run.StopChan(ctx), frontend: map[string]bool{}, visible: map[string]bool{}, external: map[string]bool{}, marked: map[string]bool{}, repeatable: map[string]bool{},
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
	t.limits = h.limitsFor(t.def)
	defer t.watch()()
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
	return t.settle(t.proceed())
}

// proceed prepares the tools, resolves the decided calls and runs the loop.
func (t *turn) proceed() error {
	if t.stopping() {
		return errStopped // boundary 1 (D47): nothing has been done yet
	}
	if err := t.prepareTools(); err != nil {
		return err
	}
	if rest, err := t.repairLastTurn(); err != nil || rest != "" {
		return err
	}
	if err := t.resolveDecided(); err != nil {
		return err
	}
	if t.waiting {
		return t.h.Store.Finish(t.ctx, t.claim, run.StatusAwaitingSubagent, "", "", nil)
	}
	return t.loop()
}

// repairLastTurn handles data written before D47, in which a step could stop half written (D47). The thread ends with an
// assistant message that has tool calls, followed by the tool messages that were written. Two things can be missing:
//  1. the records of its calls (the message and the records were two transactions): they are created as not started and the
//     calls are classified and run as dispatch would have run them. Nothing of them was sent anywhere, so running them is
//     not a repetition. Their TOOL_CALL events were written with the message and are not written again.
//  2. the tool message of a call that is finished (finished_at set) in a final state (DONE, FAILED, DENIED, REJECTED,
//     EXPIRED; its state and its message were two transactions): the message is appended from the result stored with the call, so that the next
//     model request is not refused for a tool call without its answer. A call recorded as DENIED, REJECTED or EXPIRED
//     that is not finished yet was never answered: resolveDecided answers it, so it is not repaired here too.
//
// It returns the status the run was left in, "" to go on.
func (t *turn) repairLastTurn() (string, error) {
	// the assistant message is the last one that is not a tool message
	i := len(t.messages) - 1
	for i >= 0 && t.messages[i].Role == agui.RoleTool {
		i--
	}
	if i < 0 || t.messages[i].Role != agui.RoleAssistant || len(t.messages[i].ToolCalls) == 0 {
		return "", nil
	}
	msg := t.messages[i]
	answered := map[string]bool{}
	for _, m := range t.messages[i+1:] {
		answered[m.ToolCallID] = true
	}
	records := map[string]run.ToolCall{}
	for _, tc := range t.rc.LastCalls {
		if tc.MessageID == msg.ID {
			records[tc.ID] = tc
		}
	}
	var missing []agui.ToolCall
	for _, call := range msg.ToolCalls {
		tc, ok := records[call.ID]
		switch {
		case !ok:
			missing = append(missing, call)
		case !answered[call.ID] && finalState(tc.State) && !tc.FinishedAt.IsZero():
			content := tc.Result
			answer := agui.Message{ID: "tool_" + tc.ID, Role: agui.RoleTool, ToolCallID: tc.ID, Content: content}
			if tc.IsError {
				answer.Error = "the call did not succeed"
				if answer.Content == "" {
					answer.Content = "error: " + answer.Error
				}
			}
			t.h.Log.Warn("a finished tool call had no tool message; it is written from the stored result", "run", t.claim.RunID, "toolCall", tc.ID, "state", tc.State)
			if err := t.h.Store.AppendMessages(t.ctx, t.claim, []agui.Message{answer}, []agui.Event{agui.ToolCallResult(answer.ID, tc.ID, answer.Content)}); err != nil {
				return "", err
			}
			t.messages = append(t.messages, answer)
		}
	}
	if len(missing) == 0 {
		return "", nil
	}
	msg.ToolCalls = missing
	t.step = t.rc.Run.StepCount
	recs, events, interrupts := t.classify(t.step, msg)
	t.h.Log.Warn("the last assistant message had tool calls without records; they are created and run", "run", t.claim.RunID, "calls", len(recs))
	if err := t.h.Store.RecordToolCalls(t.ctx, t.claim, recs, events); err != nil {
		return "", err
	}
	return t.dispatch(recs, interrupts)
}

// finalState: the states a tool call ends in, after which its tool message is due (D47).
func finalState(state string) bool {
	switch state {
	case run.CallDone, run.CallFailed, run.CallDenied, run.CallRejected, run.CallExpired:
		return true
	}
	return false
}

const timeoutMessage = "the run exceeded its time limit"

// Why the calls of a run in progress were cut short (D46): the causes of t.callCtx.
var (
	errDeadline        = errors.New("the run's deadline passed")
	errCancelRequested = errors.New("the owner asked to cancel the run")
	// errInterrupted is what a step of the turn returns once it has recorded what it had to (the step's row, the call's
	// state) after t.callCtx ended; settle turns it into the end of the run.
	errInterrupted = errors.New("a call in progress was cut short")
	// errStopped is what a step returns when the worker was told to stop and it reached a boundary at which nothing is
	// half done (D47); settle hands the run over.
	errStopped = errors.New("the worker was told to stop")
)

// stopping reports whether the worker was told to stop (D47): nothing new may be started, what is in progress finishes.
func (t *turn) stopping() bool {
	select {
	case <-t.stop:
		return true
	default:
		return false
	}
}

// watch gives the turn its call context and returns the function that stops watching (D46). The context ends at the
// run's deadline (D35: a run that exceeded its time limit does nothing more, so nothing in progress may go on) and when
// the store says the owner asked to cancel: the store is asked every CancelPoll, because the worker's heartbeat comes
// only every third of the lease (10 s by default) and a cancel should be seen within about a second. A claim the store
// no longer honours ends it too: the next write would be refused anyway.
func (t *turn) watch() func() {
	t.callCtx, t.stopCalls = context.WithCancelCause(t.ctx)
	done := make(chan struct{})
	var timer *time.Timer
	if d := t.rc.Run.DeadlineAt; !d.IsZero() {
		timer = time.AfterFunc(d.Sub(t.h.Now()), func() { t.stopCalls(errDeadline) })
	}
	go func() {
		tick := time.NewTicker(t.h.CancelPoll)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.callCtx.Done():
				return
			case <-tick.C:
				switch requested, err := t.h.Store.CancelRequested(t.ctx, t.claim); {
				case errors.Is(err, run.ErrFenced):
					t.stopCalls(run.ErrFenced)
				case err != nil:
					t.h.Log.Warn("cancel check failed", "run", t.claim.RunID, "error", err)
				case requested:
					t.stopCalls(errCancelRequested)
				}
			}
		}
	}()
	return func() {
		close(done)
		if timer != nil {
			timer.Stop()
		}
		t.stopCalls(nil)
	}
}

// settle ends the run as the cause of an interrupted call says: failed RUN_TIMEOUT as the deadline check of the loop does
// (D35), canceled as the check at the start of a step does, and for a lost claim or a stopped worker nothing more
// is written (run.ErrFenced, as before).
func (t *turn) settle(err error) error {
	if errors.Is(err, errStopped) {
		// D47: the turn stopped at a boundary and everything it stored is consistent; the claim is given up so that the next
		// worker takes the run at once. Past the grace period t.ctx is gone and the lease runs out as after a crash.
		if t.ctx.Err() != nil {
			return run.ErrFenced
		}
		if rerr := t.h.Store.Release(t.ctx, t.claim); rerr != nil {
			return rerr
		}
		t.h.Log.Info("run handed over: the worker was told to stop", "run", t.claim.RunID)
		return nil
	}
	if !errors.Is(err, errInterrupted) {
		return err
	}
	switch cause := context.Cause(t.callCtx); {
	case errors.Is(cause, errDeadline):
		return t.h.fail(t.ctx, t.claim, run.FailTimeout, timeoutMessage)
	case errors.Is(cause, errCancelRequested):
		return t.canceled()
	}
	return run.ErrFenced
}

// ours reports whether the call context ended because of the deadline or a cancel, as opposed to the claim being lost
// or the worker stopping: only then is there something to record, and the store still honours the claim.
func (t *turn) ours() bool {
	cause := context.Cause(t.callCtx)
	return errors.Is(cause, errDeadline) || errors.Is(cause, errCancelRequested)
}

// interruptCode is the error code of the step a call cut short ends (D46): deadline or canceled.
func (t *turn) interruptCode() string {
	if errors.Is(context.Cause(t.callCtx), errDeadline) {
		return "deadline"
	}
	return "canceled"
}

// interruptedCall records the tool call that was running when t.callCtx ended as FAILED with a message that says it was
// interrupted (D46): the thread keeps the assistant message's tool calls answered, so that a later run on it is not
// refused by the provider, and whoever reads the call knows the gateway may or may not have done it. It always returns
// errInterrupted; tc is nil for a call that has no record (listing the tools, renewing a credential).
func (t *turn) interruptedCall(tc *run.ToolCall) error {
	if tc != nil && t.ours() {
		reason := t.interruptCode() // deadline | canceled
		if m, ok := t.h.Metrics.(interruptionMetrics); ok {
			m.ToolInterrupted(false, reason)
		}
		if err := t.answerWith(*tc, run.CallFailed, "", "interrupted: "+t.interruptReason()+" while this call was running; the tool may or may not have completed it",
			t.interruptedEvent(*tc, false, reason)); err != nil {
			return err
		}
	}
	return errInterrupted
}

func (t *turn) interruptReason() string {
	if errors.Is(context.Cause(t.callCtx), errDeadline) {
		return "the run's time limit passed"
	}
	return "the owner canceled the run"
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
		lctx, cancel := context.WithTimeout(t.callCtx, h.ToolTimeout)
		tools, err := gw.Client.ListTools(lctx, gw.Endpoint, bearer)
		cancel()
		if err != nil && t.callCtx.Err() != nil {
			return errInterrupted
		}
		if mcpclient.Unauthorized(err) { // D38: renewed once; a credential the gateway still refuses ends the run
			if bearer, err = t.gatewayBearer(true); err != nil {
				return t.credentialFailure(nil, err)
			}
			lctx, cancel := context.WithTimeout(t.callCtx, h.ToolTimeout)
			tools, err = gw.Client.ListTools(lctx, gw.Endpoint, bearer)
			cancel()
			if err != nil && t.callCtx.Err() != nil {
				return errInterrupted
			}
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
			if byPattern := agent.MatchesTool(gw.ExternalTools, tool.Name); byPattern || tool.External {
				t.external[tool.Name] = true
				t.marked[tool.Name] = tool.External && !byPattern
			}
			if tool.SafeToRepeat() {
				t.repeatable[tool.Name] = true
			}
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
	rctx, cancel := context.WithTimeout(t.callCtx, 15*time.Second)
	tok, err := gw.Refresh(rctx, t.cred.RefreshToken)
	cancel()
	if err != nil && t.callCtx.Err() != nil {
		return "", errInterrupted
	}
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

// toolCallError is what the model reads of a refused call: the JSON-RPC error and, when the gateway refused the
// arguments, its reason (for example a missing required property), so that the model can correct the call instead of
// repeating it. The run's file tickets never appear in it (D37).
func (t *turn) toolCallError(err error) string {
	msg := sanitize(err)
	var rpc *mcpclient.RPCError
	if errors.As(err, &rpc) && rpc.Code == -32602 && len(rpc.Data) > 0 {
		var d struct {
			Reason string `json:"reason"`
		}
		if json.Unmarshal(rpc.Data, &d) == nil && strings.TrimSpace(d.Reason) != "" {
			r := strings.TrimSpace(d.Reason)
			if len(r) > 600 {
				cut := 600
				for cut > 0 && !utf8.RuneStart(r[cut]) {
					cut--
				}
				r = r[:cut] + "…"
			}
			msg += ": " + r
		}
	}
	return t.redactTickets(msg)
}

// redactTickets removes the run's file tickets from text the model will read.
func (t *turn) redactTickets(s string) string {
	if t.tickets == nil {
		return s
	}
	for _, tk := range append(append([]string(nil), t.tickets.Read...), t.tickets.Write) {
		if tk != "" {
			s = strings.ReplaceAll(s, tk, "[file ticket]")
		}
	}
	return s
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
		case run.CallConfirmed:
			if _, err := t.execute(tc); err != nil {
				return err
			}
		case run.CallExecuting:
			if err := t.resumeExecuting(tc); err != nil {
				return err
			}
		case run.CallDenied:
			// recorded as denied, and the worker stopped before it was answered (D47)
			reason := tc.Result
			if reason == "" {
				reason = "tool not available to this agent"
			}
			if err := t.answer(tc, run.CallDenied, "", reason); err != nil {
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
		// D49: what the sub-agent found is a source of this run too, so that a number in this run's answer that came from the
		// sub-agent's text can be looked up here; the sub-agent's own list stays with its run
		return t.answerSourced(tc, run.CallDone, out.Output, "", out.Sources)
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
		cctx, cancel := context.WithTimeout(t.callCtx, a2a.Deadline(t.h.Now(), t.rc.Run.DeadlineAt))
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
			if t.callCtx.Err() != nil {
				return t.interruptedCall(&tc)
			}
			return t.answer(tc, run.CallFailed, "", "remote agent "+sub.Name+": "+sanitize(err))
		}
		return t.answer(tc, run.CallDone, res.Text, "")
	}
	if _, err := t.h.Store.CreateChildRun(t.ctx, t.claim, run.ChildRunInput{ToolCallID: tc.ID, Agent: sub, Input: args.Input, Gateway: t.def.Tools.Gateway, ExternalTools: t.externalToolPatterns()}); err != nil {
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
	return t.answerWith(tc, state, result, errText)
}

// answerWith is answer with events written in the same transaction as the call's new state.
func (t *turn) answerWith(tc run.ToolCall, state, result, errText string, events ...agui.Event) error {
	return t.answerSourced(tc, state, result, errText, nil, events...)
}

// answerSourced is answerWith that also records the sources the call's result carried (D49), in the same transaction.
func (t *turn) answerSourced(tc run.ToolCall, state, result, errText string, src *run.SourceBatch, events ...agui.Event) error {
	msg := agui.Message{ID: "tool_" + tc.ID, Role: agui.RoleTool, ToolCallID: tc.ID, Content: result, Error: errText}
	content := result
	if errText != "" {
		if content == "" {
			content = "error: " + errText
		}
	}
	// D47: the call's final state and its tool message are one write
	if err := t.h.Store.AnswerToolCall(t.ctx, t.claim, tc.ID, state, content, errText != "", msg, src, append(events, agui.ToolCallResult(msg.ID, tc.ID, content))); err != nil {
		return err
	}
	t.messages = append(t.messages, msg)
	return nil
}

// execute runs a confirmed or automatic call: a sub-agent delegation, a resource read, or a gateway
// call whose result is recorded.
func (t *turn) execute(tc run.ToolCall) (bool, error) {
	h := t.h
	t.lastResult = nil
	if t.stopping() {
		return false, errStopped // boundary 5 (D47): no call is started once the worker was told to stop; this one stays as it is
	}
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
	if errors.Is(err, errInterrupted) {
		return false, t.interruptedCall(&tc)
	}
	if err != nil {
		return false, t.credentialFailure(&tc, err)
	}
	start := h.Now()
	cctx, cancel := context.WithTimeout(t.callCtx, h.ToolTimeout)
	res, err := t.gateway.Client.CallTool(cctx, t.gateway.Endpoint, bearer, tc.Tool, args)
	cancel()
	if err != nil && t.callCtx.Err() != nil {
		return false, t.interruptedCall(&tc) // D46: the deadline or a cancel reached the call
	}
	if mcpclient.Unauthorized(err) {
		// D38: the gateway refused the credential before it reached the tool, so the call is made again once with a
		// renewed one; a refusal after that ends the run instead of handing the model an error it cannot fix
		if bearer, err = t.gatewayBearer(true); errors.Is(err, errInterrupted) {
			return false, t.interruptedCall(&tc)
		} else if err != nil {
			return false, t.credentialFailure(&tc, err)
		}
		cctx, cancel := context.WithTimeout(t.callCtx, h.ToolTimeout)
		res, err = t.gateway.Client.CallTool(cctx, t.gateway.Endpoint, bearer, tc.Tool, args)
		cancel()
		if err != nil && t.callCtx.Err() != nil {
			return false, t.interruptedCall(&tc)
		}
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
		return false, t.answer(tc, run.CallFailed, "", "tool call failed: "+t.toolCallError(err))
	}
	t.ensureLimits()
	text := t.redactTickets(res.Text(t.limits.ToolResultBytes))
	t.lastResult = res
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
	// D49: the citations of a tool that sends content outside the enterprise become sources of this run. The text the model
	// reads is the result's text, as before; what is recorded here is never given to a model.
	var src *run.SourceBatch
	if t.external[tc.Tool] {
		if b := run.ParseCitations(res.StructuredContent); !b.Empty() {
			src = &b
		}
	}
	return true, t.answerSourced(tc, run.CallDone, text, "", src)
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

// streamed reports whether any text was received: from then on the attempt cannot be replaced by another (D45).
func (s *streamer) streamed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.full.Len() > 0
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
			return h.fail(t.ctx, t.claim, run.FailTimeout, timeoutMessage)
		}
		if t.stopping() {
			return errStopped // boundary 2 (D47): every earlier step is stored and answered; no step is started
		}
		primary := t.def.Model
		step, cancelRequested, err := h.Store.StartStep(t.ctx, t.claim, t.def.Limits.MaxSteps, t.def.Limits.MaxTokensPerRun, primary.Provider, primary.Model)
		if errors.Is(err, run.ErrStepLimit) {
			// D41: a finished file can still be in the workspace. Deliver it before the run is failed, while
			// the write ticket is still there. A store or credential error already finished the run.
			n, serr := t.salvage()
			if serr != nil {
				return serr
			}
			return h.fail(t.ctx, t.claim, run.FailStepLimit, stepLimitMessage(t.def.Limits.MaxSteps, n))
		}
		var f *run.Failure
		if errors.As(err, &f) { // a budget decision (D15)
			return h.fail(t.ctx, t.claim, f.Code, f.Message)
		}
		if err != nil {
			return err
		}
		t.step = step
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
			if errors.Is(err, errStopped) {
				// boundary 3 (D47): the attempt that was in progress failed and no other is started; the step's row is closed
				// and the next worker starts a new step
				if ferr := h.Store.FinishStep(t.ctx, t.claim, st, nil); ferr != nil {
					return ferr
				}
				if h.Metrics != nil {
					h.Metrics.StepFinished(st.Provider, st.Model, st.ErrorCode, st.Latency, model.Usage{})
				}
				return err
			}
			if errors.Is(err, errInterrupted) {
				// D46: the deadline or a cancel cut the call short; the step's row says which, and the run ends as
				// the cause says (settle). A lost claim or a stopped worker writes nothing.
				if t.ours() {
					_ = h.Store.FinishStep(t.ctx, t.claim, st, nil)
					if h.Metrics != nil {
						h.Metrics.StepFinished(st.Provider, st.Model, st.ErrorCode, st.Latency, model.Usage{})
					}
				}
				return err
			}
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
			message := "model call failed: " + st.ErrorCode
			if t.askedWait > 0 {
				message += fmt.Sprintf(": the provider asked to wait %d seconds before the next request, longer than the %d seconds this host waits",
					int(t.askedWait.Round(time.Second)/time.Second), int(t.askedCap/time.Second))
			}
			if model.IsProviderStreamCode(st.ErrorCode) {
				// D50: the provider's words are not shown to the member; the host log holds them
				message += ": the model server reported an error while answering"
			}
			if st.ErrorCode == model.CodeContextLength {
				// D40: the thread itself is what does not fit, so the same request would be refused again
				message = "the conversation no longer fits the model's context window; start a new conversation, or ask for less at a time"
			}
			return h.fail(t.ctx, t.claim, run.FailModel, message)
		}
		if t.callCtx.Err() == nil {
			// D46: a cancel the owner asked for before the answer is stored wins over it; asked after, it comes too late
			// and the run completes. The poll is made here, besides the watcher's, so that an answer that arrives
			// within a poll interval of the cancel does not hide it.
			if requested, cerr := h.Store.CancelRequested(t.ctx, t.claim); errors.Is(cerr, run.ErrFenced) {
				return cerr
			} else if cerr == nil && requested {
				t.stopCalls(errCancelRequested)
			}
		}
		if t.callCtx.Err() != nil {
			st.ErrorCode = t.interruptCode()
			if t.ours() {
				if resp.Message.Content != "" { // its text was streamed: close it, it is not stored
					if aerr := h.Store.AppendEvents(t.ctx, t.claim, []agui.Event{agui.TextMessageEnd(resp.Message.ID)}); aerr != nil {
						return aerr
					}
				}
				_ = h.Store.FinishStep(t.ctx, t.claim, st, nil)
				if h.Metrics != nil {
					h.Metrics.StepFinished(st.Provider, st.Model, st.ErrorCode, st.Latency, resp.Usage)
				}
			}
			return t.settle(errInterrupted)
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
		events = append(events, agui.Custom(agui.CustomUsage, t.usageEvent(step, st, resp.Usage)),
			agui.StepFinished(run.StepName(step)))
		if err := h.Store.FinishStep(t.ctx, t.claim, st, nil); err != nil {
			return err
		}
		var calls []run.ToolCall
		var interrupts []map[string]any
		if len(resp.Message.ToolCalls) > 0 {
			// D47: the message and the records of its calls are written in one transaction
			var classified []agui.Event
			calls, classified, interrupts = t.classify(step, resp.Message)
			events = append(events, classified...)
			if err := h.Store.AppendAssistantTurn(t.ctx, t.claim, resp.Message, calls, events); err != nil {
				return err
			}
		} else if err := h.Store.AppendMessages(t.ctx, t.claim, []agui.Message{resp.Message}, events); err != nil {
			return err
		}
		t.messages = append(t.messages, resp.Message)
		if len(resp.Message.ToolCalls) == 0 {
			if resp.StopReason == model.StopLength {
				// D40: an answer the model could not finish is not an answer; say why instead of ending as completed
				return h.fail(t.ctx, t.claim, run.FailModel, fmt.Sprintf("the model's answer was cut off: its context window or output limit is full (%d input tokens at step %d)", resp.Usage.PromptTokens, step))
			}
			return t.complete()
		}
		rest, err := t.dispatch(calls, interrupts)
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
	// D49: the citation marks of the answer are checked against the sources before the run ends; this marks and never blocks
	if err := t.h.Store.CheckSources(t.ctx, t.claim, t.finalAnswer()); err != nil {
		return err
	}
	if err := t.h.Store.Finish(t.ctx, t.claim, run.StatusCompleted, "", "", []agui.Event{agui.RunFinished(t.rc.Run.ThreadID, t.rc.Run.ID, agui.OutcomeSuccess, nil)}); err != nil {
		return err
	}
	if remember {
		t.h.remember(x, subject)
	}
	return nil
}

// finalAnswer is the text of the run's last assistant message: what the citation check reads.
func (t *turn) finalAnswer() string {
	for i := len(t.messages) - 1; i >= 0; i-- {
		if t.messages[i].Role == agui.RoleAssistant {
			return t.messages[i].Content
		}
	}
	return ""
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
	if t.refuseUnfit(t.viewFixed) {
		// D52: the estimate says the request cannot fit the window even with everything shortened that may be: the model is
		// not called, since the answer would be D40's refusal, paid for
		st.ErrorCode = model.CodeContextLength
		h.Log.Warn("request cannot fit the model's context window; not sent", "run", t.claim.RunID, "step", step, "estimateTokens", int(t.viewFixed), "window", t.limits.Window)
		return nil, st, &model.Error{Code: model.CodeContextLength}
	}
	// D41: the last steps of a run that can deliver a file are offered only the delivery tools
	tools := t.modelTools()
	// D29: the router decides the attempt order from the call's needs and what it observed
	parts := []string{t.def.Instructions}
	for _, m := range msgs {
		parts = append(parts, m.Content)
	}
	for _, tool := range tools {
		parts = append(parts, tool.Description, string(tool.Parameters))
	}
	needs := model.Needs{Tools: len(tools) > 0, EstimatedInput: model.EstimateTokens(parts...), MaxOutputTokens: t.def.Model.MaxOutputTokens}
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
	t.attempts, t.askedWait, t.askedCap, t.attemptLog = 0, 0, 0, nil
	var lastErr error
candidates:
	for i, ref := range refs {
		p, ok := h.Providers.Get(ref.Provider)
		if !ok {
			lastErr = &model.Error{Code: "unknown_provider"}
			continue
		}
		st.Provider, st.Model = ref.Provider, ref.Model
		policy := h.retryPolicy(ref.Provider)
		for attempt := 1; ; attempt++ {
			t.attempts++
			msgID := h.NewMessageID()
			mctx, cancel := context.WithCancel(t.callCtx)
			s := &streamer{t: t, msgID: msgID, last: h.Now(), cancel: cancel, out: t.output, strict: t.def.Guards.Output.Strict}
			start := h.Now()
			watch := model.NewStopwatch(h.Now) // D54: the adapter says when the request was sent and when the first output came
			resp, err := p.Complete(mctx, model.Request{Model: ref.Model, Instructions: t.def.Instructions, Messages: msgs, Tools: tools,
				Temperature: t.def.Model.Temperature, MaxOutputTokens: t.def.Model.MaxOutputTokens, Watch: watch}, s.delta)
			cancel()
			st.Latency = h.Now().Sub(start)
			timing := watch.Timing()
			t.logAttempt(ref, st.Latency, timing, err, s.blocked != "")
			cut := t.callCtx.Err() != nil // the deadline, a cancel or a stopped worker ended the call (D46)
			h.Router.Observe(ref, st.Latency, err != nil && s.blocked == "" && !cut)
			if ferr := s.finish(); ferr != nil {
				return nil, st, ferr // fenced while streaming
			}
			if s.blocked != "" {
				st.ErrorCode = "guard_blocked"
				return nil, st, &guardBlock{rule: s.blocked, detail: "the output matched a blocked pattern while streaming"}
			}
			if err == nil {
				resp.Message.ID = msgID
				st.ErrorCode = "" // an earlier attempt or candidate failed (D45); the step did not
				st.PromptTokens, st.CompletionTokens, st.StopReason = resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.StopReason
				st.FirstOutput, st.FirstText, st.AfterFirst = timing.FirstOutput, timing.FirstText, timing.AfterFirst // D54: the successful attempt's
				st.CachedTokens, st.CacheCreationTokens, st.Server = resp.Usage.CachedTokens, resp.Usage.CacheCreationTokens, resp.Usage.Server
				t.corr.Observe(t.viewRaw, resp.Usage.PromptTokens) // D52: PromptTokens is the whole prompt, the cached part included (D54)
				if gerr := t.guardOutput(resp); gerr != nil {
					st.ErrorCode = "guard_blocked"
					if errors.Is(gerr, errInterrupted) {
						st.ErrorCode = t.interruptCode()
						if t.ours() && s.started && resp.Message.Content != "" {
							if aerr := h.Store.AppendEvents(t.ctx, t.claim, []agui.Event{agui.TextMessageEnd(msgID)}); aerr != nil {
								return nil, st, aerr
							}
						}
					}
					return nil, st, gerr
				}
				if resp.Message.Content != "" && !s.started { // non-streaming provider or strict mode: emit the text now
					if err := h.Store.AppendEvents(t.ctx, t.claim, []agui.Event{agui.TextMessageStart(msgID, agui.RoleAssistant), agui.TextMessageContent(msgID, resp.Message.Content)}); err != nil {
						return nil, st, err
					}
				}
				return resp, st, nil
			}
			// the attempt failed: the text it streamed, if any, is closed so that a client following the stream
			// reads a well formed message, whatever happens next
			if s.started {
				if aerr := h.Store.AppendEvents(t.ctx, t.claim, []agui.Event{agui.TextMessageEnd(msgID)}); aerr != nil {
					return nil, st, aerr
				}
			}
			if cut {
				st.ErrorCode = t.interruptCode()
				return nil, st, errInterrupted
			}
			if t.stopping() {
				st.ErrorCode = "stopped" // D47: the attempt failed; no retry and no other candidate is started
				return nil, st, errStopped
			}
			lastErr = err
			var me *model.Error
			if !errors.As(err, &me) {
				st.ErrorCode = "error"
				break
			}
			st.ErrorCode = errorCode(me)
			if me.Code == "canceled" {
				return nil, st, err
			}
			if me.Detail != "" {
				// D50: what the provider said is for the operator and goes to the host log only; the event says that there is
				// something to look for, never what it is (the run's owner reads the event stream)
				h.Log.Warn("model call failed", "run", t.claim.RunID, "step", step, "attempt", attempt, "provider", ref.Provider, "model", ref.Model, "code", st.ErrorCode, "detail", me.Detail)
				if aerr := h.Store.AppendEvents(t.ctx, t.claim, []agui.Event{agui.Custom(agui.CustomModelError, map[string]any{"step": step, "provider": ref.Provider,
					"model": ref.Model, "attempt": attempt, "code": st.ErrorCode})}); aerr != nil {
					return nil, st, aerr
				}
			}
			if !me.Retryable && me.Code != "transport" {
				break candidates
			}
			// D45: the same candidate is asked again, after a wait, when the error is one that passes and nothing of the
			// answer has been streamed: text already written to the event log cannot be taken back, so an attempt that
			// failed after its first text delta is not repeated (the next candidate may still be tried, as before).
			if !me.Retryable || s.streamed() || attempt >= policy.Attempts {
				break
			}
			wait, ok := policy.Wait(attempt, me.RetryAfter)
			if !ok {
				t.askedWait, t.askedCap = me.RetryAfter, policy.WithDefaults().MaxRetryAfter
				h.Log.Warn("model provider asked to wait longer than this host does", "provider", ref.Provider, "retryAfter", me.RetryAfter)
				break
			}
			if d := t.rc.Run.DeadlineAt; !d.IsZero() && i < len(refs)-1 && !h.Now().Add(wait).Before(d) {
				break // the wait would outlive the run: the next candidate is asked now (with none left, the deadline ends the wait)
			}
			if err := h.Store.AppendEvents(t.ctx, t.claim, []agui.Event{agui.Custom(agui.CustomModelRetry, map[string]any{"step": step, "provider": ref.Provider, "model": ref.Model,
				"attempt": attempt, "of": policy.Attempts, "code": st.ErrorCode, "waitMs": wait.Milliseconds(), "retryAfterMs": me.RetryAfter.Milliseconds()})}); err != nil {
				return nil, st, err
			}
			if !t.sleep(wait) {
				if t.callCtx.Err() == nil && t.stopping() {
					st.ErrorCode = "stopped" // D47: the wait ends, the retry is not made
					return nil, st, errStopped
				}
				st.ErrorCode = t.interruptCode()
				return nil, st, errInterrupted
			}
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

// errorCode is the step's error code of a provider error.
// logAttempt records one request of the step: how long it took, when its first output came, how it ended (D54).
func (t *turn) logAttempt(ref agent.ModelRef, latency time.Duration, timing model.Timing, err error, blocked bool) {
	outcome := "ok"
	var me *model.Error
	switch {
	case blocked:
		outcome = "guard_blocked"
	case errors.As(err, &me):
		outcome = errorCode(me)
	case err != nil:
		outcome = "error"
	}
	e := map[string]any{"attempt": t.attempts, "provider": ref.Provider, "model": ref.Model, "outcome": outcome, "latencyMs": latency.Milliseconds()}
	addTiming(e, timing.FirstOutput, timing.FirstText, timing.AfterFirst)
	t.attemptLog = append(t.attemptLog, e)
}

func addTiming(m map[string]any, first, text, after *time.Duration) {
	if first != nil {
		m["firstOutputMs"] = first.Milliseconds()
	}
	if text != nil {
		m["firstTextMs"] = text.Milliseconds()
	}
	if after != nil {
		m["afterFirstMs"] = after.Milliseconds()
	}
}

// usageEvent is the payload of idp.host/model.usage (D54): the step's tokens and latency as before, and, only where the
// provider or the adapter knew them, the time to first output, what the provider said about reuse and processing, and (when
// the step took more than one request) each request's own timing. An absent field is not known; it is never written as 0.
func (t *turn) usageEvent(step int, st run.Step, u model.Usage) map[string]any {
	m := map[string]any{"step": step, "provider": st.Provider, "model": st.Model,
		"promptTokens": u.PromptTokens, "completionTokens": u.CompletionTokens, "latencyMs": st.Latency.Milliseconds(), "attempts": t.attempts}
	addTiming(m, st.FirstOutput, st.FirstText, st.AfterFirst)
	if u.CachedTokens != nil {
		m["cachedTokens"] = *u.CachedTokens
	}
	if u.CacheCreationTokens != nil {
		m["cacheCreationTokens"] = *u.CacheCreationTokens
	}
	if s := u.Server; s.Any() {
		sv := map[string]any{}
		if s.PromptN != nil {
			sv["promptN"] = *s.PromptN
		}
		if s.PromptMs != nil {
			sv["promptMs"] = *s.PromptMs
		}
		if s.CacheN != nil {
			sv["cacheN"] = *s.CacheN
		}
		if s.PredictedN != nil {
			sv["predictedN"] = *s.PredictedN
		}
		if s.PredictedMs != nil {
			sv["predictedMs"] = *s.PredictedMs
		}
		m["server"] = sv
	}
	if len(t.attemptLog) > 1 {
		m["attemptTimings"] = t.attemptLog
	}
	return m
}

func errorCode(me *model.Error) string {
	switch {
	case me.Code == "status":
		return fmt.Sprintf("status_%d", me.Status)
	case me.Idle:
		return "idle_timeout" // D46
	}
	return me.Code
}

// retryPolicy is the retry policy toward the provider (D45).
func (h *Harness) retryPolicy(provider string) model.RetryPolicy {
	return h.Retry[provider].WithDefaults()
}

// sleep waits for d and reports whether it did: false when the deadline, a cancel or the worker's end came first (D46).
func (t *turn) sleep(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-t.callCtx.Done():
		return false
	case <-t.stop:
		return false
	}
}

// classify decides the initial state of each tool call of an assistant message (limits, the caller's own tools, the
// release's allow list, the confirmation policy) without writing anything; the caller stores the records with the message
// (D47). It returns the records, the events of the decisions (denied, confirmation required) and the confirmations the
// owner is asked for.
func (t *turn) classify(step int, msg agui.Message) ([]run.ToolCall, []agui.Event, []map[string]any) {
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
			tc.State, tc.Result = run.CallDenied, "too many tool calls in one step"
			events = append(events, agui.Custom(agui.CustomToolDenied, map[string]any{"toolCallId": tc.ID, "tool": tc.Tool, "reason": tc.Result}))
		case t.wrappingUp() && !t.delivers(tc.Tool):
			tc.State, tc.Result = run.CallDenied, "the run is wrapping up; only file delivery is available"
			events = append(events, agui.Custom(agui.CustomToolDenied, map[string]any{"toolCallId": tc.ID, "tool": tc.Tool, "reason": tc.Result}))
		case t.frontend[tc.Tool]:
			tc.State = run.CallFrontend
		case !t.visible[tc.Tool] && !t.hostTool(tc.Tool):
			tc.State, tc.Result = run.CallDenied, "tool not available to this agent"
			events = append(events, agui.Custom(agui.CustomToolDenied, map[string]any{"toolCallId": tc.ID, "tool": tc.Tool, "reason": tc.Result}))
		case t.marked[tc.Tool] && !t.screened():
			// D51: the gateway lists this tool as external now, but the run was created without screening its context for a
			// tool that sends content out (the catalogue changed since, or was not as the HOST read it): the call is refused,
			// before any confirmation is asked, and the rest of the run goes on
			tc.State, tc.Result = run.CallDenied, "not run: the gateway now marks this tool as one that sends content outside the enterprise, and this run was created without its context being screened for such a tool; start a new run"
			events = append(events, agui.Custom(agui.CustomToolDenied, map[string]any{"toolCallId": tc.ID, "tool": tc.Tool, "reason": tc.Result, "marked": true}))
		case t.def.Confirmation.For(tc.Tool) == agent.ConfirmConfirm || t.hostRequiresConfirmation(tc.Tool):
			tc.State = run.CallPendingConfirmation
			tc.ConfirmationExpiresAt = now.Add(time.Duration(t.def.Limits.ConfirmationTimeoutSeconds) * time.Second)
			if d := t.rc.Run.DeadlineAt; !d.IsZero() && d.Before(tc.ConfirmationExpiresAt) {
				tc.ConfirmationExpiresAt = d // D35: a decision after the run's deadline could not be used
			}
			required := map[string]any{"toolCallId": tc.ID, "tool": tc.Tool, "arguments": json.RawMessage(canon), "expiresAt": tc.ConfirmationExpiresAt.UTC().Format(time.RFC3339)}
			if t.def.Confirmation.For(tc.Tool) != agent.ConfirmConfirm {
				required["requiredBy"] = "host" // D44: the HOST's configuration, not the release
			}
			events = append(events, agui.Custom(agui.CustomConfirmationRequired, required))
			interrupts = append(interrupts, map[string]any{"type": "confirmation", "toolCallId": tc.ID, "tool": tc.Tool, "arguments": json.RawMessage(canon), "expiresAt": tc.ConfirmationExpiresAt.UTC().Format(time.RFC3339)})
		default:
			tc.State = run.CallConfirmed
		}
		records = append(records, tc)
	}
	return records, events, interrupts
}

// dispatch executes the automatic calls of a step whose records are stored and decides how the turn rests. It returns
// "" to continue the loop, or the status the run was left in.
func (t *turn) dispatch(records []run.ToolCall, interrupts []map[string]any) (string, error) {
	h := t.h
	frontend, stopped := false, false
	for i, tc := range records {
		switch tc.State {
		case run.CallDenied:
			// the reason was decided by classify and stored with the record (D47), so a worker that finds the record
			// unanswered answers it the same way
			if err := t.answer(tc, run.CallDenied, "", tc.Result); err != nil {
				return "", err
			}
		case run.CallConfirmed:
			if _, err := t.execute(tc); err != nil {
				if errors.Is(err, errStopped) {
					stopped = true // boundary 4 (D47): this call and the ones after it stay CONFIRMED for the next worker
					continue
				}
				if errors.Is(err, errInterrupted) {
					err = t.abandon(records, i)
				}
				return "", err
			}
		case run.CallFrontend:
			frontend = true
		}
	}
	if len(interrupts) > 0 {
		// calls waiting for the member end the turn as always, also when the worker was told to stop: the stream needs its
		// interrupt, and a CONFIRMED call that was not started runs when the run resumes after the decisions
		if err := h.Store.Finish(t.ctx, t.claim, run.StatusAwaitingInput, "", "", []agui.Event{agui.RunFinished(t.rc.Run.ThreadID, t.rc.Run.ID, agui.OutcomeInterrupt, interrupts)}); err != nil {
			return "", err
		}
		return run.StatusAwaitingInput, nil
	}
	if stopped {
		return "", errStopped
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

// abandon answers the calls of the step that come after the one the deadline or a cancel interrupted (D46): they will not
// run, and the thread must not keep an assistant message whose tool calls nothing answers. A call waiting for the
// member's confirmation is left as it is, as it is when a run is canceled while it waits. It returns errInterrupted.
func (t *turn) abandon(records []run.ToolCall, after int) error {
	if !t.ours() {
		return errInterrupted
	}
	for _, tc := range records[after+1:] {
		var err error
		switch tc.State {
		case run.CallConfirmed:
			err = t.answer(tc, run.CallFailed, "", "not run: "+t.interruptReason()+" before this call started")
		case run.CallDenied:
			err = t.answer(tc, run.CallDenied, "", "not run: the run ended before this call was handled")
		}
		if err != nil {
			return err
		}
	}
	return errInterrupted
}

// externalToolPatterns are the gateways' external-tool patterns by gateway name, for the store to judge a child's release.
// The tools the gateway marks external in this turn's listing count as patterns for the gateway the run uses (D51), so a
// child that may call one is judged as one that may call a tool the operator lists.
func (t *turn) externalToolPatterns() map[string][]string {
	out := map[string][]string{}
	for name, g := range t.h.Gateways {
		if len(g.ExternalTools) > 0 {
			out[name] = append([]string(nil), g.ExternalTools...)
		}
	}
	if gw := t.def.Tools.Gateway; gw != "" {
		for name := range t.marked {
			out[gw] = append(out[gw], escapePattern(name))
		}
	}
	return out
}

// escapePattern makes a tool name a path.Match pattern that matches that name only.
func escapePattern(name string) string {
	return strings.NewReplacer(`\`, `\\`, "*", `\*`, "?", `\?`, "[", `\[`).Replace(name)
}

// screened reports whether what the run was given was screened against the sensitivity ceiling when it was created, for
// a model or a tool outside the enterprise or a sub-agent (D43, D44, D48), or is not the parent's unscreened context (a
// child that has an outward tool is not handed it). Such a run may call a tool that sends content out.
func (t *turn) screened() bool {
	return t.rc.Run.Input.ExternalTools || t.rc.Run.Input.ContextCeiling != ""
}

// callableMarked lists the gateway-marked tools of this turn that the run may call, sorted.
func (t *turn) callableMarked() []string {
	if !t.screened() {
		return nil
	}
	var out []string
	for name := range t.marked {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// hasOutwardTools reports whether the run has a tool that sends content outside the enterprise: its release does by
// what the HOST decided at creation (D44), or the gateway lists one now that the run may call (D51).
func (t *turn) hasOutwardTools() bool {
	if t.rc.Run.Input.ExternalTools {
		return true
	}
	if !t.screened() {
		return false
	}
	for name := range t.external {
		if t.visible[name] {
			return true
		}
	}
	return false
}

// hostRequiresConfirmation reports whether the HOST's configuration makes calls of the tool wait for the member
// whatever the release's confirmation policy says (D44; platform ADR-0003 D8). It applies to gateway tools only: a
// caller's own tool and the HOST's reserved tools are not the gateway's.
func (t *turn) hostRequiresConfirmation(tool string) bool {
	gw := t.h.Gateways[t.def.Tools.Gateway]
	return gw != nil && t.visible[tool] && agent.MatchesTool(gw.ConfirmTools, tool)
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
	var events []agui.Event
	// D44: tool results of a run that may call a tool sending content outside the enterprise are always wrapped as
	// untrusted. A release is immutable while the HOST's configuration is not, so the release's "off" is overridden here
	// and not refused when the release is published.
	if t.hasOutwardTools() && in.ToolResults == guard.ModeOff {
		in.ToolResults = guard.ModeWrap
		if !t.flagged["raised"] {
			t.flagged["raised"] = true
			raised := map[string]any{"configured": guard.ModeOff, "applied": guard.ModeWrap, "reason": "the run has tools that send content outside the enterprise"}
			if marked := t.callableMarked(); len(marked) > 0 { // D51: say when it is the gateway's marker
				raised["marked"] = marked
			}
			events = append(events, agui.Custom(agui.CustomInputGuardRaised, raised))
		}
	}
	if in.ToolResults != guard.ModeOff || in.Context != guard.ModeOff {
		msgs = append(msgs, agui.Message{ID: "guard", Role: agui.RoleDeveloper, Content: guard.Preamble})
	}
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
	t.ensureLimits()
	tools := t.modelTools()
	// D52: what is not the thread, estimated; then what the model reads of the thread is decided against the water mark
	base := requestTokens(t.def.Instructions, msgs, tools)
	wrapTokens := 0.0
	if in.ToolResults != guard.ModeOff {
		wrapTokens = ctxwin.Tokens(guard.Wrap("tool_result", ""))
	}
	view := t.shape(base, wrapTokens)
	for _, m := range t.messages {
		body := m.Content
		if s, ok := view.results[m.ID]; ok {
			body = s
		}
		m = view.withShownArguments(m)
		if m.Role == agui.RoleTool && m.Content != "" && in.ToolResults != guard.ModeOff { // an error-only result is the HOST's own text
			// the controls look at the whole result; what is shortened is what is shown inside the wrapper (D52)
			content, findings := guard.ApplyView(in.ToolResults, "tool_result", m.Content, body)
			report("tool:"+m.ID, "tool_result", m.ToolCallID, findings, in.ToolResults == guard.ModeBlock)
			m.Content = content
		} else {
			m.Content = body
		}
		msgs = append(msgs, m)
	}
	t.hint, t.viewRaw = "", requestTokens(t.def.Instructions, msgs, tools)
	t.viewFixed = t.viewRaw * t.corr.Factor()
	if t.limits.Window > 0 && t.viewFixed >= float64(t.limits.Hint) { // D52: the context is filling; say so, where the budget hint is
		t.hint = contextHint(int(t.viewFixed), t.limits.Window)
	}
	if t.step > 0 { // D41: last, so the thread before it stays a stable prefix; not stored
		note := agui.Message{ID: "budget", Role: agui.RoleDeveloper, Content: t.budgetNote()}
		msgs = append(msgs, note)
		t.viewRaw += msgTokens(note)
		t.viewFixed = t.viewRaw * t.corr.Factor()
	}
	if t.limits.Window > 0 {
		events = append(events, t.viewEvent(t.step, view, t.viewRaw, t.viewFixed, t.hint != ""))
	}
	if len(events) > 0 {
		if err := t.h.Store.AppendEvents(t.ctx, t.claim, events); err != nil {
			return nil, err
		}
	}
	return msgs, nil
}

// ensureLimits resolves the thresholds for a turn that was built without them.
func (t *turn) ensureLimits() {
	if t.limits.ToolResultBytes == 0 {
		t.limits = t.h.limitsFor(t.def)
	}
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
		cctx, cancel := context.WithTimeout(t.callCtx, t.h.ToolTimeout)
		verdict, err = c.Classify(cctx, resp.Message.Content)
		cancel()
	}
	if err != nil && t.callCtx.Err() != nil {
		return errInterrupted
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
	key := t.h.cmiKey(t.callCtx)
	if key == "" {
		return false, nil
	}
	v, err := t.h.CMI.Verify(t.callCtx, key, pkgID)
	if err != nil && t.callCtx.Err() != nil {
		return true, errInterrupted
	}
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
	var refreshEvents []agui.Event
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
			pkg, err := t.h.CMI.Resolve(t.callCtx, key, req)
			if err != nil && t.callCtx.Err() != nil {
				return true, errInterrupted
			}
			if err == nil {
				kept := t.rc.Run.Input.Context[:0:0]
				for _, c := range t.rc.Run.Input.Context {
					if !strings.HasPrefix(c.Description, "CMI ") && c.Description != cmiclient.ContextCMIWithheld {
						kept = append(kept, c)
					}
				}
				entries := cmiclient.ContextOf(pkg)
				// D44: the refresh applies the screen the run was created under, with the ceiling recorded then
				if ceiling := t.rc.Run.Input.ContextCeiling; ceiling != "" {
					s := cmiclient.Screen(pkg.Items, ceiling)
					entries = s.Context()
					if s.Withheld > 0 {
						event["refreshedWithheld"] = s.Withheld
						refreshEvents = append(refreshEvents, s.Event(t.rc.Run.Input.ContextTrigger, t.rc.Run.Input.ContextVia))
					}
				}
				t.rc.Run.Input.Context = append(kept, entries...)
				event["refreshedPackageId"], event["refreshedItems"] = pkg.PackageID, len(pkg.Items)
			} else {
				event["refreshError"] = sanitize(err)
			}
		}
	}
	return false, t.h.Store.AppendEvents(t.ctx, t.claim, append([]agui.Event{agui.Custom(agui.CustomContextStale, event)}, refreshEvents...))
}

// interruptedEvent is the stream's record that a tool call was cut short (D47): resent says whether it was sent to the
// tool again or answered to the model as interrupted, reason why it was cut short: worker_stopped (the worker that
// began it stopped or died; seen by the worker that resumes the run), deadline or canceled (D46).
func (t *turn) interruptedEvent(tc run.ToolCall, resent bool, reason string) agui.Event {
	return agui.Custom(agui.CustomToolInterrupted, map[string]any{"toolCallId": tc.ID, "tool": tc.Tool, "resent": resent, "reason": reason})
}

// mayResend reports whether a call found EXECUTING by the worker that resumes the run may be sent again (D47). The
// HOST cannot tell a call that was never delivered from one that was done and whose result was lost, so it repeats a call
// only where repeating does no harm: a gateway tool that, as the gateway lists it now, is read-only or idempotent and that
// the operator did not name in neverResendTools; the HOST's own resource__read, which only reads the release. A tool the
// gateway no longer lists (t.repeatable is built from the listing of this turn), or lists without either annotation, is
// not repeated; neither is a remote agent (A2A), whose
// work the HOST cannot see or undo. A local sub-agent call is DELEGATED, never EXECUTING; should one be found EXECUTING anyway, it is not sent again
// (a second child run is not harmless).
func (t *turn) mayResend(tc run.ToolCall) bool {
	if _, ok := t.subagents[tc.Tool]; ok {
		return false // a remote agent's work cannot be seen or undone, and a second child run is not harmless either
	}
	if tc.Tool == agent.ResourceReadTool && t.resources != nil {
		return true
	}
	if t.gateway == nil || agent.MatchesTool(t.gateway.NeverResendTools, tc.Tool) {
		return false
	}
	return t.repeatable[tc.Tool]
}

// resumeExecuting handles a call that was EXECUTING when the worker before this one stopped (D47): sent again when
// mayResend says repeating does no harm, otherwise answered to the model as a tool error that says the outcome is
// unknown. Either way the stream gets idp.host/tool.interrupted, so that an operator can see every repetition.
func (t *turn) resumeExecuting(tc run.ToolCall) error {
	resend := t.mayResend(tc)
	ev := t.interruptedEvent(tc, resend, "worker_stopped")
	t.h.Log.Warn("tool call was in progress when the worker stopped", "run", t.claim.RunID, "toolCall", tc.ID, "tool", tc.Tool, "resent", resend)
	if resend {
		if t.stopping() {
			return errStopped
		}
		if m, ok := t.h.Metrics.(interruptionMetrics); ok {
			m.ToolInterrupted(true, "worker_stopped")
		}
		if err := t.h.Store.AppendEvents(t.ctx, t.claim, []agui.Event{ev}); err != nil {
			return err
		}
		_, err := t.execute(tc)
		return err
	}
	if m, ok := t.h.Metrics.(interruptionMetrics); ok {
		m.ToolInterrupted(false, "worker_stopped")
	}
	return t.answerWith(tc, run.CallFailed, "", "interrupted: the turn that was running this call ended while it was running (its worker stopped, died or lost the database); the tool may or may not have completed it, "+
		"so its outcome is unknown. Check whether it was done before doing it again", ev)
}
