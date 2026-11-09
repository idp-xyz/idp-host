package cmiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"idp-host/internal/credential"
	"idp-host/internal/memfacts"
)

// Outcomes of one remember attempt, as counted in idp_host_memory_writes_total{outcome}.
const (
	OutcomeWritten    = "written"       // the CMI recorded it (201)
	OutcomeReplayed   = "replayed"      // the CMI already had it (200 replay or 409): success
	OutcomeRejected   = "rejected"      // a 4xx other than 409/429: not retried (missing role or source, 422, ...)
	OutcomeFailed     = "failed"        // unreachable or 5xx on every attempt
	OutcomeAuthFailed = "auth_failed"   // the writer credential could not be obtained
	OutcomeNoToken    = "no_credential" // delegation without a subject token to exchange
)

// Remember appends one episodic memory per completed run (ADR-0001 D33). Auth is the WRITER
// identity: static (Key is called per write, so a refreshed token file is picked up) or delegation
// (Exchanger trades the run owner's token for the CMI audience).
type Remember struct {
	Client      *Client
	Source      string
	Sensitivity string
	TTL         time.Duration
	MaxChars    int

	Key        func() (string, error) // static writer credential; nil when delegating
	Exchanger  *credential.Exchanger  // delegation
	ActorToken bool

	// Mode is what a finished run writes: "turns" (default: one episodic record per run, D33), "facts" (the facts the
	// user stated, extracted by Extractor and written through POST /v1/memory/facts; no record of the assistant's
	// answer) or "both". HOST ADR D34.
	Mode      string
	Extractor *memfacts.Extractor

	Attempts int           // default 4
	Backoff  time.Duration // first wait, doubled each retry (default 500 ms)
	Timeout  time.Duration // per attempt (default 5 s)
	Now      func() time.Time
}

// Exchange is one finished run to remember. Message and Answer are untrusted text.
type Exchange struct {
	RunID, ThreadID, Agent, Subject string
	Message, Answer                 string
	At                              time.Time
}

// Scope, Provenance and Validity are the parts of the CMI MemoryRecord the HOST writes.
type (
	recScope struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	}
	recProvenance struct {
		Source     string `json:"source"`
		Ref        string `json:"ref"`
		ObservedAt string `json:"observedAt"`
		RecordedBy string `json:"recordedBy"`
	}
	recValidity struct {
		ValidFrom  string `json:"validFrom"`
		ValidUntil string `json:"validUntil"`
	}
)

// Record is the request body of POST /v1/memory/records (idp.telos.cmi MemoryRecord, the part a
// writer supplies; status is omitted = active).
type Record struct {
	Scope       recScope          `json:"scope"`
	Kind        string            `json:"kind"`
	Content     string            `json:"content"`
	Structured  map[string]string `json:"structured"`
	Tags        []string          `json:"tags"`
	Provenance  recProvenance     `json:"provenance"`
	Validity    recValidity       `json:"validity"`
	Sensitivity string            `json:"sensitivity"`
}

// IdempotencyKey is the key every write of a run carries, so a retry or a replay never duplicates.
func IdempotencyKey(runID string) string { return "host-run-" + runID }

// Cut keeps the first n runes of s (on rune boundaries; invalid UTF-8 is replaced first).
func Cut(s string, n int) string {
	s = strings.ToValidUTF8(s, "�")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// Build makes the record for a finished run.
func (r *Remember) Build(x Exchange) Record {
	n := r.MaxChars
	if n <= 0 {
		n = 1500
	}
	at := x.At.UTC()
	rec := Record{
		Scope:       recScope{Kind: "subject", ID: x.Subject},
		Kind:        "episodic",
		Content:     "The user said: " + Cut(x.Message, n) + "\nThe assistant answered: " + Cut(x.Answer, n),
		Structured:  map[string]string{"runId": x.RunID, "agent": x.Agent, "threadId": x.ThreadID},
		Tags:        []string{"conversation"},
		Provenance:  recProvenance{Source: r.Source, Ref: "run:" + x.RunID, ObservedAt: at.Format(time.RFC3339), RecordedBy: "idp-host"},
		Validity:    recValidity{ValidFrom: at.Format(time.RFC3339), ValidUntil: at.Add(r.TTL).Format(time.RFC3339)},
		Sensitivity: r.Sensitivity,
	}
	// The CMI's tag grammar is [a-z0-9][a-z0-9_.-]{0,63}: no colon, so "agent-<name>" (names are
	// lower-case with hyphens); a name too long for a tag stays only in structured.agent.
	if t := "agent-" + x.Agent; len(t) <= 64 {
		rec.Tags = append(rec.Tags, t)
	}
	return rec
}

// Write records the exchange and reports the outcome. subjectToken supplies, only under delegation,
// the token of the run's owner to exchange. Content is never logged by this package.
func (r *Remember) Write(ctx context.Context, x Exchange, subjectToken func(context.Context) (string, error)) (string, error) {
	attempts, backoff, timeout := r.Attempts, r.Backoff, r.Timeout
	if attempts <= 0 {
		attempts = 4
	}
	if backoff <= 0 {
		backoff = 500 * time.Millisecond
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	body, err := json.Marshal(r.Build(x))
	if err != nil {
		return OutcomeRejected, err
	}
	bearer, outcome, err := r.acquire(ctx, subjectToken)
	if err != nil {
		return outcome, err
	}
	var last error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return OutcomeFailed, ctx.Err()
			case <-time.After(backoff << (i - 1)):
			}
		}
		b := bearer
		if r.Exchanger == nil {
			if r.Key == nil {
				return OutcomeAuthFailed, errors.New("no writer credential configured")
			}
			if b, err = r.Key(); err != nil || b == "" {
				return OutcomeAuthFailed, errors.New("writer credential unreadable")
			}
		}
		code, retry, _, err := r.post(ctx, timeout, "/v1/memory/records", b, IdempotencyKey(x.RunID), body)
		switch {
		case err == nil && code == http.StatusCreated:
			return OutcomeWritten, nil
		case err == nil && (code == http.StatusOK || code == http.StatusConflict):
			return OutcomeReplayed, nil
		case err == nil && !retry:
			return OutcomeRejected, fmt.Errorf("%w answered HTTP %d", ErrCMI, code)
		case err == nil:
			last = fmt.Errorf("%w answered HTTP %d", ErrCMI, code)
		default:
			last = err
		}
	}
	return OutcomeFailed, last
}

