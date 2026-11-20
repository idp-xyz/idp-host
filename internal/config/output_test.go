package config

import (
	"strings"
	"testing"

	"idp-host/internal/model"
)

// D60: a model entry gives the output limit and the sampling of the retry after a runaway; they reach the harness by
// "provider/model", and an entry without them is the default.
func TestAModelEntryGivesTheOutputLimitAndTheRetrySampling(t *testing.T) {
	f, err := load(t, modelHead+`      - name: qwen
        contextWindow: 131072
        maxOutputTokens: 28000
        runawayRetry: {temperature: 0.9, presencePenalty: 1.5}
      - name: plain
      - name: same
        runawayRetry: {keepSampling: true}
`)
	if err != nil {
		t.Fatal(err)
	}
	mo := f.ModelOutput()
	if q := mo["local/qwen"]; q.MaxOutputTokens != 28000 || q.Retry.Temperature != 0.9 || q.Retry.PresencePenalty != 1.5 || q.Retry.Keep {
		t.Errorf("%+v", q)
	}
	if p := mo["local/plain"]; p != (model.OutputPolicy{}) {
		t.Errorf("%+v", p)
	}
	if s := mo["local/same"]; !s.Retry.Keep {
		t.Errorf("%+v", s)
	}
	if got, src := mo["local/plain"].Limit(0); got != model.DefaultMaxOutputTokens || src != "default" {
		t.Errorf("%d %s", got, src)
	}
}

func TestTheOutputSettingsAreValidated(t *testing.T) {
	for _, c := range []struct{ yaml, want string }{
		{"      - name: m\n        maxOutputTokens: 5\n", "maxOutputTokens must be within"},
		{"      - name: m\n        maxOutputTokens: 2000000\n", "maxOutputTokens must be within"},
		{"      - name: m\n        maxOutputTokens: -1\n", "maxOutputTokens must be within"},
		{"      - name: m\n        contextWindow: 8192\n        maxOutputTokens: 8192\n", "below contextWindow"},
		{"      - name: m\n        runawayRetry: {temperature: 3}\n", "runawayRetry.temperature"},
		{"      - name: m\n        runawayRetry: {presencePenalty: -1}\n", "runawayRetry.presencePenalty"},
		{"      - name: m\n        runawayRetry: {presencepenalty: 1}\n", "field presencepenalty not found"},
	} {
		_, err := load(t, modelHead+c.yaml)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: %v, want %q", c.yaml, err, c.want)
		}
	}
}
