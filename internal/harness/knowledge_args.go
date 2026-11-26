package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"idp-host/internal/cmiclient"
)

// D65: the arguments of the HOST's knowledge tools are checked strictly before anything is sent to the CMI: every key is one
// the tool has (spelled exactly), every value has its type, and the lengths, the counts and the shapes are bounded. What the
// model wrote is data from outside, even though the model is ours. A refusal tells the model what is wrong so that it can correct
// the call, in words of the HOST's, never repeating the value.

// Bounds of the arguments.
const (
	maxQueryRunes    = 1000
	maxTextRunes     = 200
	maxListItems     = 8 // knowledgeBases, docTypes, entities
	maxDocTypeRunes  = 64
	maxHeadings      = 8
	maxHeadingRunes  = 200
	maxSheetRunes    = 128
	maxLocatorNumber = 1_000_000
)

var (
	entityTypeRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	entityIDRE   = regexp.MustCompile(`^[A-Za-z0-9_.:/@-]{1,256}$`)
	baseNameRE   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)
	documentIDRE = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)
	cellRangeRE  = regexp.MustCompile(`^[A-Z]{1,3}[0-9]{1,7}:[A-Z]{1,3}[0-9]{1,7}$`)
)

// errArgs is a refusal of the arguments; its text is what the model reads.
type errArgs string

func (e errArgs) Error() string { return string(e) }

func refuse(format string, a ...any) error { return errArgs(fmt.Sprintf(format, a...)) }

// strictObject decodes the arguments as a JSON object with exactly the allowed keys (case-sensitive: Go's decoder would accept
// "Query" for "query") into dst, which must have no other fields.
func strictObject(raw string, allowed []string, dst any, nested ...nestedKeys) error {
	if strings.TrimSpace(raw) == "" {
		raw = "{}"
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &keys); err != nil || keys == nil {
		return refuse("the arguments must be a JSON object")
	}
	for k := range keys {
		ok := false
		for _, a := range allowed {
			ok = ok || k == a
		}
		if !ok {
			return refuse("unknown argument %q; the arguments are %s", clip(k, 40), strings.Join(allowed, ", "))
		}
	}
	for _, n := range nested {
		if err := n.check(keys[n.field]); err != nil {
			return err
		}
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) && te.Field != "" {
			return refuse("argument %s has the wrong type (expected %s)", te.Field, te.Type.String())
		}
		if rest, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
			return refuse("unknown argument %s", clip(rest, 60))
		}
		return refuse("the arguments are not valid for this tool")
	}
	return nil
}

// nestedKeys says which keys an object at one level below the arguments may have: field is the argument, list says whether it
// is an array of such objects. The same spelling rule holds at every level (Go's decoder would take "Page" for "page").
type nestedKeys struct {
	field   string
	list    bool
	allowed []string
}

func (n nestedKeys) check(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var objs []json.RawMessage
	if n.list {
		if err := json.Unmarshal(raw, &objs); err != nil {
			return nil // the typed decode says what is wrong
		}
	} else {
		objs = []json.RawMessage{raw}
	}
	for _, o := range objs {
		var m map[string]json.RawMessage
		if json.Unmarshal(o, &m) != nil {
			continue
		}
		for k := range m {
			if !slices.Contains(n.allowed, k) {
				return refuse("unknown key %q in %s; the keys are %s", clip(k, 40), n.field, strings.Join(n.allowed, ", "))
			}
		}
	}
	return nil
}

// restoreBrackets is the fallback of undoing what the HOST did to a heading, a sheet name or a text it showed the model (D72): the model
// writes the heading as it read it, "Annex ⟦2⟧", and the CMI knows it as "Annex [2]". It is used for what the HOST has no record of showing
// (turn.restore knows the exact original of the headings and sheets it did show, whatever brackets they had). The residual ambiguity is a
// document whose own text has ⟦ or ⟧, which is rare; such a heading is asked for with brackets and not found.
func restoreBrackets(s string) string { return strings.NewReplacer("⟦", "[", "⟧", "]").Replace(s) }

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// plainText reports whether s is valid text with no control characters (tab and the line breaks of a query are not allowed
// either: these arguments are one line).
func plainText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

