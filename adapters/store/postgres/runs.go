package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-host/internal/agent"
	"idp-host/internal/agui"
	"idp-host/internal/canonical"
	"idp-host/internal/credential"
	"idp-host/internal/run"
)

// CreateRunInput is a run request as the API normalized it.
type CreateRunInput struct {
	ThreadID       string
	RunID          string // optional, caller-chosen
	AgentRef       string // id or name
	Owner          string
	IdempotencyKey string
	Messages       []agui.Message // the conversation as sent; messages already on the thread are skipped
	Input          run.Input
	Credential     *credential.Token // delegated gateway token, sealed before storage
	TraceID        string
	ReleaseID      string // the approved release to run; "" = the agent's current release
	ExecutionRef   string // the caller's execution reference (EER), recorded on the run
}

const runCols = `tenant_id::text, id, thread_id, agent_id::text, release_id::text, release_digest, owner, status, idempotency_key, input, step_count, event_seq,
	fencing_epoch, coalesce(lease_owner, ''), lease_until, cancel_requested, coalesce(failure_code, ''), coalesce(failure_message, ''), coalesce(execution_ref, ''),
	credential_expires_at, coalesce(trace_id, ''), deadline_at, created_at, started_at, finished_at, coalesce(parent_run_id, ''), depth`

// activeStatuses are the statuses of a run that holds its thread and a place in the tenant's concurrency.
const activeStatuses = `('QUEUED', 'RUNNING', 'AWAITING_INPUT', 'AWAITING_SUBAGENT')`

func scanRun(row pgx.Row) (*run.Run, error) {
	var r run.Run
	var input []byte
	var leaseUntil, credExp, startedAt, finishedAt *time.Time
	if err := row.Scan(&r.TenantID, &r.ID, &r.ThreadID, &r.AgentID, &r.ReleaseID, &r.ReleaseDigest, &r.Owner, &r.Status, &r.IdempotencyKey, &input, &r.StepCount, &r.EventSeq,
		&r.FencingEpoch, &r.LeaseOwner, &leaseUntil, &r.CancelRequested, &r.FailureCode, &r.FailureMessage, &r.ExecutionRef,
		&credExp, &r.TraceID, &r.DeadlineAt, &r.CreatedAt, &startedAt, &finishedAt, &r.ParentRunID, &r.Depth); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fail("NOT_FOUND", "run not found")
		}
		return nil, err
	}
	if err := json.Unmarshal(input, &r.Input); err != nil {
		return nil, err
	}
	for dst, src := range map[*time.Time]*time.Time{&r.LeaseUntil: leaseUntil, &r.CredentialExpiresAt: credExp, &r.StartedAt: startedAt, &r.FinishedAt: finishedAt} {
		if src != nil {
			*dst = *src
		}
	}
	return &r, nil
}

func appendEvents(ctx context.Context, tx pgx.Tx, tenant, runID string, events []agui.Event) error {
	if len(events) == 0 {
		return nil
	}
	var seq int
	if err := tx.QueryRow(ctx, `UPDATE runs SET event_seq = event_seq + $3 WHERE tenant_id = $1 AND id = $2 RETURNING event_seq`, tenant, runID, len(events)).Scan(&seq); err != nil {
		return err
	}
	first := seq - len(events) + 1
	b := &pgx.Batch{}
	for i, e := range events {
		raw, err := json.Marshal(e)
		if err != nil {
			return err
		}
		b.Queue(`INSERT INTO run_events (tenant_id, run_id, seq, type, payload) VALUES ($1, $2, $3, $4, $5)`, tenant, runID, first+i, e.Type(), raw)
	}
	return tx.SendBatch(ctx, b).Close()
}

