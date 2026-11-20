// Package ctxwin does the accounting of what a model request holds (ADR-0001 D52): an estimate of its size in tokens
// without a tokenizer, the correction of that estimate by what the provider reports, the thresholds derived from a
// model's context window, and the head-and-tail shortening of an earlier tool result or tool call argument. It changes
// nothing in the stored conversation; the harness applies it to the view it sends.
package ctxwin

import "math"

// Densities are tokens per byte by class of byte, and a fixed cost per message. They were fitted on the 658 requests of
// the baseline (idp-host-context-management-proposal 4.1: 21 conversations, the local Qwen model; the request rebuilt from
// the stored messages) with every coefficient then raised by 10 percent, so that the estimate is more often above the
// provider's count than below it (D52 gives the data and the error). A digit costs about one token on that model (it
// splits numbers digit by digit); a letter, a space or a punctuation mark about a quarter; a byte of a multi-byte
// character (Chinese text, box-drawing characters, emoji) about 0.4, that is 1.2 tokens for a three-byte character.
const (
	DensityASCII    = 0.275
	DensityDigit    = 1.25
	DensityNonASCII = 0.42
	PerMessage      = 34.0 // role, call ids, separators
	PerRequest      = 20.0
)

// Tokens estimates the tokens of a text.
func Tokens(s string) float64 {
	var ascii, digit, non int
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 0x80:
			non++
		case c >= '0' && c <= '9':
			digit++
		default:
			ascii++
		}
	}
	return float64(ascii)*DensityASCII + float64(digit)*DensityDigit + float64(non)*DensityNonASCII
}

// Correction scales the estimate by what the provider reported for the previous request. One report moves the factor by
// at most half of the distance to the (bounded) ratio, and the factor never leaves [MinFactor, MaxFactor], so that an odd
// report (zero, a count that leaves out a cache, a hundred times too large) cannot switch the mechanism off: an estimate
// is never taken to be less than MinFactor of itself, nor more than MaxFactor of itself.
type Correction struct{ f float64 }

// The bounds of the correction, and of a single report's ratio.
const (
	MinFactor = 0.7
	MaxFactor = 1.5
	minRatio  = 0.5
	maxRatio  = 2.0
)

// Factor is what an estimate is multiplied by.
func (c *Correction) Factor() float64 {
	if c.f == 0 {
		return 1
	}
	return c.f
}

// Observe takes in the provider's prompt token count for a request whose raw estimate was estimate. A report of zero
// tokens (a provider that does not report) is ignored.
func (c *Correction) Observe(estimate float64, reported int) {
	if estimate <= 0 || reported <= 0 {
		return
	}
	s := math.Min(math.Max(float64(reported)/estimate, minRatio), maxRatio)
	c.f = math.Min(math.Max(0.5*c.Factor()+0.5*s, MinFactor), MaxFactor)
}

// Settings are what an operator says about a model (the host configuration, per provider and model). Every field but
// Window is optional; a zero is "derive it".
type Settings struct {
	Window          int     // tokens; zero means the window is not known and nothing below applies
	MarkShare       float64 // water mark: older content is shortened further while the estimate is above this share of the window
	HintShare       float64 // the budget hint speaks of the context from this share of the window
	ToolResultBytes int     // the most of one tool result the model is given
	KeepResults     int     // the newest tool results (and the calls that asked for them) that are kept whole
	OldBytes        int     // an older result or argument value longer than this is shown as head and tail
	ArgumentBytes   int     // like OldBytes, for the values of a tool call's arguments (default: OldBytes)
	KeepNotes       bool    // D59: the budget notes of earlier steps stay in the view, dropped at a batch, so that the request keeps growing at its end
}

// Limits are Settings with every default filled in from the window.
type Limits struct {
	Window          int
	Mark, Hint      int  // in tokens
	Limit           int  // D58: where a view that cannot be shortened further is cut once more: ten points of the window above the mark, not above the hint
	PerStep         bool // D52's rule as it was before D58: level 0 at every step. No setting turns it on; the replay compares with it
	ToolResultBytes int
	KeepResults     int
	OldBytes        int
	ArgumentBytes   int
	KeepNotes       bool
}

// The D40 constants, which the derivation gives for a window of 131,072 tokens.
const (
	D40ToolResultBytes = 16 << 10
	D40KeepResults     = 6
	D40OldBytes        = 1200
)

// Default shares of the window.
const (
	DefaultMarkShare = 0.60
	DefaultHintShare = 0.80
)

// LimitAboveMark is how far above the water mark, as a share of the window, the estimate may go once shortening has nothing
// left to cut (D58). It is the criterion of the evaluation (the largest share may rise ten points above the mark), not a
// measurement.
const LimitAboveMark = 0.10

// FloorKeep is how many of the newest tool results (and their calls) the progressive shortening never touches.
const FloorKeep = 2

// Resolve fills in the defaults. fixed carries what a Harness was given explicitly (it wins over a derived default, not
// over the operator's per-model setting).
func (s Settings) Resolve(fixed Settings) Limits {
	l := Limits{Window: s.Window, KeepNotes: s.Window > 0 && s.KeepNotes}
	pick := func(model, set, derived int) int {
		switch {
		case model > 0:
			return model
		case set > 0:
			return set
		}
		return derived
	}
	pickf := func(model, derived float64) float64 {
		if model > 0 {
			return model
		}
		return derived
	}
	derivedResult, derivedOld := D40ToolResultBytes, D40OldBytes
	if s.Window > 0 {
		// 16 KiB at 131,072 tokens: an eighth of the window in bytes, within [4 KiB, 64 KiB] (64 KiB was the limit before D40)
		derivedResult = clamp(s.Window/8, 4<<10, 64<<10)
		// 1,200 bytes at 131,072 tokens: the window over 109, within [512, 4096]. Not derived from a measurement: it is the
		// D40 number, kept for the one window it was tuned on and scaled with it.
		derivedOld = clamp(s.Window*75/8192, 512, 4096)
	}
	l.ToolResultBytes = pick(s.ToolResultBytes, fixed.ToolResultBytes, derivedResult)
	l.KeepResults = pick(s.KeepResults, fixed.KeepResults, D40KeepResults)
	l.OldBytes = pick(s.OldBytes, fixed.OldBytes, derivedOld)
	l.ArgumentBytes = pick(s.ArgumentBytes, fixed.ArgumentBytes, l.OldBytes)
	if s.Window > 0 {
		l.Mark = int(float64(s.Window) * pickf(s.MarkShare, DefaultMarkShare))
		l.Hint = int(float64(s.Window) * pickf(s.HintShare, DefaultHintShare))
		l.Limit = min(l.Hint, l.Mark+int(float64(s.Window)*LimitAboveMark))
	}
	return l
}

func clamp(v, lo, hi int) int { return min(max(v, lo), hi) }
