package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"idp-host/internal/agui"
	"idp-host/internal/canonical"
	"idp-host/internal/run"
)

// RewriteUsage is what the question rewrite (D66) used: it is recorded as step 0 of the run it was made for.
type RewriteUsage struct {
	Provider, Model                string
	PromptTokens, CompletionTokens int
	LatencyMs                      int64
}

// rewriteTokens is what the rewrite's usage adds to the run's tokens_used, so that the run's own cap (MaxTokensPerRun) counts it.
func rewriteTokens(u *RewriteUsage) int64 {
	if u == nil {
		return 0
	}
	return int64(u.PromptTokens + u.CompletionTokens)
}

// ErrThreadNotOwned: the thread exists and is another caller's.
var ErrThreadNotOwned = errors.New("the thread belongs to another user")

// requestHash is the digest an idempotent replay is compared by: the request without what is resolved afresh each time - the
// CMI's items at the tail of the context (D32) and the query the rewrite made (D66).
func requestHash(agentID string, in CreateRunInput) (string, error) {
	hashSrc := map[string]any{"agent": agentID, "messages": in.Messages, "input": in.Input, "release": in.ReleaseID, "executionRef": in.ExecutionRef}
	stripped := in.Input
	stripped.ContextQuery = ""
	if n := contextItems(in.Extra); n > 0 && n <= len(in.Input.Context) {
		stripped.Context = in.Input.Context[:len(in.Input.Context)-n]
	}
	hashSrc["input"] = stripped
	return canonical.Digest(hashSrc)
}

// checkQuota is the tenant governance of D15 for a new run: concurrency and the daily token budget.
func (s *Store) checkQuota(ctx context.Context, tx pgx.Tx, policy Policy) error {
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
	return nil
}

// CheckQuota refuses with QUOTA_EXCEEDED when the tenant could not create a run now (D15). The question rewrite asks it before it
// spends a model call (D66).
func (s *Store) CheckQuota(ctx context.Context, tenant string) error {
	return s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		policy, err := policyOf(ctx, tx, tenant)
		if err != nil {
			return err
		}
		return s.checkQuota(ctx, tx, policy)
	})
}

// FindReplay returns the run an idempotent create would replay, before anything is resolved or paid for (D66): the run of this
// thread and owner with this key when the request is the same, a CONFLICT when it is not, nil when there is none. The request is
// as the API has it before the CMI's items are added.
func (s *Store) FindReplay(ctx context.Context, tenant string, in CreateRunInput) (*run.Run, error) {
	if in.IdempotencyKey == "" || in.ThreadID == "" {
		return nil, nil
	}
	var out *run.Run
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		a, err := getAgent(ctx, tx, in.AgentRef, false)
		if err != nil {
			return err
		}
		hash, err := requestHash(a.ID, in)
		if err != nil {
			return err
		}
		existing, err := scanRun(tx.QueryRow(ctx, `SELECT `+runCols+` FROM runs WHERE thread_id = $1 AND owner = $2 AND idempotency_key = $3`, in.ThreadID, in.Owner, in.IdempotencyKey))
		if err != nil {
			if se := new(Error); errors.As(err, &se) {
				return nil // none
			}
			return err
		}
		var stored string
		if err := tx.QueryRow(ctx, `SELECT request_sha256 FROM runs WHERE id = $1`, existing.ID).Scan(&stored); err != nil {
			return err
		}
		if stored != hash {
			return fail("CONFLICT", "Idempotency-Key was used for a different request")
		}
		out = existing
		return attachSources(ctx, tx, out)
	})
	return out, err
}

// ThreadMessages returns the stored messages of a thread of this owner, for the question rewrite (D66); none for a thread that does
// not exist, ErrThreadNotOwned for one that is another's: the request's messages are never taken as the history of a thread the
// caller does not own.
func (s *Store) ThreadMessages(ctx context.Context, tenant, owner, threadID string) ([]agui.Message, error) {
	if threadID == "" {
		return nil, nil
	}
	var out []agui.Message
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		var o string
		if err := tx.QueryRow(ctx, `SELECT owner FROM threads WHERE id = $1`, threadID).Scan(&o); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		if o != owner {
			return ErrThreadNotOwned
		}
		var err error
		out, err = threadMessages(ctx, tx, threadID)
		return err
	})
	return out, err
}

// FindReplayRaw is the replay check made first of all (D69): the run of this thread, owner and key whose request, as the caller sent
// it (rawHash), is the same, before the gateway is exchanged with or listed, a credential is exchanged, the question is rewritten or
// anything is retrieved. A different request under the same key is a CONFLICT. A run created before the raw digest was kept has none and is
// found later, by the digest of the whole request, as before.
func (s *Store) FindReplayRaw(ctx context.Context, tenant, threadID, owner, key, rawHash string) (*run.Run, error) {
	if threadID == "" || key == "" || rawHash == "" {
		return nil, nil
	}
	var out *run.Run
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		existing, err := scanRun(tx.QueryRow(ctx, `SELECT `+runCols+` FROM runs WHERE thread_id = $1 AND owner = $2 AND idempotency_key = $3`, threadID, owner, key))
		if err != nil {
			if se := new(Error); errors.As(err, &se) {
				return nil
			}
			return err
		}
		var stored *string
		if err := tx.QueryRow(ctx, `SELECT request_raw_sha256 FROM runs WHERE id = $1`, existing.ID).Scan(&stored); err != nil {
			return err
		}
		if stored == nil {
			return nil
		}
		if *stored != rawHash {
			return fail("CONFLICT", "Idempotency-Key was used for a different request")
		}
		out = existing
		return attachSources(ctx, tx, out)
	})
	return out, err
}

// Preflight refuses what CreateRun would refuse for the thread, before anything is paid for (D69): a thread that is another user's
// (FORBIDDEN), one bound to another agent (CONFLICT) and one with a run still in flight (CONFLICT). It writes nothing.
func (s *Store) Preflight(ctx context.Context, tenant, threadID, owner, agentRef, key string) error {
	if threadID == "" {
		return nil
	}
	return s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		a, err := getAgent(ctx, tx, agentRef, false)
		if err != nil {
			return err
		}
		var o string
		var bound *string
		if err := tx.QueryRow(ctx, `SELECT owner, agent_id::text FROM threads WHERE id = $1`, threadID).Scan(&o, &bound); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		if o != owner {
			return fail("FORBIDDEN", "thread belongs to another user")
		}
		if bound != nil && *bound != a.ID {
			return fail("CONFLICT", "thread is bound to another agent")
		}
		var active int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM runs WHERE thread_id = $1 AND status IN `+activeStatuses, threadID).Scan(&active); err != nil {
			return err
		}
		if active > 0 {
			// a run of this thread, owner and key is a replay, not a second run: it is not refused as busy here, it falls through to the
			// replay check by the digest of the whole request (a run created before the raw digest was kept has none)
			var same int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM runs WHERE thread_id = $1 AND owner = $2 AND idempotency_key = $3`, threadID, owner, key).Scan(&same); err != nil {
				return err
			}
			if same == 0 {
				return fail("CONFLICT", "a run is still in progress on this thread")
			}
		}
		return nil
	})
}
