package config

import (
	"strings"
	"testing"
)

const modelHead = `
providers:
  - name: local
    kind: openai
    baseUrl: http://model:8080/v1
    models:
`

// D52: a model entry gives the context window and optional thresholds, and reaches the harness as settings by
// "provider/model".
func TestAModelEntryGivesTheWindowAndTheThresholds(t *testing.T) {
	f, err := load(t, modelHead+`      - name: qwen
        contextWindow: 131072
      - name: small
        contextWindow: 32768
        context:
          waterMarkShare: 0.5
          hintShare: 0.75
          toolResultBytes: 8192
          keepToolResults: 4
          oldResultBytes: 800
          argumentBytes: 2000
          keepBudgetNotes: true
`)
	if err != nil {
		t.Fatal(err)
	}
	mc := f.ModelContext()
	if q := mc["local/qwen"]; q.Window != 131072 || q.MarkShare != 0 || q.ToolResultBytes != 0 {
		t.Errorf("%+v", q)
	}
	s := mc["local/small"]
	if s.Window != 32768 || s.MarkShare != 0.5 || s.HintShare != 0.75 || s.ToolResultBytes != 8192 || s.KeepResults != 4 || s.OldBytes != 800 || s.ArgumentBytes != 2000 || !s.KeepNotes || mc["local/qwen"].KeepNotes {
		t.Errorf("%+v", s)
	}
	if _, ok := mc["local/other"]; ok {
		t.Error("a model without an entry has no settings")
	}
}

func TestAModelEntryIsValidated(t *testing.T) {
	for _, c := range []struct{ yaml, want string }{
		{"      - name: m\n        contextWindow: 100\n", "contextWindow must be within"},
		{"      - name: m\n        contextWindow: -1\n", "contextWindow must be within"},
		{"      - name: m\n        contextWindow: 131072\n        context: {waterMarkShare: 0.95}\n", "waterMarkShare must be within"},
		{"      - name: m\n        contextWindow: 131072\n        context: {waterMarkShare: 0.7, hintShare: 0.6}\n", "must be below context.hintShare"},
		{"      - name: m\n        contextWindow: 131072\n        context: {waterMarkShare: 0.85}\n", "below the default hintShare"},
		{"      - name: m\n        contextWindow: 131072\n        context: {hintShare: 0.5}\n", "above the default waterMarkShare"},
		{"      - name: m\n        context: {toolResultBytes: 10}\n", "toolResultBytes must be within"},
		{"      - name: m\n        context: {keepToolResults: 99}\n", "keepToolResults must be within"},
		{"      - name: m\n        context: {oldResultBytes: 10}\n", "oldResultBytes must be within"},
		{"      - name: m\n        context: {argumentBytes: 10}\n", "argumentBytes must be within"},
		{"      - name: m\n      - name: m\n", "listed twice"},
		{"      - contextWindow: 131072\n", "needs a name"},
		{"      - name: m\n        contextWindow: 131072\n        unknown: 1\n", "unknown"},
	} {
		if _, err := load(t, modelHead+c.yaml); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: %v, want %q", c.yaml, err, c.want)
		}
	}
	// a model with thresholds and no window is allowed: they override D40's numbers and nothing else applies
	if _, err := load(t, modelHead+"      - name: m\n        context: {keepToolResults: 4}\n"); err != nil {
		t.Error(err)
	}
}
