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
	"idp-host/internal/cmiclient"
	"idp-host/internal/credential"
	"idp-host/internal/mcpclient"
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
	// Endpoint, Client and StaticBearer let the HOST read the gateway's tool catalogue when a run is created, to see which
	// tools the gateway marks as sending content outside the enterprise (D51). Without a Client the catalogue cannot be
	// read, and a release that has tools of this gateway counts as outward unless the operator's patterns already say so.
	Endpoint     string
	Client       *mcpclient.Client
	StaticBearer string
}

// API serves the HOST.
type API struct {
	Store     *postgres.Store
	Auth      Verifier
	Gateways  map[string]GatewayAuth // by gateway name as the release refers to it
	Providers []string
	// Files decides which attachments a run gets when its release may use an external model (D43).
	Files FilePolicy
	// CMI resolves context packages for runs whose release asks for them (D32); nil disables.
	CMI     *cmiclient.Client
	CMIAuth GatewayAuth // how the HOST obtains the caller's CMI credential; APIKey when static
	CMIKey  string
	// CMIToken is the HOST's own credential for the CMI, minted and refreshed by itself (D36); set
	// instead of CMIKey, which is a token that stops working when it expires.
	CMIToken func(context.Context) (string, error)
	// Rewriter makes the managed model call that rewrites the first question of a thread with earlier turns (D66); nil: no
	// rewrite is made whatever the release says. The harness implements it.
	Rewriter cmiclient.Rewriter
	// MemberRoles are the roles of the people who use the HOST as themselves (IDP_HOST_MEMBER_ROLES). Only a principal that holds one
	// of them gets a member credential for the CMI kept with its run and the knowledge tools (D68); with none configured nobody does,
	// so that a service account that can read every personal base never reaches it through these tools.
	MemberRoles []string
	Log         *slog.Logger
	Metrics     *observability.Metrics
	Version     string

	// StreamPoll is how often the event stream polls the store (default 150 ms).
	StreamPoll time.Duration
	// StreamKeepAlive is the keep-alive comment interval (default 15 s).
	StreamKeepAlive time.Duration
	// CatalogueTimeout bounds each tools/list a run's creation makes (D51); default 8 s.
	CatalogueTimeout time.Duration

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
		status := map[string]int{"NOT_FOUND": 404, "CONFLICT": 409, "FORBIDDEN": 403, "INVALID": 422, "GONE": 410, "QUOTA_EXCEEDED": 429, "EVALUATION_REQUIRED": 409}[se.Code]
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
	mux.Handle("POST /api/v1/releases/{id}/evaluate", a.role(admin, func(r *http.Request, p authorization.Principal) (int, any, error) {
		// ADR-0001 D26: the evaluation runs are ordinary runs of the candidate release, owned by the
		// evaluator; they need the same gateway credential a run of theirs would
		rel, err := a.Store.GetRelease(r.Context(), p.Tenant, r.PathValue("id"))
		if err != nil {
			return 0, nil, err
		}
		var doc agent.Document
		if err := json.Unmarshal(rel.Document, &doc); err != nil {
			return 0, nil, transport.Fail(http.StatusGone, "GONE", "release document is corrupt")
		}
		var cred *credential.Token
		if gw := doc.Effective().Tools.Gateway; len(doc.Effective().Tools.Allow) > 0 && gw != "" {
			auth, ok := a.Gateways[gw]
			if !ok {
				return 0, nil, transport.Fail(http.StatusConflict, "CONFLICT", "gateway "+gw+" is not configured on this host")
			}
			if auth.Exchanger != nil {
				tok, _ := identity.Bearer(r)
				if cred, err = a.delegate(r.Context(), auth, tok); err != nil {
					return 0, nil, transport.Fail(http.StatusForbidden, "DELEGATION_REFUSED", "the authorization server refused to delegate the caller to this host")
				}
			}
		}
		out, err := a.Store.CreateEvaluation(r.Context(), p.Tenant, rel.ID, p.Subject, cred)
		return http.StatusAccepted, out, err
	}))
	mux.Handle("GET /api/v1/releases/{id}/evaluations", a.role(admin, func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.ListEvaluations(r.Context(), p.Tenant, r.PathValue("id"))
		return 200, map[string]any{"evaluations": out}, err
	}))
	mux.Handle("GET /api/v1/evaluations/{id}", a.role(admin, func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.GetEvaluation(r.Context(), p.Tenant, r.PathValue("id"))
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
			key = "agui-" + strings.ReplaceAll(newUUID(), "-", "")
		}
		out, _, err := a.createRun(r, p, r.PathValue("ref"), key, in)
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		w.Header().Set("X-Run-Id", out.ID)
		a.stream(w, r, p.Tenant, ownerScope(p), out.ID, 0)
	}))
	a.a2aRoutes(mux)
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
	// D43: a file classified above the tenant's ceiling does not go to a run whose release may use an external model.
	// Its ticket is dropped here, before anything is stored: the run cannot fetch it, and the model is told only its name.
	var withheld []run.Attachment
	ceiling := ""
	files := a.Files
	if files.Release == nil && a.Store != nil {
		files.Release = a.Store.GetReleaseByDigest
	}
	// D51: which tools the gateway marks external is known only from its tools/list, so it is read here, with the caller's own
	// credential, when the release has tools of a gateway the run reaches; a run whose tools the caller executes has none
	var look Catalogue
	if opts.ToolExecution == "" {
		look = a.catalogue(r.Context(), doc.Effective().Tools.Gateway, cred)
	}
	outw := files.outward(r.Context(), p.Tenant, doc, look)
	opts.Attachments, withheld, ceiling = files.screen(p.Tenant, outw, opts.Attachments)
	// D44: the same trigger (a model outside the enterprise, or a tool that sends content out) screens the CMI context
	// entries; the ceiling is recorded in the run's input so that a refresh applies the rule the run was created under.
	trigger := outw.Trigger
	tickets := opts.Tickets()
	if tickets != nil {
		// D37: a member's files go to the sandbox through tool calls the HOST executes, with tickets it keeps sealed
		if opts.ToolExecution != "" {
			return nil, false, transport.Fail(http.StatusUnprocessableEntity, "INVALID", "file attachments and delivery need tools executed by the HOST (toolExecution server)")
		}
		if a.Store.Box == nil {
			return nil, false, transport.Fail(http.StatusConflict, "CONFLICT", "this host keeps no file tickets (IDP_HOST_CREDENTIAL_KEY is not set)")
		}
	}
	input := run.Input{Tools: in.Tools, Context: in.Context, ForwardedProps: run.WithoutFileOptions(in.ForwardedProps), ToolExecution: opts.ToolExecution, MaxOutputTokens: opts.MaxOutputTokens,
		Delivery: opts.Delivery != nil, ExternalTools: outw.Tools}
	if trigger != "" {
		input.ContextCeiling, input.ContextTrigger, input.ContextVia = ceiling, trigger, outw.Via
	}
	for _, at := range opts.Attachments {
		at.Ticket = ""
		input.Attachments = append(input.Attachments, at)
	}
	if input.Tools == nil {
		input.Tools = []agui.Tool{}
	}
	if input.Context == nil {
		input.Context = []agui.Context{}
	}
	if len(withheld) > 0 {
		input.Context = append(input.Context, agui.Context{Description: ContextAttachmentsWithheld, Value: withheldNote(withheld, ceiling)})
	}
	// D68: who may have the knowledge tools: a member (a principal with one of MemberRoles), not an A2A peer, a run whose tools the HOST executes
	pol := doc.Definition.Context
	subTools := files.subagentsHaveKnowledgeTools(r.Context(), p.Tenant, doc)
	knowledge := pol.HasKnowledgeTools() || subTools
	keepCred := knowledge && opts.ToolExecution == "" && a.isMember(p) && !isA2A(r.Context())
	if keepCred && a.CMIAuth.Exchanger != nil && a.Store.Box == nil {
		return nil, false, transport.Fail(http.StatusConflict, "CONFLICT", "this host keeps no member credential for the knowledge tools (IDP_HOST_CREDENTIAL_KEY is not set)")
	}
	// an idempotent replay returns the stored run before anything is paid for: no rewrite, no retrieval, no exchange (D66)
	if replay, err := a.Store.FindReplay(r.Context(), p.Tenant, postgres.CreateRunInput{ThreadID: in.ThreadID, AgentRef: ag.ID, Owner: p.Subject, IdempotencyKey: key, Messages: in.Messages,
		Input: input, ReleaseID: releaseID, ExecutionRef: opts.ExecutionRef}); err != nil {
		return nil, false, err
	} else if replay != nil {
		return replay, true, nil
	}
	// the member's CMI credential: one exchange serves the first retrieval and, kept, the tools (D68); a failed exchange is the
	// retrieval's reason to skip, and the run then has no credential
	var cmiCred *credential.Token
	var credErr error
	if a.CMI != nil && a.CMIAuth.Exchanger != nil && (pol.CMI || keepCred) {
		tok, _ := identity.Bearer(r)
		cmiCred, credErr = a.delegate(r.Context(), a.CMIAuth, tok)
	}
	// D32: context from the CMI, resolved for the caller before the run is queued
	packageID, extra, rewrote, err := a.resolveContext(r, p, doc, in, &input, cmiCred, credErr, knowledge)
	if err != nil {
		return nil, false, err
	}
	if !keepCred || credErr != nil {
		cmiCred = nil
	}
	if len(withheld) > 0 {
		a.Log.Info("attachments withheld from a run that may send content outside the enterprise", "tenant", p.Tenant, "agent", ag.ID, "files", len(withheld), "ceiling", ceiling, "external", outw.Models, "trigger", trigger, "via", outw.Via)
		extra = append(extra, withheldEvents(withheld, ceiling, outw)...)
	}
	out, replayed, err := a.Store.CreateRun(r.Context(), p.Tenant, postgres.CreateRunInput{ThreadID: in.ThreadID, RunID: in.RunID, AgentRef: ag.ID, Owner: p.Subject,
		IdempotencyKey: key, Messages: in.Messages, Input: input, Credential: cred, FileTickets: tickets, TraceID: observability.TraceID(r.Context()), Traceparent: observability.Traceparent(r.Context()),
		ReleaseID: releaseID, ExecutionRef: opts.ExecutionRef, ContextPackageID: packageID, Extra: extra, CMICredential: cmiCred, RewriteUsage: rewrote})
	return out, replayed, err
}

