package harness

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"idp-host/internal/agui"
	"idp-host/internal/run"
	"idp-host/tests/fixtures/fakemcp"
	"idp-host/tests/fixtures/fakemodel"
)

func TestBudgetIsToldAndNotStored(t *testing.T) {
	e := setup(t)
	m := newMem(def(), user("hello"), run.Input{})
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	var seen string
	for _, msg := range e.model.Last() {
		seen += msg.Content
	}
	if !strings.Contains(seen, "Budget: this is step 1 of 10. 9 steps after this one.") || !strings.Contains(seen, "The run ends at") {
		t.Fatalf("the model was not told the budget:\n%s", seen)
	}
	for _, msg := range m.rc.Messages {
		if strings.Contains(msg.Content, "Budget:") {
			t.Fatalf("the budget note was stored on the thread: %+v", msg)
		}
	}
}

func TestLastStepsOfferOnlyDelivery(t *testing.T) {
	e := setup(t)
	sandboxTools(e.mcp)
	var offered []string
	e.model.Script = func(msgs []fakemodel.Message, tools []string) fakemodel.Turn {
		offered = append(offered, strings.Join(tools, ","))
		return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "sandbox_run", Args: `{"workspaceId":"w","language":"office","code":"true"}`}}}
	}
	d := filesDef()
	d.Limits.MaxSteps = 4
	d.Tools.Allow = []string{"sandbox_run", "sandbox_export_file"}
	m := newMem(d, user("keep going"), filesInput())
	m.fileTickets = &run.FileTickets{Read: []string{readTicket}, Write: writeTicket}
	e.h.Store = m
	err := e.h.Run(context.Background(), claim)
	var f *run.Failure
	if !errorsAs(err, &f) || f.Code != run.FailStepLimit || m.status != run.StatusFailed {
		t.Fatalf("%v %s", err, m.status)
	}
	var notes []string
	for _, req := range e.model.History() {
		note := ""
		for _, msg := range req {
			if strings.HasPrefix(msg.Content, "Budget:") {
				note = msg.Content
			}
		}
		notes = append(notes, note)
	}
	if len(offered) != 4 || !strings.Contains(offered[0], "sandbox_run") || offered[0] == "sandbox_export_file" {
		t.Fatalf("early tools %q", offered)
	}
	if offered[2] != "sandbox_export_file" || offered[3] != "sandbox_export_file" {
		t.Fatalf("wrap-up tools %q", offered)
	}
	if !strings.Contains(notes[0], "step 1 of 4") || strings.Contains(notes[0], "Wrap-up") {
		t.Fatalf("first note %q", notes[0])
	}
	if !strings.Contains(notes[3], "This is the last step.") || !strings.Contains(notes[3], "Wrap-up:") {
		t.Fatalf("last note %q", notes[3])
	}
	runs := 0
	for _, c := range e.mcp.Calls() {
		if c.Name == "sandbox_run" {
			runs++
		}
		if c.Name == "sandbox_export_file" || c.Name == "sandbox_list_files" {
			t.Fatalf("salvage ran without a workspace: %s", c.Name)
		}
	}
	if runs != 2 {
		t.Fatalf("sandbox_run calls %d, want 2", runs)
	}
	denied := false
	for _, msg := range m.rc.Messages {
		if msg.Role == agui.RoleTool && strings.Contains(msg.Error, "only file delivery is available") {
			denied = true
		}
	}
	if !denied {
		t.Fatal("a wrap-up call was not refused")
	}
	if !strings.Contains(runError(m), "the run reached its limit of 4 steps") || strings.Contains(runError(m), "validated") {
		t.Fatalf("failure %q", runError(m))
	}
}

