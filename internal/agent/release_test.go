package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func sample() *Document {
	return &Document{SchemaVersion: SchemaVersion, ReleaseID: "rel-1", TenantID: "t1", AgentID: "a1", AgentName: "parcel-assistant", Version: 1,
		CreatedAt: "2026-11-03T08:00:00Z", CreatedBy: "alice",
		Definition: Definition{Instructions: "You help with parcels.", Model: ModelProfile{Provider: "local", Model: "qwen"},
			Tools:        ToolPolicy{Gateway: "edge", Allow: []string{"parcel__get_shipment", "parcel__create_shipment"}},
			Confirmation: ConfirmationPolicy{Default: ConfirmAuto, Tools: map[string]string{"parcel__create_shipment": ConfirmConfirm}}}}
}

func TestSealAppliesDefaultsAndIsDeterministic(t *testing.T) {
	a, err := Seal(sample())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Seal(sample())
	if a.Digest != b.Digest {
		t.Fatal("same definition, different digests")
	}
	d, err := Open(a)
	if err != nil {
		t.Fatal(err)
	}
	if d.Definition.Limits != DefaultLimits || d.Definition.Tools.Allow[0] != "parcel__create_shipment" {
		t.Fatalf("defaults or sorting not applied: %+v", d.Definition)
	}
	if strings.Contains(string(a.Document), "null") {
		t.Fatalf("sealed document contains null: %s", a.Document)
	}
	approved := sample()
	approved.ApprovedBy = "bob"
	c, _ := Seal(approved)
	if c.Digest == a.Digest {
		t.Fatal("approvedBy must be part of the digest")
	}
}

func TestOpenRefusesTampering(t *testing.T) {
	env, _ := Seal(sample())
	tampered := &Envelope{Digest: env.Digest, Document: json.RawMessage(strings.Replace(string(env.Document), "You help", "You rule", 1))}
	if _, err := Open(tampered); err == nil {
		t.Fatal("tampered document opened")
	}
	unknown := &Envelope{Document: json.RawMessage(strings.Replace(string(env.Document), `"version":1`, `"version":1,"extra":true`, 1))}
	unknown.Digest, _ = digestOf(unknown.Document)
	if _, err := Open(unknown); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field accepted: %v", err)
	}
}

func digestOf(raw json.RawMessage) (string, error) { return canonicalDigest(raw) }

