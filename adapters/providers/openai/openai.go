// Package openai is the model adapter for OpenAI-compatible Chat Completions endpoints (OpenAI,
// vLLM, sglang, llama.cpp, Ollama, Qwen/DashScope, DeepSeek, ...): streaming SSE, tool calls,
// usage, and the optional reasoning_content some servers emit.
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"go.opentelemetry.io/otel/propagation"

	"idp-host/internal/agui"
	"idp-host/internal/model"
	"idp-host/internal/observability"
)

// Config of one OpenAI-compatible provider.
type Config struct {
	Name    string
	BaseURL string // .../v1
	APIKey  string
	HTTP    *http.Client
	// UsageInStream asks for usage in the last streamed chunk (stream_options.include_usage).
	// Servers that reject stream_options need it off.
	UsageInStream bool
	// MaxTokensField is "max_tokens" (default; compatible servers) or "max_completion_tokens".
	MaxTokensField string
	// NewMessageID mints assistant message ids (tests inject a deterministic one).
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
	if cfg.MaxTokensField == "" {
		cfg.MaxTokensField = "max_tokens"
	}
	if cfg.MaxTokensField != "max_tokens" && cfg.MaxTokensField != "max_completion_tokens" {
		return nil, errors.New("maxTokensField must be max_tokens or max_completion_tokens")
	}
	if cfg.NewMessageID == nil {
		cfg.NewMessageID = model.NewMessageID
	}
	return &Provider{cfg: cfg}, nil
}

// Name implements model.Provider.
func (p *Provider) Name() string { return p.cfg.Name }

type wireMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

func str(s string) *string { return &s }

func (p *Provider) body(req model.Request, stream bool) map[string]any {
	msgs := make([]any, 0, len(req.Messages)+1)
	if req.Instructions != "" {
		msgs = append(msgs, wireMessage{Role: "system", Content: str(req.Instructions)})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case agui.RoleDeveloper, agui.RoleSystem:
			msgs = append(msgs, wireMessage{Role: "system", Content: str(m.Content)})
		case agui.RoleUser:
			msgs = append(msgs, wireMessage{Role: "user", Content: str(m.Content)})
		case agui.RoleAssistant:
			w := wireMessage{Role: "assistant"}
			if m.Content != "" || len(m.ToolCalls) == 0 {
				w.Content = str(m.Content)
			}
			for _, c := range m.ToolCalls {
				var tc wireToolCall
				tc.ID, tc.Type = c.ID, "function"
				tc.Function.Name, tc.Function.Arguments = c.Function.Name, c.Function.Arguments
				if tc.Function.Arguments == "" {
					tc.Function.Arguments = "{}"
				}
				w.ToolCalls = append(w.ToolCalls, tc)
			}
			msgs = append(msgs, w)
		case agui.RoleTool:
			content := m.Content
			if m.Error != "" && content == "" {
				content = "error: " + m.Error
			}
			msgs = append(msgs, wireMessage{Role: "tool", Content: str(content), ToolCallID: m.ToolCallID})
		}
	}
	b := map[string]any{"model": req.Model, "messages": msgs, "stream": stream}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			params := t.Parameters
			if len(params) == 0 {
				params = json.RawMessage(`{"type":"object"}`)
			}
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": params}})
		}
		b["tools"] = tools
		b["tool_choice"] = "auto"
	}
	if req.Temperature != nil {
		b["temperature"] = *req.Temperature
	}
	if req.MaxOutputTokens > 0 {
		b[p.cfg.MaxTokensField] = req.MaxOutputTokens
	}
	if stream && p.cfg.UsageInStream {
		b["stream_options"] = map[string]any{"include_usage": true}
	}
	return b
}

