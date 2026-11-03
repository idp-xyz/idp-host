package run_test

// The published contract idp.telos.host 1.0.0 (idp-contracts/idp.telos.host/1.0.0) is produced
// here. api/contracts/schema.json is written by hand; these tests prove it describes exactly what
// the agent package seals and what the API serves as a run record. api/contracts/examples.json is
// generated from Go values (go test ./internal/run -run TestContract -update) and checked on every
// run, so every published example is a document the implementation actually produces.

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
	contractID  = "urn:idp:telos:host:1.1.0"
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

var roots = map[string]bool{"AgentRelease": true, "SkillRelease": true, "Envelope": true, "AgentRun": true}

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
			Tools: []string{"parcel__file_declaration", "parcel__get_tariff"}, Confirmation: map[string]string{"parcel__file_declaration": agent.ConfirmConfirm}}}
}

func exampleDocuments() (full, minimal *agent.Document) {
	temp := 0.2
	skillEnv, _ := agent.SealSkill(exampleSkill())
	skill := exampleSkill()
	skill.Definition.Normalize()
	full = &agent.Document{SchemaVersion: agent.SchemaVersion, ReleaseID: "7f3c1a2e-5b6d-4c8e-9f01-23456789abcd", TenantID: "1a2b3c4d-0000-4000-8000-000000000001",
		AgentID: "9e8d7c6b-0000-4000-8000-000000000002", AgentName: "parcel-assistant", Version: 3, CreatedAt: "2026-11-03T12:00:00Z", CreatedBy: "alice", ApprovedBy: "bob",
		Definition: agent.Definition{Instructions: "You help operators with parcels. Use tools for facts; never invent shipment data.",
			Model:        agent.ModelProfile{Provider: "local", Model: "qwen3.8-flash", Temperature: &temp, MaxOutputTokens: 2048, Fallbacks: []agent.ModelRef{{Provider: "cloud", Model: "claude-sonnet-5-5"}}},
			Tools:        agent.ToolPolicy{Gateway: "edge", Allow: []string{"parcel__get_shipment", "parcel__create_shipment"}},
			Confirmation: agent.ConfirmationPolicy{Default: agent.ConfirmAuto, Tools: map[string]string{"parcel__create_shipment": agent.ConfirmConfirm}},
			Limits:       agent.Limits{MaxSteps: 12, MaxToolCallsPerStep: 4, RunTimeoutSeconds: 600, ConfirmationTimeoutSeconds: 1800, MaxTokensPerRun: 200000},
			Skills:       []agent.SkillRef{{Name: "customs-declaration", Version: 2}}},
		Skills: []agent.EmbeddedSkill{{Name: "customs-declaration", Version: 2, Digest: skillEnv.Digest, Definition: skill.Definition}}}
	minimal = &agent.Document{SchemaVersion: agent.SchemaVersion, ReleaseID: "0b1c2d3e-5b6d-4c8e-9f01-23456789abce", TenantID: "1a2b3c4d-0000-4000-8000-000000000001",
		AgentID: "4f5e6d7c-0000-4000-8000-000000000003", AgentName: "faq", Version: 1, CreatedAt: "2026-11-03T12:30:00Z", CreatedBy: "alice",
		Definition: agent.Definition{Instructions: "Answer questions about the handbook.", Model: agent.ModelProfile{Provider: "local", Model: "qwen3.8-flash"}}}
	return full, minimal
}

func exampleRuns(releaseID, digest string) []run.Run {
	at := time.Date(2026, 11, 3, 13, 0, 0, 0, time.UTC)
	return []run.Run{
		{ID: "run_01JD7Q3Z8M8T0G3C6N4V9S2A1B", ThreadID: "thr-ops-42", AgentID: "9e8d7c6b-0000-4000-8000-000000000002", ReleaseID: releaseID, ReleaseDigest: digest, Owner: "carol",
			Status: run.StatusCompleted, StepCount: 2, TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", DeadlineAt: at.Add(10 * time.Minute), CreatedAt: at, StartedAt: at.Add(time.Second), FinishedAt: at.Add(9 * time.Second)},
		{ID: "run_01JD7Q3Z8M8T0G3C6N4V9S2A1C", ThreadID: "thr-ops-42", AgentID: "9e8d7c6b-0000-4000-8000-000000000002", ReleaseID: releaseID, ReleaseDigest: digest, Owner: "carol",
			Status: run.StatusAwaitingInput, StepCount: 1, DeadlineAt: at.Add(20 * time.Minute), CreatedAt: at.Add(10 * time.Minute), StartedAt: at.Add(10*time.Minute + time.Second)},
		{ID: "run_01JD7Q3Z8M8T0G3C6N4V9S2A1D", ThreadID: "thr-ops-43", AgentID: "9e8d7c6b-0000-4000-8000-000000000002", ReleaseID: releaseID, ReleaseDigest: digest, Owner: "carol",
			Status: run.StatusFailed, StepCount: 12, CancelRequested: false, FailureCode: run.FailStepLimit, FailureMessage: "the run reached its limit of 12 steps",
			ExecutionRef: "eer:execution:0192f3a1", DeadlineAt: at.Add(30 * time.Minute), CreatedAt: at.Add(20 * time.Minute), StartedAt: at.Add(20*time.Minute + time.Second), FinishedAt: at.Add(25 * time.Minute)},
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
	b, err := json.MarshalIndent(examplesFile{
		Description: "idp.telos.host 1.0.0 的示例：每一个都是 idp-host 实际产出的文档（AgentRelease 经 agent.Seal 密封，AgentRun 是 API 返回的运行记录）。",
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
		"SkillRef": agent.SkillRef{}, "SkillDefinition": agent.SkillDefinition{}, "EmbeddedSkill": agent.EmbeddedSkill{}, "SkillRelease": agent.SkillDocument{}}
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
	}
	// cross-reference rules only the implementation can check (a schema cannot relate two arrays)
	implOnly := map[string]struct {
		path  string
		value any
	}{
		"skill name mismatch":    {"skills.0.name", "other-skill"},
		"embedded skill missing": {"skills", []any{}},
		"skill version mismatch": {"skills.0.version", 3},
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
}
