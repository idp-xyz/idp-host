package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/canonical"
	"idp-host/internal/credential"
	"idp-host/internal/run"
)

// Evaluation lifecycle (ADR-0001 D26).
const (
	EvaluationRunning = "RUNNING"
	EvaluationPassed  = "PASSED"
	EvaluationFailed  = "FAILED"
)

// Evaluation is one execution of a release's evaluation set.
type Evaluation struct {
	ID         string       `json:"evaluationId"`
	ReleaseID  string       `json:"releaseId"`
	Status     string       `json:"status"`
	Total      int          `json:"total"`
	Passed     int          `json:"passed"`
	Results    []CaseResult `json:"results"`
	CreatedBy  string       `json:"createdBy"`
	CreatedAt  time.Time    `json:"createdAt"`
	FinishedAt *time.Time   `json:"finishedAt"`
}

// CaseResult is the verdict on one case.
type CaseResult struct {
	Name   string `json:"name"`
	RunID  string `json:"runId"`
	Status string `json:"status"` // RUNNING until the run ends, then the run's terminal status
	Passed bool   `json:"passed"`
	Reason string `json:"reason,omitempty"`
}

const evalCols = `id::text, release_id::text, status, total, passed, results, created_by, created_at, finished_at`

func scanEvaluation(row pgx.Row) (*Evaluation, error) {
	var e Evaluation
	var results []byte
	if err := row.Scan(&e.ID, &e.ReleaseID, &e.Status, &e.Total, &e.Passed, &results, &e.CreatedBy, &e.CreatedAt, &e.FinishedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fail("NOT_FOUND", "evaluation not found")
		}
		return nil, err
	}
	if err := json.Unmarshal(results, &e.Results); err != nil {
		return nil, err
	}
	if e.Results == nil {
		e.Results = []CaseResult{}
	}
	return &e, nil
}

// candidateEnvelope returns the provisional (unapproved) envelope of a release, sealing it on first
// use. Evaluation runs load the release through it; the digest differs from the approved one only
// by the approver, the definition is the same.
func candidateEnvelope(ctx context.Context, tx pgx.Tx, releaseID string) (*agent.Envelope, *agent.Document, error) {
	var raw *string
	var document string
	if err := tx.QueryRow(ctx, `SELECT candidate_envelope, document FROM agent_releases WHERE id = $1 FOR UPDATE`, releaseID).Scan(&raw, &document); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, fail("NOT_FOUND", "release not found")
		}
		return nil, nil, err
	}
	if raw != nil {
		var env agent.Envelope
		if err := json.Unmarshal([]byte(*raw), &env); err != nil {
			return nil, nil, err
		}
		doc, err := agent.Open(&env)
		if err != nil {
			return nil, nil, fail("GONE", "candidate envelope is corrupt: %s", err)
		}
		return &env, doc, nil
	}
	var doc agent.Document
	dec := json.NewDecoder(strings.NewReader(document))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, nil, err
	}
	doc.ApprovedBy = ""
	env, err := agent.Seal(&doc)
	if err != nil {
		return nil, nil, fail("INVALID", "%s", err)
	}
	envRaw, _ := json.Marshal(env)
	if _, err := tx.Exec(ctx, `UPDATE agent_releases SET candidate_digest = $2, candidate_envelope = $3 WHERE id = $1`, releaseID, env.Digest, string(envRaw)); err != nil {
		return nil, nil, err
	}
	return env, &doc, nil
}

