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
// Run it with -v.

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

// serverModel is the server the proposal assumed: it reuses the whole common prefix with the previous request. It is kept
// only to show how far that is from the measurement.
type serverModel struct {
	prev []agui.Message
	has  bool
}

func (s *serverModel) next(msgs []agui.Message, length, scale, base float64) (reused float64) {
	if s.has {
		reused = math.Min((base+commonPrefixTokens(s.prev, msgs))*scale, length)
	}
	s.prev, s.has = msgs, true
	return reused
}

// strataServer is the conversation cache of the Strata engine (0.1.27, src/program/generate.cpp, serve loop; the policy in
// include/strata/program/conv_cache.hpp). Up to cap checkpoints of what the session has read, a chain of prefixes of the
// current prompt; a request resumes from the longest one its prompt starts with, and reads the rest in chunks. A
// checkpoint is taken: at the end of the system prompt of a prompt read from 0 (when that is at least root tokens); at
// the last turn boundary of the prompt (the last <|im_start|>: where the history ends and the new assistant turn
// begins, header tokens before the end); and at the first chunk end at or after every tokens of fresh reading (counted
// from where the request resumed, and again from each checkpoint). The first (the root) is kept, the others leave by
// least recent use. Not modelled: the live session (it holds the generated tokens, which the next request renders
// differently: the chat template drops a reply's reasoning and ends it with its own tokens, so it does not match), and a part
// of 64 tokens or fewer read through the verify windows (no checkpoint in it).
type strataServer struct {
	every, cap, root, chunk int
	checks                  []strataCheck
	clock                   uint64
}

type strataCheck struct {
	pos, rawPos float64
	creator     []agui.Message
	used        uint64
}

const (
	strataHeader    = 4  // <|im_start|>assistant\n and the like, after the last turn boundary
	strataShortRead = 64 // --short-read
)

func (s *strataServer) save(pos, scale float64, msgs []agui.Message) {
	pos = math.Round(pos)
	for i := range s.checks {
		if math.Abs(s.checks[i].pos-pos) < 0.5 {
			s.clock++
			s.checks[i].used = s.clock
			return
		}
	}
	s.clock++
	s.checks = append(s.checks, strataCheck{pos: pos, rawPos: pos / scale, creator: msgs, used: s.clock})
	for len(s.checks) > s.cap {
		v := 0
		if s.cap >= 2 && len(s.checks) >= 2 { // the root is pinned; the least recently used of the rest leaves
			v = 1
			for i := 2; i < len(s.checks); i++ {
				if s.checks[i].used < s.checks[v].used {
					v = i
				}
			}
		}
		s.checks = append(s.checks[:v], s.checks[v+1:]...)
	}
}

// next says how many of the request's n tokens the server resumes past. scale is true tokens per estimated token at this
// step, base the estimated tokens of the instructions and tools.
func (s *strataServer) next(msgs []agui.Message, n, scale, base float64) float64 {
	resume := 0.0
	var best *strataCheck
	kept := s.checks[:0]
	for i := range s.checks {
		c := s.checks[i]
		if c.rawPos <= base+commonPrefixTokens(c.creator, msgs)+0.5 && c.pos <= n-1 {
			if c.pos > resume {
				resume = c.pos
			}
			kept = append(kept, c)
		}
	}
	s.checks = kept
	// a request rewrites the cells from where it resumes: what lies past it is gone; none at all means a clean session
	out := s.checks[:0]
	for _, c := range s.checks {
		if c.pos <= resume && resume > 0 {
			out = append(out, c)
		}
	}
	s.checks = out
	if resume > 0 {
		for i := range s.checks {
			if s.checks[i].pos == resume {
				s.clock++
				s.checks[i].used = s.clock
				best = &s.checks[i]
			}
		}
	}
	_ = best
	turnAt, rootAt := -1.0, -1.0
	if h := n - strataHeader; h > resume {
		turnAt = math.Round(h)
		if resume == 0 && s.root > 0 {
			if r := math.Round((base + systemPartTokens(msgs)) * scale); r >= float64(s.root) && r > 0 && r < turnAt {
				rootAt = r
			}
		}
	}
	at, next := resume, resume+float64(s.every)
	for _, to := range []float64{rootAt, turnAt, math.Round(n) - 1} {
		if to <= at {
			continue
		}
		if to-at > strataShortRead {
			chunk := math.Min(float64(s.chunk), math.Ceil((to-at)/256)*256)
			for p := at; p < to; {
				done := math.Min(p+chunk, to)
				if s.every > 0 && done >= next {
					s.save(done, scale, msgs)
					next = done + float64(s.every)
				}
				p = done
			}
		}
		at = to
		if to == turnAt || to == rootAt {
			s.save(to, scale, msgs)
		}
	}
	return resume
}

