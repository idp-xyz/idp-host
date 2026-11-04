package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"idp-host/internal/a2a"
	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/authorization"
	"idp-host/internal/run"
	"idp-host/internal/transport"
)

// A2A surface (ADR-0001 D30): every published agent is an A2A agent. The agent card lives under
// /a2a/agents/{ref}/.well-known/agent-card.json; /a2a/agents/{ref} is the JSON-RPC endpoint for
// message/send, message/stream, tasks/get and tasks/cancel. A task is a HOST run, its contextId the
// thread; the same bearer tokens as the REST and AG-UI surfaces apply.

func (a *API) a2aRoutes(mux *http.ServeMux) {
	needUser := func(w http.ResponseWriter, p authorization.Principal) bool {
		if !p.Has(authorization.RoleUser) && !p.Has(authorization.RoleAdmin) {
			transport.WriteProblem(w, transport.Fail(http.StatusForbidden, "FORBIDDEN", "role "+authorization.RoleUser+" required"))
			return false
		}
		return true
	}
	mux.Handle("GET /a2a/agents/{ref}/.well-known/agent-card.json", a.authOnly(func(w http.ResponseWriter, r *http.Request, p authorization.Principal) {
		if !needUser(w, p) {
			return
		}
		card, err := a.agentCard(r, p, r.PathValue("ref"))
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		transport.WriteJSON(w, 200, card)
	}))
	mux.Handle("POST /a2a/agents/{ref}", a.authOnly(func(w http.ResponseWriter, r *http.Request, p authorization.Principal) {
		if !needUser(w, p) {
			return
		}
		var req a2a.Request
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBody))
		if err := dec.Decode(&req); err != nil {
			rpcError(w, nil, a2a.CodeParse, "parse error: "+err.Error())
			return
		}
		if req.JSONRPC != "2.0" || req.Method == "" {
			rpcError(w, req.ID, a2a.CodeInvalidRequest, "jsonrpc 2.0 request with a method required")
			return
		}
		switch req.Method {
		case "message/send":
			a.a2aSend(w, r, p, req, false)
		case "message/stream":
			a.a2aSend(w, r, p, req, true)
		case "tasks/get":
			var params a2a.IDParams
			if err := json.Unmarshal(req.Params, &params); err != nil || params.ID == "" {
				rpcError(w, req.ID, a2a.CodeInvalidParams, "id is required")
				return
			}
			task, err := a.task(r.Context(), p, params.ID, params.HistoryLength)
			if err != nil {
				a.rpcStoreError(w, req.ID, err)
				return
			}
			rpcResult(w, req.ID, task)
		case "tasks/cancel":
			var params a2a.IDParams
			if err := json.Unmarshal(req.Params, &params); err != nil || params.ID == "" {
				rpcError(w, req.ID, a2a.CodeInvalidParams, "id is required")
				return
			}
			before, err := a.Store.GetRun(r.Context(), p.Tenant, ownerScope(p), params.ID)
			if err != nil {
				a.rpcStoreError(w, req.ID, err)
				return
			}
			if run.Terminal(before.Status) {
				rpcError(w, req.ID, a2a.CodeNotCancelable, "task already "+a2a.State(before.Status))
				return
			}
			if _, err := a.Store.CancelRun(r.Context(), p.Tenant, ownerScope(p), params.ID); err != nil {
				a.rpcStoreError(w, req.ID, err)
				return
			}
			task, err := a.task(r.Context(), p, params.ID, nil)
			if err != nil {
				a.rpcStoreError(w, req.ID, err)
				return
			}
			rpcResult(w, req.ID, task)
		case "tasks/pushNotificationConfig/set", "tasks/pushNotificationConfig/get", "tasks/resubscribe":
			rpcError(w, req.ID, a2a.CodeUnsupportedOp, req.Method+" is not supported by this agent")
		default:
			rpcError(w, req.ID, a2a.CodeMethodNotFound, "method not found: "+req.Method)
		}
	}))
}

