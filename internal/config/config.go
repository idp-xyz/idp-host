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

	"gopkg.in/yaml.v3"
)

// File is the document.
type File struct {
	Providers   []Provider   `yaml:"providers"`
	Gateways    []Gateway    `yaml:"gateways"`
	Classifiers []Classifier `yaml:"classifiers"` // external output classifiers (ADR-0001 D28)
	A2APeers    []A2APeer    `yaml:"a2aPeers"`    // remote A2A agents this HOST may delegate to (ADR-0001 D30)
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
