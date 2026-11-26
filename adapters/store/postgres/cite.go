package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"idp-host/internal/agui"
	"idp-host/internal/run"
)

// The citation counter of a thread (ADR-0001 D70).
//
// The model reads the whole thread, so the numbers of its sources belong to the thread: one counter, in citation_counters, keyed by the
// thread of the ROOT run of a tree (a follow-up run in the thread is another root of the same thread; the sub-agent runs of each are in
// its tree). Every run that numbers takes its numbers from it. A number is taken in the same transaction that writes the answer it appears
// in: the counter's row is locked FIRST (before the run's own row), the answer is made with the number the counter holds, and the counter
// is moved by exactly the numbers the answer used. The row is locked by nothing else, the transaction waits for nothing outside the
// database, and it commits with the answer, so a number is never given without being in the thread and the counter never has a gap: every
// number below next was given to a source of the thread (which is what lets the check of the final answer ask only "was this number
// given?", D74). The lock is held to the end of that transaction and not only for the allocation because the answer and the counter must
// commit together; the bulk of the answer (the message and the events) is written after it but is a few statements.

// maxKnown bounds the memory of what the thread has numbered (reading the same address or passage again keeps its number); past it a source
// is simply numbered again. It is counted in citation_counters.known_count, not by counting rows.
const maxKnown = 5000

// rootOf is the root of the run tree that contains run id, and the thread of that root. parent_run_id never changes, so this needs no lock.
func rootOf(ctx context.Context, tx pgx.Tx, id string) (root, thread string, err error) {
	err = tx.QueryRow(ctx, `WITH RECURSIVE up AS (
		SELECT id, thread_id, parent_run_id FROM runs WHERE id = $1
		UNION ALL
		SELECT r.id, r.thread_id, r.parent_run_id FROM runs r JOIN up ON r.id = up.parent_run_id
	) SELECT id, thread_id FROM up WHERE parent_run_id IS NULL`, id).Scan(&root, &thread)
	return root, thread, err
}

