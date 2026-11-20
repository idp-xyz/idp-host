package harness

// D59: the budget notes of earlier steps stay in what the model reads, where they were written, until the view changes; so
// the request only grows at its end, whatever the server's cache does with a turn boundary.

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"idp-host/internal/ctxwin"
	"idp-host/internal/run"
	"idp-host/tests/fixtures/fakemodel"
)

var noteStepRE = regexp.MustCompile(`^Budget: this is step (\d+) of`)

// notesOf are the budget notes of a request with their steps, in order, and where the last one is.
func notesOf(msgs []fakemodel.Message) (steps []int, idx []int) {
	for i, m := range msgs {
		if g := noteStepRE.FindStringSubmatch(m.Content); g != nil {
			n, _ := strconv.Atoi(g[1])
			steps, idx = append(steps, n), append(idx, i)
		}
	}
	return
}

func fullPrefix(a, b []fakemodel.Message) bool { return samePrefix(a, b) == len(a) }

// D59: between two batches each request is the whole of the one before it, its budget note included, and what the step adds
// comes after: the new messages, then the new note.
func TestKeptNotesMakeEachRequestAPrefixOfTheNext(t *testing.T) {
	m, hist := tableRun(t, ctxwin.Settings{Window: 131072, KeepNotes: true}, 3000, 5, recordedLikeThread(20), 20)
	for i := 1; i < len(hist); i++ {
		if !fullPrefix(hist[i-1], hist[i]) {
			t.Errorf("request %d is not the beginning of request %d (they share %d of %d messages)", i, i+1, samePrefix(hist[i-1], hist[i]), len(hist[i-1]))
		}
		if steps, _ := notesOf(hist[i]); len(steps) != i+1 {
			t.Errorf("request %d holds %d notes, want %d", i+1, len(steps), i+1)
		}
	}
	for i, ev := range viewEvents(m) {
		if i > 0 && num(ev["batch"]) != 0 {
			t.Errorf("step %d is a batch: the test needs none after the first", i+1)
		}
	}
}

// D59: the newest note is the last message, every note names its step and says that only the last is current, the steps
// run in order, and an old note is as it was written.
func TestTheNewestNoteIsLastAndAnOldOneCannotBeTakenForIt(t *testing.T) {
	_, hist := tableRun(t, ctxwin.Settings{Window: 131072, KeepNotes: true}, 1000, 6, user("go"), 0)
	var written = map[int]string{}
	for r, req := range hist {
		steps, idx := notesOf(req)
		if len(steps) == 0 || idx[len(idx)-1] != len(req)-1 {
			t.Fatalf("request %d: the newest note is not the last message", r+1)
		}
		if steps[len(steps)-1] != r+1 {
			t.Errorf("request %d: the last note says step %d", r+1, steps[len(steps)-1])
		}
		for k, i := range idx {
			if k > 0 && steps[k] <= steps[k-1] {
				t.Errorf("request %d: notes out of order %v", r+1, steps)
			}
			c := req[i].Content
			if !strings.Contains(c, "only the last budget note of the conversation is current") || !strings.Contains(c, "dated step "+strconv.Itoa(steps[k])) {
				t.Errorf("request %d: the note of step %d does not say which step it is and that only the last is current: %q", r+1, steps[k], c)
			}
			if was, ok := written[steps[k]]; ok && k < len(idx)-1 && (was != c) {
				t.Errorf("the note of step %d changed after it was written", steps[k])
			}
			if k == len(idx)-1 {
				written[steps[k]] = c
			}
		}
		if len(steps) != r+1 {
			t.Errorf("request %d holds notes of steps %v", r+1, steps)
		}
	}
}

