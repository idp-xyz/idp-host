package harness

import (
	"context"
	"strings"
	"testing"

	"idp-host/internal/run"
)

// The gateway refuses arguments that fail the tool's pinned schema and says why; the model gets the reason (so it can
// correct the call instead of repeating it), never a file ticket that may appear in it.
func TestRefusedArgumentsReachTheModelWithTheReason(t *testing.T) {
	e := setup(t)
	for i := range e.mcp.Tools {
		if e.mcp.Tools[i].Name == "sum" {
			e.mcp.Tools[i].Refuse = func(map[string]any) string {
				return "jsonschema validation failed with 'urn:tool'\n- at '': missing property 'language'"
			}
		}
	}
	m := newMem(def(), user("please sum"), run.Input{})
	e.h.Store = m
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	var c *run.ToolCall
	for _, x := range m.calls {
		c = x
	}
	if c == nil || c.State != run.CallFailed || !strings.Contains(c.Result, "Invalid arguments for tool sum: jsonschema validation failed") || !strings.Contains(c.Result, "missing property 'language'") {
		t.Fatalf("%+v", c)
	}
}

func TestTicketsNeverReachTheModelThroughAnError(t *testing.T) {
	e := setup(t)
	sandboxTools(e.mcp)
	for i := range e.mcp.Tools {
		if e.mcp.Tools[i].Name == "sandbox_import_file" {
			e.mcp.Tools[i].Refuse = func(a map[string]any) string {
				return "at '/source': '" + a["source"].(string) + "' is longer than allowed"
			}
		}
	}
	m := newMem(filesDef(), user("edit office: cp in.docx out.docx"), filesInput())
	m.fileTickets = &run.FileTickets{Read: []string{readTicket}, Write: writeTicket}
	e.h.Store = m
	_ = e.h.Run(context.Background(), claim)
	refused := false
	for _, c := range m.calls {
		if c.Tool == "sandbox_import_file" {
			refused = true
			if strings.Contains(c.Result, readTicket) || !strings.Contains(c.Result, "[file ticket]") {
				t.Fatalf("the refusal shown to the model: %q", c.Result)
			}
		}
	}
	for _, msg := range e.model.Last() {
		if strings.Contains(msg.Content, "eyJ") {
			t.Fatal("a ticket reached the model")
		}
	}
	if !refused {
		t.Fatal("the import was not attempted")
	}
}
