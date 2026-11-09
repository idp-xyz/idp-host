// Package config reads the HOST's providers and gateways file (ADR-0001 D8, D9): the model
// endpoints the harness may call and the MCP gateways agents may use, with their credentials
// referenced as files so secrets never sit in the document itself.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// File is the document.
type File struct {
	Providers   []Provider   `yaml:"providers"`
	Gateways    []Gateway    `yaml:"gateways"`
	Classifiers []Classifier `yaml:"classifiers"` // external output classifiers (ADR-0001 D28)
	A2APeers    []A2APeer    `yaml:"a2aPeers"`    // remote A2A agents this HOST may delegate to (ADR-0001 D30)
	CMI         *CMI         `yaml:"cmi"`         // the context infrastructure runs may draw context from (ADR-0001 D32)
}

// CMI is the idp-cmi endpoint and how the HOST authenticates to it on the caller's behalf: the same
// two modes as a gateway (delegation = RFC 8693 token exchange for the CMI audience; static = one
// cmi-reader credential for development).
type CMI struct {
	Endpoint string      `yaml:"endpoint"`
	CAFile   string      `yaml:"caFile"`
	Auth     GatewayAuth `yaml:"auth"`
	// Remember makes the HOST append an episodic memory to the CMI when a run of an agent that uses
	// CMI context completes (ADR-0001 D33). Optional; off unless enabled.
	Remember *Remember `yaml:"remember"`
}

// Remember configures the memory the HOST writes after a completed run (D33). Auth is the WRITER
// identity (the CMI role cmi-writer), distinct from the reader identity in cmi.auth; with static
// auth the key file is read again on every write so a refreshed token is picked up.
type Remember struct {
	Enabled     bool        `yaml:"enabled"`
	Source      string      `yaml:"source"`      // the registered CMI source; default idp-host
	TTLDays     int         `yaml:"ttlDays"`     // validUntil = completion + ttl; default 30, 1..3650
	Sensitivity string      `yaml:"sensitivity"` // public | internal | confidential | restricted; default internal
	MaxChars    int         `yaml:"maxChars"`    // runes kept of the user message and of the answer, each; default 1500, 1..8000
	Auth        GatewayAuth `yaml:"auth"`
	// Mode is what a finished run writes (ADR D34): turns (default) = one episodic record per run; facts = the facts the
	// user stated, extracted by a model and written through POST /v1/memory/facts (the assistant's answer is not stored);
	// both = the two.
	Mode  string         `yaml:"mode"`
	Facts *RememberFacts `yaml:"facts"` // required for facts and both
}

// RememberFacts names the model that extracts facts from the user's message: a provider of this configuration.
type RememberFacts struct {
	Provider string        `yaml:"provider"`
	Model    string        `yaml:"model"`
	MaxFacts int           `yaml:"maxFacts"` // per message; default 5, 1..20
	Timeout  time.Duration `yaml:"timeout"`  // default 20s
}

// A2APeer is the credential for remote agent cards under a URL prefix.
type A2APeer struct {
	Name       string `yaml:"name"`
	URLPrefix  string `yaml:"urlPrefix"` // every card URL starting with it uses this credential
	APIKey     string `yaml:"apiKey"`
	APIKeyFile string `yaml:"apiKeyFile"`
	CAFile     string `yaml:"caFile"`
}

// Classifier is an external content classifier a release's guards.output.classifier may name:
// POST {"text": ...} answered with {"allow": bool, "categories": [...]}.
type Classifier struct {
	Name           string `yaml:"name"`
	Endpoint       string `yaml:"endpoint"`
	APIKey         string `yaml:"apiKey"`     // development only
	APIKeyFile     string `yaml:"apiKeyFile"` // production
	CAFile         string `yaml:"caFile"`
	TimeoutSeconds int    `yaml:"timeoutSeconds"`
	Insecure       bool   `yaml:"insecure"` // plain http to a non-loopback host; development mode only
}

// Provider is one model endpoint.
type Provider struct {
	Name           string `yaml:"name"`
	Kind           string `yaml:"kind"` // openai | anthropic | gemini | harness-worker (ADR-0001 D31)
	BaseURL        string `yaml:"baseUrl"`
	APIKey         string `yaml:"apiKey"`     // development only
	APIKeyFile     string `yaml:"apiKeyFile"` // production
	UsageInStream  *bool  `yaml:"usageInStream"`
	MaxTokensField string `yaml:"maxTokensField"`
	CAFile         string `yaml:"caFile"`
	// Insecure allows plain http to a non-loopback host; honored only in development mode.
	Insecure bool `yaml:"insecure"`
}

