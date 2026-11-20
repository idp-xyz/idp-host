package harness

import (
	"fmt"
	"strings"

	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/ctxwin"
	"idp-host/internal/model"
)

// D52: the accounting of what a request holds. The model's window is known (the HOST configuration), the input is
// estimated in tokens before each call, older tool results and the arguments of older tool calls are shortened further
// while the estimate is above the water mark, the model is told when the context is filling, and a request that cannot
// fit is refused before it is paid for. All of it is applied to the view that is sent. The stored thread, the tool call
// records and the sources of a run are never touched.

// limitsFor resolves the thresholds for a release's model: the smallest window among the candidates the run may be
// served by (a request has to fit whichever answers it), with that model's settings. A candidate with no entry in the
// HOST configuration is not counted, so that one unconfigured fallback does not turn the mechanism off.
func (h *Harness) limitsFor(def agent.Definition) ctxwin.Limits {
	fixed := ctxwin.Settings{ToolResultBytes: h.ToolResultLimit, KeepResults: h.KeepToolResults, OldBytes: h.OldToolResultBytes}
	refs := []agent.ModelRef{{Provider: def.Model.Provider, Model: def.Model.Model}}
	refs = append(refs, def.Model.Fallbacks...)
	if def.Model.Routing != nil {
		for _, c := range def.Model.Routing.Candidates {
			refs = append(refs, c.Ref())
		}
	}
	var best ctxwin.Settings
	found := false
	for _, r := range refs {
		if s, ok := h.ModelContext[r.Provider+"/"+r.Model]; ok && s.Window > 0 && (!found || s.Window < best.Window) {
			best, found = s, true
		}
	}
	if !found {
		best = h.ModelContext[def.Model.Provider+"/"+def.Model.Model] // overrides without a window
		best.Window = 0
	}
	return best.Resolve(fixed)
}

// msgTokens is the estimate of one message as it is sent.
func msgTokens(m agui.Message) float64 {
	n := ctxwin.PerMessage + ctxwin.Tokens(m.Content)
	if m.Role == agui.RoleTool && m.Content == "" {
		n += ctxwin.Tokens(m.Error)
	}
	for _, c := range m.ToolCalls {
		n += ctxwin.Tokens(c.Function.Name) + ctxwin.Tokens(c.Function.Arguments)
	}
	return n
}

// requestTokens is the raw estimate (not yet corrected) of a request: the instructions, the messages and the tools.
func requestTokens(instructions string, msgs []agui.Message, tools []model.ToolSpec) float64 {
	n := ctxwin.PerRequest + ctxwin.Tokens(instructions)
	for _, m := range msgs {
		n += msgTokens(m)
	}
	for _, t := range tools {
		n += ctxwin.Tokens(t.Name) + ctxwin.Tokens(t.Description) + ctxwin.Tokens(string(t.Parameters))
	}
	return n
}

// The levels of progressive shortening, in the order they are applied: what an item is cut to (as a share of
// OldBytes, at least floorBytes), the newest tool results that stay whole, and whether a skill's resource is cut too.
// Level 0 is D40's rule (and, with a window, the same for arguments); it is not progressive.
type shortLevel struct {
	limit     func(old int) int
	keep      func(l ctxwin.Limits) int
	resources bool
}

// lowWater is where a shortening that the water mark called for stops, as a share of the mark.
const lowWater = 5.0 / 6.0

var floorBytes = func(old int) int { return max(old/6, 128) }

var shortLevels = []shortLevel{
	{limit: func(old int) int { return old / 2 }, keep: func(l ctxwin.Limits) int { return l.KeepResults }},
	{limit: func(old int) int { return old / 4 }, keep: func(l ctxwin.Limits) int { return l.KeepResults }},
	{limit: floorBytes, keep: func(l ctxwin.Limits) int { return l.KeepResults }},
	{limit: func(old int) int { return old }, keep: func(ctxwin.Limits) int { return ctxwin.FloorKeep }, resources: true},
	{limit: floorBytes, keep: func(ctxwin.Limits) int { return ctxwin.FloorKeep }, resources: true},
}

