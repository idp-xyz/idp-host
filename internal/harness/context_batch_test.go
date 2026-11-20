package harness

// D58: what the model reads of the thread changes in batches, not at every step, so that a model server that keeps the
// previous prompt can reuse its beginning.

import (
	"fmt"
	"strings"
	"testing"

	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/ctxwin"
	"idp-host/internal/run"
	"idp-host/tests/fixtures/fakemcp"
	"idp-host/tests/fixtures/fakemodel"
)

// tableDef is def() with a tool that returns a table of a given size, and room for a long run.
func tableDef() agent.Definition {
	d := def()
	d.Tools.Allow = append(append([]string(nil), d.Tools.Allow...), "table")
	d.Limits.MaxSteps = 60
	return d
}

func addTable(e *env, bytes int) {
	n := 0
	e.mcp.Tools = append(e.mcp.Tools, fakemcp.Tool{Name: "table", Description: "table", InputSchema: `{"type":"object"}`,
		Annotations: map[string]any{"readOnlyHint": true},
		Handle: func(map[string]any) (string, bool) {
			n++
			return fmt.Sprintf("table %d\n", n) + strings.Repeat(fmt.Sprintf("row %d | 12345 | 67.89\n", n), bytes/25), false
		}})
}

// tableRun runs the model through n table calls and then an answer, on a thread, and returns what each request held.
func tableRun(t *testing.T, s ctxwin.Settings, bytes, n int, thread []agui.Message, have int) (*memStore, [][]fakemodel.Message) {
	t.Helper()
	e := setup(t)
	withWindow(e, s)
	addTable(e, bytes)
	var turns []fakemodel.Turn
	for i := 0; i < n; i++ {
		turns = append(turns, call("table", `{}`))
	}
	turns = append(turns, fakemodel.Turn{Text: "done"})
	e.model.Script = seqAfter(have, turns...)
	m := runWith(t, e, tableDef(), thread, run.Input{})
	if m.status != run.StatusCompleted {
		t.Fatalf("%s %s", m.status, m.failCode)
	}
	return m, e.model.History()
}

// withoutNote is a request without the host's budget note, which is last and changes at every step.
func withoutNote(msgs []fakemodel.Message) []fakemodel.Message {
	if n := len(msgs); n > 0 && strings.HasPrefix(msgs[n-1].Content, "Budget:") {
		return msgs[:n-1]
	}
	return msgs
}

// samePrefix is how many leading messages (and calls) two requests have in common.
func samePrefix(a, b []fakemodel.Message) int {
	n := 0
	for n < len(a) && n < len(b) && a[n].Role == b[n].Role && a[n].Content == b[n].Content && fmt.Sprint(a[n].ToolCalls) == fmt.Sprint(b[n].ToolCalls) {
		n++
	}
	return n
}

func shortened(msgs []fakemodel.Message) (n int) {
	for _, m := range msgs {
		if m.Role == "tool" && strings.Contains(m.Content, "are not shown") {
			n++
		}
	}
	return n
}

// D58: between two batches no item changes its shown form: each request is the one before it with the step's own
// additions after it. The thread is one in which a result slides out of the newest six at every step, and every one of them
// is longer than D40's limit, so that D52's rule would cut one more at each step.
func TestBetweenBatchesTheRequestOnlyGrows(t *testing.T) {
	m, hist := tableRun(t, ctxwin.Settings{Window: 131072}, 3000, 5, recordedLikeThread(20), 20)
	if len(hist) != 6 {
		t.Fatalf("%d requests", len(hist))
	}
	for i := 1; i < len(hist); i++ {
		prev, cur := withoutNote(hist[i-1]), withoutNote(hist[i])
		if n := samePrefix(prev, cur); n != len(prev) {
			t.Errorf("step %d: the request differs from the one before it at its message %d of %d", i+1, n, len(prev))
		}
	}
	for i, ev := range viewEvents(m) {
		wantBatch := 0
		if i == 0 {
			wantBatch = 18 // the first view of a run is a batch: every older result and argument at level 0
		}
		if num(ev["batch"]) != wantBatch {
			t.Errorf("step %d records a batch of %v, want %d", i+1, ev["batch"], wantBatch)
		}
	}
	if shortened(hist[len(hist)-1]) == 0 {
		t.Error("nothing is shortened: the test shows nothing")
	}
}