// CreateRun validates the request against the agent's current release, stores the new messages
// on the thread and queues the run. A repeated request (same thread, owner and idempotency key with
// the same content) returns the existing run.
func (s *Store) CreateRun(ctx context.Context, tenant string, in CreateRunInput) (*run.Run, bool, error) {
	if in.IdempotencyKey == "" || len(in.IdempotencyKey) > 128 {
		return nil, false, fail("INVALID", "Idempotency-Key is required (at most 128 characters)")
	}
	if in.RunID != "" && !agui.ValidID(in.RunID) || in.ThreadID != "" && !agui.ValidID(in.ThreadID) {
		return nil, false, fail("INVALID", "runId and threadId must match ^[A-Za-z0-9_.:-]{1,128}$")
	}
	if in.Credential != nil && s.Box == nil {
		return nil, false, errors.New("credential key not configured")
	}
	var out *run.Run
	replayed := false
	attempt := func() error {
		return s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			a, err := getAgent(ctx, tx, in.AgentRef, false)
			if err != nil {
				return err
			}
			var releaseID string
			var env *agent.Envelope
			var doc *agent.Document
			if in.ReleaseID != "" { // a pinned release: it must be an approved release of this agent
				releaseID = in.ReleaseID
				var owner string
				if err := tx.QueryRow(ctx, `SELECT agent_id::text FROM agent_releases WHERE id = $1`, releaseID).Scan(&owner); err != nil || owner != a.ID {
					return fail("CONFLICT", "release %s is not a release of agent %s", releaseID, a.Name)
				}
				if env, err = envelopeOf(ctx, tx, releaseID); err != nil {
					return err
				}
				if doc, err = agent.Open(env); err != nil {
					return fail("GONE", "release %s is corrupt: %s", releaseID, err)
				}
			} else if releaseID, env, doc, err = currentRelease(ctx, tx, a); err != nil {
				return err
			}
			threadID := in.ThreadID
			if threadID == "" {
				threadID = "thr_" + strings.ReplaceAll(NewUUID(), "-", "")
			}
			th, err := scanThread(tx.QueryRow(ctx, `SELECT `+threadCols+` FROM threads WHERE id = $1 FOR UPDATE`, threadID))
			if err != nil {
				var se *Error
				if !errors.As(err, &se) || se.Code != "NOT_FOUND" {
					return err
				}
				if _, err := tx.Exec(ctx, `INSERT INTO threads (tenant_id, id, owner, agent_id) VALUES ($1, $2, $3, $4)`, tenant, threadID, in.Owner, a.ID); err != nil {
					if isUniqueViolation(err) {
						return errRetry // a concurrent request created the thread; the retry locks it
					}
					return err
				}
				th = &Thread{ID: threadID, Owner: in.Owner, AgentID: &a.ID}
			}
			if th.Owner != in.Owner {
				return fail("FORBIDDEN", "thread belongs to another user")
			}
			if th.AgentID != nil && *th.AgentID != a.ID {
				return fail("CONFLICT", "thread is bound to another agent")
			}
			if th.AgentID == nil {
				if _, err := tx.Exec(ctx, `UPDATE threads SET agent_id = $2 WHERE id = $1`, threadID, a.ID); err != nil {
					return err
				}
			}
			hashSrc := map[string]any{"agent": a.ID, "messages": in.Messages, "input": in.Input, "release": in.ReleaseID, "executionRef": in.ExecutionRef}
			hash, err := canonical.Digest(hashSrc)
			if err != nil {
				return err
			}
			existing, err := scanRun(tx.QueryRow(ctx, `SELECT `+runCols+` FROM runs WHERE thread_id = $1 AND owner = $2 AND idempotency_key = $3`, threadID, in.Owner, in.IdempotencyKey))
			if err == nil {
				var stored string
				if err := tx.QueryRow(ctx, `SELECT request_sha256 FROM runs WHERE id = $1`, existing.ID).Scan(&stored); err != nil {
					return err
				}
				if stored != hash {
					return fail("CONFLICT", "Idempotency-Key was used for a different request")
				}
				out, replayed = existing, true
				return nil
			} else if se := new(Error); !errors.As(err, &se) {
				return err
			}
			runID := in.RunID
			if runID == "" {
				runID = "run_" + strings.ReplaceAll(NewUUID(), "-", "")
			}
			// a run on this thread that is still in flight must finish first: one turn at a time
			var active int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM runs WHERE thread_id = $1 AND status IN `+activeStatuses, threadID).Scan(&active); err != nil {
				return err
			}
			if active > 0 {
				return fail("CONFLICT", "a run is still in progress on this thread")
			}
			// tenant governance (D15): concurrency and the daily token budget
			policy, err := policyOf(ctx, tx, tenant)
			if err != nil {
				return err
			}
			if policy.MaxConcurrentRuns > 0 {
				n, err := activeRuns(ctx, tx)
				if err != nil {
					return err
				}
				if n >= policy.MaxConcurrentRuns {
					return fail("QUOTA_EXCEEDED", "the tenant's limit of %d concurrent runs is reached", policy.MaxConcurrentRuns)
				}
			}
			if policy.TokensPerDay > 0 {
				used, err := s.tokensLastDay(ctx, tx)
				if err != nil {
					return err
				}
				if used >= policy.TokensPerDay {
					return fail("QUOTA_EXCEEDED", "the tenant's daily token budget (%d) is exhausted", policy.TokensPerDay)
				}
			}
			// AG-UI callers send the whole conversation each run: messages already on the thread are
			// skipped by id, and - for clients that re-label what the HOST produced - an assistant
			// message repeating a known tool call, or a tool message answering an already answered
			// call, is skipped too.
			history, err := threadMessages(ctx, tx, threadID)
			if err != nil {
				return err
			}
			known, knownCalls, knownResults := map[string]bool{}, map[string]bool{}, map[string]bool{}
			for _, m := range history {
				known[m.ID] = true
				for _, c := range m.ToolCalls {
					knownCalls[c.ID] = true
				}
				if m.Role == agui.RoleTool {
					knownResults[m.ToolCallID] = true
				}
			}
			var fresh []agui.Message
			for _, m := range in.Messages {
				if known[m.ID] || (m.Role == agui.RoleTool && knownResults[m.ToolCallID]) {
					continue
				}
				dup := false
				for _, c := range m.ToolCalls {
					if knownCalls[c.ID] {
						dup = true
					}
				}
				if dup {
					continue
				}
				fresh = append(fresh, m)
			}
			if len(fresh) == 0 && len(known) == 0 {
				return fail("INVALID", "the run needs at least one message")
			}
			var cred []byte
			var credExp any
			if in.Credential != nil {
				if cred, err = s.Box.Seal([]byte(in.Credential.AccessToken)); err != nil {
					return err
				}
				credExp = in.Credential.ExpiresAt
			}
			inputRaw, _ := json.Marshal(in.Input)
			deadline := s.now().Add(time.Duration(doc.Definition.Limits.RunTimeoutSeconds) * time.Second)
			var trace, execRef any
			if in.TraceID != "" {
				trace = in.TraceID
			}
			if in.ExecutionRef != "" {
				execRef = in.ExecutionRef
			}
			if _, err := tx.Exec(ctx, `INSERT INTO runs (tenant_id, id, thread_id, agent_id, release_id, release_digest, owner, status, idempotency_key, request_sha256, input,
			credential, credential_expires_at, trace_id, deadline_at, execution_ref) VALUES ($1, $2, $3, $4, $5, $6, $7, 'QUEUED', $8, $9, $10, $11, $12, $13, $14, $15)`,
				tenant, runID, threadID, a.ID, releaseID, env.Digest, in.Owner, in.IdempotencyKey, hash, inputRaw, cred, credExp, trace, deadline, execRef); err != nil {
				if isUniqueViolation(err) {
					if strings.Contains(constraintOf(err), "idempotency") {
						return errRetry // the same request raced; the retry replays it
					}
					return fail("CONFLICT", "run %s exists", runID)
				}
				return err
			}
			if _, err := appendMessages(ctx, tx, tenant, threadID, runID, fresh); err != nil {
				return err
			}
			if err := appendEvents(ctx, tx, tenant, runID, []agui.Event{agui.RunStarted(threadID, runID)}); err != nil {
				return err
			}
			out, err = scanRun(tx.QueryRow(ctx, `SELECT `+runCols+` FROM runs WHERE id = $1`, runID))
			return err
		})
	}
	var err error
	for i := 0; i < 3; i++ {
		if err = attempt(); !errors.Is(err, errRetry) {
			break
		}
	}
	return out, replayed, err
}

// GetRun returns a run of owner ("" = any owner).
func (s *Store) GetRun(ctx context.Context, tenant, owner, id string) (*run.Run, error) {
	var out *run.Run
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
		out, err = scanRun(tx.QueryRow(ctx, `SELECT `+runCols+` FROM runs WHERE id = $1 AND ($2 = '' OR owner = $2)`, id, owner))
		return err
	})
	return out, err
}

// ListRuns returns the runs of a thread, oldest first.
func (s *Store) ListRuns(ctx context.Context, tenant, owner, threadID string) ([]run.Run, error) {
	out := []run.Run{}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+runCols+` FROM runs WHERE thread_id = $1 AND ($2 = '' OR owner = $2) ORDER BY created_at LIMIT 500`, threadID, owner)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRun(rows)
			if err != nil {
				return err
			}
			out = append(out, *r)
		}
		return rows.Err()
	})
	return out, err
}

// Events returns a run's events after the given event id (0 = from the start), at most limit.
func (s *Store) Events(ctx context.Context, tenant, owner, runID string, afterID int64, limit int) ([]run.Event, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	out := []run.Event{}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := scanRun(tx.QueryRow(ctx, `SELECT `+runCols+` FROM runs WHERE id = $1 AND ($2 = '' OR owner = $2)`, runID, owner)); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id, seq, type, payload, at FROM run_events WHERE run_id = $1 AND id > $2 ORDER BY id LIMIT $3`, runID, afterID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e run.Event
			var raw []byte
			if err := rows.Scan(&e.ID, &e.Seq, &e.Type, &raw, &e.At); err != nil {
				return err
			}
			e.RunID = runID
			if err := json.Unmarshal(raw, &e.Payload); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

const callCols = `run_id, id, step, message_id, tool, arguments, arguments_sha256, state, confirmation_expires_at, coalesce(decided_by, ''), decided_at, coalesce(result, ''), is_error, created_at, finished_at, coalesce(child_run_id, '')`

func scanCall(row pgx.Row) (*run.ToolCall, error) {
	var c run.ToolCall
	var exp, decided, finished *time.Time
	if err := row.Scan(&c.RunID, &c.ID, &c.Step, &c.MessageID, &c.Tool, &c.Arguments, &c.ArgumentsSHA256, &c.State, &exp, &c.DecidedBy, &decided, &c.Result, &c.IsError, &c.CreatedAt, &finished, &c.ChildRunID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fail("NOT_FOUND", "tool call not found")
		}
		return nil, err
	}
	for dst, src := range map[*time.Time]*time.Time{&c.ConfirmationExpiresAt: exp, &c.DecidedAt: decided, &c.FinishedAt: finished} {
		if src != nil {
			*dst = *src
		}
	}
	return &c, nil
}

func runCalls(ctx context.Context, tx pgx.Tx, runID string, states []string, unresolvedOnly bool) ([]run.ToolCall, error) {
	q := `SELECT ` + callCols + ` FROM tool_calls WHERE run_id = $1`
	args := []any{runID}
	if len(states) > 0 {
		q += ` AND state = ANY($2)`
		args = append(args, states)
	}
	if unresolvedOnly {
		q += ` AND finished_at IS NULL`
	}
	rows, err := tx.Query(ctx, q+` ORDER BY step, created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []run.ToolCall{}
	for rows.Next() {
		c, err := scanCall(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// ToolCalls returns a run's tool calls.
func (s *Store) ToolCalls(ctx context.Context, tenant, owner, runID string) ([]run.ToolCall, error) {
	var out []run.ToolCall
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := scanRun(tx.QueryRow(ctx, `SELECT `+runCols+` FROM runs WHERE id = $1 AND ($2 = '' OR owner = $2)`, runID, owner)); err != nil {
			return err
		}
		var err error
		out, err = runCalls(ctx, tx, runID, nil, false)
		return err
	})
	return out, err
}

// Decisions.
const (
	DecisionConfirm = "confirm"
	DecisionReject  = "reject"
)

// DecideToolCall records the owner's decision on a pending tool call; when no call is pending any
// more the run is queued again.
func (s *Store) DecideToolCall(ctx context.Context, tenant, owner, runID, callID, decision string) (*run.ToolCall, error) {
	if decision != DecisionConfirm && decision != DecisionReject {
		return nil, fail("INVALID", "decision must be confirm or reject")
	}
	var out *run.ToolCall
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		r, err := scanRun(tx.QueryRow(ctx, `SELECT `+runCols+` FROM runs WHERE id = $1 FOR UPDATE`, runID))
		if err != nil {
			return err
		}
		if r.Owner != owner {
			return fail("FORBIDDEN", "only the run's owner can decide")
		}
		if r.Status != run.StatusAwaitingInput {
			return fail("CONFLICT", "run is %s, not awaiting input", r.Status)
		}
		c, err := scanCall(tx.QueryRow(ctx, `SELECT `+callCols+` FROM tool_calls WHERE run_id = $1 AND id = $2 FOR UPDATE`, runID, callID))
		if err != nil {
			return err
		}
		if c.State != run.CallPendingConfirmation {
			return fail("CONFLICT", "tool call is %s", c.State)
		}
		state := run.CallConfirmed
		if decision == DecisionReject {
			state = run.CallRejected
		}
		if !c.ConfirmationExpiresAt.IsZero() && !s.now().Before(c.ConfirmationExpiresAt) {
			state = run.CallExpired
		}
		if _, err := tx.Exec(ctx, `UPDATE tool_calls SET state = $3, decided_by = $4, decided_at = now() WHERE run_id = $1 AND id = $2`, runID, callID, state, owner); err != nil {
			return err
		}
		events := []agui.Event{agui.Custom(agui.CustomConfirmationDecided, map[string]any{"toolCallId": callID, "tool": c.Tool, "decision": decision, "state": state})}
		var pending int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM tool_calls WHERE run_id = $1 AND state = 'PENDING_CONFIRMATION'`, runID).Scan(&pending); err != nil {
			return err
		}
		if pending == 0 {
			if _, err := tx.Exec(ctx, `UPDATE runs SET status = 'QUEUED' WHERE id = $1`, runID); err != nil {
				return err
			}
			events = append(events, agui.RunStarted(r.ThreadID, runID))
		}
		if err := appendEvents(ctx, tx, tenant, runID, events); err != nil {
			return err
		}
		if state == run.CallExpired {
			out, _ = scanCall(tx.QueryRow(ctx, `SELECT `+callCols+` FROM tool_calls WHERE run_id = $1 AND id = $2`, runID, callID))
			return fail("CONFLICT", "confirmation expired")
		}
		out, err = scanCall(tx.QueryRow(ctx, `SELECT `+callCols+` FROM tool_calls WHERE run_id = $1 AND id = $2`, runID, callID))
		return err
	})
	return out, err
}

