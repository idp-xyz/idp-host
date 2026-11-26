package run

// The web sources a run's tools returned and the check of the answer's citation marks against them (ADR-0001 D49; platform
// ADR-0003 D9 and its appendix D, 4.1). Everything here is data about the run: a title comes from a web page and is never
// given to a model, so nothing in this file produces text for a prompt.

import (
	"encoding/json"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Bounds on what is recorded (D49).
const (
	MaxSources    = 100       // sources of one run; beyond that entries are dropped and counted
	MaxSourceN    = 1_000_000 // the largest citation number taken as sane: idp-web numbers per caller and keeps counting
	MaxSourceURL  = 2048      // bytes of an address
	MaxSourceText = 300       // runes of a title
	maxScanned    = 100       // entries read from one tool result; the rest are counted as over the limit
)

// Source is one web source a run was given: what the member checks an answer against. Cited says whether the final answer
// carries its number. Only titles and addresses are kept, never page text or excerpts (D49, provider terms).
type Source struct {
	N         int    `json:"n"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	FetchedAt string `json:"fetchedAt,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Cited     bool   `json:"cited"`
}

// SourceBatch is what one tool result (or one finished sub-agent run) adds to a run's sources: the valid entries, and how
// many entries were dropped as invalid or past the per-result scan limit.
type SourceBatch struct {
	Items   []Source `json:"items"`
	Invalid int      `json:"invalid,omitempty"`
	Excess  int      `json:"excess,omitempty"`
}

// Empty reports whether the batch changes nothing.
func (b *SourceBatch) Empty() bool {
	return b == nil || len(b.Items) == 0 && b.Invalid == 0 && b.Excess == 0
}

// SourceSet is a run's sources and the counts of what was not recorded, as stored with the run.
type SourceSet struct {
	Items            []Source `json:"items"`
	DroppedInvalid   int      `json:"droppedInvalid,omitempty"`
	DroppedOverLimit int      `json:"droppedOverLimit,omitempty"`
	Conflicts        int      `json:"conflicts,omitempty"` // a number given again with another address; the first address was kept
	Checked          bool     `json:"checked,omitempty"`   // the citation check ran when the run completed
	Unverified       []int    `json:"unverified,omitempty"`
}

// ParseCitations reads structuredContent.citations of a tool result (platform ADR-0003 D9). An entry that does not have the
// documented shape is dropped and counted, never an error: the tool's text result goes to the model as before whatever its
// structured part says. A structuredContent without a citations array adds nothing.
func ParseCitations(structured json.RawMessage) SourceBatch {
	var doc struct {
		Citations json.RawMessage `json:"citations"`
	}
	var b SourceBatch
	if len(structured) == 0 || json.Unmarshal(structured, &doc) != nil || len(doc.Citations) == 0 {
		return b
	}
	var entries []json.RawMessage
	if json.Unmarshal(doc.Citations, &entries) != nil {
		return b
	}
	for i, raw := range entries {
		if i >= maxScanned {
			b.Excess += len(entries) - maxScanned
			break
		}
		if s, ok := parseCitation(raw); ok {
			b.Items = append(b.Items, s)
		} else {
			b.Invalid++
		}
	}
	return b
}

// parseCitation validates one entry: n a positive integer up to MaxSourceN; url http or https, at most MaxSourceURL bytes, no
// user information, no control character or space; title a string (control characters removed, truncated); fetchedAt, when
// present, RFC 3339 (kept as UTC); sha256, when present, 64 hex digits (kept lower case). Other fields are ignored.
func parseCitation(raw json.RawMessage) (Source, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return Source{}, false
	}
	var s Source
	n, ok := integer(m["n"])
	if !ok || n < 1 || n > MaxSourceN {
		return Source{}, false
	}
	s.N = n
	var u, title string
	if json.Unmarshal(m["url"], &u) != nil || !validURL(u) {
		return Source{}, false
	}
	s.URL = u
	if json.Unmarshal(m["title"], &title) != nil {
		return Source{}, false
	}
	s.Title = cleanTitle(title)
	if v, present, ok := optionalString(m["fetchedAt"]); !ok {
		return Source{}, false
	} else if present {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return Source{}, false
		}
		s.FetchedAt = t.UTC().Format(time.RFC3339)
	}
	if v, present, ok := optionalString(m["sha256"]); !ok {
		return Source{}, false
	} else if present {
		if !hexDigestRE.MatchString(v) {
			return Source{}, false
		}
		s.SHA256 = strings.ToLower(v)
	}
	return s, true
}

var hexDigestRE = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// integer reads a JSON number that is a whole number; a string, a fraction or an exponent form that is not whole is not.
func integer(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 || raw[0] == '"' {
		return 0, false
	}
	var f float64
	if json.Unmarshal(raw, &f) != nil || f < -MaxSourceN || f > MaxSourceN || f != float64(int64(f)) {
		return 0, false
	}
	return int(f), true
}

