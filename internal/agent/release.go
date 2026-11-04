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
	"idp-host/internal/guard"
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

// Tool names the HOST reserves for itself (ADR-0001 D22, D23): a sub-agent is offered to the model as
// agent__<name>; an on-demand skill resource is read through resource__read. A gateway tool with one
// of these prefixes is never offered, and an allow list may not name one.
const (
	SubagentToolPrefix = "agent__"
	ResourceToolPrefix = "resource__"
	ResourceReadTool   = ResourceToolPrefix + "read"
)

// Reserved reports whether a tool name belongs to the HOST's reserved prefixes.
func Reserved(tool string) bool {
	return strings.HasPrefix(tool, SubagentToolPrefix) || strings.HasPrefix(tool, ResourceToolPrefix)
}

// SubagentToolName is the model-facing tool name of a sub-agent reference.
func SubagentToolName(name string) string { return SubagentToolPrefix + name }

// Skill merge rules (ADR-0001 D24).
const (
	MergeAppend      = "append"      // skill instructions follow the agent's (default)
	MergePrepend     = "prepend"     // skill instructions precede the agent's
	MergePlaceholder = "placeholder" // skill instructions replace {{skills}} in the agent's instructions
	MergeUnion       = "union"       // tools: the agent's allow list plus every skill's tools (default)
	MergeCeiling     = "ceiling"     // tools: the agent's allow list is an upper bound; a skill may not exceed it
	MergeStrictest   = "strictest"   // confirmation: confirm anywhere means confirm (default)
	MergeAgent       = "agent"       // confirmation: the agent's per-tool overrides win
)

// SkillsPlaceholder marks where MergePlaceholder puts the skills' instructions.
const SkillsPlaceholder = "{{skills}}"

var (
	nameRE     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)
	toolNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	gatewayRE  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)
	providerRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)
	digestRE   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
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
	Skills       []SkillRef         `json:"skills,omitempty"`    // resolved and embedded when a release is compiled (D17)
	Subagents    []SubagentRef      `json:"subagents,omitempty"` // other agents this one may delegate to (D22)
	SkillMerge   SkillMerge         `json:"skillMerge,omitzero"` // how skills are merged into the effective definition (D24)
	Guards       Guards             `json:"guards,omitzero"`     // input and output content guards (D27, D28)
	Evaluation   Evaluation         `json:"evaluation,omitzero"` // the fixed evaluation set and whether approval needs it (D26)
}

// Guards are the release's content guards.
type Guards struct {
	Input  InputGuard  `json:"input"`
	Output OutputGuard `json:"output,omitzero"`
}

// InputGuard decides what happens to tool results and caller context before the model sees them
// (D27): off, wrap (mark as untrusted, report findings) or block (wrap and withhold flagged content).
type InputGuard struct {
	ToolResults string `json:"toolResults"`
	Context     string `json:"context"`
}

// OutputGuard filters the model's output (D28): blocked patterns (RE2) in addition to the tenant's,
// built-in redactions (email, phone, card), an external classifier registered in the HOST
// configuration with its failure policy, and strict mode (the answer is released only after the
// checks, so nothing is streamed before them).
type OutputGuard struct {
	Patterns   []string `json:"patterns,omitempty"`
	Redact     []string `json:"redact,omitempty"`
	Classifier string   `json:"classifier,omitempty"`
	OnError    string   `json:"onError,omitempty"` // allow (default) | block
	Strict     bool     `json:"strict,omitempty"`
}

// SubagentRef names another agent of the tenant this agent may delegate to; Version 0 means the
// agent's current release at compile time. Description is what the model reads to decide when to
// delegate.
type SubagentRef struct {
	Name        string `json:"name"`
	Version     int    `json:"version,omitempty"`
	Description string `json:"description"`
}

// EmbeddedSubagent is a resolved sub-agent reference inside an agent release: which release of
// the other agent a delegation runs. Only its identity is embedded, never its content.
type EmbeddedSubagent struct {
	Name    string `json:"name"`
	AgentID string `json:"agentId"`
	Version int    `json:"version"`
	Digest  string `json:"digest"`
}