// CancelRun cancels a queued or waiting run at once and asks a running one to stop.
func (s *Store) CancelRun(ctx context.Context, tenant, owner, runID string) (*run.Run, error) {
	var out *run.Run
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		r, err := scanRun(tx.QueryRow(ctx, `SELECT `+runCols+` FROM runs WHERE id = $1 FOR UPDATE`, runID))
		if err != nil {
			return err
		}
		if owner != "" && r.Owner != owner {
			return fail("FORBIDDEN", "only the run's owner can cancel")
		}
		switch r.Status {
		case run.StatusCompleted, run.StatusFailed, run.StatusCanceled:
		case run.StatusRunning:
			if !r.CancelRequested {
				if _, err := tx.Exec(ctx, `UPDATE runs SET cancel_requested = true WHERE id = $1`, runID); err != nil {
					return err
				}
				if err := appendEvents(ctx, tx, tenant, runID, []agui.Event{agui.Custom("idp.host/cancel.requested", map[string]any{"runId": runID})}); err != nil {
					return err
				}
			}
		default: // QUEUED, AWAITING_INPUT, AWAITING_SUBAGENT
			if err := s.cancelResting(ctx, tx, tenant, r, "canceled by the owner"); err != nil {
				return err
			}
		}
		out, err = scanRun(tx.QueryRow(ctx, `SELECT `+runCols+` FROM runs WHERE id = $1`, runID))
		return err
	})
	return out, err
}

