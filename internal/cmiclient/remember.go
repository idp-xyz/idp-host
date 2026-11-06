package cmiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"idp-host/internal/credential"
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
		Content:     "User: " + Cut(x.Message, n) + "\nAssistant: " + Cut(x.Answer, n),
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
	var bearer string // delegation: exchanged once; static: read on every attempt
	if r.Exchanger != nil {
		if subjectToken == nil {
			return OutcomeNoToken, errors.New("no subject token to exchange")
		}
		subj, err := subjectToken(ctx)
		if err != nil || subj == "" {
			return OutcomeNoToken, errors.New("no subject token to exchange")
		}
		actor := ""
		if r.ActorToken {
			t, err := r.Exchanger.ClientCredentials(ctx)
			if err != nil {
				return OutcomeAuthFailed, fmt.Errorf("actor token: %w", err)
			}
			actor = t.AccessToken
		}
		t, err := r.Exchanger.Exchange(ctx, subj, actor)
		if err != nil {
			return OutcomeAuthFailed, fmt.Errorf("token exchange: %w", err)
		}
		bearer = t.AccessToken
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
		code, retry, err := r.post(ctx, timeout, b, IdempotencyKey(x.RunID), body)
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
func (r *Remember) post(ctx context.Context, timeout time.Duration, bearer, key string, body []byte) (int, bool, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	hr, err := http.NewRequestWithContext(cctx, http.MethodPost, r.Client.Endpoint+"/v1/memory/records", bytes.NewReader(body))
	if err != nil {
		return 0, false, fmt.Errorf("%w: %v", ErrCMI, err)
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "application/json")
	hr.Header.Set("Authorization", "Bearer "+bearer)
	hr.Header.Set("Idempotency-Key", key)
	resp, err := r.Client.HTTP.Do(hr)
	if err != nil {
		return 0, true, fmt.Errorf("%w unreachable: %v", ErrCMI, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	c := resp.StatusCode
	return c, c >= 500 || c == http.StatusTooManyRequests, nil
}
