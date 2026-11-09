// Package fakemodel is a deterministic model server for tests: it speaks the OpenAI Chat
// Completions wire format (streaming and non-streaming, /v1/chat/completions) and the Anthropic
// Messages wire format (streaming, /v1/messages). What it answers is decided by a Script over the
// normalized conversation, so adapter tests, harness tests and the end-to-end suite share one
// upstream. It is a test double, never a product component.
package fakemodel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

// Turn is what the model answers.
type Turn struct {
	Text      string
	Reasoning string
	ToolCalls []Call
	Status    int // non-zero: answer this HTTP status instead
}

// Call is a requested tool call.
type Call struct {
	Name string
	Args string // JSON
}

// Message is the normalized view of the conversation a Script sees.
type Message struct {
	Role       string // system, user, assistant, tool
	Content    string
	ToolCalls  []Call
	ToolCallID string
}

// Script decides the answer from the conversation and the offered tool names.
type Script func(messages []Message, tools []string) Turn

// the workspace id idp-sandbox's sandbox_create answers with, and the code of a "run python:" request
var workspaceRE = regexp.MustCompile(`workspace ([0-9a-f]{32}) created`)
var sandboxCodeRE = regexp.MustCompile(`(?s)^run python:\s*(.+)$`)

var additionRE = regexp.MustCompile(`(\d+)\s*\+\s*(\d+)`)

// Server is the fake.
type Server struct {
	Script    Script
	calls     atomic.Int64
	mu        sync.Mutex
	last      []Message
	lastTools []string
	lastMax   int
	models    []string
}

// LastTools returns the tool names offered in the last request.
func (s *Server) LastTools() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lastTools...)
}

// LastMaxTokens returns the output limit of the last OpenAI-wire request (0 = none).
func (s *Server) LastMaxTokens() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastMax
}

// New returns a server with the default script.
func New() *Server {
	return &Server{Script: DefaultScript, models: []string{"fake-1"}}
}

// Calls reports how many completion requests were served.
func (s *Server) Calls() int64 { return s.calls.Load() }

// Last returns the conversation of the last request.
func (s *Server) Last() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.last...)
}

// DefaultScript: a user message mentioning "sum" calls sum_weights when offered; after a tool
// result it reports the result; a user message starting with "fail:" makes the server answer 500;
// otherwise it echoes.
func DefaultScript(messages []Message, tools []string) Turn {
	last := messages[len(messages)-1]
	has := func(name string) bool {
		for _, t := range tools {
			if t == name || strings.HasSuffix(t, "__"+name) {
				return true
			}
		}
		return false
	}
	toolName := func(name string) string {
		for _, t := range tools {
			if t == name || strings.HasSuffix(t, "__"+name) {
				return t
			}
		}
		return name
	}
	switch last.Role {
	case "tool":
		// the sandbox flow: after sandbox_create answers with a workspace id, run the user's code in it
		if m := workspaceRE.FindStringSubmatch(last.Content); m != nil && has("sandbox_run") {
			for i := len(messages) - 1; i >= 0; i-- {
				if c := sandboxCodeRE.FindStringSubmatch(messages[i].Content); messages[i].Role == "user" && c != nil {
					args, _ := json.Marshal(map[string]any{"workspaceId": m[1], "language": "python", "code": strings.TrimSpace(c[1])})
					return Turn{Reasoning: "run it in the workspace", ToolCalls: []Call{{Name: toolName("sandbox_run"), Args: string(args)}}}
				}
			}
		}
		return Turn{Text: "Result: " + last.Content}
	case "user":
		if strings.HasPrefix(last.Content, "fail:") {
			return Turn{Status: 500}
		}
		// the HOST's fact extraction (D34): "KNOWN FACTS ...\n\nUSER MESSAGE:\n<text>" -> {"facts":[...]}
		if strings.HasPrefix(last.Content, "KNOWN FACTS (key = value):") {
			return Turn{Text: extractFacts(last.Content)}
		}
		// "run python: <code>" with a sandbox offered: create a workspace first (answered by the case above)
		if sandboxCodeRE.MatchString(last.Content) && has("sandbox_create") && has("sandbox_run") {
			return Turn{Reasoning: "need a workspace", ToolCalls: []Call{{Name: toolName("sandbox_create"), Args: `{}`}}}
		}
		if strings.Contains(strings.ToLower(last.Content), "sum") && has("sum") {
			return Turn{Reasoning: "need the tool", ToolCalls: []Call{{Name: toolName("sum"), Args: `{"a":1,"b":2}`}}}
		}
		if strings.Contains(strings.ToLower(last.Content), "echo") && has("echo") {
			return Turn{ToolCalls: []Call{{Name: toolName("echo"), Args: `{"text":"hi"}`}}}
		}
		// an addition tool of any name (EER offers its capabilities as eer_calculator_add_<digest>):
		// "calculate 19 + 23" -> {"a":19,"b":23}
		if m := additionRE.FindStringSubmatch(last.Content); m != nil {
			for _, t := range tools {
				if strings.Contains(strings.ToLower(t), "add") {
					return Turn{Reasoning: "need the tool", ToolCalls: []Call{{Name: t, Args: `{"a":` + m[1] + `,"b":` + m[2] + `}`}}}
				}
			}
		}
		return Turn{Text: "Echo: " + last.Content}
	}
	return Turn{Text: "ok"}
}

