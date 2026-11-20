package harness

// D53: the thread and the event log of a recorded step, as the code before D53 wrote them (testdata/sequential_step.golden).
// The golden file was written by that code with TestWriteSequentialGolden (IDP_WRITE_GOLDEN=1); nothing in this file
// depends on D53, so the same file runs against the older code.

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/run"
	"idp-host/tests/fixtures/fakemcp"
	"idp-host/tests/fixtures/fakemodel"
)

const goldenFile = "testdata/sequential_step.golden"

// goldenRun is a step of six calls: two reads, a tool without annotations, a call to a tool the agent does not have, a read
// the tool reports an error for, and a read. The harness is not run yet.
func goldenRun(t *testing.T) (*env, *memStore) {
	t.Helper()
	e := setup(t)
	schema := `{"type":"object","properties":{"id":{"type":"string"},"fail":{"type":"boolean"}}}`
	answer := func(prefix string) func(map[string]any) (string, bool) {
		return func(args map[string]any) (string, bool) {
			id, _ := args["id"].(string)
			if f, _ := args["fail"].(bool); f {
				return prefix + " " + id + " failed", true
			}
			return prefix + " " + id, false
		}
	}
	e.mcp.Tools = append(e.mcp.Tools,
		fakemcp.Tool{Name: "read", InputSchema: schema, Annotations: map[string]any{"readOnlyHint": true}, Handle: answer("read"), Delay: 50 * time.Millisecond},
		fakemcp.Tool{Name: "plain", InputSchema: schema, Handle: answer("plain"), Delay: 50 * time.Millisecond})
	e.model.Script = func(messages []fakemodel.Message, _ []string) fakemodel.Turn {
		if spoken(messages).Role == "tool" {
			return fakemodel.Turn{Text: "done"}
		}
		return fakemodel.Turn{ToolCalls: []fakemodel.Call{
			{Name: "read", Args: `{"id":"a"}`}, {Name: "read", Args: `{"id":"b"}`}, {Name: "plain", Args: `{"id":"p"}`},
			{Name: "nowhere", Args: `{}`}, {Name: "read", Args: `{"id":"c","fail":true}`}, {Name: "read", Args: `{"id":"d"}`}}}
	}
	d := def()
	d.Tools.Allow = []string{agent.AllowAll}
	m := newMem(d, user("go"), run.Input{})
	e.h.Store = m
	return e, m
}

var msgIDRE = regexp.MustCompile(`msg_[0-9a-f]{32}`)

// transcript is the thread and the event log of the run, without what differs from one run to the next (the time, the
// duration of a model call, the random ids of messages, which are numbered in the order they appear).
func transcript(m *memStore) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var sb strings.Builder
	sb.WriteString("THREAD\n")
	for _, msg := range m.rc.Messages {
		b, _ := json.Marshal(msg)
		sb.Write(b)
		sb.WriteString("\n")
	}
	sb.WriteString("EVENTS\n")
	for _, ev := range m.events {
		c := agui.Event{}
		for k, v := range ev {
			if k != "timestamp" {
				c[k] = v
			}
		}
		if v, ok := c["value"].(map[string]any); ok && c["name"] == agui.CustomUsage {
			w := map[string]any{}
			for k, x := range v {
				if k != "latencyMs" && k != "firstOutputMs" && k != "firstTextMs" && k != "afterFirstMs" { // timings differ from run to run (D54)
					w[k] = x
				}
			}
			c["value"] = w
		}
		b, _ := json.Marshal(c)
		sb.Write(b)
		sb.WriteString("\n")
	}
	ids := map[string]string{}
	return msgIDRE.ReplaceAllStringFunc(sb.String(), func(id string) string {
		if _, ok := ids[id]; !ok {
			ids[id] = "msg#" + string(rune('a'+len(ids)))
		}
		return ids[id]
	})
}

func readGolden(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(goldenFile)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestWriteSequentialGolden writes the golden file; it is run on the code before D53 and then never again.
func TestWriteSequentialGolden(t *testing.T) {
	if os.Getenv("IDP_WRITE_GOLDEN") != "1" {
		t.Skip("writes testdata/sequential_step.golden; set IDP_WRITE_GOLDEN=1 on the code whose output is to be kept")
	}
	e, m := goldenRun(t)
	if err := e.h.Run(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(goldenFile, []byte(transcript(m)), 0o644); err != nil {
		t.Fatal(err)
	}
}