// cancelResting cancels a run that no worker holds, cascades to its active sub-agent runs (a running
// child is asked to stop) and tells its own parent, if any, that it ended.
func (s *Store) cancelResting(ctx context.Context, tx pgx.Tx, tenant string, r *run.Run, reason string) error {
	if _, err := tx.Exec(ctx, `UPDATE tool_calls SET state = 'REJECTED', decided_by = $2, decided_at = now() WHERE run_id = $1 AND state = 'PENDING_CONFIRMATION'`, r.ID, "cancel"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET status = 'CANCELED', failure_code = 'CANCELED', failure_message = $2, finished_at = now(), lease_owner = NULL, lease_until = NULL, credential = NULL WHERE id = $1`, r.ID, reason); err != nil {
		return err
	}
	if err := appendEvents(ctx, tx, tenant, r.ID, []agui.Event{agui.RunError(reason, "CANCELED")}); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT `+runCols+` FROM runs WHERE parent_run_id = $1 AND status IN `+activeStatuses+` ORDER BY created_at FOR UPDATE`, r.ID)
	if err != nil {
		return err
	}
	var children []*run.Run
	for rows.Next() {
		c, err := scanRun(rows)
		if err != nil {
			rows.Close()
			return err
		}
		children = append(children, c)
	}
	rows.Close()
	for _, c := range children {
		if c.Status == run.StatusRunning {
			if !c.CancelRequested {
				if _, err := tx.Exec(ctx, `UPDATE runs SET cancel_requested = true WHERE id = $1`, c.ID); err != nil {
					return err
				}
				if err := appendEvents(ctx, tx, tenant, c.ID, []agui.Event{agui.Custom("idp.host/cancel.requested", map[string]any{"runId": c.ID, "parentRunId": r.ID})}); err != nil {
					return err
				}
			}
			continue
		}
		if err := s.cancelResting(ctx, tx, tenant, c, "the parent run was canceled"); err != nil {
			return err
		}
	}
	return childEnded(ctx, tx, tenant, r, run.StatusCanceled, "CANCELED")
}

