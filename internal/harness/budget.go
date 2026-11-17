package harness

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"idp-host/internal/canonical"
	"idp-host/internal/mcpclient"
	"idp-host/internal/model"
	"idp-host/internal/run"
)

// A run long enough to do the work keeps its last two steps for delivery (D41). Shorter runs have no
// separate wrap-up, or the model would have no step left to do the work; salvage still runs at the limit.
const wrapUpSteps = 2

const (
	salvageMaxChecks  = 8
	salvageMaxDeliver = 5
)

var workspaceCreatedRE = regexp.MustCompile(`workspace ([0-9a-f]{32}) created`)

// budgetNote is what the model reads about the run's budget this step. It is not stored on the thread.
func (t *turn) budgetNote() string {
	max := t.def.Limits.MaxSteps
	left := max - t.step
	var b strings.Builder
	fmt.Fprintf(&b, "Budget: this is step %d of %d. ", t.step, max)
	if left <= 0 {
		b.WriteString("This is the last step.")
	} else {
		fmt.Fprintf(&b, "%s after this one.", plural(left, "step"))
	}
	if d := t.rc.Run.DeadlineAt; !d.IsZero() {
		remain := d.Sub(t.h.Now())
		if remain < 0 {
			remain = 0
		}
		fmt.Fprintf(&b, " The run ends at %s (%s left).", d.UTC().Format(time.RFC3339), humanRemain(remain))
	}
	if t.wrappingUp() {
		b.WriteString(" Wrap-up: only file delivery is available now. Deliver each file the user should receive, with target output. Do not inspect, edit or run code.")
	}
	return b.String()
}

// wrappingUp reports whether this step may only deliver a file.
func (t *turn) wrappingUp() bool {
	max := t.def.Limits.MaxSteps
	if t.step == 0 || max < wrapUpSteps+1 || t.step <= max-wrapUpSteps {
		return false
	}
	for _, props := range t.fileProps {
		for _, format := range props {
			if format == fileDelivery {
				return true
			}
		}
	}
	return false
}

// delivers reports whether the tool's schema takes a file the run delivers (idp-delivery).
func (t *turn) delivers(name string) bool {
	for _, format := range t.fileProps[name] {
		if format == fileDelivery {
			return true
		}
	}
	return false
}

// modelTools is the tool list for this step: during wrap-up, only the tools that deliver a file.
func (t *turn) modelTools() []model.ToolSpec {
	if !t.wrappingUp() {
		return t.tools
	}
	var out []model.ToolSpec
	for _, tool := range t.tools {
		if t.delivers(tool.Name) {
			out = append(out, tool)
		}
	}
	return out
}

func stepLimitMessage(max, delivered int) string {
	msg := fmt.Sprintf("the run reached its limit of %d steps", max)
	switch delivered {
	case 0:
		return msg
	case 1:
		return msg + "; 1 file was delivered from the workspace as it stood"
	default:
		return fmt.Sprintf("%s; %d files were delivered from the workspace as they stood", msg, delivered)
	}
}

// salvageable says whether a file left in the workspace is one the host delivers at the step limit, and the
// command that must succeed first ("" = none). An office file must validate and a PDF must open (D41, D42).
// Markdown has no such check, and it is also what a model takes its notes in, so only a Markdown file under
// out/ counts: that is where the skill tells the model to put what is meant for the user.
func salvageable(p string) (check string, ok bool) {
	switch strings.ToLower(path.Ext(p)) {
	case ".docx", ".xlsx", ".pptx":
		return "officecli validate " + shQuote(p), true
	case ".pdf":
		return "pdftext --check " + shQuote(p), true
	case ".md":
		return "", strings.HasPrefix(p, "out/")
	}
	return "", false
}

// workspacePath is a path as sandbox_list_files gives it: relative to the workspace, without ./ or /work/.
func workspacePath(p string) string {
	p = path.Clean(strings.TrimPrefix(strings.TrimSpace(p), "/work/"))
	if p == "." || strings.HasPrefix(p, "../") {
		return ""
	}
	return strings.TrimPrefix(p, "/")
}

