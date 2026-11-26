package cmiclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReadAndFindPostToTheirEndpointsAndDecodeAPackage(t *testing.T) {
	var path, auth string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		body = nil
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"packageId":"p1","digest":"sha256:x","package":{"purpose":"read","items":[{"kind":"knowledge","id":"d1","source":"s","trust":"authoritative","ref":"r","excerpt":"e","observedAt":"2026-11-01T00:00:00Z","sensitivity":"internal","tokens":1,"match":"exact","locator":{"page":3,"headings":["A","B"]},"hits":[{"offset":4,"length":2}]}],"budget":{"usedTokens":1},"omitted":{"budget":2}}}`))
	}))
	defer srv.Close()
	c := &Client{Endpoint: srv.URL, HTTP: srv.Client()}
	around := 2
	p, err := c.Read(context.Background(), "tok", ReadRequest{DocumentID: "d1", Locator: &Locator{Page: 3}, Around: &around, Purpose: "assistant"})
	if err != nil || path != "/v1/context/read" || auth != "Bearer tok" || body["documentId"] != "d1" || body["around"] != float64(2) || body["purpose"] != "assistant" {
		t.Fatalf("%v %s %s %v", err, path, auth, body)
	}
	if p.PackageID != "p1" || p.Purpose != "read" || p.Omitted["budget"] != 2 || len(p.Items) != 1 || p.Items[0].Locator.Page != 3 || p.Items[0].Locator.Headings[1] != "B" || p.Items[0].Hits[0].Length != 2 || p.Items[0].Match != "exact" {
		t.Fatalf("%+v", p)
	}
	if _, err := c.Find(context.Background(), "tok", FindRequest{DocumentID: "d1", Text: "x", MaxHits: 5, Purpose: "assistant"}); err != nil || path != "/v1/context/find" || body["text"] != "x" || body["maxHits"] != float64(5) {
		t.Fatalf("%v %s %v", err, path, body)
	}
	// resolve carries the KB8 filters
	if _, err := c.Resolve(context.Background(), "tok", Request{Query: "q", KnowledgeBases: []string{"ops"}, DocTypes: []string{"policy"}, Entities: []Entity{{Type: "customer", ID: "a"}}}); err != nil ||
		path != "/v1/context/resolve" || body["knowledgeBases"].([]any)[0] != "ops" || body["docTypes"].([]any)[0] != "policy" {
		t.Fatalf("%v %v", err, body)
	}
}

func TestACMIRefusalIsAStatusErrorThatSaysNothingOfTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":"NOT_FOUND","message":"document secret-123 does not exist in tenant acme"}}`, 404)
	}))
	defer srv.Close()
	c := &Client{Endpoint: srv.URL, HTTP: srv.Client(), Timeout: time.Second}
	_, err := c.Read(context.Background(), "tok", ReadRequest{DocumentID: "d"})
	if Status(err) != 404 || !errors.Is(err, ErrCMI) || strings.Contains(err.Error(), "secret") || err.Error() != "cmi answered HTTP 404" {
		t.Fatalf("%v", err)
	}
	if Status(errors.New("x")) != 0 {
		t.Error("only a refusal has a status")
	}
}

func TestKnowledgeItemsAreNumberedInOrderAndAMemoryHasNoNumber(t *testing.T) {
	items := []Item{{Kind: "knowledge", Source: "a", Trust: "authoritative", Ref: "r1", Excerpt: "one"}, {Kind: "memory", Source: "m", Trust: "informational", Ref: "r2", Excerpt: "two"}, {Kind: "knowledge", Source: "a", Trust: "authoritative", Ref: "r3", Excerpt: "three"}}
	got, next := ContextFrom(items, 4)
	if next != 6 || len(got) != 3 || !strings.HasPrefix(got[0].Description, "CMI [4] knowledge from a") || !strings.HasPrefix(got[1].Description, "CMI memory from m") || !strings.HasPrefix(got[2].Description, "CMI [5] knowledge") {
		t.Fatalf("%d %+v", next, got)
	}
	if got, next := ContextFrom(items, 0); next != 0 || strings.Contains(got[0].Description, "[") {
		t.Errorf("first 0 numbers nothing: %d %+v", next, got)
	}
	// the screen keeps the numbering to what is kept
	items[0].Sensitivity, items[2].Sensitivity = "public", "restricted"
	s := Screen(items[:1:1], "internal")
	s2 := Screen([]Item{items[0], items[2]}, "internal")
	if _, n := s.ContextFrom(1); n != 2 {
		t.Errorf("%d", n)
	}
	out, n := s2.ContextFrom(7)
	if n != 8 || len(out) != 2 || !strings.HasPrefix(out[0].Description, "CMI [7] ") || out[1].Description != ContextCMIWithheld {
		t.Fatalf("%d %+v", n, out)
	}
}
