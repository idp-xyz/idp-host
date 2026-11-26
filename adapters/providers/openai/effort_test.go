package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"idp-host/internal/agui"
	"idp-host/internal/model"
)

// D63-2 as the question rewrite needs it: reasoning_effort goes only to a provider that declared its models take it.
func TestReasoningEffortIsSentOnlyToAProviderThatDeclaredIt(t *testing.T) {
	for _, c := range []struct {
		declared bool
		effort   string
		want     any
	}{{true, model.EffortLow, "low"}, {true, model.EffortHigh, "high"}, {true, model.EffortOff, "none"}, {true, "", nil}, {false, model.EffortLow, nil}, {false, model.EffortOff, nil}} {
		var body map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"x"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		}))
		hc, _ := model.NewHTTPClient(5*time.Second, "")
		p, err := New(Config{Name: "p", BaseURL: srv.URL + "/v1", HTTP: hc, ReasoningEffort: c.declared})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Complete(context.Background(), model.Request{Model: "m", Effort: c.effort, Messages: []agui.Message{{ID: "u", Role: agui.RoleUser, Content: "hi"}}}, func(model.Delta) {}); err != nil {
			t.Fatal(err)
		}
		srv.Close()
		if got := body["reasoning_effort"]; got != c.want {
			t.Errorf("declared %v effort %q: sent %v, want %v", c.declared, c.effort, got, c.want)
		}
	}
}