// salvage delivers the files the run made and never exported, when the step limit has been reached (D41,
// D42): office files that validate, PDF files that open, and Markdown under out/. It returns how many files
// were delivered. A store or credential error is returned as it is (the run is already finished when the
// error is a failure). Anything else — no workspace, a tool error, a file that fails its check — delivers
// nothing for that file and does not by itself fail the run.
func (t *turn) salvage() (int, error) {
	if !t.canSalvage() || run.Overdue(t.rc.Run.DeadlineAt, t.h.Now()) {
		return 0, nil
	}
	ws, imported, exported := t.sandboxState()
	if ws == "" {
		return 0, nil
	}
	ok, res, err := t.salvageCall(t.toolNamed("sandbox_list_files"), map[string]any{"workspaceId": ws})
	if err != nil || !ok || res == nil {
		return 0, err
	}
	var candidates []string
	for _, p := range listedFiles(res) {
		p = workspacePath(p)
		if _, ok := salvageable(p); !ok || imported[p] || exported[p] {
			continue
		}
		candidates = append(candidates, p)
	}
	// what the model put under out/ first: the limits below may not reach every file
	sort.Slice(candidates, func(i, j int) bool {
		a, b := strings.HasPrefix(candidates[i], "out/"), strings.HasPrefix(candidates[j], "out/")
		if a != b {
			return a
		}
		return candidates[i] < candidates[j]
	})
	delivered := 0
	checked := 0
	for _, p := range candidates {
		if delivered >= salvageMaxDeliver || checked >= salvageMaxChecks || run.Overdue(t.rc.Run.DeadlineAt, t.h.Now()) {
			break
		}
		if check, _ := salvageable(p); check != "" {
			checked++
			ok, res, err = t.salvageCall(t.toolNamed("sandbox_run"), map[string]any{
				"workspaceId": ws,
				"language":    "office",
				"code":        check,
			})
			if err != nil || !ok {
				return delivered, err
			}
			code, known := exitCode(res)
			if !known || code != 0 {
				continue
			}
		}
		ok, res, err = t.salvageCall(t.toolNamed("sandbox_export_file"), map[string]any{
			"workspaceId": ws,
			"path":        p,
			"target":      "output",
			"name":        path.Base(p),
		})
		if err != nil || !ok {
			return delivered, err
		}
		if exportSucceeded(res) {
			delivered++
		}
	}
	return delivered, nil
}

// canSalvage reports whether this run can deliver a file and has the sandbox tools salvage needs.
func (t *turn) canSalvage() bool {
	if !t.rc.Run.Input.Delivery {
		return false
	}
	export := t.toolNamed("sandbox_export_file")
	return export != "" && t.delivers(export) && t.toolNamed("sandbox_list_files") != "" && t.toolNamed("sandbox_run") != ""
}

// toolNamed finds a visible gateway tool by its own name, ignoring a gateway prefix (sandbox__sandbox_run).
func (t *turn) toolNamed(base string) string {
	for name := range t.visible {
		if toolBase(name) == base {
			return name
		}
	}
	return ""
}

func toolBase(name string) string {
	if i := strings.LastIndex(name, "__"); i >= 0 {
		return name[i+2:]
	}
	return name
}

// sandboxState reads the workspace the model was using and which paths it imported or already delivered.
// Only a call that succeeded counts: a refused call does not change the workspace, a failed import is
// not the user's file, and a failed export still needs delivering.
func (t *turn) sandboxState() (ws string, imported, exported map[string]bool) {
	imported, exported = map[string]bool{}, map[string]bool{}
	kind := map[string]string{} // tool call id -> sandbox tool base name
	paths := map[string]string{}
	workspaces := map[string]string{}
	for _, m := range t.messages {
		for _, c := range m.ToolCalls {
			base := toolBase(c.Function.Name)
			if !strings.HasPrefix(base, "sandbox_") {
				continue
			}
			var args struct {
				WorkspaceID string `json:"workspaceId"`
				Path        string `json:"path"`
			}
			_ = json.Unmarshal([]byte(c.Function.Arguments), &args)
			kind[c.ID] = base
			paths[c.ID] = workspacePath(args.Path) // ./in/a.pdf and in/a.pdf are one file
			workspaces[c.ID] = args.WorkspaceID
		}
		if m.Role != "tool" || m.Error != "" {
			continue
		}
		if workspaces[m.ToolCallID] != "" {
			ws = workspaces[m.ToolCallID]
		}
		switch kind[m.ToolCallID] {
		case "sandbox_create":
			if w := workspaceCreatedRE.FindStringSubmatch(m.Content); w != nil {
				ws = w[1]
			}
		case "sandbox_import_file":
			if paths[m.ToolCallID] != "" {
				imported[paths[m.ToolCallID]] = true
			}
		case "sandbox_export_file":
			if paths[m.ToolCallID] != "" {
				exported[paths[m.ToolCallID]] = true
			}
		}
	}
	return ws, imported, exported
}