// shortItem is a tool result or the arguments of a tool call, which can be shown shortened.
type shortItem struct {
	kind     string // "result" | "arguments"
	id       string // the tool message's id | the call's id
	full     string
	rank     int // how many tool results are newer than this one (the result of the call, for arguments); 0 = the newest
	resource bool
	limit    int // 0 = whole
	cur      string
	tokens   float64
}

// shown is one entry of the per-step record of what was shortened (G3): the item, its size, the limit it was cut to and
// the size that was shown.
type shown struct {
	Kind  string
	ID    string
	Bytes int
	Limit int
	Shown int
}

// threadView is what the model reads of the thread this step, in place of the stored messages.
type threadView struct {
	results map[string]string // tool message id -> the text shown (before the input guard's wrapper)
	args    map[string]string // tool call id -> the arguments shown
	items   []shown
	level   int    // the deepest progressive level used (0 = none)
	batch   int    // D58: how many items a batch cut at this step (0 = no batch)
	why     string // what called the batch: "start" | "mark" | "limit"
	bottom  bool   // D58: shortening is at its bottom; the view is frozen until the limit
}

// batchState is where a run is in D58's batches: the view changes only at a batch, and a batch is called by the first view
// of the run, by the estimate passing the water mark, or, once shortening has nothing left to cut, by the estimate
// reaching the limit.
type batchState struct {
	started bool // the first view of the run was made (it is a batch: the run's start)
	bottom  bool // the last batch could not bring the estimate down to the low water: nothing is cut again until the limit
	told    bool // at the bottom the estimate reached the limit: the model is told from then on
}