func TestValidateFailsClosed(t *testing.T) {
	cases := map[string]func(*Document){
		"no instructions":        func(d *Document) { d.Definition.Instructions = " " },
		"bad provider":           func(d *Document) { d.Definition.Model.Provider = "Local Provider" },
		"no model":               func(d *Document) { d.Definition.Model.Model = "" },
		"temperature":            func(d *Document) { v := 3.0; d.Definition.Model.Temperature = &v },
		"tools without gateway":  func(d *Document) { d.Definition.Tools.Gateway = "" },
		"bad tool name":          func(d *Document) { d.Definition.Tools.Allow = []string{"bad name"} },
		"duplicate tool":         func(d *Document) { d.Definition.Tools.Allow = []string{"x", "x"} },
		"bad confirmation":       func(d *Document) { d.Definition.Confirmation.Default = "ask" },
		"bad confirmation tool":  func(d *Document) { d.Definition.Confirmation.Tools = map[string]string{"x": "maybe"} },
		"steps out of range":     func(d *Document) { d.Definition.Limits.MaxSteps = 1000 },
		"bad agent name":         func(d *Document) { d.AgentName = "Parcel" },
		"version zero":           func(d *Document) { d.Version = 0 },
		"wrong schema":           func(d *Document) { d.SchemaVersion = "idp.host.agent/v2" },
		"fallback without model": func(d *Document) { d.Definition.Model.Fallbacks = []ModelRef{{Provider: "x"}} },
	}
	for name, mutate := range cases {
		d := sample()
		mutate(d)
		if _, err := Seal(d); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := sample()
	ok.Definition.Tools = ToolPolicy{} // an agent without tools needs no gateway
	if _, err := Seal(ok); err != nil {
		t.Fatal(err)
	}
}

func TestPolicies(t *testing.T) {
	d := sample().Definition
	if !d.Tools.Allows("parcel__get_shipment") || d.Tools.Allows("other") {
		t.Fatal("allow list")
	}
	if !(ToolPolicy{Allow: []string{AllowAll}}).Allows("anything") {
		t.Fatal("wildcard")
	}
	if d.Confirmation.For("parcel__create_shipment") != ConfirmConfirm || d.Confirmation.For("parcel__get_shipment") != ConfirmAuto {
		t.Fatal("confirmation policy")
	}
}

func TestSkillMergeRulesAndResources(t *testing.T) {
	skill := func(tools []string, conf map[string]string) EmbeddedSkill {
		return EmbeddedSkill{Name: "customs", Version: 1, Digest: "sha256:" + strings.Repeat("ab", 32), Definition: SkillDefinition{Instructions: "File customs.", Tools: tools, Confirmation: conf,
			Resources: []Resource{{Name: "codes", MediaType: "text/plain", Content: "HS 0101"}, {Name: "big", MediaType: "text/markdown", Content: "# big", OnDemand: true}}}}
	}
	base := func() *Document {
		return &Document{SchemaVersion: SchemaVersion, ReleaseID: "r", TenantID: "t", AgentID: "a", AgentName: "parcel", Version: 1, CreatedAt: "2026-11-04T00:00:00Z", CreatedBy: "alice",
			Definition: Definition{Instructions: "Help. {{skills}} Done.", Model: ModelProfile{Provider: "local", Model: "m"}, Tools: ToolPolicy{Gateway: "edge", Allow: []string{"get"}},
				Confirmation: ConfirmationPolicy{Default: ConfirmAuto, Tools: map[string]string{"file": ConfirmAuto}}, Skills: []SkillRef{{Name: "customs"}}},
			Skills: []EmbeddedSkill{skill([]string{"file", "get"}, map[string]string{"file": ConfirmConfirm})}}
	}
	// defaults: append, union, strictest; inline resource follows the skill, on-demand one does not
	d := base()
	d.Definition.Normalize()
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	eff := d.Effective()
	if !strings.HasSuffix(eff.Instructions, "## Skill: customs\n\nFile customs.\n\n### Resource: codes (text/plain)\n\nHS 0101") || strings.Contains(eff.Instructions, "# big") {
		t.Fatalf("%q", eff.Instructions)
	}
	if strings.Join(eff.Tools.Allow, ",") != "file,get" || eff.Confirmation.Tools["file"] != ConfirmConfirm {
		t.Fatalf("%v %v", eff.Tools.Allow, eff.Confirmation.Tools)
	}
	if res := d.OnDemandResources(); res["customs"]["big"].Content != "# big" || len(res["customs"]) != 1 {
		t.Fatalf("%v", res)
	}
	// placeholder + ceiling + agent
	d = base()
	d.Definition.SkillMerge = SkillMerge{Instructions: MergePlaceholder, Tools: MergeCeiling, Confirmation: MergeAgent}
	d.Definition.Normalize()
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "exceeds the agent's allow list") {
		t.Fatalf("ceiling must reject a skill tool outside the allow list: %v", err)
	}
	d.Skills[0].Definition.Tools = []string{"get"}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	eff = d.Effective()
	if !strings.HasPrefix(eff.Instructions, "Help. ## Skill: customs") || !strings.HasSuffix(eff.Instructions, "HS 0101 Done.") {
		t.Fatalf("%q", eff.Instructions)
	}
	if strings.Join(eff.Tools.Allow, ",") != "get" || eff.Confirmation.Tools["file"] != ConfirmAuto {
		t.Fatalf("%v %v", eff.Tools.Allow, eff.Confirmation.Tools)
	}
	// prepend
	d = base()
	d.Definition.Instructions = "Help."
	d.Definition.SkillMerge.Instructions = MergePrepend
	d.Definition.Normalize()
	if eff := d.Effective(); !strings.HasPrefix(eff.Instructions, "## Skill: customs") || !strings.HasSuffix(eff.Instructions, "\n\nHelp.") {
		t.Fatalf("%q", eff.Instructions)
	}
	// placeholder requires exactly one marker
	d = base()
	d.Definition.Instructions = "Help."
	d.Definition.SkillMerge.Instructions = MergePlaceholder
	d.Definition.Normalize()
	if err := d.Definition.Validate(); err == nil || !strings.Contains(err.Error(), "{{skills}}") {
		t.Fatalf("%v", err)
	}
}

