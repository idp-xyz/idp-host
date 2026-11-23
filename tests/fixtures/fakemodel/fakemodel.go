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
	"path"
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
	// ErrorCode is the code of the error answered with Status on the OpenAI wire (e.g. context_length_exceeded).
	ErrorCode string
	// CutOff answers on the OpenAI wire with finish_reason "length": the model ran out of context or output. With ToolCalls
	// it is a cut-off answer whose calls (their Args may be cut JSON) were being written.
	CutOff bool
	// Completion is the completion_tokens the server says it generated; zero is the double's usual 7. A runaway answer (D60)
	// says as many as the output limit it was given.
	Completion int
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

// the workspace id idp-sandbox's sandbox_create answers with, and the language and code of a "run python:" request (also
// "run shell:" and "run office:", the sandbox's office profile)
var workspaceRE = regexp.MustCompile(`workspace ([0-9a-f]{32}) created`)
var sandboxCodeRE = regexp.MustCompile(`(?s)^run (python|shell|office):\s*(.+)$`)

// the file flow (idp-host D37): "edit office: <code>" with a file attached creates a workspace, imports attachment:1 as
// in<ext>, runs the code (which writes out<ext>), delivers out<ext> as "<name>-edited<ext>" and reports what was delivered.
// The attachment's name comes from the HOST's note about the files ("attachment:1  \"<name>\" ...").
var editRE = regexp.MustCompile(`(?s)^edit (python|shell|office):\s*(.+)$`)
var attachmentNoteRE = regexp.MustCompile(`attachment:1  "([^"]+)"`)

// editFlow answers the next step of the file flow, or false when the conversation is not one.
func editFlow(messages []Message, has func(string) bool, toolName func(string) string) (Turn, bool) {
	var code []string
	name := ""
	for _, m := range messages {
		if c := editRE.FindStringSubmatch(m.Content); m.Role == "user" && c != nil {
			code = c
		}
		if n := attachmentNoteRE.FindStringSubmatch(m.Content); n != nil && m.Role != "user" && m.Role != "tool" {
			_ = json.Unmarshal([]byte(`"`+n[1]+`"`), &name)
		}
	}
	if code == nil || name == "" || !has("sandbox_create") || !has("sandbox_import_file") || !has("sandbox_run") || !has("sandbox_export_file") {
		return Turn{}, false
	}
	ext := path.Ext(name)
	last := messages[len(messages)-1]
	if last.Role == "user" {
		return Turn{Reasoning: "need a workspace for the file", ToolCalls: []Call{{Name: toolName("sandbox_create"), Args: `{}`}}}, true
	}
	ws := ""
	for _, m := range messages {
		if w := workspaceRE.FindStringSubmatch(m.Content); m.Role == "tool" && w != nil {
			ws = w[1]
		}
	}
	prev := ""
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" && len(messages[i].ToolCalls) > 0 {
			prev = messages[i].ToolCalls[0].Name
			break
		}
	}
	call := func(tool string, args map[string]any) (Turn, bool) {
		b, _ := json.Marshal(args)
		return Turn{Reasoning: "next step of the file", ToolCalls: []Call{{Name: toolName(tool), Args: string(b)}}}, true
	}
	switch {
	case ws == "":
		return Turn{Text: "Result: " + last.Content}, true
	case strings.HasSuffix(prev, "sandbox_create"):
		return call("sandbox_import_file", map[string]any{"workspaceId": ws, "source": "attachment:1", "path": "in" + ext})
	case strings.HasSuffix(prev, "sandbox_import_file") && !strings.HasPrefix(last.Content, "error"):
		return call("sandbox_run", map[string]any{"workspaceId": ws, "language": code[1], "code": strings.TrimSpace(code[2])})
	case strings.HasSuffix(prev, "sandbox_run"):
		return call("sandbox_export_file", map[string]any{"workspaceId": ws, "path": "out" + ext, "target": "output", "name": strings.TrimSuffix(name, ext) + "-edited" + ext})
	}
	return Turn{Text: "Result: " + last.Content}, true
}

