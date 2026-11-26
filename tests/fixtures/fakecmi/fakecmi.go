// Package fakecmi is a CMI for tests that follows docs/adr/kb8-cmi-api.md (idp.telos.cmi 1.7.0): POST /v1/context/resolve with
// knowledgeBases and docTypes, POST /v1/context/read, POST /v1/context/find, and POST /v1/context/packages/{id}/verify. It holds a
// few documents in memory and answers from them. The member is the one the bearer names ("member:<subject>", or any other
// text, which is a subject of that text), and a document is visible to the subjects it lists (none listed: everyone), so a test
// can see that the call was made as the member. It is a test double, never a product component; it does not rank, it matches words.
package fakecmi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
)

// Locator is where a block is in its document.
type Locator struct {
	Headings []string `json:"headings,omitempty"`
	Page     int      `json:"page,omitempty"`
	Slide    int      `json:"slide,omitempty"`
	Sheet    string   `json:"sheet,omitempty"`
	Range    string   `json:"range,omitempty"`
}

// Block is one chunk of a document.
type Block struct {
	Text        string
	Locator     Locator
	Sensitivity string // default: the document's
}

// Doc is a knowledge document.
type Doc struct {
	ID          string
	Title       string
	Source      string
	KB          string // knowledge base name
	DocType     string
	Sensitivity string // default internal
	Entities    []Entity
	Visible     []string // subjects that may see it; empty = everyone
	Blocks      []Block
}

// Entity is a business object reference.
type Entity struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// Request is what the fake saw: the path, the bearer and the decoded body.
type Request struct {
	Path   string
	Bearer string
	Body   map[string]any
}

// Server is the fake.
type Server struct {
	mu   sync.Mutex
	docs []Doc
	reqs []Request
	n    int
	// FailWith makes the next answer of a path (e.g. "/v1/context/read") that HTTP status, once; Status makes every answer that status.
	FailWith map[string]int
	Status   int
	// Stale makes verify answer that the package is stale.
	Stale bool
	// Hold, when set, is called with the path of every resolve, read and find after it was recorded and before it is answered;
	// a test blocks in it to hold a call in progress.
	Hold func(path string)
}

// New returns a fake with the documents.
func New(docs ...Doc) *Server { return &Server{docs: docs, FailWith: map[string]int{}} }

// Add adds a document.
func (s *Server) Add(d Doc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.docs = append(s.docs, d)
}

// Requests returns every request seen, in order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.reqs...)
}

// RequestsTo returns the requests to a path.
func (s *Server) RequestsTo(path string) []Request {
	var out []Request
	for _, r := range s.Requests() {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

// Start serves the fake on a loopback port.
func (s *Server) Start() *httptest.Server { return httptest.NewServer(s.Handler()) }

// Handler serves the four endpoints.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/context/resolve", s.handle(s.resolve))
	mux.HandleFunc("POST /v1/context/read", s.handle(s.read))
	mux.HandleFunc("POST /v1/context/find", s.handle(s.find))
	mux.HandleFunc("POST /v1/context/packages/{id}/verify", func(w http.ResponseWriter, r *http.Request) {
		s.record(r, nil)
		w.Header().Set("Content-Type", "application/json")
		stale, items := false, "[]"
		if s.Stale {
			stale, items = true, `["x"]`
		}
		fmt.Fprintf(w, `{"packageId":%q,"stale":%v,"staleItems":%s,"redacted":false}`, r.PathValue("id"), stale, items)
	})
	return mux
}

func (s *Server) record(r *http.Request, body map[string]any) {
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	s.reqs = append(s.reqs, Request{Path: r.URL.Path, Bearer: bearer, Body: body})
	s.mu.Unlock()
}

// subject is who the bearer says the caller is.
func subject(bearer string) string { return strings.TrimPrefix(bearer, "member:") }

func problem(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":{"code":%q,"message":"fake"}}`, code)
}

type answerer func(sub string, body map[string]any) (items []map[string]any, omitted map[string]int, purpose string, status int)

func (s *Server) handle(f answerer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.record(r, body)
		if s.Hold != nil {
			s.Hold(r.URL.Path)
		}
		s.mu.Lock()
		status := s.Status
		if st, ok := s.FailWith[r.URL.Path]; ok {
			status = st
			delete(s.FailWith, r.URL.Path)
		}
		s.mu.Unlock()
		if status != 0 {
			problem(w, status, "FAKE")
			return
		}
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		items, omitted, purpose, st := f(subject(bearer), body)
		if st != 0 {
			problem(w, st, "FAKE")
			return
		}
		s.mu.Lock()
		s.n++
		id := fmt.Sprintf("pkg_fake_%d", s.n)
		s.mu.Unlock()
		pkg := map[string]any{"schemaVersion": "idp.cmi.package/v1", "packageId": id, "purpose": purpose, "items": items,
			"budget": map[string]any{"maxTokens": 3000, "usedTokens": usedTokens(items), "maxItems": 20}}
		if len(omitted) > 0 {
			pkg["omitted"] = omitted
		}
		raw, _ := json.Marshal(pkg)
		sum := sha256.Sum256(raw)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"packageId": id, "digest": "sha256:" + hex.EncodeToString(sum[:]), "package": pkg})
	}
}

