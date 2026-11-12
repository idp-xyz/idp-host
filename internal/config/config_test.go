package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
