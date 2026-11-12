// Package run holds the HOST's run model (ADR-0001 D3): the persisted state machine, its event log,
// its tool calls and the claim a worker holds while it executes a run.
package run

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"idp-host/internal/agent"
	"idp-host/internal/agui"
)

// Status of a run.
const (
	StatusQueued        = "QUEUED"
	StatusRunning       = "RUNNING"
	StatusAwaitingInput = "AWAITING_INPUT"
	// StatusAwaitingSubagent: the run delegated to sub-agents and rests until they end (D22). It is
	// not claimable, counts as active, and its event stream stays open (no RUN_FINISHED).
	StatusAwaitingSubagent = "AWAITING_SUBAGENT"
	StatusCompleted        = "COMPLETED"
	StatusFailed           = "FAILED"
	StatusCanceled         = "CANCELED"
)

// PurposeEvaluation marks a run the HOST created to judge an evaluation case of a release (D26). Such
// runs do not count against the tenant's concurrency.
const PurposeEvaluation = "evaluation"

// Terminal reports whether status is final.
func Terminal(status string) bool {
	return status == StatusCompleted || status == StatusFailed || status == StatusCanceled
}

// Overdue reports whether a run with this deadline is out of time at now (a zero deadline never is).
func Overdue(deadline, now time.Time) bool {
	return !deadline.IsZero() && !now.Before(deadline)
}

// Tool call states.
const (
	CallPendingConfirmation = "PENDING_CONFIRMATION"
	CallConfirmed           = "CONFIRMED"
	CallRejected            = "REJECTED"
	CallExpired             = "EXPIRED"
	CallExecuting           = "EXECUTING"
	CallDone                = "DONE"
	CallFailed              = "FAILED"
	CallFrontend            = "FRONTEND"  // a tool of the caller; the HOST never executes it
	CallDenied              = "DENIED"    // not allowed by the release or not visible at the gateway
	CallDelegated           = "DELEGATED" // a sub-agent run was created for it; answered when that run ends (D22)
)

// Failure codes.
const (
	FailStepLimit   = "STEP_LIMIT"
	FailTimeout     = "RUN_TIMEOUT"
	FailModel       = "MODEL_ERROR"
	FailGateway     = "GATEWAY_ERROR"
	FailCredential  = "CREDENTIAL_EXPIRED"
	FailBudget      = "BUDGET_EXCEEDED"
	FailInternal    = "INTERNAL"
	FailReleaseGone = "RELEASE_UNAVAILABLE"
	FailGuard       = "GUARD_BLOCKED" // the output guard refused the model's answer (D28)
	FailContext     = "CONTEXT_STALE" // the CMI package the run drew on went stale and the release said fail (CMI D24)
)

// Refusal is a store decision against a worker's request (quota, missing release) as opposed to an
// infrastructure failure: the harness reports it to the model as a tool error and continues.
type Refusal struct {
	Code    string
	Message string
}

// Error implements error.
func (r *Refusal) Error() string { return r.Code + ": " + r.Message }

// ErrFenced is returned to a worker whose claim is no longer valid (lease expired and re-claimed,
// run canceled or finished by someone else). The worker stops without writing anything.
var ErrFenced = errors.New("claim is no longer valid")

// ErrStepLimit is returned by StartStep when the release's step limit is reached.
var ErrStepLimit = errors.New("step limit reached")

// Failure is a run-level failure with a stable code (see the Fail* constants).
type Failure struct {
	Code    string
	Message string
}

// Error implements error.
func (f *Failure) Error() string { return f.Code + ": " + f.Message }

// Context is everything a worker needs to continue a run.
type Context struct {
	Run             Run
	Release         *agent.Document
	Messages        []agui.Message
	ToolCalls       []ToolCall // unresolved: pending, confirmed, rejected, expired or delegated but not yet answered
	BlockedPatterns []string   // the tenant policy's blocked output patterns (D28)
}

// ChildRunInput creates a sub-agent run under the parent's claim (D22).
type ChildRunInput struct {
	ToolCallID string                 // the parent's DELEGATED tool call
	Agent      agent.EmbeddedSubagent // which agent and which of its releases
	Input      string                 // the user message the child starts with
	Gateway    string                 // the parent's effective gateway; the credential is inherited when the child uses the same one
}

// ChildOutcome is what a parent learns of a finished (or still running) child run.
type ChildOutcome struct {
	RunID          string
	Status         string
	FailureCode    string
	FailureMessage string
	Output         string // the child's last assistant message
}

// ChildRunID derives the deterministic id of the child run a tool call delegates to, so a retried
// parent turn replays the child instead of creating a second one.
func ChildRunID(parentRunID, toolCallID string) string {
	sum := sha256.Sum256([]byte(parentRunID + "\x00" + toolCallID))
	return "run_" + hex.EncodeToString(sum[:16])
}

