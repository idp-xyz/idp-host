package cmiclient

import (
	"fmt"
	"sort"
	"strings"

	"idp-host/internal/agui"
	"idp-host/internal/run"
)

// ContextCMIWithheld is the description of the context entry that tells the model how many CMI entries it was not given,
// so that it can say so instead of guessing (D44). Its value carries counts and levels only, never what was withheld.
const ContextCMIWithheld = "idp.host/cmi.withheld"

// LevelUnknown is how the count of entries whose sensitivity is empty or not one of the CMI's levels is reported. The
// value itself is not repeated: it came from outside and goes into text the model reads.
const LevelUnknown = "unknown"

// Screening is a package split by a sensitivity ceiling.
type Screening struct {
	Kept     []Item
	Withheld int
	ByLevel  map[string]int
	Ceiling  string
}

// Screen splits the items of a package into those at or below the ceiling and those above it (D44; platform ADR-0003
// D7). An entry whose sensitivity is empty or not a known level counts as above the ceiling: unlike a member's
// attachment, where no level means internal, an entry the CMI sent without a valid level is a fault to fail closed on.
func Screen(items []Item, ceiling string) Screening {
	s := Screening{ByLevel: map[string]int{}, Ceiling: ceiling}
	limit, ok := run.SensitivityRank(ceiling)
	if !ok {
		limit = -1 // an unusable ceiling lets nothing through
	}
	for _, it := range items {
		rank, known := run.SensitivityRank(it.Sensitivity)
		if it.Sensitivity != "" && known && rank <= limit {
			s.Kept = append(s.Kept, it)
			continue
		}
		level := it.Sensitivity
		if it.Sensitivity == "" || !known {
			level = LevelUnknown
		}
		s.Withheld++
		s.ByLevel[level]++
	}
	return s
}

// Context is what the run is given: one entry per kept item and, when anything was withheld, one note. Its length is the
// number of entries appended to the run's context.
func (s Screening) Context() []agui.Context {
	out, _ := s.ContextFrom(0)
	return out
}

// ContextFrom is Context with the kept knowledge items numbered from first (ContextFrom in cmiclient.go, D67); it returns
// the number the next knowledge item gets.
func (s Screening) ContextFrom(first int) ([]agui.Context, int) {
	out, next := ContextFrom(s.Kept, first)
	if s.Withheld > 0 {
		out = append(out, agui.Context{Description: ContextCMIWithheld, Value: s.Note()})
	}
	return out, next
}

// Note is what the model is told: counts and levels, and nothing of the entries.
func (s Screening) Note() string {
	levels := make([]string, 0, len(s.ByLevel))
	for l := range s.ByLevel {
		levels = append(levels, l)
	}
	sort.Strings(levels)
	var parts []string
	for _, l := range levels {
		parts = append(parts, fmt.Sprintf("%d %s", s.ByLevel[l], l))
	}
	return fmt.Sprintf("%d context entries that were found for this request were NOT given to you, because they are classified above what may be sent outside the enterprise by this agent's model or tools (the limit is %s; %s). Do not guess their content. If the answer needs them, tell the user that some information could not be used.",
		s.Withheld, s.Ceiling, strings.Join(parts, ", "))
}

// Event records the screening on the run's stream.
// via names the sub-agents or remote agents that made the run outward, when that is why (D48); it holds names and
// reasons, never what was withheld.
func (s Screening) Event(trigger string, via []string) agui.Event {
	payload := map[string]any{"count": s.Withheld, "byLevel": s.ByLevel, "ceiling": s.Ceiling, "trigger": trigger}
	if len(via) > 0 {
		payload["via"] = via
	}
	return agui.Custom(agui.CustomContextWithheld, payload)
}

// DropInherited removes the CMI entries a child run inherited from a parent whose context was not screened (D44). An
// entry's text carries no level, so none can be judged and all of them go (better too many withheld than one leaked). It
// returns the context to give the child, with a note of the count in place of the entries, and how many were dropped.
func DropInherited(inherited []agui.Context) (kept []agui.Context, dropped int) {
	kept = make([]agui.Context, 0, len(inherited))
	for _, c := range inherited {
		if strings.HasPrefix(c.Description, "CMI ") {
			dropped++
			continue
		}
		kept = append(kept, c)
	}
	if dropped > 0 {
		kept = append(kept, agui.Context{Description: ContextCMIWithheld, Value: fmt.Sprintf(
			"%d context entries that the parent run had were NOT given to you, because this agent may use tools that send content outside the enterprise and the sensitivity of those entries is not known to this run. Do not guess their content. If the answer needs them, tell the user that some information could not be used.", dropped)})
	}
	return kept, dropped
}

// InheritedEvent records on a child run's stream that inherited entries were withheld.
func InheritedEvent(dropped int) agui.Event {
	return agui.Custom(agui.CustomContextWithheld, map[string]any{"count": dropped, "byLevel": map[string]int{LevelUnknown: dropped}, "ceiling": "", "trigger": agui.ScreenExternalTools, "inherited": true})
}
