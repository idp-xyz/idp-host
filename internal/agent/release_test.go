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
