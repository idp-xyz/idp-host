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
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
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
	Declared int  // entries in the array, valid or not
	Present  bool // the result has a citations array, even an empty one
	Cut      bool // the text was cut for D40's limit before the HOST saw it: a header in it proves nothing about what was cut off
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
	out.Declared, out.Present = len(entries), true
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
		return WebRendered{Text: capText(t+neutralisedNote(n), limit), Mode: WebNeutralised}
	}
	cites := c.Items
	pos, ok := headerPositions(text, cites)
	if !ok || c.Cut { // a text the limit cut proves nothing about the blocks that were cut off: whatever header is left may be a forged one
		return renderIndexed(text, cites, first, known, limit)
	}
	type block struct {
		text  string
		isNew bool
		src   Source
	}
	blocks := make([]block, 0, len(pos))
	changed, next := 0, first
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
		token := "[" + strconv.Itoa(cites[j].N) + "]"
		neutral, k := Neutralise(text[pos[j]+len(token) : end])
		changed += k
		blocks = append(blocks, block{"[" + strconv.Itoa(n) + "]" + neutral, isNew, src})
	}
	// D40 applies to what the model is given: after the numbers are written and the page's own are neutralised (which makes each of them
	// longer). Whole blocks from the end are left out until it fits; what is left out is not numbered.
	size := func() int {
		t := 0
		for _, b := range blocks {
			t += len(b.text)
		}
		if changed > 0 {
			t += len(neutralisedNote(1))
		}
		return t
	}
	out := WebRendered{Mode: WebRewritten}
	for limit > 0 && len(blocks) > 1 && size() > limit {
		blocks = blocks[:len(blocks)-1]
		out.NotShown++
	}
	var b strings.Builder
	for _, bl := range blocks {
		b.WriteString(bl.text)
		out.Sources = append(out.Sources, bl.src)
		if bl.isNew {
			out.Used++
		}
	}
	// idp-web's head line names the offset where ITS part of the page ends; a text cut here ends earlier, so the offset is no way to continue
	out.Text = capTextWith(b.String()+neutralisedNote(changed), limit, func(cut, total int) string {
		return fmt.Sprintf("\n[truncated here: %d of %d bytes shown; do not continue from the offset in the first line, it is past what you were shown]", cut, total)
	})
	return out
}

// capText cuts s to at most limit bytes at a character boundary, and says so; limit 0 is no limit.
func capText(s string, limit int) string {
	return capTextWith(s, limit, func(cut, total int) string {
		return fmt.Sprintf("\n[truncated: %d of %d bytes shown]", cut, total)
	})
}

// capTextWith is capText with the note it ends with: the result, note included, is at most limit bytes.
func capTextWith(s string, limit int, note func(cut, total int) string) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	cut := max(limit-len(note(limit, len(s))), 0) // the note with the longest numbers it can have
	for cut > 0 && cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut--
	}
	out := s[:cut] + note(cut, len(s))
	if len(out) > limit {
		return out[:limit]
	}
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

// headerPositions finds where the block of each citation starts, or false when the text is not plainly what idp-web writes: the first
// block does not start the text, or some block's header is missing (a middle one, one cut off by D40's limit) or found more than once
// (a block's own excerpt repeats a later block's header and address), or they are not in the order of the citations. Where there is any
// ambiguity the text is not rewritten; the caller lists the sources and neutralises the text instead.
func headerPositions(text string, cites []Source) ([]int, bool) {
	if len(cites) == 0 || !strings.HasPrefix(text, "["+strconv.Itoa(cites[0].N)+"] ") {
		return nil, false
	}
	pos := make([]int, len(cites))
	for i := 1; i < len(cites); i++ {
		c := headerCandidates(text, cites[i])
		if len(c) != 1 || c[0] <= pos[i-1] {
			return nil, false
		}
		pos[i] = c[0]
	}
	// the first block's own number must not come back as a header either
	if len(headerCandidates(text, cites[0])) != 0 {
		return nil, false
	}
	return pos, true
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
	head := b.Len()
	b.WriteString(neutral)
	out.Text = b.String()
	if limit > 0 && len(out.Text) > limit {
		if limit-head > 120 {
			out.Text = out.Text[:head] + capText(neutral, limit-head)
		} else {
			out.Text = capText(out.Text, limit)
		}
	}
	return out
}

