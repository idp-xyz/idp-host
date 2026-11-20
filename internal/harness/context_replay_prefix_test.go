package harness

// D58: a predictor of how much of each step's prompt the model server must process again, run over the stored
// conversations of a long evaluation. It is validated against the server's own count (run_steps.server_prompt_n and
// server_cache_n, D54) under the rule that was running (D52: level 0 at every step), and then asked what the new rule
// (batches) would have sent. Like D52's replay it predicts and does not measure: the model's own steps after the first
// change would differ.
//
// What the server does, from the measurement (the local llama.cpp server with a hybrid model, ADR D58): it does not keep
// "the previous prompt and reuse the common prefix". It keeps checkpoints of its state at fixed positions, and reuses the
// last checkpoint at or before the common prefix: the cache_n values of the dump are 0, 2,588, 18,972, 35,356, 51,740,
// 68,124 and nothing else, which is 2,588 + k * 16,384. A checkpoint after the first difference is no longer valid; the
// positions a request passes while it processes its prompt are checkpointed. The predictor is that, and nothing more.
//
// Skipped unless IDP_CONTEXT_REPLAY_DIRS lists (colon separated) directories made by tests/replay/export_baseline.sh.
// With IDP_CONTEXT_REPLAY_SWEEP=1 it also sweeps the water mark. Run it with -v.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/ctxwin"
	"idp-host/internal/run"
)

// The checkpoints of the server the evaluation ran on (see above). They are a property of that server's settings, not of
// the model or of the host: another server, or a cloud model, has others.
const (
	ckFirst  = 2588
	ckPeriod = 16384
)

// serialized is a message as the server sees it: what makes two messages the same text.
func serialized(m agui.Message) string {
	var b strings.Builder
	b.WriteString(string(m.Role))
	b.WriteByte(0)
	b.WriteString(m.Content)
	b.WriteByte(0)
	b.WriteString(m.ToolCallID)
	b.WriteString(m.Error)
	for _, c := range m.ToolCalls {
		b.WriteByte(0)
		b.WriteString(c.ID + "|" + c.Function.Name + "|" + c.Function.Arguments)
	}
	return b.String()
}

// commonPrefixTokens is the estimated tokens of what two requests (the messages after the instructions and tools, which
// they share) have in common from the start, and of the part of the request before its first user message.
func commonPrefixTokens(a, b []agui.Message) float64 {
	n := 0.0
	for i := 0; i < len(a) && i < len(b); i++ {
		sa, sb := serialized(a[i]), serialized(b[i])
		if sa == sb {
			n += msgTokens(a[i])
			continue
		}
		k := 0
		for k < len(sa) && k < len(sb) && sa[k] == sb[k] {
			k++
		}
		return n + ctxwin.Tokens(sa[:k])
	}
	return n
}

func systemPartTokens(msgs []agui.Message) float64 {
	n := 0.0
	for _, m := range msgs {
		if m.Role == agui.RoleUser || m.Role == agui.RoleAssistant || m.Role == agui.RoleTool {
			break
		}
		n += msgTokens(m)
	}
	return n
}

// serverModel is the checkpoints a server holds, and what it reuses for a request.
type serverModel struct {
	ck   map[int]bool
	grid bool // false: the server reuses the whole common prefix (what the proposal assumed)
	prev []agui.Message
	has  bool
}

// next says how many of the request's length tokens the server reuses, given the tokens its common prefix with the
// previous request holds and those of the part before the first user message (which the first checkpoint covers).
func (s *serverModel) next(msgs []agui.Message, length, scale, base float64) (reused float64) {
	if !s.has {
		s.ck = map[int]bool{}
	} else {
		lcp := (base + commonPrefixTokens(s.prev, msgs)) * scale
		if !s.grid {
			reused = math.Min(lcp, length)
		} else {
			sys := (base + systemPartTokens(msgs)) * scale
			for c := range s.ck {
				if (c == ckFirst && lcp >= sys) || (c != ckFirst && float64(c) <= lcp) {
					reused = math.Max(reused, float64(c))
				} else {
					delete(s.ck, c)
				}
			}
		}
	}
	if s.grid {
		s.ck[ckFirst] = true
		for c := ckFirst; float64(c) <= length; c += ckPeriod {
			s.ck[c] = true
		}
	}
	s.prev, s.has = msgs, true
	return reused
}

