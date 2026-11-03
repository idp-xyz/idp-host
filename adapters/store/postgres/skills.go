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

// Skill is a skill record with its editable definition (ADR-0001 D17).
type Skill struct {
	ID               string                `json:"skillId"`
	Name             string                `json:"name"`
	DisplayName      string                `json:"displayName"`
	Definition       agent.SkillDefinition `json:"definition"`
	CurrentReleaseID *string               `json:"currentReleaseId"`
	CreatedBy        string                `json:"createdBy"`
	CreatedAt        time.Time             `json:"createdAt"`
	UpdatedAt        time.Time             `json:"updatedAt"`
}

// SkillRelease is a compiled skill release.
type SkillRelease struct {
	ID         string          `json:"releaseId"`
	SkillID    string          `json:"skillId"`
	Version    int             `json:"version"`
	Status     string          `json:"status"`
	Digest     *string         `json:"digest"`
	Document   json.RawMessage `json:"document"`
	CreatedBy  string          `json:"createdBy"`
	ApprovedBy *string         `json:"approvedBy"`
	CreatedAt  time.Time       `json:"createdAt"`
	ApprovedAt *time.Time      `json:"approvedAt"`
}

const skillCols = `id::text, name, display_name, definition, current_release_id::text, created_by, created_at, updated_at`

func scanSkill(row pgx.Row) (*Skill, error) {
	var s Skill
	var def []byte
	if err := row.Scan(&s.ID, &s.Name, &s.DisplayName, &def, &s.CurrentReleaseID, &s.CreatedBy, &s.CreatedAt, &s.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fail("NOT_FOUND", "skill not found")
		}
		return nil, err
	}
	if err := json.Unmarshal(def, &s.Definition); err != nil {
		return nil, err
	}
	return &s, nil
}

func getSkill(ctx context.Context, tx pgx.Tx, ref string, lock bool) (*Skill, error) {
	where, arg := agentWhere(ref)
	q := `SELECT ` + skillCols + ` FROM skills WHERE ` + where
	if lock {
		q += ` FOR UPDATE`
	}
	return scanSkill(tx.QueryRow(ctx, q, arg))
}

// CreateSkill creates a skill with an initial definition.
func (s *Store) CreateSkill(ctx context.Context, tenant, name, displayName string, def agent.SkillDefinition, by string) (*Skill, error) {
	if !agent.ValidName(name) {
		return nil, fail("INVALID", "skill name must match ^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$")
	}
	if strings.TrimSpace(displayName) == "" || len(displayName) > 200 {
		return nil, fail("INVALID", "displayName is required (at most 200 characters)")
	}
	def.Normalize()
	if err := def.Validate(); err != nil {
		return nil, fail("INVALID", "%s", err)
	}
	raw, _ := json.Marshal(def)
	var out *Skill
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		id := NewUUID()
		if _, err := tx.Exec(ctx, `INSERT INTO skills (id, tenant_id, name, display_name, definition, created_by) VALUES ($1, $2, $3, $4, $5, $6)`,
			id, tenant, name, displayName, raw, by); err != nil {
			if isUniqueViolation(err) {
				return fail("CONFLICT", "a skill named %s exists", name)
			}
			return err
		}
		if err := Audit(ctx, tx, tenant, by, "skill.create", id, map[string]any{"name": name}); err != nil {
			return err
		}
		var err error
		out, err = getSkill(ctx, tx, id, false)
		return err
	})
	return out, err
}

// UpdateSkill replaces the display name and/or the draft definition.
func (s *Store) UpdateSkill(ctx context.Context, tenant, ref string, displayName *string, def *agent.SkillDefinition, by string) (*Skill, error) {
	if def != nil {
		def.Normalize()
		if err := def.Validate(); err != nil {
			return nil, fail("INVALID", "%s", err)
		}
	}
	if displayName != nil && (strings.TrimSpace(*displayName) == "" || len(*displayName) > 200) {
		return nil, fail("INVALID", "displayName is required (at most 200 characters)")
	}
	var out *Skill
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		sk, err := getSkill(ctx, tx, ref, true)
		if err != nil {
			return err
		}
		if displayName != nil {
			sk.DisplayName = *displayName
		}
		if def != nil {
			sk.Definition = *def
		}
		raw, _ := json.Marshal(sk.Definition)
		if _, err := tx.Exec(ctx, `UPDATE skills SET display_name = $2, definition = $3, updated_at = now() WHERE id = $1`, sk.ID, sk.DisplayName, raw); err != nil {
			return err
		}
		if err := Audit(ctx, tx, tenant, by, "skill.update", sk.ID, nil); err != nil {
			return err
		}
		out, err = getSkill(ctx, tx, sk.ID, false)
		return err
	})
	return out, err
}

// GetSkill returns a skill by id or name.
func (s *Store) GetSkill(ctx context.Context, tenant, ref string) (*Skill, error) {
	var out *Skill
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
		out, err = getSkill(ctx, tx, ref, false)
		return err
	})
	return out, err
}

