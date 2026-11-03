// Package agent defines the AgentRelease (ADR-0001 D2): the immutable, content-addressed document
// that fixes everything an agent's behaviour depends on - instructions, model, tools, confirmation
// policy and limits - and the editable definition it is compiled from. A run records the digest of
// the release it ran.
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

// SchemaVersion identifies the release document format.
const SchemaVersion = "idp.host.agent/v1"

// Confirmation policy values.
const (
	ConfirmAuto    = "auto"    // the tool call is executed without asking
	ConfirmConfirm = "confirm" // the run waits for the owner's confirmation
)

// AllowAll in a tool allow list grants every tool the gateway shows the caller.
const AllowAll = "*"

var (
	nameRE     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)
	toolNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	gatewayRE  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)
	providerRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)
)

// ValidName reports whether s may name an agent.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// Definition is the editable part of an agent: what the release fixes.
type Definition struct {
	Instructions string             `json:"instructions"`
	Model        ModelProfile       `json:"model"`
	Tools        ToolPolicy         `json:"tools"`
	Confirmation ConfirmationPolicy `json:"confirmation"`
	Limits       Limits             `json:"limits"`
}

// ModelProfile selects the model and its parameters; Fallbacks are tried in order when the
// primary provider fails.
type ModelProfile struct {
	Provider        string     `json:"provider"`
	Model           string     `json:"model"`
	Temperature     *float64   `json:"temperature,omitempty"`
	MaxOutputTokens int        `json:"maxOutputTokens,omitempty"`
	Fallbacks       []ModelRef `json:"fallbacks,omitempty"`
}

// ModelRef names a provider and model.
type ModelRef struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// ToolPolicy names the MCP gateway and the exposed tool names the agent may use ("*" = all the
// gateway shows the caller). Empty Allow means the agent has no tools.
type ToolPolicy struct {
	Gateway string   `json:"gateway,omitempty"`
	Allow   []string `json:"allow"`
}

// ConfirmationPolicy decides which tool calls wait for the owner (ADR-0001 D7).
type ConfirmationPolicy struct {
	Default string            `json:"default"`
	Tools   map[string]string `json:"tools,omitempty"`
}

// For returns the policy for a tool.
func (c ConfirmationPolicy) For(tool string) string {
	if v, ok := c.Tools[tool]; ok {
		return v
	}
	return c.Default
}

// Limits bound a run. MaxTokensPerRun 0 means "the tenant policy's default" (ADR-0001 D15).
type Limits struct {
	MaxSteps                   int   `json:"maxSteps"`
	MaxToolCallsPerStep        int   `json:"maxToolCallsPerStep"`
	RunTimeoutSeconds          int   `json:"runTimeoutSeconds"`
	ConfirmationTimeoutSeconds int   `json:"confirmationTimeoutSeconds"`
	MaxTokensPerRun            int64 `json:"maxTokensPerRun,omitempty"`
}

// Defaults applied by Normalize.
var DefaultLimits = Limits{MaxSteps: 10, MaxToolCallsPerStep: 8, RunTimeoutSeconds: 300, ConfirmationTimeoutSeconds: 900}

// Normalize fills defaults and sorts the allow list so equal definitions compile to equal releases.
func (d *Definition) Normalize() {
	if d.Limits.MaxSteps == 0 {
		d.Limits.MaxSteps = DefaultLimits.MaxSteps
	}
	if d.Limits.MaxToolCallsPerStep == 0 {
		d.Limits.MaxToolCallsPerStep = DefaultLimits.MaxToolCallsPerStep
	}
	if d.Limits.RunTimeoutSeconds == 0 {
		d.Limits.RunTimeoutSeconds = DefaultLimits.RunTimeoutSeconds
	}
	if d.Limits.ConfirmationTimeoutSeconds == 0 {
		d.Limits.ConfirmationTimeoutSeconds = DefaultLimits.ConfirmationTimeoutSeconds
	}
	if d.Confirmation.Default == "" {
		d.Confirmation.Default = ConfirmAuto
	}
	if d.Tools.Allow == nil {
		d.Tools.Allow = []string{}
	}
	sort.Strings(d.Tools.Allow)
	if d.Model.Fallbacks == nil {
		d.Model.Fallbacks = []ModelRef{}
	}
}

