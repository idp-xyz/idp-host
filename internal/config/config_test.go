package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func load(t *testing.T, yaml string) (*File, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "host.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

const cmiHead = `
cmi:
  endpoint: http://cmi:8550
  auth: {mode: static, apiKey: reader}
`

func TestRememberDefaults(t *testing.T) {
	key := filepath.Join(t.TempDir(), "writer.key")
	if err := os.WriteFile(key, []byte("writer-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := load(t, cmiHead+"  remember:\n    enabled: true\n    auth: {mode: static, apiKeyFile: "+key+"}\n")
	if err != nil {
		t.Fatal(err)
	}
	r := f.CMI.Remember
	if !r.Enabled || r.Source != "idp-host" || r.TTLDays != 30 || r.Sensitivity != "internal" || r.MaxChars != 1500 || r.Auth.APIKey != "writer-token" || r.Auth.APIKeyFile != key {
		t.Fatalf("%+v", r)
	}
}

func TestRememberExplicitAndDelegation(t *testing.T) {
	f, err := load(t, cmiHead+`  remember:
    enabled: true
    source: assistant-host
    ttlDays: 3650
    sensitivity: confidential
    maxChars: 8000
    auth: {mode: delegation, tokenEndpoint: "https://kc/token", clientId: idp-host, clientSecret: s, audience: idp-cmi, actorToken: true}
`)
	if err != nil {
		t.Fatal(err)
	}
	r := f.CMI.Remember
	if r.Source != "assistant-host" || r.TTLDays != 3650 || r.Sensitivity != "confidential" || r.MaxChars != 8000 || r.Auth.Mode != "delegation" || !r.Auth.ActorToken {
		t.Fatalf("%+v", r)
	}
}

// D36: the HOST's own credential for the CMI, which it mints and refreshes - so that a credential
// configured once keeps working, unlike a token handed over at deployment time.
func TestClientCredentialsForTheCMIAndTheWriter(t *testing.T) {
	secretFile := filepath.Join(t.TempDir(), "svc.secret")
	if err := os.WriteFile(secretFile, []byte("svc-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := load(t, `
cmi:
  endpoint: http://cmi:8550
  auth: {mode: client_credentials, tokenEndpoint: "https://kc/token", clientId: idp-host-svc, clientSecretFile: `+secretFile+`, audience: idp-cmi}
  remember:
    enabled: true
    auth: {mode: client_credentials, tokenEndpoint: "https://kc/token", clientId: idp-host-svc, clientSecret: s, audience: idp-cmi}
`)
	if err != nil {
		t.Fatal(err)
	}
	if f.CMI.Auth.Mode != "client_credentials" || f.CMI.Auth.ClientSecret != "svc-secret" || f.CMI.Auth.Audience != "idp-cmi" {
		t.Fatalf("%+v", f.CMI.Auth)
	}
	if r := f.CMI.Remember.Auth; r.Mode != "client_credentials" || r.ClientID != "idp-host-svc" || r.ClientSecret != "s" || r.Audience != "idp-cmi" {
		t.Fatalf("%+v", r)
	}
}

func TestClientCredentialsRejected(t *testing.T) {
	head := "\ncmi:\n  endpoint: http://cmi:8550\n  auth: "
	cases := map[string]struct{ body, want string }{
		"no endpoint": {"{mode: client_credentials, clientId: c, clientSecret: s, audience: a}", "tokenEndpoint"},
		"no clientId": {"{mode: client_credentials, tokenEndpoint: \"https://kc\", clientSecret: s, audience: a}", "clientId"},
		"no audience": {"{mode: client_credentials, tokenEndpoint: \"https://kc\", clientId: c, clientSecret: s}", "audience"},
		"no secret":   {"{mode: client_credentials, tokenEndpoint: \"https://kc\", clientId: c, audience: a}", "clientSecret"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := load(t, head+c.body+"\n"); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
		})
	}
	// and the same for the writer
	if _, err := load(t, cmiHead+"  remember:\n    enabled: true\n    auth: {mode: client_credentials, clientId: c, clientSecret: s, audience: a}\n"); err == nil || !strings.Contains(err.Error(), "tokenEndpoint") {
		t.Fatalf("writer: %v", err)
	}
}

func TestRememberAbsentOrDisabledNeedsNoAuth(t *testing.T) {
	if f, err := load(t, cmiHead); err != nil || f.CMI.Remember != nil {
		t.Fatal(err)
	}
	if _, err := load(t, cmiHead+"  remember: {enabled: false}\n"); err != nil {
		t.Fatal(err)
	}
}

func TestRememberRejected(t *testing.T) {
	auth := "    auth: {mode: static, apiKey: w}\n"
	cases := map[string]struct{ body, want string }{
		"missing auth":         {"    enabled: true\n", "auth"},
		"unknown mode":         {"    enabled: true\n    auth: {mode: basic}\n", "auth.mode"},
		"static without key":   {"    enabled: true\n    auth: {mode: static}\n", "apiKey"},
		"missing key file":     {"    enabled: true\n    auth: {mode: static, apiKeyFile: /nonexistent/w.key}\n", "cmi.remember"},
		"delegation no id":     {"    enabled: true\n    auth: {mode: delegation, clientSecret: s}\n", "tokenEndpoint"},
		"delegation no secret": {"    enabled: true\n    auth: {mode: delegation, tokenEndpoint: \"https://kc\", clientId: c}\n", "clientSecret"},
		"ttl zero-negative":    {"    enabled: true\n    ttlDays: -1\n" + auth, "ttlDays"},
		"ttl too long":         {"    enabled: true\n    ttlDays: 3651\n" + auth, "ttlDays"},
		"bad sensitivity":      {"    enabled: true\n    sensitivity: secret\n" + auth, "sensitivity"},
		"maxChars negative":    {"    enabled: true\n    maxChars: -5\n" + auth, "maxChars"},
		"maxChars too large":   {"    enabled: true\n    maxChars: 8001\n" + auth, "maxChars"},
		"bad source":           {"    enabled: true\n    source: Idp Host\n" + auth, "source"},
		"bad ttl when off":     {"    enabled: false\n    ttlDays: 99999\n", "ttlDays"},
		"unknown key":          {"    enabled: true\n    ttl: 3\n" + auth, "ttl"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := load(t, cmiHead+"  remember:\n"+c.body)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error naming %q, got %v", c.want, err)
			}
		})
	}
}

func TestRememberMode(t *testing.T) {
	base := cmiHead + "  remember:\n    enabled: true\n    auth: {mode: static, apiKey: w}\n"
	for name, tc := range map[string]struct {
		extra string
		ok    bool
	}{
		"default":        {"", true},
		"turns":          {"    mode: turns\n", true},
		"facts":          {"    mode: facts\n    facts: {provider: local, model: m}\n", true},
		"both":           {"    mode: both\n    facts: {provider: local, model: m, maxFacts: 3, timeout: 10s}\n", true},
		"facts-no-model": {"    mode: facts\n", false},
		"facts-no-name":  {"    mode: facts\n    facts: {provider: local}\n", false},
		"too-many":       {"    mode: facts\n    facts: {provider: local, model: m, maxFacts: 99}\n", false},
		"unknown":        {"    mode: all\n", false},
	} {
		f, err := load(t, base+tc.extra)
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v", name, err)
		}
		if err == nil && name == "default" && f.CMI.Remember.Mode != "turns" {
			t.Errorf("the default mode is turns: %q", f.CMI.Remember.Mode)
		}
		if err == nil && name == "both" && (f.CMI.Remember.Facts.MaxFacts != 3 || f.CMI.Remember.Facts.Timeout.Seconds() != 10) {
			t.Errorf("%+v", f.CMI.Remember.Facts)
		}
	}
}

// D43: a provider is outside the enterprise unless the operator says otherwise, and the ceiling on what may reach one
// is internal unless set, for all tenants or for one.
func TestProvidersAreExternalUnlessDeclaredAndTheFileCeiling(t *testing.T) {
	f, err := load(t, `
providers:
  - {name: cloud, kind: openai, baseUrl: "https://api.example/v1", apiKey: k}
  - {name: onprem, kind: openai, baseUrl: "https://llm.corp.example/v1", apiKey: k, external: false}
  - {name: stated, kind: openai, baseUrl: "https://api2.example/v1", apiKey: k, external: true}
`)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Providers[0].IsExternal() || f.Providers[1].IsExternal() || !f.Providers[2].IsExternal() {
		t.Fatalf("%+v", f.Providers)
	}
	if f.Files.Ceiling("any") != "internal" {
		t.Fatalf("the default ceiling is %q", f.Files.Ceiling("any"))
	}
	var none *Files
	if none.Ceiling("any") != "internal" {
		t.Fatal("a host without a configuration file has no ceiling")
	}
	f, err = load(t, `
files:
  externalModelCeiling: public
  tenants: {"7a1b": confidential}
`)
	if err != nil || f.Files.Ceiling("7a1b") != "confidential" || f.Files.Ceiling("other") != "public" {
		t.Fatalf("%+v %v", f.Files, err)
	}
	for _, bad := range []string{"files: {externalModelCeiling: secret}", `files: {tenants: {"7a1b": top}}`, `files: {tenants: {"": internal}}`} {
		if _, err := load(t, bad); err == nil || !strings.Contains(err.Error(), "files:") {
			t.Errorf("%s: %v", bad, err)
		}
	}
}

// D44: gateways name the tools that send content outside the enterprise and the tools that always need the member's
// confirmation; the patterns are checked when the file is loaded, not when a run first meets them.
func TestGatewayToolPatternsAreValidatedAtLoad(t *testing.T) {
	gw := func(extra string) string {
		return "gateways:\n  - {name: web, endpoint: \"https://gw/mcp\", auth: {mode: static, apiKey: k}" + extra + "}\n"
	}
	f, err := load(t, gw(`, externalTools: ["web__*"], confirmTools: ["web__web_open_url"]`))
	if err != nil || len(f.Gateways[0].ExternalTools) != 1 || f.Gateways[0].ConfirmTools[0] != "web__web_open_url" {
		t.Fatalf("%+v %v", f.Gateways, err)
	}
	if f, err = load(t, gw("")); err != nil || f.Gateways[0].ExternalTools != nil {
		t.Fatalf("both are optional: %v", err)
	}
	for _, bad := range []string{`externalTools: ["web__["]`, `confirmTools: ["["]`, `externalTools: [""]`} {
		if _, err := load(t, gw(", "+bad)); err == nil || !strings.Contains(err.Error(), "gateway web") || !strings.Contains(err.Error(), "pattern") {
			t.Errorf("%s: %v", bad, err)
		}
	}
	if _, err := load(t, gw(`, externalTool: ["web__*"]`)); err == nil {
		t.Error("a misspelt key must be refused, not ignored")
	}
}

// D47: neverResendTools takes the same patterns as externalTools and confirmTools, validated the same way, and is optional.
func TestNeverResendToolsIsReadAndValidatedAtLoad(t *testing.T) {
	gw := func(extra string) string {
		return "gateways:\n  - {name: web, endpoint: \"https://gw/mcp\", auth: {mode: static, apiKey: k}" + extra + "}\n"
	}
	f, err := load(t, gw(`, neverResendTools: ["sandbox_*", "web__send"]`))
	if err != nil || len(f.Gateways[0].NeverResendTools) != 2 || f.Gateways[0].NeverResendTools[1] != "web__send" {
		t.Fatalf("%+v %v", f.Gateways, err)
	}
	if f, err = load(t, gw("")); err != nil || f.Gateways[0].NeverResendTools != nil {
		t.Fatalf("optional: %v", err)
	}
	for _, bad := range []string{`neverResendTools: ["["]`, `neverResendTools: [""]`} {
		if _, err := load(t, gw(", "+bad)); err == nil || !strings.Contains(err.Error(), "neverResendTools") || !strings.Contains(err.Error(), "pattern") {
			t.Errorf("%s: %v", bad, err)
		}
	}
}

// D45, D46: a provider that sets nothing gets the defaults, one that sets the retry and idle settings gets them as the
// harness and the adapters read them, and a setting that makes no sense is refused when the file is loaded.
func TestProviderRetryAndIdleSettings(t *testing.T) {
	f, err := load(t, "providers:\n  - {name: plain, kind: openai, baseUrl: http://127.0.0.1:1/v1}\n"+
		"  - {name: tuned, kind: openai, baseUrl: http://127.0.0.1:1/v1, idleTimeoutSeconds: 90, retry: {attempts: 5, backoffSeconds: 0.5, maxBackoffSeconds: 10, maxRetryAfterSeconds: 120}}\n")
	if err != nil {
		t.Fatal(err)
	}
	if p := f.Providers[0].RetryPolicy().WithDefaults(); p.Attempts != 3 || p.Backoff != time.Second || p.MaxBackoff != 20*time.Second || p.MaxRetryAfter != time.Minute || f.Providers[0].IdleTimeout() != 0 {
		t.Errorf("defaults %+v idle %v", p, f.Providers[0].IdleTimeout())
	}
	if p := f.Providers[1].RetryPolicy(); p.Attempts != 5 || p.Backoff != 500*time.Millisecond || p.MaxBackoff != 10*time.Second || p.MaxRetryAfter != 2*time.Minute || f.Providers[1].IdleTimeout() != 90*time.Second {
		t.Errorf("tuned %+v idle %v", p, f.Providers[1].IdleTimeout())
	}
	for name, setting := range map[string]string{
		"more attempts than allowed": "retry: {attempts: 11}",
		"negative attempts":          "retry: {attempts: -1}",
		"negative backoff":           "retry: {backoffSeconds: -1}",
		"backoff above its cap":      "retry: {backoffSeconds: 30, maxBackoffSeconds: 10}",
		"Retry-After cap too large":  "retry: {maxRetryAfterSeconds: 601}",
		"negative idle timeout":      "idleTimeoutSeconds: -1",
		"idle timeout too large":     "idleTimeoutSeconds: 4000",
	} {
		if _, err := load(t, "providers:\n  - {name: p, kind: openai, baseUrl: http://127.0.0.1:1/v1, "+setting+"}\n"); err == nil || !strings.Contains(err.Error(), "provider p") {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
	if _, err := load(t, "providers:\n  - {name: p, kind: openai, baseUrl: http://127.0.0.1:1/v1, retry: {attemps: 2}}\n"); err == nil {
		t.Error("a misspelled retry setting must be refused, not ignored")
	}
}

// D57: whether a provider is sent the trace context: what the operator says, else only a provider that is the
// enterprise's own (external: false). A provider outside the enterprise is not, by default.
func TestProviderTracePropagation(t *testing.T) {
	f, err := load(t, "providers:\n"+
		"  - {name: undeclared, kind: openai, baseUrl: http://127.0.0.1:1/v1}\n"+
		"  - {name: external, kind: openai, baseUrl: http://127.0.0.1:1/v1, external: true}\n"+
		"  - {name: ours, kind: openai, baseUrl: http://127.0.0.1:1/v1, external: false}\n"+
		"  - {name: external-allowed, kind: openai, baseUrl: http://127.0.0.1:1/v1, external: true, tracePropagation: true}\n"+
		"  - {name: ours-silent, kind: openai, baseUrl: http://127.0.0.1:1/v1, external: false, tracePropagation: false}\n")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"undeclared": false, "external": false, "ours": true, "external-allowed": true, "ours-silent": false}
	for _, p := range f.Providers {
		if p.PropagatesTrace() != want[p.Name] {
			t.Errorf("%s: PropagatesTrace %v, want %v", p.Name, p.PropagatesTrace(), want[p.Name])
		}
	}
}
