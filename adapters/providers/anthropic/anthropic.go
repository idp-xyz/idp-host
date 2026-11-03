// Package anthropic is the model adapter for the Anthropic Messages API (and servers that
// implement it): streaming events, tool_use / tool_result blocks, thinking blocks, usage.
package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/propagation"

	"idp-host/internal/agui"
	"idp-host/internal/model"
	"idp-host/internal/observability"
)

// Version is the anthropic-version header sent.
const Version = "2023-06-01"

// DefaultMaxTokens applies when the request sets no output limit (the API requires one).
const DefaultMaxTokens = 4096

// Config of one Anthropic-style provider.
type Config struct {
	Name         string
	BaseURL      string // without /v1
	APIKey       string
	HTTP         *http.Client
	NewMessageID func() string
	// AllowPlainHTTP permits http:// to non-loopback hosts (development only).
	AllowPlainHTTP bool
}

// Provider implements model.Provider.
type Provider struct{ cfg Config }

// New validates the configuration and returns the provider.
func New(cfg Config) (*Provider, error) {
	base, err := model.CheckEndpoint(cfg.BaseURL, cfg.AllowPlainHTTP)
	if err != nil {
		return nil, fmt.Errorf("provider %s: %w", cfg.Name, err)
	}
	cfg.BaseURL = base
	if cfg.HTTP == nil {
		return nil, errors.New("http client required")
	}
	if cfg.NewMessageID == nil {
		cfg.NewMessageID = model.NewMessageID
	}
	return &Provider{cfg: cfg}, nil
}

// Name implements model.Provider.
func (p *Provider) Name() string { return p.cfg.Name }

type block map[string]any

type wireMessage struct {
	Role    string  `json:"role"`
	Content []block `json:"content"`
}

func (p *Provider) body(req model.Request) (map[string]any, error) {
	system := req.Instructions
	var msgs []wireMessage
	push := func(role string, b block) {
		if n := len(msgs); n > 0 && msgs[n-1].Role == role {
			msgs[n-1].Content = append(msgs[n-1].Content, b)
			return
		}
		msgs = append(msgs, wireMessage{Role: role, Content: []block{b}})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case agui.RoleDeveloper, agui.RoleSystem:
			if system != "" {
				system += "\n\n"
			}
			system += m.Content
		case agui.RoleUser:
			push("user", block{"type": "text", "text": m.Content})
		case agui.RoleAssistant:
			if m.Content != "" {
				push("assistant", block{"type": "text", "text": m.Content})
			}
			for _, c := range m.ToolCalls {
				var input any = map[string]any{}
				if c.Function.Arguments != "" {
					if err := json.Unmarshal([]byte(c.Function.Arguments), &input); err != nil {
						return nil, fmt.Errorf("tool call %s: arguments are not JSON", c.ID)
					}
				}
				push("assistant", block{"type": "tool_use", "id": c.ID, "name": c.Function.Name, "input": input})
			}
			if m.Content == "" && len(m.ToolCalls) == 0 {
				push("assistant", block{"type": "text", "text": ""})
			}
		case agui.RoleTool:
			b := block{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": m.Content}
			if m.Error != "" {
				b["is_error"] = true
				if m.Content == "" {
					b["content"] = m.Error
				}
			}
			push("user", b)
		}
	}
	if len(msgs) == 0 || msgs[0].Role != "user" {
		return nil, errors.New("the conversation must start with a user message")
	}
	maxTokens := req.MaxOutputTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	b := map[string]any{"model": req.Model, "max_tokens": maxTokens, "messages": msgs, "stream": true}
	if system != "" {
		b["system"] = system
	}
	if len(req.Tools) > 0 {
		var tools []any
		for _, t := range req.Tools {
			params := t.Parameters
			if len(params) == 0 {
				params = json.RawMessage(`{"type":"object"}`)
			}
			tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "input_schema": params})
		}
		b["tools"] = tools
	}
	if req.Temperature != nil {
		b["temperature"] = *req.Temperature
	}
	return b, nil
}

