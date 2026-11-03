// Package gemini is the model adapter for the Google Gemini API (generateContent): SSE streaming,
// function declarations / calls / responses, usage metadata, thought parts as reasoning.
package gemini

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel/propagation"

	"idp-host/internal/agui"
	"idp-host/internal/model"
	"idp-host/internal/observability"
)

// Config of one Gemini-style provider.
type Config struct {
	Name           string
	BaseURL        string // e.g. https://generativelanguage.googleapis.com (the adapter adds /v1beta)
	APIKey         string
	HTTP           *http.Client
	NewMessageID   func() string
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

type part map[string]any

type content struct {
	Role  string `json:"role"`
	Parts []part `json:"parts"`
}

func (p *Provider) body(req model.Request) (map[string]any, error) {
	var contents []content
	push := func(role string, parts ...part) {
		if n := len(contents); n > 0 && contents[n-1].Role == role {
			contents[n-1].Parts = append(contents[n-1].Parts, parts...)
			return
		}
		contents = append(contents, content{Role: role, Parts: parts})
	}
	// Gemini has no tool call ids: the function name travels with the response, so we remember it
	callNames := map[string]string{}
	system := req.Instructions
	for _, m := range req.Messages {
		switch m.Role {
		case agui.RoleDeveloper, agui.RoleSystem:
			if system != "" {
				system += "\n\n"
			}
			system += m.Content
		case agui.RoleUser:
			push("user", part{"text": m.Content})
		case agui.RoleAssistant:
			var parts []part
			if m.Content != "" {
				parts = append(parts, part{"text": m.Content})
			}
			for _, c := range m.ToolCalls {
				var args any = map[string]any{}
				if c.Function.Arguments != "" {
					if err := json.Unmarshal([]byte(c.Function.Arguments), &args); err != nil {
						return nil, fmt.Errorf("tool call %s: arguments are not JSON", c.ID)
					}
				}
				callNames[c.ID] = c.Function.Name
				parts = append(parts, part{"functionCall": map[string]any{"name": c.Function.Name, "args": args}})
			}
			if len(parts) == 0 {
				parts = append(parts, part{"text": ""})
			}
			push("model", parts...)
		case agui.RoleTool:
			name := callNames[m.ToolCallID]
			if name == "" {
				return nil, fmt.Errorf("tool message %s answers an unknown call", m.ID)
			}
			var response any
			if err := json.Unmarshal([]byte(m.Content), &response); err != nil || m.Content == "" {
				response = map[string]any{"content": m.Content}
			}
			if m.Error != "" {
				response = map[string]any{"error": m.Error, "content": m.Content}
			}
			if _, ok := response.(map[string]any); !ok { // responses must be objects
				response = map[string]any{"result": response}
			}
			push("user", part{"functionResponse": map[string]any{"name": name, "response": response}})
		}
	}
	if len(contents) == 0 {
		return nil, errors.New("the conversation is empty")
	}
	b := map[string]any{"contents": contents}
	if system != "" {
		b["systemInstruction"] = map[string]any{"parts": []part{{"text": system}}}
	}
	if len(req.Tools) > 0 {
		var decls []map[string]any
		for _, t := range req.Tools {
			d := map[string]any{"name": t.Name, "description": t.Description}
			if len(t.Parameters) > 0 && string(t.Parameters) != "null" {
				d["parameters"] = t.Parameters
			}
			decls = append(decls, d)
		}
		b["tools"] = []map[string]any{{"functionDeclarations": decls}}
	}
	gen := map[string]any{}
	if req.Temperature != nil {
		gen["temperature"] = *req.Temperature
	}
	if req.MaxOutputTokens > 0 {
		gen["maxOutputTokens"] = req.MaxOutputTokens
	}
	if len(gen) > 0 {
		b["generationConfig"] = gen
	}
	return b, nil
}

type chunk struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text         string `json:"text"`
				Thought      bool   `json:"thought"`
				FunctionCall *struct {
					Name string          `json:"name"`
					Args json.RawMessage `json:"args"`
				} `json:"functionCall"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		ThoughtsTokenCount   int `json:"thoughtsTokenCount"`
	} `json:"usageMetadata"`
	Error *json.RawMessage `json:"error"`
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
	endpoint := p.cfg.BaseURL + "/v1beta/models/" + url.PathEscape(req.Model) + ":streamGenerateContent?alt=sse"
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "text/event-stream")
	if p.cfg.APIKey != "" {
		hr.Header.Set("x-goog-api-key", p.cfg.APIKey)
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
	msg := agui.Message{ID: p.cfg.NewMessageID(), Role: agui.RoleAssistant}
	var text, reasoning strings.Builder
	var usage model.Usage
	finish := ""
	calls := 0
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
		var c chunk
		if err := json.Unmarshal([]byte(payload), &c); err != nil || c.Error != nil {
			return &model.Error{Code: "decode"}
		}
		if c.UsageMetadata != nil {
			usage = model.Usage{PromptTokens: c.UsageMetadata.PromptTokenCount, CompletionTokens: c.UsageMetadata.CandidatesTokenCount + c.UsageMetadata.ThoughtsTokenCount}
		}
		for _, cand := range c.Candidates {
			if cand.FinishReason != "" {
				finish = cand.FinishReason
			}
			for _, pt := range cand.Content.Parts {
				switch {
				case pt.FunctionCall != nil:
					args := strings.TrimSpace(string(pt.FunctionCall.Args))
					if args == "" || args == "null" {
						args = "{}"
					}
					if pt.FunctionCall.Name == "" || !json.Valid([]byte(args)) {
						return &model.Error{Code: "schema"}
					}
					calls++
					msg.ToolCalls = append(msg.ToolCalls, agui.ToolCall{ID: fmt.Sprintf("%s-call-%d", msg.ID, calls), Type: "function",
						Function: agui.FunctionCall{Name: pt.FunctionCall.Name, Arguments: args}})
				case pt.Thought:
					reasoning.WriteString(pt.Text)
					if onDelta != nil && pt.Text != "" {
						onDelta(model.Delta{Reasoning: pt.Text})
					}
				case pt.Text != "":
					text.WriteString(pt.Text)
					if onDelta != nil {
						onDelta(model.Delta{Text: pt.Text})
					}
				}
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
	msg.Content = text.String()
	stop := model.StopOther
	switch {
	case len(msg.ToolCalls) > 0:
		stop = model.StopToolCalls
	case finish == "STOP":
		stop = model.StopEnd
	case finish == "MAX_TOKENS":
		stop = model.StopLength
	}
	return &model.Response{Message: msg, StopReason: stop, Usage: usage, Reasoning: reasoning.String()}, nil
}