// Validate checks a definition. It fails closed on anything the harness could misinterpret.
func (d *Definition) Validate() error {
	var errs []string
	bad := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }
	if strings.TrimSpace(d.Instructions) == "" || len(d.Instructions) > 64<<10 {
		bad("instructions are required (at most 64 KiB)")
	}
	if !providerRE.MatchString(d.Model.Provider) || d.Model.Model == "" || len(d.Model.Model) > 256 {
		bad("model.provider and model.model are required")
	}
	if d.Model.Temperature != nil && (*d.Model.Temperature < 0 || *d.Model.Temperature > 2) {
		bad("model.temperature must be within [0, 2]")
	}
	if d.Model.MaxOutputTokens < 0 || d.Model.MaxOutputTokens > 1<<20 {
		bad("model.maxOutputTokens out of range")
	}
	for i, f := range d.Model.Fallbacks {
		if !providerRE.MatchString(f.Provider) || f.Model == "" {
			bad("model.fallbacks[%d]: provider and model are required", i)
		}
	}
	seen := map[string]bool{}
	for _, t := range d.Tools.Allow {
		if t != AllowAll && !toolNameRE.MatchString(t) || seen[t] {
			bad("tools.allow entry %q is invalid or duplicated", t)
		}
		seen[t] = true
	}
	if len(d.Tools.Allow) > 0 && !gatewayRE.MatchString(d.Tools.Gateway) {
		bad("tools.gateway is required when tools are allowed")
	}
	if len(d.Tools.Allow) == 0 && d.Tools.Gateway != "" && !gatewayRE.MatchString(d.Tools.Gateway) {
		bad("tools.gateway is invalid")
	}
	if d.Confirmation.Default != ConfirmAuto && d.Confirmation.Default != ConfirmConfirm {
		bad("confirmation.default must be auto or confirm")
	}
	for tool, v := range d.Confirmation.Tools {
		if !toolNameRE.MatchString(tool) || (v != ConfirmAuto && v != ConfirmConfirm) {
			bad("confirmation.tools[%q] is invalid", tool)
		}
	}
	l := d.Limits
	if l.MaxSteps < 1 || l.MaxSteps > 200 || l.MaxToolCallsPerStep < 1 || l.MaxToolCallsPerStep > 64 ||
		l.RunTimeoutSeconds < 1 || l.RunTimeoutSeconds > 86400 || l.ConfirmationTimeoutSeconds < 1 || l.ConfirmationTimeoutSeconds > 86400 ||
		l.MaxTokensPerRun < 0 || l.MaxTokensPerRun > 100_000_000 {
		bad("limits out of range")
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("invalid definition: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Allows reports whether the exposed tool name is in the allow list.
func (p ToolPolicy) Allows(tool string) bool {
	for _, t := range p.Allow {
		if t == AllowAll || t == tool {
			return true
		}
	}
	return false
}

// Document is the sealed content of a release.
type Document struct {
	SchemaVersion string     `json:"schemaVersion"`
	ReleaseID     string     `json:"releaseId"`
	TenantID      string     `json:"tenantId"`
	AgentID       string     `json:"agentId"`
	AgentName     string     `json:"agentName"`
	Version       int        `json:"version"`
	CreatedAt     string     `json:"createdAt"` // RFC 3339, UTC
	CreatedBy     string     `json:"createdBy"`
	ApprovedBy    string     `json:"approvedBy,omitempty"` // part of the digest once approved
	Definition    Definition `json:"definition"`
}

// Validate checks the document.
func (d *Document) Validate() error {
	if d.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schemaVersion must be %s", SchemaVersion)
	}
	if d.ReleaseID == "" || d.TenantID == "" || d.AgentID == "" || d.CreatedAt == "" || d.CreatedBy == "" {
		return errors.New("releaseId, tenantId, agentId, createdAt and createdBy are required")
	}
	if !ValidName(d.AgentName) {
		return errors.New("agentName is invalid")
	}
	if d.Version < 1 {
		return errors.New("version must be >= 1")
	}
	return d.Definition.Validate()
}

// Envelope is what is stored and distributed: the document plus its digest.
type Envelope struct {
	Digest   string          `json:"digest"`
	Document json.RawMessage `json:"document"`
}

// Seal normalizes and validates a document and wraps it with its digest.
func Seal(d *Document) (*Envelope, error) {
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

// Open verifies an envelope's digest and validates its document.
func Open(e *Envelope) (*Document, error) {
	digest, err := canonical.Digest(e.Document)
	if err != nil {
		return nil, fmt.Errorf("release document is not JSON: %w", err)
	}
	if digest != e.Digest {
		return nil, errors.New("release digest does not match its document")
	}
	var d Document
	dec := json.NewDecoder(strings.NewReader(string(e.Document)))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("release document: %w", err)
	}
	if err := d.Validate(); err != nil {
		return nil, err
	}
	return &d, nil
}
