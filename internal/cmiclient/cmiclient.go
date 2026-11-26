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
	"idp-host/internal/run"
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
	// KB8 (idp.telos.cmi 1.7.0): the knowledge tools narrow a resolve
	Entities           []Entity `json:"entities,omitempty"`
	AsOf               *AsOf    `json:"asOf,omitempty"`
	KnowledgeBases     []string `json:"knowledgeBases,omitempty"`
	DocTypes           []string `json:"docTypes,omitempty"`
	SensitivityCeiling string   `json:"sensitivityCeiling,omitempty"`
}

// Entity names a business object (idp.telos.cmi Entity).
type Entity struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// AsOf is the bitemporal view of a request (idp.telos.cmi AsOf).
type AsOf struct {
	Valid    *time.Time `json:"valid,omitempty"`
	Recorded *time.Time `json:"recorded,omitempty"`
}

// Locator says where in a document a chunk sits (idp.telos.cmi ContextItem.locator); on a read request it names the place to read.
type Locator struct {
	Headings []string `json:"headings,omitempty"`
	Page     int      `json:"page,omitempty"`
	Slide    int      `json:"slide,omitempty"`
	Sheet    string   `json:"sheet,omitempty"`
	Range    string   `json:"range,omitempty"`
	Table    string   `json:"table,omitempty"`
}

// Hit is where the searched text sits in an item of a find package (offsets relative to the chunk's text).
type Hit struct {
	Offset int `json:"offset"`
	Length int `json:"length"`
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
	Match       string    `json:"match,omitempty"`    // lexical | semantic | both | exact (find)
	Locator     *Locator  `json:"locator,omitempty"`  // where a knowledge chunk sits in its document
	Hits        []Hit     `json:"hits,omitempty"`     // find: where the text is in the chunk, only those inside the excerpt
	HitCount    int       `json:"hitCount,omitempty"` // find: how many hits the chunk holds in all
}

// Package is the resolved package.
type Package struct {
	PackageID string
	Digest    string
	Items     []Item
	Tokens    int
	Purpose   string
	Omitted   map[string]int // what matched but was not packed, by reason (budget, flagged, ...)
}

// ErrCMI marks a CMI failure (unreachable, refused, malformed).
var ErrCMI = errors.New("cmi")

// StatusError is a CMI answer that was not a success. Its text is the status only: nothing the CMI wrote is repeated.
type StatusError struct{ Status int }

func (e *StatusError) Error() string { return fmt.Sprintf("cmi answered HTTP %d", e.Status) }
func (e *StatusError) Unwrap() error { return ErrCMI }

// Status returns the HTTP status of a CMI refusal, or 0 for any other error.
func Status(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Status
	}
	return 0
}

// Resolve asks the CMI for a package with the given bearer.
func (c *Client) Resolve(ctx context.Context, bearer string, req Request) (*Package, error) {
	return c.post(ctx, bearer, "/v1/context/resolve", req)
}

// post sends a request to one of the CMI's package endpoints (resolve, read, find: the answer has the same shape).
func (c *Client) post(ctx context.Context, bearer, path string, req any) (*Package, error) {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, _ := json.Marshal(req)
	hr, err := http.NewRequestWithContext(cctx, http.MethodPost, c.Endpoint+path, bytes.NewReader(body))
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
		return nil, &StatusError{Status: resp.StatusCode}
	}
	var out struct {
		PackageID string `json:"packageId"`
		Digest    string `json:"digest"`
		Package   struct {
			Purpose string `json:"purpose"`
			Items   []Item `json:"items"`
			Budget  struct {
				UsedTokens int `json:"usedTokens"`
			} `json:"budget"`
			Omitted map[string]int `json:"omitted"`
		} `json:"package"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.PackageID == "" || out.Digest == "" {
		return nil, fmt.Errorf("%w answered no package", ErrCMI)
	}
	return &Package{PackageID: out.PackageID, Digest: out.Digest, Items: out.Package.Items, Tokens: out.Package.Budget.UsedTokens, Purpose: out.Package.Purpose, Omitted: out.Package.Omitted}, nil
}

// ContextOf turns a package into caller context: one entry per item, described so the model knows
// what it is reading - the kind, the source and its trust, when it was observed - and labelled with
// the package it came from.
func ContextOf(p *Package) []agui.Context {
	out, _ := ContextFrom(p.Items, 0)
	return out
}

// ContextFrom is ContextOf with citation numbers (D67): the knowledge items are numbered first, first+1, ... in order and
// carry their number in the description ("CMI [3] knowledge from ..."); a memory has none. first 0 numbers nothing. It
// returns the number the next knowledge item of the run gets.
func ContextFrom(items []Item, first int) ([]agui.Context, int) {
	out := make([]agui.Context, 0, len(items))
	next := first
	for _, it := range items {
		num := ""
		if first > 0 && it.Kind == "knowledge" {
			num = fmt.Sprintf("[%d] ", next)
			next++
		}
		desc := fmt.Sprintf("%s from %s (%s, observed %s, ref %s)", it.Kind, it.Source, it.Trust, it.ObservedAt.UTC().Format(time.RFC3339), it.Ref)
		if it.Title != "" {
			desc += " — " + it.Title
		}
		value := it.Excerpt
		if num != "" {
			// D70: a bracketed number in a document's own text must not read as a citation number beside the HOST's
			desc, _ = run.Neutralise(desc)
			value, _ = run.Neutralise(value)
		}
		out = append(out, agui.Context{Description: "CMI " + num + desc, Value: value})
	}
	return out, next
}

// Verification is the CMI's answer to a delivery-time check of a package (idp.telos.cmi Verification).
type Verification struct {
	PackageID  string   `json:"packageId"`
	Stale      bool     `json:"stale"`
	StaleItems []string `json:"staleItems"`
	Redacted   bool     `json:"redacted"`
}

// Verify asks the CMI whether a package's items are still usable (CMI ADR-0001 D24).
func (c *Client) Verify(ctx context.Context, bearer, packageID string) (*Verification, error) {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	hr, err := http.NewRequestWithContext(cctx, http.MethodPost, c.Endpoint+"/v1/context/packages/"+packageID+"/verify", nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCMI, err)
	}
	hr.Header.Set("Accept", "application/json")
	hr.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := c.HTTP.Do(hr)
	if err != nil {
		return nil, fmt.Errorf("%w unreachable: %v", ErrCMI, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{Status: resp.StatusCode}
	}
	var out Verification
	if err := json.Unmarshal(raw, &out); err != nil || out.PackageID == "" {
		return nil, fmt.Errorf("%w answered no verification", ErrCMI)
	}
	return &out, nil
}
