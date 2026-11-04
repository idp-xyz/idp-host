// Package harnessworker is the model adapter for an external harness worker (ADR-0001 D31): a
// process that performs the reasoning of one step with whatever SDK it likes, behind the HOST's
// own protocol. The HOST keeps persistence, gating, confirmation, tool execution and quotas; the
// worker only turns one request (instructions, messages, tools, limits) into text deltas, tool
// calls and a stop reason. It is registered as a provider of kind harness-worker, so a release
// selects it through model.provider and everything else in the loop stays the same.
//
// Protocol: POST {endpoint}/v1/turn with a JSON body
//
//	{"model": "...", "instructions": "...", "messages": [AG-UI messages], "tools": [{name, description, parameters}],
//	 "temperature": 0.2, "maxOutputTokens": 1024}
//
// answered as text/event-stream whose data lines are JSON events:
//
//	{"type":"text","delta":"..."}            assistant text
//	{"type":"reasoning","delta":"..."}       provider reasoning (never shown to tools)
//	{"type":"tool_call","id":"...","name":"...","arguments":"{...}"}
//	{"type":"done","stopReason":"end|tool_calls|length|other","usage":{"promptTokens":n,"completionTokens":n}}
//	{"type":"error","code":"...","message":"...","retryable":false}
package harnessworker

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

	"idp-host/internal/agui"
	"idp-host/internal/model"
)

// Config of one harness worker.
type Config struct {
	Name           string
	Endpoint       string // http(s)://host:port, without /v1/turn
	APIKey         string
	HTTP           *http.Client
	AllowPlainHTTP bool
	NewMessageID   func() string
}

// Provider implements model.Provider.
type Provider struct{ cfg Config }

// New validates the configuration.
func New(cfg Config) (*Provider, error) {
	base, err := model.CheckEndpoint(cfg.Endpoint, cfg.AllowPlainHTTP)
	if err != nil {
		return nil, fmt.Errorf("harness worker %s: %w", cfg.Name, err)
	}
	cfg.Endpoint = base
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

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type wireRequest struct {
	Model           string         `json:"model"`
	Instructions    string         `json:"instructions"`
	Messages        []agui.Message `json:"messages"`
	Tools           []wireTool     `json:"tools"`
	Temperature     *float64       `json:"temperature,omitempty"`
	MaxOutputTokens int            `json:"maxOutputTokens,omitempty"`
}

type wireEvent struct {
	Type       string `json:"type"`
	Delta      string `json:"delta"`
	ID         string `json:"id"`
	Name       string `json:"name"`
	Arguments  string `json:"arguments"`
	StopReason string `json:"stopReason"`
	Usage      *struct {
		PromptTokens     int `json:"promptTokens"`
		CompletionTokens int `json:"completionTokens"`
	} `json:"usage"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// Complete implements model.Provider.
func (p *Provider) Complete(ctx context.Context, req model.Request, onDelta func(model.Delta)) (*model.Response, error) {
	tools := make([]wireTool, 0, len(req.Tools))
	for _, t := range req.Tools {
		tools = append(tools, wireTool{Name: t.Name, Description: t.Description, Parameters: t.Parameters})
	}
	msgs := req.Messages
	if msgs == nil {
		msgs = []agui.Message{}
	}
	body, err := json.Marshal(wireRequest{Model: req.Model, Instructions: req.Instructions, Messages: msgs, Tools: tools, Temperature: req.Temperature, MaxOutputTokens: req.MaxOutputTokens})
	if err != nil {
		return nil, &model.Error{Code: "schema"}
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.Endpoint+"/v1/turn", bytes.NewReader(body))
	if err != nil {
		return nil, &model.Error{Code: "transport"}
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "text/event-stream")
	if p.cfg.APIKey != "" {
		hr.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}
	resp, err := p.cfg.HTTP.Do(hr)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &model.Error{Code: "canceled"}
		}
		return nil, &model.Error{Code: "transport", Retryable: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, model.Classify(resp.StatusCode)
	}
	out := &model.Response{Message: agui.Message{ID: p.cfg.NewMessageID(), Role: agui.RoleAssistant}}
	var content, reasoning strings.Builder
	sc := bufio.NewScanner(io.LimitReader(resp.Body, model.MaxResponse))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	done := false
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev wireEvent
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev); err != nil {
			return nil, &model.Error{Code: "decode"}
		}
		switch ev.Type {
		case "text":
			content.WriteString(ev.Delta)
			if onDelta != nil && ev.Delta != "" {
				onDelta(model.Delta{Text: ev.Delta})
			}
		case "reasoning":
			reasoning.WriteString(ev.Delta)
			if onDelta != nil && ev.Delta != "" {
				onDelta(model.Delta{Reasoning: ev.Delta})
			}
		case "tool_call":
			if ev.ID == "" || ev.Name == "" {
				return nil, &model.Error{Code: "decode"}
			}
			args := ev.Arguments
			if args == "" {
				args = "{}"
			}
			if !json.Valid([]byte(args)) {
				return nil, &model.Error{Code: "decode"}
			}
			out.Message.ToolCalls = append(out.Message.ToolCalls, agui.ToolCall{ID: ev.ID, Type: "function", Function: agui.FunctionCall{Name: ev.Name, Arguments: args}})
		case "done":
			done = true
			out.StopReason = ev.StopReason
			if ev.Usage != nil {
				out.Usage = model.Usage{PromptTokens: ev.Usage.PromptTokens, CompletionTokens: ev.Usage.CompletionTokens}
			}
		case "error":
			code := "worker"
			if ev.Code != "" {
				code = "worker_" + ev.Code
			}
			return nil, &model.Error{Code: code, Retryable: ev.Retryable}
		default:
			return nil, &model.Error{Code: "decode"}
		}
		if done {
			break
		}
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return nil, &model.Error{Code: "canceled"}
		}
		return nil, &model.Error{Code: "transport", Retryable: true}
	}
	if !done {
		if ctx.Err() != nil {
			return nil, &model.Error{Code: "canceled"}
		}
		return nil, &model.Error{Code: "decode"} // the stream ended without done
	}
	out.Message.Content = content.String()
	out.Reasoning = reasoning.String()
	switch out.StopReason {
	case model.StopEnd, model.StopToolCalls, model.StopLength, model.StopOther:
	case "":
		out.StopReason = model.StopEnd
		if len(out.Message.ToolCalls) > 0 {
			out.StopReason = model.StopToolCalls
		}
	default:
		out.StopReason = model.StopOther
	}
	return out, nil
}
