package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-host/internal/agent"
	"idp-host/internal/canonical"
)

// Agent is an agent record with its editable definition.
type Agent struct {
	ID               string           `json:"agentId"`
	Name             string           `json:"name"`
	DisplayName      string           `json:"displayName"`
	Definition       agent.Definition `json:"definition"`
	CurrentReleaseID *string          `json:"currentReleaseId"`
	CreatedBy        string           `json:"createdBy"`
	CreatedAt        time.Time        `json:"createdAt"`
	UpdatedAt        time.Time        `json:"updatedAt"`
}

// Release lifecycle.
const (
	ReleaseCandidate  = "CANDIDATE"
	ReleaseApproved   = "APPROVED"
	ReleaseRejected   = "REJECTED"
	ReleaseSuperseded = "SUPERSEDED"
)

// Release is a compiled agent release.
type Release struct {
	ID         string          `json:"releaseId"`
	AgentID    string          `json:"agentId"`
	Version    int             `json:"version"`
	Status     string          `json:"status"`
	Digest     *string         `json:"digest"`
	Document   json.RawMessage `json:"document"`
	CreatedBy  string          `json:"createdBy"`
	ApprovedBy *string         `json:"approvedBy"`
	CreatedAt  time.Time       `json:"createdAt"`
	ApprovedAt *time.Time      `json:"approvedAt"`
}

const agentCols = `id::text, name, display_name, definition, current_release_id::text, created_by, created_at, updated_at`

func scanAgent(row pgx.Row) (*Agent, error) {
	var a Agent
	var def []byte
	if err := row.Scan(&a.ID, &a.Name, &a.DisplayName, &def, &a.CurrentReleaseID, &a.CreatedBy, &a.CreatedAt, &a.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fail("NOT_FOUND", "agent not found")
		}
		return nil, err
	}
	if err := json.Unmarshal(def, &a.Definition); err != nil {
		return nil, err
	}
	return &a, nil
}

func agentWhere(ref string) (string, any) {
	if IsUUID(ref) {
		return `id = $1`, ref
	}
	return `name = $1`, ref
}

func getAgent(ctx context.Context, tx pgx.Tx, ref string, lock bool) (*Agent, error) {
	where, arg := agentWhere(ref)
	q := `SELECT ` + agentCols + ` FROM agents WHERE ` + where
	if lock {
		q += ` FOR UPDATE`
	}
	return scanAgent(tx.QueryRow(ctx, q, arg))
}

// CreateAgent creates an agent with an initial (validated) definition.
func (s *Store) CreateAgent(ctx context.Context, tenant, name, displayName string, def agent.Definition, by string) (*Agent, error) {
	if !agent.ValidName(name) {
		return nil, fail("INVALID", "agent name must match ^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$")
	}
	if strings.TrimSpace(displayName) == "" || len(displayName) > 200 {
		return nil, fail("INVALID", "displayName is required (at most 200 characters)")
	}
	def.Normalize()
	if err := def.Validate(); err != nil {
		return nil, fail("INVALID", "%s", err)
	}
	raw, _ := json.Marshal(def)
	var out *Agent
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		id := NewUUID()
		if _, err := tx.Exec(ctx, `INSERT INTO agents (id, tenant_id, name, display_name, definition, created_by) VALUES ($1, $2, $3, $4, $5, $6)`,
			id, tenant, name, displayName, raw, by); err != nil {
			if isUniqueViolation(err) {
				return fail("CONFLICT", "an agent named %s exists", name)
			}
			return err
		}
		if err := Audit(ctx, tx, tenant, by, "agent.create", id, map[string]any{"name": name}); err != nil {
			return err
		}
		var err error
		out, err = getAgent(ctx, tx, id, false)
		return err
	})
	return out, err
}

// UpdateAgent replaces the display name and/or the draft definition.
func (s *Store) UpdateAgent(ctx context.Context, tenant, ref string, displayName *string, def *agent.Definition, by string) (*Agent, error) {
	if def != nil {
		def.Normalize()
		if err := def.Validate(); err != nil {
			return nil, fail("INVALID", "%s", err)
		}
	}
	if displayName != nil && (strings.TrimSpace(*displayName) == "" || len(*displayName) > 200) {
		return nil, fail("INVALID", "displayName is required (at most 200 characters)")
	}
	var out *Agent
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		a, err := getAgent(ctx, tx, ref, true)
		if err != nil {
			return err
		}
		if displayName != nil {
			a.DisplayName = *displayName
		}
		if def != nil {
			a.Definition = *def
		}
		raw, _ := json.Marshal(a.Definition)
		if _, err := tx.Exec(ctx, `UPDATE agents SET display_name = $2, definition = $3, updated_at = now() WHERE id = $1`, a.ID, a.DisplayName, raw); err != nil {
			return err
		}
		if err := Audit(ctx, tx, tenant, by, "agent.update", a.ID, nil); err != nil {
			return err
		}
		out, err = getAgent(ctx, tx, a.ID, false)
		return err
	})
	return out, err
}