// NeutraliseText is Neutralise for a whole text that the model reads: with the note that says what ⟦n⟧ is, and within limit (D40).
func NeutraliseText(text string, limit int) string {
	t, n := Neutralise(text)
	return capText(t+neutralisedNote(n), limit)
}

// The brackets a model might read as square brackets around a citation: the ASCII ones, the full-width and lenticular look-alikes and
// the other Ps/Pe pairs of Unicode that are drawn as square brackets. Any opening one may be closed by any closing one.
const openBrackets = "[［【〔〖〘〚⦋⦍⦏﹝❲⁅⟬"
const closeBrackets = "]］】〕〗〙〛⦌⦎⦐﹞❳⁆⟭"

// inBracket is what may stand between the brackets of a number as a model reads it: digits of any script and the other numerals
// (superscripts), spaces of any kind (NBSP and the ideographic space too), combining marks (the keycap sequence 3 U+FE0F U+20E3 is a mark
// after a digit), the invisible format characters (zero-width and the like), the separators, dashes and Markdown emphasis marks.
func inBracket(r rune) bool {
	return unicode.IsNumber(r) || unicode.IsSpace(r) || unicode.Is(unicode.M, r) || unicode.Is(unicode.Cf, r) || strings.ContainsRune(",;，、；*_`-‐‑‒–—―~", r)
}

// The brackets as HTML character references, which a model reads as the brackets they stand for: numeric (decimal and hexadecimal) and named.
var openEntities = []string{"&#91;", "&#x5b;", "&lsqb;", "&lbrack;"}
var closeEntities = []string{"&#93;", "&#x5d;", "&rsqb;", "&rbrack;"}

// bracketAt is how many characters the opening (or closing) bracket at rs[i] takes: 1 for a bracket, 2 for one with a backslash before it,
// the length of a character reference for one, 0 when there is none.
func bracketAt(rs []rune, i int, brackets string, entities []string) int {
	if i >= len(rs) {
		return 0
	}
	switch {
	case rs[i] == '\\' && i+1 < len(rs) && strings.ContainsRune(brackets, rs[i+1]):
		return 2
	case strings.ContainsRune(brackets, rs[i]):
		return 1
	case rs[i] == '&':
		for _, e := range entities {
			if i+len(e) <= len(rs) && strings.EqualFold(string(rs[i:i+len(e)]), e) {
				return len(e)
			}
		}
	}
	return 0
}

// Neutralise turns every bracketed number in text from a page or a document ([3], [1, 2], 【4】, [ 5 ], \[3\], [**3**], [３], [³], [3-keycap],
// [3–4], &#91;3&#93;, [\u200b3]) into ⟦3⟧: still readable as what the page wrote, but not a number the model can cite and not one the HOST's
// check of the answer would take for a citation. A group is a bracket (escaped, or a character reference) that opens, at least one numeral
// and only numerals, spaces, marks, invisible characters, separators, dashes and emphasis marks, and a bracket that closes; the matching is
// done on the text as it is, character by character, so that nothing that reads as a number slips through by being written another way. It
// returns the number of groups changed. It is applied to everything a tool result holds that the HOST did not write, so that a page (or a
// document) cannot put a citation number in front of the model, which is what a fake citation list in an excerpt would be. The ⟦ ⟧ of its
// own output are not brackets of this kind, so it can be applied twice.
func Neutralise(text string) (string, int) {
	if !strings.ContainsAny(text, openBrackets+"&") {
		return text, 0
	}
	rs := []rune(text)
	var b strings.Builder
	n := 0
	for i := 0; i < len(rs); {
		if open := bracketAt(rs, i, openBrackets, openEntities); open > 0 {
			j := i + open
			k, digits := j, false
			for k < len(rs) && inBracket(rs[k]) {
				digits = digits || unicode.IsNumber(rs[k])
				k++
			}
			if closing := bracketAt(rs, k, closeBrackets, closeEntities); digits && closing > 0 {
				b.WriteString("⟦")
				b.WriteString(string(rs[j:k]))
				b.WriteString("⟧")
				n++
				i = k + closing
				continue
			}
		}
		b.WriteRune(rs[i])
		i++
	}
	return b.String(), n
}
