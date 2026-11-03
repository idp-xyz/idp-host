// Package run holds the HOST's run model (ADR-0001 D3): the persisted state machine, its event log,
// its tool calls and the claim a worker holds while it executes a run.
package run

import (
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
	StatusCompleted     = "COMPLETED"
	StatusFailed        = "FAILED"
	StatusCanceled      = "CANCELED"
)

// Terminal reports whether status is final.
func Terminal(status string) bool {
	return status == StatusCompleted || status == StatusFailed || status == StatusCanceled
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
	CallFrontend            = "FRONTEND" // a tool of the caller; the HOST never executes it
	CallDenied              = "DENIED"   // not allowed by the release or not visible at the gateway
)

// Failure codes.
const (
	FailStepLimit   = "STEP_LIMIT"
	FailTimeout     = "RUN_TIMEOUT"
	FailModel       = "MODEL_ERROR"
	FailGateway     = "GATEWAY_ERROR"
	FailCredential  = "CREDENTIAL_EXPIRED"
	FailInternal    = "INTERNAL"
	FailReleaseGone = "RELEASE_UNAVAILABLE"
)

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
	Run       Run
	Release   *agent.Document
	Messages  []agui.Message
	ToolCalls []ToolCall // unresolved: pending, confirmed, rejected or expired but not yet answered
}

// Input is the part of the request that is not the conversation.
type Input struct {
	Tools          []agui.Tool     `json:"tools"`
	Context        []agui.Context  `json:"context"`
	ForwardedProps json.RawMessage `json:"forwardedProps,omitempty"`
}

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
