package run

// The text of a web result written from its structure (ADR-0001 D71). A web tool that declares an outputSchema (idp-web does, for
// web_search, web_fetch and web_open_url) gives everything its text shows as fields; when the structured result is valid against the
// schema the gateway listed, the HOST writes the text the model reads itself, from the fields, with its own numbers, and idp-web's
// text is not read at all. A result without a valid structure goes the older way (RenderWeb, the text rewritten).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// How a web result's text was made (the render field of idp.host/citations.numbered).
const (
	RenderStructured = "structured"
	RenderText       = "text"
)

// Why a structured result was not used (the reason field of idp.host/citations.numbered).
const (
	ReasonNoSchema  = "no_output_schema"  // the tool listed no outputSchema
	ReasonNoStruct  = "no_structure"      // the result has no structuredContent
	ReasonViolation = "schema_violation"  // the structure is not valid against the schema the tool listed
	ReasonUnusable  = "unusable_entry"    // valid, but an entry is one the HOST would not record (an address that is not http or https...)
	ReasonBadSchema = "schema_unreadable" // the listed schema could not be compiled
)

var schemaCache sync.Map // schema text -> *jsonschema.Schema (or the error that compiling gave)

func compiled(schema json.RawMessage) (*jsonschema.Schema, error) {
	if v, ok := schemaCache.Load(string(schema)); ok {
		if e, isErr := v.(error); isErr {
			return nil, e
		}
		return v.(*jsonschema.Schema), nil
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	var sch *jsonschema.Schema
	if err == nil {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		if err = c.AddResource("urn:idp:host:outputSchema", doc); err == nil {
			sch, err = c.Compile("urn:idp:host:outputSchema")
		}
	}
	if err != nil {
		schemaCache.Store(string(schema), err)
		return nil, err
	}
	schemaCache.Store(string(schema), sch)
	return sch, nil
}

// webItem is one entry of the citations of a web tool's structured result.
type webItem struct {
	Source
	Ref       string
	Published string
	Excerpt   string
	FinalURL  string
	Rendered  bool
	IsRead    bool // the entry carries a part of a page (text), not a search result
	Text      string
	Start     int
	End       int
	Total     int
	Next      *int
	Truncated bool
	Notice    string
	PDF       *struct {
		Pages, PagesRead int
		Cut              bool
	}
}

// StructuredWeb is a web tool's structured result that is valid against the tool's declared schema and whose entries the HOST can
// record: it can be rendered for the model (Render).
type StructuredWeb struct {
	items []webItem
}

// Empty reports whether the result lists no source (an empty search).
func (s *StructuredWeb) Empty() bool { return len(s.items) == 0 }

// ParseStructured checks a result's structuredContent against the outputSchema the tool listed and reads its entries. When it cannot
// be used it says why (the Reason constants), never with the content: the caller then renders the result from its text.
func ParseStructured(schema, structured json.RawMessage) (*StructuredWeb, string) {
	switch {
	case len(bytes.TrimSpace(schema)) == 0:
		return nil, ReasonNoSchema
	case len(bytes.TrimSpace(structured)) == 0:
		return nil, ReasonNoStruct
	}
	sch, err := compiled(schema)
	if err != nil {
		return nil, ReasonBadSchema
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(structured))
	if err != nil || sch.Validate(inst) != nil {
		return nil, ReasonViolation
	}
	var doc struct {
		Citations []map[string]json.RawMessage `json:"citations"`
	}
	if json.Unmarshal(structured, &doc) != nil || doc.Citations == nil || len(doc.Citations) > maxScanned {
		return nil, ReasonViolation
	}
	out := &StructuredWeb{}
	seen := map[int]bool{}
	for _, m := range doc.Citations {
		raw, _ := json.Marshal(m)
		src, ok := parseCitation(raw)
		if !ok || seen[src.N] {
			return nil, ReasonUnusable
		}
		seen[src.N] = true
		src.Kind = KindWeb
		it := webItem{Source: src}
		str := func(k string) string {
			var v string
			_ = json.Unmarshal(m[k], &v)
			return v
		}
		num := func(k string) int {
			var v int
			_ = json.Unmarshal(m[k], &v)
			return v
		}
		it.Ref, it.Published, it.Excerpt, it.FinalURL, it.Notice = str("ref"), str("published"), str("excerpt"), str("finalUrl"), str("notice")
		if it.FinalURL != "" && !validURL(it.FinalURL) {
			it.FinalURL = "" // where a redirect ended is shown only when it is an address like any other; otherwise the note is dropped
		}
		_ = json.Unmarshal(m["rendered"], &it.Rendered)
		_ = json.Unmarshal(m["truncated"], &it.Truncated)
		if _, ok := m["text"]; ok {
			it.IsRead = true
			it.Text, it.Start, it.End, it.Total = str("text"), num("start"), num("end"), num("total")
			if _, has := m["nextOffset"]; has {
				n := num("nextOffset")
				it.Next = &n
			}
			if p, has := m["pdf"]; has {
				var pdf struct {
					Pages, PagesRead int
					Cut              bool
				}
				_ = json.Unmarshal(p, &pdf)
				it.PDF = &pdf
			}
		}
		out.items = append(out.items, it)
	}
	return out, ""
}

// sanitize makes text of a page safe to show: invalid UTF-8 replaced, control characters other than the line break and tab removed,
// and the format characters (the bidirectional overrides and isolates, zero-width characters) removed.
func cleanPageText(s string) string {
	s = strings.ToValidUTF8(s, "�")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r == '\r':
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == 0x2028 || r == 0x2029:
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// oneLine is a label (a title, an excerpt) as one line: whitespace collapses, so that nothing in it can start a block of its own.
func oneLineText(s string, max int) string {
	s = strings.Join(strings.Fields(cleanPageText(strings.NewReplacer("\n", " ", "\t", " ").Replace(s))), " ")
	if utf8.RuneCountInString(s) > max {
		s = string([]rune(s)[:max-1]) + "…"
	}
	return s
}

// showURL is an address as it is written into the text the model reads: the square brackets are percent-encoded, so that a "[2]" in an
// address is not a number.
func showURL(u string) string { return strings.NewReplacer("[", "%5B", "]", "%5D").Replace(u) }

// Render writes the text the model reads of the result (D71): for a search, a block per source with its HOST number, address, ref,
// date and excerpt; for a read, a head line with the HOST number, the title and where in the page this part lies, the address and
// ref, and the text. tool is the name the call was made with ("the rest: web_fetch with offset N"). Every field that came from a web
// page is cleaned and neutralised (D72); the number at the head of a block is the only bracketed number the HOST writes. limit is
// D40's limit on what the model is given, all of it counted (the blocks, the note on neutralised numbers): a block that would pass it is
// not shown and not numbered. A part of a page that would pass it is cut on the RAW text, at a character boundary, before it is cleaned and
// neutralised; the head then says where the shown part ends and offers that offset to continue from, so that continuing resumes exactly where
// the model stopped and skips nothing.
func (s *StructuredWeb) Render(tool string, first int, known func(webN int, url string) (int, bool), limit int) WebRendered {
	out := WebRendered{Mode: WebRewritten, Render: RenderStructured}
	if len(s.items) == 0 {
		out.Text = "no results"
		return out
	}
	type block struct {
		text    string
		changed int
		isNew   bool
		src     Source
	}
	noteSize := len(neutralisedNote(1))
	var blocks []block
	used := 0 // bytes of the blocks so far, with the blank lines between them
	next := first
	for _, it := range s.items {
		n, isNew := number(it.Source, next, known)
		changed := 0
		neut := func(x string) string {
			y, k := Neutralise(x)
			changed += k
			return y
		}
		title := neut(oneLineText(it.Title, MaxSourceText))
		if title == "" {
			title = neut(showURL(it.URL))
		}
		var text string
		if !it.IsRead {
			var blk strings.Builder
			fmt.Fprintf(&blk, "[%d] %s\naddress: %s\nref: %s", n, title, showURL(it.URL), neut(oneLineText(it.Ref, 4096)))
			if it.Published != "" {
				fmt.Fprintf(&blk, "\npublished: %s", neut(oneLineText(it.Published, 64)))
			}
			if it.Excerpt != "" {
				fmt.Fprintf(&blk, "\nexcerpt: %s", neut(oneLineText(it.Excerpt, 4000)))
			}
			text = blk.String()
		} else {
			sep := 0
			if len(blocks) > 0 {
				sep = 2
			}
			budget := limit - used - sep - noteSize
			var add int
			text, add = renderRead(it, n, title, tool, budget, limit > 0)
			changed += add
		}
		sep := 0
		if len(blocks) > 0 {
			sep = 2
		}
		if limit > 0 && len(blocks) > 0 && used+sep+len(text) > limit-noteSize {
			out.NotShown = len(s.items) - len(blocks)
			break
		}
		used += sep + len(text)
		if isNew {
			next++
		}
		src := it.Source
		src.WebN, src.N = src.N, n
		blocks = append(blocks, block{text, changed, isNew, src})
	}
	var b strings.Builder
	changed := 0
	for i, bl := range blocks {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(bl.text)
		changed += bl.changed
		out.Sources = append(out.Sources, bl.src)
		if bl.isNew {
			out.Used++
		}
	}
	out.Text = capText(b.String()+neutralisedNote(changed), limit)
	return out
}

// renderRead writes one part of a page: the head, the address and ref, and the text, cut on the raw text to fit budget when there is one. It
// returns the text and how many bracketed numbers it neutralised (the title, neutralised by the caller, is not among them).
func renderRead(it webItem, n int, title, tool string, budget int, limited bool) (string, int) {
	changed := 0
	neut := func(x string) string {
		y, k := Neutralise(x)
		changed += k
		return y
	}
	final, ref := "", ""
	if it.FinalURL != "" && it.FinalURL != it.URL {
		final = neut(showURL(it.FinalURL))
	}
	if it.Ref != "" {
		ref = neut(oneLineText(it.Ref, 4096))
	}
	head := func(end int, next *int) string {
		h := fmt.Sprintf("[%d] %s", n, title)
		if final != "" {
			h += " [the address redirected to " + final + "]"
		}
		var what string
		if it.PDF != nil {
			what = fmt.Sprintf("PDF, %d pages", it.PDF.Pages)
			if it.PDF.Cut {
				what += fmt.Sprintf(", only the first %d are read", it.PDF.PagesRead)
			}
			what += "; "
		}
		if it.Rendered {
			what += "rendered; "
		}
		h += fmt.Sprintf(" (%sbytes %d–%d of %d", what, it.Start, end, it.Total)
		if next != nil {
			h += fmt.Sprintf("; the rest: %s with offset %d)", tool, *next)
		} else {
			h += ")"
		}
		h += "\naddress: " + showURL(it.URL)
		if ref != "" {
			h += "\nref: " + ref
		}
		return h
	}
	if it.Notice != "" || it.Text == "" {
		body := "(no text found on this page)"
		if it.Notice == "offset_past_end" {
			body = "(nothing at this offset: the page text is shorter)"
		}
		return head(it.End, it.Next) + "\n\n" + body, changed
	}
	raw, end, next := it.Text, it.End, it.Next
	cut := len(raw)
	if limited {
		// the room for the text is what the budget leaves after the head; what neutralising adds is counted, and the raw text is cut
		// until the whole fits
		room := budget - len(head(end, next)) - 2 - 40
		for tries := 0; ; tries++ {
			cut = min(len(raw), max(room, 0))
			for cut > 0 && cut < len(raw) && !utf8.RuneStart(raw[cut]) {
				cut--
			}
			e, nx := end, next
			if cut < len(raw) {
				e = it.Start + cut
				v := e
				nx = &v
			}
			body, _ := Neutralise(cleanPageText(raw[:cut]))
			size := len(head(e, nx)) + 2 + len(body)
			if size <= budget || cut == 0 || tries > 8 {
				if cut < len(raw) {
					end, next = e, nx
				}
				break
			}
			room -= size - budget + 1
		}
	}
	body := neut(cleanPageText(raw[:cut]))
	if body == "" {
		body = "(the limit left no room for the text of this part)"
	}
	return head(end, next) + "\n\n" + body, changed
}