func rpcResult(w http.ResponseWriter, id json.RawMessage, result any) {
	transport.WriteJSON(w, 200, a2a.Response{JSONRPC: "2.0", ID: id, Result: result})
}

func rpcError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	transport.WriteJSON(w, 200, a2a.Response{JSONRPC: "2.0", ID: id, Error: &a2a.Error{Code: code, Message: message}})
}

// rpcStoreError maps store and problem errors to JSON-RPC errors.
func (a *API) rpcStoreError(w http.ResponseWriter, id json.RawMessage, err error) {
	var pb *transport.Problem
	if errors.As(err, &pb) {
		code := a2a.CodeInvalidParams
		if pb.Status >= 500 {
			code = a2a.CodeInternal
		}
		rpcError(w, id, code, pb.Message)
		return
	}
	type coded interface{ Error() string }
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "NOT_FOUND"):
		rpcError(w, id, a2a.CodeTaskNotFound, "task not found")
	case strings.HasPrefix(msg, "CONFLICT"), strings.HasPrefix(msg, "INVALID"), strings.HasPrefix(msg, "FORBIDDEN"), strings.HasPrefix(msg, "QUOTA_EXCEEDED"):
		rpcError(w, id, a2a.CodeInvalidParams, msg)
	default:
		a.Log.Error("a2a request failed", "error", err)
		rpcError(w, id, a2a.CodeInternal, "internal error")
	}
	_ = coded(nil)
}

