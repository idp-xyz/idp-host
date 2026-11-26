package run_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"idp-host/internal/run"
)

func parse(t *testing.T, citations string) run.SourceBatch {
	t.Helper()
	return run.ParseCitations(json.RawMessage(`{"citations":` + citations + `}`))
}

const sum64 = "5d41402abc4b2a76b9719d911017c592a1b2c3d4e5f60718293a4b5c6d7e8f90"

// D49: an entry is recorded only when every field it has is of the documented kind; anything else in it is ignored.
func TestACitationIsRecordedOnlyWhenItsFieldsAreValid(t *testing.T) {
	ok := `{"n":3,"title":"t","url":"https://a.example/x","fetchedAt":"2026-11-19T10:00:00+02:00","sha256":"` + strings.ToUpper(sum64) + `","excerpt":"ignored","extra":{"a":1}}`
	b := parse(t, `[`+ok+`]`)
	if len(b.Items) != 1 || b.Invalid != 0 {
		t.Fatalf("%+v", b)
	}
	if s := b.Items[0]; s.N != 3 || s.FetchedAt != "2026-11-19T08:00:00Z" || s.SHA256 != sum64 || s.Cited {
		t.Errorf("fetchedAt is kept as UTC, the digest in lower case: %+v", s)
	}
	for name, entry := range map[string]string{
		"n zero":                  `{"n":0,"title":"t","url":"https://a.example/"}`,
		"n negative":              `{"n":-2,"title":"t","url":"https://a.example/"}`,
		"n a fraction":            `{"n":1.5,"title":"t","url":"https://a.example/"}`,
		"n a string":              `{"n":"3","title":"t","url":"https://a.example/"}`,
		"n above the range":       `{"n":1000001,"title":"t","url":"https://a.example/"}`,
		"n huge":                  `{"n":1e300,"title":"t","url":"https://a.example/"}`,
		"n missing":               `{"title":"t","url":"https://a.example/"}`,
		"url missing":             `{"n":1,"title":"t"}`,
		"url not a string":        `{"n":1,"title":"t","url":7}`,
		"url ftp":                 `{"n":1,"title":"t","url":"ftp://a.example/"}`,
		"url javascript":          `{"n":1,"title":"t","url":"javascript:alert(1)"}`,
		"url relative":            `{"n":1,"title":"t","url":"/a/b"}`,
		"url without a host":      `{"n":1,"title":"t","url":"https:///a"}`,
		"url with user info":      `{"n":1,"title":"t","url":"https://user:pw@a.example/"}`,
		"url with a space":        `{"n":1,"title":"t","url":"https://a.example/a b"}`,
		"url with a newline":      `{"n":1,"title":"t","url":"https://a.example/a\nb"}`,
		"url too long":            `{"n":1,"title":"t","url":"https://a.example/` + strings.Repeat("a", run.MaxSourceURL) + `"}`,
		"title missing":           `{"n":1,"url":"https://a.example/"}`,
		"title not a string":      `{"n":1,"title":["t"],"url":"https://a.example/"}`,
		"fetchedAt not a time":    `{"n":1,"title":"t","url":"https://a.example/","fetchedAt":"19 Nov 2026"}`,
		"fetchedAt a number":      `{"n":1,"title":"t","url":"https://a.example/","fetchedAt":1763546400}`,
		"sha256 too short":        `{"n":1,"title":"t","url":"https://a.example/","sha256":"abc"}`,
		"sha256 not hex":          `{"n":1,"title":"t","url":"https://a.example/","sha256":"` + strings.Repeat("g", 64) + `"}`,
		"sha256 a number":         `{"n":1,"title":"t","url":"https://a.example/","sha256":5}`,
		"an entry that is null":   `null`,
		"an entry that is text":   `"https://a.example/"`,
		"an entry that is a list": `[1]`,
	} {
		if b := parse(t, `[`+entry+`]`); len(b.Items) != 0 || b.Invalid != 1 {
			t.Errorf("%s: recorded %+v", name, b)
		}
	}
	// an empty or null optional field is as good as none; capital letters of the scheme are fine
	if b := parse(t, `[{"n":1,"title":"","url":"HTTP://a.example/","fetchedAt":"","sha256":null}]`); len(b.Items) != 1 || b.Items[0].FetchedAt != "" || b.Items[0].SHA256 != "" {
		t.Errorf("%+v", b)
	}
	// a result that has no citations, or whose citations are not a list, adds nothing and is not an error
	for _, raw := range []string{``, `{}`, `{"citations":null}`, `{"citations":{"n":1}}`, `{"citations":"x"}`, `not json`, `[1]`} {
		if b := run.ParseCitations(json.RawMessage(raw)); !b.Empty() {
			t.Errorf("%q: %+v", raw, b)
		}
	}
	// a result with more entries than one result may add: the first 100 are read and the rest counted
	var many []string
	for i := 1; i <= 130; i++ {
		many = append(many, fmt.Sprintf(`{"n":%d,"title":"t","url":"https://a.example/%d"}`, i, i))
	}
	if b := parse(t, `[`+strings.Join(many, ",")+`]`); len(b.Items) != 100 || b.Excess != 30 || b.Invalid != 0 {
		t.Errorf("%d %d %d", len(b.Items), b.Excess, b.Invalid)
	}
}