// optionalString reads a field that may be missing (or an empty string, which idp-web never sends): present reports whether
// it has a value, ok false when it is there with another type.
func optionalString(raw json.RawMessage) (v string, present, ok bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false, true
	}
	if json.Unmarshal(raw, &v) != nil {
		return "", false, false
	}
	return v, v != "", true
}

func validURL(s string) bool {
	if s == "" || len(s) > MaxSourceURL || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r <= ' ' || unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.Hostname() == "" || u.User != nil {
		return false
	}
	return strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https")
}

// cleanTitle makes a title safe to store and show as text: line breaks and tabs become spaces, every other control character
// and every format character (the bidirectional overrides among them) is removed, runs of spaces collapse, and the result is
// cut at MaxSourceText runes with an ellipsis. It does not try to find instructions in the title: a title is never given to a
// model, so there is nothing to defend (D49).
func cleanTitle(s string) string {
	s = strings.ToValidUTF8(s, "")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t' || unicode.IsSpace(r):
			b.WriteRune(' ')
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if utf8.RuneCountInString(out) > MaxSourceText {
		out = string([]rune(out)[:MaxSourceText-1]) + "…"
	}
	return out
}

// Merge adds a batch to the set, by number. A number already there is merged: the first address and the first non-empty
// title stay, the earliest fetchedAt wins, and a sha256 that is already there is not replaced (it is the digest of the first
// segment of the page the run read; a page read in several parts has one digest per part and the set keeps the first). A
// number given again with another address keeps the first and counts a conflict: it should not happen, except that idp-web
// cites the address a page ended up at after redirects while numbering the address that was asked for. A new number past
// MaxSources is not recorded and counts as over the limit. Merging the same batch twice changes nothing but the counts.
func (s *SourceSet) Merge(b *SourceBatch) {
	if b == nil {
		return
	}
	s.DroppedInvalid += b.Invalid
	s.DroppedOverLimit += b.Excess
	for _, in := range b.Items {
		in.Cited = false
		i := sort.Search(len(s.Items), func(i int) bool { return s.Items[i].N >= in.N })
		if i < len(s.Items) && s.Items[i].N == in.N {
			have := &s.Items[i]
			if have.URL != in.URL {
				s.Conflicts++
				continue
			}
			if have.Title == "" {
				have.Title = in.Title
			}
			if in.FetchedAt != "" && (have.FetchedAt == "" || earlier(in.FetchedAt, have.FetchedAt)) {
				have.FetchedAt = in.FetchedAt
			}
			if have.SHA256 == "" {
				have.SHA256 = in.SHA256
			}
			continue
		}
		if len(s.Items) >= MaxSources {
			s.DroppedOverLimit++
			continue
		}
		s.Items = append(s.Items, Source{})
		copy(s.Items[i+1:], s.Items[i:])
		s.Items[i] = in
	}
}

func earlier(a, b string) bool {
	ta, errA := time.Parse(time.RFC3339, a)
	tb, errB := time.Parse(time.RFC3339, b)
	return errA == nil && errB == nil && ta.Before(tb)
}

// Batch returns the set as a batch for a parent run: its entries, without the counts, which belong to the run that dropped
// them.
func (s *SourceSet) Batch() *SourceBatch {
	if s == nil || len(s.Items) == 0 {
		return nil
	}
	return &SourceBatch{Items: append([]Source(nil), s.Items...)}
}

// Check compares the citation marks of the final answer with the set (D49): a source whose number is cited gets Cited, and
// a number that is not among the sources is recorded as unverified. It marks and never changes the answer. It only finds a
// number that does not exist, not a source that exists and does not support the sentence.
func (s *SourceSet) Check(answer string) { s.checkKnowing(answer, 0) }

// checkKnowing is Check for a run that also numbered knowledge items 1..knowledge (D67): those numbers exist, so they are not
// reported as unverified. They are not web sources and are never marked cited here.
func (s *SourceSet) checkKnowing(answer string, knowledge int) {
	known := map[int]int{}
	for i := range s.Items {
		s.Items[i].Cited = false
		known[s.Items[i].N] = i
	}
	cited, unknown := CitationMarks(answer, func(n int) bool { _, ok := known[n]; return ok || (n >= 1 && n <= knowledge) })
	for _, n := range cited {
		if i, ok := known[n]; ok {
			s.Items[i].Cited = true
		}
	}
	s.Checked = true
	s.Unverified = unknown
}

// A group of numbers in square brackets: [3], [1, 2]. Numbers have no leading zero and at most six digits.
var markRE = regexp.MustCompile(`\[([1-9][0-9]{0,5}(?:[ \t]*,[ \t]*[1-9][0-9]{0,5})*)\]`)

var inlineCodeRE = regexp.MustCompile("`[^`\n]*`")