// Input is the part of the request that is not the conversation.
type Input struct {
	Tools          []agui.Tool     `json:"tools"`
	Context        []agui.Context  `json:"context"`
	ForwardedProps json.RawMessage `json:"forwardedProps,omitempty"`
	// Caller options (ADR-0001 D19), taken from forwardedProps["idp.host/..."].
	ToolExecution   string `json:"toolExecution,omitempty"`   // "" = server, or ToolExecutionCaller
	MaxOutputTokens int    `json:"maxOutputTokens,omitempty"` // caps the release's model output limit
}

// ToolExecutionCaller: the caller executes every tool. The run offers the model only the caller's
// tools, never contacts the MCP gateway, and ends after one model call with either a final answer
// or the proposed calls (an EER agent turn).
const ToolExecutionCaller = "caller"

// Run is one execution of an agent on a thread.
type Run struct {
	TenantID            string    `json:"-"`
	ID                  string    `json:"runId"`
	ThreadID            string    `json:"threadId"`
	AgentID             string    `json:"agentId"`
	ReleaseID           string    `json:"releaseId"`
	ReleaseDigest       string    `json:"releaseDigest"`
	Owner               string    `json:"owner"`
	Status              string    `json:"status"`
	IdempotencyKey      string    `json:"-"`
	Input               Input     `json:"-"`
	StepCount           int       `json:"stepCount"`
	EventSeq            int       `json:"-"`
	FencingEpoch        int64     `json:"-"`
	LeaseOwner          string    `json:"-"`
	LeaseUntil          time.Time `json:"-"`
	CancelRequested     bool      `json:"cancelRequested"`
	FailureCode         string    `json:"failureCode,omitempty"`
	FailureMessage      string    `json:"failureMessage,omitempty"`
	ExecutionRef        string    `json:"executionRef,omitempty"`
	ParentRunID         string    `json:"parentRunId,omitempty"`      // the run that delegated to this one (D22)
	Depth               int       `json:"depth,omitempty"`            // 0 for a caller's run, parent depth + 1 for a sub-agent run
	Purpose             string    `json:"purpose,omitempty"`          // "" (a caller's run) or PurposeEvaluation (D26)
	ContextPackageID    string    `json:"contextPackageId,omitempty"` // the CMI package the run's context came from (D32)
	EvaluationID        string    `json:"-"`
	CredentialExpiresAt time.Time `json:"-"`
	TraceID             string    `json:"traceId,omitempty"`
	DeadlineAt          time.Time `json:"deadlineAt"`
	CreatedAt           time.Time `json:"createdAt"`
	StartedAt           time.Time `json:"startedAt,omitzero"`
	FinishedAt          time.Time `json:"finishedAt,omitzero"`
}

// Event is one persisted AG-UI event of a run.
type Event struct {
	ID      int64      `json:"id"`
	RunID   string     `json:"runId"`
	Seq     int        `json:"seq"`
	Type    string     `json:"type"`
	Payload agui.Event `json:"event"`
	At      time.Time  `json:"at"`
}

// ToolCall is a tool call the model requested in a run.
type ToolCall struct {
	RunID                 string    `json:"runId"`
	ID                    string    `json:"toolCallId"`
	Step                  int       `json:"step"`
	MessageID             string    `json:"messageId"`
	Tool                  string    `json:"tool"`
	Arguments             string    `json:"arguments"`
	ArgumentsSHA256       string    `json:"argumentsSha256"`
	State                 string    `json:"state"`
	ChildRunID            string    `json:"childRunId,omitempty"` // the sub-agent run of a DELEGATED call
	ConfirmationExpiresAt time.Time `json:"confirmationExpiresAt,omitzero"`
	DecidedBy             string    `json:"decidedBy,omitempty"`
	DecidedAt             time.Time `json:"decidedAt,omitzero"`
	Result                string    `json:"result,omitempty"`
	IsError               bool      `json:"isError"`
	CreatedAt             time.Time `json:"createdAt"`
	FinishedAt            time.Time `json:"finishedAt,omitzero"`
}

// Step is the accounting of one model call.
type Step struct {
	Step             int           `json:"step"`
	Provider         string        `json:"provider"`
	Model            string        `json:"model"`
	PromptTokens     int           `json:"promptTokens"`
	CompletionTokens int           `json:"completionTokens"`
	Latency          time.Duration `json:"-"`
	StopReason       string        `json:"stopReason,omitempty"`
	ErrorCode        string        `json:"errorCode,omitempty"`
}

// StepName is the AG-UI step name of step n.
func StepName(n int) string { return "step-" + itoa(n) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// Claim is a worker's lease on a run. Every write made under it is checked against the run's
// epoch and lease (fencing).
type Claim struct {
	TenantID string
	RunID    string
	WorkerID string
	Epoch    int64
}