// salvageCall makes one gateway call in the run's name and records it like a model call. The recorded
// arguments keep target output; the ticket is substituted on the way to the gateway, as for the model (D37).
func (t *turn) salvageCall(name string, args map[string]any) (bool, *mcpclient.CallResult, error) {
	if run.Overdue(t.rc.Run.DeadlineAt, t.h.Now()) {
		return false, nil, nil
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return false, nil, err
	}
	canon, err := canonical.Canonical(json.RawMessage(raw))
	if err != nil {
		canon = raw
	}
	digest, _ := canonical.Digest(json.RawMessage(canon))
	t.salvageN++
	tc := run.ToolCall{
		RunID: t.rc.Run.ID, ID: fmt.Sprintf("salvage_%d", t.salvageN), Step: t.def.Limits.MaxSteps,
		MessageID: "salvage", Tool: name, Arguments: string(canon), ArgumentsSHA256: digest,
		CreatedAt: t.h.Now(), State: run.CallConfirmed,
	}
	if err := t.h.Store.RecordToolCalls(t.ctx, t.claim, []run.ToolCall{tc}, nil); err != nil {
		return false, nil, err
	}
	ok, err := t.execute(tc)
	return ok, t.lastResult, err
}

func listedFiles(res *mcpclient.CallResult) []string {
	if res == nil {
		return nil
	}
	var s struct {
		Files []struct {
			Path string `json:"path"`
			Size int64  `json:"size"`
		} `json:"files"`
	}
	if json.Unmarshal(res.StructuredContent, &s) == nil && s.Files != nil {
		var out []string
		for _, f := range s.Files {
			if f.Path != "" && f.Size > 0 {
				out = append(out, f.Path)
			}
		}
		return out
	}
	var out []string
	for _, line := range strings.Split(res.Text(1<<20), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if _, err := fmt.Sscanf(fields[0], "%d", new(int)); err != nil {
			continue
		}
		out = append(out, fields[1])
	}
	return out
}

func exitCode(res *mcpclient.CallResult) (int, bool) {
	if res == nil {
		return 0, false
	}
	var s struct {
		Exit *float64 `json:"exitCode"`
	}
	if json.Unmarshal(res.StructuredContent, &s) == nil && s.Exit != nil {
		return int(*s.Exit), true
	}
	text := res.Text(80)
	var n int
	if _, err := fmt.Sscanf(text, "exit code %d", &n); err == nil {
		return n, true
	}
	return 0, false
}

func exportSucceeded(res *mcpclient.CallResult) bool {
	if res == nil {
		return false
	}
	var s struct {
		File *struct {
			Ref string `json:"ref"`
		} `json:"deliveredFile"`
	}
	return json.Unmarshal(res.StructuredContent, &s) == nil && s.File != nil && s.File.Ref != ""
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func humanRemain(d time.Duration) string {
	if d < time.Second {
		return "under a second"
	}
	s := int(d.Round(time.Second).Seconds())
	if s < 60 {
		return plural(s, "second")
	}
	m := int(d.Round(time.Minute).Minutes())
	if m < 60 {
		return plural(m, "minute")
	}
	h := m / 60
	m = m % 60
	if m == 0 {
		return plural(h, "hour")
	}
	return plural(h, "hour") + " " + plural(m, "minute")
}
