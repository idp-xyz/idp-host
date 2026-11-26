package postgres

import (
	"context"
	"encoding/json"
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
// is simply numbered again.
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

// lockCounter locks the thread's counter row (creating it) and returns the number the thread's next source gets and what it has numbered.
func lockCounter(ctx context.Context, tx pgx.Tx, tenant, thread string) (int, map[string]int, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO citation_counters (tenant_id, thread_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, tenant, thread); err != nil {
		return 0, nil, err
	}
	var next int
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT next, known FROM citation_counters WHERE tenant_id = $1 AND thread_id = $2 FOR UPDATE`, tenant, thread).Scan(&next, &raw); err != nil {
		return 0, nil, err
	}
	known := map[string]int{}
	if err := json.Unmarshal(raw, &known); err != nil {
		return 0, nil, err
	}
	return next, known, nil
}

// moveCounter writes the new counter and what the answer's sources add to the known numbers.
func moveCounter(ctx context.Context, tx pgx.Tx, tenant, thread string, next int, known map[string]int, items []run.Source) error {
	add := run.KnownOf(items)
	if len(known)+len(add) > maxKnown {
		add = map[string]int{}
	}
	raw, _ := json.Marshal(add)
	_, err := tx.Exec(ctx, `UPDATE citation_counters SET next = $3, known = known || $4::jsonb WHERE tenant_id = $1 AND thread_id = $2`, tenant, thread, next, raw)
	return err
}

// AnswerCited answers a tool call whose result the HOST numbers (D70); see harness.Store.
func (s *Store) AnswerCited(ctx context.Context, c *run.Claim, id string, cite run.CiteFunc) error {
	return s.InTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		_, thread, err := rootOf(ctx, tx, c.RunID)
		if err != nil {
			return err
		}
		first, known, err := lockCounter(ctx, tx, c.TenantID, thread) // the counter's row before the run's own
		if err != nil {
			return err
		}
		r, err := s.owned(ctx, tx, c)
		if err != nil {
			return err
		}
		a, err := cite(first, &run.TreeNumbers{Known: known})
		if err != nil {
			return err
		}
		if a.Used < 0 {
			return errors.New("an answer cannot take a negative number of citation numbers")
		}
		var items []run.Source
		if a.Src != nil {
			items = a.Src.Items
		}
		if a.Used > 0 {
			if err := moveCounter(ctx, tx, c.TenantID, thread, first+a.Used, known, items); err != nil {
				return err
			}
		} else if len(run.KnownOf(items)) > 0 {
			if err := moveCounter(ctx, tx, c.TenantID, thread, first, known, items); err != nil {
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
		var known map[string]int
		if first, known, err = lockCounter(ctx, tx, c.TenantID, thread); err != nil {
			return err
		}
		if _, err := s.owned(ctx, tx, c); err != nil {
			return err
		}
		items := sources(first)
		if err := moveCounter(ctx, tx, c.TenantID, thread, first+count, known, nil); err != nil {
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
