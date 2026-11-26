package run

// What the model reads of a web tool's result, once the HOST is the only numbering authority of a run tree (ADR-0001 D70-D72).
//
// idp-web numbers its sources per caller, increasing and not consecutive, and writes its own [n] into the text of its results. The
// HOST gives every source of a run tree one number, from one counter (D70), so the numbers idp-web wrote must not reach the model:
// RenderWeb replaces them with the HOST's. What idp-web puts in structuredContent.citations (n, title, url, finalUrl, fetchedAt,
// rendered, sha256) is not the whole result: the excerpt, the ref that web_fetch needs, the date of publication and the text of a
// page are only in idp-web's text. So the HOST cannot write the result from the structured part alone; it keeps idp-web's text and
// changes only what carries a number: the header of each source's block, which gets the HOST's number, and every other bracketed
// number in the text, which is turned into something that cannot be read as a citation (Neutralise).

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// How the text of a web result was made (WebRendered.Mode).
const (
	WebRewritten   = "rewritten"   // the headers carry the HOST's numbers; the rest is idp-web's text with its bracketed numbers neutralised
	WebIndexed     = "indexed"     // the text did not have the shape the HOST knows: a list of the sources with the HOST's numbers, then the text neutralised
	WebNeutralised = "neutralised" // citations were declared but none is usable: the text, neutralised, with no number
	WebUntouched   = "untouched"   // no structured citations: the text as it was, with no number (D71)
)

// WebCitations is what a result's structuredContent.citations gave: the entries that have the documented shape, in order, with
// idp-web's number in N, and how many did not.
type WebCitations struct {
	Items    []Source
	Invalid  int
	Excess   int
	Declared int // entries in the array, valid or not
}

// ParseWebCitations reads structuredContent.citations (platform ADR-0003 D9) as ParseCitations does and also says how many entries
// the array held. A number that appears twice in one result is one source: the second entry is dropped (counted invalid when it
// names another address).
func ParseWebCitations(structured json.RawMessage) WebCitations {
	var doc struct {
		Citations json.RawMessage `json:"citations"`
	}
	var out WebCitations
	if len(structured) == 0 || json.Unmarshal(structured, &doc) != nil || len(doc.Citations) == 0 {
		return out
	}
	var entries []json.RawMessage
	if json.Unmarshal(doc.Citations, &entries) != nil {
		return out
	}
	out.Declared = len(entries)
	seen := map[int]string{}
	for i, raw := range entries {
		if i >= maxScanned {
			out.Excess += len(entries) - maxScanned
			break
		}
		s, ok := parseCitation(raw)
		if !ok {
			out.Invalid++
			continue
		}
		if u, dup := seen[s.N]; dup {
			if u != s.URL {
				out.Invalid++
			}
			continue
		}
		seen[s.N] = s.URL
		s.Kind = KindWeb
		out.Items = append(out.Items, s)
	}
	return out
}

// WebRendered is the text of a web result as the model reads it, and what it gave the run.
type WebRendered struct {
	Text string
	// Sources are the sources numbered in this result, in order, in the HOST's numbers (N) with idp-web's in WebN: new ones and ones
	// the tree had already numbered (the same address read again keeps its number).
	Sources []Source
	// Used is how many new numbers were taken, first .. first+Used-1; a source the tree knew takes none.
	Used int
	// NotShown counts the citations whose block the text does not hold (the result was cut for D40's limit): they are not numbered,
	// as a knowledge passage that is not shown is not.
	NotShown int
	Mode     string
	// Render says which way the text was made: RenderStructured (from the declared structure) or RenderText (idp-web's text rewritten).
	Render string
}

// RenderWeb makes the text the model reads of a web result (D71) from idp-web's text and the citations the result declared.
// first is the HOST's next number; known says which number the tree already gave an idp-web number with this address (reading a
// page again must not give it a second number); limit bounds what a fallback list may add.
//
//   - No citations were declared: the text as it is, no number (WebUntouched). Nothing in it was numbered by the HOST, and the
//     caller records that.
//   - The text has the shape idp-web writes (the first source's block starts it with its number; later blocks follow a blank line,
//     start with their number and have an "address:" line naming the cited address): the number at the head of each block becomes the
//     HOST's, and every other bracketed number is neutralised (WebRewritten).
//   - Otherwise: a short list of the sources with the HOST's numbers, then the text with every bracketed number neutralised
//     (WebIndexed), so that a change in idp-web's layout costs the layout and never lets a foreign number through.
//   - Declared but all unusable: the text neutralised (WebNeutralised).
//
// Of two readings of where a block starts, the later one wins, so a block's own excerpt cannot pass off its text as the header of
// a source that comes after it. A text that reproduces a later source's header and address line verbatim inside an earlier
// source's block still cannot give a number that this result did not carry.
func RenderWeb(text string, c WebCitations, first int, known func(webN int, url string) (int, bool), limit int) WebRendered {
	if c.Declared == 0 {
		return WebRendered{Text: text, Mode: WebUntouched}
	}
	if len(c.Items) == 0 {
		t, n := Neutralise(text)
		return WebRendered{Text: t + neutralisedNote(n), NotShown: 0, Mode: WebNeutralised}
	}
	cites := c.Items
	pos, ok := headerPositions(text, cites)
	if !ok {
		return renderIndexed(text, cites, first, known, limit)
	}
	var out WebRendered
	out.Mode = WebRewritten
	out.NotShown = len(cites) - len(pos)
	var b strings.Builder
	changed := 0
	next := first
	for j := range pos {
		end := len(text)
		if j+1 < len(pos) {
			end = pos[j+1]
		}
		n, isNew := number(cites[j], next, known)
		if isNew {
			next++
		}
		src := cites[j]
		src.WebN, src.N = src.N, n
		out.Sources = append(out.Sources, src)
		token := "[" + strconv.Itoa(cites[j].N) + "]"
		seg := text[pos[j]+len(token) : end]
		neutral, k := Neutralise(seg)
		changed += k
		b.WriteString("[" + strconv.Itoa(n) + "]")
		b.WriteString(neutral)
	}
	out.Used = next - first
	out.Text = b.String() + neutralisedNote(changed)
	return out
}