type timelineStep struct {
	th, run, step int
	at            time.Time
	actual        int
	srvProcessed  *int
	srvReused     *int
}

type predRow struct {
	thread        string
	runIndex      int
	step          int
	actual        int
	srvProcessed  int // -1: no measurement
	srvReused     int
	processed     map[string]float64 // predicted, by variant
	whole         map[string]float64 // predicted, the server reusing every common prefix token
	prompt        map[string]float64 // the request's tokens, by variant (scaled to what the provider counts)
	batch         map[string]bool
	bottom        map[string]bool
	predictedFull map[string]float64
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	w := append([]float64(nil), v...)
	sort.Float64s(w)
	if len(w)%2 == 1 {
		return w[len(w)/2]
	}
	return (w[len(w)/2-1] + w[len(w)/2]) / 2
}

func TestReplayPredictsHowMuchOfThePromptTheServerProcesses(t *testing.T) {
	dirs := os.Getenv("IDP_CONTEXT_REPLAY_DIRS")
	if dirs == "" {
		t.Skip("IDP_CONTEXT_REPLAY_DIRS is not set")
	}
	marks := []float64{0.6}
	if os.Getenv("IDP_CONTEXT_REPLAY_SWEEP") != "" {
		marks = []float64{0.4, 0.5, 0.6, 0.7}
	}
	variants := []replayVariant{{name: "current", set: &ctxwin.Settings{Window: replayWindow}}}
	for _, m := range marks {
		variants = append(variants, replayVariant{name: fmt.Sprintf("new%.0f", m*100), set: &ctxwin.Settings{Window: replayWindow, MarkShare: m, HintShare: 0.8 + 0.05*math.Max(0, m-0.7)*10}})
	}
	for _, dir := range strings.Split(dirs, ":") {
		rows := replayPrefix(t, dir, variants)
		reportPrefix(t, filepath.Base(dir), rows, variants)
	}
}

