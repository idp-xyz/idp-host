// Package agui implements the AG-UI protocol objects the HOST speaks with its callers (ADR-0001
// D6): the message model (also the HOST's internal thread message form), RunAgentInput and the
// event vocabulary. Events are plain JSON objects with a "type"; the HOST persists them and derives
// its Server-Sent Events stream from the persisted log.
package agui

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Message roles.
const (
	RoleDeveloper = "developer"
	RoleSystem    = "system"
	RoleAssistant = "assistant"
	RoleUser      = "user"
	RoleTool      = "tool"
)

var idRE = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

// ValidID reports whether s may be used as a message, tool call, thread or run id.
func ValidID(s string) bool { return idRE.MatchString(s) }

// Message is an AG-UI message. Content is text; user and tool messages may arrive as content
// parts, of which only text parts are accepted (they are concatenated).
type Message struct {
	ID         string     `json:"id"`
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"toolCalls,omitempty"`
	ToolCallID string     `json:"toolCallId,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// ToolCall is a function call the assistant requested.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall names the tool and carries its arguments as a JSON-encoded string.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool is a tool definition offered to the agent for a run (a frontend tool when it comes from
// RunAgentInput: the HOST never executes those).
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Context is a piece of caller-provided context for the run.
type Context struct {
	Description string `json:"description"`
	Value       string `json:"value"`
}

// RunAgentInput is the AG-UI run request.
type RunAgentInput struct {
	ThreadID       string          `json:"threadId"`
	RunID          string          `json:"runId"`
	ParentRunID    string          `json:"parentRunId,omitempty"`
	State          json.RawMessage `json:"state,omitempty"`
	Messages       []Message       `json:"messages"`
	Tools          []Tool          `json:"tools"`
	Context        []Context       `json:"context"`
	ForwardedProps json.RawMessage `json:"forwardedProps,omitempty"`
}

type rawMessage struct {
	ID         string          `json:"id"`
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name"`
	ToolCalls  []ToolCall      `json:"toolCalls"`
	ToolCallID string          `json:"toolCallId"`
	Error      string          `json:"error"`
}

// UnmarshalJSON accepts string content or an array of text content parts.
func (m *Message) UnmarshalJSON(b []byte) error {
	var r rawMessage
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	content, err := contentText(r.Content)
	if err != nil {
		return err
	}
	*m = Message{ID: r.ID, Role: r.Role, Content: content, Name: r.Name, ToolCalls: r.ToolCalls, ToolCallID: r.ToolCallID, Error: r.Error}
	return nil
}

func contentText(raw json.RawMessage) (string, error) {
	raw = []byte(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	if raw[0] == '"' {
		var s string
		return s, json.Unmarshal(raw, &s)
	}
	if raw[0] != '[' {
		return "", errors.New("message content must be a string or an array of content parts")
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Type != "text" {
			return "", fmt.Errorf("content part type %q is not supported", p.Type)
		}
		sb.WriteString(p.Text)
	}
	return sb.String(), nil
}

// Validate checks a message as received from a caller.
func (m *Message) Validate() error {
	if !ValidID(m.ID) {
		return errors.New("message id is required")
	}
	switch m.Role {
	case RoleDeveloper, RoleSystem, RoleUser:
		if m.ToolCalls != nil || m.ToolCallID != "" {
			return fmt.Errorf("%s message must not carry tool calls", m.Role)
		}
	case RoleAssistant:
		if m.ToolCallID != "" {
			return errors.New("assistant message must not carry toolCallId")
		}
		for _, c := range m.ToolCalls {
			if err := c.Validate(); err != nil {
				return err
			}
		}
	case RoleTool:
		if !ValidID(m.ToolCallID) {
			return errors.New("tool message needs toolCallId")
		}
	default:
		return fmt.Errorf("unknown message role %q", m.Role)
	}
	if len(m.Content) > 1<<20 {
		return errors.New("message content exceeds 1 MiB")
	}
	return nil
}

// Validate checks a tool call.
func (c *ToolCall) Validate() error {
	if !ValidID(c.ID) || c.Type != "function" || c.Function.Name == "" {
		return errors.New("tool call needs id, type function and a name")
	}
	if c.Function.Arguments != "" && !json.Valid([]byte(c.Function.Arguments)) {
		return errors.New("tool call arguments must be a JSON document")
	}
	return nil
}

// Validate checks the run input: ids, messages, tools and context.
func (in *RunAgentInput) Validate() error {
	if in.ThreadID != "" && !ValidID(in.ThreadID) || in.RunID != "" && !ValidID(in.RunID) {
		return errors.New("threadId and runId must be identifiers")
	}
	seen := map[string]bool{}
	for i := range in.Messages {
		if err := in.Messages[i].Validate(); err != nil {
			return fmt.Errorf("messages[%d]: %w", i, err)
		}
		if seen[in.Messages[i].ID] {
			return fmt.Errorf("messages[%d]: duplicate id", i)
		}
		seen[in.Messages[i].ID] = true
	}
	names := map[string]bool{}
	for i, t := range in.Tools {
		if t.Name == "" || len(t.Name) > 128 || names[t.Name] {
			return fmt.Errorf("tools[%d]: name missing or duplicated", i)
		}
		if len(t.Parameters) > 0 && !json.Valid(t.Parameters) {
			return fmt.Errorf("tools[%d]: parameters must be JSON", i)
		}
		names[t.Name] = true
	}
	for i, c := range in.Context {
		if len(c.Value) > 1<<20 {
			return fmt.Errorf("context[%d]: value exceeds 1 MiB", i)
		}
	}
	return nil
}

// Event types (AG-UI 1.x).
const (
	EventRunStarted         = "RUN_STARTED"
	EventRunFinished        = "RUN_FINISHED"
	EventRunError           = "RUN_ERROR"
	EventStepStarted        = "STEP_STARTED"
	EventStepFinished       = "STEP_FINISHED"
	EventTextMessageStart   = "TEXT_MESSAGE_START"
	EventTextMessageContent = "TEXT_MESSAGE_CONTENT"
	EventTextMessageEnd     = "TEXT_MESSAGE_END"
	EventToolCallStart      = "TOOL_CALL_START"
	EventToolCallArgs       = "TOOL_CALL_ARGS"
	EventToolCallEnd        = "TOOL_CALL_END"
	EventToolCallResult     = "TOOL_CALL_RESULT"
	EventMessagesSnapshot   = "MESSAGES_SNAPSHOT"
	EventCustom             = "CUSTOM"
)

// Event is one AG-UI event: a "type" plus its fields.
type Event map[string]any

// Type returns the event type.
func (e Event) Type() string {
	t, _ := e["type"].(string)
	return t
}

func event(t string, fields map[string]any) Event {
	e := Event{"type": t, "timestamp": time.Now().UnixMilli()}
	for k, v := range fields {
		e[k] = v
	}
	return e
}

// RunStarted opens a run.
func RunStarted(threadID, runID string) Event {
	return event(EventRunStarted, map[string]any{"threadId": threadID, "runId": runID})
}

// Outcome of a finished run: success, or an interrupt that the caller must resolve.
const (
	OutcomeSuccess   = "success"
	OutcomeInterrupt = "interrupt"
)

// RunFinished closes a run successfully or with an interrupt (frontend tool calls, confirmation).
func RunFinished(threadID, runID, outcome string, interrupts []map[string]any) Event {
	o := map[string]any{"type": outcome}
	if outcome == OutcomeInterrupt {
		o["interrupts"] = interrupts
	}
	return event(EventRunFinished, map[string]any{"threadId": threadID, "runId": runID, "outcome": o})
}

// RunError closes a run with an error.
func RunError(message, code string) Event {
	return event(EventRunError, map[string]any{"message": message, "code": code})
}

// StepStarted marks a step of the harness loop.
func StepStarted(name string) Event { return event(EventStepStarted, map[string]any{"stepName": name}) }

// StepFinished closes a step.
func StepFinished(name string) Event {
	return event(EventStepFinished, map[string]any{"stepName": name})
}

// TextMessageStart opens an assistant message.
func TextMessageStart(messageID, role string) Event {
	return event(EventTextMessageStart, map[string]any{"messageId": messageID, "role": role})
}

// TextMessageContent appends text.
func TextMessageContent(messageID, delta string) Event {
	return event(EventTextMessageContent, map[string]any{"messageId": messageID, "delta": delta})
}

// TextMessageEnd closes a message.
func TextMessageEnd(messageID string) Event {
	return event(EventTextMessageEnd, map[string]any{"messageId": messageID})
}

// ToolCallStart opens a tool call of a message.
func ToolCallStart(toolCallID, name, parentMessageID string) Event {
	return event(EventToolCallStart, map[string]any{"toolCallId": toolCallID, "toolCallName": name, "parentMessageId": parentMessageID})
}

// ToolCallArgs appends argument text.
func ToolCallArgs(toolCallID, delta string) Event {
	return event(EventToolCallArgs, map[string]any{"toolCallId": toolCallID, "delta": delta})
}

// ToolCallEnd closes a tool call.
func ToolCallEnd(toolCallID string) Event {
	return event(EventToolCallEnd, map[string]any{"toolCallId": toolCallID})
}

// ToolCallResult reports a tool's result as a tool message.
func ToolCallResult(messageID, toolCallID, content string) Event {
	return event(EventToolCallResult, map[string]any{"messageId": messageID, "toolCallId": toolCallID, "content": content, "role": RoleTool})
}

// MessagesSnapshot carries the full thread.
func MessagesSnapshot(messages []Message) Event {
	if messages == nil {
		messages = []Message{}
	}
	return event(EventMessagesSnapshot, map[string]any{"messages": messages})
}

// Custom is an application event (name is namespaced, e.g. idp.host/confirmation.required).
func Custom(name string, value any) Event {
	return event(EventCustom, map[string]any{"name": name, "value": value})
}

// Custom event names the HOST emits.
const (
	CustomConfirmationRequired = "idp.host/confirmation.required"
	CustomConfirmationDecided  = "idp.host/confirmation.decided"
	CustomUsage                = "idp.host/model.usage"
	CustomToolDenied           = "idp.host/tool.denied"
	// sub-agent runs (ADR-0001 D22), emitted on the parent run's stream
	CustomSubagentStarted       = "idp.host/subagent.started"
	CustomSubagentAwaitingInput = "idp.host/subagent.awaiting_input"
	CustomSubagentFinished      = "idp.host/subagent.finished"
	// CMI context (ADR-0001 D32)
	CustomContextResolved = "idp.host/context.resolved"
	CustomContextSkipped  = "idp.host/context.skipped"
	// model routing (ADR-0001 D29)
	CustomModelRouted = "idp.host/model.routed"
	// content guards (ADR-0001 D27, D28)
	CustomGuardFlagged = "idp.host/guard.flagged"
	CustomGuardBlocked = "idp.host/guard.blocked"
)