// line checks a required one-line text argument of at most max characters.
func line(name, s string, max int) (string, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return "", refuse("%s is required", name)
	case !plainText(s):
		return "", refuse("%s must be plain text on one line", name)
	case utf8.RuneCountInString(s) > max:
		return "", refuse("%s is too long (at most %d characters)", name, max)
	}
	return s, nil
}

// searchArgs are the arguments of knowledge__search.
type searchArgs struct {
	Query          string             `json:"query"`
	Entities       []cmiclient.Entity `json:"entities"`
	AsOf           *string            `json:"asOf"`
	KnowledgeBases []string           `json:"knowledgeBases"`
	DocTypes       []string           `json:"docTypes"`
}

// parseSearch checks the arguments of knowledge__search and returns the request to the CMI (without budget and purpose, which
// are the release's).
func parseSearch(raw string, now time.Time) (cmiclient.Request, error) {
	var a searchArgs
	if err := strictObject(raw, []string{"query", "entities", "asOf", "knowledgeBases", "docTypes"}, &a, nestedKeys{"entities", true, []string{"type", "id"}}); err != nil {
		return cmiclient.Request{}, err
	}
	q, err := line("query", a.Query, maxQueryRunes)
	if err != nil {
		return cmiclient.Request{}, err
	}
	req := cmiclient.Request{Query: q}
	if len(a.Entities) > maxListItems {
		return req, refuse("entities has too many entries (at most %d)", maxListItems)
	}
	for _, e := range a.Entities {
		if !entityTypeRE.MatchString(e.Type) || !entityIDRE.MatchString(e.ID) {
			return req, refuse("an entity needs a type (lower case letters, digits and hyphens) and an id (letters, digits and _ . : / @ -)")
		}
	}
	req.Entities = a.Entities
	if req.AsOf, err = parseAsOf(a.AsOf, now); err != nil {
		return req, err
	}
	if len(a.KnowledgeBases) > maxListItems {
		return req, refuse("knowledgeBases has too many entries (at most %d)", maxListItems)
	}
	seen := map[string]bool{}
	for _, k := range a.KnowledgeBases {
		if !baseNameRE.MatchString(k) || seen[k] {
			return req, refuse("knowledgeBases entries must be distinct knowledge base names (lower case letters, digits and hyphens)")
		}
		seen[k] = true
	}
	req.KnowledgeBases = a.KnowledgeBases
	if len(a.DocTypes) > maxListItems {
		return req, refuse("docTypes has too many entries (at most %d)", maxListItems)
	}
	seen = map[string]bool{}
	for _, d := range a.DocTypes {
		if d != strings.TrimSpace(d) || d == "" || !plainText(d) || utf8.RuneCountInString(d) > maxDocTypeRunes || seen[d] {
			return req, refuse("docTypes entries must be distinct document types of at most %d characters", maxDocTypeRunes)
		}
		seen[d] = true
	}
	req.DocTypes = a.DocTypes
	return req, nil
}

// parseAsOf reads the business date a question is about: a date (the end of that day, UTC) or an RFC 3339 time. The future is
// refused by the CMI; a date of today is the end of today, which is the future for a few hours, so it counts as now.
func parseAsOf(s *string, now time.Time) (*cmiclient.AsOf, error) {
	if s == nil || *s == "" {
		return nil, nil
	}
	var t time.Time
	if d, err := time.Parse("2006-01-02", *s); err == nil {
		t = d.Add(24*time.Hour - time.Second)
	} else if t, err = time.Parse(time.RFC3339, *s); err != nil {
		return nil, refuse("asOf must be a date (YYYY-MM-DD) or an RFC 3339 time")
	}
	if t.After(now) {
		t = now
	}
	t = t.UTC()
	return &cmiclient.AsOf{Valid: &t}, nil
}

