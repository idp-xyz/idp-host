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
	"time"

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
	// IdleTimeout is how long a response may stay silent between two reads before the request is cancelled and the call
	// fails as a retryable transport error (D46); zero means model.DefaultIdleTimeout.
	IdleTimeout time.Duration
	// MaxStreamBytes bounds the bytes of one streamed answer; zero means model.MaxResponse. Tests inject a small one.
	MaxStreamBytes int
	// ReasoningEffort declares that the models of this provider take the reasoning_effort field (D63-2: OpenAI's reasoning models,
	// DeepSeek's none/low/high/max). Without it the field is never sent: a model that does not know it may refuse the request.
	ReasoningEffort bool
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

// idle is the silence a response may keep (D46).
func (p *Provider) idle() time.Duration {
	if p.cfg.IdleTimeout > 0 {
		return p.cfg.IdleTimeout
	}
	return model.DefaultIdleTimeout
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
	if req.PresencePenalty != nil {
		b["presence_penalty"] = *req.PresencePenalty // D60: only the retry of a runaway answer sets it
	}
	if req.MaxOutputTokens > 0 {
		b[p.cfg.MaxTokensField] = req.MaxOutputTokens
	}
	switch effort := req.Effort; { // D63-2, as far as the question rewrite needs it; only for a provider that declared it takes the field
	case !p.cfg.ReasoningEffort:
	case effort == model.EffortLow || effort == model.EffortMedium || effort == model.EffortHigh:
		b["reasoning_effort"] = effort
	case effort == model.EffortOff:
		b["reasoning_effort"] = "none"
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
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens *int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	// Timings is llama.cpp's own account of the call (D54): sent in the last chunk of a stream (and of a plain answer)
	// without any request option, in every chunk with timings_per_token. Other servers do not send it.
	Timings *struct {
		PromptN     *int     `json:"prompt_n"`
		PromptMs    *float64 `json:"prompt_ms"`
		CacheN      *int     `json:"cache_n"`
		PredictedN  *int     `json:"predicted_n"`
		PredictedMs *float64 `json:"predicted_ms"`
	} `json:"timings"`
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
	watch     *model.Stopwatch // nil for a non-streamed answer: its first output is not known (D54)
}

func (a *accumulator) apply(c *chunk, onDelta func(model.Delta)) {
	if c.Usage != nil {
		// prompt_tokens is the whole prompt, cached part included: cached_tokens is a share of it (OpenAI's documentation;
		// llama.cpp's usage_json_oaicompat), so nothing is added back (D54)
		cached, creation, srv := a.usage.CachedTokens, a.usage.CacheCreationTokens, a.usage.Server
		a.usage = model.Usage{PromptTokens: c.Usage.PromptTokens, CompletionTokens: c.Usage.CompletionTokens, CachedTokens: cached, CacheCreationTokens: creation, Server: srv}
		if d := c.Usage.PromptTokensDetails; d != nil && d.CachedTokens != nil {
			n := *d.CachedTokens
			a.usage.CachedTokens = &n
		}
		a.sawUsage = true
	}
	if t := c.Timings; t != nil {
		st := &model.ServerTimings{PromptN: t.PromptN, PromptMs: t.PromptMs, CacheN: t.CacheN, PredictedN: t.PredictedN, PredictedMs: t.PredictedMs}
		if st.Any() {
			a.usage.Server = st
		}
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
			a.watch.Text()
			if onDelta != nil {
				onDelta(model.Delta{Text: *d.Content})
			}
		}
		for _, r := range []*string{d.ReasoningContent, d.Reasoning} {
			if r != nil && *r != "" {
				a.reasoning.WriteString(*r)
				a.watch.Output()
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
			if tc.ID != "" || tc.Function.Name != "" || tc.Function.Arguments != "" {
				a.watch.Output() // a tool call is output: its name and arguments are written by the model
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
	cut := a.finish == "length"
	for n, idx := range a.order {
		c := *a.calls[idx]
		if c.ID == "" {
			c.ID = fmt.Sprintf("%s-call-%d", msgID, n)
		}
		if strings.TrimSpace(c.Function.Arguments) == "" {
			c.Function.Arguments = "{}"
		}
		// an answer stopped by the output limit may have been writing this call: its arguments are cut, valid JSON or
		// not, and the host drops the call (D55); a finished answer with unusable arguments is a schema error
		if !cut && (c.Function.Name == "" || !json.Valid([]byte(c.Function.Arguments))) {
			return nil, &model.Error{Code: "schema"}
		}
		msg.ToolCalls = append(msg.ToolCalls, c)
	}
	stop := model.StopOther
	switch {
	case cut: // D55: whatever it carries, an answer the limit stopped is not complete
		stop = model.StopLength
	case len(msg.ToolCalls) > 0:
		stop = model.StopToolCalls
	case a.finish == "stop" || a.finish == "end_turn":
		stop = model.StopEnd
	}
	return &model.Response{Message: msg, StopReason: stop, Usage: a.usage, Reasoning: a.reasoning.String()}, nil
}

// Complete implements model.Provider with a streaming request.
func (p *Provider) Complete(ctx context.Context, req model.Request, onDelta func(model.Delta)) (*model.Response, error) {
	raw, err := json.Marshal(p.body(req, true))
	if err != nil {
		return nil, err
	}
	rctx, cancelReq := context.WithCancel(ctx) // cancelled by the idle watch (D46)
	defer cancelReq()
	hr, err := http.NewRequestWithContext(rctx, http.MethodPost, p.cfg.BaseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "text/event-stream, application/json")
	if p.cfg.APIKey != "" {
		hr.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}
	observability.InjectIfAllowed(ctx, hr.Header) // only when the harness says this provider may receive it (D57)
	req.Watch.Sent()
	resp, err := p.cfg.HTTP.Do(hr)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &model.Error{Code: "canceled"}
		}
		return nil, &model.Error{Code: "transport", Retryable: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10)) // read to classify it (D40), never kept
		return nil, model.ClassifyHTTP(resp.StatusCode, resp.Header, body, time.Now())
	}
	watch := model.NewIdleWatch(p.idle(), cancelReq)
	defer watch.Stop()
	stream := watch.Wrap(resp.Body)
	acc := &accumulator{}
	mt := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	if mt == "application/json" { // a server that ignored stream: true
		limit := model.StreamLimit(p.cfg.MaxStreamBytes)
		raw, err := io.ReadAll(io.LimitReader(stream, int64(limit)+1))
		if err != nil {
			return nil, watch.StreamError(ctx)
		}
		if len(raw) > limit {
			return nil, model.RunawayError("the response body is longer than the limit") // D60
		}
		var c chunk
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, model.DecodeError("the response body is not JSON")
		}
		if c.Error != nil {
			return nil, model.ClassifyStreamError(*c.Error)
		}
		acc.apply(&c, onDelta) // acc has no watch here: nothing was streamed, so the first output stays absent (D54)
		return acc.response(p.cfg.NewMessageID())
	}
	acc.watch = req.Watch
	sc := bufio.NewScanner(stream)
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
			return model.DecodeError("a stream chunk is not JSON")
		}
		if c.Error != nil {
			return model.ClassifyStreamError(*c.Error) // D50: the provider's own error, inside a stream that began with 200
		}
		acc.apply(&c, onDelta)
		return nil
	}
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		total += len(line)
		if total > model.StreamLimit(p.cfg.MaxStreamBytes) {
			return nil, model.RunawayError("the stream is longer than the limit") // D60: an answer that does not end, not an unreadable chunk
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
		return nil, watch.StreamError(ctx)
	}
	req.Watch.Done()
	return acc.response(p.cfg.NewMessageID())
}