// catalogue returns the lookup of the gateways' catalogues for one run creation (D51). Each gateway is asked at most once
// per creation, whatever the number of releases that use it. The caller's delegated credential (cred) is the run's own
// gateway's; any other gateway can be asked only with a static credential, since a sub-agent run is handed no credential
// for another gateway either. Any failure, a refusal of the credential included, is returned as an error: the caller then
// counts the release as outward. The credential is not renewed here: a renewal that rotates the refresh token would
// invalidate the one the run stores.
func (a *API) catalogue(ctx context.Context, runGateway string, cred *credential.Token) Catalogue {
	type answer struct {
		tools map[string]bool
		err   error
	}
	asked := map[string]answer{}
	timeout := a.CatalogueTimeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	return func(gateway string) (map[string]bool, error) {
		if got, ok := asked[gateway]; ok {
			return got.tools, got.err
		}
		tools, err := a.listGateway(ctx, gateway, runGateway, cred, timeout)
		asked[gateway] = answer{tools, err}
		return tools, err
	}
}

func (a *API) listGateway(ctx context.Context, gateway, runGateway string, cred *credential.Token, timeout time.Duration) (map[string]bool, error) {
	auth, ok := a.Gateways[gateway]
	if !ok || auth.Client == nil || auth.Endpoint == "" {
		return nil, errors.New("the HOST cannot read this gateway's catalogue")
	}
	bearer := auth.StaticBearer
	if bearer == "" && gateway == runGateway && cred != nil {
		bearer = cred.AccessToken
	}
	if bearer == "" {
		return nil, errors.New("no credential for this gateway")
	}
	lctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	listed, err := auth.Client.ListTools(lctx, auth.Endpoint, bearer)
	if err != nil {
		a.Log.Warn("a gateway's tool catalogue could not be read at run creation; its tools count as sending content out", "gateway", gateway, "error", err)
		return nil, err
	}
	out := make(map[string]bool, len(listed))
	for _, t := range listed {
		out[t.Name] = t.External
	}
	return out, nil
}