func TestSubagentReferencesAndReservedTools(t *testing.T) {
	d := Definition{Instructions: "x", Model: ModelProfile{Provider: "p", Model: "m"}, Subagents: []SubagentRef{{Name: "helper", Description: "Arithmetic."}}}
	d.Normalize()
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	if d.Limits.MaxSubagentDepth != 1 || d.SkillMerge != (SkillMerge{MergeAppend, MergeUnion, MergeStrictest}) {
		t.Fatalf("%+v %+v", d.Limits, d.SkillMerge)
	}
	bad := []Definition{
		{Instructions: "x", Model: ModelProfile{Provider: "p", Model: "m"}, Subagents: []SubagentRef{{Name: "helper"}}},                                             // no description
		{Instructions: "x", Model: ModelProfile{Provider: "p", Model: "m"}, Subagents: []SubagentRef{{Name: "a", Description: "d"}, {Name: "a", Description: "d"}}}, // duplicate
		{Instructions: "x", Model: ModelProfile{Provider: "p", Model: "m"}, Subagents: []SubagentRef{{Name: strings.Repeat("a", 60), Description: "d"}}},            // tool name > 64
		{Instructions: "x", Model: ModelProfile{Provider: "p", Model: "m"}, Tools: ToolPolicy{Gateway: "g", Allow: []string{"agent__x"}}},                           // reserved
		{Instructions: "x", Model: ModelProfile{Provider: "p", Model: "m"}, Limits: Limits{MaxSubagentDepth: 9}},                                                    // depth
		{Instructions: "x", Model: ModelProfile{Provider: "p", Model: "m"}, SkillMerge: SkillMerge{Instructions: "mix"}},                                            // merge value
	}
	for i, b := range bad {
		b.Normalize()
		if err := b.Validate(); err == nil {
			t.Fatalf("bad[%d] accepted", i)
		}
	}
	doc := &Document{SchemaVersion: SchemaVersion, ReleaseID: "r", TenantID: "t", AgentID: "a", AgentName: "parent", Version: 1, CreatedAt: "2026-11-04T00:00:00Z", CreatedBy: "alice", Definition: d}
	if err := doc.Validate(); err == nil || !strings.Contains(err.Error(), "embedded exactly once") {
		t.Fatalf("%v", err)
	}
	doc.Subagents = []EmbeddedSubagent{{Name: "helper", AgentID: "a", Version: 1, Digest: "sha256:" + strings.Repeat("ab", 32)}}
	if err := doc.Validate(); err == nil || !strings.Contains(err.Error(), "itself") {
		t.Fatalf("%v", err)
	}
	doc.Subagents[0].AgentID = "b"
	if err := doc.Validate(); err != nil {
		t.Fatal(err)
	}
	env, err := Seal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(env); err != nil {
		t.Fatal(err)
	}
	sk := SkillDefinition{Instructions: "x", Tools: []string{"resource__read"}}
	if err := sk.Validate(); err == nil {
		t.Fatal("reserved skill tool accepted")
	}
	sk = SkillDefinition{Instructions: "x", Resources: []Resource{{Name: "a", MediaType: "application/json", Content: "{"}}}
	if err := sk.Validate(); err == nil || !strings.Contains(err.Error(), "not JSON") {
		t.Fatalf("%v", err)
	}
	sk = SkillDefinition{Instructions: "x", Resources: []Resource{{Name: "a", MediaType: "text/plain", Content: strings.Repeat("x", MaxResourceSize+1)}}}
	if err := sk.Validate(); err == nil {
		t.Fatal("oversized resource accepted")
	}
}

