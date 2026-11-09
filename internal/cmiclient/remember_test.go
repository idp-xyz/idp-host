package cmiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"idp-host/internal/credential"
)

type noLoader struct{}

func (noLoader) Load(string) (any, error) { return nil, os.ErrNotExist }

// memoryRecordSchema compiles the pinned idp.telos.cmi MemoryRecord.
func memoryRecordSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	b, err := os.ReadFile("../../tests/contracts/idp.telos.cmi/1.4.0/schema.json")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseLoader(noLoader{})
	c.AssertFormat()
	id := "urn:idp:telos:cmi:pinned"
	if err := c.AddResource(id, doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile(id + "#/$defs/MemoryRecord")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// validAsMemoryRecord validates a request body against the pinned MemoryRecord. The schema describes
// the stored record, so the fields the CMI assigns (recordId, status, acl, createdAt) are added to
// the body first; everything the HOST sends must then satisfy the schema, which forbids unknown fields.
func validAsMemoryRecord(t *testing.T, body []byte) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["status"]; ok {
		t.Errorf("status must be omitted (active): %v", m["status"])
	}
	m["recordId"], m["status"], m["acl"], m["createdAt"] = "mem_1", "active", map[string]any{}, "2026-11-06T10:00:00Z"
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(mustJSON(m)))
	if err != nil {
		t.Fatal(err)
	}
	if err := memoryRecordSchema(t).Validate(inst); err != nil {
		t.Fatalf("the request is not a valid MemoryRecord: %v\n%s", err, body)
	}
}

