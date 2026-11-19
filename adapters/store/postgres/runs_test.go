package postgres

import (
	"testing"

	"idp-host/internal/agui"
)

// D44: the replay hash drops the CMI entries at the tail of the context; after screening that tail is the kept entries
// plus one note, which "appended" counts, while "items" keeps its meaning (what the CMI returned). Runs created before
// D44 have no "appended".
func TestContextItemsReadsTheNumberOfEntriesAppended(t *testing.T) {
	ev := func(v map[string]any) []agui.Event { return []agui.Event{agui.Custom(agui.CustomContextResolved, v)} }
	if n := contextItems(ev(map[string]any{"items": 5, "appended": 3})); n != 3 {
		t.Fatalf("screened: %d", n)
	}
	if n := contextItems(ev(map[string]any{"items": 5})); n != 5 {
		t.Fatalf("an event from before D44: %d", n)
	}
	if n := contextItems([]agui.Event{agui.Custom(agui.CustomContextSkipped, map[string]any{"reason": "x"})}); n != 0 {
		t.Fatalf("%d", n)
	}
}