// ListSkills returns the tenant's skills by name.
func (s *Store) ListSkills(ctx context.Context, tenant string) ([]Skill, error) {
	out := []Skill{}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+skillCols+` FROM skills ORDER BY name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			sk, err := scanSkill(rows)
			if err != nil {
				return err
			}
			out = append(out, *sk)
		}
		return rows.Err()
	})
	return out, err
}

const skillReleaseCols = `id::text, skill_id::text, version, status, digest, document, created_by, approved_by, created_at, approved_at`

func scanSkillRelease(row pgx.Row) (*SkillRelease, error) {
	var r SkillRelease
	var doc string
	if err := row.Scan(&r.ID, &r.SkillID, &r.Version, &r.Status, &r.Digest, &doc, &r.CreatedBy, &r.ApprovedBy, &r.CreatedAt, &r.ApprovedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fail("NOT_FOUND", "skill release not found")
		}
		return nil, err
	}
	r.Document = json.RawMessage(doc)
	return &r, nil
}

// CreateSkillRelease compiles the skill's draft definition into a candidate release.
func (s *Store) CreateSkillRelease(ctx context.Context, tenant, ref, by string) (*SkillRelease, error) {
	var out *SkillRelease
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		sk, err := getSkill(ctx, tx, ref, true)
		if err != nil {
			return err
		}
		var version int
		if err := tx.QueryRow(ctx, `SELECT coalesce(max(version), 0) + 1 FROM skill_releases WHERE skill_id = $1`, sk.ID).Scan(&version); err != nil {
			return err
		}
		id := NewUUID()
		doc := &agent.SkillDocument{SchemaVersion: agent.SkillSchemaVersion, ReleaseID: id, TenantID: tenant, SkillID: sk.ID, SkillName: sk.Name, Version: version,
			CreatedAt: s.now().UTC().Format(time.RFC3339), CreatedBy: by, Definition: sk.Definition}
		doc.Definition.Normalize()
		if err := doc.Validate(); err != nil {
			return fail("INVALID", "%s", err)
		}
		raw, err := canonical.Canonical(doc)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO skill_releases (id, tenant_id, skill_id, version, status, document, created_by) VALUES ($1, $2, $3, $4, 'CANDIDATE', $5, $6)`,
			id, tenant, sk.ID, version, string(raw), by); err != nil {
			return err
		}
		if err := Audit(ctx, tx, tenant, by, "skill-release.create", id, map[string]any{"skill": sk.Name, "version": version}); err != nil {
			return err
		}
		out, err = scanSkillRelease(tx.QueryRow(ctx, `SELECT `+skillReleaseCols+` FROM skill_releases WHERE id = $1`, id))
		return err
	})
	return out, err
}

