package ctxwin

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"unicode/utf8"
)

// The estimator charges a class of byte its density: letters and marks a quarter token, a digit about one, a byte of a
// multi-byte character about 0.4; and an empty text nothing.
func TestTokensChargeEachClassOfByteItsDensity(t *testing.T) {
	near := func(got, want float64) bool { return math.Abs(got-want) < 1e-9 }
	for _, c := range []struct {
		text string
		want float64
	}{
		{"", 0},
		{strings.Repeat("a", 1000), 1000 * DensityASCII},
		{strings.Repeat("7", 1000), 1000 * DensityDigit},
		{strings.Repeat("价", 1000), 3000 * DensityNonASCII},               // three bytes a character
		{"ab 12\n价", 4*DensityASCII + 2*DensityDigit + 3*DensityNonASCII}, // a, b, the space and the newline are ASCII
	} {
		if got := Tokens(c.text); !near(got, c.want) {
			t.Errorf("%q: %v, want %v", c.text, got, c.want)
		}
	}
	if Tokens("12345678") <= Tokens("abcdefgh") {
		t.Error("numbers must cost more than words: that is what the recorded tables showed")
	}
}

// The correction follows what the provider reports, one report at a time and by half, and no report can move it outside
// its bounds.
func TestTheCorrectionFollowsUsageAndIsBounded(t *testing.T) {
	var c Correction
	if c.Factor() != 1 {
		t.Fatal("a run starts uncorrected")
	}
	c.Observe(1000, 800) // the estimate was 25 percent high: half way to 0.8
	if f := c.Factor(); math.Abs(f-0.9) > 1e-9 {
		t.Errorf("%v", f)
	}
	for i := 0; i < 20; i++ {
		c.Observe(1000, 800)
	}
	if f := c.Factor(); math.Abs(f-0.8) > 1e-6 {
		t.Errorf("a steady report should be followed: %v", f)
	}
	// odd reports: nothing, zero, a hundredth of the estimate, a hundred times the estimate, a negative number
	for _, odd := range []int{0, -5, 10, 100000} {
		var d Correction
		d.Observe(1000, 800)
		before := d.Factor()
		d.Observe(1000, odd)
		if odd <= 0 && d.Factor() != before {
			t.Errorf("a report of %d moved the factor", odd)
		}
		if f := d.Factor(); f < MinFactor || f > MaxFactor {
			t.Errorf("a report of %d took the factor to %v", odd, f)
		}
		// one odd report moves it by at most half of the distance between the bounds of a ratio
		if f := d.Factor(); math.Abs(f-before) > 0.5*(maxRatio-minRatio)+1e-9 {
			t.Errorf("a report of %d moved the factor from %v to %v", odd, before, f)
		}
	}
	// a provider that always reports far too little (a cache's tokens left out) reaches the floor and stays there
	var e Correction
	for i := 0; i < 100; i++ {
		e.Observe(50000, 100)
	}
	if e.Factor() != MinFactor {
		t.Errorf("%v", e.Factor())
	}
	var g Correction
	for i := 0; i < 100; i++ {
		g.Observe(100, 500000)
	}
	if g.Factor() != MaxFactor {
		t.Errorf("%v", g.Factor())
	}
	var h Correction
	h.Observe(0, 100)
	if h.Factor() != 1 {
		t.Error("an estimate of zero says nothing")
	}
}

// The thresholds come out of the window; for a window of 131,072 tokens they are D40's numbers.
func TestThresholdsAreDerivedFromTheWindow(t *testing.T) {
	l := Settings{Window: 131072}.Resolve(Settings{})
	if l.ToolResultBytes != 16384 || l.KeepResults != 6 || l.OldBytes != 1200 || l.ArgumentBytes != 1200 {
		t.Errorf("D40's numbers are what 131,072 tokens must give: %+v", l)
	}
	if l.Mark != 78643 || l.Hint != 104857 {
		t.Errorf("mark and hint: %+v", l)
	}
	small := Settings{Window: 32768}.Resolve(Settings{})
	if small.ToolResultBytes != 4096 || small.OldBytes != 512 {
		t.Errorf("a smaller window gives smaller bounds, within the clamp: %+v", small)
	}
	big := Settings{Window: 4_000_000}.Resolve(Settings{})
	if big.ToolResultBytes != 64<<10 || big.OldBytes != 4096 {
		t.Errorf("a very large window is clamped: %+v", big)
	}
	// no window: D40 exactly, no water mark, no hint
	none := Settings{}.Resolve(Settings{})
	if none.ToolResultBytes != 16<<10 || none.KeepResults != 6 || none.OldBytes != 1200 || none.Mark != 0 || none.Hint != 0 || none.Window != 0 {
		t.Errorf("%+v", none)
	}
	// precedence: the operator's value for the model, then what the harness was given, then the derived one
	got := Settings{Window: 131072, OldBytes: 900}.Resolve(Settings{OldBytes: 700, KeepResults: 3})
	if got.OldBytes != 900 || got.KeepResults != 3 || got.ArgumentBytes != 900 {
		t.Errorf("%+v", got)
	}
	got = Settings{Window: 131072, MarkShare: 0.5, HintShare: 0.9, ArgumentBytes: 2000}.Resolve(Settings{})
	if got.Mark != 65536 || got.Hint != 117964 || got.ArgumentBytes != 2000 {
		t.Errorf("%+v", got)
	}
}

