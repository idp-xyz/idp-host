package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
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
	Resources    []Resource        `json:"resources,omitempty"` // reference material published with the skill (D23)
}

// Resource is a document a skill ships besides its instructions: examples, tables, rules. An inline
// resource is appended to the skill's instructions; an on-demand one is read by the model through
// the HOST's resource__read tool, which serves the release's own content and touches no gateway.
type Resource struct {
	Name      string `json:"name"`
	MediaType string `json:"mediaType"` // text/plain | text/markdown | application/json
	Content   string `json:"content"`
	OnDemand  bool   `json:"onDemand,omitempty"`
}

// Resource bounds.
const (
	MaxResourceSize  = 256 << 10
	MaxResourcesSize = 1 << 20
)

var (
	resourceNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	mediaTypes     = map[string]bool{"text/plain": true, "text/markdown": true, "application/json": true}
)

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
		if !toolNameRE.MatchString(t) || seen[t] || Reserved(t) {
			errs = append(errs, fmt.Sprintf("tools entry %q is invalid, reserved or duplicated", t))
		}
		seen[t] = true
	}
	for tool, v := range d.Confirmation {
		if !toolNameRE.MatchString(tool) || (v != ConfirmAuto && v != ConfirmConfirm) {
			errs = append(errs, fmt.Sprintf("confirmation[%q] is invalid", tool))
		}
	}
	names := map[string]bool{}
	total := 0
	for i, r := range d.Resources {
		if !resourceNameRE.MatchString(r.Name) || names[r.Name] {
			errs = append(errs, fmt.Sprintf("resources[%d]: name is invalid or duplicated", i))
		}
		names[r.Name] = true
		if !mediaTypes[r.MediaType] {
			errs = append(errs, fmt.Sprintf("resources[%d]: mediaType must be text/plain, text/markdown or application/json", i))
		}
		if r.Content == "" || len(r.Content) > MaxResourceSize {
			errs = append(errs, fmt.Sprintf("resources[%d]: content is required (at most %d bytes)", i, MaxResourceSize))
		}
		if r.MediaType == "application/json" && !json.Valid([]byte(r.Content)) {
			errs = append(errs, fmt.Sprintf("resources[%d]: content is not JSON", i))
		}
		total += len(r.Content)
	}
	if total > MaxResourcesSize {
		errs = append(errs, fmt.Sprintf("resources exceed %d bytes in total", MaxResourcesSize))
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("invalid skill definition: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Resource finds a resource by name.
func (d *SkillDefinition) Resource(name string) (Resource, bool) {
	for _, r := range d.Resources {
		if r.Name == name {
			return r, true
		}
	}
	return Resource{}, false
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

// Effective is what the harness runs: the agent's definition with its embedded skills merged in
// under the definition's SkillMerge rules (D24) - instructions appended, prepended or placed at the
// placeholder; tools united or bounded by the agent's allow list; a confirmation requirement
// anywhere winning, or the agent's overrides winning. Inline skill resources (D23) follow their
// skill's instructions.
func (d *Document) Effective() Definition {
	eff := d.Definition
	if len(d.Skills) == 0 {
		return eff
	}
	rules := eff.SkillMerge
	allow := map[string]bool{}
	for _, t := range eff.Tools.Allow {
		allow[t] = true
	}
	conf := map[string]string{}
	for k, v := range eff.Confirmation.Tools {
		conf[k] = v
	}
	var sb strings.Builder
	for _, s := range d.Skills {
		sb.WriteString("\n\n## Skill: " + s.Name + "\n\n" + s.Definition.Instructions)
		for _, r := range s.Definition.Resources {
			if !r.OnDemand {
				sb.WriteString("\n\n### Resource: " + r.Name + " (" + r.MediaType + ")\n\n" + r.Content)
			}
		}
		for _, t := range s.Definition.Tools {
			if rules.Tools != MergeCeiling || eff.Tools.Allows(t) {
				allow[t] = true
			}
		}
		for k, v := range s.Definition.Confirmation {
			switch rules.Confirmation {
			case MergeAgent:
				if _, set := eff.Confirmation.Tools[k]; !set {
					conf[k] = v
				}
			default:
				if v == ConfirmConfirm || conf[k] == "" {
					conf[k] = v
				}
			}
		}
	}
	skills := sb.String()
	switch rules.Instructions {
	case MergePrepend:
		eff.Instructions = strings.TrimPrefix(skills, "\n\n") + "\n\n" + eff.Instructions
	case MergePlaceholder:
		eff.Instructions = strings.Replace(eff.Instructions, SkillsPlaceholder, strings.TrimPrefix(skills, "\n\n"), 1)
	default:
		eff.Instructions += skills
	}
	eff.Tools.Allow = make([]string, 0, len(allow))
	for t := range allow {
		eff.Tools.Allow = append(eff.Tools.Allow, t)
	}
	sort.Strings(eff.Tools.Allow)
	eff.Confirmation.Tools = conf
	return eff
}

// OnDemandResources lists the resources the model may read through resource__read, by skill then name.
func (d *Document) OnDemandResources() map[string]map[string]Resource {
	out := map[string]map[string]Resource{}
	for _, s := range d.Skills {
		for _, r := range s.Definition.Resources {
			if r.OnDemand {
				if out[s.Name] == nil {
					out[s.Name] = map[string]Resource{}
				}
				out[s.Name][r.Name] = r
			}
		}
	}
	return out
}