func TestEvaluationValidationAndJudge(t *testing.T) {
	ok := Evaluation{Required: true, Cases: []EvalCase{{Name: "greets", Input: "hi", Expect: Expectation{Contains: []string{"Hello"}, NoToolCall: true, MaxSteps: 1}}}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []Evaluation{
		{Required: true},
		{Cases: []EvalCase{{Name: "a b", Input: "x", Expect: Expectation{Contains: []string{"y"}}}}},
		{Cases: []EvalCase{{Name: "a", Input: "", Expect: Expectation{Contains: []string{"y"}}}}},
		{Cases: []EvalCase{{Name: "a", Input: "x", Expect: Expectation{}}}},
		{Cases: []EvalCase{{Name: "a", Input: "x", Expect: Expectation{Matches: "("}}}},
		{Cases: []EvalCase{{Name: "a", Input: "x", Expect: Expectation{ToolCalled: "sum", NoToolCall: true}}}},
		{Cases: []EvalCase{{Name: "a", Input: "x", Expect: Expectation{Contains: []string{"y"}}}, {Name: "a", Input: "x", Expect: Expectation{Contains: []string{"y"}}}}},
	}
	for i, b := range bad {
		if err := b.Validate(); err == nil {
			t.Fatalf("bad[%d] accepted", i)
		}
	}
	x := Expectation{Contains: []string{"hello"}, NotContains: []string{"sorry"}, Matches: `\d+`, ToolCalled: "sum", MaxSteps: 2}
	good := Outcome{Status: "COMPLETED", Answer: "Hello, the sum is 3", Tools: []string{"sum"}, Steps: 2}
	if pass, why := x.Judge(good); !pass {
		t.Fatal(why)
	}
	for name, o := range map[string]Outcome{
		"failed run":  {Status: "FAILED", FailureCode: "STEP_LIMIT", Answer: "Hello 3", Tools: []string{"sum"}, Steps: 1},
		"missing":     {Status: "COMPLETED", Answer: "the sum is 3", Tools: []string{"sum"}, Steps: 1},
		"forbidden":   {Status: "COMPLETED", Answer: "Hello, sorry, 3", Tools: []string{"sum"}, Steps: 1},
		"no match":    {Status: "COMPLETED", Answer: "Hello, three", Tools: []string{"sum"}, Steps: 1},
		"tool unused": {Status: "COMPLETED", Answer: "Hello 3", Tools: nil, Steps: 1},
		"too long":    {Status: "COMPLETED", Answer: "Hello 3", Tools: []string{"sum"}, Steps: 3},
	} {
		if pass, why := x.Judge(o); pass || why == "" {
			t.Fatalf("%s judged pass", name)
		}
	}
	if pass, why := (Expectation{NoToolCall: true}).Judge(Outcome{Status: "COMPLETED", Answer: "x", Tools: []string{"echo"}}); pass || !strings.Contains(why, "echo") {
		t.Fatalf("%v %s", pass, why)
	}
	// guards validation
	d := Definition{Instructions: "x", Model: ModelProfile{Provider: "p", Model: "m"}}
	d.Normalize()
	if d.Guards.Input != (InputGuard{ToolResults: "wrap", Context: "wrap"}) {
		t.Fatalf("%+v", d.Guards.Input)
	}
	for _, g := range []Guards{
		{Input: InputGuard{ToolResults: "sanitize", Context: "wrap"}},
		{Input: InputGuard{ToolResults: "wrap", Context: "wrap"}, Output: OutputGuard{Patterns: []string{"("}}},
		{Input: InputGuard{ToolResults: "wrap", Context: "wrap"}, Output: OutputGuard{Redact: []string{"ssn"}}},
		{Input: InputGuard{ToolResults: "wrap", Context: "wrap"}, Output: OutputGuard{OnError: "block"}},
		{Input: InputGuard{ToolResults: "wrap", Context: "wrap"}, Output: OutputGuard{Classifier: "Bad Name"}},
	} {
		d.Guards = g
		if err := d.Validate(); err == nil {
			t.Fatalf("guards %+v accepted", g)
		}
	}
}

// D44: a tool policy "matches" the external-tool patterns when any allowed name matches (path.Match syntax), a skill's
// tools count, and an allow list of "*" matches because the gateway's catalogue is not known when a run is created.
func TestToolPolicyMatchesExternalToolPatterns(t *testing.T) {
	web := []string{"web__*"}
	for name, c := range map[string]struct {
		allow    []string
		patterns []string
		want     bool
	}{
		"a tool of the family":      {[]string{"parcel__get", "web__web_search"}, web, true},
		"none of them":              {[]string{"parcel__get", "sum"}, web, false},
		"exact name":                {[]string{"web__web_fetch"}, []string{"web__web_fetch"}, true},
		"another tool of the same":  {[]string{"web__web_search"}, []string{"web__web_fetch"}, false},
		"allow all":                 {[]string{AllowAll}, web, true},
		"allow all, no patterns":    {[]string{AllowAll}, nil, false},
		"no tools":                  {nil, web, false},
		"a malformed pattern never": {[]string{"web__x"}, []string{"web__["}, false},
	} {
		if got := (ToolPolicy{Gateway: "edge", Allow: c.allow}).MatchesAny(c.patterns); got != c.want {
			t.Errorf("%s: %v", name, got)
		}
	}
	// the tools skills bring are part of the effective policy
	d := &Document{Definition: Definition{Tools: ToolPolicy{Gateway: "edge", Allow: []string{"get"}}, Skills: []SkillRef{{Name: "research"}}},
		Skills: []EmbeddedSkill{{Name: "research", Definition: SkillDefinition{Tools: []string{"web__web_search"}}}}}
	if !d.Effective().Tools.MatchesAny(web) || d.Definition.Tools.MatchesAny(web) {
		t.Fatal("a skill's tool must count, and only through the effective definition")
	}
	if !MatchesTool(web, "web__web_open_url") || MatchesTool(web, "webby") || MatchesTool(nil, "web__x") {
		t.Fatal("MatchesTool")
	}
}

// D65, D66: the knowledge tools and the rewrite are switches of the context policy, and the prefix of the tools is the HOST's.
func TestKnowledgeToolsAndRewriteAreSwitchesOfTheContextPolicy(t *testing.T) {
	with := func(c ContextPolicy) *Document {
		d := sample()
		d.Definition.Tools = ToolPolicy{}
		d.Definition.Context = c
		return d
	}
	for name, c := range map[string]ContextPolicy{
		"search only": {CMI: true, Tools: []string{KnowledgeSearchTool}},
		"all three":   {CMI: true, Tools: KnowledgeTools, Rewrite: RewriteModel},
		"rewrite off": {CMI: true, Rewrite: RewriteOff},
	} {
		if _, err := Seal(with(c)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, c := range map[string]ContextPolicy{
		"unknown tool":      {CMI: true, Tools: []string{"knowledge__delete"}},
		"a gateway's name":  {CMI: true, Tools: []string{"parcel__get_shipment"}},
		"repeated tool":     {CMI: true, Tools: []string{KnowledgeFindTool, KnowledgeFindTool}},
		"unknown rewrite":   {CMI: true, Rewrite: "history"},
		"tools without cmi": {Tools: []string{KnowledgeSearchTool}},
		"rewrite without":   {Rewrite: RewriteModel},
	} {
		if _, err := Seal(with(c)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// the prefix is the HOST's: an allow list may not name a knowledge tool, and the gateway's tools of that name are never offered
	for _, name := range KnowledgeTools {
		d := sample()
		d.Definition.Tools.Allow = []string{name}
		if _, err := Seal(d); err == nil || !Reserved(name) {
			t.Errorf("%s is reserved: %v", name, err)
		}
	}
	c := ContextPolicy{CMI: true, Tools: []string{KnowledgeReadTool}, Rewrite: RewriteModel}
	if !c.KnowledgeToolOn(KnowledgeReadTool) || c.KnowledgeToolOn(KnowledgeFindTool) || !c.HasKnowledgeTools() || !c.RewritesQuestion() {
		t.Errorf("%+v", c)
	}
	if (ContextPolicy{Tools: []string{KnowledgeReadTool}}).KnowledgeToolOn(KnowledgeReadTool) || (ContextPolicy{CMI: true}).RewritesQuestion() {
		t.Error("nothing is on without context.cmi, or without the switch")
	}
}
