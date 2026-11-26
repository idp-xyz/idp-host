package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"idp-host/internal/agui"
	"idp-host/internal/run"
)

// The citation counter of a run tree (ADR-0001 D70).
//
// One counter belongs to the ROOT run, in runs.citation_next on its row, and every run of the tree (its sub-agents, theirs) takes
// the numbers of its sources from it. A number is taken in the same transaction that writes the answer it appears in: the root's
// row is locked FIRST (before the run's own row, the order of D69: ancestors before descendants), the answer is made with the number
// the counter holds, and the counter is moved by exactly the numbers the answer used. The transaction waits for nothing outside the
// database, so the lock is held for a few statements. Because the allocation commits or rolls back with the answer, a number is
// never given without being in the thread, and the counter never has a gap: every number below citation_next was given to a source
// of the tree (which is what lets the check of the final answer ask only "was this number given?", D74).

// rootOf is the root of the run tree that contains run id. parent_run_id never changes, so this needs no lock.
func rootOf(ctx context.Context, tx pgx.Tx, id string) (string, error) {
	var root string
	err := tx.QueryRow(ctx, `WITH RECURSIVE up AS (
		SELECT id, parent_run_id FROM runs WHERE id = $1
		UNION ALL
		SELECT r.id, r.parent_run_id FROM runs r JOIN up ON r.id = up.parent_run_id
	) SELECT id FROM up WHERE parent_run_id IS NULL`, id).Scan(&root)
	return root, err
}

// lockCounter locks the root's row and returns the number the tree's next source gets.
func lockCounter(ctx context.Context, tx pgx.Tx, root string) (int, error) {
	var next int
	err := tx.QueryRow(ctx, `SELECT citation_next FROM runs WHERE id = $1 FOR UPDATE`, root).Scan(&next)
	return next, err
}

// treeNumbers reads what the tree has numbered that a later result may share (D71): the web sources of every run of it, by idp-web's
// number.
func treeNumbers(ctx context.Context, tx pgx.Tx, tenant, root string) (*run.TreeNumbers, error) {
	rows, err := tx.Query(ctx, `WITH RECURSIVE t AS (
		SELECT id FROM runs WHERE id = $1
		UNION ALL
		SELECT r.id FROM runs r JOIN t ON r.parent_run_id = t.id
	) SELECT doc FROM run_sources WHERE tenant_id = $2 AND run_id IN (SELECT id FROM t)`, root, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &run.TreeNumbers{Web: map[int]run.Source{}}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		set, err := decodeSources(raw)
		if err != nil {
			return nil, err
		}
		for _, s := range set.Items {
			if s.WebN > 0 && (s.Kind == "" || s.Kind == run.KindWeb) {
				if have, ok := out.Web[s.WebN]; !ok || s.N < have.N {
					out.Web[s.WebN] = s
				}
			}
		}
	}
	return out, rows.Err()
}

// AnswerCited answers a tool call whose result the HOST numbers (D70); see harness.Store.
func (s *Store) AnswerCited(ctx context.Context, c *run.Claim, id string, cite run.CiteFunc) error {
	return s.InTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		root, err := rootOf(ctx, tx, c.RunID)
		if err != nil {
			return err
		}
		first, err := lockCounter(ctx, tx, root) // the root's row before the run's own
		if err != nil {
			return err
		}
		r, err := s.owned(ctx, tx, c)
		if err != nil {
			return err
		}
		tree, err := treeNumbers(ctx, tx, c.TenantID, root)
		if err != nil {
			return err
		}
		a, err := cite(first, tree)
		if err != nil {
			return err
		}
		if a.Used < 0 {
			return errors.New("an answer cannot take a negative number of citation numbers")
		}
		if a.Used > 0 {
			if _, err := tx.Exec(ctx, `UPDATE runs SET citation_next = $2 WHERE id = $1`, root, first+a.Used); err != nil {
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
		root, err := rootOf(ctx, tx, c.RunID)
		if err != nil {
			return err
		}
		if first, err = lockCounter(ctx, tx, root); err != nil {
			return err
		}
		if _, err := s.owned(ctx, tx, c); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE runs SET citation_next = $2 WHERE id = $1`, root, first+count); err != nil {
			return err
		}
		return recordSources(ctx, tx, c, &run.SourceBatch{Items: sources(first)})
	})
	return first, err
}

// citationsGiven is how many numbers the run tree has given: the root's counter less one (read without a lock; the counter only
// grows).
func citationsGiven(ctx context.Context, tx pgx.Tx, runID string) (int, error) {
	root, err := rootOf(ctx, tx, runID)
	if err != nil {
		return 0, err
	}
	var next int
	if err := tx.QueryRow(ctx, `SELECT citation_next FROM runs WHERE id = $1`, root).Scan(&next); err != nil {
		return 0, err
	}
	return next - 1, nil
}