// SkillMerge fixes how embedded skills combine with the agent's own definition.
type SkillMerge struct {
	Instructions string `json:"instructions"` // append | prepend | placeholder
	Tools        string `json:"tools"`        // union | ceiling
	Confirmation string `json:"confirmation"` // strictest | agent
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
// MaxSubagentDepth is how deep delegation may nest below a run of this release: a run at depth d
// may delegate only while d < MaxSubagentDepth (D22).
type Limits struct {
	MaxSteps                   int   `json:"maxSteps"`
	MaxToolCallsPerStep        int   `json:"maxToolCallsPerStep"`
	RunTimeoutSeconds          int   `json:"runTimeoutSeconds"`
	ConfirmationTimeoutSeconds int   `json:"confirmationTimeoutSeconds"`
	MaxTokensPerRun            int64 `json:"maxTokensPerRun,omitempty"`
	MaxSubagentDepth           int   `json:"maxSubagentDepth,omitempty"`
}

// Defaults applied by Normalize.
var DefaultLimits = Limits{MaxSteps: 10, MaxToolCallsPerStep: 8, RunTimeoutSeconds: 300, ConfirmationTimeoutSeconds: 900, MaxSubagentDepth: 1}

// MaxSubagentDepth is the deepest nesting a release may allow.
const MaxSubagentDepth = 5

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
	if d.Limits.MaxSubagentDepth == 0 {
		d.Limits.MaxSubagentDepth = DefaultLimits.MaxSubagentDepth
	}
	if d.SkillMerge.Instructions == "" {
		d.SkillMerge.Instructions = MergeAppend
	}
	if d.SkillMerge.Tools == "" {
		d.SkillMerge.Tools = MergeUnion
	}
	if d.SkillMerge.Confirmation == "" {
		d.SkillMerge.Confirmation = MergeStrictest
	}
	if d.Guards.Input.ToolResults == "" {
		d.Guards.Input.ToolResults = guard.ModeWrap
	}
	if d.Guards.Input.Context == "" {
		d.Guards.Input.Context = guard.ModeWrap
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
		if t != AllowAll && !toolNameRE.MatchString(t) || seen[t] || Reserved(t) {
			bad("tools.allow entry %q is invalid, reserved or duplicated", t)
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
	names := map[string]bool{}
	for i, s := range d.Skills {
		if !ValidName(s.Name) || s.Version < 0 || names[s.Name] {
			bad("skills[%d]: invalid or duplicated", i)
		}
		names[s.Name] = true
	}
	subs := map[string]bool{}
	for i, s := range d.Subagents {
		// the model-facing tool name must fit every provider's 64-character limit
		if !ValidName(s.Name) || len(SubagentToolName(s.Name)) > 64 || s.Version < 0 || subs[s.Name] {
			bad("subagents[%d]: invalid, too long or duplicated", i)
		}
		if strings.TrimSpace(s.Description) == "" || len(s.Description) > 1024 {
			bad("subagents[%d]: description is required (at most 1024 characters)", i)
		}
		subs[s.Name] = true
	}
	if m := d.SkillMerge; m.Instructions != MergeAppend && m.Instructions != MergePrepend && m.Instructions != MergePlaceholder ||
		m.Tools != MergeUnion && m.Tools != MergeCeiling || m.Confirmation != MergeStrictest && m.Confirmation != MergeAgent {
		bad("skillMerge values are invalid")
	}
	if d.SkillMerge.Instructions == MergePlaceholder && strings.Count(d.Instructions, SkillsPlaceholder) != 1 {
		bad("skillMerge.instructions placeholder needs exactly one %s in the instructions", SkillsPlaceholder)
	}
	for name, mode := range map[string]string{"toolResults": d.Guards.Input.ToolResults, "context": d.Guards.Input.Context} {
		if mode != guard.ModeOff && mode != guard.ModeWrap && mode != guard.ModeBlock {
			bad("guards.input.%s must be off, wrap or block", name)
		}
	}
	if _, err := guard.NewOutput(nil, d.Guards.Output.Patterns, d.Guards.Output.Redact); err != nil {
		bad("guards.output: %v", err)
	}
	if c := d.Guards.Output.Classifier; c != "" && !providerRE.MatchString(c) {
		bad("guards.output.classifier is invalid")
	}
	if e := d.Guards.Output.OnError; e != "" && e != guard.OnErrorAllow && e != guard.OnErrorBlock {
		bad("guards.output.onError must be allow or block")
	}
	if d.Guards.Output.OnError != "" && d.Guards.Output.Classifier == "" {
		bad("guards.output.onError needs a classifier")
	}
	if err := d.Evaluation.Validate(); err != nil {
		bad("evaluation: %v", err)
	}
	l := d.Limits
	if l.MaxSteps < 1 || l.MaxSteps > 200 || l.MaxToolCallsPerStep < 1 || l.MaxToolCallsPerStep > 64 ||
		l.RunTimeoutSeconds < 1 || l.RunTimeoutSeconds > 86400 || l.ConfirmationTimeoutSeconds < 1 || l.ConfirmationTimeoutSeconds > 86400 ||
		l.MaxTokensPerRun < 0 || l.MaxTokensPerRun > 100_000_000 || l.MaxSubagentDepth < 1 || l.MaxSubagentDepth > MaxSubagentDepth {
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
	// Skills are the skill releases the definition referenced, resolved when the release was
	// compiled (one per reference, same order).
	Skills []EmbeddedSkill `json:"skills,omitempty"`
	// Subagents are the agent releases the definition's sub-agent references resolved to when the
	// release was compiled (one per reference, same order).
	Subagents []EmbeddedSubagent `json:"subagents,omitempty"`
}

// Validate checks the document, including that the embedded skills match the references and
// that the effective tool set has a gateway.
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
	if err := d.Definition.Validate(); err != nil {
		return err
	}
	if len(d.Skills) != len(d.Definition.Skills) {
		return errors.New("skills: every reference must be embedded exactly once")
	}
	for i, s := range d.Skills {
		ref := d.Definition.Skills[i]
		if s.Name != ref.Name || s.Version < 1 || (ref.Version != 0 && s.Version != ref.Version) || !digestRE.MatchString(s.Digest) {
			return fmt.Errorf("skills[%d]: does not match reference %s", i, ref.Name)
		}
		if err := s.Definition.Validate(); err != nil {
			return fmt.Errorf("skills[%d]: %w", i, err)
		}
	}
	if len(d.Subagents) != len(d.Definition.Subagents) {
		return errors.New("subagents: every reference must be embedded exactly once")
	}
	for i, s := range d.Subagents {
		ref := d.Definition.Subagents[i]
		if s.Name != ref.Name || s.AgentID == "" || s.Version < 1 || (ref.Version != 0 && s.Version != ref.Version) || !digestRE.MatchString(s.Digest) {
			return fmt.Errorf("subagents[%d]: does not match reference %s", i, ref.Name)
		}
		if s.Name == d.AgentName || s.AgentID == d.AgentID {
			return fmt.Errorf("subagents[%d]: an agent cannot delegate to itself", i)
		}
	}
	if d.Definition.SkillMerge.Tools == MergeCeiling {
		for i, s := range d.Skills {
			for _, t := range s.Definition.Tools {
				if !d.Definition.Tools.Allows(t) {
					return fmt.Errorf("skills[%d]: tool %s exceeds the agent's allow list (skillMerge.tools = ceiling)", i, t)
				}
			}
		}
	}
	if eff := d.Effective(); len(eff.Tools.Allow) > 0 && !gatewayRE.MatchString(eff.Tools.Gateway) {
		return errors.New("tools.gateway is required when the agent or its skills use tools")
	}
	return nil
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

// Evaluation is the release's fixed evaluation set (ADR-0001 D26): cases the HOST runs as ordinary
// runs of the candidate release, each judged against its expectation. Required releases cannot be
// approved before their latest evaluation passed in full.
type Evaluation struct {
	Required bool       `json:"required,omitempty"`
	Cases    []EvalCase `json:"cases"`
}

// EvalCase is one evaluation case: a user message and what the agent's answer must satisfy.
type EvalCase struct {
	Name   string      `json:"name"`
	Input  string      `json:"input"`
	Expect Expectation `json:"expect"`
}

// Expectation is judged on the run's final assistant message, the tools it called and its step count.
type Expectation struct {
	Contains    []string `json:"contains,omitempty"`    // every substring must appear (case-insensitive)
	NotContains []string `json:"notContains,omitempty"` // no substring may appear (case-insensitive)
	Matches     string   `json:"matches,omitempty"`     // RE2 the answer must match
	ToolCalled  string   `json:"toolCalled,omitempty"`  // a tool the run must have called
	NoToolCall  bool     `json:"noToolCall,omitempty"`  // the run must not have called any tool
	MaxSteps    int      `json:"maxSteps,omitempty"`    // the run must finish within this many steps
}

// MaxEvalCases bounds an evaluation set.
const MaxEvalCases = 32

var caseNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// Validate checks the evaluation set.
func (e *Evaluation) Validate() error {
	if len(e.Cases) > MaxEvalCases {
		return fmt.Errorf("at most %d cases", MaxEvalCases)
	}
	if e.Required && len(e.Cases) == 0 {
		return errors.New("a required evaluation needs at least one case")
	}
	names := map[string]bool{}
	for i, c := range e.Cases {
		if !caseNameRE.MatchString(c.Name) || names[c.Name] {
			return fmt.Errorf("cases[%d]: name is invalid or duplicated", i)
		}
		names[c.Name] = true
		if strings.TrimSpace(c.Input) == "" || len(c.Input) > 64<<10 {
			return fmt.Errorf("cases[%d]: input is required (at most 64 KiB)", i)
		}
		x := c.Expect
		if len(x.Contains) == 0 && len(x.NotContains) == 0 && x.Matches == "" && x.ToolCalled == "" && !x.NoToolCall && x.MaxSteps == 0 {
			return fmt.Errorf("cases[%d]: expect needs at least one rule", i)
		}
		for _, s := range append(append([]string{}, x.Contains...), x.NotContains...) {
			if s == "" || len(s) > 1024 {
				return fmt.Errorf("cases[%d]: contains/notContains entries must be 1-1024 characters", i)
			}
		}
		if x.Matches != "" {
			if len(x.Matches) > 512 {
				return fmt.Errorf("cases[%d]: matches is too long", i)
			}
			if _, err := regexp.Compile(x.Matches); err != nil {
				return fmt.Errorf("cases[%d]: matches: %v", i, err)
			}
		}
		if x.ToolCalled != "" && !toolNameRE.MatchString(x.ToolCalled) {
			return fmt.Errorf("cases[%d]: toolCalled is not a tool name", i)
		}
		if x.ToolCalled != "" && x.NoToolCall {
			return fmt.Errorf("cases[%d]: toolCalled and noToolCall contradict", i)
		}
		if x.MaxSteps < 0 || x.MaxSteps > 200 {
			return fmt.Errorf("cases[%d]: maxSteps out of range", i)
		}
	}
	return nil
}

// Outcome is what an evaluation case is judged on.
type Outcome struct {
	Status      string   // the run's terminal status
	FailureCode string   // when it failed
	Answer      string   // the run's last assistant message
	Tools       []string // the tools the run called (HOST-executed, delegated or frontend)
	Steps       int
}

// Judge decides whether an outcome satisfies the expectation and says why not.
func (x Expectation) Judge(o Outcome) (bool, string) {
	if o.Status != "COMPLETED" {
		return false, fmt.Sprintf("the run ended %s %s", o.Status, o.FailureCode)
	}
	lower := strings.ToLower(o.Answer)
	for _, s := range x.Contains {
		if !strings.Contains(lower, strings.ToLower(s)) {
			return false, fmt.Sprintf("answer does not contain %q", s)
		}
	}
	for _, s := range x.NotContains {
		if strings.Contains(lower, strings.ToLower(s)) {
			return false, fmt.Sprintf("answer contains %q", s)
		}
	}
	if x.Matches != "" {
		if re, err := regexp.Compile(x.Matches); err != nil || !re.MatchString(o.Answer) {
			return false, fmt.Sprintf("answer does not match %q", x.Matches)
		}
	}
	if x.ToolCalled != "" {
		called := false
		for _, t := range o.Tools {
			if t == x.ToolCalled {
				called = true
			}
		}
		if !called {
			return false, fmt.Sprintf("tool %s was not called (called: %s)", x.ToolCalled, strings.Join(o.Tools, ", "))
		}
	}
	if x.NoToolCall && len(o.Tools) > 0 {
		return false, "tools were called: " + strings.Join(o.Tools, ", ")
	}
	if x.MaxSteps > 0 && o.Steps > x.MaxSteps {
		return false, fmt.Sprintf("%d steps, at most %d allowed", o.Steps, x.MaxSteps)
	}
	return true, ""
}