// number is the HOST's number for a cited source: the one the tree already gave this address, else next (isNew).
func number(c Source, next int, known func(int, string) (int, bool)) (n int, isNew bool) {
	if known != nil {
		if n, ok := known(c.N, c.URL); ok {
			return n, false
		}
	}
	return next, true
}

func neutralisedNote(n int) string {
	if n == 0 {
		return ""
	}
	return "\n\n(Numbers in square brackets inside the text above that are not the number at the head of a source were shown as ⟦n⟧: they are the page's own, not citation numbers.)"
}

// headerPositions finds where the block of each citation starts (the longest prefix of the citations whose blocks are all found), or
// false when the first one does not start the text.
func headerPositions(text string, cites []Source) ([]int, bool) {
	if len(cites) == 0 || !strings.HasPrefix(text, "["+strconv.Itoa(cites[0].N)+"] ") {
		return nil, false
	}
	cands := make([][]int, len(cites))
	for i := 1; i < len(cites); i++ {
		cands[i] = headerCandidates(text, cites[i])
	}
	for k := len(cites); k >= 1; k-- {
		pos := make([]int, k)
		bound := len(text) + 1
		good := true
		for i := k - 1; i >= 1; i-- {
			p := -1
			for j := len(cands[i]) - 1; j >= 0; j-- {
				if cands[i][j] < bound {
					p = cands[i][j]
					break
				}
			}
			if p < 0 {
				good = false
				break
			}
			pos[i], bound = p, p
		}
		if good {
			pos[0] = 0
			return pos, true
		}
	}
	return nil, false
}

// headerCandidates are the places in text where a block of this citation could begin: after a blank line, its number and a space,
// then (the line after) "address: " and the cited address.
func headerCandidates(text string, c Source) []int {
	head := "[" + strconv.Itoa(c.N) + "] "
	addr := "address: " + c.URL
	var out []int
	for from := 0; ; {
		i := strings.Index(text[from:], "\n\n"+head)
		if i < 0 {
			return out
		}
		p := from + i + 2
		from = p
		eol := strings.IndexByte(text[p:], '\n')
		if eol < 0 {
			continue
		}
		rest := text[p+eol+1:]
		if !strings.HasPrefix(rest, addr) {
			continue
		}
		if tail := rest[len(addr):]; tail != "" && tail[0] != '\n' && tail[0] != '\r' {
			continue
		}
		out = append(out, p)
	}
}

// renderIndexed is the fallback: the HOST's numbers in a list the HOST writes, then the text, neutralised.
func renderIndexed(text string, cites []Source, first int, known func(int, string) (int, bool), limit int) WebRendered {
	budget := max(limit/4, 2048)
	var out WebRendered
	out.Mode = WebIndexed
	var b strings.Builder
	b.WriteString("Sources in this result (cite one you use as [n] with the number shown here):\n")
	next := first
	for _, c := range cites {
		title, _ := Neutralise(cleanTitle(c.Title))
		if title == "" {
			title = c.URL
		}
		line := fmt.Sprintf("[%d] %s\naddress: %s\n", 0, title, c.URL)
		if b.Len()+len(line)+8 > budget && len(out.Sources) > 0 {
			out.NotShown = len(cites) - len(out.Sources)
			break
		}
		n, isNew := number(c, next, known)
		if isNew {
			next++
		}
		src := c
		src.WebN, src.N = src.N, n
		out.Sources = append(out.Sources, src)
		fmt.Fprintf(&b, "[%d] %s\naddress: %s\n", n, title, c.URL)
	}
	out.Used = next - first
	neutral, k := Neutralise(text)
	b.WriteString("\nThe result's own text follows. Bracketed numbers in it are not citation numbers")
	if k > 0 {
		b.WriteString(" (they were shown as ⟦n⟧)")
	}
	b.WriteString(".\n\n")
	b.WriteString(neutral)
	out.Text = b.String()
	return out
}

// A bracketed number as a model might read it as a citation: a number, or several separated by commas, in square brackets or one of
// the full-width and lenticular look-alikes, with spaces allowed inside.
var bracketedNumberRE = regexp.MustCompile(`[\[［【〔][ \t]*\p{Nd}[\p{Nd} \t,;，、\-–]*[\]］】〕]`)

// Neutralise turns every bracketed number in text from a page or a document ([3], [1, 2], 【4】, [ 5 ]) into ⟦3⟧: still readable as
// what the page wrote, but not a number the model can cite and not one the HOST's check of the answer would take for a citation. It
// returns the number of groups changed. It is applied to everything a tool result holds that the HOST did not write, so that a page
// (or a document) cannot put a citation number in front of the model, which is what a fake citation list in an excerpt would be.
func Neutralise(text string) (string, int) {
	n := 0
	out := bracketedNumberRE.ReplaceAllStringFunc(text, func(m string) string {
		n++
		r := []rune(m)
		return "⟦" + string(r[1:len(r)-1]) + "⟧"
	})
	return out, n
}
