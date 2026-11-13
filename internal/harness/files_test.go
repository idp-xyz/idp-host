package harness

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/run"
	"idp-host/tests/fixtures/fakemcp"
)

const (
	readTicket  = "eyJhbGciOiJFUzI1NiJ9.eyJraW5kIjoicmVhZCJ9.cmVhZA"
	writeTicket = "eyJhbGciOiJFUzI1NiJ9.eyJraW5kIjoid3JpdGUifQ.d3JpdGU"
)

// sandboxTools stand in for idp-sandbox's tools as the gateway lists them: the transfer tools mark the properties that
// take a member's file with format idp-attachment / idp-delivery.
func sandboxTools(fx *fakemcp.Server) {
	fx.Tools = append(fx.Tools,
		fakemcp.Tool{Name: "sandbox_create", InputSchema: `{"type":"object"}`, Handle: func(map[string]any) (string, bool) {
			return "workspace 0123456789abcdef0123456789abcdef created", false
		}},
		fakemcp.Tool{Name: "sandbox_import_file", InputSchema: `{"type":"object","properties":{"workspaceId":{"type":"string"},"source":{"type":"string","format":"idp-attachment"},"path":{"type":"string"}}}`,
			Handle: func(a map[string]any) (string, bool) { return "imported to " + a["path"].(string), false }},
		fakemcp.Tool{Name: "sandbox_run", InputSchema: `{"type":"object","properties":{"workspaceId":{"type":"string"},"language":{"type":"string"},"code":{"type":"string"},"note":{"type":"string"}}}`,
			Handle: func(map[string]any) (string, bool) { return "exit code 0", false }},
		fakemcp.Tool{Name: "sandbox_export_file", InputSchema: `{"type":"object","properties":{"workspaceId":{"type":"string"},"path":{"type":"string"},"target":{"type":"string","format":"idp-delivery"},"name":{"type":"string"}}}`,
			Handle: func(a map[string]any) (string, bool) { return "delivered " + a["name"].(string), false },
			Structured: func(a map[string]any) any {
				return map[string]any{"deliveredFile": map[string]any{"ref": "file-out-1", "name": a["name"], "mediaType": "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "size": 4096, "sha256": strings.Repeat("ab", 32)}}
			}},
	)
}

func filesDef() agent.Definition {
	d := def()
	d.Tools.Allow = []string{"sandbox_create", "sandbox_import_file", "sandbox_run", "sandbox_export_file"}
	d.Limits.MaxSteps = 10
	return d
}

func filesInput() run.Input {
	return run.Input{Delivery: true, Attachments: []run.Attachment{{Ref: "file-in-1", Name: "报价单 & 附录.docx", MediaType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", Size: 48213, SHA256: strings.Repeat("3b", 32)}}}
}

func TestFilesGoThroughTicketsTheModelNeverSees(t *testing.T) {
	e := setup(t)
	sandboxTools(e.mcp)
	m := newMem(filesDef(), user("edit office: officecli set in.docx /body/p[1] --prop text=x && cp in.docx out.docx"), filesInput())
	m.fileTickets = &run.FileTickets{Read: []string{readTicket}, Write: writeTicket}
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if m.status != run.StatusCompleted || !strings.Contains(m.last().Content, "delivered 报价单 & 附录-edited.docx") {
		t.Fatalf("%s %+v", m.status, m.last())
	}
	// the gateway got the tickets, in the marked properties only
	var names []string
	for _, c := range e.mcp.Calls() {
		names = append(names, c.Name)
		switch c.Name {
		case "sandbox_import_file":
			if c.Args["source"] != readTicket || c.Args["path"] != "in.docx" {
				t.Fatalf("import: %v", c.Args)
			}
		case "sandbox_export_file":
			if c.Args["target"] != writeTicket || c.Args["path"] != "out.docx" {
				t.Fatalf("export: %v", c.Args)
			}
		}
	}
	if strings.Join(names, ",") != "sandbox_create,sandbox_import_file,sandbox_run,sandbox_export_file" {
		t.Fatalf("calls %v", names)
	}
	// the model was told about the file and never saw a ticket; the records and events keep what it wrote
	var seen strings.Builder
	for _, msg := range e.model.Last() {
		seen.WriteString(msg.Content)
		for _, c := range msg.ToolCalls {
			seen.WriteString(c.Args)
		}
	}
	if !strings.Contains(seen.String(), `attachment:1  "报价单 & 附录.docx"  application/vnd.openxmlformats-officedocument.wordprocessingml.document  47 KB`) {
		t.Fatalf("the model was not told about the file:\n%s", seen.String())
	}
	if !strings.Contains(seen.String(), "target output") {
		t.Fatal("the model was not told it can deliver")
	}
	everything, _ := json.Marshal(struct {
		Events []agui.Event
		Calls  map[string]*run.ToolCall
		Msgs   []agui.Message
	}{m.events, m.calls, m.rc.Messages})
	if strings.Contains(seen.String()+string(everything), "eyJ") {
		t.Fatal("a ticket reached the model, the event log, the call records or the conversation")
	}
	recorded := false
	for _, c := range m.calls {
		recorded = recorded || c.Tool == "sandbox_import_file" && strings.Contains(c.Arguments, `"source":"attachment:1"`)
	}
	if !recorded {
		t.Fatal("the recorded call lost what the model wrote")
	}
	var ev agui.Event
	for _, x := range m.events {
		if x.Type() == agui.EventCustom && x["name"] == agui.CustomFileDelivered {
			ev = x
		}
	}
	if ev == nil {
		t.Fatalf("no file.delivered: %v", m.types())
	}
	v := ev["value"].(map[string]any)
	if v["ref"] != "file-out-1" || v["name"] != "报价单 & 附录-edited.docx" || v["toolCallId"] == "" {
		t.Fatalf("%v", v)
	}
}

func TestFileReferencesTheRunDoesNotHaveAreRefused(t *testing.T) {
	e := setup(t)
	sandboxTools(e.mcp)
	m := newMem(filesDef(), user("x"), run.Input{Attachments: filesInput().Attachments}) // no delivery
	m.fileTickets = &run.FileTickets{Read: []string{readTicket}}
	e.h.Store = m
	turn := &turn{h: e.h, ctx: context.Background(), claim: claim, rc: &m.rc, def: filesDef(), visible: map[string]bool{}, frontend: map[string]bool{}}
	e.h.defaults()
	if err := turn.prepareTools(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ tool, args, want string }{
		{"sandbox_import_file", `{"workspaceId":"w","source":"attachment:2","path":"a"}`, "there is no attachment:2"},
		{"sandbox_import_file", `{"workspaceId":"w","source":"` + readTicket + `","path":"a"}`, "must be attachment:N"},
		{"sandbox_import_file", `{"workspaceId":"w","source":"attachment:0","path":"a"}`, "must be attachment:N"},
		{"sandbox_import_file", `{"workspaceId":"w","source":7,"path":"a"}`, "must be a string"},
		{"sandbox_export_file", `{"workspaceId":"w","path":"a","target":"output"}`, "cannot be delivered"},
		{"sandbox_export_file", `{"workspaceId":"w","path":"a","target":"elsewhere"}`, "must be output"},
	} {
		_, _, refusal, err := turn.fileArguments(run.ToolCall{ID: "c", Tool: c.tool, Arguments: c.args})
		if err != nil || !strings.Contains(refusal, c.want) {
			t.Errorf("%s %s: %q %v", c.tool, c.args, refusal, err)
		}
	}
	// a tool that does not declare a file property never gets a ticket, whatever the model writes
	args, _, refusal, _ := turn.fileArguments(run.ToolCall{ID: "c", Tool: "sandbox_run", Arguments: `{"workspaceId":"w","note":"output","code":"attachment:1"}`})
	if refusal != "" || strings.Contains(string(args), "eyJ") {
		t.Fatalf("%s %s", args, refusal)
	}
	if len(e.mcp.Calls()) != 0 {
		t.Fatal("a refused call reached the gateway")
	}
}

func TestNoFileNoteWithoutFiles(t *testing.T) {
	e := setup(t)
	sandboxTools(e.mcp)
	m := newMem(filesDef(), user("hello"), run.Input{})
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	for _, msg := range e.model.Last() {
		if strings.Contains(msg.Content, "attachment:") || strings.Contains(msg.Content, "target output") {
			t.Fatalf("a files note without files: %q", msg.Content)
		}
	}
}