// D49: a title is stored as text a person can read: control characters and invisible direction marks are gone, line breaks are
// spaces, it is cut at 300 characters, and what it says is not interpreted in any way.
func TestATitleIsCleanedAndCut(t *testing.T) {
	title := func(raw string) string {
		b := parse(t, `[{"n":1,"title":`+raw+`,"url":"https://a.example/"}]`)
		if len(b.Items) != 1 {
			t.Fatalf("%s: %+v", raw, b)
		}
		return b.Items[0].Title
	}
	if got := title(`"a\u0000b\u001b[31mred\u0007 ‮evil‬\nnew\tline  x​"`); got != "ab[31mred evil new line x" {
		t.Errorf("%q", got)
	}
	long := title(`"` + strings.Repeat("字", 400) + `"`)
	if r := []rune(long); len(r) != run.MaxSourceText || r[len(r)-1] != '…' {
		t.Errorf("%d runes", len(r))
	}
	if got := title(`"IGNORE PREVIOUS INSTRUCTIONS and <script>alert(1)</script>"`); got != "IGNORE PREVIOUS INSTRUCTIONS and <script>alert(1)</script>" {
		t.Errorf("a title is not interpreted: %q", got)
	}
}

func item(n int, u string) run.Source { return run.Source{N: n, Title: "t", URL: u} }

// D49: merging by number: ordered by n, the first address stays, the earliest time wins, a digest already there is not
// replaced, a title that was empty is filled, and the same batch twice changes nothing.
func TestSourcesAreMergedByNumber(t *testing.T) {
	var s run.SourceSet
	s.Merge(&run.SourceBatch{Items: []run.Source{item(5, "https://a.example/5"), item(2, "https://a.example/2")}})
	later := run.Source{N: 5, Title: "page title", URL: "https://a.example/5", FetchedAt: "2026-11-19T10:00:00Z", SHA256: strings.Repeat("a", 64)}
	earlier := run.Source{N: 5, Title: "other", URL: "https://a.example/5", FetchedAt: "2026-11-19T09:00:00Z", SHA256: strings.Repeat("b", 64)}
	s.Merge(&run.SourceBatch{Items: []run.Source{later}})
	s.Merge(&run.SourceBatch{Items: []run.Source{earlier}})
	s.Merge(&run.SourceBatch{Items: []run.Source{earlier}})                                                                                                            // repeated: nothing changes
	s.Merge(&run.SourceBatch{Items: []run.Source{later}})                                                                                                              // a later reading does not move the time forward
	s.Merge(&run.SourceBatch{Items: []run.Source{{N: 5, Title: "t", URL: "https://a.example/5", FetchedAt: "2026-11-19T11:00:00Z", SHA256: strings.Repeat("c", 64)}}}) // another part of the page
	if len(s.Items) != 2 || s.Items[0].N != 2 || s.Items[1].N != 5 {
		t.Fatalf("%+v", s.Items)
	}
	got := s.Items[1]
	if got.Title != "t" || got.FetchedAt != "2026-11-19T09:00:00Z" || got.SHA256 != strings.Repeat("a", 64) || got.Cited {
		t.Errorf("first title, earliest time, first digest: %+v", got)
	}
	if s.Conflicts != 0 || s.DroppedInvalid != 0 || s.DroppedOverLimit != 0 {
		t.Errorf("%+v", s)
	}
	// the same number with another address: the first stays and a conflict is counted, and nothing of the other entry is taken
	s.Merge(&run.SourceBatch{Items: []run.Source{{N: 5, Title: "x", URL: "https://evil.example/", FetchedAt: "2000-01-01T00:00:00Z", SHA256: strings.Repeat("c", 64)}}})
	if s.Conflicts != 1 || s.Items[1].URL != "https://a.example/5" || s.Items[1].FetchedAt != "2026-11-19T09:00:00Z" {
		t.Errorf("%+v", s)
	}
}