// shape decides what the model reads of the thread. base is the estimate of everything else in the request. The
// system part, the context entries, the user's messages and the assistant's text are never shortened; neither is an
// error result, and neither are the newest tool results (the last KeepResults, and at the deepest levels the last
// FloorKeep) and the calls that asked for them.
func (t *turn) shape(base, wrapTokens float64) *threadView {
	lim := t.limits
	v := &threadView{results: map[string]string{}, args: map[string]string{}}
	resource := map[string]bool{}
	rankOfCall := map[string]int{}
	seen := 0
	for i := len(t.messages) - 1; i >= 0; i-- {
		if m := t.messages[i]; m.Role == agui.RoleTool {
			rankOfCall[m.ToolCallID] = seen
			seen++
		}
	}
	var items []*shortItem
	total := base
	for _, m := range t.messages {
		for _, c := range m.ToolCalls {
			if c.Function.Name == agent.ResourceReadTool {
				resource[c.ID] = true
			}
		}
		switch {
		case m.Role == agui.RoleTool && m.Content != "":
			it := &shortItem{kind: "result", id: m.ID, full: m.Content, rank: rankOfCall[m.ToolCallID], resource: resource[m.ToolCallID], cur: m.Content}
			if m.Error != "" {
				it.rank = -1 // an error is the host's own text and small: never shortened
			}
			it.tokens = ctxwin.Tokens(it.full) + wrapTokens
			total += ctxwin.PerMessage + it.tokens
			items = append(items, it)
		case m.Role == agui.RoleTool:
			total += msgTokens(m)
		default:
			total += ctxwin.PerMessage + ctxwin.Tokens(m.Content)
			for _, c := range m.ToolCalls {
				total += ctxwin.Tokens(c.Function.Name)
				it := &shortItem{kind: "arguments", id: c.ID, full: c.Function.Arguments, rank: 0, cur: c.Function.Arguments}
				if r, ok := rankOfCall[c.ID]; ok {
					it.rank = r
				}
				it.tokens = ctxwin.Tokens(it.full)
				total += it.tokens
				if lim.Window > 0 { // arguments are shortened only when the window is known; without it nothing changes (D52)
					items = append(items, it)
				}
			}
		}
	}
	apply := func(it *shortItem, limit int, legacy bool) {
		how := ctxwin.HowCommand
		if it.resource {
			how = ctxwin.HowResource
		}
		var next string
		if it.kind == "result" {
			next = ctxwin.Shorten(it.full, limit, how)
			if next == it.full || (!legacy && len(next) >= len(it.cur)) {
				return
			}
		} else {
			s, ok := ctxwin.ShortenArguments(it.full, limit)
			if !ok || len(s) >= len(it.cur) {
				return
			}
			next = s
		}
		nt := ctxwin.Tokens(next)
		if it.kind == "result" {
			nt += wrapTokens
		}
		total += nt - it.tokens
		it.cur, it.tokens, it.limit = next, nt, limit
	}
	// level 0: D40's rule. A result older than the newest KeepResults, over OldBytes, is shown as head and tail; with a
	// window the arguments of the calls that asked for those results are too (at ArgumentBytes).
	level0 := func() {
		for _, it := range items {
			if it.limit != 0 || it.rank < lim.KeepResults || (it.kind == "result" && (it.resource || len(it.full) <= lim.OldBytes)) {
				continue
			}
			if it.kind == "result" {
				apply(it, lim.OldBytes, true)
			} else {
				apply(it, lim.ArgumentBytes, false)
			}
		}
	}
	// the progressive levels, oldest first, until the estimate is at or under stop; false when everything that may be cut
	// is cut and the estimate is still above it
	deeper := func(stop float64) bool {
		over := func() bool { return total*t.corr.Factor() > stop }
		for li, lv := range shortLevels {
			if !over() {
				return true
			}
			limit := lv.limit(lim.OldBytes)
			for _, it := range items {
				if it.rank < lv.keep(lim) || (it.resource && !lv.resources) {
					continue
				}
				l := limit
				if it.kind == "arguments" {
					l = lv.limit(lim.ArgumentBytes)
				}
				if it.limit != 0 && it.limit <= l {
					continue
				}
				before := it.limit
				apply(it, l, false)
				if it.limit != before {
					v.level = max(v.level, li+1)
				}
				if !over() {
					return true
				}
			}
		}
		return !over()
	}
	// what an earlier step of this run cut stays cut: an item is never shown longer than it was, so that the part of the
	// request before the newest cut does not change from one step to the next (the prefix a model server may have cached)
	stick := func() {
		for _, it := range items {
			if l := t.sticky[it.kind+"|"+it.id]; l > 0 && (it.limit == 0 || l < it.limit) {
				apply(it, l, false)
			}
		}
	}
	low := float64(lim.Mark) * lowWater
	switch {
	case lim.Window == 0:
		level0()
	case lim.PerStep:
		// D52 as it was before D58: level 0 at every step, and the water mark calls for more
		level0()
		stick()
		over := total*t.corr.Factor() > float64(lim.Mark)
		stop := float64(lim.Mark)
		if over {
			stop = low
		}
		if over {
			deeper(stop)
		}
	default:
		// D58: nothing changes between batches. What is cut stays cut; an item that leaves the newest KeepResults is shown
		// whole until a batch; a batch is called by the run's first view (to take up where the stored thread left the last
		// run: shown as level 0 at least), by the estimate passing the water mark, and, once shortening is at its bottom, by
		// the estimate reaching the limit.
		stick()
		before := total * t.corr.Factor()
		call := ""
		switch {
		case !t.batch.started:
			call = "start"
		case t.batch.bottom && before >= float64(lim.Limit):
			call = "limit"
			t.batch.told = true
		case !t.batch.bottom && before > float64(lim.Mark):
			call = "mark"
		}
		t.batch.started = true
		if call != "" {
			was := make(map[*shortItem]int, len(items))
			for _, it := range items {
				was[it] = it.limit
			}
			level0()
			done := true
			if total*t.corr.Factor() > float64(lim.Mark) {
				done = deeper(low)
			}
			t.batch.bottom = !done
			for _, it := range items {
				if it.limit != was[it] { // cut, or cut further
					v.batch++
				}
			}
			v.why = call
			if v.batch == 0 { // a call that cut nothing is not a batch
				v.why = ""
			}
		}
		v.bottom = t.batch.bottom
	}
	if lim.Window > 0 && t.sticky == nil {
		t.sticky = map[string]int{}
	}
	for _, it := range items {
		if it.limit == 0 {
			continue
		}
		if lim.Window > 0 {
			t.sticky[it.kind+"|"+it.id] = it.limit
		}
		if it.kind == "result" {
			v.results[it.id] = it.cur
		} else {
			v.args[it.id] = it.cur
		}
		v.items = append(v.items, shown{Kind: it.kind, ID: it.id, Bytes: len(it.full), Limit: it.limit, Shown: len(it.cur)})
	}
	if lim.Window == 0 {
		v.items = nil
	}
	return v
}

