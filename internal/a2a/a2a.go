// Package a2a is the HOST's Agent2Agent protocol surface (ADR-0001 D30; A2A 0.3, JSON-RPC 2.0 over
// HTTP): the objects an agent card, a task and the streamed task events are made of, the mapping
// from a HOST run to an A2A task, and a client the harness uses to delegate to a remote agent.
package a2a

import (
	"encoding/json"
	"time"

	"idp-host/internal/agui"
	"idp-host/internal/run"
)

// ProtocolVersion is the A2A version the HOST speaks.
const ProtocolVersion = "0.3.0"

// Task states (A2A TaskState).
const (
	StateSubmitted     = "submitted"
	StateWorking       = "working"
	StateInputRequired = "input-required"
	StateCompleted     = "completed"
	StateCanceled      = "canceled"
	StateFailed        = "failed"
	StateRejected      = "rejected"
)

// JSON-RPC error codes (A2A).
const (
	CodeParse          = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
	CodeTaskNotFound   = -32001
	CodeNotCancelable  = -32002
	CodeUnsupportedOp  = -32004
	CodeContentNotSupp = -32005
)

// Part is a message or artifact part; the HOST speaks text.
type Part struct {
	Kind     string         `json:"kind"`
	Text     string         `json:"text,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Message is an A2A message.
type Message struct {
	Kind      string         `json:"kind"`
	MessageID string         `json:"messageId"`
	Role      string         `json:"role"` // user | agent
	Parts     []Part         `json:"parts"`
	ContextID string         `json:"contextId,omitempty"`
	TaskID    string         `json:"taskId,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// Status is a task's state.
type Status struct {
	State     string   `json:"state"`
	Timestamp string   `json:"timestamp,omitempty"`
	Message   *Message `json:"message,omitempty"`
}

// Artifact is a task output.
type Artifact struct {
	ArtifactID string `json:"artifactId"`
	Name       string `json:"name,omitempty"`
	Parts      []Part `json:"parts"`
}

// Task is an A2A task: one HOST run.
type Task struct {
	Kind      string         `json:"kind"`
	ID        string         `json:"id"`
	ContextID string         `json:"contextId"`
	Status    Status         `json:"status"`
	Artifacts []Artifact     `json:"artifacts,omitempty"`
	History   []Message      `json:"history,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// StatusUpdate is a streamed status change.
type StatusUpdate struct {
	Kind      string `json:"kind"`
	TaskID    string `json:"taskId"`
	ContextID string `json:"contextId"`
	Status    Status `json:"status"`
	Final     bool   `json:"final"`
}

// ArtifactUpdate is a streamed artifact chunk.
type ArtifactUpdate struct {
	Kind      string   `json:"kind"`
	TaskID    string   `json:"taskId"`
	ContextID string   `json:"contextId"`
	Artifact  Artifact `json:"artifact"`
	Append    bool     `json:"append"`
	LastChunk bool     `json:"lastChunk"`
}

// Card is an agent card.
type Card struct {
	Name               string                `json:"name"`
	Description        string                `json:"description"`
	URL                string                `json:"url"`
	Version            string                `json:"version"`
	ProtocolVersion    string                `json:"protocolVersion"`
	Capabilities       Capabilities          `json:"capabilities"`
	DefaultInputModes  []string              `json:"defaultInputModes"`
	DefaultOutputModes []string              `json:"defaultOutputModes"`
	Skills             []Skill               `json:"skills"`
	SecuritySchemes    map[string]Scheme     `json:"securitySchemes,omitempty"`
	Security           []map[string][]string `json:"security,omitempty"`
	Provider           *Provider             `json:"provider,omitempty"`
}

// Capabilities of an agent card.
type Capabilities struct {
	Streaming              bool `json:"streaming"`
	PushNotifications      bool `json:"pushNotifications"`
	StateTransitionHistory bool `json:"stateTransitionHistory"`
}

// Skill of an agent card.
type Skill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
}

// Scheme is a security scheme.
type Scheme struct {
	Type   string `json:"type"`
	Scheme string `json:"scheme,omitempty"`
}

// Provider names the organization behind an agent.
type Provider struct {
	Organization string `json:"organization"`
	URL          string `json:"url,omitempty"`
}

// Request is a JSON-RPC request.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// Response is a JSON-RPC response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC error.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// SendParams are the params of message/send and message/stream.
type SendParams struct {
	Message       Message `json:"message"`
	Configuration *struct {
		Blocking      *bool `json:"blocking"`
		HistoryLength *int  `json:"historyLength"`
	} `json:"configuration"`
}

// IDParams are the params of tasks/get and tasks/cancel.
type IDParams struct {
	ID            string `json:"id"`
	HistoryLength *int   `json:"historyLength"`
}

// Text joins the text parts of a message.
func (m Message) Text() string {
	out := ""
	for _, p := range m.Parts {
		if p.Kind == "text" {
			out += p.Text
		}
	}
	return out
}

// State maps a run status to a task state.
func State(status string) string {
	switch status {
	case run.StatusQueued:
		return StateSubmitted
	case run.StatusRunning, run.StatusAwaitingSubagent:
		return StateWorking
	case run.StatusAwaitingInput:
		return StateInputRequired
	case run.StatusCompleted:
		return StateCompleted
	case run.StatusCanceled:
		return StateCanceled
	default:
		return StateFailed
	}
}

// TaskOf builds the task of a run: its status, the final assistant messages as artifacts and the
// thread as history (the last historyLength messages when given).
func TaskOf(r *run.Run, messages []agui.Message, historyLength *int) Task {
	t := Task{Kind: "task", ID: r.ID, ContextID: r.ThreadID, Status: Status{State: State(r.Status), Timestamp: time.Now().UTC().Format(time.RFC3339)},
		Metadata: map[string]any{"idp.host/runId": r.ID, "idp.host/releaseDigest": r.ReleaseDigest, "idp.host/status": r.Status}}
	if r.FailureCode != "" {
		t.Status.Message = &Message{Kind: "message", MessageID: "status_" + r.ID, Role: "agent", Parts: []Part{{Kind: "text", Text: r.FailureCode + ": " + r.FailureMessage}}, TaskID: r.ID, ContextID: r.ThreadID}
	}
	var last *agui.Message
	for i := range messages {
		m := messages[i]
		if m.Role == agui.RoleAssistant && m.Content != "" {
			last = &messages[i]
		}
		t.History = append(t.History, MessageOf(m, r))
	}
	if historyLength != nil && *historyLength >= 0 && len(t.History) > *historyLength {
		t.History = t.History[len(t.History)-*historyLength:]
	}
	if last != nil && run.Terminal(r.Status) && r.Status == run.StatusCompleted {
		t.Artifacts = []Artifact{{ArtifactID: last.ID, Name: "answer", Parts: []Part{{Kind: "text", Text: last.Content}}}}
	}
	return t
}

// MessageOf converts a thread message.
func MessageOf(m agui.Message, r *run.Run) Message {
	role := "user"
	if m.Role == agui.RoleAssistant {
		role = "agent"
	}
	text := m.Content
	if m.Role == agui.RoleTool {
		role = "agent"
		if text == "" && m.Error != "" {
			text = "error: " + m.Error
		}
	}
	out := Message{Kind: "message", MessageID: m.ID, Role: role, Parts: []Part{{Kind: "text", Text: text}}, ContextID: r.ThreadID}
	if m.Role == agui.RoleTool || len(m.ToolCalls) > 0 {
		out.Metadata = map[string]any{"idp.host/role": m.Role}
		if len(m.ToolCalls) > 0 {
			calls := make([]map[string]string, 0, len(m.ToolCalls))
			for _, c := range m.ToolCalls {
				calls = append(calls, map[string]string{"id": c.ID, "name": c.Function.Name, "arguments": c.Function.Arguments})
			}
			out.Metadata["idp.host/toolCalls"] = calls
		}
		if m.ToolCallID != "" {
			out.Metadata["idp.host/toolCallId"] = m.ToolCallID
		}
	}
	return out
}