// D58: a run's first view takes up where the stored thread left it: everything outside the newest six is at level 0 at
// once, whatever the estimate, and the newest six are whole.
func TestTheFirstViewOfARunCutsEverythingOutsideTheNewestSix(t *testing.T) {
	e := setup(t)
	withWindow(e, ctxwin.Settings{Window: 131072})
	m := runWith(t, e, def(), recordedLikeThread(20), run.Input{})
	ev := viewEvents(m)[0]
	if ev["batchWhy"] != "start" || num(ev["batch"]) == 0 || num(ev["level"]) != 0 {
		t.Fatalf("%v", ev)
	}
	var results []fakemodel.Message
	for _, x := range e.model.Last() {
		if x.Role == "tool" {
			results = append(results, x)
		}
	}
	for i, r := range results {
		cut := strings.Contains(r.Content, "are not shown")
		if i >= len(results)-ctxwin.D40KeepResults && cut {
			t.Errorf("result %d of %d is among the newest six and is cut", i+1, len(results))
		}
		if i < len(results)-ctxwin.D40KeepResults && !cut && len(r.Content) > 1300 && !strings.Contains(r.Content, "REFERENCE") { // a skill's resource is cut at the deepest levels only
			t.Errorf("result %d of %d is older than the newest six and long, and is whole", i+1, len(results))
		}
	}
}

// D58: nothing is cut while the estimate is under the water mark, even for an item outside the newest six; the step that
// crosses the mark cuts every item outside the newest six at once, and the record says so; what is cut stays cut.
func TestAnEstimateOverTheMarkCallsOneBatchOfEverythingOutsideTheNewestSix(t *testing.T) {
	m, hist := tableRun(t, ctxwin.Settings{Window: 150000, ToolResultBytes: 16 << 10, OldBytes: 1200}, 16000, 22, user("go"), 0)
	evs := viewEvents(m)
	crossing := -1
	for i, ev := range evs {
		if ev["batchWhy"] == "mark" && crossing < 0 {
			crossing = i
		}
	}
	if crossing < 7 {
		t.Fatalf("no batch by the water mark (or before the seventh result), at step %d", crossing+1)
	}
	for i := 0; i < crossing; i++ {
		if shortened(hist[i]) != 0 || num(evs[i]["batch"]) != 0 {
			t.Errorf("step %d, before the mark: %d results cut, batch %v", i+1, shortened(hist[i]), evs[i]["batch"])
		}
		if num(evs[i]["estimateTokens"]) > num(evs[i]["markTokens"]) {
			t.Errorf("step %d: the estimate is over the mark and nothing was done", i+1)
		}
	}
	// it was called by the mark and not earlier: one more table (7,700 tokens here) on the step before it would have crossed
	if before := num(evs[crossing-1]["estimateTokens"]); before+9000 <= num(evs[crossing]["markTokens"]) {
		t.Errorf("the batch came at step %d with the estimate at %d, far under the mark %v", crossing+1, before, evs[crossing]["markTokens"])
	}
	ev := evs[crossing]
	results := crossing // the table results the thread holds at that step
	if want := results - ctxwin.D40KeepResults; num(ev["batch"]) != want || shortened(hist[crossing]) != want {
		t.Errorf("the batch cut %v items and %d results are cut, want %d of the %d results: all of those outside the newest six", ev["batch"], shortened(hist[crossing]), want, results)
	}
	if num(ev["level"]) != 0 {
		t.Errorf("level %v: level 0 was enough, nothing deeper was called for", ev["level"])
	}
	if num(ev["estimateTokens"]) > num(ev["markTokens"]) {
		t.Errorf("after the batch the estimate %v is over the mark %v", ev["estimateTokens"], ev["markTokens"])
	}
	// the record of every step but the batch's is the step before's; after the batch, what was cut stays cut and the
	// prefix is stable again until the next one
	for i := crossing + 1; i < len(hist); i++ {
		if shortened(hist[i]) < shortened(hist[i-1]) {
			t.Errorf("step %d shows fewer cut results than step %d", i+1, i)
		}
		if evs[i]["batch"] != nil && num(evs[i]["batch"]) == 0 {
			if prev, cur := withoutNote(hist[i-1]), withoutNote(hist[i]); samePrefix(prev, cur) != len(prev) {
				t.Errorf("step %d changed what step %d showed, without a batch", i+1, i)
			}
		}
	}
}