func TestStepLimitDeliversTheValidatedFile(t *testing.T) {
	e := setup(t)
	sandboxTools(e.mcp)
	e.mcp.Tools = append(e.mcp.Tools, fakemcp.Tool{
		Name: "sandbox_list_files", InputSchema: `{"type":"object","properties":{"workspaceId":{"type":"string"}}}`,
		Handle: func(map[string]any) (string, bool) { return "4 files", false },
		Structured: func(map[string]any) any {
			return map[string]any{"files": []map[string]any{
				{"path": "in.xlsx", "size": 100},
				{"path": "out/报价.xlsx", "size": 2000},
				{"path": "notes.txt", "size": 10},
				{"path": "out/broken.xlsx", "size": 3000},
				{"path": "empty.xlsx", "size": 0},
			}}
		},
	})
	for i := range e.mcp.Tools {
		if e.mcp.Tools[i].Name != "sandbox_run" {
			continue
		}
		e.mcp.Tools[i].Handle = func(a map[string]any) (string, bool) {
			code, _ := a["code"].(string)
			if strings.Contains(code, "broken") {
				return "exit code 1\n--- stdout ---\ninvalid", false
			}
			return "exit code 0\n--- stdout ---\nvalid", false
		}
		e.mcp.Tools[i].Structured = func(a map[string]any) any {
			code, _ := a["code"].(string)
			exit := 0
			if strings.Contains(code, "broken") {
				exit = 1
			}
			return map[string]any{"exitCode": exit}
		}
	}
	wsRe := regexp.MustCompile(`workspace ([0-9a-f]{32}) created`)
	e.model.Script = func(msgs []fakemodel.Message, tools []string) fakemodel.Turn {
		for _, msg := range msgs {
			if msg.Role == "tool" {
				if w := wsRe.FindStringSubmatch(msg.Content); w != nil {
					return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "sandbox_import_file", Args: `{"workspaceId":"` + w[1] + `","source":"attachment:1","path":"in.xlsx"}`}}}
				}
			}
		}
		return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "sandbox_create", Args: `{}`}}}
	}
	d := filesDef()
	d.Limits.MaxSteps = 2
	d.Tools.Allow = []string{"sandbox_create", "sandbox_import_file", "sandbox_run", "sandbox_list_files", "sandbox_export_file"}
	m := newMem(d, user("rebuild the price list"), filesInput())
	m.fileTickets = &run.FileTickets{Read: []string{readTicket}, Write: writeTicket}
	e.h.Store = m
	err := e.h.Run(context.Background(), claim)
	var f *run.Failure
	if !errorsAs(err, &f) || f.Code != run.FailStepLimit {
		t.Fatalf("%v", err)
	}
	if runError(m) != "the run reached its limit of 2 steps; 1 file was delivered from the workspace as it stood" {
		t.Fatalf("failure %q", runError(m))
	}
	var exported []string
	var validated []string
	for _, c := range e.mcp.Calls() {
		switch c.Name {
		case "sandbox_export_file":
			if c.Args["target"] != writeTicket {
				t.Fatalf("export went out without the write ticket: %v", c.Args["target"])
			}
			exported = append(exported, c.Args["path"].(string))
			if c.Args["name"] != "报价.xlsx" {
				t.Fatalf("name %v", c.Args["name"])
			}
		case "sandbox_run":
			code, _ := c.Args["code"].(string)
			if !strings.HasPrefix(code, "officecli validate ") || c.Args["language"] != "office" {
				t.Fatalf("validate call %v", c.Args)
			}
			validated = append(validated, code)
		}
	}
	if strings.Join(exported, ",") != "out/报价.xlsx" {
		t.Fatalf("exported %v", exported)
	}
	if len(validated) != 2 || !strings.Contains(validated[0], "broken.xlsx") || !strings.Contains(validated[1], "报价.xlsx") {
		t.Fatalf("validated %v", validated)
	}
	for _, c := range m.calls {
		if strings.Contains(c.Arguments, "eyJ") || strings.Contains(c.Arguments, writeTicket) {
			t.Fatalf("a ticket was recorded: %s", c.Arguments)
		}
		if c.Tool == "sandbox_export_file" && !strings.Contains(c.Arguments, `"target":"output"`) {
			t.Fatalf("the record lost output: %s", c.Arguments)
		}
	}
	found := false
	for _, ev := range m.events {
		if ev.Type() == agui.EventCustom && ev["name"] == agui.CustomFileDelivered {
			v := ev["value"].(map[string]any)
			if v["name"] != "报价.xlsx" || v["ref"] != "file-out-1" {
				t.Fatalf("%v", v)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("no file.delivered")
	}
}

func TestStepLimitSkipsAFileAlreadyDelivered(t *testing.T) {
	e := setup(t)
	sandboxTools(e.mcp)
	e.mcp.Tools = append(e.mcp.Tools, fakemcp.Tool{
		Name: "sandbox_list_files", InputSchema: `{"type":"object","properties":{"workspaceId":{"type":"string"}}}`,
		Handle: func(map[string]any) (string, bool) { return "1 files", false },
		Structured: func(map[string]any) any {
			return map[string]any{"files": []map[string]any{{"path": "out.xlsx", "size": 20}}}
		},
	})
	wsRe := regexp.MustCompile(`workspace ([0-9a-f]{32}) created`)
	e.model.Script = func(msgs []fakemodel.Message, tools []string) fakemodel.Turn {
		for _, msg := range msgs {
			if msg.Role == "tool" {
				if w := wsRe.FindStringSubmatch(msg.Content); w != nil {
					return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "sandbox_export_file", Args: `{"workspaceId":"` + w[1] + `","path":"out.xlsx","target":"output","name":"out.xlsx"}`}}}
				}
			}
		}
		return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "sandbox_create", Args: `{}`}}}
	}
	d := filesDef()
	d.Limits.MaxSteps = 2
	d.Tools.Allow = []string{"sandbox_create", "sandbox_list_files", "sandbox_run", "sandbox_export_file"}
	m := newMem(d, user("send it"), filesInput())
	m.fileTickets = &run.FileTickets{Write: writeTicket}
	e.h.Store = m
	err := e.h.Run(context.Background(), claim)
	var f *run.Failure
	if !errorsAs(err, &f) || f.Code != run.FailStepLimit {
		t.Fatalf("%v", err)
	}
	exports := 0
	for _, c := range e.mcp.Calls() {
		if c.Name == "sandbox_export_file" {
			exports++
		}
		if c.Name == "sandbox_run" {
			t.Fatal("validated a file the model had already delivered")
		}
	}
	if exports != 1 {
		t.Fatalf("exports %d", exports)
	}
	if runError(m) != "the run reached its limit of 2 steps" {
		t.Fatalf("failure %q", runError(m))
	}
}