// post makes one attempt; retry says a failure is worth another (5xx, 429, network).
func (r *Remember) post(ctx context.Context, timeout time.Duration, path, bearer, key string, body []byte) (int, bool, []byte, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	hr, err := http.NewRequestWithContext(cctx, http.MethodPost, r.Client.Endpoint+path, bytes.NewReader(body))
	if err != nil {
		return 0, false, nil, fmt.Errorf("%w: %v", ErrCMI, err)
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "application/json")
	hr.Header.Set("Authorization", "Bearer "+bearer)
	hr.Header.Set("Idempotency-Key", key)
	resp, err := r.Client.HTTP.Do(hr)
	if err != nil {
		return 0, true, nil, fmt.Errorf("%w unreachable: %v", ErrCMI, err)
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	c := resp.StatusCode
	return c, c >= 500 || c == http.StatusTooManyRequests, answer, nil
}

// acquire gets the bearer for the writes of one run: delegation exchanges the owner's token once, static reads the key on
// every use (so it is returned empty here and the caller asks r.Key). The outcome is only meaningful with an error.
func (r *Remember) acquire(ctx context.Context, subjectToken func(context.Context) (string, error)) (bearer, outcome string, err error) {
	if r.Exchanger == nil {
		return "", "", nil
	}
	if subjectToken == nil {
		return "", OutcomeNoToken, errors.New("no subject token to exchange")
	}
	subj, serr := subjectToken(ctx)
	if serr != nil || subj == "" {
		return "", OutcomeNoToken, errors.New("no subject token to exchange")
	}
	actor := ""
	if r.ActorToken {
		t, err := r.Exchanger.ClientCredentials(ctx)
		if err != nil {
			return "", OutcomeAuthFailed, fmt.Errorf("actor token: %w", err)
		}
		actor = t.AccessToken
	}
	t, err := r.Exchanger.Exchange(ctx, subj, actor)
	if err != nil {
		return "", OutcomeAuthFailed, fmt.Errorf("token exchange: %w", err)
	}
	return t.AccessToken, "", nil
}

// token is the bearer for one attempt.
func (r *Remember) token(bearer string) (string, error) {
	if r.Exchanger != nil {
		return bearer, nil
	}
	if r.Key == nil {
		return "", errors.New("no writer credential configured")
	}
	b, err := r.Key()
	if err != nil || b == "" {
		return "", errors.New("writer credential unreadable")
	}
	return b, nil
}

// Outcomes of learning facts, counted with the others in idp_host_memory_writes_total{outcome}.
const (
	OutcomeFactCreated   = "fact_created"   // a new fact
	OutcomeFactUpdated   = "fact_updated"   // a new value closed the old one
	OutcomeFactUnchanged = "fact_unchanged" // the CMI already held this value
	OutcomeFactStale     = "fact_stale"     // the CMI holds a newer value than this statement (409)
	OutcomeExtractFailed = "extract_failed" // the model call or its answer was unusable
	OutcomeNoFacts       = "no_facts"       // the message states nothing to remember
)

// factBody is the request of POST /v1/memory/facts: a MemoryRecord's writer part plus key and value.
type factBody struct {
	Key        string        `json:"key"`
	Value      string        `json:"value"`
	Scope      recScope      `json:"scope"`
	Kind       string        `json:"kind"`
	Content    string        `json:"content"`
	Tags       []string      `json:"tags"`
	Provenance recProvenance `json:"provenance"`
	Validity   struct {
		ValidFrom string `json:"validFrom"`
	} `json:"validity"`
	Sensitivity string `json:"sensitivity"`
}

// known reads the facts the CMI holds for the subject (key, current value). A failure returns what it has: the extraction
// then runs without them (a changed key only costs an update that does not replace).
func (r *Remember) known(ctx context.Context, bearer, subject string) []memfacts.Known {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	q := url.Values{"scopeKind": {"subject"}, "scopeId": {subject}, "limit": {"200"}}
	hr, err := http.NewRequestWithContext(cctx, http.MethodGet, r.Client.Endpoint+"/v1/memory/records?"+q.Encode(), nil)
	if err != nil {
		return nil
	}
	hr.Header.Set("Accept", "application/json")
	hr.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := r.Client.HTTP.Do(hr)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var out struct {
		Records []struct {
			Structured struct {
				Fact *struct{ Key, Value string } `json:"fact"`
			} `json:"structured"`
		} `json:"records"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	var ks []memfacts.Known
	for _, rec := range out.Records {
		if f := rec.Structured.Fact; f != nil && f.Key != "" {
			ks = append(ks, memfacts.Known{Key: f.Key, Value: f.Value})
		}
	}
	return ks
}

// Learn extracts the facts the user stated in x.Message and writes each through POST /v1/memory/facts. It returns one
// outcome per fact (or one for the extraction when it failed or found nothing). The answer is never sent to the model.
func (r *Remember) Learn(ctx context.Context, x Exchange, subjectToken func(context.Context) (string, error)) ([]string, error) {
	attempts, backoff, timeout := r.Attempts, r.Backoff, r.Timeout
	if attempts <= 0 {
		attempts = 4
	}
	if backoff <= 0 {
		backoff = 500 * time.Millisecond
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	bearer, outcome, err := r.acquire(ctx, subjectToken)
	if err != nil {
		return []string{outcome}, err
	}
	first, err := r.token(bearer)
	if err != nil {
		return []string{OutcomeAuthFailed}, err
	}
	facts, err := r.Extractor.Extract(ctx, Cut(x.Message, r.maxChars()), r.known(ctx, first, x.Subject))
	if err != nil {
		return []string{OutcomeExtractFailed}, err
	}
	if len(facts) == 0 {
		return []string{OutcomeNoFacts}, nil
	}
	at := x.At.UTC().Format(time.RFC3339)
	var outcomes []string
	var lastErr error
	for _, f := range facts {
		b := factBody{Key: f.Key, Value: f.Value, Scope: recScope{Kind: "subject", ID: x.Subject}, Kind: "semantic", Content: f.Statement,
			Tags: []string{"fact", "conversation"}, Provenance: recProvenance{Source: r.Source, Ref: "run:" + x.RunID + "/fact/" + f.Key, ObservedAt: at, RecordedBy: "idp-host"}, Sensitivity: r.Sensitivity}
		b.Validity.ValidFrom = at
		body, err := json.Marshal(b)
		if err != nil {
			outcomes = append(outcomes, OutcomeRejected)
			continue
		}
		o, err := r.postFact(ctx, attempts, backoff, timeout, bearer, "host-fact-"+x.RunID+"-"+f.Key, body)
		outcomes = append(outcomes, o)
		if err != nil {
			lastErr = err
		}
	}
	return outcomes, lastErr
}

func (r *Remember) maxChars() int {
	if r.MaxChars > 0 {
		return r.MaxChars
	}
	return 1500
}

func (r *Remember) postFact(ctx context.Context, attempts int, backoff, timeout time.Duration, bearer, key string, body []byte) (string, error) {
	var last error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return OutcomeFailed, ctx.Err()
			case <-time.After(backoff << (i - 1)):
			}
		}
		tok, err := r.token(bearer)
		if err != nil {
			return OutcomeAuthFailed, err
		}
		code, retry, answer, err := r.post(ctx, timeout, "/v1/memory/facts", tok, key, body)
		switch {
		case err == nil && code == http.StatusCreated:
			var out struct {
				Outcome string `json:"outcome"`
			}
			if json.Unmarshal(answer, &out) == nil && out.Outcome == "updated" {
				return OutcomeFactUpdated, nil
			}
			return OutcomeFactCreated, nil
		case err == nil && code == http.StatusOK:
			return OutcomeFactUnchanged, nil
		case err == nil && code == http.StatusConflict:
			return OutcomeFactStale, nil
		case err == nil && !retry:
			return OutcomeRejected, fmt.Errorf("%w answered HTTP %d", ErrCMI, code)
		case err == nil:
			last = fmt.Errorf("%w answered HTTP %d", ErrCMI, code)
		default:
			last = err
		}
	}
	return OutcomeFailed, last
}
