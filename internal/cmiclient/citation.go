package cmiclient

// What the HOST records of a knowledge passage it numbered (ADR-0001 D70): the number, the document, where in it.

import (
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
	return string([]rune(s)[:n]) + "…"
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

// KnowledgeSource is the record of knowledge item it numbered n: the title (or the ref when it has none), the document and the
// place in it, and the CMI source. Never the passage's text (D49: only labels are kept).
func KnowledgeSource(n int, it Item) run.Source {
	title := OneLine(it.Title, run.MaxSourceText)
	if title == "" {
		title = OneLine(it.Ref, run.MaxSourceText)
	}
	return run.Source{N: n, Kind: run.KindKnowledge, Title: title, DocumentID: OneLine(it.ID, 128), Where: Where(it.Locator), Origin: OneLine(it.Source, 64)}
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
			out = append(out, KnowledgeSource(n, it))
			n++
		}
	}
	return out
}