// GetAgent returns an agent by id or name.
func (s *Store) GetAgent(ctx context.Context, tenant, ref string) (*Agent, error) {
	var out *Agent
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
		out, err = getAgent(ctx, tx, ref, false)
		return err
	})
	return out, err
}

// ListAgents returns the tenant's agents by name.
func (s *Store) ListAgents(ctx context.Context, tenant string) ([]Agent, error) {
	out := []Agent{}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+agentCols+` FROM agents ORDER BY name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			a, err := scanAgent(rows)
			if err != nil {
				return err
			}
			out = append(out, *a)
		}
		return rows.Err()
	})
	return out, err
}

const releaseCols = `id::text, agent_id::text, version, status, digest, document, created_by, approved_by, created_at, approved_at`

func scanRelease(row pgx.Row) (*Release, error) {
	var r Release
	var doc string
	if err := row.Scan(&r.ID, &r.AgentID, &r.Version, &r.Status, &r.Digest, &doc, &r.CreatedBy, &r.ApprovedBy, &r.CreatedAt, &r.ApprovedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fail("NOT_FOUND", "release not found")
		}
		return nil, err
	}
	r.Document = json.RawMessage(doc)
	return &r, nil
}

// CreateRelease compiles the agent's draft definition into a candidate release.
func (s *Store) CreateRelease(ctx context.Context, tenant, ref, by string) (*Release, error) {
	var out *Release
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		a, err := getAgent(ctx, tx, ref, true)
		if err != nil {
			return err
		}
		var version int
		if err := tx.QueryRow(ctx, `SELECT coalesce(max(version), 0) + 1 FROM agent_releases WHERE agent_id = $1`, a.ID).Scan(&version); err != nil {
			return err
		}
		id := NewUUID()
		doc := &agent.Document{SchemaVersion: agent.SchemaVersion, ReleaseID: id, TenantID: tenant, AgentID: a.ID, AgentName: a.Name, Version: version,
			CreatedAt: s.now().UTC().Format(time.RFC3339), CreatedBy: by, Definition: a.Definition}
		doc.Definition.Normalize()
		if err := doc.Validate(); err != nil {
			return fail("INVALID", "%s", err)
		}
		raw, err := canonical.Canonical(doc)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO agent_releases (id, tenant_id, agent_id, version, status, document, created_by) VALUES ($1, $2, $3, $4, 'CANDIDATE', $5, $6)`,
			id, tenant, a.ID, version, string(raw), by); err != nil {
			return err
		}
		if err := Audit(ctx, tx, tenant, by, "release.create", id, map[string]any{"agent": a.Name, "version": version}); err != nil {
			return err
		}
		out, err = scanRelease(tx.QueryRow(ctx, `SELECT `+releaseCols+` FROM agent_releases WHERE id = $1`, id))
		return err
	})
	return out, err
}

// ApproveRelease seals a candidate with the approver's name (four eyes: never the creator) and makes
// it the agent's current release.
func (s *Store) ApproveRelease(ctx context.Context, tenant, releaseID, by string) (*Release, error) {
	var out *Release
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		r, err := scanRelease(tx.QueryRow(ctx, `SELECT `+releaseCols+` FROM agent_releases WHERE id = $1 FOR UPDATE`, releaseID))
		if err != nil {
			return err
		}
		if r.Status != ReleaseCandidate {
			return fail("CONFLICT", "release is %s, not a candidate", r.Status)
		}
		if r.CreatedBy == by {
			return fail("FORBIDDEN", "the creator of a release cannot approve it")
		}
		var doc agent.Document
		dec := json.NewDecoder(strings.NewReader(string(r.Document)))
		dec.UseNumber()
		if err := dec.Decode(&doc); err != nil {
			return err
		}
		doc.ApprovedBy = by
		env, err := agent.Seal(&doc)
		if err != nil {
			return fail("INVALID", "%s", err)
		}
		envRaw, _ := json.Marshal(env)
		if _, err := tx.Exec(ctx, `UPDATE agent_releases SET status = 'APPROVED', digest = $2, envelope = $3, document = $4, approved_by = $5, approved_at = now() WHERE id = $1`,
			r.ID, env.Digest, string(envRaw), string(env.Document), by); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE agents SET current_release_id = $2, updated_at = now() WHERE id = $1`, r.AgentID, r.ID); err != nil {
			return err
		}
		if err := Audit(ctx, tx, tenant, by, "release.approve", r.ID, map[string]any{"digest": env.Digest, "version": r.Version}); err != nil {
			return err
		}
		out, err = scanRelease(tx.QueryRow(ctx, `SELECT `+releaseCols+` FROM agent_releases WHERE id = $1`, r.ID))
		return err
	})
	return out, err
}

// RejectRelease refuses a candidate.
func (s *Store) RejectRelease(ctx context.Context, tenant, releaseID, by, reason string) (*Release, error) {
	var out *Release
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		r, err := scanRelease(tx.QueryRow(ctx, `SELECT `+releaseCols+` FROM agent_releases WHERE id = $1 FOR UPDATE`, releaseID))
		if err != nil {
			return err
		}
		if r.Status != ReleaseCandidate {
			return fail("CONFLICT", "release is %s, not a candidate", r.Status)
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_releases SET status = 'REJECTED', approved_by = $2, approved_at = now() WHERE id = $1`, r.ID, by); err != nil {
			return err
		}
		if err := Audit(ctx, tx, tenant, by, "release.reject", r.ID, map[string]any{"reason": reason}); err != nil {
			return err
		}
		out, err = scanRelease(tx.QueryRow(ctx, `SELECT `+releaseCols+` FROM agent_releases WHERE id = $1`, r.ID))
		return err
	})
	return out, err
}