type timelineStep struct {
	th, run, step int
	at            time.Time
	actual        int
	srvProcessed  *int
	srvReused     *int
}

// hostRule is one way the host builds the request: D52's rule (a cut at every step) or D58's batches, with the budget note
// replaced at every step (D41) or kept in the view until the view changes (D59).
type hostRule struct {
	name    string
	perStep bool
	keep    bool
}

var hostRules = []hostRule{{"d52-rep", true, false}, {"d58-rep", false, false}, {"d52-kept", true, true}, {"d58-kept", false, true}}

// serverSetting is a Strata engine's --prompt-cache-every and --prompt-cache, with the prompt chunk it reads in (8192:
// what the evidence shows the running one has, since a checkpoint at 18,972 comes from reading 2,588 + 2 x 8,192).
type serverSetting struct {
	name              string
	every, cap, chunk int
}

var serverSettings = []serverSetting{
	{"16384x6", 16384, 6, 8192}, {"2048x10", 2048, 10, 8192}, {"4096x8", 4096, 8, 8192},
	{"2048x10 chunk 2048", 2048, 10, 2048}, {"4096x8 chunk 4096", 4096, 8, 4096},
}

type predRow struct {
	thread       string
	runIndex     int
	step         int
	actual       int
	srvProcessed int // -1: no measurement
	srvReused    int
	processed    map[string]float64 // predicted, by rule + "|" + server
	whole        map[string]float64 // predicted by the proposal's server, by rule
	prompt       map[string]float64 // the request's tokens, by rule (scaled to what the provider counts)
	notes        map[string]float64 // of which budget notes kept from earlier steps, by rule
	noteSize     float64            // the newest note, tokens
	keptNoteSize float64            // the same when it is a kept note (D59: it carries the rule that says which is current)
	batch        map[string]bool
	lastIsNote   bool    // the request ends with the budget note (D41), as in the recorded requests
	headDiff     float64 // d52-rep: how many messages from the end the first difference with the previous request of the run lies
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

func TestReplayPredictsHowMuchOfThePromptTheServerProcesses(t *testing.T) {
	dirs := os.Getenv("IDP_CONTEXT_REPLAY_DIRS")
	if dirs == "" {
		t.Skip("IDP_CONTEXT_REPLAY_DIRS is not set")
	}
	for _, dir := range strings.Split(dirs, ":") {
		rows := replayPrefix(t, dir)
		reportPrefix(t, filepath.Base(dir), rows)
	}
}

func replayPrefix(t *testing.T, dir string) []predRow {
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
	prevMsgs := map[runKey][]agui.Message{}
	toolsTokens := 0.0
	strata := map[string]*strataServer{}
	wholes := map[string]*serverModel{}
	for _, r := range hostRules {
		wholes[r.name] = &serverModel{}
		for _, sv := range serverSettings {
			strata[r.name+"|"+sv.name] = &strataServer{every: sv.every, cap: sv.cap, root: 2048, chunk: sv.chunk}
		}
	}
	var rows []predRow
	for _, ts := range timeline {
		th := threads[ts.th]
		key := runKey{ts.th, ts.run}
		if turns[key] == nil {
			turns[key] = map[string]*turn{}
			for _, r := range hostRules {
				h := &Harness{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: time.Now}
				h.ModelContext = map[string]ctxwin.Settings{"local/qwen": {Window: replayWindow, KeepNotes: r.keep}}
				h.defaults()
				d := agent.Definition{Instructions: instructions, Model: agent.ModelProfile{Provider: "local", Model: "qwen"}}
				d.Normalize()
				d.Limits.MaxSteps = 120 // the evaluation's release
				m := newMem(d, nil, run.Input{})
				tr := &turn{h: h, ctx: context.Background(), claim: claim, rc: &m.rc, def: d, flagged: map[string]bool{}, visible: map[string]bool{}, frontend: map[string]bool{}}
				h.Store = m
				tr.limits = h.limitsFor(d)
				tr.limits.PerStep = r.perStep
				if toolsTokens > 0 {
					tr.tools = toolSpecOf(toolsTokens)
				}
				turns[key][r.name] = tr
			}
		}
		var vis []agui.Message
		for _, m := range th.Messages {
			if m.CreatedAt.Before(ts.at) {
				vis = append(vis, m.Message)
			}
		}
		row := predRow{thread: th.Thread, runIndex: ts.run, step: ts.step, actual: ts.actual, srvProcessed: -1,
			processed: map[string]float64{}, whole: map[string]float64{}, prompt: map[string]float64{}, notes: map[string]float64{}, batch: map[string]bool{}}
		if ts.srvProcessed != nil && ts.srvReused != nil {
			row.srvProcessed, row.srvReused = *ts.srvProcessed, *ts.srvReused
		}
		raws := map[string]float64{}
		msgsOf := map[string][]agui.Message{}
		for _, r := range hostRules {
			tr := turns[key][r.name]
			tr.messages, tr.step = vis, ts.step
			if p, ok := lastRaw[key]; ok {
				tr.corr.Observe(p, lastActual[key])
			}
			msgs, err := tr.guardedMessages()
			if err != nil {
				t.Fatal(err)
			}
			raws[r.name] = requestTokens(tr.def.Instructions, msgs, tr.modelTools())
			msgsOf[r.name] = msgs
			if ev := viewEvents(tr.h.Store.(*memStore)); len(ev) > 0 && !r.perStep {
				row.batch[r.name] = num(ev[len(ev)-1]["batch"]) > 0
			}
		}
		if toolsTokens == 0 { // the tool specifications are not stored: one constant, from the first request
			toolsTokens = float64(ts.actual) - raws["d52-rep"]
			for _, r := range hostRules {
				raws[r.name] += toolsTokens
				turns[key][r.name].tools = toolSpecOf(toolsTokens)
			}
		}
		lastRaw[key], lastActual[key] = raws["d52-rep"], ts.actual
		bias := float64(ts.actual) / raws["d52-rep"] // the estimator's error at this step, as D52's replay does
		for _, r := range hostRules {
			msgs := msgsOf[r.name]
			length := raws[r.name] * bias
			base := raws[r.name] - sumMsgTokens(msgs) // the instructions and the tool specifications
			row.prompt[r.name] = length
			nn := 0.0
			for i, m := range msgs {
				if strings.HasPrefix(m.Content, "Budget:") && i < len(msgs)-1 {
					nn += msgTokens(m)
				}
			}
			row.notes[r.name] = nn * bias
			row.whole[r.name] = length - wholes[r.name].next(msgs, length, bias, base)
			for _, sv := range serverSettings {
				row.processed[r.name+"|"+sv.name] = length - strata[r.name+"|"+sv.name].next(msgs, length, bias, base)
			}
		}
		if m := msgsOf["d52-rep"]; len(m) > 0 {
			row.lastIsNote = strings.HasPrefix(m[len(m)-1].Content, "Budget:")
			row.noteSize = msgTokens(m[len(m)-1]) * bias
			if k := msgsOf["d58-kept"]; len(k) > 0 {
				row.keptNoteSize = msgTokens(k[len(k)-1]) * bias
			}
			if p := prevMsgs[key]; p != nil {
				row.headDiff = float64(len(p) - samePrefixN(p, m)) // messages of the previous request that are not in this one
			} else {
				row.headDiff = -1
			}
			prevMsgs[key] = m
		}
		rows = append(rows, row)
	}
	return rows
}

// samePrefixN is how many leading messages two requests have in common.
func samePrefixN(a, b []agui.Message) int {
	n := 0
	for n < len(a) && n < len(b) && serialized(a[n]) == serialized(b[n]) {
		n++
	}
	return n
}

func sumMsgTokens(msgs []agui.Message) float64 {
	n := 0.0
	for _, m := range msgs {
		n += msgTokens(m)
	}
	return n
}

var caseOf = regexp.MustCompile(`-(L[0-9]+)-r([0-9])`)

func caseName(thread string) string {
	if m := caseOf.FindStringSubmatch(thread); m != nil {
		return m[1] + "-r" + m[2]
	}
	if i := strings.LastIndex(thread, "-"); i > 3 {
		return thread[i-3:]
	}
	return thread
}

func reportPrefix(t *testing.T, name string, rows []predRow) {
	measured := false
	for _, r := range rows {
		measured = measured || r.srvProcessed >= 0
	}
	// the second repetition of the office-tools measurement had other traffic on the server (a dump without the server's
	// counts has no such reason to leave one out)
	clean := func(r predRow) bool { return !measured || !strings.HasSuffix(caseName(r.thread), "r2") }
	// 1. what the recorded requests say about the end of the request: the budget note is last, and the first
	// difference with the previous request of the run lies at it, or earlier
	atNote, earlier, steps := 0, 0, 0
	var noteSizes, keptSizes []float64
	for _, r := range rows {
		if r.headDiff < 0 {
			continue
		}
		steps++
		noteSizes = append(noteSizes, r.noteSize)
		keptSizes = append(keptSizes, r.keptNoteSize)
		if r.headDiff <= 1 {
			atNote++
		} else {
			earlier++
		}
	}
	t.Logf("%s: THE END OF THE REQUEST: of %d requests that follow another of their run, %d differ from it only at the budget note (the previous request's last message; its turn-boundary checkpoint covers that note, so it cannot match), %d differ earlier too; the note is the last message of %d%%; its size: median %.0f tokens, as a kept note (with the rule that says which note is current) %.0f", name, steps, atNote, earlier, 100*countLast(rows)/len(rows), median(noteSizes), median(keptSizes))

	// 2. validation against the server's own count, under the rule that ran (d52-rep), on the server setting that ran
	var meas, pred, naive, absErr, absNaive []float64
	exact, within, n := 0, 0, 0
	for _, r := range rows {
		if r.srvProcessed < 0 || !clean(r) {
			continue
		}
		p := r.processed["d52-rep|16384x6"]
		n++
		meas = append(meas, float64(r.srvProcessed))
		pred = append(pred, p)
		naive = append(naive, r.whole["d52-rep"])
		absErr = append(absErr, math.Abs(p-float64(r.srvProcessed)))
		absNaive = append(absNaive, math.Abs(r.whole["d52-rep"]-float64(r.srvProcessed)))
		if math.Abs(p-float64(r.srvProcessed)) < 600 {
			exact++
		}
		if math.Abs(p-float64(r.srvProcessed)) < 3000 {
			within++
		}
	}
	if n > 0 {
		t.Logf("%s: VALIDATION on %d steps of the first and third repetitions, the rule that ran, Strata with --prompt-cache-every 16384 --prompt-cache 6:", name, n)
		t.Logf("   processed tokens per step: measured median %.0f mean %.0f | Strata model median %.0f mean %.0f | whole-prefix model median %.0f mean %.0f", median(meas), mean(meas), median(pred), mean(pred), median(naive), mean(naive))
		t.Logf("   absolute error per step: Strata model median %.0f, mean %.0f; within 600 tokens on %d%% of steps, within 3000 on %d%%; whole-prefix model median %.0f, mean %.0f",
			median(absErr), mean(absErr), 100*exact/n, 100*within/n, median(absNaive), mean(absNaive))
		if 100*within/n < 90 || mean(absErr) > 1500 || mean(absErr) >= mean(absNaive)/2 {
			t.Errorf("the predictor does not reproduce the server: within 3000 tokens on %d%% of steps, mean error %.0f (whole-prefix model %.0f)", 100*within/n, mean(absErr), mean(absNaive))
		}
	}
	// 3. the prediction: every host rule against every server setting, pooled over the clean cases
	servers := []string{}
	for _, sv := range serverSettings {
		servers = append(servers, sv.name)
	}
	type cell struct{ proc, prompt, notes []float64 }
	pool := map[string]*cell{}
	perCase := map[string]map[string]*cell{}
	var order []string
	for _, r := range rows {
		c := caseName(r.thread)
		if perCase[c] == nil {
			perCase[c] = map[string]*cell{}
			order = append(order, c)
		}
		for _, h := range hostRules {
			for _, sv := range servers {
				k := h.name + "|" + sv
				for _, m := range []map[string]*cell{pool, perCase[c]} {
					if m == nil || (len(m) > 0 && false) {
						continue
					}
					if m[k] == nil {
						m[k] = &cell{}
					}
				}
				if clean(r) {
					pool[k].proc = append(pool[k].proc, r.processed[k])
					pool[k].prompt = append(pool[k].prompt, r.prompt[h.name])
					pool[k].notes = append(pool[k].notes, r.notes[h.name])
				}
				pc := perCase[c][k]
				pc.proc = append(pc.proc, r.processed[k])
				pc.prompt = append(pc.prompt, r.prompt[h.name])
				pc.notes = append(pc.notes, r.notes[h.name])
			}
		}
	}
	sort.Strings(order)
	maxOf := func(v []float64) float64 {
		m := 0.0
		for _, x := range v {
			m = math.Max(m, x)
		}
		return m
	}
	t.Logf("%s: PREDICTION, all cases but the second repetitions, tokens processed per step (median / mean), the largest prompt as a share of the window (of it the kept budget notes)", name)
	type scored struct {
		k   string
		med float64
	}
	var scores []scored
	for _, sv := range servers {
		for _, h := range hostRules {
			k := h.name + "|" + sv
			c := pool[k]
			if c == nil {
				continue
			}
			t.Logf("   server %-20s %-9s median %6.0f  mean %6.0f  largest %5.1f%% (notes %4.1f%%)", sv, h.name, median(c.proc), mean(c.proc), 100*maxOf(c.prompt)/replayWindow, 100*maxOf(c.notes)/replayWindow)
			scores = append(scores, scored{k, median(c.proc)})
		}
	}
	sort.Slice(scores, func(i, j int) bool { return scores[i].med < scores[j].med })
	t.Logf("%s: PER CASE, the ones that ran (D52, D58), D58 with kept notes on the running server, and the best (median / mean processed per step, largest share)", name)
	pick := []string{"d52-rep|16384x6", "d58-rep|16384x6", "d58-kept|16384x6", scores[0].k}
	for _, c := range order {
		line := fmt.Sprintf("   %-12s %3d steps", c, len(perCase[c][pick[0]].proc))
		for _, k := range pick {
			x := perCase[c][k]
			line += fmt.Sprintf(" | %s %6.0f /%6.0f %4.1f%%", k, median(x.proc), mean(x.proc), 100*maxOf(x.prompt)/replayWindow)
		}
		t.Log(line)
	}
}

func countLast(rows []predRow) int {
	n := 0
	for _, r := range rows {
		if r.lastIsNote {
			n++
		}
	}
	return n
}