// D58: the cut an item got at a batch is not undone at a later one, and a later batch only cuts further.
func TestWhatABatchCutStaysCutAtTheNextBatches(t *testing.T) {
	m, _ := tableRun(t, ctxwin.Settings{Window: 30000, ToolResultBytes: 16 << 10, OldBytes: 1200}, 16000, 30, user("go"), 0)
	limits := map[string]int{}
	batches := 0
	for _, ev := range viewEvents(m) {
		if num(ev["batch"]) > 0 {
			batches++
		}
		if ev["itemsAsBefore"] != nil {
			continue
		}
		now := map[string]int{}
		for _, it := range itemsOf(ev) {
			now[it[0].(string)+"|"+it[1].(string)] = num(it[3])
		}
		for k, l := range limits {
			if n, ok := now[k]; !ok || n > l {
				t.Errorf("step %v: %s was cut to %d and is now %d", ev["step"], k, l, n)
			}
		}
		limits = now
	}
	if batches < 3 {
		t.Errorf("%d batches: the test needs several to show that a cut stays", batches)
	}
}

// D58: when shortening has nothing left to cut, the view is frozen: no item changes and nothing is cut a little more at
// every step. It is cut once more when the estimate reaches the limit (ten points above the mark), and the model is told
// from then on.
func TestAtTheBottomTheViewIsFrozenUntilTheLimit(t *testing.T) {
	s := ctxwin.Settings{Window: 40000, MarkShare: 0.2, HintShare: 0.9, ToolResultBytes: 16 << 10, OldBytes: 1200}
	m, hist := tableRun(t, s, 3000, 14, recordedLikeThread(20), 20)
	evs := viewEvents(m)
	if evs[0]["batchWhy"] != "start" || evs[0]["bottom"] != true || num(evs[0]["level"]) != len(shortLevels) {
		t.Fatalf("the first view should reach the bottom: %v", evs[0])
	}
	limit := num(evs[0]["markTokens"]) + 40000/10
	frozen, limitBatches, told := 0, 0, -1
	for i := 1; i < len(evs); i++ {
		ev := evs[i]
		if ev["bottom"] != true {
			t.Fatalf("step %d left the bottom: %v", i+1, ev)
		}
		switch {
		case num(ev["batch"]) == 0:
			frozen++
			if prev, cur := withoutNote(hist[i-1]), withoutNote(hist[i]); samePrefix(prev, cur) != len(prev) {
				t.Errorf("step %d: the view changed with no batch", i+1)
			}
			if i > 0 && num(ev["estimateTokens"]) >= limit && told < 0 {
				t.Errorf("step %d: the estimate %v is at the limit %d and nothing was cut", i+1, ev["estimateTokens"], limit)
			}
		case ev["batchWhy"] == "limit":
			limitBatches++
			if told < 0 {
				told = i
			}
			if num(ev["estimateTokens"]) >= limit+3000 {
				t.Errorf("step %d: after the batch at the limit the estimate is still %v", i+1, ev["estimateTokens"])
			}
		default:
			t.Errorf("step %d: batch %v by %v", i+1, ev["batch"], ev["batchWhy"])
		}
	}
	if frozen < 3 || limitBatches < 1 {
		t.Errorf("%d frozen steps and %d batches at the limit: the test needs both", frozen, limitBatches)
	}
	if told < 0 {
		t.Fatal("no step reached the limit")
	}
	for i := 0; i < told; i++ {
		if evs[i]["hint"] == true {
			t.Errorf("step %d tells the model before the limit was reached", i+1)
		}
	}
	for i := told; i < len(evs); i++ {
		if evs[i]["hint"] != true || !strings.Contains(lastBudget(hist[i]), "Context: this conversation fills") {
			t.Errorf("step %d, from the limit on, does not tell the model (hint %v)", i+1, evs[i]["hint"])
		}
	}
}