// the web flows (platform ADR-0003 / idp-host D44): "web: <query>" searches, fetches the first result and answers with "[1]"
// and the start of what was read; "web-obey: <query>" then "obeys" the page it read (PATH N): it fetches the first URL found in
// the page with the search result's ref (the service must refuse that) and asks to open it (the HOST asks the member).
//
// "web-cite: <query>" (D49) is "web:" whose answer also cites InventedCitation, a number no source of a run can have
// (idp-web numbers per caller and does not come near it), so that the check of the answer's citations has something to find.
var webRE = regexp.MustCompile(`(?s)^(web|web-obey|web-cite):\s*(.+)$`)

// InventedCitation is the number "web-cite:" adds to its answer: three digits, so that the check reports it, and far above
// any number a caller's sources reach in an acceptance run.
const InventedCitation = 987

// the first entry of a search result: "[n] title\naddress: <url>\nref: <ref>"
var searchEntryRE = regexp.MustCompile(`(?m)^\[\d+\] [^\n]*\naddress: (\S+)\nref: (\S+)`)
var pageURLRE = regexp.MustCompile(`https?://[^\s<>"'\x60)\]]+`)

// webFlow answers the next step of a web flow, or false when the conversation is not one or the tools it needs are not offered.
func webFlow(messages []Message, has func(string) bool, toolName func(string) string) (Turn, bool) {
	var m []string
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			m = webRE.FindStringSubmatch(messages[i].Content)
			break
		}
	}
	if m == nil || !has("web_search") || !has("web_fetch") || (m[1] == "web-obey" && !has("web_open_url")) {
		return Turn{}, false
	}
	obey := m[1] == "web-obey"
	call := func(tool string, args map[string]any) (Turn, bool) {
		b, _ := json.Marshal(args)
		return Turn{Reasoning: "next step of the web request", ToolCalls: []Call{{Name: toolName(tool), Args: string(b)}}}, true
	}
	last := messages[len(messages)-1]
	if last.Role == "user" {
		return call("web_search", map[string]any{"query": strings.TrimSpace(m[2])})
	}
	// the calls made so far, in order
	var made []string
	fetches := 0
	for _, msg := range messages {
		if msg.Role == "assistant" {
			for _, c := range msg.ToolCalls {
				made = append(made, c.Name)
				if strings.HasSuffix(c.Name, "web_fetch") {
					fetches++
				}
			}
		}
	}
	if len(made) == 0 {
		return Turn{}, false
	}
	prev := made[len(made)-1]
	// an error ends the flow with its text, except the refusal the obeying script is waiting for: it is the point of PATH N
	refusalExpected := obey && strings.HasSuffix(prev, "web_fetch") && fetches == 2
	if strings.HasPrefix(last.Content, "error") && !refusalExpected {
		return Turn{Text: last.Content}, true
	}
	// the first search result, from the tool result that answered web_search
	first := func() []string {
		for _, msg := range messages {
			if msg.Role == "tool" {
				if e := searchEntryRE.FindStringSubmatch(msg.Content); e != nil {
					return e
				}
			}
		}
		return nil
	}
	switch {
	case strings.HasSuffix(prev, "web_search"):
		e := searchEntryRE.FindStringSubmatch(last.Content)
		if e == nil {
			return Turn{Text: "No results: " + last.Content}, true
		}
		return call("web_fetch", map[string]any{"url": e[1], "ref": e[2]})
	case strings.HasSuffix(prev, "web_fetch") && !obey:
		text := "Result [1]: " + firstRunes(last.Content, 300)
		if m[1] == "web-cite" {
			text += fmt.Sprintf(" Also [%d].", InventedCitation)
		}
		return Turn{Text: text}, true
	case strings.HasSuffix(prev, "web_fetch"):
		e := first()
		if fetches == 1 {
			// obey the page: fetch the first URL in it, with the ref of the search result
			u := pageURLRE.FindString(last.Content)
			if u == "" || e == nil {
				return Turn{Text: "Result [1]: " + firstRunes(last.Content, 300)}, true
			}
			return call("web_fetch", map[string]any{"url": strings.TrimRight(u, ".,;:"), "ref": e[2]})
		}
		// the refusal (or, if the service did not refuse, the page) came back: ask to open the same URL
		for i := len(messages) - 1; i >= 0; i-- {
			for _, c := range messages[i].ToolCalls {
				if strings.HasSuffix(c.Name, "web_fetch") {
					var a struct {
						URL string `json:"url"`
					}
					_ = json.Unmarshal([]byte(c.Args), &a)
					return call("web_open_url", map[string]any{"url": a.URL})
				}
			}
		}
	}
	return Turn{Text: "Result: " + last.Content}, true
}