func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// agentCard describes a published agent.
func (a *API) agentCard(r *http.Request, p authorization.Principal, ref string) (*a2a.Card, error) {
	ag, err := a.Store.GetAgent(r.Context(), p.Tenant, ref)
	if err != nil {
		return nil, err
	}
	if ag.CurrentReleaseID == nil {
		return nil, transport.Fail(http.StatusConflict, "CONFLICT", "agent has no published release")
	}
	rel, err := a.Store.GetRelease(r.Context(), p.Tenant, *ag.CurrentReleaseID)
	if err != nil {
		return nil, err
	}
	var doc agent.Document
	_ = json.Unmarshal(rel.Document, &doc)
	card := &a2a.Card{Name: ag.Name, Description: ag.DisplayName, URL: baseURL(r) + "/a2a/agents/" + ag.Name, Version: strconv.Itoa(rel.Version), ProtocolVersion: a2a.ProtocolVersion,
		Capabilities: a2a.Capabilities{Streaming: true}, DefaultInputModes: []string{"text/plain"}, DefaultOutputModes: []string{"text/plain"},
		SecuritySchemes: map[string]a2a.Scheme{"bearer": {Type: "http", Scheme: "bearer"}}, Security: []map[string][]string{{"bearer": {}}},
		Provider: &a2a.Provider{Organization: "idp-host"}}
	card.Skills = []a2a.Skill{{ID: ag.Name, Name: ag.DisplayName, Description: firstLine(doc.Definition.Instructions), Tags: []string{"idp-host", "agent"}}}
	for _, s := range doc.Skills {
		card.Skills = append(card.Skills, a2a.Skill{ID: ag.Name + "/" + s.Name, Name: s.Name, Description: firstLine(s.Definition.Instructions), Tags: []string{"idp-host", "skill"}})
	}
	return card, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// task builds the task of a run the principal may see.
func (a *API) task(ctx context.Context, p authorization.Principal, runID string, historyLength *int) (a2a.Task, error) {
	r, err := a.Store.GetRun(ctx, p.Tenant, ownerScope(p), runID)
	if err != nil {
		return a2a.Task{}, err
	}
	msgs, err := a.Store.ListMessages(ctx, p.Tenant, ownerScope(p), r.ThreadID)
	if err != nil {
		return a2a.Task{}, err
	}
	return a2a.TaskOf(r, msgs, historyLength), nil
}

// a2aSend handles message/send (optionally blocking) and message/stream.
func (a *API) a2aSend(w http.ResponseWriter, r *http.Request, p authorization.Principal, req a2a.Request, stream bool) {
	var params a2a.SendParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		rpcError(w, req.ID, a2a.CodeInvalidParams, "message is required")
		return
	}
	m := params.Message
	text := m.Text()
	if m.Role != "user" || strings.TrimSpace(text) == "" {
		rpcError(w, req.ID, a2a.CodeInvalidParams, "a user message with text parts is required")
		return
	}
	for _, part := range m.Parts {
		if part.Kind != "text" {
			rpcError(w, req.ID, a2a.CodeContentNotSupp, "only text parts are supported")
			return
		}
	}
	if m.MessageID == "" {
		m.MessageID = "a2a_" + strings.ReplaceAll(newUUID(), "-", "")
	}
	if !agui.ValidID(m.MessageID) || m.ContextID != "" && !agui.ValidID(m.ContextID) {
		rpcError(w, req.ID, a2a.CodeInvalidParams, "messageId and contextId must be identifiers")
		return
	}
	if m.TaskID != "" {
		// a HOST run takes no further messages: a waiting task is resolved through the confirmation
		// API, a finished one is followed by a new task on the same context
		if prev, err := a.Store.GetRun(r.Context(), p.Tenant, ownerScope(p), m.TaskID); err == nil && !run.Terminal(prev.Status) {
			rpcError(w, req.ID, a2a.CodeUnsupportedOp, "task "+m.TaskID+" is "+a2a.State(prev.Status)+"; decide its pending tool calls through /v1/runs/{id}/tool-calls or start a new task on the context")
			return
		}
	}
	// idp.host/ keys of the message metadata are the run options (release pinning, caller mode, ...)
	var forwarded json.RawMessage
	if len(m.Metadata) > 0 {
		opts := map[string]any{}
		for k, v := range m.Metadata {
			if strings.HasPrefix(k, "idp.host/") {
				opts[k] = v
			}
		}
		if len(opts) > 0 {
			forwarded, _ = json.Marshal(opts)
		}
	}
	in := agui.RunAgentInput{ThreadID: m.ContextID, Messages: []agui.Message{{ID: m.MessageID, Role: agui.RoleUser, Content: text}}, Tools: []agui.Tool{}, Context: []agui.Context{}, ForwardedProps: forwarded}
	out, _, err := a.createRun(r, p, r.PathValue("ref"), "a2a:"+m.MessageID, in)
	if err != nil {
		a.rpcStoreError(w, req.ID, err)
		return
	}
	if stream {
		a.a2aStream(w, r, p, req.ID, out)
		return
	}
	blocking := params.Configuration != nil && params.Configuration.Blocking != nil && *params.Configuration.Blocking
	var hl *int
	if params.Configuration != nil {
		hl = params.Configuration.HistoryLength
	}
	if blocking {
		ctx, cancel := context.WithDeadline(r.Context(), out.DeadlineAt.Add(5*time.Second))
		defer cancel()
		poll := time.NewTicker(a.StreamPoll)
		defer poll.Stop()
		for {
			cur, err := a.Store.GetRun(ctx, p.Tenant, ownerScope(p), out.ID)
			if err != nil {
				a.rpcStoreError(w, req.ID, err)
				return
			}
			if run.Terminal(cur.Status) || cur.Status == run.StatusAwaitingInput {
				break
			}
			select {
			case <-ctx.Done():
				task, err := a.task(r.Context(), p, out.ID, hl)
				if err != nil {
					a.rpcStoreError(w, req.ID, err)
					return
				}
				rpcResult(w, req.ID, task) // still working: the client polls tasks/get
				return
			case <-poll.C:
			}
		}
	}
	task, err := a.task(r.Context(), p, out.ID, hl)
	if err != nil {
		a.rpcStoreError(w, req.ID, err)
		return
	}
	rpcResult(w, req.ID, task)
}

