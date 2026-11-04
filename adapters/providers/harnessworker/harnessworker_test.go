package harnessworker

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"idp-host/internal/agui"
	"idp-host/internal/model"
	"idp-host/tests/fixtures/fakemodel"
)

func TestWorkerProtocol(t *testing.T) {
	var got map[string]any
	script := `{"type":"reasoning","delta":"think"}
{"type":"text","delta":"The sum "}
{"type":"text","delta":"is 3."}
{"type":"tool_call","id":"c1","name":"sum","arguments":"{\"a\":1,\"b\":2}"}
{"type":"done","stopReason":"tool_calls","usage":{"promptTokens":12,"completionTokens":5}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/turn" || r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(401)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, line := range strings.Split(script, "\n") {
			_, _ = w.Write([]byte("data: " + line + "\n\n"))
		}
	}))
	defer srv.Close()
	hc, _ := model.NewHTTPClient(5*time.Second, "")
	p, err := New(Config{Name: "w", Endpoint: srv.URL, APIKey: "k", HTTP: hc})
	if err != nil {
		t.Fatal(err)
	}
	var deltas []string
	resp, err := p.Complete(context.Background(), model.Request{Model: "m", Instructions: "help", Messages: []agui.Message{{ID: "u1", Role: "user", Content: "sum 1 and 2"}},
		Tools: []model.ToolSpec{{Name: "sum", Description: "adds", Parameters: json.RawMessage(`{"type":"object"}`)}}, MaxOutputTokens: 100}, func(d model.Delta) { deltas = append(deltas, d.Text+"|"+d.Reasoning) })
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Content != "The sum is 3." || resp.Reasoning != "think" || resp.StopReason != model.StopToolCalls || resp.Usage.PromptTokens != 12 || len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].Function.Name != "sum" {
		t.Fatalf("%+v", resp)
	}
	if got["instructions"] != "help" || len(got["tools"].([]any)) != 1 || got["maxOutputTokens"] != 100.0 {
		t.Fatalf("%v", got)
	}
	if strings.Join(deltas, ",") != "|think,The sum |,is 3.|" {
		t.Fatalf("%v", deltas)
	}
	// error event, bad status, stream without done
	for name, body := range map[string]string{
		"error":   `{"type":"error","code":"upstream_503","message":"down","retryable":true}`,
		"no done": `{"type":"text","delta":"x"}`,
		"unknown": `{"type":"surprise"}`,
	} {
		script = body
		_, err := p.Complete(context.Background(), model.Request{Model: "m"}, nil)
		var me *model.Error
		if err == nil || !errorsAs(err, &me) {
			t.Fatalf("%s: %v", name, err)
		}
		if name == "error" && (me.Code != "worker_upstream_503" || !me.Retryable) {
			t.Fatalf("%s: %+v", name, me)
		}
	}
	p2, _ := New(Config{Name: "w", Endpoint: srv.URL, APIKey: "wrong", HTTP: hc})
	if _, err := p2.Complete(context.Background(), model.Request{Model: "m"}, nil); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("%v", err)
	}
}

func errorsAs(err error, target **model.Error) bool {
	me, ok := err.(*model.Error)
	if ok {
		*target = me
	}
	return ok
}

// TestReferencePythonWorker runs deploy/harness-worker/worker.py against the fake model: the whole
// loop from the HOST's request to the worker's events, with no SDK involved.
func TestReferencePythonWorker(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	fm := fakemodel.New()
	up := httptest.NewServer(fm.Handler())
	defer up.Close()
	script, _ := filepath.Abs("../../../deploy/harness-worker/worker.py")
	if _, err := os.Stat(script); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(py, "-I", script, "--listen", "127.0.0.1:0", "--upstream", up.URL+"/v1", "--token", "t")
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	sc := bufio.NewScanner(stdout)
	addr := ""
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "listening on ") {
			addr = strings.TrimPrefix(sc.Text(), "listening on ")
			break
		}
	}
	if addr == "" {
		t.Fatal("worker did not report its address")
	}
	hc, _ := model.NewHTTPClient(10*time.Second, "")
	p, err := New(Config{Name: "py", Endpoint: "http://" + addr, APIKey: "t", HTTP: hc})
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	resp, err := p.Complete(context.Background(), model.Request{Model: "fake-1", Instructions: "help", Messages: []agui.Message{{ID: "u1", Role: "user", Content: "please sum"}},
		Tools: []model.ToolSpec{{Name: "sum", Description: "adds", Parameters: json.RawMessage(`{"type":"object"}`)}}}, func(d model.Delta) { text.WriteString(d.Text) })
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].Function.Name != "sum" || resp.StopReason != model.StopToolCalls || resp.Usage.PromptTokens == 0 {
		t.Fatalf("%+v", resp)
	}
	resp, err = p.Complete(context.Background(), model.Request{Model: "fake-1", Messages: []agui.Message{{ID: "u1", Role: "user", Content: "hello"}}}, func(d model.Delta) { text.WriteString(d.Text) })
	if err != nil || resp.Message.Content != "Echo: hello" || text.String() != "Echo: hello" {
		t.Fatalf("%v %+v %q", err, resp, text.String())
	}
}
