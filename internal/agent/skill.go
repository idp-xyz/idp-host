package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"idp-host/internal/canonical"
)

// Skills (ADR-0001 D17): a skill is an instruction fragment plus the tools it needs and the
// confirmation overrides it demands. Skills are published like agents (draft -> SkillRelease with
// four-eyes approval); an agent definition references skills by name (optionally pinned to a
// version) and the agent's release EMBEDS the resolved skill releases, so a release stays
// self-contained and content-addressed.

// SkillSchemaVersion identifies the skill release document format.
const SkillSchemaVersion = "idp.host.skill/v1"

// SkillDefinition is the editable content of a skill.
type SkillDefinition struct {
	Instructions string            `json:"instructions"`
	Tools        []string          `json:"tools"`
	Confirmation map[string]string `json:"confirmation,omitempty"`
}

// Normalize sorts the tool list so equal definitions compile to equal releases.
func (d *SkillDefinition) Normalize() {
	if d.Tools == nil {
		d.Tools = []string{}
	}
	sort.Strings(d.Tools)
}

// Validate checks a skill definition.
func (d *SkillDefinition) Validate() error {
	var errs []string
	if strings.TrimSpace(d.Instructions) == "" || len(d.Instructions) > 64<<10 {
		errs = append(errs, "instructions are required (at most 64 KiB)")
	}
	seen := map[string]bool{}
	for _, t := range d.Tools {
		if !toolNameRE.MatchString(t) || seen[t] {
			errs = append(errs, fmt.Sprintf("tools entry %q is invalid or duplicated", t))
		}
		seen[t] = true
	}
	for tool, v := range d.Confirmation {
		if !toolNameRE.MatchString(tool) || (v != ConfirmAuto && v != ConfirmConfirm) {
			errs = append(errs, fmt.Sprintf("confirmation[%q] is invalid", tool))
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("invalid skill definition: %s", strings.Join(errs, "; "))
	}
	return nil
}

// SkillRef names a skill in an agent definition; Version 0 means the skill's current release.
type SkillRef struct {
	Name    string `json:"name"`
	Version int    `json:"version,omitempty"`
}

// EmbeddedSkill is a resolved skill release inside an agent release.
type EmbeddedSkill struct {
	Name       string          `json:"name"`
	Version    int             `json:"version"`
	Digest     string          `json:"digest"`
	Definition SkillDefinition `json:"definition"`
}

// SkillDocument is the sealed content of a skill release.
type SkillDocument struct {
	SchemaVersion string          `json:"schemaVersion"`
	ReleaseID     string          `json:"releaseId"`
	TenantID      string          `json:"tenantId"`
	SkillID       string          `json:"skillId"`
	SkillName     string          `json:"skillName"`
	Version       int             `json:"version"`
	CreatedAt     string          `json:"createdAt"`
	CreatedBy     string          `json:"createdBy"`
	ApprovedBy    string          `json:"approvedBy,omitempty"`
	Definition    SkillDefinition `json:"definition"`
}

// Validate checks the document.
func (d *SkillDocument) Validate() error {
	if d.SchemaVersion != SkillSchemaVersion {
		return fmt.Errorf("schemaVersion must be %s", SkillSchemaVersion)
	}
	if d.ReleaseID == "" || d.TenantID == "" || d.SkillID == "" || d.CreatedAt == "" || d.CreatedBy == "" {
		return errors.New("releaseId, tenantId, skillId, createdAt and createdBy are required")
	}
	if !ValidName(d.SkillName) {
		return errors.New("skillName is invalid")
	}
	if d.Version < 1 {
		return errors.New("version must be >= 1")
	}
	return d.Definition.Validate()
}

// SealSkill normalizes and validates a skill document and wraps it with its digest.
func SealSkill(d *SkillDocument) (*Envelope, error) {
	d.Definition.Normalize()
	if err := d.Validate(); err != nil {
		return nil, err
	}
	raw, err := canonical.Canonical(d)
	if err != nil {
		return nil, err
	}
	digest, err := canonical.Digest(json.RawMessage(raw))
	if err != nil {
		return nil, err
	}
	return &Envelope{Digest: digest, Document: raw}, nil
}

// OpenSkill verifies an envelope's digest and validates its skill document.
func OpenSkill(e *Envelope) (*SkillDocument, error) {
	digest, err := canonical.Digest(e.Document)
	if err != nil {
		return nil, fmt.Errorf("skill document is not JSON: %w", err)
	}
	if digest != e.Digest {
		return nil, errors.New("skill digest does not match its document")
	}
	var d SkillDocument
	dec := json.NewDecoder(strings.NewReader(string(e.Document)))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("skill document: %w", err)
	}
	if err := d.Validate(); err != nil {
		return nil, err
	}
	return &d, nil
}

// Effective is what the harness runs: the agent's definition with its embedded skills merged in -
// instructions concatenated, tool allow lists united, and a tool needing confirmation anywhere
// needing it everywhere.
func (d *Document) Effective() Definition {
	eff := d.Definition
	if len(d.Skills) == 0 {
		return eff
	}
	var sb strings.Builder
	sb.WriteString(eff.Instructions)
	allow := map[string]bool{}
	for _, t := range eff.Tools.Allow {
		allow[t] = true
	}
	conf := map[string]string{}
	for k, v := range eff.Confirmation.Tools {
		conf[k] = v
	}
	for _, s := range d.Skills {
		sb.WriteString("\n\n## Skill: " + s.Name + "\n\n" + s.Definition.Instructions)
		for _, t := range s.Definition.Tools {
			allow[t] = true
		}
		for k, v := range s.Definition.Confirmation {
			if v == ConfirmConfirm || conf[k] == "" {
				conf[k] = v
			}
		}
	}
	eff.Instructions = sb.String()
	eff.Tools.Allow = make([]string, 0, len(allow))
	for t := range allow {
		eff.Tools.Allow = append(eff.Tools.Allow, t)
	}
	sort.Strings(eff.Tools.Allow)
	eff.Confirmation.Tools = conf
	return eff
}
