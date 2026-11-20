package model

import (
	"strings"
	"testing"
)

func TestTheOutputLimitIsTheOperatorsCeilingAndTheDefaultIsNot(t *testing.T) {
	for _, c := range []struct {
		name              string
		operator, release int
		want              int
		source            string
	}{
		{"nothing set", 0, 0, DefaultMaxOutputTokens, "default"},
		{"the operator's", 8000, 0, 8000, "operator"},
		{"the release lowers", 8000, 4000, 4000, "release"},
		{"the release cannot raise the operator's", 8000, 9000, 8000, "operator"},
		{"the release equal to the operator's", 8000, 8000, 8000, "operator"},
		{"the release lowers the default", 0, 1000, 1000, "release"},
		{"the release raises the default, which is not the operator's word", 0, 40000, 40000, "release"},
	} {
		if got, src := (OutputPolicy{MaxOutputTokens: c.operator}).Limit(c.release); got != c.want || src != c.source {
			t.Errorf("%s: %d %s, want %d %s", c.name, got, src, c.want, c.source)
		}
	}
}

func TestTheRetrySampling(t *testing.T) {
	temp := 0.2
	zero := 0.0
	if tt, pp, ok := (OutputPolicy{}).Sampling(nil); !ok || tt != DefaultRetryTemperature || pp != DefaultRetryPresencePenalty {
		t.Errorf("default: %v %v %v", tt, pp, ok)
	}
	if tt, _, _ := (OutputPolicy{}).Sampling(&zero); tt != DefaultRetryTemperature {
		t.Errorf("a release temperature of 0 is greedy: the retry must not be: %v", tt)
	}
	if tt, pp, _ := (OutputPolicy{Retry: RunawayRetry{Temperature: 1, PresencePenalty: 2}}).Sampling(&temp); tt != 0.2 || pp != 2 {
		t.Errorf("the release's temperature is kept: %v %v", tt, pp)
	}
	if _, _, ok := (OutputPolicy{Retry: RunawayRetry{Keep: true}}).Sampling(nil); ok {
		t.Error("keepSampling still changed the request")
	}
}

func TestARunawayErrorSaysSoAndKeepsItsDetailOutOfTheMessage(t *testing.T) {
	e := RunawayError("the stream is longer than the limit \x00 SECRET")
	if e.Code != CodeOutputRunaway || !e.Retryable || e.Detail == "" {
		t.Fatalf("%+v", e)
	}
	if msg := e.Error(); !strings.Contains(msg, "did not end") || !strings.Contains(msg, CodeOutputRunaway) || strings.Contains(msg, "SECRET") {
		t.Errorf("%q", msg)
	}
	if strings.ContainsRune(e.Detail, 0) {
		t.Errorf("detail %q is not cleaned", e.Detail)
	}
}