// Handler serves both wire formats.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		var data []map[string]any
		for _, m := range s.models {
			data = append(data, map[string]any{"id": m, "object": "model"})
		}
		writeJSON(w, 200, map[string]any{"object": "list", "data": data})
	})
	mux.HandleFunc("POST /v1/chat/completions", s.openai)
	mux.HandleFunc("POST /v1/messages", s.anthropic)
	mux.HandleFunc("POST /v1beta/models/{model}", s.gemini) // {model} is "<id>:streamGenerateContent"
	return mux
}

// ---- Gemini wire ----

type geminiRequest struct {
	SystemInstruction *struct {
		Parts []map[string]any `json:"parts"`
	} `json:"systemInstruction"`
	Contents []struct {
		Role  string           `json:"role"`
		Parts []map[string]any `json:"parts"`
	} `json:"contents"`
	Tools []struct {
		FunctionDeclarations []struct {
			Name string `json:"name"`
		} `json:"functionDeclarations"`
	} `json:"tools"`
}

func (s *Server) gemini(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.PathValue("model"), ":streamGenerateContent") || r.URL.Query().Get("alt") != "sse" {
		writeJSON(w, 404, map[string]any{"error": map[string]any{"message": "only :streamGenerateContent?alt=sse"}})
		return
	}
	var req geminiRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"error": map[string]any{"message": "bad json"}})
		return
	}
	var msgs []Message
	if req.SystemInstruction != nil {
		var sb strings.Builder
		for _, p := range req.SystemInstruction.Parts {
			t, _ := p["text"].(string)
			sb.WriteString(t)
		}
		msgs = append(msgs, Message{Role: "system", Content: sb.String()})
	}
	for _, c := range req.Contents {
		role := c.Role
		if role == "model" {
			role = "assistant"
		}
		for _, p := range c.Parts {
			switch {
			case p["functionCall"] != nil:
				fc := p["functionCall"].(map[string]any)
				args, _ := json.Marshal(fc["args"])
				name, _ := fc["name"].(string)
				msgs = append(msgs, Message{Role: "assistant", ToolCalls: []Call{{Name: name, Args: string(args)}}})
			case p["functionResponse"] != nil:
				fr := p["functionResponse"].(map[string]any)
				name, _ := fr["name"].(string)
				resp, _ := json.Marshal(fr["response"])
				content := string(resp)
				if m, ok := fr["response"].(map[string]any); ok {
					if c, ok := m["content"].(string); ok {
						content = c
					} else if v, ok := m["result"]; ok && len(m) == 1 { // the adapter wraps non-object results
						b, _ := json.Marshal(v)
						content = string(b)
					}
				}
				msgs = append(msgs, Message{Role: "tool", Content: content, ToolCallID: name})
			default:
				t, _ := p["text"].(string)
				msgs = append(msgs, Message{Role: role, Content: t})
			}
		}
	}
	var tools []string
	for _, t := range req.Tools {
		for _, d := range t.FunctionDeclarations {
			tools = append(tools, d.Name)
		}
	}
	if len(msgs) == 0 {
		writeJSON(w, 400, map[string]any{"error": map[string]any{"message": "no contents"}})
		return
	}
	turn := s.decide(msgs, tools)
	if turn.Status != 0 {
		writeJSON(w, turn.Status, map[string]any{"error": map[string]any{"code": turn.Status, "message": "scripted failure"}})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	f, _ := w.(http.Flusher)
	send := func(parts []map[string]any, finish string, usage bool) {
		cand := map[string]any{"content": map[string]any{"parts": parts, "role": "model"}}
		if finish != "" {
			cand["finishReason"] = finish
		}
		ev := map[string]any{"candidates": []any{cand}}
		if usage {
			ev["usageMetadata"] = map[string]any{"promptTokenCount": 10 * len(msgs), "candidatesTokenCount": 7, "totalTokenCount": 10*len(msgs) + 7}
		}
		b, _ := json.Marshal(ev)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		if f != nil {
			f.Flush()
		}
	}
	if turn.Reasoning != "" {
		send([]map[string]any{{"text": turn.Reasoning, "thought": true}}, "", false)
	}
	pieces := split(turn.Text, 5)
	for i, piece := range pieces {
		finish := ""
		if i == len(pieces)-1 && len(turn.ToolCalls) == 0 {
			finish = "STOP"
		}
		send([]map[string]any{{"text": piece}}, finish, i == len(pieces)-1 && len(turn.ToolCalls) == 0)
	}
	if len(turn.ToolCalls) > 0 {
		var parts []map[string]any
		for _, c := range turn.ToolCalls {
			var args any
			_ = json.Unmarshal([]byte(c.Args), &args)
			parts = append(parts, map[string]any{"functionCall": map[string]any{"name": c.Name, "args": args}})
		}
		send(parts, "STOP", true)
	} else if len(pieces) == 0 {
		send([]map[string]any{{"text": ""}}, "STOP", true)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// unwrapRE matches the HOST's untrusted-content wrapper (ADR-0001 D27) around a tool result.
var unwrapRE = regexp.MustCompile(`(?s)^<untrusted source="[a-z_]+">\n(.*)\n</untrusted>$`)

func (s *Server) decide(msgs []Message, tools []string) Turn {
	s.calls.Add(1)
	s.mu.Lock()
	s.last = msgs
	s.lastTools = tools
	s.mu.Unlock()
	// scripts reason about the tool's own output: strip the wrapper a guarded HOST adds
	plain := make([]Message, len(msgs))
	for i, m := range msgs {
		if m.Role == "tool" {
			if sub := unwrapRE.FindStringSubmatch(m.Content); sub != nil {
				m.Content = sub[1]
			}
		}
		plain[i] = m
	}
	msgs = plain
	return s.Script(msgs, tools)
}

func (s *Server) recordMax(n int) {
	s.mu.Lock()
	s.lastMax = n
	s.mu.Unlock()
}

// ---- OpenAI wire ----

type oaiRequest struct {
	Model     string `json:"model"`
	Stream    bool   `json:"stream"`
	MaxTokens int    `json:"max_tokens"`
	Messages  []struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCallID string          `json:"tool_call_id"`
		ToolCalls  []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

func (s *Server) openai(w http.ResponseWriter, r *http.Request) {
	var req oaiRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"error": map[string]any{"message": "bad json"}})
		return
	}
	s.recordMax(req.MaxTokens)
	var msgs []Message
	for _, m := range req.Messages {
		var content string
		_ = json.Unmarshal(m.Content, &content)
		msg := Message{Role: m.Role, Content: content, ToolCallID: m.ToolCallID}
		for _, c := range m.ToolCalls {
			msg.ToolCalls = append(msg.ToolCalls, Call{Name: c.Function.Name, Args: c.Function.Arguments})
		}
		msgs = append(msgs, msg)
	}
	var tools []string
	for _, t := range req.Tools {
		tools = append(tools, t.Function.Name)
	}
	if len(msgs) == 0 {
		writeJSON(w, 400, map[string]any{"error": map[string]any{"message": "no messages"}})
		return
	}
	turn := s.decide(msgs, tools)
	if turn.Status != 0 {
		writeJSON(w, turn.Status, map[string]any{"error": map[string]any{"message": "scripted failure"}})
		return
	}
	finish := "stop"
	var toolCalls []map[string]any
	for i, c := range turn.ToolCalls {
		toolCalls = append(toolCalls, map[string]any{"index": i, "id": fmt.Sprintf("call_%d_%d", s.calls.Load(), i), "type": "function",
			"function": map[string]any{"name": c.Name, "arguments": c.Args}})
	}
	if len(toolCalls) > 0 {
		finish = "tool_calls"
	}
	usage := map[string]any{"prompt_tokens": 10 * len(msgs), "completion_tokens": 7, "total_tokens": 10*len(msgs) + 7}
	if !req.Stream {
		msg := map[string]any{"role": "assistant", "content": turn.Text}
		if turn.Reasoning != "" {
			msg["reasoning_content"] = turn.Reasoning
		}
		if len(toolCalls) > 0 {
			msg["content"] = nil
			msg["tool_calls"] = toolCalls
		}
		writeJSON(w, 200, map[string]any{"id": "chatcmpl-fake", "object": "chat.completion", "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}}, "usage": usage})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	f, _ := w.(http.Flusher)
	send := func(v any) {
		b, _ := json.Marshal(v)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		if f != nil {
			f.Flush()
		}
	}
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{"id": "chatcmpl-fake", "object": "chat.completion.chunk", "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
	}
	send(chunk(map[string]any{"role": "assistant", "content": ""}, nil))
	if turn.Reasoning != "" {
		send(chunk(map[string]any{"reasoning_content": turn.Reasoning}, nil))
	}
	for _, piece := range split(turn.Text, 5) {
		send(chunk(map[string]any{"content": piece}, nil))
	}
	for i, c := range turn.ToolCalls {
		id := fmt.Sprintf("call_%d_%d", s.calls.Load(), i)
		send(chunk(map[string]any{"tool_calls": []any{map[string]any{"index": i, "id": id, "type": "function", "function": map[string]any{"name": c.Name, "arguments": ""}}}}, nil))
		for _, piece := range split(c.Args, 4) {
			send(chunk(map[string]any{"tool_calls": []any{map[string]any{"index": i, "function": map[string]any{"arguments": piece}}}}, nil))
		}
	}
	send(chunk(map[string]any{}, finish))
	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		send(map[string]any{"id": "chatcmpl-fake", "object": "chat.completion.chunk", "model": req.Model, "choices": []any{}, "usage": usage})
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if f != nil {
		f.Flush()
	}
}

// ---- Anthropic wire ----

type antRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	System   string `json:"system"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Name string `json:"name"`
	} `json:"tools"`
}

func (s *Server) anthropic(w http.ResponseWriter, r *http.Request) {
	var req antRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": "bad json"}})
		return
	}
	var msgs []Message
	if req.System != "" {
		msgs = append(msgs, Message{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		var text string
		if err := json.Unmarshal(m.Content, &text); err == nil {
			msgs = append(msgs, Message{Role: m.Role, Content: text})
			continue
		}
		var blocks []map[string]any
		_ = json.Unmarshal(m.Content, &blocks)
		for _, b := range blocks {
			switch b["type"] {
			case "text":
				t, _ := b["text"].(string)
				msgs = append(msgs, Message{Role: m.Role, Content: t})
			case "tool_use":
				args, _ := json.Marshal(b["input"])
				name, _ := b["name"].(string)
				msgs = append(msgs, Message{Role: "assistant", ToolCalls: []Call{{Name: name, Args: string(args)}}})
			case "tool_result":
				id, _ := b["tool_use_id"].(string)
				c, _ := b["content"].(string)
				msgs = append(msgs, Message{Role: "tool", Content: c, ToolCallID: id})
			}
		}
	}
	var tools []string
	for _, t := range req.Tools {
		tools = append(tools, t.Name)
	}
	if len(msgs) == 0 {
		writeJSON(w, 400, map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": "no messages"}})
		return
	}
	turn := s.decide(msgs, tools)
	if turn.Status != 0 {
		writeJSON(w, turn.Status, map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": "scripted failure"}})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	f, _ := w.(http.Flusher)
	send := func(event string, v any) {
		b, _ := json.Marshal(v)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if f != nil {
			f.Flush()
		}
	}
	send("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_fake", "type": "message", "role": "assistant",
		"model": req.Model, "content": []any{}, "usage": map[string]any{"input_tokens": 10 * len(msgs), "output_tokens": 0}}})
	idx := 0
	if turn.Reasoning != "" {
		send("content_block_start", map[string]any{"type": "content_block_start", "index": idx, "content_block": map[string]any{"type": "thinking", "thinking": ""}})
		send("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "thinking_delta", "thinking": turn.Reasoning}})
		send("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
		idx++
	}
	if turn.Text != "" {
		send("content_block_start", map[string]any{"type": "content_block_start", "index": idx, "content_block": map[string]any{"type": "text", "text": ""}})
		for _, piece := range split(turn.Text, 5) {
			send("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "text_delta", "text": piece}})
		}
		send("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
		idx++
	}
	stop := "end_turn"
	for i, c := range turn.ToolCalls {
		id := fmt.Sprintf("toolu_%d_%d", s.calls.Load(), i)
		send("content_block_start", map[string]any{"type": "content_block_start", "index": idx, "content_block": map[string]any{"type": "tool_use", "id": id, "name": c.Name, "input": map[string]any{}}})
		for _, piece := range split(c.Args, 4) {
			send("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "input_json_delta", "partial_json": piece}})
		}
		send("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
		idx++
		stop = "tool_use"
	}
	send("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 7}})
	send("message_stop", map[string]any{"type": "message_stop"})
}

func split(s string, n int) []string {
	var out []string
	r := []rune(s)
	for i := 0; i < len(r); i += n {
		j := i + n
		if j > len(r) {
			j = len(r)
		}
		out = append(out, string(r[i:j]))
	}
	return out
}

var factPatterns = []struct {
	re         *regexp.Regexp
	key, label string
}{
	{regexp.MustCompile(`(?i)favou?rite colou?r (?:is|to) (?:now )?([A-Za-z]+)`), "favourite_colour", "favourite colour"},
	{regexp.MustCompile(`(?i)favou?rite fruit is (?:now )?([A-Za-z]+)`), "favourite_fruit", "favourite fruit"},
	{regexp.MustCompile(`(?i)\bI live in ([A-Za-z]+)`), "city", "city"},
}

// extractFacts is the double's fact extractor: the same JSON the HOST asks a real model for, read off a few English patterns in
// the user's message (never from the known facts or the rest of the prompt).
func extractFacts(prompt string) string {
	_, msg, _ := strings.Cut(prompt, "USER MESSAGE:\n")
	type fact struct {
		Key       string `json:"key"`
		Value     string `json:"value"`
		Statement string `json:"statement"`
	}
	facts := []fact{}
	for _, p := range factPatterns {
		if m := p.re.FindStringSubmatch(msg); m != nil {
			facts = append(facts, fact{p.key, strings.ToLower(m[1]), "The user's " + p.label + " is " + strings.ToLower(m[1])})
		}
	}
	b, _ := json.Marshal(map[string]any{"facts": facts})
	return string(b)
}