// validAsMemoryRecordMap validates an already decoded body (see validAsMemoryRecord).
func validAsMemoryRecordMap(t *testing.T, m map[string]any) {
	t.Helper()
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(mustJSON(m)))
	if err != nil {
		t.Fatal(err)
	}
	if err := memoryRecordSchema(t).Validate(inst); err != nil {
		t.Fatalf("not a valid MemoryRecord: %v\n%s", err, mustJSON(m))
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

type seenWrite struct {
	auth, idem, ctype string
	body              []byte
}

type fakeCMI struct {
	mu     sync.Mutex
	writes []seenWrite
	status []int // answers by call; the last repeats
}

func (f *fakeCMI) handler(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.writes = append(f.writes, seenWrite{r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key"), r.Header.Get("Content-Type"), b})
	i := len(f.writes) - 1
	if i >= len(f.status) {
		i = len(f.status) - 1
	}
	code := f.status[i]
	f.mu.Unlock()
	if r.Method != http.MethodPost || r.URL.Path != "/v1/memory/records" {
		w.WriteHeader(404)
		return
	}
	w.WriteHeader(code)
	_, _ = w.Write([]byte(`{}`))
}

func newRemember(t *testing.T, status ...int) (*Remember, *fakeCMI) {
	t.Helper()
	f := &fakeCMI{status: status}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	return &Remember{Client: &Client{Endpoint: srv.URL, HTTP: srv.Client()}, Source: "idp-host", Sensitivity: "internal", TTL: 30 * 24 * time.Hour, MaxChars: 1500,
		Key: func() (string, error) { return "writer-key", nil }, Backoff: time.Millisecond}, f
}

var at = time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)

func exchange() Exchange {
	return Exchange{RunID: "run_abc", ThreadID: "thr_1", Agent: "support", Subject: "alice", Message: "what is the returns policy?", Answer: "30 days.", At: at}
}

func TestRememberWritesTheRecord(t *testing.T) {
	r, f := newRemember(t, 201)
	out, err := r.Write(context.Background(), exchange(), nil)
	if out != OutcomeWritten || err != nil || len(f.writes) != 1 {
		t.Fatalf("%s %v %d", out, err, len(f.writes))
	}
	w := f.writes[0]
	if w.auth != "Bearer writer-key" || w.idem != "host-run-run_abc" || w.ctype != "application/json" {
		t.Fatalf("%+v", w)
	}
	validAsMemoryRecord(t, w.body)
	var got struct {
		Scope       struct{ Kind, ID string }
		Kind        string
		Content     string
		Tags        []string
		Structured  map[string]string
		Provenance  map[string]string
		Validity    map[string]string
		Sensitivity string
		Status      *string
	}
	if err := json.Unmarshal(w.body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Scope.Kind != "subject" || got.Scope.ID != "alice" || got.Kind != "episodic" || got.Sensitivity != "internal" || got.Status != nil {
		t.Fatalf("%+v", got)
	}
	if got.Content != "The user said: what is the returns policy?\nThe assistant answered: 30 days." {
		t.Fatalf("content %q", got.Content)
	}
	if strings.Join(got.Tags, ",") != "conversation,agent-support" {
		t.Fatalf("tags %v", got.Tags)
	}
	if got.Structured["runId"] != "run_abc" || got.Structured["agent"] != "support" || got.Structured["threadId"] != "thr_1" || len(got.Structured) != 3 {
		t.Fatalf("%v", got.Structured)
	}
	p := got.Provenance
	if p["source"] != "idp-host" || p["ref"] != "run:run_abc" || p["observedAt"] != "2026-11-06T10:00:00Z" || p["recordedBy"] != "idp-host" {
		t.Fatalf("%v", p)
	}
	if got.Validity["validUntil"] != "2026-12-06T10:00:00Z" || got.Validity["validFrom"] != "2026-11-06T10:00:00Z" {
		t.Fatalf("%v", got.Validity)
	}
}

func TestRememberCutsOnRuneBoundaries(t *testing.T) {
	r, f := newRemember(t, 201)
	r.MaxChars = 5
	x := exchange()
	x.Message = "你好世界和平与爱心的故事"
	x.Answer = "答案是四十二，这是最终的回答。"
	if out, err := r.Write(context.Background(), x, nil); out != OutcomeWritten || err != nil {
		t.Fatal(out, err)
	}
	var got struct{ Content string }
	_ = json.Unmarshal(f.writes[0].body, &got)
	if got.Content != "The user said: 你好世界和\nThe assistant answered: 答案是四十" {
		t.Fatalf("%q", got.Content)
	}
	validAsMemoryRecord(t, f.writes[0].body)
	// a cut in the middle of a multi-byte rune is impossible; invalid UTF-8 is replaced, not passed on
	if c := Cut("ab\xffcd", 10); c != "ab�cd" {
		t.Fatalf("%q", c)
	}
	if Cut("héllo", 2) != "hé" || Cut("abc", 3) != "abc" {
		t.Fatal("cut")
	}
	// the longest allowed content (two cuts of 8000 four-byte runes) stays inside the CMI's 64 KiB
	r.MaxChars = 8000
	x.Message, x.Answer = strings.Repeat("😀", 9000), strings.Repeat("😀", 9000)
	if c := r.Build(x).Content; len(c) > 64<<10 {
		t.Fatalf("%d bytes", len(c))
	}
}

func TestRememberLongAgentNameKeepsATagThatTheCMIAccepts(t *testing.T) {
	r, _ := newRemember(t, 201)
	x := exchange()
	x.Agent = strings.Repeat("a", 63)
	if tags := r.Build(x).Tags; len(tags) != 1 || tags[0] != "conversation" {
		t.Fatalf("%v", tags)
	}
}

func TestRememberStatusHandling(t *testing.T) {
	cases := []struct {
		name   string
		status []int
		out    string
		calls  int
	}{
		{"created", []int{201}, OutcomeWritten, 1},
		{"idempotent replay", []int{200}, OutcomeReplayed, 1},
		{"conflict is success", []int{409}, OutcomeReplayed, 1},
		{"5xx retried then given up", []int{503}, OutcomeFailed, 4},
		{"5xx then created", []int{500, 502, 201}, OutcomeWritten, 3},
		{"403 not retried", []int{403}, OutcomeRejected, 1},
		{"422 not retried", []int{422}, OutcomeRejected, 1},
		{"401 not retried", []int{401}, OutcomeRejected, 1},
		{"429 retried", []int{429, 201}, OutcomeWritten, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, f := newRemember(t, c.status...)
			out, err := r.Write(context.Background(), exchange(), nil)
			if out != c.out || len(f.writes) != c.calls {
				t.Fatalf("%s (%v) after %d calls, want %s after %d", out, err, len(f.writes), c.out, c.calls)
			}
			if (out == OutcomeWritten || out == OutcomeReplayed) != (err == nil) {
				t.Fatalf("%s %v", out, err)
			}
			for _, w := range f.writes { // every retry carries the same key and body
				if w.idem != "host-run-run_abc" || !bytes.Equal(w.body, f.writes[0].body) {
					t.Fatalf("%+v", w)
				}
			}
		})
	}
}

func TestRememberUnreachableIsRetriedAndGivenUp(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	endpoint := srv.URL
	srv.Close()
	r := &Remember{Client: &Client{Endpoint: endpoint, HTTP: http.DefaultClient}, Source: "idp-host", Sensitivity: "internal", TTL: time.Hour, Key: func() (string, error) { return "k", nil }, Backoff: time.Millisecond, Attempts: 2}
	if out, err := r.Write(context.Background(), exchange(), nil); out != OutcomeFailed || err == nil {
		t.Fatal(out, err)
	}
}

func TestRememberStaticKeyIsReadOnEveryAttempt(t *testing.T) {
	r, f := newRemember(t, 503, 201)
	keys := []string{"old-token", "refreshed-token"}
	n := 0
	r.Key = func() (string, error) { k := keys[n]; n++; return k, nil }
	if out, _ := r.Write(context.Background(), exchange(), nil); out != OutcomeWritten {
		t.Fatal(out)
	}
	if f.writes[0].auth != "Bearer old-token" || f.writes[1].auth != "Bearer refreshed-token" {
		t.Fatalf("%+v", f.writes)
	}
	r.Key = func() (string, error) { return "", os.ErrNotExist }
	if out, _ := r.Write(context.Background(), exchange(), nil); out != OutcomeAuthFailed {
		t.Fatal(out)
	}
}

func TestRememberDelegationExchangesTheOwnersToken(t *testing.T) {
	var forms []url.Values
	var mu sync.Mutex
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		forms = append(forms, r.PostForm)
		mu.Unlock()
		if r.PostForm.Get("grant_type") == "client_credentials" {
			_, _ = w.Write([]byte(`{"access_token":"host-actor","expires_in":300}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"writer-as-alice","expires_in":300}`))
	}))
	defer tokens.Close()
	r, f := newRemember(t, 201)
	r.Key = func() (string, error) { t.Fatal("the static key must not be used when delegating"); return "", nil }
	r.Exchanger = &credential.Exchanger{TokenEndpoint: tokens.URL, ClientID: "idp-host", ClientSecret: "s", Audience: "idp-cmi", HTTP: tokens.Client()}
	r.ActorToken = true
	subject := func(context.Context) (string, error) { return "alice-token", nil }
	if out, err := r.Write(context.Background(), exchange(), subject); out != OutcomeWritten || err != nil {
		t.Fatal(out, err)
	}
	if f.writes[0].auth != "Bearer writer-as-alice" {
		t.Fatalf("%+v", f.writes[0])
	}
	ex := forms[len(forms)-1]
	if ex.Get("grant_type") != "urn:ietf:params:oauth:grant-type:token-exchange" || ex.Get("subject_token") != "alice-token" || ex.Get("audience") != "idp-cmi" || ex.Get("actor_token") != "host-actor" {
		t.Fatalf("%v", ex)
	}
	// without a token to exchange nothing is sent
	before := len(f.writes)
	if out, _ := r.Write(context.Background(), exchange(), func(context.Context) (string, error) { return "", nil }); out != OutcomeNoToken || len(f.writes) != before {
		t.Fatal(out)
	}
	// a refused exchange is an auth failure, not a write
	tokens.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(400) })
	if out, _ := r.Write(context.Background(), exchange(), subject); out != OutcomeAuthFailed || len(f.writes) != before {
		t.Fatal(out)
	}
}

// The pinned schema is not vacuous: it rejects the tag a colon would make, and unknown fields.
func TestRememberSchemaRejectsWhatTheCMIRejects(t *testing.T) {
	r, _ := newRemember(t, 201)
	for name, mutate := range map[string]func(m map[string]any){
		"colon tag":      func(m map[string]any) { m["tags"] = []string{"conversation", "agent:support"} },
		"unknown field":  func(m map[string]any) { m["extra"] = 1 },
		"empty content":  func(m map[string]any) { m["content"] = "" },
		"bad source":     func(m map[string]any) { m["provenance"].(map[string]any)["source"] = "Idp Host" },
		"bad sensitive.": func(m map[string]any) { m["sensitivity"] = "secret" },
	} {
		var m map[string]any
		_ = json.Unmarshal(mustJSON(r.Build(exchange())), &m)
		m["recordId"], m["status"], m["acl"], m["createdAt"] = "mem_1", "active", map[string]any{}, "2026-11-06T10:00:00Z"
		mutate(m)
		inst, _ := jsonschema.UnmarshalJSON(bytes.NewReader(mustJSON(m)))
		if memoryRecordSchema(t).Validate(inst) == nil {
			t.Errorf("%s: the schema accepted it", name)
		}
	}
}