// Gateway is one MCP Infrastructure gateway.
type Gateway struct {
	Name     string      `yaml:"name"`
	Endpoint string      `yaml:"endpoint"`
	CAFile   string      `yaml:"caFile"`
	Auth     GatewayAuth `yaml:"auth"`
}

// GatewayAuth is how the HOST authenticates to the gateway on the user's behalf.
type GatewayAuth struct {
	Mode             string `yaml:"mode"` // delegation | static
	TokenEndpoint    string `yaml:"tokenEndpoint"`
	ClientID         string `yaml:"clientId"`
	ClientSecret     string `yaml:"clientSecret"`
	ClientSecretFile string `yaml:"clientSecretFile"`
	Audience         string `yaml:"audience"`
	ActorToken       bool   `yaml:"actorToken"`
	APIKey           string `yaml:"apiKey"`
	APIKeyFile       string `yaml:"apiKeyFile"`
	CAFile           string `yaml:"caFile"` // for the token endpoint
}

var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// Load reads and validates the file; secrets referenced by file are read into the Secret fields.
func Load(path string) (*File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f File
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, errEOF(err)) {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	seen := map[string]bool{}
	for i := range f.Providers {
		p := &f.Providers[i]
		if !nameRE.MatchString(p.Name) || seen["p:"+p.Name] {
			return nil, fmt.Errorf("providers[%d]: name %q invalid or duplicated", i, p.Name)
		}
		seen["p:"+p.Name] = true
		if p.Kind != "openai" && p.Kind != "anthropic" && p.Kind != "gemini" && p.Kind != "harness-worker" {
			return nil, fmt.Errorf("provider %s: kind must be openai, anthropic, gemini or harness-worker", p.Name)
		}
		if p.BaseURL == "" {
			return nil, fmt.Errorf("provider %s: baseUrl is required", p.Name)
		}
		if p.APIKey, err = secret(p.APIKey, p.APIKeyFile); err != nil {
			return nil, fmt.Errorf("provider %s: %w", p.Name, err)
		}
	}
	for i := range f.A2APeers {
		p := &f.A2APeers[i]
		if !nameRE.MatchString(p.Name) || seen["a:"+p.Name] {
			return nil, fmt.Errorf("a2aPeers[%d]: name %q invalid or duplicated", i, p.Name)
		}
		seen["a:"+p.Name] = true
		if !strings.HasPrefix(p.URLPrefix, "https://") && !strings.HasPrefix(p.URLPrefix, "http://") {
			return nil, fmt.Errorf("a2a peer %s: urlPrefix must be an http(s) URL", p.Name)
		}
		if p.APIKey, err = secret(p.APIKey, p.APIKeyFile); err != nil {
			return nil, fmt.Errorf("a2a peer %s: %w", p.Name, err)
		}
	}
	for i := range f.Classifiers {
		c := &f.Classifiers[i]
		if !nameRE.MatchString(c.Name) || seen["c:"+c.Name] {
			return nil, fmt.Errorf("classifiers[%d]: name %q invalid or duplicated", i, c.Name)
		}
		seen["c:"+c.Name] = true
		if c.Endpoint == "" {
			return nil, fmt.Errorf("classifier %s: endpoint is required", c.Name)
		}
		if c.TimeoutSeconds < 0 || c.TimeoutSeconds > 60 {
			return nil, fmt.Errorf("classifier %s: timeoutSeconds must be within [0, 60]", c.Name)
		}
		if c.APIKey, err = secret(c.APIKey, c.APIKeyFile); err != nil {
			return nil, fmt.Errorf("classifier %s: %w", c.Name, err)
		}
	}
	if c := f.CMI; c != nil {
		if c.Endpoint == "" {
			return nil, errors.New("cmi: endpoint is required")
		}
		switch c.Auth.Mode {
		case "delegation":
			if c.Auth.TokenEndpoint == "" || c.Auth.ClientID == "" {
				return nil, errors.New("cmi: delegation needs tokenEndpoint and clientId")
			}
			if c.Auth.ClientSecret, err = secret(c.Auth.ClientSecret, c.Auth.ClientSecretFile); err != nil {
				return nil, fmt.Errorf("cmi: %w", err)
			}
			if c.Auth.ClientSecret == "" {
				return nil, errors.New("cmi: delegation needs clientSecret[File]")
			}
		case "static":
			if c.Auth.APIKey, err = secret(c.Auth.APIKey, c.Auth.APIKeyFile); err != nil {
				return nil, fmt.Errorf("cmi: %w", err)
			}
			if c.Auth.APIKey == "" {
				return nil, errors.New("cmi: static auth needs apiKey[File]")
			}
		default:
			return nil, errors.New("cmi: auth.mode must be delegation or static")
		}
		if r := c.Remember; r != nil {
			if r.Source == "" {
				r.Source = "idp-host"
			}
			if !nameRE.MatchString(r.Source) {
				return nil, errors.New("cmi.remember: source must be a CMI source name (lower-case letters, digits, hyphens)")
			}
			if r.TTLDays == 0 {
				r.TTLDays = 30
			}
			if r.TTLDays < 1 || r.TTLDays > 3650 {
				return nil, errors.New("cmi.remember: ttlDays must be within [1, 3650]")
			}
			if r.Sensitivity == "" {
				r.Sensitivity = "internal"
			}
			switch r.Sensitivity {
			case "public", "internal", "confidential", "restricted":
			default:
				return nil, errors.New("cmi.remember: sensitivity must be public, internal, confidential or restricted")
			}
			if r.MaxChars == 0 {
				r.MaxChars = 1500
			}
			if r.MaxChars < 1 || r.MaxChars > 8000 {
				return nil, errors.New("cmi.remember: maxChars must be within [1, 8000]")
			}
			switch r.Mode {
			case "", "turns":
				r.Mode = "turns"
			case "facts", "both":
				f := r.Facts
				if f == nil || f.Provider == "" || f.Model == "" {
					return nil, errors.New("cmi.remember: mode " + r.Mode + " needs facts.provider and facts.model")
				}
				if f.MaxFacts == 0 {
					f.MaxFacts = 5
				}
				if f.MaxFacts < 1 || f.MaxFacts > 20 {
					return nil, errors.New("cmi.remember: facts.maxFacts must be within [1, 20]")
				}
				if f.Timeout == 0 {
					f.Timeout = 20 * time.Second
				}
			default:
				return nil, errors.New("cmi.remember: mode must be turns, facts or both")
			}
			if r.Enabled {
				a := &r.Auth
				switch a.Mode {
				case "":
					return nil, errors.New("cmi.remember: auth (the writer identity) is required when enabled")
				case "delegation":
					if a.TokenEndpoint == "" || a.ClientID == "" {
						return nil, errors.New("cmi.remember: delegation needs tokenEndpoint and clientId")
					}
					if a.ClientSecret, err = secret(a.ClientSecret, a.ClientSecretFile); err != nil {
						return nil, fmt.Errorf("cmi.remember: %w", err)
					}
					if a.ClientSecret == "" {
						return nil, errors.New("cmi.remember: delegation needs clientSecret[File]")
					}
				case "static":
					if a.APIKey, err = secret(a.APIKey, a.APIKeyFile); err != nil {
						return nil, fmt.Errorf("cmi.remember: %w", err)
					}
					if a.APIKey == "" {
						return nil, errors.New("cmi.remember: static auth needs apiKey[File]")
					}
				default:
					return nil, errors.New("cmi.remember: auth.mode must be delegation or static")
				}
			}
		}
	}
	for i := range f.Gateways {
		g := &f.Gateways[i]
		if !nameRE.MatchString(g.Name) || seen["g:"+g.Name] {
			return nil, fmt.Errorf("gateways[%d]: name %q invalid or duplicated", i, g.Name)
		}
		seen["g:"+g.Name] = true
		if g.Endpoint == "" {
			return nil, fmt.Errorf("gateway %s: endpoint is required", g.Name)
		}
		switch g.Auth.Mode {
		case "delegation":
			if g.Auth.TokenEndpoint == "" || g.Auth.ClientID == "" {
				return nil, fmt.Errorf("gateway %s: delegation needs tokenEndpoint and clientId", g.Name)
			}
			if g.Auth.ClientSecret, err = secret(g.Auth.ClientSecret, g.Auth.ClientSecretFile); err != nil {
				return nil, fmt.Errorf("gateway %s: %w", g.Name, err)
			}
			if g.Auth.ClientSecret == "" {
				return nil, fmt.Errorf("gateway %s: delegation needs clientSecret[File]", g.Name)
			}
		case "static":
			if g.Auth.APIKey, err = secret(g.Auth.APIKey, g.Auth.APIKeyFile); err != nil {
				return nil, fmt.Errorf("gateway %s: %w", g.Name, err)
			}
			if g.Auth.APIKey == "" {
				return nil, fmt.Errorf("gateway %s: static auth needs apiKey[File]", g.Name)
			}
		default:
			return nil, fmt.Errorf("gateway %s: auth.mode must be delegation or static", g.Name)
		}
	}
	return &f, nil
}

func errEOF(err error) error {
	if err != nil && err.Error() == "EOF" {
		return err
	}
	return errors.New("not eof")
}

func secret(inline, file string) (string, error) {
	if inline != "" && file != "" {
		return "", errors.New("give either the inline value or the file, not both")
	}
	if file == "" {
		return inline, nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