// SetCurrentRelease points the agent at an approved release (rollback or roll-forward).
func (s *Store) SetCurrentRelease(ctx context.Context, tenant, ref, releaseID, by string) (*Agent, error) {
	var out *Agent
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		a, err := getAgent(ctx, tx, ref, true)
		if err != nil {
			return err
		}
		r, err := scanRelease(tx.QueryRow(ctx, `SELECT `+releaseCols+` FROM agent_releases WHERE id = $1 AND agent_id = $2`, releaseID, a.ID))
		if err != nil {
			return err
		}
		if r.Status != ReleaseApproved {
			return fail("CONFLICT", "release is %s, not approved", r.Status)
		}
		if _, err := tx.Exec(ctx, `UPDATE agents SET current_release_id = $2, updated_at = now() WHERE id = $1`, a.ID, r.ID); err != nil {
			return err
		}
		if err := Audit(ctx, tx, tenant, by, "release.set-current", r.ID, map[string]any{"agent": a.Name, "version": r.Version}); err != nil {
			return err
		}
		out, err = getAgent(ctx, tx, a.ID, false)
		return err
	})
	return out, err
}

// ListReleases returns an agent's releases, newest first.
func (s *Store) ListReleases(ctx context.Context, tenant, ref string) ([]Release, error) {
	out := []Release{}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		a, err := getAgent(ctx, tx, ref, false)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+releaseCols+` FROM agent_releases WHERE agent_id = $1 ORDER BY version DESC`, a.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRelease(rows)
			if err != nil {
				return err
			}
			out = append(out, *r)
		}
		return rows.Err()
	})
	return out, err
}

// GetRelease returns one release.
func (s *Store) GetRelease(ctx context.Context, tenant, releaseID string) (*Release, error) {
	var out *Release
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
		out, err = scanRelease(tx.QueryRow(ctx, `SELECT `+releaseCols+` FROM agent_releases WHERE id = $1`, releaseID))
		return err
	})
	return out, err
}

// GetEnvelope returns the sealed envelope of an approved release.
func (s *Store) GetEnvelope(ctx context.Context, tenant, releaseID string) (*agent.Envelope, error) {
	var out *agent.Envelope
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
		out, err = envelopeOf(ctx, tx, releaseID)
		return err
	})
	return out, err
}

func envelopeOf(ctx context.Context, tx pgx.Tx, releaseID string) (*agent.Envelope, error) {
	var env *string
	if err := tx.QueryRow(ctx, `SELECT envelope FROM agent_releases WHERE id = $1 AND status = 'APPROVED'`, releaseID).Scan(&env); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fail("NOT_FOUND", "approved release not found")
		}
		return nil, err
	}
	if env == nil {
		return nil, fail("NOT_FOUND", "release has no envelope")
	}
	var e agent.Envelope
	if err := json.Unmarshal([]byte(*env), &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// currentRelease resolves an agent's current release and opens its document (digest verified).
func currentRelease(ctx context.Context, tx pgx.Tx, a *Agent) (string, *agent.Envelope, *agent.Document, error) {
	if a.CurrentReleaseID == nil {
		return "", nil, nil, fail("CONFLICT", "agent %s has no published release", a.Name)
	}
	env, err := envelopeOf(ctx, tx, *a.CurrentReleaseID)
	if err != nil {
		return "", nil, nil, err
	}
	doc, err := agent.Open(env)
	if err != nil {
		return "", nil, nil, fail("GONE", "release %s is corrupt: %s", *a.CurrentReleaseID, err)
	}
	return *a.CurrentReleaseID, env, doc, nil
}