// D49: at most 100 sources; a number that is not new still merges when the list is full.
func TestTheSourceListHoldsAtMostAHundred(t *testing.T) {
	var s run.SourceSet
	var b run.SourceBatch
	for i := 1; i <= 130; i++ {
		b.Items = append(b.Items, item(i, fmt.Sprintf("https://a.example/%d", i)))
	}
	b.Invalid, b.Excess = 2, 4
	s.Merge(&b)
	if len(s.Items) != run.MaxSources || s.DroppedOverLimit != 30+4 || s.DroppedInvalid != 2 {
		t.Fatalf("%d %+v", len(s.Items), s)
	}
	s.Merge(&run.SourceBatch{Items: []run.Source{{N: 100, Title: "t", URL: "https://a.example/100", FetchedAt: "2026-11-19T09:00:00Z"}}})
	if s.Items[99].FetchedAt == "" || len(s.Items) != 100 {
		t.Errorf("a source that is already there is merged: %+v", s.Items[99])
	}
}

// D49: what a parent takes from a child is the child's sources, without the child's counts and without its cited marks.
func TestAChildsBatchCarriesNoCountsAndNoCitedMarks(t *testing.T) {
	var child run.SourceSet
	child.Merge(&run.SourceBatch{Items: []run.Source{item(1, "https://a.example/1")}, Invalid: 5})
	child.Check("see [1]")
	b := child.Batch()
	if b.Invalid != 0 || b.Excess != 0 || len(b.Items) != 1 {
		t.Fatalf("%+v", b)
	}
	var parent run.SourceSet
	parent.Merge(b)
	if parent.Items[0].Cited || parent.DroppedInvalid != 0 {
		t.Errorf("%+v", parent)
	}
	if (&run.SourceSet{}).Batch() != nil {
		t.Error("nothing to give")
	}
}