func lastBudget(msgs []fakemodel.Message) string {
	if n := len(msgs); n > 0 && strings.HasPrefix(msgs[n-1].Content, "Budget:") {
		return msgs[n-1].Content
	}
	return ""
}

// D58: with no window configured a run of several steps sends what D40 sent, byte for byte: the digests are those of the
// code of D57 (the branch this one starts from), which wrote no record of D52 or D58 and cut at every step by D40's rule.
func TestWithoutAContextWindowEveryRequestOfARunIsThatOfD40(t *testing.T) {
	golden := map[int][]string{
		20: {"e523779ca92c37a64803a88b3a1f1f0f2d08f1096a00e6dd5944f10d57c6ff22", "058d0ed18450c390658995f89d9bb0e4e2ead98e2d17843f7eaa2598f7294280", "d237e8251a8337eefd561c60bcb00ac33c267ec36d41e10a9f9ddc4f00e19dd1", "f40b7cb7b9c85ca6ec351553143940b0d8a8b130237f38ac4ce06657aa42c86c", "5809416fcfa1d81c427ebde73118defe0e26d3acd91bdc0761302647612b5bd4", "906c51bab083b067fcddd8e6ccc6f1d011c91b122cf81af30133e7a5403e9378"},
		45: {"73bbdf111958a6fa1785506684192ed4b03843c5f341097a32a86da4d137847e", "c54d3b1af77d03c0c1ce5492b06b9e2759be03bbbd8242a73f866f58528bef21", "49f591a7a9e0efadbd06c320eef0f0829ec93376d86ba4daff0b534b44fea487", "dff06ec21fac4fcf64b457d105ae074e062672a3cbc779600db4f0e6753a22b8", "88ba7507feb2af6c0cf039c18682fe51f7b7bd7d4340c2af56dbe66bbe2cc93d", "ab75cda82df23a21dcc6b72cc5d9d248b163997076988e16d1dd3eb3794c20b6"},
	}
	for pairs, want := range golden {
		e := setup(t)
		e.model.Script = seqAfter(pairs, call("sum", `{"a":1,"b":2}`), call("sum", `{"a":2,"b":2}`), call("sum", `{"a":3,"b":2}`), call("sum", `{"a":4,"b":2}`), call("sum", `{"a":5,"b":2}`), fakemodel.Turn{Text: "done"})
		m := runWith(t, e, def(), recordedLikeThread(pairs), run.Input{})
		hist := e.model.History()
		if len(hist) != len(want) {
			t.Fatalf("%d pairs: %d requests", pairs, len(hist))
		}
		for i, h := range hist {
			if got := requestDigest(h); got != want[i] {
				t.Errorf("%d pairs, request %d: not what D40 sent: %s", pairs, i+1, got)
			}
		}
		if len(eventNamed(m, agui.CustomContextView)) != 0 {
			t.Error("an event of D52 was written for a model with no window")
		}
	}
}