// resolveContext asks the CMI for a package when the release wants one (D32) and appends its items
// to the run's context. The caller's own context comes first; CMI items follow it. A CMI failure
// skips the context (recorded on the run's stream) or refuses the run, as the release decides. cred is the member's
// credential the caller's token was exchanged for (credErr why it was not); the HOST's own credential serves only when the CMI is not
// reached by delegation. numbered says whether the run (or a sub-agent of it) has the knowledge tools: only then are the first
// package's knowledge items numbered (D67), and a release without them sees the text it saw before KB8. It returns the usage of the
// question rewrite, when one was made, for the run to be charged for (D66).
func (a *API) resolveContext(r *http.Request, p authorization.Principal, doc *agent.Document, in agui.RunAgentInput, input *run.Input, cred *credential.Token, credErr error, numbered bool) (string, []agui.Event, *postgres.RewriteUsage, error) {
	pol := doc.Definition.Context
	if !pol.CMI {
		return "", nil, nil, nil
	}
	var events []agui.Event // what the stream records of this resolve: the rewrite, then the package (or why there is none)
	var usage *postgres.RewriteUsage
	skip := func(reason string, err error) (string, []agui.Event, *postgres.RewriteUsage, error) {
		if pol.OnError == "fail" {
			return "", nil, nil, transport.Fail(http.StatusBadGateway, "CONTEXT_UNAVAILABLE", "the context infrastructure is unavailable: "+reason)
		}
		a.Log.Warn("CMI context skipped", "reason", reason, "error", err)
		return "", append(events, agui.Custom(agui.CustomContextSkipped, map[string]any{"reason": reason})), usage, nil
	}
	if a.CMI == nil {
		return skip("no CMI configured on this host", nil)
	}
	query := ""
	for i := len(in.Messages) - 1; i >= 0; i-- {
		if in.Messages[i].Role == agui.RoleUser && in.Messages[i].Content != "" {
			query = in.Messages[i].Content
			break
		}
	}
	if query == "" {
		return skip("no user message to resolve context for", nil)
	}
	query = cmiclient.CutBytes(query, 4096)
	bearer := a.CMIKey
	if a.CMIToken != nil {
		tok, err := a.CMIToken(r.Context())
		if err != nil {
			return skip("the HOST's own CMI credential could not be obtained", err)
		}
		bearer = tok
	}
	if a.CMIAuth.Exchanger != nil {
		if credErr != nil || cred == nil {
			return skip("delegation to the CMI refused", credErr)
		}
		bearer = cred.AccessToken
	}
	// D66: the question of a thread that has earlier turns is rewritten before the first retrieval, when the release lets the
	// HOST. The managed model call falls back to the original question; the event says what the query was and why. The tenant's
	// budget is asked first: a tenant that could not create the run is not charged a model call.
	if pol.RewritesQuestion() && a.Rewriter != nil {
		if pi, ok := cmiclient.PriorTurns(a.rewriteHistory(r.Context(), p, in)); ok {
			if err := a.Store.CheckQuota(r.Context(), p.Tenant); err != nil {
				return "", nil, nil, err
			}
			if policy, err := a.Store.GetPolicy(r.Context(), p.Tenant); err == nil {
				pi.BlockedPatterns = policy.BlockedPatterns // D28
			}
			out := a.Rewriter.RewriteQuestion(r.Context(), doc.Effective(), pi)
			if out.Query != query {
				input.ContextQuery = out.Query // a refresh asks the same question again
			}
			query = out.Query
			events = append(events, out.Event)
			if u := out.Usage; u != nil {
				usage = &postgres.RewriteUsage{Provider: u.Provider, Model: u.Model, PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, LatencyMs: u.LatencyMs}
			}
		}
	}
	req := cmiclient.Request{Query: query, Purpose: pol.Purpose, Kinds: pol.Kinds, Sources: pol.Sources}
	if in.ThreadID != "" {
		req.Scope = &struct {
			Kind string `json:"kind"`
			ID   string `json:"id,omitempty"`
		}{Kind: "thread", ID: in.ThreadID}
	}
	if pol.MaxTokens > 0 || pol.MaxItems > 0 {
		req.Budget = &struct {
			MaxTokens int `json:"maxTokens,omitempty"`
			MaxItems  int `json:"maxItems,omitempty"`
		}{MaxTokens: pol.MaxTokens, MaxItems: pol.MaxItems}
	}
	pkg, err := a.CMI.Resolve(r.Context(), bearer, req)
	if err != nil {
		return skip("resolve failed", err)
	}
	// D44: a run that may send content outside the enterprise is not given entries above the ceiling (or without a valid
	// level); the model is told how many. "items" stays what the CMI returned; "appended" is how many entries ended up at
	// the tail of the context, which the store needs to compare a replayed request without them.
	// D67: with the knowledge tools in reach, the knowledge items are numbered from 1 and the tools of the run go on from where this
	// stops; without them nothing is numbered
	first := 0
	if numbered {
		first = 1
	}
	entries, next := cmiclient.ContextFrom(pkg.Items, first)
	var withheld *cmiclient.Screening
	if input.ContextCeiling != "" {
		s := cmiclient.Screen(pkg.Items, input.ContextCeiling)
		entries, next = s.ContextFrom(first)
		withheld = &s
	}
	input.Context = append(input.Context, entries...)
	resolved := map[string]any{"packageId": pkg.PackageID, "digest": pkg.Digest, "items": len(pkg.Items), "appended": len(entries), "tokens": pkg.Tokens, "query": query}
	if numbered {
		resolved["firstIndex"], resolved["numbered"] = 1, next-1
	}
	events = append(events, agui.Custom(agui.CustomContextResolved, resolved))
	if withheld != nil && withheld.Withheld > 0 {
		a.Log.Info("CMI context entries withheld from a run that may send content outside the enterprise", "tenant", p.Tenant, "count", withheld.Withheld, "ceiling", input.ContextCeiling, "trigger", input.ContextTrigger, "via", input.ContextVia)
		events = append(events, withheld.Event(input.ContextTrigger, input.ContextVia))
	}
	return pkg.PackageID, events, usage, nil
}