// ApproveSkillRelease seals a candidate (four eyes) and makes it the skill's current release.
func (s *Store) ApproveSkillRelease(ctx context.Context, tenant, releaseID, by string) (*SkillRelease, error) {
	var out *SkillRelease
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		r, err := scanSkillRelease(tx.QueryRow(ctx, `SELECT `+skillReleaseCols+` FROM skill_releases WHERE id = $1 FOR UPDATE`, releaseID))
		if err != nil {
			return err
		}
		if r.Status != ReleaseCandidate {
			return fail("CONFLICT", "skill release is %s, not a candidate", r.Status)
		}
		if r.CreatedBy == by {
			return fail("FORBIDDEN", "the creator of a release cannot approve it")
		}
		var doc agent.SkillDocument
		dec := json.NewDecoder(strings.NewReader(string(r.Document)))
		dec.UseNumber()
		if err := dec.Decode(&doc); err != nil {
			return err
		}
		doc.ApprovedBy = by
		env, err := agent.SealSkill(&doc)
		if err != nil {
			return fail("INVALID", "%s", err)
		}
		envRaw, _ := json.Marshal(env)
		if _, err := tx.Exec(ctx, `UPDATE skill_releases SET status = 'APPROVED', digest = $2, envelope = $3, document = $4, approved_by = $5, approved_at = now() WHERE id = $1`,
			r.ID, env.Digest, string(envRaw), string(env.Document), by); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE skills SET current_release_id = $2, updated_at = now() WHERE id = $1`, r.SkillID, r.ID); err != nil {
			return err
		}
		if err := Audit(ctx, tx, tenant, by, "skill-release.approve", r.ID, map[string]any{"digest": env.Digest, "version": r.Version}); err != nil {
			return err
		}
		out, err = scanSkillRelease(tx.QueryRow(ctx, `SELECT `+skillReleaseCols+` FROM skill_releases WHERE id = $1`, r.ID))
		return err
	})
	return out, err
}

// RejectSkillRelease refuses a candidate.
func (s *Store) RejectSkillRelease(ctx context.Context, tenant, releaseID, by, reason string) (*SkillRelease, error) {
	var out *SkillRelease
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		r, err := scanSkillRelease(tx.QueryRow(ctx, `SELECT `+skillReleaseCols+` FROM skill_releases WHERE id = $1 FOR UPDATE`, releaseID))
		if err != nil {
			return err
		}
		if r.Status != ReleaseCandidate {
			return fail("CONFLICT", "skill release is %s, not a candidate", r.Status)
		}
		if _, err := tx.Exec(ctx, `UPDATE skill_releases SET status = 'REJECTED', approved_by = $2, approved_at = now() WHERE id = $1`, r.ID, by); err != nil {
			return err
		}
		if err := Audit(ctx, tx, tenant, by, "skill-release.reject", r.ID, map[string]any{"reason": reason}); err != nil {
			return err
		}
		out, err = scanSkillRelease(tx.QueryRow(ctx, `SELECT `+skillReleaseCols+` FROM skill_releases WHERE id = $1`, r.ID))
		return err
	})
	return out, err
}

// SetCurrentSkillRelease points the skill at an approved release.
func (s *Store) SetCurrentSkillRelease(ctx context.Context, tenant, ref, releaseID, by string) (*Skill, error) {
	var out *Skill
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		sk, err := getSkill(ctx, tx, ref, true)
		if err != nil {
			return err
		}
		r, err := scanSkillRelease(tx.QueryRow(ctx, `SELECT `+skillReleaseCols+` FROM skill_releases WHERE id = $1 AND skill_id = $2`, releaseID, sk.ID))
		if err != nil {
			return err
		}
		if r.Status != ReleaseApproved {
			return fail("CONFLICT", "skill release is %s, not approved", r.Status)
		}
		if _, err := tx.Exec(ctx, `UPDATE skills SET current_release_id = $2, updated_at = now() WHERE id = $1`, sk.ID, r.ID); err != nil {
			return err
		}
		if err := Audit(ctx, tx, tenant, by, "skill-release.set-current", r.ID, map[string]any{"skill": sk.Name, "version": r.Version}); err != nil {
			return err
		}
		out, err = getSkill(ctx, tx, sk.ID, false)
		return err
	})
	return out, err
}

// ListSkillReleases returns a skill's releases, newest first.
func (s *Store) ListSkillReleases(ctx context.Context, tenant, ref string) ([]SkillRelease, error) {
	out := []SkillRelease{}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		sk, err := getSkill(ctx, tx, ref, false)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+skillReleaseCols+` FROM skill_releases WHERE skill_id = $1 ORDER BY version DESC`, sk.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanSkillRelease(rows)
			if err != nil {
				return err
			}
			out = append(out, *r)
		}
		return rows.Err()
	})
	return out, err
}

// GetSkillRelease returns one skill release.
func (s *Store) GetSkillRelease(ctx context.Context, tenant, releaseID string) (*SkillRelease, error) {
	var out *SkillRelease
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
		out, err = scanSkillRelease(tx.QueryRow(ctx, `SELECT `+skillReleaseCols+` FROM skill_releases WHERE id = $1`, releaseID))
		return err
	})
	return out, err
}

// resolveSkills turns the definition's skill references into embedded, digest-verified skill
// releases: the pinned version when given, else the skill's current release.
func resolveSkills(ctx context.Context, tx pgx.Tx, refs []agent.SkillRef) ([]agent.EmbeddedSkill, error) {
	var out []agent.EmbeddedSkill
	for _, ref := range refs {
		sk, err := getSkill(ctx, tx, ref.Name, false)
		if err != nil {
			return nil, fail("CONFLICT", "skill %s does not exist", ref.Name)
		}
		var envRaw *string
		var version int
		if ref.Version > 0 {
			err = tx.QueryRow(ctx, `SELECT envelope, version FROM skill_releases WHERE skill_id = $1 AND version = $2 AND status = 'APPROVED'`, sk.ID, ref.Version).Scan(&envRaw, &version)
		} else if sk.CurrentReleaseID != nil {
			err = tx.QueryRow(ctx, `SELECT envelope, version FROM skill_releases WHERE id = $1 AND status = 'APPROVED'`, *sk.CurrentReleaseID).Scan(&envRaw, &version)
		} else {
			err = pgx.ErrNoRows
		}
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && envRaw == nil) {
			return nil, fail("CONFLICT", "skill %s has no approved release to embed (version %d)", ref.Name, ref.Version)
		}
		if err != nil {
			return nil, err
		}
		var env agent.Envelope
		if err := json.Unmarshal([]byte(*envRaw), &env); err != nil {
			return nil, err
		}
		doc, err := agent.OpenSkill(&env)
		if err != nil {
			return nil, fail("GONE", "skill release of %s is corrupt: %s", ref.Name, err)
		}
		out = append(out, agent.EmbeddedSkill{Name: sk.Name, Version: version, Digest: env.Digest, Definition: doc.Definition})
	}
	return out, nil
}