// childEnded is called in the transaction that ends a run with a parent: it tells the parent's event
// stream and, when no child of the parent is active any more, queues the parent again.
func childEnded(ctx context.Context, tx pgx.Tx, tenant string, child *run.Run, status, failureCode string) error {
	if child.ParentRunID == "" {
		return nil
	}
	parent, err := scanRun(tx.QueryRow(ctx, `SELECT `+runCols+` FROM runs WHERE id = $1 FOR UPDATE`, child.ParentRunID))
	if err != nil {
		return err
	}
	if run.Terminal(parent.Status) {
		return nil // the parent ended first (canceled); nothing listens
	}
	if err := appendEvents(ctx, tx, tenant, parent.ID, []agui.Event{agui.Custom(agui.CustomSubagentFinished, map[string]any{"runId": child.ID, "status": status, "failureCode": failureCode})}); err != nil {
		return err
	}
	if parent.Status != run.StatusAwaitingSubagent {
		return nil // still running (it will see the child when it rests) or waiting for its owner
	}
	var active int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM runs WHERE parent_run_id = $1 AND status IN `+activeStatuses, parent.ID).Scan(&active); err != nil {
		return err
	}
	if active == 0 {
		_, err = tx.Exec(ctx, `UPDATE runs SET status = 'QUEUED' WHERE id = $1`, parent.ID)
	}
	return err
}

// ---- worker side ----

// Claim leases the oldest claimable run (queued, or running with an expired lease) for workerID.
// It returns nil when there is nothing to do.
func (s *Store) Claim(ctx context.Context, workerID string, lease time.Duration) (*run.Claim, error) {
	var c *run.Claim
	err := s.inWorker(ctx, func(tx pgx.Tx) error {
		var tenant, id string
		var epoch int64
		err := tx.QueryRow(ctx, `WITH cand AS (
			SELECT tenant_id, id FROM runs
			WHERE status = 'QUEUED' OR (status = 'RUNNING' AND lease_until <= now())
			ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED)
			UPDATE runs r SET status = 'RUNNING', fencing_epoch = r.fencing_epoch + 1, lease_owner = $1, lease_until = now() + $2::interval,
			  started_at = coalesce(r.started_at, now())
			FROM cand WHERE r.tenant_id = cand.tenant_id AND r.id = cand.id
			RETURNING r.tenant_id::text, r.id, r.fencing_epoch`, workerID, lease.String()).Scan(&tenant, &id, &epoch)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		c = &run.Claim{TenantID: tenant, RunID: id, WorkerID: workerID, Epoch: epoch}
		return nil
	})
	return c, err
}

// owned locks the claimed run and verifies the claim is still current.
func (s *Store) owned(ctx context.Context, tx pgx.Tx, c *run.Claim) (*run.Run, error) {
	r, err := scanRun(tx.QueryRow(ctx, `SELECT `+runCols+` FROM runs WHERE id = $1 FOR UPDATE`, c.RunID))
	if err != nil {
		return nil, err
	}
	if r.Status != run.StatusRunning || r.FencingEpoch != c.Epoch || r.LeaseOwner != c.WorkerID || !r.LeaseUntil.After(s.now()) {
		return nil, run.ErrFenced
	}
	return r, nil
}

// Heartbeat extends the lease; ErrFenced if the claim is no longer valid.
func (s *Store) Heartbeat(ctx context.Context, c *run.Claim, lease time.Duration) error {
	return s.InTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		if _, err := s.owned(ctx, tx, c); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE runs SET lease_until = now() + $2::interval WHERE id = $1`, c.RunID, lease.String())
		return err
	})
}

// Context loads the run, its release, the thread and the unresolved tool calls.
func (s *Store) Context(ctx context.Context, c *run.Claim) (*run.Context, error) {
	var out run.Context
	err := s.InTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		r, err := s.owned(ctx, tx, c)
		if err != nil {
			return err
		}
		out.Run = *r
		env, err := envelopeOf(ctx, tx, r.ReleaseID)
		if err != nil {
			return err
		}
		if out.Release, err = agent.Open(env); err != nil {
			return fail("GONE", "release is corrupt: %s", err)
		}
		if out.Release.ReleaseID != r.ReleaseID || env.Digest != r.ReleaseDigest {
			return fail("GONE", "release digest changed")
		}
		if out.Messages, err = threadMessages(ctx, tx, r.ThreadID); err != nil {
			return err
		}
		out.ToolCalls, err = runCalls(ctx, tx, c.RunID, []string{run.CallPendingConfirmation, run.CallConfirmed, run.CallRejected, run.CallExpired, run.CallExecuting, run.CallDelegated}, true)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// AppendEvents appends events under the claim.
func (s *Store) AppendEvents(ctx context.Context, c *run.Claim, events []agui.Event) error {
	return s.InTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		if _, err := s.owned(ctx, tx, c); err != nil {
			return err
		}
		return appendEvents(ctx, tx, c.TenantID, c.RunID, events)
	})
}

// AppendMessages stores messages produced by the run on its thread, with events, atomically.
func (s *Store) AppendMessages(ctx context.Context, c *run.Claim, msgs []agui.Message, events []agui.Event) error {
	return s.InTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		r, err := s.owned(ctx, tx, c)
		if err != nil {
			return err
		}
		if _, err := appendMessages(ctx, tx, c.TenantID, r.ThreadID, c.RunID, msgs); err != nil {
			return err
		}
		return appendEvents(ctx, tx, c.TenantID, c.RunID, events)
	})
}