// D42: at the step limit a PDF that opens is delivered like an office file that validates, and so is Markdown the
// model put under out/. A PDF that does not open, Markdown elsewhere (the model's notes) and the member's own file,
// however the model wrote its path when it imported it, stay where they are.
func TestStepLimitDeliversPDFAndMarkdownToo(t *testing.T) {
	e := setup(t)
	sandboxTools(e.mcp)
	e.mcp.Tools = append(e.mcp.Tools, fakemcp.Tool{
		Name: "sandbox_list_files", InputSchema: `{"type":"object","properties":{"workspaceId":{"type":"string"}}}`,
		Handle: func(map[string]any) (string, bool) { return "7 files", false },
		Structured: func(map[string]any) any {
			return map[string]any{"files": []map[string]any{
				{"path": "in/来件.pdf", "size": 9000},
				{"path": "notes.md", "size": 300},
				{"path": "draft.pdf", "size": 4000},
				{"path": "bad.pdf", "size": 4000},
				{"path": "out/摘要.md", "size": 800},
				{"path": "out/报告.pdf", "size": 5000},
				{"path": "out/data.csv", "size": 50},
			}}
		},
	})
	for i := range e.mcp.Tools {
		if e.mcp.Tools[i].Name != "sandbox_run" {
			continue
		}
		e.mcp.Tools[i].Handle = func(map[string]any) (string, bool) { return "ran", false }
		e.mcp.Tools[i].Structured = func(a map[string]any) any {
			code, _ := a["code"].(string)
			exit := 0
			if strings.Contains(code, "bad.pdf") {
				exit = 1
			}
			return map[string]any{"exitCode": exit}
		}
	}
	for i := range e.mcp.Tools {
		if e.mcp.Tools[i].Name == "sandbox_export_file" { // the fixture's reference is the same for every file
			e.mcp.Tools[i].Structured = func(a map[string]any) any {
				return map[string]any{"deliveredFile": map[string]any{"ref": "file-" + a["name"].(string), "name": a["name"], "mediaType": "application/pdf", "size": 1, "sha256": strings.Repeat("ab", 32)}}
			}
		}
	}
	wsRe := regexp.MustCompile(`workspace ([0-9a-f]{32}) created`)
	e.model.Script = func(msgs []fakemodel.Message, tools []string) fakemodel.Turn {
		for _, msg := range msgs {
			if msg.Role == "tool" {
				if w := wsRe.FindStringSubmatch(msg.Content); w != nil {
					return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "sandbox_import_file", Args: `{"workspaceId":"` + w[1] + `","source":"attachment:1","path":"./in/来件.pdf"}`}}}
				}
			}
		}
		return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "sandbox_create", Args: `{}`}}}
	}
	d := filesDef()
	d.Limits.MaxSteps = 2
	d.Tools.Allow = []string{"sandbox_create", "sandbox_import_file", "sandbox_run", "sandbox_list_files", "sandbox_export_file"}
	m := newMem(d, user("summarize the PDF and give me a report"), filesInput())
	m.fileTickets = &run.FileTickets{Read: []string{readTicket}, Write: writeTicket}
	e.h.Store = m
	err := e.h.Run(context.Background(), claim)
	var f *run.Failure
	if !errorsAs(err, &f) || f.Code != run.FailStepLimit {
		t.Fatalf("%v", err)
	}
	if runError(m) != "the run reached its limit of 2 steps; 3 files were delivered from the workspace as they stood" {
		t.Fatalf("failure %q", runError(m))
	}
	var exported, checks []string
	for _, c := range e.mcp.Calls() {
		switch c.Name {
		case "sandbox_export_file":
			if c.Args["target"] != writeTicket {
				t.Fatalf("export went out without the write ticket: %v", c.Args["target"])
			}
			exported = append(exported, c.Args["path"].(string)+" as "+c.Args["name"].(string))
		case "sandbox_run":
			if c.Args["language"] != "office" {
				t.Fatalf("check call %v", c.Args)
			}
			checks = append(checks, c.Args["code"].(string))
		}
	}
	// what is under out/ comes first; Markdown is not run through a check
	if got := strings.Join(exported, ", "); got != "out/报告.pdf as 报告.pdf, out/摘要.md as 摘要.md, draft.pdf as draft.pdf" {
		t.Fatalf("exported %s", got)
	}
	if got := strings.Join(checks, "; "); got != "pdftext --check 'out/报告.pdf'; pdftext --check 'bad.pdf'; pdftext --check 'draft.pdf'" {
		t.Fatalf("checks %s", got)
	}
	delivered := 0
	for _, ev := range m.events {
		if ev.Type() == agui.EventCustom && ev["name"] == agui.CustomFileDelivered {
			delivered++
		}
	}
	if delivered != 3 {
		t.Fatalf("%d file.delivered events", delivered)
	}
}