// readArgs are the arguments of knowledge__read.
type readArgs struct {
	DocumentID string       `json:"documentId"`
	Locator    *locatorArgs `json:"locator"`
}

type locatorArgs struct {
	Page     *int     `json:"page"`
	Slide    *int     `json:"slide"`
	Headings []string `json:"headings"`
	Sheet    *string  `json:"sheet"`
	Range    *string  `json:"range"`
}

func parseDocumentID(id string) (string, error) {
	if !documentIDRE.MatchString(id) {
		return "", refuse("documentId must be a document id as a search result shows it")
	}
	return id, nil
}

// parseRead checks the arguments of knowledge__read: a document and, optionally, one place in it: a page, a slide, a heading
// path, or a sheet (with the cell range to read).
func parseRead(raw string) (cmiclient.ReadRequest, error) {
	var a readArgs
	if err := strictObject(raw, []string{"documentId", "locator"}, &a, nestedKeys{"locator", false, []string{"page", "slide", "headings", "sheet", "range"}}); err != nil {
		return cmiclient.ReadRequest{}, err
	}
	id, err := parseDocumentID(a.DocumentID)
	if err != nil {
		return cmiclient.ReadRequest{}, err
	}
	req := cmiclient.ReadRequest{DocumentID: id}
	if a.Locator == nil {
		return req, nil
	}
	l := a.Locator
	set := 0
	for _, b := range []bool{l.Page != nil, l.Slide != nil, len(l.Headings) > 0, l.Sheet != nil} {
		if b {
			set++
		}
	}
	if set != 1 || (l.Range != nil && l.Sheet == nil) {
		return req, refuse("locator names one place: page, slide, headings, or sheet (with an optional range)")
	}
	out := &cmiclient.Locator{}
	switch {
	case l.Page != nil:
		if *l.Page < 1 || *l.Page > maxLocatorNumber {
			return req, refuse("locator.page must be a page number from 1")
		}
		out.Page = *l.Page
	case l.Slide != nil:
		if *l.Slide < 1 || *l.Slide > maxLocatorNumber {
			return req, refuse("locator.slide must be a slide number from 1")
		}
		out.Slide = *l.Slide
	case len(l.Headings) > 0:
		if len(l.Headings) > maxHeadings {
			return req, refuse("locator.headings has too many entries (at most %d)", maxHeadings)
		}
		for _, h := range l.Headings {
			if strings.TrimSpace(h) == "" || !plainText(h) || utf8.RuneCountInString(h) > maxHeadingRunes {
				return req, refuse("locator.headings entries must be headings of at most %d characters", maxHeadingRunes)
			}
		}
		out.Headings = l.Headings
	default:
		if strings.TrimSpace(*l.Sheet) == "" || !plainText(*l.Sheet) || utf8.RuneCountInString(*l.Sheet) > maxSheetRunes {
			return req, refuse("locator.sheet must be a sheet name of at most %d characters", maxSheetRunes)
		}
		out.Sheet = *l.Sheet
		if l.Range != nil {
			if !cellRangeRE.MatchString(*l.Range) {
				return req, refuse("locator.range must be a cell range such as A12:F40")
			}
			out.Range = *l.Range
		}
	}
	req.Locator = out
	return req, nil
}

// findArgs are the arguments of knowledge__find.
type findArgs struct {
	DocumentID string `json:"documentId"`
	Text       string `json:"text"`
}

// parseFind checks the arguments of knowledge__find: a document and the words to look for in it.
func parseFind(raw string) (cmiclient.FindRequest, error) {
	var a findArgs
	if err := strictObject(raw, []string{"documentId", "text"}, &a); err != nil {
		return cmiclient.FindRequest{}, err
	}
	id, err := parseDocumentID(a.DocumentID)
	if err != nil {
		return cmiclient.FindRequest{}, err
	}
	text, err := line("text", a.Text, maxTextRunes)
	if err != nil {
		return cmiclient.FindRequest{}, err
	}
	return cmiclient.FindRequest{DocumentID: id, Text: text}, nil
}