// StartStep increments the step counter and records the step's start, emitting STEP_STARTED. It
// returns run.ErrStepLimit when the release's limit is reached, and whether a cancel is pending.
func (s *Store) StartStep(ctx context.Context, c *run.Claim, maxSteps int, maxTokensPerRun int64, provider, model string) (step int, cancelRequested bool, err error) {
	err = s.InTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		r, err := s.owned(ctx, tx, c)
		if err != nil {
			return err
		}
		cancelRequested = r.CancelRequested
		if r.StepCount >= maxSteps {
			return run.ErrStepLimit
		}
		// budgets (D15): the tenant's daily tokens and the run's own cap, checked before every model call
		policy, err := policyOf(ctx, tx, c.TenantID)
		if err != nil {
			return err
		}
		if policy.TokensPerDay > 0 {
			used, err := s.tokensLastDay(ctx, tx)
			if err != nil {
				return err
			}
			if used >= policy.TokensPerDay {
				return &run.Failure{Code: run.FailBudget, Message: "the tenant's daily token budget is exhausted"}
			}
		}
		cap := policy.MaxTokensPerRun
		if maxTokensPerRun > 0 {
			cap = maxTokensPerRun
		}
		if cap > 0 {
			var used int64
			if err := tx.QueryRow(ctx, `SELECT tokens_used FROM runs WHERE id = $1`, c.RunID).Scan(&used); err != nil {
				return err
			}
			if used >= cap {
				return &run.Failure{Code: run.FailBudget, Message: "the run's token cap is reached"}
			}
		}
		if err := tx.QueryRow(ctx, `UPDATE runs SET step_count = step_count + 1 WHERE id = $1 RETURNING step_count`, c.RunID).Scan(&step); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO run_steps (tenant_id, run_id, step, provider, model) VALUES ($1, $2, $3, $4, $5)`, c.TenantID, c.RunID, step, provider, model); err != nil {
			return err
		}
		return appendEvents(ctx, tx, c.TenantID, c.RunID, []agui.Event{agui.StepStarted(run.StepName(step))})
	})
	return step, cancelRequested, err
}

// FinishStep records the model call's outcome.
func (s *Store) FinishStep(ctx context.Context, c *run.Claim, st run.Step, events []agui.Event) error {
	return s.InTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		if _, err := s.owned(ctx, tx, c); err != nil {
			return err
		}
		var stop, code any
		if st.StopReason != "" {
			stop = st.StopReason
		}
		if st.ErrorCode != "" {
			code = st.ErrorCode
		}
		if _, err := tx.Exec(ctx, `UPDATE run_steps SET provider = $3, model = $4, prompt_tokens = $5, completion_tokens = $6, latency_ms = $7, stop_reason = $8, error_code = $9, finished_at = now()
			WHERE run_id = $1 AND step = $2`, c.RunID, st.Step, st.Provider, st.Model, st.PromptTokens, st.CompletionTokens, st.Latency.Milliseconds(), stop, code); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE runs SET tokens_used = tokens_used + $2 WHERE id = $1`, c.RunID, int64(st.PromptTokens+st.CompletionTokens)); err != nil {
			return err
		}
		return appendEvents(ctx, tx, c.TenantID, c.RunID, events)
	})
}

// RecordToolCalls stores the tool calls of a step in their initial states.
func (s *Store) RecordToolCalls(ctx context.Context, c *run.Claim, calls []run.ToolCall, events []agui.Event) error {
	return s.InTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		if _, err := s.owned(ctx, tx, c); err != nil {
			return err
		}
		for _, tc := range calls {
			var exp any
			if !tc.ConfirmationExpiresAt.IsZero() {
				exp = tc.ConfirmationExpiresAt
			}
			var child any
			if tc.ChildRunID != "" {
				child = tc.ChildRunID
			}
			if _, err := tx.Exec(ctx, `INSERT INTO tool_calls (tenant_id, run_id, id, step, message_id, tool, arguments, arguments_sha256, state, confirmation_expires_at, child_run_id)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`, c.TenantID, c.RunID, tc.ID, tc.Step, tc.MessageID, tc.Tool, tc.Arguments, tc.ArgumentsSHA256, tc.State, exp, child); err != nil {
				if isUniqueViolation(err) {
					return fail("CONFLICT", "tool call id %s repeated", tc.ID)
				}
				return err
			}
		}
		return appendEvents(ctx, tx, c.TenantID, c.RunID, events)
	})
}

// UpdateToolCall sets a call's state and result.
func (s *Store) UpdateToolCall(ctx context.Context, c *run.Claim, id, state, result string, isError bool, events []agui.Event) error {
	return s.InTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		if _, err := s.owned(ctx, tx, c); err != nil {
			return err
		}
		finished := state == run.CallDone || state == run.CallFailed || state == run.CallDenied || state == run.CallRejected || state == run.CallExpired
		var res any
		if result != "" {
			res = result
		}
		if _, err := tx.Exec(ctx, `UPDATE tool_calls SET state = $3, result = $4, is_error = $5, finished_at = CASE WHEN $6 THEN now() ELSE finished_at END WHERE run_id = $1 AND id = $2`,
			c.RunID, id, state, res, isError, finished); err != nil {
			return err
		}
		return appendEvents(ctx, tx, c.TenantID, c.RunID, events)
	})
}