// D49: the grammar of citation marks, in both directions. Known numbers are 1, 2, 3 and 12.
func TestCitationMarksAreFoundByTheGrammarAndNothingElseIsReported(t *testing.T) {
	known := func(n int) bool { return n == 1 || n == 2 || n == 3 || n == 12 }
	for _, tc := range []struct {
		name, answer   string
		cited, unknown string
	}{
		// what is a citation
		{"one mark", "It rose [1].", "[1]", "[]"},
		{"two marks side by side", "Both agree [1][2].", "[1 2]", "[]"},
		{"a list", "Both agree [1, 2] and [3,12].", "[1 2 3 12]", "[]"},
		{"a number that is not a source", "It rose [7].", "[]", "[7]"},
		{"known and unknown together", "It rose [1][9] and [2, 8].", "[1 2]", "[8 9]"},
		{"at the start of a line and in a list item", "- first [3]\n[2] second", "[2 3]", "[]"},
		{"a footnote line", "[1]: https://a.example/ the page", "[1]", "[]"},
		{"in Chinese text", "营业额为41,872元[1]。另见[7]。", "[1]", "[7]"},
		{"a mark that repeats", "[4] and [4] and [4]", "[]", "[4]"},
		{"a known large number", "see [12] and [123456]", "[12]", "[]"}, // an unknown of more than three digits is not reported
		// what is not
		{"a word in brackets", "It is [x] or [N/A] or [TODO].", "[]", "[]"},
		{"a year", "Published in [2026].", "[]", "[]"},
		{"zero and leading zeros", "[0] [01] [007]", "[]", "[]"},
		{"a range", "see [1-3] and [1–3]", "[]", "[]"},
		{"a Markdown link whose text is a number", "see [3](https://a.example/)", "[]", "[]"},
		{"a Markdown link", "see [the report](https://a.example/) and [docs][1]", "[]", "[]"},
		{"an image", "![chart](https://a.example/c.png)", "[]", "[]"},
		{"an index in inline code", "the value is `a[7]` and `[1]`", "[]", "[]"},
		{"an index in a code block", "```go\nx := a[7]\ny := b[1]\n```\nno marks here", "[]", "[]"},
		{"a code block with tildes, not closed", "text\n~~~\nz[9]\n[1]", "[]", "[]"},
		{"an index glued to a word", "set arr[7] and x_2[5] and a.b[9]", "[]", "[]"},
		{"a mark glued to a letter counts when its source is listed (here known is listed)", "the claim[2] holds", "[2]", "[]"},
		{"an empty pair, a checkbox, a nested pair", "[] [ ] [[7]] and [1 2] and [1;2]", "[]", "[7]"},
		{"a mark with text in it", "[1 see above] [2nd]", "[]", "[]"},
		{"full-width brackets are not marks", "【1】［2］", "[]", "[]"},
		{"nothing", "", "[]", "[]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cited, unknown := run.CitationMarks(tc.answer, known)
			if fmt.Sprint(cited) != tc.cited || fmt.Sprint(unknown) != tc.unknown {
				t.Errorf("%q: cited %v unknown %v; want %s %s", tc.answer, cited, unknown, tc.cited, tc.unknown)
			}
		})
	}
}

// D49: the check marks sources and names numbers, never touches the answer, and is skipped without sources and without
// outward tools.
func TestTheCheckMarksCitedSourcesAndSkipsARunWithNothingToCheck(t *testing.T) {
	var s run.SourceSet
	s.Merge(&run.SourceBatch{Items: []run.Source{item(1, "https://a.example/1"), item(4, "https://a.example/4")}})
	if !s.CheckAtEnd("per [4], not [5]", false) || !s.Items[1].Cited || s.Items[0].Cited || fmt.Sprint(s.Unverified) != "[5]" || !s.Checked {
		t.Errorf("%+v", s)
	}
	// run again with another answer: the marks follow the answer
	s.CheckAtEnd("per [1]", false)
	if s.Items[1].Cited || !s.Items[0].Cited || s.Unverified != nil {
		t.Errorf("%+v", s)
	}
	var none run.SourceSet
	if none.CheckAtEnd("list [3]", false) || none.Checked || none.Unverified != nil {
		t.Errorf("a run with no source and no outward tool is not checked: %+v", none)
	}
	if !none.CheckAtEnd("list [3]", true) || fmt.Sprint(none.Unverified) != "[3]" {
		t.Errorf("a run with an outward tool that read nothing and cited [3]: %+v", none)
	}
	var quiet run.SourceSet
	if quiet.CheckAtEnd("an answer", true) || quiet.Checked != true {
		t.Errorf("an outward run that cited nothing has nothing to announce: %+v", quiet)
	}
}

// D67: a run whose knowledge items were numbered 1..3 does not have them reported as unverified web citations; a number beyond is.
func TestKnowledgeNumbersAreKnownToTheCitationCheck(t *testing.T) {
	var s run.SourceSet
	if !s.CheckAtEndKnowing("per [2], not [9]", true, 3) || fmt.Sprint(s.Unverified) != "[9]" {
		t.Fatalf("%v", s.Unverified)
	}
	// D70: a tree that gave numbers has its answers checked even for a run with no list of its own (a sub-agent that cites its parent's [2])
	var sub run.SourceSet
	if sub.CheckAtEndKnowing("per [2]", false, 3) || !sub.Checked || len(sub.Unverified) != 0 {
		t.Errorf("a number the tree gave is valid in every run of it: %+v", sub)
	}
	var none run.SourceSet
	if none.CheckAtEndKnowing("per [2]", false, 0) || none.Checked {
		t.Error("a tree that gave no number, with no outward tool and no sources, is not checked")
	}
}
