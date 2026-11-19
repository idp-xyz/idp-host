package harness

// D52: the replay of the baseline's stored conversations under the rules of this change. It predicts; it does not measure:
// the model will act differently when it reads shortened content, and every step after the first change would differ.
// Skipped unless IDP_CONTEXT_REPLAY_DIR names a directory made by tests/replay/export_baseline.sh (third-party content,
// kept outside the repository). Run it with -v: it logs the tables of the ADR.

import (
	"context"
	"encoding/json"
	"idp-host/internal/model"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/ctxwin"
	"idp-host/internal/run"
)

type replayThread struct {
	Thread string `json:"thread"`
	Runs   []struct {
		ID        string    `json:"id"`
		CreatedAt time.Time `json:"createdAt"`
		Steps     []struct {
			Step      int       `json:"step"`
			StartedAt time.Time `json:"startedAt"`
			Prompt    int       `json:"prompt"`
			Error     *string   `json:"error"`
		} `json:"steps"`
	} `json:"runs"`
	Messages []struct {
		Seq       int          `json:"seq"`
		CreatedAt time.Time    `json:"createdAt"`
		Message   agui.Message `json:"message"`
	} `json:"messages"`
}

const replayWindow = 131072

// replayVariant is one way of building the request of a step.
type replayVariant struct {
	name   string
	set    *ctxwin.Settings // nil: no window, D40 only
	noMark bool             // window known, but the water mark is out of reach: level 0 only (arguments and results by D40's rule)
}

type stepRow struct {
	thread, run string
	runIndex    int
	step        int
	actual      int
	raw         map[string]float64 // raw estimate by variant
	level       map[string]int
	factor      map[string]float64
}

func TestReplayTheBaselineUnderTheRulesOfD52(t *testing.T) {
	dir := os.Getenv("IDP_CONTEXT_REPLAY_DIR")
	if dir == "" {
		t.Skip("IDP_CONTEXT_REPLAY_DIR is not set")
	}
	instrRaw, err := os.ReadFile(filepath.Join(dir, "instructions.txt"))
	if err != nil {
		t.Fatal(err)
	}
	instructions := strings.TrimSpace(string(instrRaw))
	files, _ := filepath.Glob(filepath.Join(dir, "le-*.json"))
	sort.Strings(files)
	if len(files) == 0 {
		t.Fatal("no conversations in " + dir)
	}
	variants := []replayVariant{
		{name: "d40"},
		{name: "args", set: &ctxwin.Settings{Window: replayWindow}, noMark: true},
		{name: "full", set: &ctxwin.Settings{Window: replayWindow}},
		{name: "mark50", set: &ctxwin.Settings{Window: replayWindow, MarkShare: 0.5, HintShare: 0.8}},
		{name: "mark70", set: &ctxwin.Settings{Window: replayWindow, MarkShare: 0.7, HintShare: 0.85}},
	}
	var rows []stepRow
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var th replayThread
		if err := json.Unmarshal(raw, &th); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, replayConversation(t, th, instructions, variants)...)
	}

	// 1. the estimator against the provider's count, on the requests as D40 sent them
	var errs, big, corrected []float64
	for _, r := range rows {
		e := (r.raw["d40"] - float64(r.actual)) / float64(r.actual)
		errs = append(errs, e)
		if r.actual > 60000 {
			big = append(big, e)
		}
		corrected = append(corrected, (r.raw["d40"]*r.factor["d40"]-float64(r.actual))/float64(r.actual))
	}
	report := func(name string, v []float64) {
		sort.Float64s(v)
		q := func(p float64) float64 { return v[int(p*float64(len(v)-1))] * 100 }
		t.Logf("estimator error, %-40s n=%3d  p1 %+6.1f%%  p5 %+6.1f%%  p25 %+6.1f%%  median %+6.1f%%  p75 %+6.1f%%  p95 %+6.1f%%  p99 %+6.1f%%  max %+6.1f%%", name, len(v), q(.01), q(.05), q(.25), q(.5), q(.75), q(.95), q(.99), q(1))
	}
	report("raw, every step (tools: one constant per conversation)", errs)
	report("raw, steps over 60,000 tokens", big)
	report("corrected by the previous step's usage, every step", corrected)
	var bigCorrected []float64
	for _, r := range rows {
		if r.actual > 60000 {
			bigCorrected = append(bigCorrected, (r.raw["d40"]*r.factor["d40"]-float64(r.actual))/float64(r.actual))
		}
	}
	report("corrected, steps over 60,000 tokens", bigCorrected)

	// 2. the predicted prompt per turn: the recorded maximum, and what each variant would send, at this step's own
	// estimator bias (actual / raw estimate of the request as it was sent), so that only the change of content shows
	type key struct {
		thread string
		turn   int
	}
	maxShare := map[key]map[string]float64{}
	for _, r := range rows {
		k := key{r.thread, r.runIndex + 1}
		if maxShare[k] == nil {
			maxShare[k] = map[string]float64{}
		}
		bias := float64(r.actual) / r.raw["d40"]
		upd := func(name string, v float64) { maxShare[k][name] = math.Max(maxShare[k][name], v/replayWindow) }
		upd("recorded", float64(r.actual))
		for _, v := range variants {
			upd(v.name, r.raw[v.name]*bias)
		}
	}
	var keys []key
	for k := range maxShare {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].thread != keys[j].thread {
			return keys[i].thread < keys[j].thread
		}
		return keys[i].turn < keys[j].turn
	})
	t.Logf("largest prompt of a turn, as a share of the %d-token window (recorded | D40 estimate | + arguments | + progressive: mark 60%% (default) | mark 50%% | mark 70%%)", replayWindow)
	third := map[string]float64{}
	for _, k := range keys {
		m := maxShare[k]
		t.Logf("%-28s turn %d   %5.1f%% | %5.1f%% | %5.1f%% | %5.1f%% | %5.1f%% | %5.1f%%", k.thread, k.turn, 100*m["recorded"], 100*m["d40"], 100*m["args"], 100*m["full"], 100*m["mark50"], 100*m["mark70"])
		if strings.Contains(k.thread, "-L2-") && k.turn == 3 {
			for n, v := range m {
				third[n] = math.Max(third[n], v)
			}
		}
	}
	t.Logf("L2, third turn, maximum over the three conversations: recorded %.1f%%, D40 estimate %.1f%%, with argument shortening %.1f%%, with progressive shortening (default) %.1f%%",
		100*third["recorded"], 100*third["d40"], 100*third["args"], 100*third["full"])
	// 3. the whole series of the third turns, by step, for the ADR
	for _, r := range rows {
		if strings.Contains(r.thread, "-L2-") && r.runIndex == 2 && (r.step%4 == 1) {
			bias := float64(r.actual) / r.raw["d40"]
			t.Logf("%-26s step %2d  recorded %5.1f%%  args %5.1f%%  full %5.1f%% (level %d, factor %.2f)", r.thread, r.step, 100*float64(r.actual)/replayWindow, 100*r.raw["args"]*bias/replayWindow, 100*r.raw["full"]*bias/replayWindow, r.level["full"], r.factor["full"])
		}
	}
	if third["full"] > 0.80 {
		t.Errorf("the predicted maximum of the third turn is %.1f%% of the window, above the 80%% the criteria ask", 100*third["full"])
	}
	if third["args"] >= third["d40"] || third["full"] > third["args"] {
		t.Errorf("each mechanism should lower the maximum: %v", third)
	}
}