// CreateEvaluation runs the release's evaluation set: one run per case on its own thread, owned by
// the evaluator, pinned to the release's provisional envelope, carrying the credential the
// evaluator's token was exchanged for. The evaluation itself is judged as the runs end.
func (s *Store) CreateEvaluation(ctx context.Context, tenant, releaseID, by string, cred *credential.Token) (*Evaluation, error) {
	if cred != nil && s.Box == nil {
		return nil, errors.New("credential key not configured")
	}
	var out *Evaluation
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		r, err := scanRelease(tx.QueryRow(ctx, `SELECT `+releaseCols+` FROM agent_releases WHERE id = $1`, releaseID))
		if err != nil {
			return err
		}
		if r.Status != ReleaseCandidate && r.Status != ReleaseApproved {
			return fail("CONFLICT", "release is %s; only candidates and approved releases are evaluated", r.Status)
		}
		env, doc, err := candidateEnvelope(ctx, tx, releaseID)
		if err != nil {
			return err
		}
		cases := doc.Definition.Evaluation.Cases
		if len(cases) == 0 {
			return fail("CONFLICT", "the release has no evaluation cases")
		}
		var sealed []byte
		var credExp any
		if cred != nil {
			if sealed, err = s.Box.Seal([]byte(cred.AccessToken)); err != nil {
				return err
			}
			credExp = cred.ExpiresAt
		}
		evalID := NewUUID()
		results := make([]CaseResult, 0, len(cases))
		resultsRaw, _ := json.Marshal(results)
		if _, err := tx.Exec(ctx, `INSERT INTO evaluations (id, tenant_id, release_id, status, total, results, created_by) VALUES ($1, $2, $3, 'RUNNING', $4, $5, $6)`,
			evalID, tenant, releaseID, len(cases), resultsRaw, by); err != nil {
			return err
		}
		deadline := time.Duration(doc.Definition.Limits.RunTimeoutSeconds) * time.Second
		for i, c := range cases {
			runID := fmt.Sprintf("eval_%s_%d", strings.ReplaceAll(evalID, "-", ""), i)
			if _, err := tx.Exec(ctx, `INSERT INTO threads (tenant_id, id, owner, agent_id) VALUES ($1, $2, $3, $4)`, tenant, runID, by, r.AgentID); err != nil {
				return err
			}
			msg := agui.Message{ID: "u_" + runID, Role: agui.RoleUser, Content: c.Input}
			input := run.Input{Tools: []agui.Tool{}, Context: []agui.Context{}}
			hash, err := canonical.Digest(map[string]any{"agent": r.AgentID, "messages": []agui.Message{msg}, "input": input, "release": releaseID, "evaluation": evalID})
			if err != nil {
				return err
			}
			inputRaw, _ := json.Marshal(input)
			if _, err := tx.Exec(ctx, `INSERT INTO runs (tenant_id, id, thread_id, agent_id, release_id, release_digest, owner, status, idempotency_key, request_sha256, input,
			credential, credential_expires_at, deadline_at, purpose, evaluation_id) VALUES ($1, $2, $2, $3, $4, $5, $6, 'QUEUED', $7, $8, $9, $10, $11, $12, 'evaluation', $13)`,
				tenant, runID, r.AgentID, releaseID, env.Digest, by, "eval:"+evalID+":"+c.Name, hash, inputRaw, sealed, credExp, s.now().Add(deadline), evalID); err != nil {
				return err
			}
			if _, err := appendMessages(ctx, tx, tenant, runID, runID, []agui.Message{msg}); err != nil {
				return err
			}
			if err := appendEvents(ctx, tx, tenant, runID, []agui.Event{agui.RunStarted(runID, runID)}); err != nil {
				return err
			}
			results = append(results, CaseResult{Name: c.Name, RunID: runID, Status: run.StatusQueued})
		}
		resultsRaw, _ = json.Marshal(results)
		if _, err := tx.Exec(ctx, `UPDATE evaluations SET results = $2 WHERE id = $1`, evalID, resultsRaw); err != nil {
			return err
		}
		if err := Audit(ctx, tx, tenant, by, "release.evaluate", releaseID, map[string]any{"evaluation": evalID, "cases": len(cases)}); err != nil {
			return err
		}
		out, err = scanEvaluation(tx.QueryRow(ctx, `SELECT `+evalCols+` FROM evaluations WHERE id = $1`, evalID))
		return err
	})
	return out, err
}