type chunk struct {
	Choices []struct {
		Index        int     `json:"index"`
		FinishReason *string `json:"finish_reason"`
		Delta        struct {
			Role             string         `json:"role"`
			Content          *string        `json:"content"`
			ReasoningContent *string        `json:"reasoning_content"`
			Reasoning        *string        `json:"reasoning"`
			ToolCalls        []wireToolCall `json:"tool_calls"`
		} `json:"delta"`
		Message *struct {
			Role             string         `json:"role"`
			Content          *string        `json:"content"`
			ReasoningContent *string        `json:"reasoning_content"`
			ToolCalls        []wireToolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *json.RawMessage `json:"error"`
}

type accumulator struct {
	content   strings.Builder
	reasoning strings.Builder
	calls     map[int]*agui.ToolCall
	order     []int
	finish    string
	usage     model.Usage
	sawUsage  bool
}

func (a *accumulator) apply(c *chunk, onDelta func(model.Delta)) {
	if c.Usage != nil {
		a.usage = model.Usage{PromptTokens: c.Usage.PromptTokens, CompletionTokens: c.Usage.CompletionTokens}
		a.sawUsage = true
	}
	for _, ch := range c.Choices {
		if ch.Index != 0 {
			continue
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			a.finish = *ch.FinishReason
		}
		if ch.Message != nil { // non-streaming shape
			if ch.Message.Content != nil {
				a.content.WriteString(*ch.Message.Content)
			}
			if ch.Message.ReasoningContent != nil {
				a.reasoning.WriteString(*ch.Message.ReasoningContent)
			}
			for i, tc := range ch.Message.ToolCalls {
				idx := i
				a.call(idx, tc)
			}
			continue
		}
		d := ch.Delta
		if d.Content != nil && *d.Content != "" {
			a.content.WriteString(*d.Content)
			if onDelta != nil {
				onDelta(model.Delta{Text: *d.Content})
			}
		}
		for _, r := range []*string{d.ReasoningContent, d.Reasoning} {
			if r != nil && *r != "" {
				a.reasoning.WriteString(*r)
				if onDelta != nil {
					onDelta(model.Delta{Reasoning: *r})
				}
			}
		}
		for i, tc := range d.ToolCalls {
			idx := i
			if tc.Index != nil {
				idx = *tc.Index
			}
			a.call(idx, tc)
		}
	}
}

func (a *accumulator) call(idx int, tc wireToolCall) {
	if a.calls == nil {
		a.calls = map[int]*agui.ToolCall{}
	}
	cur := a.calls[idx]
	if cur == nil {
		cur = &agui.ToolCall{Type: "function"}
		a.calls[idx] = cur
		a.order = append(a.order, idx)
	}
	if tc.ID != "" {
		cur.ID = tc.ID
	}
	if tc.Function.Name != "" {
		cur.Function.Name += tc.Function.Name
	}
	cur.Function.Arguments += tc.Function.Arguments
}

func (a *accumulator) response(msgID string) (*model.Response, error) {
	msg := agui.Message{ID: msgID, Role: agui.RoleAssistant, Content: a.content.String()}
	sort.Ints(a.order)
	for n, idx := range a.order {
		c := *a.calls[idx]
		if c.ID == "" {
			c.ID = fmt.Sprintf("%s-call-%d", msgID, n)
		}
		if strings.TrimSpace(c.Function.Arguments) == "" {
			c.Function.Arguments = "{}"
		}
		if c.Function.Name == "" || !json.Valid([]byte(c.Function.Arguments)) {
			return nil, &model.Error{Code: "schema"}
		}
		msg.ToolCalls = append(msg.ToolCalls, c)
	}
	stop := model.StopOther
	switch {
	case len(msg.ToolCalls) > 0:
		stop = model.StopToolCalls
	case a.finish == "stop" || a.finish == "end_turn":
		stop = model.StopEnd
	case a.finish == "length":
		stop = model.StopLength
	}
	return &model.Response{Message: msg, StopReason: stop, Usage: a.usage, Reasoning: a.reasoning.String()}, nil
}

// Complete implements model.Provider with a streaming request.
func (p *Provider) Complete(ctx context.Context, req model.Request, onDelta func(model.Delta)) (*model.Response, error) {
	raw, err := json.Marshal(p.body(req, true))
	if err != nil {
		return nil, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BaseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "text/event-stream, application/json")
	if p.cfg.APIKey != "" {
		hr.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
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
	acc := &accumulator{}
	mt := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	if mt == "application/json" { // a server that ignored stream: true
		body, err := io.ReadAll(io.LimitReader(resp.Body, model.MaxResponse))
		if err != nil {
			return nil, &model.Error{Code: "transport", Retryable: true}
		}
		var c chunk
		if err := json.Unmarshal(body, &c); err != nil || c.Error != nil {
			return nil, &model.Error{Code: "decode"}
		}
		acc.apply(&c, onDelta)
		return acc.response(p.cfg.NewMessageID())
	}
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
		if strings.TrimSpace(payload) == "[DONE]" {
			return nil
		}
		var c chunk
		if err := json.Unmarshal([]byte(payload), &c); err != nil {
			return &model.Error{Code: "decode"}
		}
		if c.Error != nil {
			return &model.Error{Code: "decode"}
		}
		acc.apply(&c, onDelta)
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
	return acc.response(p.cfg.NewMessageID())
}
