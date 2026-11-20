package model

// The output limit and the answer that does not end (ADR-0001 D60).

// DefaultMaxOutputTokens is the output limit sent, where the model's window is known (D60), when neither the release, the run nor the operator's entry for the model
// sets one. It is taken from the recorded runs of the reference model: the longest step in 122 distinct long-run steps
// (3,769 with output) was 24,107 tokens, the 99.9th percentile 20,507, and Strata's log has one more of 25,286; most of that
// is the model's reasoning, which the limit counts. 32,768 is 1.3 times the longest answer seen and still below the 35,351 and
// 35,850 tokens of the two repetition loops. It is a bound against a runaway, not a budget: a release that needs more says so.
const DefaultMaxOutputTokens = 32768

// MaxRunawayAttempts is how many requests in all one candidate is asked for a step whose answers do not end: one retry,
// whatever the provider's retry policy allows for transient errors. A runaway attempt costs minutes, not milliseconds.
const MaxRunawayAttempts = 2

// The sampling a retry of a runaway uses unless the operator says otherwise (RunawayRetry). Qwen's own guidance for its
// models is not to decode greedily (it "can lead to ... endless repetitions") and to raise presence_penalty to reduce them;
// 0.7 is the temperature of its published settings for the Next models and 0.5 a cautious share of the 0..2 it gives.
const (
	DefaultRetryTemperature     = 0.7
	DefaultRetryPresencePenalty = 0.5
)

// OutputPolicy is what the operator says about the output of one model ("provider/model" in the host's configuration).
type OutputPolicy struct {
	// MaxOutputTokens is the most the model may write in one answer; zero means DefaultMaxOutputTokens. A release can lower
	// it, never raise it.
	MaxOutputTokens int
	// Retry is the sampling of the retry after a runaway.
	Retry RunawayRetry
}

// RunawayRetry is what a retry of a runaway changes in the request. Without a change, a server that decodes greedily
// (Strata does, unless its run configuration says otherwise) gives the same answer, the same loop.
type RunawayRetry struct {
	Keep            bool    // the retry repeats the request as it was
	Temperature     float64 // used when the release sets none (or 0); zero means DefaultRetryTemperature
	PresencePenalty float64 // zero means DefaultRetryPresencePenalty
}

// Limit is the output limit of a request to a model the release gave release tokens (0: none), and who set it: "release",
// "operator" (the entry's own value) or "default". The operator's value is a ceiling a release cannot raise; the default
// is not the operator's word, so a release that names a higher limit gets it.
func (p OutputPolicy) Limit(release int) (tokens int, source string) {
	tokens, source = p.MaxOutputTokens, "operator"
	if tokens <= 0 {
		tokens, source = DefaultMaxOutputTokens, "default"
		if release > tokens {
			return release, "release"
		}
	}
	if release > 0 && release < tokens {
		return release, "release"
	}
	return tokens, source
}

// Sampling is what the retry after a runaway sends: the temperature (the release's own when it is above zero) and the
// presence penalty; ok is false when the operator said to keep the request as it was.
func (p OutputPolicy) Sampling(release *float64) (temperature, presence float64, ok bool) {
	r := p.Retry
	if r.Keep {
		return 0, 0, false
	}
	temperature, presence = r.Temperature, r.PresencePenalty
	if temperature <= 0 {
		temperature = DefaultRetryTemperature
	}
	if presence <= 0 {
		presence = DefaultRetryPresencePenalty
	}
	if release != nil && *release > 0 {
		temperature = *release
	}
	return temperature, presence, true
}

// RunawayError is the error of a stream that ended only because a limit of this host stopped it (D60): retryable under the
// cap of MaxRunawayAttempts. detail is for the host log.
func RunawayError(detail string) *Error {
	return &Error{Code: CodeOutputRunaway, Retryable: true, Detail: CleanDetail(detail, MaxDetail)}
}
