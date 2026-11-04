// Package cmiclient is the HOST's client of the CMI's context API (ADR-0001 D32): at run creation
// it asks the CMI for a sealed ContextPackage for the caller's question and turns the package's
// items into caller context the model reads. The package id is recorded on the run so audit can
// answer "what did the model see". The CMI is idp-cmi (contract idp.telos.cmi 1.0.0).
package cmiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"idp-host/internal/agui"
)

// Client calls one CMI.
type Client struct {
	Endpoint string // https://cmi:8550
	HTTP     *http.Client
	Timeout  time.Duration // default 10 s
}

// Request mirrors the CMI's resolve request.
type Request struct {
	Query   string `json:"query"`
	Purpose string `json:"purpose,omitempty"`
	Scope   *struct {
		Kind string `json:"kind"`
		ID   string `json:"id,omitempty"`
	} `json:"scope,omitempty"`
	Budget *struct {
		MaxTokens int `json:"maxTokens,omitempty"`
		MaxItems  int `json:"maxItems,omitempty"`
	} `json:"budget,omitempty"`
	Kinds   []string `json:"kinds,omitempty"`
	Sources []string `json:"sources,omitempty"`
}

// Item is one package item as the HOST reads it.
type Item struct {
	Kind        string    `json:"kind"`
	ID          string    `json:"id"`
	Source      string    `json:"source"`
	Trust       string    `json:"trust"`
	Ref         string    `json:"ref"`
	Title       string    `json:"title"`
	Excerpt     string    `json:"excerpt"`
	ObservedAt  time.Time `json:"observedAt"`
	ValidUntil  time.Time `json:"validUntil"`
	Sensitivity string    `json:"sensitivity"`
	Tokens      int       `json:"tokens"`
}

// Package is the resolved package.
type Package struct {
	PackageID string
	Digest    string
	Items     []Item
	Tokens    int
}

// ErrCMI marks a CMI failure (unreachable, refused, malformed).
var ErrCMI = errors.New("cmi")

// Resolve asks the CMI for a package with the given bearer.
func (c *Client) Resolve(ctx context.Context, bearer string, req Request) (*Package, error) {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, _ := json.Marshal(req)
	hr, err := http.NewRequestWithContext(cctx, http.MethodPost, c.Endpoint+"/v1/context/resolve", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCMI, err)
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "application/json")
	hr.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := c.HTTP.Do(hr)
	if err != nil {
		return nil, fmt.Errorf("%w unreachable: %v", ErrCMI, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w answered HTTP %d", ErrCMI, resp.StatusCode)
	}
	var out struct {
		PackageID string `json:"packageId"`
		Digest    string `json:"digest"`
		Package   struct {
			Items  []Item `json:"items"`
			Budget struct {
				UsedTokens int `json:"usedTokens"`
			} `json:"budget"`
		} `json:"package"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.PackageID == "" || out.Digest == "" {
		return nil, fmt.Errorf("%w answered no package", ErrCMI)
	}
	return &Package{PackageID: out.PackageID, Digest: out.Digest, Items: out.Package.Items, Tokens: out.Package.Budget.UsedTokens}, nil
}

// ContextOf turns a package into caller context: one entry per item, described so the model knows
// what it is reading - the kind, the source and its trust, when it was observed - and labelled with
// the package it came from.
func ContextOf(p *Package) []agui.Context {
	out := make([]agui.Context, 0, len(p.Items))
	for _, it := range p.Items {
		desc := fmt.Sprintf("CMI %s from %s (%s, observed %s, ref %s)", it.Kind, it.Source, it.Trust, it.ObservedAt.UTC().Format(time.RFC3339), it.Ref)
		if it.Title != "" {
			desc += " — " + it.Title
		}
		out = append(out, agui.Context{Description: desc, Value: it.Excerpt})
	}
	return out
}