func usedTokens(items []map[string]any) int {
	n := 0
	for _, it := range items {
		n += it["tokens"].(int)
	}
	return n
}

func (s *Server) visible(d Doc, sub string) bool {
	if len(d.Visible) == 0 {
		return true
	}
	for _, v := range d.Visible {
		if v == sub {
			return true
		}
	}
	return false
}

func (s *Server) doc(id, sub string) (Doc, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.docs {
		if d.ID == id && s.visible(d, sub) {
			return d, true
		}
	}
	return Doc{}, false
}

func item(d Doc, idx int, b Block, match string) map[string]any {
	sens := b.Sensitivity
	if sens == "" {
		sens = d.Sensitivity
	}
	if sens == "" {
		sens = "internal"
	}
	it := map[string]any{"kind": "knowledge", "id": d.ID, "source": d.Source, "trust": "authoritative", "ref": fmt.Sprintf("%s#%d", d.ID, idx), "title": d.Title,
		"excerpt": b.Text, "observedAt": "2026-11-01T00:00:00Z", "sensitivity": sens, "score": 1.0, "tokens": len(b.Text)/4 + 1, "match": match}
	if l := b.Locator; l.Page != 0 || l.Slide != 0 || l.Sheet != "" || len(l.Headings) > 0 {
		it["locator"] = l
	}
	return it
}

