package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Policy is a tenant's governance policy (ADR-0001 D15). Zero means unlimited.
type Policy struct {
	MaxConcurrentRuns int       `json:"maxConcurrentRuns"`
	TokensPerDay      int64     `json:"tokensPerDay"`
	MaxTokensPerRun   int64     `json:"maxTokensPerRun"`
	UpdatedBy         string    `json:"updatedBy,omitempty"`
	UpdatedAt         time.Time `json:"updatedAt,omitzero"`
	Default           bool      `json:"default"` // true when the tenant has no stored policy
}

// DefaultPolicy applies to tenants without a stored policy.
var DefaultPolicy = Policy{MaxConcurrentRuns: 10, Default: true}

// Usage is a tenant's consumption over the rolling day plus what is in flight now.
type Usage struct {
	ActiveRuns     int   `json:"activeRuns"`
	RunsLast24h    int   `json:"runsLast24h"`
	TokensLast24h  int64 `json:"tokensLast24h"`
	TokensPerDay   int64 `json:"tokensPerDay"`
	MaxConcurrency int   `json:"maxConcurrentRuns"`
}

// Validate checks a policy.
func (p Policy) Validate() error {
	if p.MaxConcurrentRuns < 0 || p.MaxConcurrentRuns > 100000 || p.TokensPerDay < 0 || p.MaxTokensPerRun < 0 {
		return fail("INVALID", "policy values must be >= 0 (0 = unlimited)")
	}
	return nil
}

func policyOf(ctx context.Context, tx pgx.Tx, tenant string) (Policy, error) {
	var p Policy
	var by *string
	err := tx.QueryRow(ctx, `SELECT max_concurrent_runs, tokens_per_day, max_tokens_per_run, updated_by, updated_at FROM tenant_policies WHERE tenant_id = $1`, tenant).
		Scan(&p.MaxConcurrentRuns, &p.TokensPerDay, &p.MaxTokensPerRun, &by, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultPolicy, nil
	}
	if err != nil {
		return p, err
	}
	if by != nil {
		p.UpdatedBy = *by
	}
	return p, nil
}

// GetPolicy returns the tenant's policy (the default when none is stored).
func (s *Store) GetPolicy(ctx context.Context, tenant string) (*Policy, error) {
	var p Policy
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
		p, err = policyOf(ctx, tx, tenant)
		return err
	})
	return &p, err
}

// SetPolicy stores the tenant's policy.
func (s *Store) SetPolicy(ctx context.Context, tenant string, p Policy, by string) (*Policy, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	var out Policy
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO tenant_policies (tenant_id, max_concurrent_runs, tokens_per_day, max_tokens_per_run, updated_by)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (tenant_id) DO UPDATE SET max_concurrent_runs = EXCLUDED.max_concurrent_runs, tokens_per_day = EXCLUDED.tokens_per_day,
			  max_tokens_per_run = EXCLUDED.max_tokens_per_run, updated_by = EXCLUDED.updated_by, updated_at = now()`,
			tenant, p.MaxConcurrentRuns, p.TokensPerDay, p.MaxTokensPerRun, by); err != nil {
			return err
		}
		if err := Audit(ctx, tx, tenant, by, "policy.set", "", p); err != nil {
			return err
		}
		var err error
		out, err = policyOf(ctx, tx, tenant)
		return err
	})
	return &out, err
}

// tokensLastDay is the tenant's token consumption in the rolling 24 hours.
func (s *Store) tokensLastDay(ctx context.Context, tx pgx.Tx) (int64, error) {
	var n int64
	err := tx.QueryRow(ctx, `SELECT coalesce(sum(prompt_tokens + completion_tokens), 0) FROM run_steps WHERE started_at >= $1`, s.now().Add(-24*time.Hour)).Scan(&n)
	return n, err
}

func activeRuns(ctx context.Context, tx pgx.Tx) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM runs WHERE status IN ('QUEUED', 'RUNNING', 'AWAITING_INPUT', 'AWAITING_SUBAGENT')`).Scan(&n)
	return n, err
}

// GetUsage reports the tenant's consumption against its policy.
func (s *Store) GetUsage(ctx context.Context, tenant string) (*Usage, error) {
	var u Usage
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		p, err := policyOf(ctx, tx, tenant)
		if err != nil {
			return err
		}
		u.TokensPerDay, u.MaxConcurrency = p.TokensPerDay, p.MaxConcurrentRuns
		if u.ActiveRuns, err = activeRuns(ctx, tx); err != nil {
			return err
		}
		if u.TokensLast24h, err = s.tokensLastDay(ctx, tx); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM runs WHERE created_at >= $1`, s.now().Add(-24*time.Hour)).Scan(&u.RunsLast24h)
	})
	return &u, err
}
