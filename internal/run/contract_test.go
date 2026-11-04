package run_test

// The published contract idp.telos.host (idp-contracts/idp.telos.host/<version>) is produced here.
// api/contracts/schema.json is written by hand; these tests prove it describes exactly what the
// agent package seals, what the API serves as a run record and which caller options the API
// accepts. api/contracts/examples.json is generated from Go values (go test ./internal/run -run
// TestContract -update) and checked on every run, so every published example is a document the
// implementation actually produces (releases, runs) or accepts unchanged (run options).

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"idp-host/internal/agent"
	"idp-host/internal/canonical"
	"idp-host/internal/run"
)

var update = flag.Bool("update", false, "regenerate api/contracts/examples.json from the Go values")

const (
	contractDir = "../../api/contracts"
	contractID  = "urn:idp:telos:host:1.7.0"
)

type noLoader struct{}

func (noLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("external reference %q is not loaded", url)
}

type contract struct {
	t    *testing.T
	c    *jsonschema.Compiler
	defs map[string]*jsonschema.Schema
	raw  map[string]any
}

func loadContract(t *testing.T) *contract {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(contractDir, "schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseLoader(noLoader{})
	c.AssertFormat()
	if err := c.AddResource(contractID, doc); err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	_ = json.Unmarshal(b, &raw)
	return &contract{t: t, c: c, defs: map[string]*jsonschema.Schema{}, raw: raw}
}

func (k *contract) schema(def string) *jsonschema.Schema {
	if s := k.defs[def]; s != nil {
		return s
	}
	loc := contractID
	if def != "" {
		loc += "#/$defs/" + def
	}
	s, err := k.c.Compile(loc)
	if err != nil {
		k.t.Fatalf("compile %s: %v", loc, err)
	}
	k.defs[def] = s
	return s
}

var roots = map[string]bool{"AgentRelease": true, "SkillRelease": true, "Envelope": true, "AgentRun": true, "RunOptions": true}

func (k *contract) validate(def string, raw []byte) error {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	if err := k.schema(def).Validate(inst); err != nil {
		return fmt.Errorf("%s: %w", def, err)
	}
	if roots[def] {
		if err := k.schema("").Validate(inst); err != nil {
			return fmt.Errorf("root oneOf rejects a valid %s: %w", def, err)
		}
	}
	return nil
}

func exampleSkill() *agent.SkillDocument {
	return &agent.SkillDocument{SchemaVersion: agent.SkillSchemaVersion, ReleaseID: "c4d5e6f7-5b6d-4c8e-9f01-23456789abcf", TenantID: "1a2b3c4d-0000-4000-8000-000000000001",
		SkillID: "a1b2c3d4-0000-4000-8000-000000000004", SkillName: "customs-declaration", Version: 2, CreatedAt: "2026-11-03T11:00:00Z", CreatedBy: "alice", ApprovedBy: "bob",
		Definition: agent.SkillDefinition{Instructions: "When a shipment crosses a border, prepare the customs declaration with the declaration tool and ask before filing.",
			Tools: []string{"parcel__file_declaration", "parcel__get_tariff"}, Confirmation: map[string]string{"parcel__file_declaration": agent.ConfirmConfirm},
			Resources: []agent.Resource{{Name: "incoterms", MediaType: "text/markdown", Content: "| Term | Risk passes |\n|---|---|\n| EXW | at the seller's premises |\n| DAP | at the named place |"},
				{Name: "tariff-chapters", MediaType: "application/json", Content: `{"01":"Live animals","02":"Meat"}`, OnDemand: true}}}}
}

func exampleDocuments() (full, minimal *agent.Document) {
	temp := 0.2
	skillEnv, _ := agent.SealSkill(exampleSkill())
	skill := exampleSkill()
	skill.Definition.Normalize()
	full = &agent.Document{SchemaVersion: agent.SchemaVersion, ReleaseID: "7f3c1a2e-5b6d-4c8e-9f01-23456789abcd", TenantID: "1a2b3c4d-0000-4000-8000-000000000001",
		AgentID: "9e8d7c6b-0000-4000-8000-000000000002", AgentName: "parcel-assistant", Version: 3, CreatedAt: "2026-11-03T12:00:00Z", CreatedBy: "alice", ApprovedBy: "bob",
		Definition: agent.Definition{Instructions: "You help operators with parcels. Use tools for facts; never invent shipment data.",
			Model: agent.ModelProfile{Provider: "local", Model: "qwen3.8-flash", Temperature: &temp, MaxOutputTokens: 2048, Fallbacks: []agent.ModelRef{{Provider: "cloud", Model: "claude-sonnet-5-5"}},
				Routing: &agent.Routing{Strategy: agent.RouteCost, Candidates: []agent.Candidate{
					{Provider: "local", Model: "qwen3.8-flash", CostPer1kInput: 0, CostPer1kOutput: 0, Capabilities: []string{"tools", "reasoning"}, MaxContextTokens: 131072},
					{Provider: "cloud", Model: "claude-sonnet-5-5", CostPer1kInput: 3, CostPer1kOutput: 15, Capabilities: []string{"tools", "reasoning", "vision", "longContext"}, MaxContextTokens: 1000000}}}},
			Tools:        agent.ToolPolicy{Gateway: "edge", Allow: []string{"parcel__create_shipment", "parcel__file_declaration", "parcel__get_shipment", "parcel__get_tariff"}},
			Confirmation: agent.ConfirmationPolicy{Default: agent.ConfirmAuto, Tools: map[string]string{"parcel__create_shipment": agent.ConfirmConfirm}},
			Limits:       agent.Limits{MaxSteps: 12, MaxToolCallsPerStep: 4, RunTimeoutSeconds: 600, ConfirmationTimeoutSeconds: 1800, MaxTokensPerRun: 200000, MaxSubagentDepth: 2},
			Skills:       []agent.SkillRef{{Name: "customs-declaration", Version: 2}},
			Subagents: []agent.SubagentRef{{Name: "tariff-expert", Version: 4, Description: "Delegate tariff classification questions (HS codes, duty rates) to the tariff expert."},
				{Name: "carrier-desk", Description: "Ask the carrier's own agent about pickup slots and surcharges.", A2A: &agent.A2ARef{CardURL: "https://agents.carrier.example/a2a/agents/desk/.well-known/agent-card.json"}}},
			SkillMerge: agent.SkillMerge{Instructions: agent.MergeAppend, Tools: agent.MergeCeiling, Confirmation: agent.MergeStrictest},
			Guards: agent.Guards{Input: agent.InputGuard{ToolResults: "block", Context: "wrap"},
				Output: agent.OutputGuard{Patterns: []string{`(?i)internal use only`}, Redact: []string{"email", "card"}, Classifier: "moderation", OnError: "block", Strict: true}},
			Context: agent.ContextPolicy{CMI: true, Purpose: "parcel operations assistant", MaxTokens: 3000, MaxItems: 12, Kinds: []string{"memory", "knowledge"}, Sources: []string{"ops-handbook"}, OnError: "skip", OnStale: "refresh"},
			Evaluation: agent.Evaluation{Required: true, Cases: []agent.EvalCase{
				{Name: "greets", Input: "hello", Expect: agent.Expectation{Contains: []string{"parcel"}, NoToolCall: true, MaxSteps: 1}},
				{Name: "looks-up-shipment", Input: "where is shipment 42?", Expect: agent.Expectation{ToolCalled: "parcel__get_shipment", NotContains: []string{"I cannot"}, Matches: `(?i)shipment 42`}}}}},
		Skills: []agent.EmbeddedSkill{{Name: "customs-declaration", Version: 2, Digest: skillEnv.Digest, Definition: skill.Definition}},
		Subagents: []agent.EmbeddedSubagent{{Name: "tariff-expert", AgentID: "5a6b7c8d-0000-4000-8000-000000000005", Version: 4, Digest: "sha256:" + strings.Repeat("7e", 32)},
			{Name: "carrier-desk", A2A: &agent.A2ARef{CardURL: "https://agents.carrier.example/a2a/agents/desk/.well-known/agent-card.json"}}}}
	minimal = &agent.Document{SchemaVersion: agent.SchemaVersion, ReleaseID: "0b1c2d3e-5b6d-4c8e-9f01-23456789abce", TenantID: "1a2b3c4d-0000-4000-8000-000000000001",
		AgentID: "4f5e6d7c-0000-4000-8000-000000000003", AgentName: "faq", Version: 1, CreatedAt: "2026-11-03T12:30:00Z", CreatedBy: "alice",
		Definition: agent.Definition{Instructions: "Answer questions about the handbook.", Model: agent.ModelProfile{Provider: "local", Model: "qwen3.8-flash"}}}
	return full, minimal
}

func exampleRuns(releaseID, digest string) []run.Run {
	at := time.Date(2026, 11, 3, 13, 0, 0, 0, time.UTC)
	return []run.Run{
		{ID: "run_01JD7Q3Z8M8T0G3C6N4V9S2A1B", ThreadID: "thr-ops-42", AgentID: "9e8d7c6b-0000-4000-8000-000000000002", ReleaseID: releaseID, ReleaseDigest: digest, Owner: "carol",
			Status: run.StatusCompleted, StepCount: 2, TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", ContextPackageID: "pkg_01JD7Q3Z8M8T0G3C6N4V9S2B00",
			DeadlineAt: at.Add(10 * time.Minute), CreatedAt: at, StartedAt: at.Add(time.Second), FinishedAt: at.Add(9 * time.Second)},
		{ID: "run_01JD7Q3Z8M8T0G3C6N4V9S2A1C", ThreadID: "thr-ops-42", AgentID: "9e8d7c6b-0000-4000-8000-000000000002", ReleaseID: releaseID, ReleaseDigest: digest, Owner: "carol",
			Status: run.StatusAwaitingInput, StepCount: 1, DeadlineAt: at.Add(20 * time.Minute), CreatedAt: at.Add(10 * time.Minute), StartedAt: at.Add(10*time.Minute + time.Second)},
		{ID: "run_01JD7Q3Z8M8T0G3C6N4V9S2A1E", ThreadID: "thr-ops-44", AgentID: "9e8d7c6b-0000-4000-8000-000000000002", ReleaseID: releaseID, ReleaseDigest: digest, Owner: "carol",
			Status: run.StatusAwaitingSubagent, StepCount: 1, DeadlineAt: at.Add(30 * time.Minute), CreatedAt: at.Add(20 * time.Minute), StartedAt: at.Add(20*time.Minute + time.Second)},
		{ID: run.ChildRunID("run_01JD7Q3Z8M8T0G3C6N4V9S2A1E", "call_tariff_1"), ThreadID: run.ChildRunID("run_01JD7Q3Z8M8T0G3C6N4V9S2A1E", "call_tariff_1"), AgentID: "5a6b7c8d-0000-4000-8000-000000000005",
			ReleaseID: "e1f2a3b4-5b6d-4c8e-9f01-23456789abd0", ReleaseDigest: "sha256:" + strings.Repeat("7e", 32), Owner: "carol", Status: run.StatusRunning, StepCount: 1,
			ParentRunID: "run_01JD7Q3Z8M8T0G3C6N4V9S2A1E", Depth: 1, DeadlineAt: at.Add(30 * time.Minute), CreatedAt: at.Add(20*time.Minute + 2*time.Second), StartedAt: at.Add(20*time.Minute + 3*time.Second)},
		{ID: "eval_7f3c1a2e5b6d4c8e9f0123456789abcd_0", ThreadID: "eval_7f3c1a2e5b6d4c8e9f0123456789abcd_0", AgentID: "9e8d7c6b-0000-4000-8000-000000000002", ReleaseID: releaseID, ReleaseDigest: digest, Owner: "alice",
			Status: run.StatusCompleted, StepCount: 1, Purpose: run.PurposeEvaluation, DeadlineAt: at.Add(40 * time.Minute), CreatedAt: at.Add(30 * time.Minute), StartedAt: at.Add(30*time.Minute + time.Second), FinishedAt: at.Add(30*time.Minute + 4*time.Second)},
		{ID: "run_01JD7Q3Z8M8T0G3C6N4V9S2A1D", ThreadID: "thr-ops-43", AgentID: "9e8d7c6b-0000-4000-8000-000000000002", ReleaseID: releaseID, ReleaseDigest: digest, Owner: "carol",
			Status: run.StatusFailed, StepCount: 12, CancelRequested: false, FailureCode: run.FailStepLimit, FailureMessage: "the run reached its limit of 12 steps",
			ExecutionRef: "eer:execution:0192f3a1", DeadlineAt: at.Add(30 * time.Minute), CreatedAt: at.Add(20 * time.Minute), StartedAt: at.Add(20*time.Minute + time.Second), FinishedAt: at.Add(25 * time.Minute)},
	}
}

// exampleOptions: what EER sends for one agent turn, and a caller that only pins a release.
func exampleOptions(digest string) []run.Options {
	return []run.Options{
		{ExecutionRef: "execution:shipment-7f3a", ToolExecution: run.ToolExecutionCaller, ReleaseDigest: digest, MaxOutputTokens: 1024},
		{ReleaseDigest: digest},
	}
}

type example struct {
	Definition string          `json:"definition"`
	Value      json.RawMessage `json:"value"`
}

type examplesFile struct {
	Description string    `json:"description"`
	GeneratedBy string    `json:"generatedBy"`
	Examples    []example `json:"examples"`
}

func buildExamples(t *testing.T) []byte {
	t.Helper()
	full, minimal := exampleDocuments()
	var out []example
	add := func(def string, v any) {
		raw, err := canonical.Canonical(v)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, example{Definition: def, Value: raw})
	}
	fullEnv, err := agent.Seal(full)
	if err != nil {
		t.Fatal(err)
	}
	minEnv, err := agent.Seal(minimal)
	if err != nil {
		t.Fatal(err)
	}
	add("AgentRelease", json.RawMessage(fullEnv.Document))
	add("AgentRelease", json.RawMessage(minEnv.Document))
	skillEnv, err := agent.SealSkill(exampleSkill())
	if err != nil {
		t.Fatal(err)
	}
	add("SkillRelease", json.RawMessage(skillEnv.Document))
	add("Envelope", fullEnv)
	add("Envelope", skillEnv)
	for _, r := range exampleRuns(full.ReleaseID, fullEnv.Digest) {
		add("AgentRun", r)
	}
	for _, o := range exampleOptions(fullEnv.Digest) {
		add("RunOptions", o)
	}
	b, err := json.MarshalIndent(examplesFile{
		Description: "idp.telos.host 1.7.0 的示例：每一个都是 idp-host 实际产出或原样接受的文档（AgentRelease 经 agent.Seal 密封，AgentRun 是 API 返回的运行记录，RunOptions 经 run.ParseOptions 解析后原样往返）。",
		GeneratedBy: "go test ./internal/run -run TestContract -update",
		Examples:    out,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

func TestContractExamplesAreGeneratedAndValid(t *testing.T) {
	want := buildExamples(t)
	path := filepath.Join(contractDir, "examples.json")
	if *update {
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	have, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(have, want) {
		t.Fatalf("%s is out of date; run: go test ./internal/run -run TestContract -update", path)
	}
	var f examplesFile
	if err := json.Unmarshal(have, &f); err != nil {
		t.Fatal(err)
	}
	k := loadContract(t)
	seen := map[string]int{}
	for i, x := range f.Examples {
		seen[x.Definition]++
		if err := k.validate(x.Definition, x.Value); err != nil {
			t.Errorf("example %d (%s): %v", i, x.Definition, err)
		}
		switch x.Definition {
		case "AgentRelease":
			digest, _ := canonical.Digest(x.Value)
			if _, err := agent.Open(&agent.Envelope{Digest: digest, Document: x.Value}); err != nil {
				t.Errorf("example %d: the implementation rejects the published AgentRelease: %v", i, err)
			}
		case "SkillRelease":
			digest, _ := canonical.Digest(x.Value)
			if _, err := agent.OpenSkill(&agent.Envelope{Digest: digest, Document: x.Value}); err != nil {
				t.Errorf("example %d: the implementation rejects the published SkillRelease: %v", i, err)
			}
		case "Envelope":
			var env agent.Envelope
			if err := json.Unmarshal(x.Value, &env); err != nil {
				t.Fatal(err)
			}
			if _, err := agent.Open(&env); err != nil {
				if _, err2 := agent.OpenSkill(&env); err2 != nil {
					t.Errorf("example %d: the implementation rejects the published Envelope: %v / %v", i, err, err2)
				}
			}
		case "AgentRun":
			var r run.Run
			dec := json.NewDecoder(bytes.NewReader(x.Value))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&r); err != nil {
				t.Errorf("example %d: not a run record: %v", i, err)
				continue
			}
			back, _ := canonical.Canonical(r)
			if want, _ := canonical.Canonical(x.Value); !bytes.Equal(back, want) {
				t.Errorf("example %d: run record does not round-trip:\n%s\n%s", i, want, back)
			}
		case "RunOptions":
			o, err := run.ParseOptions(x.Value)
			if err != nil {
				t.Errorf("example %d: the API rejects the published RunOptions: %v", i, err)
				continue
			}
			back, _ := canonical.Canonical(o)
			if want, _ := canonical.Canonical(x.Value); !bytes.Equal(back, want) {
				t.Errorf("example %d: run options do not round-trip:\n%s\n%s", i, want, back)
			}
		}
	}
	for def := range roots {
		if seen[def] == 0 {
			t.Errorf("no example for %s", def)
		}
	}
}

// TestContractFieldsMatchGo: every definition is closed, its properties are exactly the struct's
// JSON fields and required is exactly the fields without omitempty/omitzero.
func TestContractFieldsMatchGo(t *testing.T) {
	k := loadContract(t)
	defs := k.raw["$defs"].(map[string]any)
	strs := func(v any) []string {
		var out []string
		for _, x := range v.([]any) {
			out = append(out, x.(string))
		}
		sort.Strings(out)
		return out
	}
	props := func(def string) []string {
		var out []string
		for name := range defs[def].(map[string]any)["properties"].(map[string]any) {
			out = append(out, name)
		}
		sort.Strings(out)
		return out
	}
	closed := map[string]any{"AgentRelease": agent.Document{}, "Definition": agent.Definition{}, "ModelProfile": agent.ModelProfile{}, "ModelRef": agent.ModelRef{},
		"ToolPolicy": agent.ToolPolicy{}, "ConfirmationPolicy": agent.ConfirmationPolicy{}, "Limits": agent.Limits{}, "Envelope": agent.Envelope{}, "AgentRun": run.Run{},
		"SkillRef": agent.SkillRef{}, "SkillDefinition": agent.SkillDefinition{}, "EmbeddedSkill": agent.EmbeddedSkill{}, "SkillRelease": agent.SkillDocument{},
		"RunOptions": run.Options{}, "SubagentRef": agent.SubagentRef{}, "EmbeddedSubagent": agent.EmbeddedSubagent{}, "SkillMerge": agent.SkillMerge{}, "Resource": agent.Resource{},
		"Guards": agent.Guards{}, "InputGuard": agent.InputGuard{}, "OutputGuard": agent.OutputGuard{}, "Evaluation": agent.Evaluation{}, "EvalCase": agent.EvalCase{}, "Expectation": agent.Expectation{},
		"Routing": agent.Routing{}, "Candidate": agent.Candidate{}, "A2ARef": agent.A2ARef{}, "ContextPolicy": agent.ContextPolicy{}}
	for def, zero := range closed {
		d, ok := defs[def].(map[string]any)
		if !ok {
			t.Errorf("definition %s missing", def)
			continue
		}
		if d["additionalProperties"] != false {
			t.Errorf("%s: additionalProperties must be false", def)
		}
		var fields, required []string
		rt := reflect.TypeOf(zero)
		for i := 0; i < rt.NumField(); i++ {
			tag := rt.Field(i).Tag.Get("json")
			name, opts, _ := strings.Cut(tag, ",")
			if name == "-" {
				continue
			}
			fields = append(fields, name)
			if !strings.Contains(opts, "omitempty") && !strings.Contains(opts, "omitzero") {
				required = append(required, name)
			}
		}
		sort.Strings(fields)
		sort.Strings(required)
		if got := props(def); !reflect.DeepEqual(got, fields) {
			t.Errorf("%s: schema properties %v, Go fields %v", def, got, fields)
		}
		if got := strs(d["required"]); !reflect.DeepEqual(got, required) {
			t.Errorf("%s: schema required %v, Go non-omitempty fields %v", def, got, required)
		}
		for _, key := range []string{"title", "description", "x-idp-telos-owner"} {
			if s, _ := d[key].(string); s == "" {
				t.Errorf("%s: %s missing", def, key)
			}
		}
	}
	if len(defs) != len(closed) {
		t.Errorf("schema has %d definitions, the registry lists %d", len(defs), len(closed))
	}
}

func set(m map[string]any, path string, v any) {
	keys := strings.Split(path, ".")
	var cur any = m
	for i, key := range keys {
		last := i == len(keys)-1
		switch node := cur.(type) {
		case map[string]any:
			if last {
				node[key] = v
				return
			}
			cur = node[key]
		case []any:
			var idx int
			fmt.Sscanf(key, "%d", &idx)
			if last {
				node[idx] = v
				return
			}
			cur = node[idx]
		}
	}
}

// TestContractRejectsWhatOpenRejects: documents the implementation refuses are refused by the schema
// too, where the rule is expressible.
func TestContractRejectsWhatOpenRejects(t *testing.T) {
	k := loadContract(t)
	full, _ := exampleDocuments()
	env, err := agent.Seal(full)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		path  string
		value any
	}{
		"wrong schemaVersion":       {"schemaVersion", "idp.host.agent/v2"},
		"version zero":              {"version", 0},
		"bad agent name":            {"agentName", "Parcel Assistant"},
		"empty instructions":        {"definition.instructions", ""},
		"bad provider":              {"definition.model.provider", "Local Provider"},
		"temperature out of range":  {"definition.model.temperature", 3},
		"fallback without model":    {"definition.model.fallbacks.0", map[string]any{"provider": "x"}},
		"bad tool name":             {"definition.tools.allow.0", "bad name"},
		"bad confirmation default":  {"definition.confirmation.default", "ask"},
		"bad confirmation override": {"definition.confirmation.tools", map[string]any{"x": "maybe"}},
		"steps out of range":        {"definition.limits.maxSteps", 1000},
		"unknown top-level field":   {"extra", true},
		"unknown definition field":  {"definition.extra", true},
		"skill digest malformed":    {"skills.0.digest", "sha256:zz"},
		"skill bad tool":            {"skills.0.definition.tools.0", "bad name"},
		"skill ref bad name":        {"definition.skills.0.name", "Customs"},
		"subagent digest malformed": {"subagents.0.digest", "sha256:zz"},
		"subagent ref no text":      {"definition.subagents.0.description", ""},
		"subagent ref bad name":     {"definition.subagents.0.name", "Tariff Expert"},
		"merge rule unknown":        {"definition.skillMerge.tools", "mix"},
		"depth out of range":        {"definition.limits.maxSubagentDepth", 9},
		"resource bad media type":   {"skills.0.definition.resources.0.mediaType", "image/png"},
		"resource empty content":    {"skills.0.definition.resources.0.content", ""},
		"resource bad name":         {"skills.0.definition.resources.0.name", "has space"},
		"reserved allow entry":      {"definition.tools.allow.0", "agent__tariff-expert"},
		"reserved skill tool":       {"skills.0.definition.tools.0", "resource__read"},
		"guard input mode unknown":  {"definition.guards.input.toolResults", "sanitize"},
		"guard redact unknown":      {"definition.guards.output.redact.0", "ssn"},
		"guard onError unknown":     {"definition.guards.output.onError", "retry"},
		"guard pattern empty":       {"definition.guards.output.patterns.0", ""},
		"eval case bad name":        {"definition.evaluation.cases.0.name", "has space"},
		"eval case empty input":     {"definition.evaluation.cases.0.input", ""},
		"eval max steps too high":   {"definition.evaluation.cases.0.expect.maxSteps", 1000},
		"eval tool not a name":      {"definition.evaluation.cases.1.expect.toolCalled", "bad name"},
		"unknown evaluation field":  {"definition.evaluation.extra", true},
		"routing strategy unknown":  {"definition.model.routing.strategy", "random"},
		"routing no candidates":     {"definition.model.routing.candidates", []any{}},
		"candidate bad capability":  {"definition.model.routing.candidates.0.capabilities.0", "telepathy"},
		"candidate negative price":  {"definition.model.routing.candidates.1.costPer1kInput", -1},
		"a2a card not a url":        {"definition.subagents.1.a2a.cardUrl", "carrier-desk"},
		"context onError unknown":   {"definition.context.onError", "retry"},
		"context onStale unknown":   {"definition.context.onStale", "ignore"},
		"context kind unknown":      {"definition.context.kinds.0", "secrets"},
		"context budget too high":   {"definition.context.maxTokens", 64000},
		"context source not a name": {"definition.context.sources.0", "Ops Handbook"},
		"unknown context field":     {"definition.context.ttl", 30},
		"a2a embedded not a url":    {"subagents.1.a2a.cardUrl", "nope"},
	}
	// cross-reference rules only the implementation can check (a schema cannot relate two arrays)
	implOnly := map[string]struct {
		path  string
		value any
	}{
		"skill name mismatch":         {"skills.0.name", "other-skill"},
		"embedded skill missing":      {"skills", []any{}},
		"skill version mismatch":      {"skills.0.version", 3},
		"subagent name mismatch":      {"subagents.0.name", "other-agent"},
		"subagent missing":            {"subagents", []any{}},
		"subagent is itself":          {"subagents.0.agentId", "9e8d7c6b-0000-4000-8000-000000000002"},
		"skill tool over ceiling":     {"skills.0.definition.tools.0", "parcel__pay_duty"},
		"guard bad regex":             {"definition.guards.output.patterns.0", "("},
		"guard onError no classifier": {"definition.guards.output", map[string]any{"onError": "block"}},
		"eval duplicate case":         {"definition.evaluation.cases.1.name", "greets"},
		"eval empty expectation":      {"definition.evaluation.cases.0.expect", map[string]any{}},
		"eval contradiction":          {"definition.evaluation.cases.1.expect", map[string]any{"toolCalled": "x", "noToolCall": true}},
		"eval required no cases":      {"definition.evaluation", map[string]any{"required": true, "cases": []any{}}},
		"candidate duplicated":        {"definition.model.routing.candidates.1", map[string]any{"provider": "local", "model": "qwen3.8-flash"}},
		"a2a ref pinned version":      {"definition.subagents.1.version", 2},
		"a2a embedded with digest":    {"subagents.1.digest", "sha256:" + strings.Repeat("ab", 32)},
		"local embedded as remote":    {"subagents.0.a2a", map[string]any{"cardUrl": "https://x.example/card.json"}},
	}
	for name, c := range implOnly {
		var m map[string]any
		dec := json.NewDecoder(bytes.NewReader(env.Document))
		dec.UseNumber()
		_ = dec.Decode(&m)
		set(m, c.path, c.value)
		raw, _ := canonical.Canonical(m)
		digest, _ := canonical.Digest(json.RawMessage(raw))
		if _, err := agent.Open(&agent.Envelope{Digest: digest, Document: raw}); err == nil {
			t.Errorf("%s: the implementation accepts it", name)
		}
	}
	for name, c := range cases {
		var m map[string]any
		dec := json.NewDecoder(bytes.NewReader(env.Document))
		dec.UseNumber()
		_ = dec.Decode(&m)
		set(m, c.path, c.value)
		raw, _ := canonical.Canonical(m)
		if err := k.validate("AgentRelease", raw); err == nil {
			t.Errorf("%s: schema accepts it", name)
		}
		digest, _ := canonical.Digest(json.RawMessage(raw))
		if _, err := agent.Open(&agent.Envelope{Digest: digest, Document: raw}); err == nil {
			t.Errorf("%s: the implementation accepts it", name)
		}
	}
	runCases := map[string]struct {
		path  string
		value any
	}{
		"unknown status":    {"status", "DONE"},
		"bad run id":        {"runId", "run 1"},
		"bad digest":        {"releaseDigest", "sha256:zz"},
		"negative steps":    {"stepCount", -1},
		"bad trace id":      {"traceId", "xyz"},
		"unknown run field": {"arguments", "x"},
		"empty owner":       {"owner", ""},
		"bad deadline":      {"deadlineAt", "tomorrow"},
		"negative depth":    {"depth", -1},
		"bad parent id":     {"parentRunId", "run 1"},
		"unknown purpose":   {"purpose", "test"},
	}
	r := exampleRuns(full.ReleaseID, env.Digest)[0]
	for name, c := range runCases {
		b, _ := json.Marshal(r)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		set(m, c.path, c.value)
		raw, _ := canonical.Canonical(m)
		if err := k.validate("AgentRun", raw); err == nil {
			t.Errorf("run %s: schema accepts it", name)
		}
	}
	// every option value the API refuses (422) is refused by the schema too
	optionCases := map[string]struct {
		key   string
		value any
	}{
		"unknown option":            {"idp.host/unknown", 1},
		"tool execution both":       {run.OptToolExecution, "both"},
		"tool execution null":       {run.OptToolExecution, nil},
		"digest malformed":          {run.OptReleaseDigest, "sha256:zz"},
		"digest uppercase":          {run.OptReleaseDigest, "sha256:" + strings.Repeat("AB", 32)},
		"max tokens zero":           {run.OptMaxOutputTokens, 0},
		"max tokens over the cap":   {run.OptMaxOutputTokens, 1<<20 + 1},
		"max tokens as text":        {run.OptMaxOutputTokens, "512"},
		"execution ref empty":       {run.OptExecutionRef, ""},
		"execution ref with space":  {run.OptExecutionRef, "has space"},
		"execution ref too long":    {run.OptExecutionRef, strings.Repeat("x", 257)},
		"execution ref not visible": {run.OptExecutionRef, "tab\there"},
	}
	opts := exampleOptions(env.Digest)[0]
	for name, c := range optionCases {
		b, _ := json.Marshal(opts)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		m[c.key] = c.value
		raw, _ := canonical.Canonical(m)
		if err := k.validate("RunOptions", raw); err == nil {
			t.Errorf("options %s: schema accepts it", name)
		}
		if _, err := run.ParseOptions(raw); err == nil {
			t.Errorf("options %s: the API accepts it", name)
		}
	}
	// the schema describes only the idp.host/ keys: the API ignores the caller's other keys
	if _, err := run.ParseOptions(json.RawMessage(`{"app/theme":"dark","idp.host/toolExecution":"server"}`)); err != nil {
		t.Errorf("the API rejects a caller's own forwardedProps key: %v", err)
	}
}
