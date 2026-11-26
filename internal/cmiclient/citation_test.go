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