// Finish ends the worker's turn: a terminal status, or AWAITING_INPUT. The lease is released and
// the final events are appended in the same transaction.
func (s *Store) Finish(ctx context.Context, c *run.Claim, status, failureCode, failureMessage string, events []agui.Event) error {
	return s.InTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		r, err := s.owned(ctx, tx, c)
		if err != nil {
			return err
		}
		if status == run.StatusAwaitingSubagent {
			// a child may have ended while this turn was still running: then there is nothing to wait for
			var active int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM runs WHERE parent_run_id = $1 AND status IN `+activeStatuses, c.RunID).Scan(&active); err != nil {
				return err
			}
			if active == 0 {
				status = run.StatusQueued
			}
		}
		var code, msg any
		if failureCode != "" {
			code = failureCode
		}
		if failureMessage != "" {
			msg = failureMessage
		}
		finished := run.Terminal(status)
		if _, err := tx.Exec(ctx, `UPDATE runs SET status = $2, failure_code = $3, failure_message = $4, lease_owner = NULL, lease_until = NULL,
			finished_at = CASE WHEN $5 THEN now() ELSE finished_at END WHERE id = $1`, c.RunID, status, code, msg, finished); err != nil {
			return err
		}
		if finished {
			if _, err := tx.Exec(ctx, `UPDATE runs SET credential = NULL WHERE id = $1`, c.RunID); err != nil {
				return err
			}
		}
		if err := appendEvents(ctx, tx, c.TenantID, c.RunID, events); err != nil {
			return err
		}
		if r.ParentRunID != "" {
			if finished {
				return childEnded(ctx, tx, c.TenantID, r, status, failureCode)
			}
			if status == run.StatusAwaitingInput {
				return appendEvents(ctx, tx, c.TenantID, r.ParentRunID, []agui.Event{agui.Custom(agui.CustomSubagentAwaitingInput, map[string]any{"runId": r.ID})})
			}
		}
		return nil
	})
}

// CreateChildRun creates the sub-agent run a DELEGATED tool call of the claimed run points at (D22):
// a new thread named after the child run, the parent's owner, the pinned release of the other agent,
// the parent's context, executionRef and trace, and - when both use the same gateway - the parent's
// credential. The call is marked DELEGATED in the same transaction. The child's id is derived from
// the parent run and the tool call, so a retried turn finds the child it already created.
func (s *Store) CreateChildRun(ctx context.Context, c *run.Claim, in run.ChildRunInput) (*run.Run, error) {
	var out *run.Run
	err := s.InTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		parent, err := s.owned(ctx, tx, c)
		if err != nil {
			return err
		}
		childID := run.ChildRunID(parent.ID, in.ToolCallID)
		existing, err := scanRun(tx.QueryRow(ctx, `SELECT `+runCols+` FROM runs WHERE id = $1`, childID))
		if err == nil {
			out = existing
		} else if se := new(Error); !errors.As(err, &se) {
			return err
		} else {
			ag, err := getAgent(ctx, tx, in.Agent.AgentID, false)
			if err != nil {
				return fail("CONFLICT", "sub-agent %s no longer exists", in.Agent.Name)
			}
			var releaseID string
			if err := tx.QueryRow(ctx, `SELECT id::text FROM agent_releases WHERE agent_id = $1 AND digest = $2 AND status IN ('APPROVED', 'SUPERSEDED')`, ag.ID, in.Agent.Digest).Scan(&releaseID); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return fail("CONFLICT", "sub-agent %s has no approved release with digest %s", in.Agent.Name, in.Agent.Digest)
				}
				return err
			}
			env, err := envelopeOf(ctx, tx, releaseID)
			if err != nil {
				return err
			}
			doc, err := agent.Open(env)
			if err != nil {
				return fail("GONE", "release of sub-agent %s is corrupt: %s", in.Agent.Name, err)
			}
			policy, err := policyOf(ctx, tx, c.TenantID)
			if err != nil {
				return err
			}
			if policy.MaxConcurrentRuns > 0 {
				n, err := activeRuns(ctx, tx)
				if err != nil {
					return err
				}
				if n >= policy.MaxConcurrentRuns {
					return fail("QUOTA_EXCEEDED", "the tenant's limit of %d concurrent runs is reached", policy.MaxConcurrentRuns)
				}
			}
			if policy.TokensPerDay > 0 {
				used, err := s.tokensLastDay(ctx, tx)
				if err != nil {
					return err
				}
				if used >= policy.TokensPerDay {
					return fail("QUOTA_EXCEEDED", "the tenant's daily token budget (%d) is exhausted", policy.TokensPerDay)
				}
			}
			if _, err := tx.Exec(ctx, `INSERT INTO threads (tenant_id, id, owner, agent_id) VALUES ($1, $2, $3, $4)`, c.TenantID, childID, parent.Owner, ag.ID); err != nil {
				return err
			}
			var cred []byte
			var credExp *time.Time
			if eff := doc.Effective(); in.Gateway != "" && eff.Tools.Gateway == in.Gateway && len(eff.Tools.Allow) > 0 {
				if err := tx.QueryRow(ctx, `SELECT credential, credential_expires_at FROM runs WHERE id = $1`, parent.ID).Scan(&cred, &credExp); err != nil {
					return err
				}
			}
			input := run.Input{Tools: []agui.Tool{}, Context: parent.Input.Context, MaxOutputTokens: parent.Input.MaxOutputTokens}
			if input.Context == nil {
				input.Context = []agui.Context{}
			}
			msg := agui.Message{ID: "u_" + childID, Role: agui.RoleUser, Content: in.Input}
			hash, err := canonical.Digest(map[string]any{"agent": ag.ID, "messages": []agui.Message{msg}, "input": input, "release": releaseID, "executionRef": parent.ExecutionRef})
			if err != nil {
				return err
			}
			inputRaw, _ := json.Marshal(input)
			deadline := s.now().Add(time.Duration(doc.Definition.Limits.RunTimeoutSeconds) * time.Second)
			var trace, execRef, exp any
			if parent.TraceID != "" {
				trace = parent.TraceID
			}
			if parent.ExecutionRef != "" {
				execRef = parent.ExecutionRef
			}
			if credExp != nil {
				exp = *credExp
			}
			if _, err := tx.Exec(ctx, `INSERT INTO runs (tenant_id, id, thread_id, agent_id, release_id, release_digest, owner, status, idempotency_key, request_sha256, input,
			credential, credential_expires_at, trace_id, deadline_at, execution_ref, parent_run_id, depth) VALUES ($1, $2, $2, $3, $4, $5, $6, 'QUEUED', $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
				c.TenantID, childID, ag.ID, releaseID, env.Digest, parent.Owner, "sub:"+in.ToolCallID, hash, inputRaw, cred, exp, trace, deadline, execRef, parent.ID, parent.Depth+1); err != nil {
				return err
			}
			if _, err := appendMessages(ctx, tx, c.TenantID, childID, childID, []agui.Message{msg}); err != nil {
				return err
			}
			if err := appendEvents(ctx, tx, c.TenantID, childID, []agui.Event{agui.RunStarted(childID, childID)}); err != nil {
				return err
			}
			if out, err = scanRun(tx.QueryRow(ctx, `SELECT `+runCols+` FROM runs WHERE id = $1`, childID)); err != nil {
				return err
			}
		}
		tag, err := tx.Exec(ctx, `UPDATE tool_calls SET state = 'DELEGATED', child_run_id = $3 WHERE run_id = $1 AND id = $2 AND state <> 'DELEGATED'`, c.RunID, in.ToolCallID, childID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil // already delegated by an earlier attempt of this turn
		}
		return appendEvents(ctx, tx, c.TenantID, c.RunID, []agui.Event{agui.Custom(agui.CustomSubagentStarted, map[string]any{"toolCallId": in.ToolCallID, "agent": in.Agent.Name, "runId": childID, "threadId": childID})})
	})
	if se := new(Error); errors.As(err, &se) {
		return nil, &run.Refusal{Code: se.Code, Message: se.Message}
	}
	return out, err
}

