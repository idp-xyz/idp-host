//go:build repro

package harness

// This reproduces a defect whose fix is not decided yet (R4: an answer cut off at the output limit). It fails today on
// purpose and is not part of `make test`. Run it with `go test -tags repro ./internal/harness/`. The helpers it shares
// with the other tests (setup, newMem) are in the untagged files. (R1, the tool call in progress when the worker stopped,
// was decided as D47 and its tests are in resume_test.go.)

import (
	"context"
	"testing"

	"idp-host/internal/run"
	"idp-host/tests/fixtures/fakemodel"
)

// R4: the model's answer is cut off at the output limit (finish_reason "length", no tool calls). Expects: the run does not
// end as failed MODEL_ERROR (it completes with what was said, or the model is asked to continue). Setup: the fake model
// answers a partial text with finish_reason length. Reproduction, not a fix.
func TestAnAnswerCutOffAtTheOutputLimitDoesNotFailTheRun(t *testing.T) {
	e := setup(t)
	e.model.Script = func([]fakemodel.Message, []string) fakemodel.Turn {
		return fakemodel.Turn{Text: "The first three of the five parts of the report are", CutOff: true}
	}
	m := newMem(def(), user("write the report"), run.Input{})
	e.h.Store = m
	err := e.h.Run(context.Background(), claim)
	if m.status == run.StatusFailed {
		t.Errorf("run failed with %s after %d model call(s): %v; the partial answer %q was stored and streamed", m.failCode, e.model.Calls(), err, m.last().Content)
	}
}