func TestStepLimitWithoutDeliveryDoesNotTouchTheWorkspace(t *testing.T) {
	e := setup(t)
	sandboxTools(e.mcp)
	e.mcp.Tools = append(e.mcp.Tools, fakemcp.Tool{
		Name: "sandbox_list_files", InputSchema: `{"type":"object"}`,
		Handle: func(map[string]any) (string, bool) { return "1 files", false },
	})
	e.model.Script = func(msgs []fakemodel.Message, tools []string) fakemodel.Turn {
		return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "sandbox_create", Args: `{}`}}}
	}
	d := filesDef()
	d.Limits.MaxSteps = 1
	m := newMem(d, user("loop"), run.Input{}) // delivery not asked for
	e.h.Store = m
	err := e.h.Run(context.Background(), claim)
	var f *run.Failure
	if !errorsAs(err, &f) || f.Code != run.FailStepLimit {
		t.Fatalf("%v", err)
	}
	for _, c := range e.mcp.Calls() {
		if c.Name == "sandbox_list_files" || c.Name == "sandbox_export_file" {
			t.Fatalf("salvage ran: %s", c.Name)
		}
	}
}

func TestSalvageKeepsTheWorkspaceThatRan(t *testing.T) {
	e := setup(t)
	sandboxTools(e.mcp)
	e.mcp.Tools = append(e.mcp.Tools, fakemcp.Tool{
		Name: "sandbox_list_files", InputSchema: `{"type":"object","properties":{"workspaceId":{"type":"string"}}}`,
		Handle: func(map[string]any) (string, bool) { return "1 files", false },
		Structured: func(a map[string]any) any {
			if a["workspaceId"] == "dead" {
				return map[string]any{"files": []any{}}
			}
			return map[string]any{"files": []map[string]any{{"path": "out.xlsx", "size": 10}}}
		},
	})
	for i := range e.mcp.Tools {
		if e.mcp.Tools[i].Name != "sandbox_run" {
			continue
		}
		e.mcp.Tools[i].Handle = func(map[string]any) (string, bool) { return "exit code 0", false }
		e.mcp.Tools[i].Structured = func(map[string]any) any { return map[string]any{"exitCode": 0} }
	}
	wsRe := regexp.MustCompile(`workspace ([0-9a-f]{32}) created`)
	e.model.Script = func(msgs []fakemodel.Message, tools []string) fakemodel.Turn {
		real := ""
		for _, msg := range msgs {
			if msg.Role != "tool" {
				continue
			}
			if w := wsRe.FindStringSubmatch(msg.Content); w != nil {
				real = w[1]
			}
		}
		if len(tools) == 1 && strings.Contains(tools[0], "sandbox_export_file") {
			return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "sandbox_run", Args: `{"workspaceId":"dead","language":"office","code":"true"}`}}}
		}
		if real == "" {
			return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "sandbox_create", Args: `{}`}}}
		}
		return fakemodel.Turn{ToolCalls: []fakemodel.Call{{Name: "sandbox_run", Args: `{"workspaceId":"` + real + `","language":"office","code":"true"}`}}}
	}
	d := filesDef()
	d.Limits.MaxSteps = 4
	d.Tools.Allow = []string{"sandbox_create", "sandbox_run", "sandbox_list_files", "sandbox_export_file"}
	m := newMem(d, user("make the workbook"), filesInput())
	m.fileTickets = &run.FileTickets{Write: writeTicket}
	e.h.Store = m
	err := e.h.Run(context.Background(), claim)
	var f *run.Failure
	if !errorsAs(err, &f) || f.Code != run.FailStepLimit || !strings.Contains(runError(m), "1 file was delivered") {
		t.Fatalf("%v %q", err, runError(m))
	}
	for _, c := range e.mcp.Calls() {
		if c.Name == "sandbox_list_files" && (c.Args["workspaceId"] == "dead" || len(c.Args["workspaceId"].(string)) != 32) {
			t.Fatalf("listed the refused call's workspace: %v", c.Args["workspaceId"])
		}
	}
}

func runError(m *memStore) string {
	ev := m.events[len(m.events)-1]
	return ev["message"].(string)
}

func errorsAs(err error, f **run.Failure) bool {
	if err == nil {
		return false
	}
	ff, ok := err.(*run.Failure)
	if !ok {
		return false
	}
	*f = ff
	return true
}