// lockCounter locks the thread's counter row (creating it) and returns the number the thread's next source gets and how many sources it
// remembers.
func lockCounter(ctx context.Context, tx pgx.Tx, tenant, thread string) (next, knownCount int, err error) {
	if _, err := tx.Exec(ctx, `INSERT INTO citation_counters (tenant_id, thread_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, tenant, thread); err != nil {
		return 0, 0, err
	}
	err = tx.QueryRow(ctx, `SELECT next, known_count FROM citation_counters WHERE tenant_id = $1 AND thread_id = $2 FOR UPDATE`, tenant, thread).Scan(&next, &knownCount)
	return next, knownCount, err
}

// threadTree is what the thread has numbered, asked key by key inside the transaction (only the keys of the answer being made are read).
// A failed read is kept and reported by err: the questions have no way to return it.
type threadTree struct {
	ctx    context.Context
	tx     pgx.Tx
	tenant string
	thread string
	found  map[string]bool
	failed error
}

func (t *threadTree) tree() *run.TreeNumbers {
	return &run.TreeNumbers{Lookup: func(key string) (int, bool) {
		var n int
		err := t.tx.QueryRow(t.ctx, `SELECT n FROM citation_known WHERE tenant_id = $1 AND thread_id = $2 AND key = $3`, t.tenant, t.thread, key).Scan(&n)
		switch {
		case err == nil:
			t.found[key] = true
			return n, true
		case errors.Is(err, pgx.ErrNoRows):
			return 0, false
		}
		if t.failed == nil {
			t.failed = err
		}
		return 0, false
	}}
}

// remember writes the sources of an answer that the thread did not know (only those), up to maxKnown, and returns how many it wrote.
func (t *threadTree) remember(items []run.Source, knownCount int) (int, error) {
	add := run.KnownOf(items)
	var keys []string
	var nums []int32
	for k, n := range add {
		if !t.found[k] {
			keys, nums = append(keys, k), append(nums, int32(n))
		}
	}
	if len(keys) == 0 || knownCount+len(keys) > maxKnown {
		return 0, nil
	}
	tag, err := t.tx.Exec(t.ctx, `INSERT INTO citation_known (tenant_id, thread_id, key, n) SELECT $1, $2, k, n FROM unnest($3::text[], $4::int[]) AS u(k, n) ON CONFLICT DO NOTHING`, t.tenant, t.thread, keys, nums)
	return int(tag.RowsAffected()), err
}

// moveCounter writes the new counter and how many sources were remembered more.
func moveCounter(ctx context.Context, tx pgx.Tx, tenant, thread string, next, remembered int) error {
	_, err := tx.Exec(ctx, `UPDATE citation_counters SET next = $3, known_count = known_count + $4 WHERE tenant_id = $1 AND thread_id = $2`, tenant, thread, next, remembered)
	return err
}

// AnswerCited answers a tool call whose result the HOST numbers (D70); see harness.Store.
func (s *Store) AnswerCited(ctx context.Context, c *run.Claim, id string, cite run.CiteFunc) error {
	return s.InTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		_, thread, err := rootOf(ctx, tx, c.RunID)
		if err != nil {
			return err
		}
		first, knownCount, err := lockCounter(ctx, tx, c.TenantID, thread) // the counter's row before the run's own
		if err != nil {
			return err
		}
		r, err := s.owned(ctx, tx, c)
		if err != nil {
			return err
		}
		th := &threadTree{ctx: ctx, tx: tx, tenant: c.TenantID, thread: thread, found: map[string]bool{}}
		a, err := cite(first, th.tree())
		if err != nil {
			return err
		}
		if th.failed != nil {
			return th.failed
		}
		if a.Used < 0 {
			return errors.New("an answer cannot take a negative number of citation numbers")
		}
		var items []run.Source
		if a.Src != nil {
			items = a.Src.Items
		}
		remembered, err := th.remember(items, knownCount)
		if err != nil {
			return err
		}
		if a.Used > 0 || remembered > 0 { // nothing new: nothing is written to the counter
			if err := moveCounter(ctx, tx, c.TenantID, thread, first+a.Used, remembered); err != nil {
				return err
			}
		}
		if err := updateToolCall(ctx, tx, c, id, a.State, a.Result, a.IsError); err != nil {
			return err
		}
		if _, err := appendMessages(ctx, tx, c.TenantID, r.ThreadID, c.RunID, []agui.Message{a.Msg}); err != nil {
			return err
		}
		if err := recordSources(ctx, tx, c, a.Src); err != nil {
			return err
		}
		return appendEvents(ctx, tx, c.TenantID, c.RunID, a.Events)
	})
}

// CiteContext takes count numbers for the context entries a refresh brings in and records their sources (D70).
func (s *Store) CiteContext(ctx context.Context, c *run.Claim, count int, sources func(first int) []run.Source) (int, error) {
	var first int
	err := s.InTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		_, thread, err := rootOf(ctx, tx, c.RunID)
		if err != nil {
			return err
		}
		var knownCount int
		if first, knownCount, err = lockCounter(ctx, tx, c.TenantID, thread); err != nil {
			return err
		}
		if _, err := s.owned(ctx, tx, c); err != nil {
			return err
		}
		items := sources(first)
		th := &threadTree{ctx: ctx, tx: tx, tenant: c.TenantID, thread: thread, found: map[string]bool{}}
		remembered, err := th.remember(items, knownCount)
		if err != nil {
			return err
		}
		if err := moveCounter(ctx, tx, c.TenantID, thread, first+count, remembered); err != nil {
			return err
		}
		return recordSources(ctx, tx, c, &run.SourceBatch{Items: items})
	})
	return first, err
}

// CitationNext is the number the next source of a thread gets: 1 for a thread that numbered nothing (or does not exist yet).
func (s *Store) CitationNext(ctx context.Context, tenant, thread string) (int, error) {
	next := 1
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT next FROM citation_counters WHERE tenant_id = $1 AND thread_id = $2`, tenant, thread).Scan(&next)
		if errors.Is(err, pgx.ErrNoRows) {
			next = 1
			return nil
		}
		return err
	})
	return next, err
}

// citationsGiven is how many numbers the thread of the run's tree has given: its counter less one (read without a lock; the counter only
// grows).
func citationsGiven(ctx context.Context, tx pgx.Tx, tenant, runID string) (int, error) {
	_, thread, err := rootOf(ctx, tx, runID)
	if err != nil {
		return 0, err
	}
	next := 1
	if err := tx.QueryRow(ctx, `SELECT next FROM citation_counters WHERE tenant_id = $1 AND thread_id = $2`, tenant, thread).Scan(&next); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	return next - 1, nil
}