// replayConversation builds, for each step of each run of a conversation, the request of every variant from the messages
// the thread held when the step started.
func replayConversation(t *testing.T, th replayThread, instructions string, variants []replayVariant) []stepRow {
	t.Helper()
	var out []stepRow
	toolsTokens := 0.0 // the tool specifications are not stored: one constant per conversation, from the first request
	for ri, r := range th.Runs {
		turns := map[string]*turn{}
		last := map[string]float64{}
		for _, v := range variants {
			h := &Harness{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: time.Now}
			if v.set != nil {
				h.ModelContext = map[string]ctxwin.Settings{"local/qwen": *v.set}
			}
			h.defaults()
			d := agent.Definition{Instructions: instructions, Model: agent.ModelProfile{Provider: "local", Model: "qwen"}}
			d.Normalize()
			m := newMem(d, nil, run.Input{})
			tr := &turn{h: h, ctx: context.Background(), claim: claim, rc: &m.rc, def: d, flagged: map[string]bool{}, visible: map[string]bool{}, frontend: map[string]bool{}}
			h.Store = m
			tr.limits = h.limitsFor(d)
			if v.noMark {
				tr.limits.Mark = 1 << 40
			}
			if toolsTokens > 0 {
				tr.tools = toolSpecOf(toolsTokens)
			}
			turns[v.name] = tr
		}
		for _, st := range r.Steps {
			if st.Error != nil || st.Prompt == 0 {
				continue
			}
			var vis []agui.Message
			for _, m := range th.Messages {
				if m.CreatedAt.Before(st.StartedAt) {
					vis = append(vis, m.Message)
				}
			}
			row := stepRow{thread: th.Thread, run: r.ID, runIndex: ri, step: st.Step, actual: st.Prompt, raw: map[string]float64{}, level: map[string]int{}, factor: map[string]float64{}}
			for _, v := range variants {
				tr := turns[v.name]
				tr.messages, tr.step = vis, st.Step
				if p, ok := last["d40"]; ok { // what the previous step reported, against what D40 had estimated for it
					tr.corr.Observe(p, int(last["actual"]))
				}
				msgs, err := tr.guardedMessages()
				if err != nil {
					t.Fatal(err)
				}
				row.raw[v.name] = requestTokens(tr.def.Instructions, msgs, tr.modelTools())
				row.factor[v.name] = tr.corr.Factor()
				if ev := viewEvents(tr.h.Store.(*memStore)); v.set != nil && len(ev) > 0 {
					row.level[v.name] = num(ev[len(ev)-1]["level"])
				}
			}
			if toolsTokens == 0 {
				toolsTokens = float64(st.Prompt) - row.raw["d40"]
				for _, v := range variants {
					row.raw[v.name] += toolsTokens
					turns[v.name].tools = toolSpecOf(toolsTokens)
				}
			}
			last["d40"], last["actual"] = row.raw["d40"], float64(st.Prompt)
			out = append(out, row)
		}
	}
	return out
}

// toolSpecOf stands for the tool specifications of the run: one tool whose description costs that many tokens.
func toolSpecOf(tokens float64) []model.ToolSpec {
	return []model.ToolSpec{{Name: "tools", Description: strings.Repeat("a", int(tokens/ctxwin.DensityASCII))}}
}