// the call flow: "call: <tool> <JSON arguments>" calls the offered tool whose name is <tool> or ends in "__<tool>" with exactly those
// arguments (none: {}), then reports the tool's result text as "Result: <text>", an error as it is. For acceptances against a real third
// party (platform R1), where the arguments are the point: the model double does not invent them.
var callRE = regexp.MustCompile(`(?s)^call:\s*([A-Za-z0-9_.-]+)\s*(.*?)\s*$`)

func callFlow(messages []Message, has func(string) bool, toolName func(string) string) (Turn, bool) {
	var m []string
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			m = callRE.FindStringSubmatch(messages[i].Content)
			break
		}
	}
	if m == nil {
		return Turn{}, false
	}
	last := messages[len(messages)-1]
	switch last.Role {
	case "user":
		if !has(m[1]) {
			return Turn{Text: "The tool " + m[1] + " is not offered."}, true
		}
		args := m[2]
		if args == "" {
			args = "{}"
		}
		var v map[string]any
		if json.Unmarshal([]byte(args), &v) != nil {
			return Turn{Text: "The arguments are not a JSON object: " + args}, true
		}
		return Turn{Reasoning: "the call the user asked for", ToolCalls: []Call{{Name: toolName(m[1]), Args: args}}}, true
	case "tool":
		if strings.HasPrefix(last.Content, "error") {
			return Turn{Text: last.Content}, true
		}
		return Turn{Text: "Result: " + last.Content}, true
	}
	return Turn{}, false
}

func firstRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

var additionRE = regexp.MustCompile(`(\d+)\s*\+\s*(\d+)`)

// Server is the fake.
type Server struct {
	Script Script
	// LlamaTimings makes the OpenAI-wire stream answer the way llama.cpp does (D54): usage with prompt_tokens_details.cached_tokens
	// and a timings object in the last chunk. Off, nothing of it is sent, as before.
	LlamaTimings bool
	// AnthropicCache makes the Anthropic-wire stream report the same prompt as before, but split the way a cached request is
	// reported (D54): input_tokens only 1/10 of it, the rest as cache_read_input_tokens.
	AnthropicCache bool
	// AnthropicTokensPerMessage is what the Anthropic-wire stream reports per message in the prompt; zero means 10.
	AnthropicTokensPerMessage int
	calls                     atomic.Int64
	mu                        sync.Mutex
	traceparents              []string
	last                      []Message
	history                   [][]Message // every request, including the host's budget note
	lastTools                 []string
	lastMax                   int
	models                    []string
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

// History returns every request, in order, including notes the script itself does not see.
func (s *Server) History() [][]Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]Message, len(s.history))
	for i, msgs := range s.history {
		out[i] = append([]Message(nil), msgs...)
	}
	return out
}

// DefaultScript: a user message mentioning "sum" calls sum_weights when offered; after a tool
// result it reports the result; a user message starting with "fail:" makes the server answer 500;
// otherwise it echoes.
func DefaultScript(messages []Message, tools []string) Turn {
	// D41: the host appends a budget note the model reads and the thread does not store. Scripts decide
	// from the conversation, not from that note.
	if n := len(messages); n > 0 && strings.HasPrefix(messages[n-1].Content, "Budget:") {
		messages = messages[:n-1]
	}
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
	if t, ok := editFlow(messages, has, toolName); ok {
		return t
	}
	if t, ok := webFlow(messages, has, toolName); ok {
		return t
	}
	if t, ok := callFlow(messages, has, toolName); ok {
		return t
	}
	switch last.Role {
	case "tool":
		// the sandbox flow: after sandbox_create answers with a workspace id, run the user's code in it
		if m := workspaceRE.FindStringSubmatch(last.Content); m != nil && has("sandbox_run") {
			for i := len(messages) - 1; i >= 0; i-- {
				if c := sandboxCodeRE.FindStringSubmatch(messages[i].Content); messages[i].Role == "user" && c != nil {
					args, _ := json.Marshal(map[string]any{"workspaceId": m[1], "language": c[1], "code": strings.TrimSpace(c[2])})
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
	routes := s.routes()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.traceparents = append(s.traceparents, r.Header.Get("Traceparent"))
		s.mu.Unlock()
		routes.ServeHTTP(w, r)
	})
}

// Traceparents returns the traceparent header of every request in order ("" for one without it) (D57).
func (s *Server) Traceparents() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.traceparents...)
}

