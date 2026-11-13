// Package fakemcp is a minimal MCP 2026-07-28 modern server for tests: server/discover, tools/list
// and tools/call over stateless Streamable HTTP, with a few scripted tools, an optional bearer
// requirement, and a record of the credentials it saw. It stands in for the MCP Infrastructure
// gateway in unit and integration tests; the end-to-end suite uses the real gateway.
package fakemcp

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Tool is a scripted tool.
type Tool struct {
	Name        string
	Description string
	InputSchema string
	Handle      func(args map[string]any) (text string, isError bool)
	// Structured, when set, gives the result's structuredContent.
	Structured func(args map[string]any) any
}

// Server is the fake.
type Server struct {
	Tools         []Tool
	RequireBearer string            // when set, calls must carry exactly this bearer
	StreamResults bool              // answer tools/call as an SSE stream
	Results       map[string]string // when set for a tool name, that text is returned instead of the handler's
	mu            sync.Mutex
	bearers       []string
	calls         []Call
}

// Call is one recorded tools/call.
type Call struct {
	Name   string
	Args   map[string]any
	Bearer string
}

// New returns a server with the default tools: sum (a+b), echo (message), fail (isError), slow.
func New() *Server {
	return &Server{Results: map[string]string{}, Tools: []Tool{
		{Name: "sum", Description: "Adds a and b", InputSchema: `{"type":"object","properties":{"a":{"type":"number"},"b":{"type":"number"}},"required":["a","b"]}`,
			Handle: func(args map[string]any) (string, bool) {
				a, _ := args["a"].(float64)
				b, _ := args["b"].(float64)
				return json.Number(strings.TrimSuffix(strings.TrimSuffix(formatFloat(a+b), "0"), ".")).String(), false
			}},
		{Name: "echo", Description: "Echoes text", InputSchema: `{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`,
			Handle: func(args map[string]any) (string, bool) { m, _ := args["text"].(string); return m, false }},
		{Name: "fail", Description: "Always fails", InputSchema: `{"type":"object"}`,
			Handle: func(map[string]any) (string, bool) { return "it failed", true }},
		{Name: "slow", Description: "Sleeps", InputSchema: `{"type":"object","properties":{"ms":{"type":"number"}}}`,
			Handle: func(args map[string]any) (string, bool) {
				ms, _ := args["ms"].(float64)
				time.Sleep(time.Duration(ms) * time.Millisecond)
				return "done", false
			}},
	}}
}

func formatFloat(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// Calls returns the recorded tools/call requests.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// Bearers returns every bearer seen, in order.
func (s *Server) Bearers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bearers...)
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  map[string]any  `json:"params"`
}

func (s *Server) write(w http.ResponseWriter, id json.RawMessage, result any, rpcErr map[string]any, status int) {
	body := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		body["error"] = rpcErr
	} else {
		body["result"] = result
	}
	b, _ := json.Marshal(body)
	if s.StreamResults && rpcErr == nil {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(": keepalive\n\nevent: message\ndata: " + string(b) + "\n\n"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// Handler serves POST /mcp.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mcp", func(w http.ResponseWriter, r *http.Request) {
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		s.mu.Lock()
		s.bearers = append(s.bearers, bearer)
		s.mu.Unlock()
		if s.RequireBearer != "" && bearer != s.RequireBearer {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var req request
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil || req.JSONRPC != "2.0" {
			s.write(w, nil, nil, map[string]any{"code": -32700, "message": "parse error"}, http.StatusBadRequest)
			return
		}
		if r.Header.Get("MCP-Protocol-Version") != "2026-07-28" || r.Header.Get("Mcp-Method") != req.Method {
			s.write(w, req.ID, nil, map[string]any{"code": -32600, "message": "missing modern headers"}, http.StatusBadRequest)
			return
		}
		meta, _ := req.Params["_meta"].(map[string]any)
		if meta["io.modelcontextprotocol/protocolVersion"] != "2026-07-28" {
			s.write(w, req.ID, nil, map[string]any{"code": -32600, "message": "missing _meta"}, http.StatusBadRequest)
			return
		}
		info := map[string]any{"io.modelcontextprotocol/serverInfo": map[string]any{"name": "fakemcp", "version": "1"}}
		switch req.Method {
		case "server/discover":
			s.write(w, req.ID, map[string]any{"supportedVersions": []string{"2026-07-28"}, "capabilities": map[string]any{"tools": map[string]any{}},
				"resultType": "complete", "_meta": info}, nil, 200)
		case "tools/list":
			var tools []map[string]any
			for _, t := range s.Tools {
				tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": json.RawMessage(t.InputSchema)})
			}
			s.write(w, req.ID, map[string]any{"tools": tools, "_meta": info}, nil, 200)
		case "tools/call":
			name, _ := req.Params["name"].(string)
			if r.Header.Get("Mcp-Name") != name {
				s.write(w, req.ID, nil, map[string]any{"code": -32600, "message": "Mcp-Name mismatch"}, http.StatusBadRequest)
				return
			}
			args, _ := req.Params["arguments"].(map[string]any)
			s.mu.Lock()
			s.calls = append(s.calls, Call{Name: name, Args: args, Bearer: bearer})
			s.mu.Unlock()
			for _, t := range s.Tools {
				if t.Name == name {
					text, isErr := t.Handle(args)
					if fixed, ok := s.Results[name]; ok {
						text, isErr = fixed, false
					}
					res := map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isErr, "_meta": info}
					if t.Structured != nil && !isErr {
						res["structuredContent"] = t.Structured(args)
					}
					s.write(w, req.ID, res, nil, 200)
					return
				}
			}
			s.write(w, req.ID, nil, map[string]any{"code": -32602, "message": "Unknown tool: " + name}, 200)
		default:
			s.write(w, req.ID, nil, map[string]any{"code": -32601, "message": "Method not found"}, http.StatusNotFound)
		}
	})
	return mux
}
