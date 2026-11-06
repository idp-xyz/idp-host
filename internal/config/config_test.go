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
