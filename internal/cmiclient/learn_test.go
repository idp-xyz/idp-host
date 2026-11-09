package cmiclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"idp-host/internal/memfacts"
	"idp-host/internal/model"
)

// fakeFacts is a CMI that lists one known fact and answers fact writes by key.
type fakeFacts struct {
	mu     sync.Mutex
	posts  []seenWrite
	lists  []string
	answer map[string]int // key -> status; default 201
	body   map[string]string
}

func (f *fakeFacts) handler(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/memory/records":
		f.lists = append(f.lists, r.URL.RawQuery+"|"+r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"records":[{"structured":{"fact":{"key":"favourite_colour","value":"青色"}}},{"structured":{"runId":"x"}}]}`))
	case r.Method == http.MethodPost && r.URL.Path == "/v1/memory/facts":
		f.posts = append(f.posts, seenWrite{r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key"), r.Header.Get("Content-Type"), b})
		var req struct{ Key string }
		_ = json.Unmarshal(b, &req)
		code := 201
		if c, ok := f.answer[req.Key]; ok {
			code = c
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(f.body[req.Key]))
	default:
		w.WriteHeader(404)
	}
}

type scripted struct {
	answer string
	err    error
	got    model.Request
}

func (s *scripted) Name() string { return "scripted" }
func (s *scripted) Complete(_ context.Context, req model.Request, _ func(model.Delta)) (*model.Response, error) {
	s.got = req
	if s.err != nil {
		return nil, s.err
	}
	r := &model.Response{}
	r.Message.Content = s.answer
	return r, nil
}

func newLearner(t *testing.T, p *scripted, f *fakeFacts) *Remember {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	return &Remember{Client: &Client{Endpoint: srv.URL, HTTP: srv.Client()}, Source: "idp-host", Sensitivity: "internal", TTL: 30 * 24 * time.Hour, MaxChars: 1500, Mode: "facts",
		Key: func() (string, error) { return "writer-key", nil }, Backoff: time.Millisecond, Extractor: &memfacts.Extractor{Provider: p, Model: "m"}}
}

func TestLearnWritesFactsAndNeverShowsTheModelTheAnswer(t *testing.T) {
	f := &fakeFacts{answer: map[string]int{"city": 200}, body: map[string]string{"favourite_colour": `{"outcome":"updated"}`}}
	p := &scripted{answer: `{"facts":[{"key":"favourite_colour","value":"红色","statement":"用户最喜欢的颜色是红色"},{"key":"city","value":"成都","statement":"用户住在成都"}]}`}
	r := newLearner(t, p, f)
	x := exchange()
	x.Message, x.Answer = "我现在最喜欢红色，住在成都", "SECRET-ANSWER-TEXT"
	x.At = time.Date(2026, 11, 6, 10, 0, 0, 123456000, time.UTC)
	outs, err := r.Learn(context.Background(), x, nil)
	if err != nil || strings.Join(outs, ",") != "fact_updated,fact_unchanged" {
		t.Fatalf("%v %v", outs, err)
	}
	if strings.Contains(p.got.Messages[0].Content, "SECRET-ANSWER-TEXT") || !strings.Contains(p.got.Messages[0].Content, "我现在最喜欢红色") || !strings.Contains(p.got.Messages[0].Content, "favourite_colour = 青色") {
		t.Fatalf("the model sees the user's message and the known facts, never the answer:\n%s", p.got.Messages[0].Content)
	}
	if len(f.lists) != 1 || !strings.Contains(f.lists[0], "scopeId=alice") || !strings.Contains(f.lists[0], "scopeKind=subject") || !strings.HasSuffix(f.lists[0], "Bearer writer-key") {
		t.Fatalf("the known facts are read for the subject: %v", f.lists)
	}
	if len(f.posts) != 2 || f.posts[0].idem != "host-fact-run_abc-favourite_colour" || f.posts[0].auth != "Bearer writer-key" {
		t.Fatalf("%+v", f.posts)
	}
	var got struct {
		Key, Value, Kind, Content string
		Scope                     struct{ Kind, ID string }
		Tags                      []string
		Provenance                map[string]string
		Validity                  map[string]string
		Sensitivity               string
	}
	_ = json.Unmarshal(f.posts[0].body, &got)
	if got.Key != "favourite_colour" || got.Value != "红色" || got.Kind != "semantic" || got.Scope.Kind != "subject" || got.Scope.ID != "alice" || got.Provenance["ref"] != "run:run_abc/fact/favourite_colour" ||
		got.Validity["validFrom"] != "2026-11-06T10:00:00.123456Z" || got.Validity["validUntil"] != "" || got.Sensitivity != "internal" {
		t.Fatalf("%+v", got)
	}
	// the record part of the body is a valid MemoryRecord once key and value (the endpoint's own fields) are taken out
	var m map[string]any
	_ = json.Unmarshal(f.posts[0].body, &m)
	delete(m, "key")
	delete(m, "value")
	m["recordId"], m["status"], m["acl"], m["createdAt"] = "mem_1", "active", map[string]any{}, "2026-11-06T10:00:00Z"
	validAsMemoryRecordMap(t, m)
}

func TestLearnOutcomes(t *testing.T) {
	cases := []struct {
		name   string
		p      *scripted
		status map[string]int
		want   string
	}{
		{"older-than-current", &scripted{answer: `{"facts":[{"key":"city","value":"x","statement":"s"}]}`}, map[string]int{"city": 409}, "fact_stale"},
		{"rejected", &scripted{answer: `{"facts":[{"key":"city","value":"x","statement":"s"}]}`}, map[string]int{"city": 422}, "rejected"},
		{"nothing-to-remember", &scripted{answer: `{"facts":[]}`}, nil, "no_facts"},
		{"extraction-failed", &scripted{answer: "no json"}, nil, "extract_failed"},
		{"model-down", &scripted{err: context.DeadlineExceeded}, nil, "extract_failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeFacts{answer: c.status}
			outs, _ := newLearner(t, c.p, f).Learn(context.Background(), exchange(), nil)
			if len(outs) != 1 || outs[0] != c.want {
				t.Fatalf("%v, want %s", outs, c.want)
			}
		})
	}
	t.Run("server-errors-are-retried", func(t *testing.T) {
		f := &fakeFacts{answer: map[string]int{"city": 503}}
		r := newLearner(t, &scripted{answer: `{"facts":[{"key":"city","value":"x","statement":"s"}]}`}, f)
		r.Attempts = 3
		outs, err := r.Learn(context.Background(), exchange(), nil)
		if len(outs) != 1 || outs[0] != OutcomeFailed || err == nil || len(f.posts) != 3 {
			t.Fatalf("%v %v %d", outs, err, len(f.posts))
		}
	})
}
