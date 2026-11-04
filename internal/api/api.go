// Package api is the HOST's HTTP surface (ADR-0001 D6): the administrative REST API for agents and
// releases, the user REST API for sessions, threads, runs and confirmations, the AG-UI endpoint,
// and the event stream every run exposes. Every request is authenticated with a bearer token;
// tenant isolation is the database's (RLS bound to the principal's tenant).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"idp-host/adapters/identity"
	"idp-host/adapters/store/postgres"
	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/authorization"
	"idp-host/internal/credential"
	"idp-host/internal/observability"
	"idp-host/internal/run"
	"idp-host/internal/transport"
)

// MaxBody bounds request bodies.
const MaxBody = 4 << 20

// Verifier authenticates bearer tokens.
type Verifier interface {
	Verify(ctx context.Context, token string) (authorization.Principal, error)
}

// GatewayAuth decides how a run obtains its gateway credential.
type GatewayAuth struct {
	// Exchanger performs the delegation; nil means the gateway is called with a static credential
	// configured on the worker side and the run carries none.
	Exchanger *credential.Exchanger
	// ActorToken sends the HOST's own token as actor_token (Keycloak's delegation profile).
	ActorToken bool
}

// API serves the HOST.
type API struct {
	Store     *postgres.Store
	Auth      Verifier
	Gateways  map[string]GatewayAuth // by gateway name as the release refers to it
	Providers []string
	Log       *slog.Logger
	Metrics   *observability.Metrics
	Version   string

	// StreamPoll is how often the event stream polls the store (default 150 ms).
	StreamPoll time.Duration
	// StreamKeepAlive is the keep-alive comment interval (default 15 s).
	StreamKeepAlive time.Duration

	actorMu    chan struct{}
	actorToken *credential.Token
	quota      *observability.CounterVec
}

func quotaKind(message string) string {
	if strings.Contains(message, "concurrent") {
		return "concurrency"
	}
	return "budget"
}

type ctxKey int

const principalKey ctxKey = 1

func principalOf(r *http.Request) authorization.Principal {
	p, _ := r.Context().Value(principalKey).(authorization.Principal)
	return p
}

// unauthenticated paths: liveness, readiness and version carry no tenant data.
var open = map[string]bool{"/healthz": true, "/readyz": true, "/version": true}

func (a *API) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if open[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		tok, err := identity.Bearer(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", "Bearer")
			transport.WriteProblem(w, transport.Fail(http.StatusUnauthorized, "UNAUTHENTICATED", "bearer token required"))
			return
		}
		p, err := a.Auth.Verify(r.Context(), tok)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			transport.WriteProblem(w, transport.Fail(http.StatusUnauthorized, "UNAUTHENTICATED", "invalid token"))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
	})
}

type handler func(r *http.Request, p authorization.Principal) (int, any, error)

func (a *API) role(role string, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principalOf(r)
		if role != "" && !p.Has(role) && !(role == authorization.RoleUser && p.Has(authorization.RoleAdmin)) {
			transport.WriteProblem(w, transport.Fail(http.StatusForbidden, "FORBIDDEN", "role "+role+" required"))
			return
		}
		status, body, err := h(r, p)
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		if body == nil {
			w.WriteHeader(status)
			return
		}
		transport.WriteJSON(w, status, body)
	}
}

func (a *API) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var se *postgres.Error
	var pb *transport.Problem
	switch {
	case errors.As(err, &pb):
	case errors.As(err, &se):
		status := map[string]int{"NOT_FOUND": 404, "CONFLICT": 409, "FORBIDDEN": 403, "INVALID": 422, "GONE": 410, "QUOTA_EXCEEDED": 429}[se.Code]
		if se.Code == "QUOTA_EXCEEDED" && a.quota != nil {
			a.quota.Inc(quotaKind(se.Message))
		}
		if status == 0 {
			status = 500
		}
		pb = transport.Fail(status, se.Code, se.Message)
	default:
		a.Log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err, "trace", observability.TraceID(r.Context()))
		pb = transport.Fail(http.StatusInternalServerError, "INTERNAL", "internal error")
	}
	transport.WriteProblem(w, pb)
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, MaxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return transport.Fail(http.StatusBadRequest, "BAD_REQUEST", "invalid JSON body: "+err.Error())
	}
	return nil
}