func replayPrefix(t *testing.T, dir string, variants []replayVariant) []predRow {
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
	var threads []replayThread
	var timeline []timelineStep
	for ti, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var th replayThread
		if err := json.Unmarshal(raw, &th); err != nil {
			t.Fatal(err)
		}
		threads = append(threads, th)
		for ri, r := range th.Runs {
			for _, st := range r.Steps {
				if st.Error != nil || st.Prompt == 0 {
					continue
				}
				timeline = append(timeline, timelineStep{th: ti, run: ri, step: st.Step, at: st.StartedAt, actual: st.Prompt, srvProcessed: st.ServerPromptN, srvReused: st.ServerCacheN})
			}
		}
	}
	// the server sees the requests in the order they were sent, whichever conversation they belong to
	sort.SliceStable(timeline, func(i, j int) bool { return timeline[i].at.Before(timeline[j].at) })

	type runKey struct{ th, run int }
	turns := map[runKey]map[string]*turn{}
	lastRaw := map[runKey]float64{}
	lastActual := map[runKey]int{}
	toolsTokens := 0.0
	servers := map[string]*serverModel{}
	wholes := map[string]*serverModel{}
	for _, v := range variants {
		servers[v.name] = &serverModel{grid: true}
		wholes[v.name] = &serverModel{}
	}
	var rows []predRow
	for _, ts := range timeline {
		th := threads[ts.th]
		key := runKey{ts.th, ts.run}
		if turns[key] == nil {
			turns[key] = map[string]*turn{}
			for _, v := range variants {
				h := &Harness{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: time.Now}
				h.ModelContext = map[string]ctxwin.Settings{"local/qwen": *v.set}
				h.defaults()
				d := agent.Definition{Instructions: instructions, Model: agent.ModelProfile{Provider: "local", Model: "qwen"}}
				d.Normalize()
				m := newMem(d, nil, run.Input{})
				tr := &turn{h: h, ctx: context.Background(), claim: claim, rc: &m.rc, def: d, flagged: map[string]bool{}, visible: map[string]bool{}, frontend: map[string]bool{}}
				h.Store = m
				tr.limits = h.limitsFor(d)
				tr.limits.PerStep = v.name == "current"
				if toolsTokens > 0 {
					tr.tools = toolSpecOf(toolsTokens)
				}
				turns[key][v.name] = tr
			}
		}
		var vis []agui.Message
		for _, m := range th.Messages {
			if m.CreatedAt.Before(ts.at) {
				vis = append(vis, m.Message)
			}
		}
		row := predRow{thread: th.Thread, runIndex: ts.run, step: ts.step, actual: ts.actual, srvProcessed: -1,
			processed: map[string]float64{}, whole: map[string]float64{}, prompt: map[string]float64{}, batch: map[string]bool{}, bottom: map[string]bool{}}
		if ts.srvProcessed != nil && ts.srvReused != nil {
			row.srvProcessed, row.srvReused = *ts.srvProcessed, *ts.srvReused
		}
		raws := map[string]float64{}
		msgsOf := map[string][]agui.Message{}
		for _, v := range variants {
			tr := turns[key][v.name]
			tr.messages, tr.step = vis, ts.step
			if p, ok := lastRaw[key]; ok {
				tr.corr.Observe(p, lastActual[key])
			}
			msgs, err := tr.guardedMessages()
			if err != nil {
				t.Fatal(err)
			}
			raws[v.name] = requestTokens(tr.def.Instructions, msgs, tr.modelTools())
			msgsOf[v.name] = msgs
			if ev := viewEvents(tr.h.Store.(*memStore)); len(ev) > 0 {
				last := ev[len(ev)-1]
				row.batch[v.name] = num(last["batch"]) > 0
				row.bottom[v.name], _ = last["bottom"].(bool)
			}
		}
		if toolsTokens == 0 { // the tool specifications are not stored: one constant, from the first request
			toolsTokens = float64(ts.actual) - raws["current"]
			for _, v := range variants {
				raws[v.name] += toolsTokens
				turns[key][v.name].tools = toolSpecOf(toolsTokens)
			}
		}
		lastRaw[key], lastActual[key] = raws["current"], ts.actual
		bias := float64(ts.actual) / raws["current"] // the estimator's error at this step, as D52's replay does
		for _, v := range variants {
			msgs := msgsOf[v.name]
			length := raws[v.name] * bias
			base := raws[v.name] - sumMsgTokens(msgs) // the instructions and the tool specifications
			row.prompt[v.name] = length
			row.processed[v.name] = length - servers[v.name].next(msgs, length, bias, base)
			row.whole[v.name] = length - wholes[v.name].next(msgs, length, bias, base)
		}
		rows = append(rows, row)
	}
	return rows
}

func sumMsgTokens(msgs []agui.Message) float64 {
	n := 0.0
	for _, m := range msgs {
		n += msgTokens(m)
	}
	return n
}

var caseOf = regexp.MustCompile(`-(L[0-9]+)-r([0-9])`)

