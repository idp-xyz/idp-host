package cmiclient

import (
	"strings"
	"testing"
	"time"
)

// D70: a bracketed number inside a numbered context entry is the document's, not a citation number; a context that is not numbered is as it was.
func TestANumberedContextEntryNeutralisesBracketedNumbersAndAnUnnumberedOneDoesNot(t *testing.T) {
	items := []Item{{Kind: "knowledge", ID: "d1", Source: "kb", Trust: "authoritative", Ref: "r", Title: "Annex [4]", Excerpt: "see [2] and [1, 3]", ObservedAt: time.Unix(0, 0)}}
	num, next := ContextFrom(items, 5)
	if next != 6 || !strings.HasPrefix(num[0].Description, "CMI [5] knowledge from kb") || strings.Contains(num[0].Description, "[4]") || num[0].Value != "see ⟦2⟧ and ⟦1, 3⟧" {
		t.Errorf("%+v", num)
	}
	plain, _ := ContextFrom(items, 0)
	if !strings.Contains(plain[0].Description, "Annex [4]") || plain[0].Value != "see [2] and [1, 3]" {
		t.Errorf("a release without the tools reads what it read before: %+v", plain)
	}
	srcs := KnowledgeSources(items, 5)
	if len(srcs) != 1 || srcs[0].N != 5 || srcs[0].Kind != "knowledge" || srcs[0].DocumentID != "d1" || KnowledgeSources(items, 0) != nil {
		t.Errorf("%+v", srcs)
	}
}

// The contract's lengths are for what is recorded: a cut label is at most n characters with its ellipsis, a document id is never cut (one that is
// too long is not recorded), and the place is at most 1000.
func TestWhatIsRecordedOfAPassageFitsTheContractsLengths(t *testing.T) {
	if got := OneLine(strings.Repeat("题", 400), 300); len([]rune(got)) != 300 || !strings.HasSuffix(got, "…") {
		t.Errorf("%d runes", len([]rune(got)))
	}
	if got := OneLine("short", 300); got != "short" {
		t.Error(got)
	}
	it := Item{Kind: "knowledge", ID: strings.Repeat("d", 128), Title: strings.Repeat("t", 900), Source: strings.Repeat("s", 100), Locator: &Locator{Headings: []string{strings.Repeat("h", 400), strings.Repeat("i", 400), strings.Repeat("j", 400)}}}
	src, ok := KnowledgeSource(1, it)
	if !ok || len([]rune(src.Title)) > 300 || len([]rune(src.Where)) > 1000 || len([]rune(src.Origin)) > 64 || src.DocumentID != it.ID {
		t.Errorf("%v %d %d %d", ok, len([]rune(src.Title)), len([]rune(src.Where)), len([]rune(src.Origin)))
	}
	it.ID = strings.Repeat("d", 129)
	if _, ok := KnowledgeSource(1, it); ok {
		t.Error("a document id of 129 characters was recorded, cut or not")
	}
	// the passage still has its number: the list of sources skips it, the counter does not
	if got := KnowledgeSources([]Item{it, {Kind: "knowledge", ID: "ok", Title: "t"}}, 5); len(got) != 1 || got[0].N != 6 {
		t.Errorf("%+v", got)
	}
}

func TestTheSamePlaceWithOtherTextIsAnotherPassage(t *testing.T) {
	a, _ := KnowledgeSource(1, Item{Kind: "knowledge", ID: "d", Excerpt: "Returns are accepted within 30 days."})
	b, _ := KnowledgeSource(2, Item{Kind: "knowledge", ID: "d", Excerpt: "Returns are accepted within 30 days."})
	c, _ := KnowledgeSource(3, Item{Kind: "knowledge", ID: "d", Excerpt: "Returns are accepted within 60 days."})
	if a.Digest == "" || a.Digest != b.Digest || a.Digest == c.Digest {
		t.Errorf("%q %q %q", a.Digest, b.Digest, c.Digest)
	}
}