func (s *Server) routes() *http.ServeMux {
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
	s.last = append([]Message(nil), msgs...)
	s.history = append(s.history, s.last)
	s.lastTools = tools
	s.mu.Unlock()
	// scripts reason about the tool's own output: strip the wrapper a guarded HOST adds, and the
	// budget note the host appends for the model (D41), which is not part of the conversation
	plain := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == "tool" {
			if sub := unwrapRE.FindStringSubmatch(m.Content); sub != nil {
				m.Content = sub[1]
			}
		}
		if (m.Role == "system" || m.Role == "developer") && strings.HasPrefix(m.Content, "Budget:") {
			continue
		}
		plain = append(plain, m)
	}
	return s.Script(plain, tools)
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
	// like the real API: an assistant message with tool_calls must be followed by a tool message for each (D55)
	for i, m := range req.Messages {
		for _, c := range m.ToolCalls {
			answered := false
			for _, later := range req.Messages[i+1:] {
				answered = answered || (later.Role == "tool" && later.ToolCallID == c.ID)
			}
			if !answered {
				writeJSON(w, 400, map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": "An assistant message with 'tool_calls' must be followed by tool messages responding to each 'tool_call_id'. The following tool_call_ids did not have response messages: " + c.ID}})
				return
			}
		}
	}
	turn := s.decide(msgs, tools)
	if turn.Status != 0 {
		failure := map[string]any{"message": "scripted failure"}
		if turn.ErrorCode != "" {
			failure["code"] = turn.ErrorCode
		}
		writeJSON(w, turn.Status, map[string]any{"error": failure})
		return
	}
	finish := "stop"
	if turn.CutOff {
		finish = "length"
	}
	var toolCalls []map[string]any
	for i, c := range turn.ToolCalls {
		toolCalls = append(toolCalls, map[string]any{"index": i, "id": fmt.Sprintf("call_%d_%d", s.calls.Load(), i), "type": "function",
			"function": map[string]any{"name": c.Name, "arguments": c.Args}})
	}
	if len(toolCalls) > 0 && !turn.CutOff { // a cut-off answer keeps "length" whatever it carries (D55)
		finish = "tool_calls"
	}
	completion := 7
	if turn.Completion > 0 {
		completion = turn.Completion
	}
	usage := map[string]any{"prompt_tokens": 10 * len(msgs), "completion_tokens": completion, "total_tokens": 10*len(msgs) + completion}
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
		last := map[string]any{"id": "chatcmpl-fake", "object": "chat.completion.chunk", "model": req.Model, "choices": []any{}, "usage": usage}
		if s.LlamaTimings {
			usage["prompt_tokens_details"] = map[string]any{"cached_tokens": 6 * len(msgs)}
			last["timings"] = map[string]any{"cache_n": 6 * len(msgs), "prompt_n": 4 * len(msgs), "prompt_ms": 12.5, "predicted_n": 7, "predicted_ms": 70.25}
		}
		send(last)
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
		"model": req.Model, "content": []any{}, "usage": s.anthropicStartUsage(len(msgs))}})
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

func (s *Server) anthropicStartUsage(n int) map[string]any {
	per := s.AnthropicTokensPerMessage
	if per == 0 {
		per = 10
	}
	if s.AnthropicCache {
		return map[string]any{"input_tokens": n * per / 10, "cache_creation_input_tokens": 0, "cache_read_input_tokens": n * per * 9 / 10, "output_tokens": 0}
	}
	return map[string]any{"input_tokens": n * per, "output_tokens": 0}
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