// evaluationRunEnded judges the case an evaluation run stood for and closes the evaluation when
// every case is judged. Called in the transaction that ends the run.
func evaluationRunEnded(ctx context.Context, tx pgx.Tx, r *run.Run, status, failureCode string) error {
	ev, err := scanEvaluation(tx.QueryRow(ctx, `SELECT `+evalCols+` FROM evaluations WHERE id = $1 FOR UPDATE`, r.EvaluationID))
	if err != nil {
		return err
	}
	env, doc, err := candidateEnvelope(ctx, tx, ev.ReleaseID)
	if err != nil {
		return err
	}
	_ = env
	idx := -1
	for i, cr := range ev.Results {
		if cr.RunID == r.ID {
			idx = i
		}
	}
	if idx < 0 || idx >= len(doc.Definition.Evaluation.Cases) {
		return nil // not one of this evaluation's cases (should not happen)
	}
	outcome := agent.Outcome{Status: status, FailureCode: failureCode, Steps: r.StepCount}
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT message FROM thread_messages WHERE thread_id = $1 AND message->>'role' = 'assistant' AND coalesce(message->>'content', '') <> '' ORDER BY seq DESC LIMIT 1`, r.ThreadID).Scan(&raw); err == nil {
		var m agui.Message
		if json.Unmarshal(raw, &m) == nil {
			outcome.Answer = m.Content
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT tool FROM tool_calls WHERE run_id = $1 AND state <> 'DENIED' ORDER BY tool`, r.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return err
		}
		outcome.Tools = append(outcome.Tools, t)
	}
	rows.Close()
	passed, reason := doc.Definition.Evaluation.Cases[idx].Expect.Judge(outcome)
	ev.Results[idx].Status, ev.Results[idx].Passed, ev.Results[idx].Reason = status, passed, reason
	done, count := true, 0
	for _, cr := range ev.Results {
		if !run.Terminal(cr.Status) {
			done = false
		}
		if cr.Passed {
			count++
		}
	}
	resultsRaw, _ := json.Marshal(ev.Results)
	final := EvaluationRunning
	if done {
		final = EvaluationFailed
		if count == len(ev.Results) {
			final = EvaluationPassed
		}
	}
	_, err = tx.Exec(ctx, `UPDATE evaluations SET results = $2, passed = $3, status = $4, finished_at = CASE WHEN $5 THEN now() ELSE finished_at END WHERE id = $1`, ev.ID, resultsRaw, count, final, done)
	return err
}

// GetEvaluation returns one evaluation.
func (s *Store) GetEvaluation(ctx context.Context, tenant, id string) (*Evaluation, error) {
	var out *Evaluation
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
		out, err = scanEvaluation(tx.QueryRow(ctx, `SELECT `+evalCols+` FROM evaluations WHERE id = $1`, id))
		return err
	})
	return out, err
}

// ListEvaluations returns a release's evaluations, newest first.
func (s *Store) ListEvaluations(ctx context.Context, tenant, releaseID string) ([]Evaluation, error) {
	out := []Evaluation{}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := scanRelease(tx.QueryRow(ctx, `SELECT `+releaseCols+` FROM agent_releases WHERE id = $1`, releaseID)); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+evalCols+` FROM evaluations WHERE release_id = $1 ORDER BY created_at DESC LIMIT 100`, releaseID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanEvaluation(rows)
			if err != nil {
				return err
			}
			out = append(out, *e)
		}
		return rows.Err()
	})
	return out, err
}

// evaluationGate refuses the approval of a release whose definition requires an evaluation until the
// latest evaluation passed in full.
func evaluationGate(ctx context.Context, tx pgx.Tx, releaseID string, doc *agent.Document) error {
	if !doc.Definition.Evaluation.Required {
		return nil
	}
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM evaluations WHERE release_id = $1 ORDER BY created_at DESC LIMIT 1`, releaseID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return fail("EVALUATION_REQUIRED", "the release requires an evaluation before approval; none has run")
	}
	if err != nil {
		return err
	}
	if status != EvaluationPassed {
		return fail("EVALUATION_REQUIRED", "the release's latest evaluation is %s; approval needs PASSED", status)
	}
	return nil
}