// rewriteHistory is the conversation the question rewrite reads (D66): what the thread already holds, then the messages of this
// request that are not on it. A client that sends the whole conversation each time and one that sends only the new message (an A2A
// follow-up on a contextId) get the same earlier turns.
func (a *API) rewriteHistory(ctx context.Context, p authorization.Principal, in agui.RunAgentInput) []agui.Message {
	stored, err := a.Store.ThreadMessages(ctx, p.Tenant, p.Subject, in.ThreadID)
	if err != nil || len(stored) == 0 {
		return in.Messages
	}
	known := map[string]bool{}
	for _, m := range stored {
		known[m.ID] = true
	}
	out := append([]agui.Message(nil), stored...)
	for _, m := range in.Messages {
		if !known[m.ID] {
			out = append(out, m)
		}
	}
	return out
}

// isMember reports whether the principal is one of the people who use the HOST as themselves (MemberRoles). With no member roles
// configured nobody is.
func (a *API) isMember(p authorization.Principal) bool {
	for _, r := range a.MemberRoles {
		if p.Has(r) {
			return true
		}
	}
	return false
}

type a2aKey struct{}

// withA2A marks the context of a request that came in through the A2A endpoint: the caller is a peer agent, not a member.
func withA2A(ctx context.Context) context.Context { return context.WithValue(ctx, a2aKey{}, true) }

func isA2A(ctx context.Context) bool { v, _ := ctx.Value(a2aKey{}).(bool); return v }

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

func newUUID() string { return postgres.NewUUID() }