func reportPrefix(t *testing.T, name string, rows []predRow, variants []replayVariant) {
	// 1. validation against the server's own count, under the rule that ran
	var meas, pred, naive, absErr, absNaive []float64
	exact, within, n := 0, 0, 0
	for _, r := range rows {
		if r.srvProcessed < 0 || caseOf.FindStringSubmatch(r.thread) == nil || caseOf.FindStringSubmatch(r.thread)[2] == "2" {
			continue
		}
		n++
		meas = append(meas, float64(r.srvProcessed))
		pred = append(pred, r.processed["current"])
		naive = append(naive, r.whole["current"])
		absErr = append(absErr, math.Abs(r.processed["current"]-float64(r.srvProcessed)))
		absNaive = append(absNaive, math.Abs(r.whole["current"]-float64(r.srvProcessed)))
		if math.Abs(r.processed["current"]-float64(r.srvProcessed)) < 600 {
			exact++
		}
		if math.Abs(r.processed["current"]-float64(r.srvProcessed)) < 3000 {
			within++
		}
	}
	if n > 0 {
		sumM, sumP, sumN := 0.0, 0.0, 0.0
		for i := range meas {
			sumM += meas[i]
			sumP += pred[i]
			sumN += naive[i]
		}
		t.Logf("%s: VALIDATION on %d steps of the first and third repetitions (the second had other traffic on the server), the rule that ran:", name, n)
		t.Logf("   processed tokens per step: measured median %.0f mean %.0f | checkpoint model median %.0f mean %.0f | whole-prefix model median %.0f mean %.0f", median(meas), sumM/float64(n), median(pred), sumP/float64(n), median(naive), sumN/float64(n))
		t.Logf("   absolute error per step: checkpoint model median %.0f, mean %.0f; within 600 tokens on %d%% of steps, within 3000 on %d%%; whole-prefix model median %.0f, mean %.0f",
			median(absErr), mean(absErr), 100*exact/n, 100*within/n, median(absNaive), mean(absNaive))
		if 100*within/n < 90 || mean(absErr) > 1500 || mean(absErr) >= mean(absNaive)/2 {
			t.Errorf("the predictor does not reproduce the server: within 3000 tokens on %d%% of steps, mean error %.0f (whole-prefix model %.0f)", 100*within/n, mean(absErr), mean(absNaive))
		}
	}
	// 2. per case: the median processed per step, the mean, the largest share, the batches
	type agg struct {
		processed, srv []float64
		prompts        map[string][]float64
		batches        map[string]int
		steps          int
		hit, whole     map[string][]float64
	}
	cases := map[string]*agg{}
	var order []string
	for _, r := range rows {
		c := r.thread
		if m := caseOf.FindStringSubmatch(r.thread); m != nil {
			c = m[1] + "-r" + m[2]
		} else if i := strings.LastIndex(r.thread, "-"); i > 0 {
			c = r.thread[i-3:]
		}
		a := cases[c]
		if a == nil {
			a = &agg{prompts: map[string][]float64{}, batches: map[string]int{}, hit: map[string][]float64{}, whole: map[string][]float64{}}
			cases[c] = a
			order = append(order, c)
		}
		a.steps++
		if r.srvProcessed >= 0 {
			a.srv = append(a.srv, float64(r.srvProcessed))
		}
		for _, v := range variants {
			a.hit[v.name] = append(a.hit[v.name], r.processed[v.name])
			a.whole[v.name] = append(a.whole[v.name], r.whole[v.name])
			a.prompts[v.name] = append(a.prompts[v.name], r.prompt[v.name])
			if r.batch[v.name] {
				a.batches[v.name]++
			}
		}
	}
	sort.Strings(order)
	hdr := fmt.Sprintf("%s: per case, tokens processed per step (median / mean), largest prompt as a share of the window, steps whose view changed", name)
	t.Log(hdr)
	pool, poolWhole := map[string][]float64{}, map[string][]float64{}
	for _, c := range order {
		a := cases[c]
		line := fmt.Sprintf("   %-14s %3d steps", c, a.steps)
		if len(a.srv) > 0 {
			line += fmt.Sprintf(" | server %6.0f /%6.0f", median(a.srv), mean(a.srv))
		}
		for _, v := range variants {
			mx := 0.0
			for _, p := range a.prompts[v.name] {
				mx = math.Max(mx, p)
			}
			line += fmt.Sprintf(" | %s %6.0f /%6.0f %4.1f%% b%d", v.name, median(a.hit[v.name]), mean(a.hit[v.name]), 100*mx/replayWindow, a.batches[v.name])
			if !strings.HasSuffix(c, "r2") {
				pool[v.name] = append(pool[v.name], a.hit[v.name]...)
				poolWhole[v.name] = append(poolWhole[v.name], a.whole[v.name]...)
			}
		}
		t.Log(line)
	}
	if len(pool) > 0 {
		line := fmt.Sprintf("%s: all steps but the second repetition's:", name)
		for _, v := range variants {
			line += fmt.Sprintf(" | %s median %.0f mean %.0f", v.name, median(pool[v.name]), mean(pool[v.name]))
		}
		t.Log(line)
		line = fmt.Sprintf("%s: the same, had the server reused the whole common prefix:", name)
		for _, v := range variants {
			line += fmt.Sprintf(" | %s median %.0f mean %.0f", v.name, median(poolWhole[v.name]), mean(poolWhole[v.name]))
		}
		t.Log(line)
		if len(variants) > 1 && median(pool["new60"]) >= median(pool["current"]) {
			t.Errorf("the median processed per step does not fall: %.0f to %.0f", median(pool["current"]), median(pool["new60"]))
		}
	}
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}