// Handler builds the router.
func (a *API) Handler() http.Handler {
	if a.Log == nil {
		a.Log = slog.Default()
	}
	if a.StreamPoll == 0 {
		a.StreamPoll = 150 * time.Millisecond
	}
	if a.StreamKeepAlive == 0 {
		a.StreamKeepAlive = 15 * time.Second
	}
	if a.actorMu == nil {
		a.actorMu = make(chan struct{}, 1)
	}
	if a.Metrics != nil {
		a.quota = a.Metrics.Counter("idp_host_quota_rejections_total", "Run requests refused by tenant governance, by kind.", "kind")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := a.Store.Pool.Ping(ctx); err != nil {
			transport.WriteProblem(w, transport.Fail(http.StatusServiceUnavailable, "NOT_READY", "database unreachable"))
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		transport.WriteJSON(w, 200, map[string]any{"service": "idp-host", "version": a.Version})
	})

	admin := authorization.RoleAdmin
	mux.Handle("GET /api/v1/providers", a.role(admin, func(*http.Request, authorization.Principal) (int, any, error) {
		return 200, map[string]any{"providers": a.Providers}, nil
	}))
	mux.Handle("POST /api/v1/agents", a.role(admin, func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in struct {
			Name        string           `json:"name"`
			DisplayName string           `json:"displayName"`
			Definition  agent.Definition `json:"definition"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		out, err := a.Store.CreateAgent(r.Context(), p.Tenant, in.Name, in.DisplayName, in.Definition, p.Subject)
		return http.StatusCreated, out, err
	}))
	mux.Handle("GET /api/v1/agents", a.role(admin, func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.ListAgents(r.Context(), p.Tenant)
		return 200, map[string]any{"agents": out}, err
	}))
	mux.Handle("GET /api/v1/agents/{ref}", a.role(admin, func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.GetAgent(r.Context(), p.Tenant, r.PathValue("ref"))
		return 200, out, err
	}))
	mux.Handle("PATCH /api/v1/agents/{ref}", a.role(admin, func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in struct {
			DisplayName *string           `json:"displayName"`
			Definition  *agent.Definition `json:"definition"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		out, err := a.Store.UpdateAgent(r.Context(), p.Tenant, r.PathValue("ref"), in.DisplayName, in.Definition, p.Subject)
		return 200, out, err
	}))
	mux.Handle("POST /api/v1/agents/{ref}/releases", a.role(admin, func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.CreateRelease(r.Context(), p.Tenant, r.PathValue("ref"), p.Subject)
		return http.StatusCreated, out, err
	}))
	mux.Handle("GET /api/v1/agents/{ref}/releases", a.role(admin, func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.ListReleases(r.Context(), p.Tenant, r.PathValue("ref"))
		return 200, map[string]any{"releases": out}, err
	}))
	mux.Handle("POST /api/v1/agents/{ref}/set-current", a.role(admin, func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in struct {
			ReleaseID string `json:"releaseId"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		out, err := a.Store.SetCurrentRelease(r.Context(), p.Tenant, r.PathValue("ref"), in.ReleaseID, p.Subject)
		return 200, out, err
	}))
	mux.Handle("GET /api/v1/releases/{id}", a.role(admin, func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.GetRelease(r.Context(), p.Tenant, r.PathValue("id"))
		return 200, out, err
	}))
	mux.Handle("GET /api/v1/releases/{id}/envelope", a.role(admin, func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.GetEnvelope(r.Context(), p.Tenant, r.PathValue("id"))
		return 200, out, err
	}))
	mux.Handle("POST /api/v1/releases/{id}/approve", a.role(authorization.RoleApprover, func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.ApproveRelease(r.Context(), p.Tenant, r.PathValue("id"), p.Subject)
		return 200, out, err
	}))
	mux.Handle("POST /api/v1/releases/{id}/reject", a.role(authorization.RoleApprover, func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in struct {
			Reason string `json:"reason"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		out, err := a.Store.RejectRelease(r.Context(), p.Tenant, r.PathValue("id"), p.Subject, in.Reason)
		return 200, out, err
	}))
	mux.Handle("POST /api/v1/skills", a.role(admin, func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in struct {
			Name        string                `json:"name"`
			DisplayName string                `json:"displayName"`
			Definition  agent.SkillDefinition `json:"definition"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		out, err := a.Store.CreateSkill(r.Context(), p.Tenant, in.Name, in.DisplayName, in.Definition, p.Subject)
		return http.StatusCreated, out, err
	}))
	mux.Handle("GET /api/v1/skills", a.role(admin, func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.ListSkills(r.Context(), p.Tenant)
		return 200, map[string]any{"skills": out}, err
	}))
	mux.Handle("GET /api/v1/skills/{ref}", a.role(admin, func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.GetSkill(r.Context(), p.Tenant, r.PathValue("ref"))
		return 200, out, err
	}))
	mux.Handle("PATCH /api/v1/skills/{ref}", a.role(admin, func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in struct {
			DisplayName *string                `json:"displayName"`
			Definition  *agent.SkillDefinition `json:"definition"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		out, err := a.Store.UpdateSkill(r.Context(), p.Tenant, r.PathValue("ref"), in.DisplayName, in.Definition, p.Subject)
		return 200, out, err
	}))
	mux.Handle("POST /api/v1/skills/{ref}/releases", a.role(admin, func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.CreateSkillRelease(r.Context(), p.Tenant, r.PathValue("ref"), p.Subject)
		return http.StatusCreated, out, err
	}))
	mux.Handle("GET /api/v1/skills/{ref}/releases", a.role(admin, func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.ListSkillReleases(r.Context(), p.Tenant, r.PathValue("ref"))
		return 200, map[string]any{"releases": out}, err
	}))
	mux.Handle("POST /api/v1/skills/{ref}/set-current", a.role(admin, func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in struct {
			ReleaseID string `json:"releaseId"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		out, err := a.Store.SetCurrentSkillRelease(r.Context(), p.Tenant, r.PathValue("ref"), in.ReleaseID, p.Subject)
		return 200, out, err
	}))
	mux.Handle("GET /api/v1/skill-releases/{id}", a.role(admin, func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.GetSkillRelease(r.Context(), p.Tenant, r.PathValue("id"))
		return 200, out, err
	}))
	mux.Handle("POST /api/v1/skill-releases/{id}/approve", a.role(authorization.RoleApprover, func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.ApproveSkillRelease(r.Context(), p.Tenant, r.PathValue("id"), p.Subject)
		return 200, out, err
	}))
	mux.Handle("POST /api/v1/skill-releases/{id}/reject", a.role(authorization.RoleApprover, func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in struct {
			Reason string `json:"reason"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		out, err := a.Store.RejectSkillRelease(r.Context(), p.Tenant, r.PathValue("id"), p.Subject, in.Reason)
		return 200, out, err
	}))
	mux.Handle("GET /api/v1/policy", a.role(admin, func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.GetPolicy(r.Context(), p.Tenant)
		return 200, out, err
	}))
	mux.Handle("PUT /api/v1/policy", a.role(admin, func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in postgres.Policy
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		out, err := a.Store.SetPolicy(r.Context(), p.Tenant, in, p.Subject)
		return 200, out, err
	}))
	mux.Handle("GET /api/v1/usage", a.role(admin, func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.GetUsage(r.Context(), p.Tenant)
		return 200, out, err
	}))
	mux.Handle("GET /api/v1/audit", a.role(admin, func(r *http.Request, p authorization.Principal) (int, any, error) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		out, err := a.Store.ListAudit(r.Context(), p.Tenant, limit)
		return 200, map[string]any{"events": out}, err
	}))

	user := authorization.RoleUser
	mux.Handle("POST /v1/sessions", a.role(user, func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in struct {
			Title string `json:"title"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		out, err := a.Store.CreateSession(r.Context(), p.Tenant, p.Subject, in.Title)
		return http.StatusCreated, out, err
	}))
	mux.Handle("GET /v1/sessions", a.role(user, func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.ListSessions(r.Context(), p.Tenant, p.Subject)
		return 200, map[string]any{"sessions": out}, err
	}))
	mux.Handle("GET /v1/sessions/{id}", a.role(user, func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.GetSession(r.Context(), p.Tenant, ownerScope(p), r.PathValue("id"))
		return 200, out, err
	}))
	mux.Handle("POST /v1/threads", a.role(user, func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in struct {
			ThreadID  string `json:"threadId"`
			SessionID string `json:"sessionId"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		out, err := a.Store.CreateThread(r.Context(), p.Tenant, p.Subject, in.ThreadID, in.SessionID)
		return http.StatusCreated, out, err
	}))
	mux.Handle("GET /v1/threads/{id}", a.role(user, func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.GetThread(r.Context(), p.Tenant, ownerScope(p), r.PathValue("id"))
		return 200, out, err
	}))
	mux.Handle("GET /v1/threads/{id}/messages", a.role(user, func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.ListMessages(r.Context(), p.Tenant, ownerScope(p), r.PathValue("id"))
		return 200, map[string]any{"messages": out}, err
	}))
	mux.Handle("GET /v1/threads/{id}/runs", a.role(user, func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.ListRuns(r.Context(), p.Tenant, ownerScope(p), r.PathValue("id"))
		return 200, map[string]any{"runs": out}, err
	}))
	mux.Handle("POST /v1/agents/{ref}/runs", a.role(user, func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in agui.RunAgentInput
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			key = in.RunID
		}
		out, replayed, err := a.createRun(r, p, r.PathValue("ref"), key, in)
		if err != nil {
			return 0, nil, err
		}
		if replayed {
			return 200, out, nil
		}
		return http.StatusAccepted, out, nil
	}))
	mux.Handle("GET /v1/runs/{id}", a.role(user, func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.GetRun(r.Context(), p.Tenant, ownerScope(p), r.PathValue("id"))
		return 200, out, err
	}))
	mux.Handle("GET /v1/runs/{id}/subruns", a.role(user, func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.ListChildRuns(r.Context(), p.Tenant, ownerScope(p), r.PathValue("id"))
		return 200, map[string]any{"runs": out}, err
	}))
	mux.Handle("GET /v1/runs/{id}/tool-calls", a.role(user, func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.ToolCalls(r.Context(), p.Tenant, ownerScope(p), r.PathValue("id"))
		return 200, map[string]any{"toolCalls": out}, err
	}))
	mux.Handle("POST /v1/runs/{id}/tool-calls/{call}/decide", a.role(user, func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in struct {
			Decision string `json:"decision"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		out, err := a.Store.DecideToolCall(r.Context(), p.Tenant, p.Subject, r.PathValue("id"), r.PathValue("call"), in.Decision)
		return 200, out, err
	}))
	mux.Handle("POST /v1/runs/{id}/cancel", a.role(user, func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.CancelRun(r.Context(), p.Tenant, ownerScope(p), r.PathValue("id"))
		return 200, out, err
	}))
	mux.Handle("GET /v1/runs/{id}/events", a.authOnly(func(w http.ResponseWriter, r *http.Request, p authorization.Principal) {
		if !p.Has(user) && !p.Has(admin) {
			transport.WriteProblem(w, transport.Fail(http.StatusForbidden, "FORBIDDEN", "role "+user+" required"))
			return
		}
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		if id := r.Header.Get("Last-Event-ID"); id != "" {
			if v, err := strconv.ParseInt(id, 10, 64); err == nil {
				after = v
			}
		}
		if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			a.stream(w, r, p.Tenant, ownerScope(p), r.PathValue("id"), after)
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		out, err := a.Store.Events(r.Context(), p.Tenant, ownerScope(p), r.PathValue("id"), after, limit)
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		transport.WriteJSON(w, 200, map[string]any{"events": out})
	}))
	mux.Handle("POST /ag-ui/agents/{ref}", a.authOnly(func(w http.ResponseWriter, r *http.Request, p authorization.Principal) {
		if !p.Has(user) && !p.Has(admin) {
			transport.WriteProblem(w, transport.Fail(http.StatusForbidden, "FORBIDDEN", "role "+user+" required"))
			return
		}
		var in agui.RunAgentInput
		if err := decode(r, &in); err != nil {
			a.writeError(w, r, err)
			return
		}
		key := in.RunID
		if key == "" {
			key = "agui-" + strings.ReplaceAll(postgres.NewUUID(), "-", "")
		}
		out, _, err := a.createRun(r, p, r.PathValue("ref"), key, in)
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		w.Header().Set("X-Run-Id", out.ID)
		a.stream(w, r, p.Tenant, ownerScope(p), out.ID, 0)
	}))
	var h http.Handler = a.authenticate(mux)
	if a.Metrics != nil {
		h = observability.HTTPMetrics(a.Metrics, "idp_host", h)
	}
	return h
}

func (a *API) authOnly(h func(http.ResponseWriter, *http.Request, authorization.Principal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { h(w, r, principalOf(r)) }
}

// ownerScope: users see their own objects, administrators every object of the tenant.
func ownerScope(p authorization.Principal) string {
	if p.Has(authorization.RoleAdmin) {
		return ""
	}
	return p.Subject
}

// createRun validates the request, obtains the gateway credential the run needs and queues it.
func (a *API) createRun(r *http.Request, p authorization.Principal, ref, key string, in agui.RunAgentInput) (*run.Run, bool, error) {
	if err := in.Validate(); err != nil {
		return nil, false, transport.Fail(http.StatusUnprocessableEntity, "INVALID", err.Error())
	}
	if len(in.Messages) == 0 {
		return nil, false, transport.Fail(http.StatusUnprocessableEntity, "INVALID", "messages are required")
	}
	opts, err := run.ParseOptions(in.ForwardedProps)
	if err != nil {
		return nil, false, transport.Fail(http.StatusUnprocessableEntity, "INVALID", err.Error())
	}
	ag, err := a.Store.GetAgent(r.Context(), p.Tenant, ref)
	if err != nil {
		return nil, false, err
	}
	releaseID := ""
	if opts.ReleaseDigest != "" {
		if releaseID, err = a.Store.ReleaseIDByDigest(r.Context(), p.Tenant, ag.ID, opts.ReleaseDigest); err != nil {
			return nil, false, err
		}
	} else if ag.CurrentReleaseID != nil {
		releaseID = *ag.CurrentReleaseID
	} else {
		return nil, false, transport.Fail(http.StatusConflict, "CONFLICT", "agent has no published release")
	}
	env, err := a.Store.GetEnvelope(r.Context(), p.Tenant, releaseID)
	if err != nil {
		return nil, false, err
	}
	doc, err := agent.Open(env)
	if err != nil {
		return nil, false, transport.Fail(http.StatusGone, "GONE", "release is corrupt")
	}
	var cred *credential.Token
	// With caller-executed tools the run never reaches the gateway, so it needs no credential.
	if gw := doc.Effective().Tools.Gateway; opts.ToolExecution == "" && len(doc.Effective().Tools.Allow) > 0 && gw != "" {
		auth, ok := a.Gateways[gw]
		if !ok {
			return nil, false, transport.Fail(http.StatusConflict, "CONFLICT", "gateway "+gw+" is not configured on this host")
		}
		if auth.Exchanger != nil {
			tok, _ := identity.Bearer(r)
			if cred, err = a.delegate(r.Context(), auth, tok); err != nil {
				a.Log.Warn("token exchange failed", "gateway", gw, "subject", p.Subject, "error", err)
				return nil, false, transport.Fail(http.StatusForbidden, "DELEGATION_REFUSED", "the authorization server refused to delegate the caller to this host")
			}
		}
	}
	input := run.Input{Tools: in.Tools, Context: in.Context, ForwardedProps: in.ForwardedProps, ToolExecution: opts.ToolExecution, MaxOutputTokens: opts.MaxOutputTokens}
	if input.Tools == nil {
		input.Tools = []agui.Tool{}
	}
	if input.Context == nil {
		input.Context = []agui.Context{}
	}
	out, replayed, err := a.Store.CreateRun(r.Context(), p.Tenant, postgres.CreateRunInput{ThreadID: in.ThreadID, RunID: in.RunID, AgentRef: ag.ID, Owner: p.Subject,
		IdempotencyKey: key, Messages: in.Messages, Input: input, Credential: cred, TraceID: observability.TraceID(r.Context()),
		ReleaseID: releaseID, ExecutionRef: opts.ExecutionRef})
	return out, replayed, err
}

// delegate exchanges the caller's token; the HOST's own actor token is cached until it is close to expiry.
func (a *API) delegate(ctx context.Context, auth GatewayAuth, subjectToken string) (*credential.Token, error) {
	actor := ""
	if auth.ActorToken {
		a.actorMu <- struct{}{}
		if a.actorToken == nil || time.Until(a.actorToken.ExpiresAt) < 30*time.Second {
			tok, err := auth.Exchanger.ClientCredentials(ctx)
			if err != nil {
				<-a.actorMu
				return nil, err
			}
			a.actorToken = tok
		}
		actor = a.actorToken.AccessToken
		<-a.actorMu
	}
	return auth.Exchanger.Exchange(ctx, subjectToken, actor)
}

// stream serves a run's events as Server-Sent Events from the persisted log, until a RUN_FINISHED
// or RUN_ERROR is delivered or the client leaves.
func (a *API) stream(w http.ResponseWriter, r *http.Request, tenant, owner, runID string, after int64) {
	// authorization before the stream opens
	if _, err := a.Store.GetRun(r.Context(), tenant, owner, runID); err != nil {
		a.writeError(w, r, err)
		return
	}
	sse := transport.NewSSEWriter(w)
	if sse == nil {
		transport.WriteProblem(w, transport.Fail(http.StatusInternalServerError, "INTERNAL", "streaming unsupported"))
		return
	}
	ctx := r.Context()
	keep := time.NewTicker(a.StreamKeepAlive)
	defer keep.Stop()
	poll := time.NewTicker(a.StreamPoll)
	defer poll.Stop()
	for {
		events, err := a.Store.Events(ctx, tenant, owner, runID, after, 500)
		if err != nil {
			_ = sse.Send("", "error", []byte(`{"error":"stream aborted"}`))
			return
		}
		for _, e := range events {
			b, _ := json.Marshal(e.Payload)
			if err := sse.Send(strconv.FormatInt(e.ID, 10), "", b); err != nil {
				return
			}
			after = e.ID
			if e.Type == agui.EventRunFinished || e.Type == agui.EventRunError {
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