type event struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Usage struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type  string          `json:"type"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Text  string          `json:"text"`
		Input json.RawMessage `json:"input"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		Thinking    string `json:"thinking"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *json.RawMessage `json:"error"`
}

type blockState struct {
	kind string
	id   string
	name string
	text strings.Builder
}

// Complete implements model.Provider with a streaming request.
func (p *Provider) Complete(ctx context.Context, req model.Request, onDelta func(model.Delta)) (*model.Response, error) {
	body, err := p.body(req)
	if err != nil {
		return nil, &model.Error{Code: "schema"}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BaseURL+"/v1/messages", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "text/event-stream")
	hr.Header.Set("anthropic-version", Version)
	if p.cfg.APIKey != "" {
		hr.Header.Set("x-api-key", p.cfg.APIKey)
	}
	observability.Propagator.Inject(ctx, propagation.HeaderCarrier(hr.Header))
	resp, err := p.cfg.HTTP.Do(hr)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &model.Error{Code: "canceled"}
		}
		return nil, &model.Error{Code: "transport", Retryable: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, model.Classify(resp.StatusCode)
	}
	blocks := map[int]*blockState{}
	var order []int
	var usage model.Usage
	stop := ""
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var data []string
	total := 0
	flush := func() error {
		if len(data) == 0 {
			return nil
		}
		payload := strings.Join(data, "\n")
		data = nil
		var ev event
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			return &model.Error{Code: "decode"}
		}
		switch ev.Type {
		case "error":
			return &model.Error{Code: "decode"}
		case "message_start":
			if ev.Message != nil {
				usage.PromptTokens = ev.Message.Usage.InputTokens
			}
		case "content_block_start":
			if ev.ContentBlock == nil {
				return &model.Error{Code: "decode"}
			}
			st := &blockState{kind: ev.ContentBlock.Type, id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
			st.text.WriteString(ev.ContentBlock.Text)
			if st.kind == "tool_use" && len(ev.ContentBlock.Input) > 0 && string(ev.ContentBlock.Input) != "{}" {
				st.text.Write(ev.ContentBlock.Input)
			}
			blocks[ev.Index] = st
			order = append(order, ev.Index)
		case "content_block_delta":
			st := blocks[ev.Index]
			if st == nil || ev.Delta == nil {
				return &model.Error{Code: "decode"}
			}
			switch ev.Delta.Type {
			case "text_delta":
				st.text.WriteString(ev.Delta.Text)
				if onDelta != nil && st.kind == "text" {
					onDelta(model.Delta{Text: ev.Delta.Text})
				}
			case "input_json_delta":
				st.text.WriteString(ev.Delta.PartialJSON)
			case "thinking_delta":
				st.text.WriteString(ev.Delta.Thinking)
				if onDelta != nil {
					onDelta(model.Delta{Reasoning: ev.Delta.Thinking})
				}
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				stop = ev.Delta.StopReason
			}
			if ev.Usage != nil {
				usage.CompletionTokens = ev.Usage.OutputTokens
			}
		}
		return nil
	}
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		total += len(line)
		if total > model.MaxResponse {
			return nil, &model.Error{Code: "decode"}
		}
		switch {
		case line == "":
			if err := flush(); err != nil {
				return nil, err
			}
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return nil, &model.Error{Code: "canceled"}
		}
		return nil, &model.Error{Code: "transport", Retryable: true}
	}
	msg := agui.Message{ID: p.cfg.NewMessageID(), Role: agui.RoleAssistant}
	var reasoning strings.Builder
	var content strings.Builder
	for _, idx := range order {
		st := blocks[idx]
		switch st.kind {
		case "text":
			content.WriteString(st.text.String())
		case "thinking":
			reasoning.WriteString(st.text.String())
		case "tool_use":
			args := strings.TrimSpace(st.text.String())
			if args == "" {
				args = "{}"
			}
			if st.id == "" || st.name == "" || !json.Valid([]byte(args)) {
				return nil, &model.Error{Code: "schema"}
			}
			msg.ToolCalls = append(msg.ToolCalls, agui.ToolCall{ID: st.id, Type: "function", Function: agui.FunctionCall{Name: st.name, Arguments: args}})
		}
	}
	msg.Content = content.String()
	reason := model.StopOther
	switch {
	case len(msg.ToolCalls) > 0:
		reason = model.StopToolCalls
	case stop == "end_turn" || stop == "stop_sequence":
		reason = model.StopEnd
	case stop == "max_tokens":
		reason = model.StopLength
	}
	return &model.Response{Message: msg, StopReason: reason, Usage: usage, Reasoning: reasoning.String()}, nil
}