// withoutCode blanks fenced code blocks (a line that starts with three backticks or tildes opens one, the next such line
// closes it; an unclosed block runs to the end) and inline code spans, where [1] is an index and not a citation.
func withoutCode(s string) string {
	var b strings.Builder
	fenced := false
	for _, line := range strings.SplitAfter(s, "\n") {
		t := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fenced = !fenced
			b.WriteString("\n")
			continue
		}
		if fenced {
			b.WriteString("\n")
			continue
		}
		b.WriteString(line)
	}
	return inlineCodeRE.ReplaceAllString(b.String(), " ")
}

// maxUnverifiedDigits: an unknown number is reported only when it has at most this many digits. A year ([2026]) or a larger
// number in brackets is not clearly a citation, while a known number is a citation whatever its size (idp-web numbers per
// caller and the count can pass 999).
const maxUnverifiedDigits = 3

// CitationMarks finds the citation marks of an answer (D49): the numbers it cites that known accepts (cited) and the ones it
// does not (unknown), each sorted and without repeats. The grammar, written for the skill text of the web agent ("cite each
// source as [n]"):
//   - [n] and [n, m]: one or more positive integers without a leading zero in square brackets, separated by commas. [1][2]
//     is two marks. A range such as [1-3] is not a mark.
//   - Not marks, whatever the numbers: anything inside a code block (fenced or inline); [n](...) and [n][...] preceded
//     by a bracketed text, which are Markdown links; [x], [2026], [ ] and anything else that is not made of those numbers.
//   - A group glued to a word ("array[1]") is a citation only if it names a known source, and is never reported as
//     unknown, because it reads as an index. A number of more than three digits is never reported as unknown.
func CitationMarks(answer string, known func(int) bool) (cited, unknown []int) {
	text := withoutCode(answer)
	seenCited, seenUnknown := map[int]bool{}, map[int]bool{}
	for _, loc := range markRE.FindAllStringSubmatchIndex(text, -1) {
		start, end := loc[0], loc[1]
		if end < len(text) && text[end] == '(' {
			continue // a Markdown link
		}
		glued := false
		if start > 0 {
			switch prev := text[start-1]; {
			case prev == ']':
				if !followsNumericMark(text, start) {
					continue // [text][1]: a reference link
				}
			case prev >= 'a' && prev <= 'z' || prev >= 'A' && prev <= 'Z' || prev >= '0' && prev <= '9' || prev == '_':
				glued = true
			}
		}
		for _, f := range strings.FieldsFunc(text[loc[2]:loc[3]], func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
			n, err := strconv.Atoi(f)
			if err != nil {
				continue
			}
			switch {
			case known(n):
				seenCited[n] = true
			case !glued && len(f) <= maxUnverifiedDigits:
				seenUnknown[n] = true
			}
		}
	}
	return keys(seenCited), keys(seenUnknown)
}

// followsNumericMark reports whether the "]" just before start closes a group of numbers ([1][2]) and not a text in brackets.
func followsNumericMark(text string, start int) bool {
	open := strings.LastIndexByte(text[:start-1], '[')
	if open < 0 || strings.ContainsRune(text[open:start], '\n') {
		return false
	}
	return markRE.MatchString(text[open:start]) && markRE.FindString(text[open:start]) == text[open:start]
}

func keys(m map[int]bool) []int {
	var out []int
	for n := range m {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

// Event is the payload of idp.host/run.sources, the record at the end of a run (D49): the full list and what was not
// recorded.
func (s *SourceSet) Event() map[string]any {
	items := s.Items
	if items == nil {
		items = []Source{}
	}
	unverified := s.Unverified
	if unverified == nil {
		unverified = []int{}
	}
	return map[string]any{"sources": items, "unverifiedCitations": unverified, "droppedInvalid": s.DroppedInvalid, "droppedOverLimit": s.DroppedOverLimit, "conflicts": s.Conflicts, "checked": s.Checked}
}

// Worth reports whether the end of the run has something to record: sources, an unverified number, or entries that were
// dropped.
func (s *SourceSet) Worth() bool {
	return len(s.Items) > 0 || len(s.Unverified) > 0 || s.DroppedInvalid > 0 || s.DroppedOverLimit > 0 || s.Conflicts > 0
}

// CheckAtEnd is the check a completing run makes (D49). A run without sources whose release has no outward tool skips it
// entirely: a bracketed number in such an answer is not a citation. It reports whether there is anything to store and to
// announce (see Worth).
func (s *SourceSet) CheckAtEnd(answer string, outward bool) bool {
	return s.CheckAtEndKnowing(answer, outward, 0)
}

// CheckAtEndKnowing is CheckAtEnd for a run whose knowledge items were numbered 1..knowledge (D67).
func (s *SourceSet) CheckAtEndKnowing(answer string, outward bool, knowledge int) bool {
	if len(s.Items) == 0 && !outward {
		return s.Worth()
	}
	s.checkKnowing(answer, knowledge)
	return s.Worth()
}