func TestShortenKeepsHeadAndTailAndD40sWords(t *testing.T) {
	s := strings.Repeat("0123456789", 300)
	got := Shorten(s, 300, HowCommand)
	if !strings.HasPrefix(got, s[:200]) || !strings.HasSuffix(got, s[len(s)-100:]) || !strings.Contains(got, "[… 2700 bytes of this earlier result are not shown; run the command again, narrowed, if you need them …]") {
		t.Errorf("%q", got)
	}
	if Shorten("short", 300, HowCommand) != "short" {
		t.Error("a text within the limit is left alone")
	}
	cjk := strings.Repeat("价格表", 400)
	if g := Shorten(cjk, 301, HowCommand); !utf8.ValidString(g) {
		t.Error("a cut inside a character")
	}
}

// A shortened argument stays what every wire form needs: valid JSON, an object, the same keys in the same order, other
// values as they were, and the marker inside the string.
func TestShortenedArgumentsStayValidJSON(t *testing.T) {
	long := strings.Repeat("import os\nprint(\"价格\")  # <b>&</b>\n", 200)
	for _, c := range []struct{ name, args string }{
		{"one long string", mustJSON(map[string]any{"language": "python", "code": long})},
		{"nested", `{"a":1,"b":{"c":[1,2,{"d":"` + strings.Repeat("x", 3000) + `"}],"e":null,"f":true},"g":1.5e10}`},
		{"two long strings", mustJSON(map[string]any{"x": long, "y": long, "n": 7})},
		{"unicode and escapes", `{"t":"` + strings.Repeat(`价\n\"q\" `, 600) + `"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ShortenArguments(c.args, 500)
			if !ok || len(got) >= len(c.args) {
				t.Fatalf("not shortened: %d -> %d", len(c.args), len(got))
			}
			var in, out map[string]json.RawMessage
			if err := json.Unmarshal([]byte(c.args), &in); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(got), &out); err != nil {
				t.Fatalf("not valid JSON: %v\n%.300s", err, got)
			}
			if len(in) != len(out) {
				t.Errorf("keys %d -> %d", len(in), len(out))
			}
			if !strings.Contains(got, "bytes of this earlier argument value are not shown") || !strings.Contains(got, "The call was made with the whole value") {
				t.Error("the marker is missing")
			}
			if keys(c.args) != keys(got) {
				t.Errorf("the order of keys changed: %s vs %s", keys(c.args), keys(got))
			}
			if strings.Contains(got, "\\"+"u003c") {
				t.Error("HTML characters were escaped: the model's text would differ")
			}
		})
	}
	// numbers and short values are not touched
	got, _ := ShortenArguments(`{"n":12345678901234567890,"s":"`+strings.Repeat("y", 2000)+`","t":"short"}`, 300)
	if !strings.Contains(got, `"n":12345678901234567890`) || !strings.Contains(got, `"t":"short"`) {
		t.Errorf("%s", got[:80])
	}
}

func TestArgumentsThatCannotOrNeedNotBeShortenedAreReturnedAsTheyAre(t *testing.T) {
	for _, in := range []string{
		`{"a":"short"}`,                              // within the limit
		`{"a":` + strings.Repeat("1,", 2000),         // not JSON
		`[` + strings.Repeat("1,", 2000) + `1]`,      // numbers only: nothing to cut
		`{"a":1} {"b":2}` + strings.Repeat(" ", 600), // trailing data
		"",
	} {
		if got, ok := ShortenArguments(in, 100); ok || got != in {
			t.Errorf("%.40q: changed (%v)", in, ok)
		}
	}
	// a value only just over the limit would not get smaller: it is left whole
	v := `{"a":"` + strings.Repeat("z", 120) + `"}`
	if got, ok := ShortenArguments(v, 100); ok || got != v {
		t.Errorf("a value that cannot get smaller was changed: %d", len(got))
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// keys lists the top-level keys of an object in the order they appear.
func keys(s string) string {
	dec := json.NewDecoder(strings.NewReader(s))
	if _, err := dec.Token(); err != nil {
		return ""
	}
	var out []string
	for dec.More() {
		k, _ := dec.Token()
		out = append(out, k.(string))
		var skip json.RawMessage
		_ = dec.Decode(&skip)
	}
	return strings.Join(out, ",")
}
