package postgres

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"idp-host/internal/agui"
	"idp-host/internal/run"
)

// loadSources reads a run's source set (D49); a run that recorded nothing has an empty one.
func loadSources(ctx context.Context, tx pgx.Tx, tenant, runID string) (*run.SourceSet, error) {
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT doc FROM run_sources WHERE tenant_id = $1 AND run_id = $2`, tenant, runID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return &run.SourceSet{}, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeSources(raw)
}

func decodeSources(raw []byte) (*run.SourceSet, error) {
	var set run.SourceSet
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, err
	}
	return &set, nil
}

// saveSources writes a run's source set. The caller holds the run's row lock (owned), so two writers of one run cannot lose
// each other's entries.
func saveSources(ctx context.Context, tx pgx.Tx, tenant, runID string, set *run.SourceSet) error {
	raw, err := json.Marshal(set)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO run_sources (tenant_id, run_id, doc) VALUES ($1, $2, $3)
		ON CONFLICT (tenant_id, run_id) DO UPDATE SET doc = EXCLUDED.doc, updated_at = now()`, tenant, runID, raw)
	return err
}

// recordSources adds a tool result's (or a sub-agent's) sources to the claimed run's set (D49), under the run's lock.
func recordSources(ctx context.Context, tx pgx.Tx, c *run.Claim, src *run.SourceBatch) error {
	if src.Empty() {
		return nil
	}
	set, err := loadSources(ctx, tx, c.TenantID, c.RunID)
	if err != nil {
		return err
	}
	set.Merge(src)
	return saveSources(ctx, tx, c.TenantID, c.RunID, set)
}

// CheckSources compares the final answer's citation marks with the run's sources, stores the outcome and appends
// idp.host/run.sources, in one transaction (D49). The run's input says whether its release has outward tools: without them
// and without sources the check is skipped and nothing is written.
func (s *Store) CheckSources(ctx context.Context, c *run.Claim, answer string) error {
	return s.InTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		r, err := s.owned(ctx, tx, c)
		if err != nil {
			return err
		}
		set, err := loadSources(ctx, tx, c.TenantID, c.RunID)
		if err != nil {
			return err
		}
		// D74: the numbers of the whole run tree are valid, whichever run of it was given them
		given, err := citationsGiven(ctx, tx, c.TenantID, c.RunID)
		if err != nil {
			return err
		}
		if !set.CheckAtEndKnowing(answer, r.Input.ExternalTools, given) {
			return nil
		}
		if err := saveSources(ctx, tx, c.TenantID, c.RunID, set); err != nil {
			return err
		}
		return appendEvents(ctx, tx, c.TenantID, c.RunID, []agui.Event{agui.Custom(agui.CustomRunSources, set.Event())})
	})
}

// attachSources fills Sources and UnverifiedCitations of the runs read in tx, with one query.
func attachSources(ctx context.Context, tx pgx.Tx, runs ...*run.Run) error {
	if len(runs) == 0 {
		return nil
	}
	ids := make([]string, len(runs))
	byID := map[string]*run.Run{}
	for i, r := range runs {
		ids[i] = r.ID
		byID[r.ID] = r
	}
	rows, err := tx.Query(ctx, `SELECT run_id, doc FROM run_sources WHERE run_id = ANY($1)`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return err
		}
		var set run.SourceSet
		if err := json.Unmarshal(raw, &set); err != nil {
			return err
		}
		web, know := run.SplitSources(set.Items)
		r := byID[id]
		r.Sources, r.KnowledgeSources, r.UnverifiedCitations, r.UnlistedCitations = web, know, set.Unverified, set.Unlisted
		if len(web) == 0 {
			r.Sources = nil
		}
		if len(know) == 0 {
			r.KnowledgeSources = nil
		}
	}
	return rows.Err()
}

// attachSourcesTo is attachSources for a slice of runs.
func attachSourcesTo(ctx context.Context, tx pgx.Tx, runs []run.Run) error {
	ptrs := make([]*run.Run, len(runs))
	for i := range runs {
		ptrs[i] = &runs[i]
	}
	return attachSources(ctx, tx, ptrs...)
}