func strs(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// resolve matches the words of the query against the blocks; knowledgeBases, docTypes and entities narrow, and nothing widens what
// the bearer's subject may see.
func (s *Server) resolve(sub string, body map[string]any) ([]map[string]any, map[string]int, string, int) {
	q, _ := body["query"].(string)
	var kbs, dts []string
	if v, ok := body["knowledgeBases"]; ok {
		kbs = strs(v)
	}
	if v, ok := body["docTypes"]; ok {
		dts = strs(v)
	}
	var ents []Entity
	if v, ok := body["entities"]; ok {
		raw, _ := json.Marshal(v)
		_ = json.Unmarshal(raw, &ents)
	}
	maxItems := 20
	if b, ok := body["budget"].(map[string]any); ok {
		if n, ok := b["maxItems"].(float64); ok && n > 0 {
			maxItems = int(n)
		}
	}
	words := strings.Fields(strings.ToLower(q))
	var items []map[string]any
	omitted := map[string]int{}
	s.mu.Lock()
	docs := append([]Doc(nil), s.docs...)
	s.mu.Unlock()
	for _, d := range docs {
		if !s.visible(d, sub) || (len(kbs) > 0 && !has(kbs, d.KB)) || (len(dts) > 0 && !has(dts, d.DocType)) {
			continue
		}
		if len(ents) > 0 {
			ok := false
			for _, e := range ents {
				for _, de := range d.Entities {
					ok = ok || e == de
				}
			}
			if !ok {
				continue
			}
		}
		for i, b := range d.Blocks {
			text := strings.ToLower(b.Text + " " + d.Title)
			hit := false
			for _, w := range words {
				hit = hit || strings.Contains(text, w)
			}
			if !hit {
				continue
			}
			if len(items) >= maxItems {
				omitted["budget"]++
				continue
			}
			items = append(items, item(d, i, b, "lexical"))
		}
	}
	return items, omitted, "assistant", 0
}

func overlaps(a, b string) bool {
	re := regexp.MustCompile(`^([A-Z]+)([0-9]+):([A-Z]+)([0-9]+)$`)
	ma, mb := re.FindStringSubmatch(a), re.FindStringSubmatch(b)
	if ma == nil || mb == nil {
		return false
	}
	var ra1, ra2, rb1, rb2 int
	fmt.Sscan(ma[2], &ra1)
	fmt.Sscan(ma[4], &ra2)
	fmt.Sscan(mb[2], &rb1)
	fmt.Sscan(mb[4], &rb2)
	return ra1 <= rb2 && rb1 <= ra2
}

func hit(l, want Locator) bool {
	switch {
	case want.Page != 0:
		return l.Page == want.Page
	case want.Slide != 0:
		return l.Slide == want.Slide
	case len(want.Headings) > 0:
		if len(l.Headings) < len(want.Headings) {
			return false
		}
		for i, h := range want.Headings {
			if l.Headings[i] != h {
				return false
			}
		}
		return true
	case want.Sheet != "":
		return l.Sheet == want.Sheet && (want.Range == "" || overlaps(l.Range, want.Range))
	}
	return true
}

// read returns the blocks at the locator, in document order, with `around` blocks each side (default 1, at most 5); without a locator,
// from the start while the budget lasts.
func (s *Server) read(sub string, body map[string]any) ([]map[string]any, map[string]int, string, int) {
	id, _ := body["documentId"].(string)
	d, ok := s.doc(id, sub)
	if !ok {
		return nil, nil, "", http.StatusNotFound
	}
	around := 1
	if n, ok := body["around"].(float64); ok {
		around = min(int(n), 5)
	}
	var want Locator
	hasLoc := false
	if v, ok := body["locator"]; ok && v != nil {
		raw, _ := json.Marshal(v)
		_ = json.Unmarshal(raw, &want)
		hasLoc = true
	}
	maxTokens := 3000
	if b, ok := body["budget"].(map[string]any); ok {
		if n, ok := b["maxTokens"].(float64); ok && n > 0 {
			maxTokens = int(n)
		}
	}
	in := make([]bool, len(d.Blocks))
	if !hasLoc {
		for i := range in {
			in[i] = true
		}
	} else {
		for i, b := range d.Blocks {
			if hit(b.Locator, want) {
				for j := max(i-around, 0); j <= min(i+around, len(d.Blocks)-1); j++ {
					in[j] = true
				}
			}
		}
	}
	var items []map[string]any
	omitted := map[string]int{}
	used := 0
	for i, b := range d.Blocks {
		if !in[i] {
			continue
		}
		it := item(d, i, b, "exact")
		if used+it["tokens"].(int) > maxTokens && len(items) > 0 {
			omitted["budget"]++
			continue
		}
		used += it["tokens"].(int)
		items = append(items, it)
	}
	return items, omitted, "read", 0
}

var spaceRE = regexp.MustCompile(`\s+`)

func norm(s string) string {
	return strings.ToLower(strings.TrimSpace(spaceRE.ReplaceAllString(s, " ")))
}

// find returns the blocks that hold the text (whitespace normalised, case-insensitive), in document order, with the offset of the hits.
func (s *Server) find(sub string, body map[string]any) ([]map[string]any, map[string]int, string, int) {
	id, _ := body["documentId"].(string)
	d, ok := s.doc(id, sub)
	if !ok {
		return nil, nil, "", http.StatusNotFound
	}
	text, _ := body["text"].(string)
	want := norm(text)
	var items []map[string]any
	if want == "" {
		return nil, nil, "", http.StatusUnprocessableEntity
	}
	for i, b := range d.Blocks {
		n := norm(b.Text)
		at := strings.Index(n, want)
		if at < 0 {
			continue
		}
		at = len([]rune(n[:at])) // 1.7.0: offsets and lengths are code points of the block's text
		it := item(d, i, b, "exact")
		it["hits"] = []map[string]int{{"offset": at, "length": len([]rune(want))}}
		items = append(items, it)
	}
	return items, nil, "find", 0
}
