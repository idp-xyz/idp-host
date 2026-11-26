package cmiclient

// What the HOST records of a knowledge passage it numbered (ADR-0001 D70): the number, the document, where in it.

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"idp-host/internal/run"
)

// OneLine makes a label of the CMI's (a title, a heading) safe to put in a line of text the model reads: no control characters, no
// line breaks, at most n characters.
func OneLine(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:max(n-1, 0)]) + "…" // at most n characters, the ellipsis included
}

// Where describes a locator in words.
func Where(l *Locator) string {
	if l == nil {
		return ""
	}
	var parts []string
	if l.Page > 0 {
		parts = append(parts, "page "+strconv.Itoa(l.Page))
	}
	if l.Slide > 0 {
		parts = append(parts, "slide "+strconv.Itoa(l.Slide))
	}
	if l.Sheet != "" {
		s := "sheet " + OneLine(l.Sheet, 128)
		if l.Range != "" {
			s += " " + OneLine(l.Range, 32)
		}
		parts = append(parts, s)
	}
	if l.Table != "" {
		parts = append(parts, "table "+OneLine(l.Table, 128))
	}
	if len(l.Headings) > 0 {
		hs := make([]string, 0, len(l.Headings))
		for _, h := range l.Headings {
			hs = append(hs, OneLine(h, 200))
		}
		parts = append(parts, strings.Join(hs, " > "))
	}
	return strings.Join(parts, "; ")
}

// ExcerptDigest identifies the text of a passage: the same place with other text is another passage.
func ExcerptDigest(excerpt string) string {
	sum := sha256.Sum256([]byte(excerpt))
	return hex.EncodeToString(sum[:8])
}

// MaxDocumentID is the longest document id the HOST records (the contract's maxLength).
const MaxDocumentID = 128

// KnowledgeSource is the record of knowledge item it numbered n: the title (or the ref when it has none), the document and the
// place in it, and the CMI source; each cut to what the contract allows. Never the passage's text (D49: only labels are kept). A
// document id is an identifier and is never cut: one that is empty or too long is not recorded (ok false), though the passage still
// has its number.
func KnowledgeSource(n int, it Item) (src run.Source, ok bool) {
	if it.ID == "" || utf8.RuneCountInString(it.ID) > MaxDocumentID || strings.ContainsFunc(it.ID, unicode.IsControl) {
		return run.Source{}, false
	}
	title := OneLine(it.Title, run.MaxSourceText)
	if title == "" {
		title = OneLine(it.Ref, run.MaxSourceText)
	}
	return run.Source{N: n, Kind: run.KindKnowledge, Title: title, DocumentID: it.ID, Where: OneLine(Where(it.Locator), 1000), Origin: OneLine(it.Source, 64), Digest: ExcerptDigest(it.Excerpt)}, true
}

// KnowledgeSources are the records of the knowledge items ContextFrom numbers first, first+1, ... (the items of kind knowledge, in
// order). first 0 numbers nothing.
func KnowledgeSources(items []Item, first int) []run.Source {
	if first <= 0 {
		return nil
	}
	var out []run.Source
	n := first
	for _, it := range items {
		if it.Kind == "knowledge" {
			if src, ok := KnowledgeSource(n, it); ok {
				out = append(out, src)
			}
			n++
		}
	}
	return out
}