// withShownArguments is m with the arguments of its calls as the view says; m itself is not changed (its call slice is
// copied before it is written to).
func (v *threadView) withShownArguments(m agui.Message) agui.Message {
	var calls []agui.ToolCall
	for i, c := range m.ToolCalls {
		if s, ok := v.args[c.ID]; ok {
			if calls == nil {
				calls = append([]agui.ToolCall(nil), m.ToolCalls...)
			}
			calls[i].Function.Arguments = s
		}
	}
	if calls != nil {
		m.ToolCalls = calls
	}
	return m
}

// viewEvent is the record of what the model was given of the thread at a step (G3): the thresholds, the estimate, the
// deepest level of shortening and the items that were shortened, each as [kind, id, bytes, limit, shown]. With the
// stored thread this says what the model read; the items are left out when they are those of the step before.
func (t *turn) viewEvent(step int, v *threadView, raw, corrected float64, hint bool) agui.Event {
	lim := t.limits
	key := make([]string, 0, len(v.items))
	items := make([][]any, 0, len(v.items))
	for _, s := range v.items {
		key = append(key, fmt.Sprintf("%s|%s|%d", s.Kind, s.ID, s.Limit))
		items = append(items, []any{s.Kind, s.ID, s.Bytes, s.Limit, s.Shown})
	}
	val := map[string]any{"step": step, "windowTokens": lim.Window, "markTokens": lim.Mark, "estimateTokens": int(corrected + 0.5), "rawEstimateTokens": int(raw + 0.5),
		"factor": float64(int(t.corr.Factor()*1000+0.5)) / 1000, "level": v.level, "shortened": len(items), "hint": hint}
	if !lim.PerStep { // D58: whether the view changed at this step, and how much of it; the evaluation lines this up with the server's reused and processed tokens
		val["batch"] = v.batch
		if v.why != "" {
			val["batchWhy"] = v.why
		}
		val["bottom"] = v.bottom
	}
	joined := strings.Join(key, ",")
	if joined == t.lastView {
		val["itemsAsBefore"] = true
	} else {
		val["items"] = items
	}
	t.lastView = joined
	return agui.Custom(agui.CustomContextView, val)
}

// contextHint is what the budget note adds once the estimate has passed the hint share of the window.
func contextHint(used, window int) string {
	return fmt.Sprintf(" Context: this conversation fills about %d%% of the model's context window (about %d of %d tokens); the run fails when the window is full and cannot go on. "+
		"Do not start new work: finish what is under way, deliver what exists, say plainly what is not done, and suggest that the user start a new conversation for the rest.",
		100*used/window, used, window)
}

// refuseUnfit says whether the request cannot fit even after everything that may be shortened was shortened (D52): its
// corrected estimate leaves no room for the answer. The 5 percent is the estimate's own margin, so that an estimate
// that is a little high does not end a run that would have fitted.
func (t *turn) refuseUnfit(corrected float64) bool {
	lim := t.limits
	if lim.Window == 0 {
		return false
	}
	room := float64(lim.Window - max(t.def.Model.MaxOutputTokens, 256))
	return corrected > room*1.05
}