// ChildOutcome reports the state of a sub-agent run of the claimed run and, when it ended, its last
// assistant message.
func (s *Store) ChildOutcome(ctx context.Context, c *run.Claim, childID string) (*run.ChildOutcome, error) {
	var out run.ChildOutcome
	err := s.InTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		if _, err := s.owned(ctx, tx, c); err != nil {
			return err
		}
		child, err := scanRun(tx.QueryRow(ctx, `SELECT `+runCols+` FROM runs WHERE id = $1 AND parent_run_id = $2`, childID, c.RunID))
		if err != nil {
			return err
		}
		out = run.ChildOutcome{RunID: child.ID, Status: child.Status, FailureCode: child.FailureCode, FailureMessage: child.FailureMessage}
		if !run.Terminal(child.Status) {
			return nil
		}
		var raw []byte
		err = tx.QueryRow(ctx, `SELECT message FROM thread_messages WHERE thread_id = $1 AND message->>'role' = 'assistant' AND coalesce(message->>'content', '') <> '' ORDER BY seq DESC LIMIT 1`, child.ThreadID).Scan(&raw)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var m agui.Message
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		out.Output = m.Content
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ListChildRuns returns the sub-agent runs of a run, oldest first.
func (s *Store) ListChildRuns(ctx context.Context, tenant, owner, parentID string) ([]run.Run, error) {
	out := []run.Run{}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := scanRun(tx.QueryRow(ctx, `SELECT `+runCols+` FROM runs WHERE id = $1 AND ($2 = '' OR owner = $2)`, parentID, owner)); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+runCols+` FROM runs WHERE parent_run_id = $1 ORDER BY created_at LIMIT 500`, parentID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRun(rows)
			if err != nil {
				return err
			}
			out = append(out, *r)
		}
		return rows.Err()
	})
	return out, err
}

// Credential returns the run's decrypted gateway token.
func (s *Store) Credential(ctx context.Context, c *run.Claim) (string, error) {
	var tok string
	err := s.InTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		r, err := s.owned(ctx, tx, c)
		if err != nil {
			return err
		}
		var sealed []byte
		if err := tx.QueryRow(ctx, `SELECT credential FROM runs WHERE id = $1`, c.RunID).Scan(&sealed); err != nil {
			return err
		}
		if sealed == nil {
			return &run.Failure{Code: run.FailCredential, Message: "the run carries no gateway credential"}
		}
		if !r.CredentialExpiresAt.IsZero() && !s.now().Before(r.CredentialExpiresAt) {
			return &run.Failure{Code: run.FailCredential, Message: "the delegated credential expired at " + r.CredentialExpiresAt.UTC().Format(time.RFC3339)}
		}
		if s.Box == nil {
			return errors.New("credential key not configured")
		}
		plain, err := s.Box.Open(sealed)
		if err != nil {
			return &run.Failure{Code: run.FailCredential, Message: "the credential cannot be decrypted"}
		}
		tok = string(plain)
		return nil
	})
	return tok, err
}

// ExpireConfirmations turns pending confirmations past their expiry into EXPIRED and queues the
// runs that no longer wait; it returns how many runs were queued.
func (s *Store) ExpireConfirmations(ctx context.Context) (int, error) {
	type ref struct{ tenant, id string }
	var waiting []ref
	err := s.inWorker(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT tenant_id::text, id FROM runs WHERE status = 'AWAITING_INPUT' ORDER BY created_at LIMIT 200`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r ref
			if err := rows.Scan(&r.tenant, &r.id); err != nil {
				return err
			}
			waiting = append(waiting, r)
		}
		return rows.Err()
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, w := range waiting {
		err := s.InTenant(ctx, w.tenant, func(tx pgx.Tx) error {
			r, err := scanRun(tx.QueryRow(ctx, `SELECT `+runCols+` FROM runs WHERE id = $1 FOR UPDATE`, w.id))
			if err != nil || r.Status != run.StatusAwaitingInput {
				return err
			}
			tag, err := tx.Exec(ctx, `UPDATE tool_calls SET state = 'EXPIRED', decided_at = now() WHERE run_id = $1 AND state = 'PENDING_CONFIRMATION' AND confirmation_expires_at <= $2`, w.id, s.now())
			if err != nil || tag.RowsAffected() == 0 {
				return err
			}
			var pending int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM tool_calls WHERE run_id = $1 AND state = 'PENDING_CONFIRMATION'`, w.id).Scan(&pending); err != nil {
				return err
			}
			events := []agui.Event{agui.Custom(agui.CustomConfirmationDecided, map[string]any{"decision": "expired", "expired": tag.RowsAffected()})}
			if pending == 0 {
				if _, err := tx.Exec(ctx, `UPDATE runs SET status = 'QUEUED' WHERE id = $1`, w.id); err != nil {
					return err
				}
				events = append(events, agui.RunStarted(r.ThreadID, w.id))
				n++
			}
			return appendEvents(ctx, tx, w.tenant, w.id, events)
		})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