// D59: a batch drops the notes of earlier steps (the view changes anyway) and only the newest is left; between batches the
// notes accumulate again and each request is the one before it with its additions.
func TestABatchDropsTheOldNotes(t *testing.T) {
	m, hist := tableRun(t, ctxwin.Settings{Window: 150000, ToolResultBytes: 16 << 10, OldBytes: 1200, KeepNotes: true}, 16000, 22, user("go"), 0)
	evs := viewEvents(m)
	batches := 0
	for i := range hist {
		steps, idx := notesOf(hist[i])
		if idx[len(idx)-1] != len(hist[i])-1 {
			t.Fatalf("step %d: the newest note is not last", i+1)
		}
		if num(evs[i]["batch"]) > 0 && i > 0 {
			batches++
			if len(steps) != 1 {
				t.Errorf("step %d, a batch: %d notes are left, want only the newest (%v)", i+1, len(steps), steps)
			}
		} else if i > 0 {
			if !fullPrefix(hist[i-1], hist[i]) {
				t.Errorf("step %d: not the previous request with additions, and no batch", i+1)
			}
		}
	}
	if batches < 2 {
		t.Errorf("%d batches: the test shows nothing", batches)
	}
}

// D59: at the bottom the notes accumulate in the frozen view, and the batch at the limit drops them.
func TestAtTheBottomTheNotesPileUpUntilTheLimitBatch(t *testing.T) {
	s := ctxwin.Settings{Window: 40000, MarkShare: 0.2, HintShare: 0.9, ToolResultBytes: 16 << 10, OldBytes: 1200, KeepNotes: true}
	m, hist := tableRun(t, s, 3000, 14, recordedLikeThread(20), 20)
	evs := viewEvents(m)
	frozen, limits := 0, 0
	for i := 2; i < len(evs); i++ {
		steps, _ := notesOf(hist[i])
		switch {
		case num(evs[i]["batch"]) == 0:
			frozen++
			if !fullPrefix(hist[i-1], hist[i]) {
				t.Errorf("step %d: the view changed with no batch", i+1)
			}
		case evs[i]["batchWhy"] == "limit":
			limits++
			if len(steps) != 1 {
				t.Errorf("step %d, a batch at the limit: %d notes left", i+1, len(steps))
			}
		}
	}
	if frozen < 2 || limits < 1 {
		t.Errorf("%d frozen steps, %d batches at the limit", frozen, limits)
	}
}

// D59: without a window the setting does nothing: the requests are those of D40, byte for byte.
func TestKeptNotesNeedAWindow(t *testing.T) {
	for pairs, want := range goldenMultiStepD40 {
		e := setup(t)
		e.h.ModelContext = map[string]ctxwin.Settings{"fake/fake-1": {KeepNotes: true}}
		e.model.Script = seqAfter(pairs, call("sum", `{"a":1,"b":2}`), call("sum", `{"a":2,"b":2}`), call("sum", `{"a":3,"b":2}`), call("sum", `{"a":4,"b":2}`), call("sum", `{"a":5,"b":2}`), fakemodel.Turn{Text: "done"})
		runWith(t, e, def(), recordedLikeThread(pairs), run.Input{})
		for i, h := range e.model.History() {
			if got := requestDigest(h); got != want[i] {
				t.Errorf("%d pairs, request %d: not what D40 sent", pairs, i+1)
			}
			if steps, _ := notesOf(h); len(steps) != 1 || strings.Contains(lastBudget(h), "dated step") { // the digest leaves the notes out
				t.Errorf("%d pairs, request %d holds the notes of steps %v", pairs, i+1, steps)
			}
		}
	}
}

// D59: with the setting off (the default) the note is replaced at every step as before: one note, last, in the form of D41.
func TestWithoutTheSettingTheNoteIsReplacedAtEveryStep(t *testing.T) {
	_, hist := tableRun(t, ctxwin.Settings{Window: 131072}, 1000, 5, user("go"), 0)
	for r, req := range hist {
		if steps, _ := notesOf(req); len(steps) != 1 {
			t.Errorf("request %d holds %d notes", r+1, len(steps))
		}
		if strings.Contains(lastBudget(req), "dated step") {
			t.Errorf("request %d: the note carries D59's rule", r+1)
		}
	}
}