// a2aStream turns the run's event log into A2A streaming responses until the task rests.
func (a *API) a2aStream(w http.ResponseWriter, r *http.Request, p authorization.Principal, id json.RawMessage, rn *run.Run) {
	sse := transport.NewSSEWriter(w)
	if sse == nil {
		transport.WriteProblem(w, transport.Fail(http.StatusInternalServerError, "INTERNAL", "streaming unsupported"))
		return
	}
	send := func(result any) error {
		b, _ := json.Marshal(a2a.Response{JSONRPC: "2.0", ID: id, Result: result})
		return sse.Send("", "", b)
	}
	if err := send(a2a.Task{Kind: "task", ID: rn.ID, ContextID: rn.ThreadID, Status: a2a.Status{State: a2a.StateSubmitted, Timestamp: time.Now().UTC().Format(time.RFC3339)}}); err != nil {
		return
	}
	ctx := r.Context()
	poll := time.NewTicker(a.StreamPoll)
	defer poll.Stop()
	keep := time.NewTicker(a.StreamKeepAlive)
	defer keep.Stop()
	var after int64
	working := false
	for {
		events, err := a.Store.Events(ctx, p.Tenant, ownerScope(p), rn.ID, after, 500)
		if err != nil {
			_ = send(a2a.StatusUpdate{Kind: "status-update", TaskID: rn.ID, ContextID: rn.ThreadID, Status: a2a.Status{State: a2a.StateFailed}, Final: true})
			return
		}
		for _, e := range events {
			after = e.ID
			ev := e.Payload
			switch e.Type {
			case agui.EventStepStarted:
				if !working {
					working = true
					if err := send(a2a.StatusUpdate{Kind: "status-update", TaskID: rn.ID, ContextID: rn.ThreadID, Status: a2a.Status{State: a2a.StateWorking, Timestamp: time.Now().UTC().Format(time.RFC3339)}}); err != nil {
						return
					}
				}
			case agui.EventTextMessageContent:
				msgID, _ := ev["messageId"].(string)
				delta, _ := ev["delta"].(string)
				if err := send(a2a.ArtifactUpdate{Kind: "artifact-update", TaskID: rn.ID, ContextID: rn.ThreadID, Artifact: a2a.Artifact{ArtifactID: msgID, Name: "answer", Parts: []a2a.Part{{Kind: "text", Text: delta}}}, Append: true}); err != nil {
					return
				}
			case agui.EventTextMessageEnd:
				msgID, _ := ev["messageId"].(string)
				if err := send(a2a.ArtifactUpdate{Kind: "artifact-update", TaskID: rn.ID, ContextID: rn.ThreadID, Artifact: a2a.Artifact{ArtifactID: msgID, Name: "answer", Parts: []a2a.Part{}}, Append: true, LastChunk: true}); err != nil {
					return
				}
			case agui.EventRunFinished, agui.EventRunError:
				cur, err := a.Store.GetRun(ctx, p.Tenant, ownerScope(p), rn.ID)
				if err != nil {
					return
				}
				st := a2a.Status{State: a2a.State(cur.Status), Timestamp: time.Now().UTC().Format(time.RFC3339)}
				if cur.FailureCode != "" {
					st.Message = &a2a.Message{Kind: "message", MessageID: "status_" + cur.ID, Role: "agent", Parts: []a2a.Part{{Kind: "text", Text: cur.FailureCode + ": " + cur.FailureMessage}}, TaskID: cur.ID, ContextID: cur.ThreadID}
				}
				_ = send(a2a.StatusUpdate{Kind: "status-update", TaskID: rn.ID, ContextID: rn.ThreadID, Status: st, Final: true})
				return
			}
		}
		if len(events) == 500 {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
		case <-keep.C:
			if err := sse.Comment("keep-alive"); err != nil {
				return
			}
		}
	}
}
