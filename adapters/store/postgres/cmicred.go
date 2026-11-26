package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-host/internal/credential"
	"idp-host/internal/run"
)

// Renewal of the member's CMI credential (ADR-0001 D68, D69).
//
// The credential is held by one run (the root's); its sub-agents reference it. A renewal calls the authorization server, which is slow
// and not ours: no row lock is held across that call. One run renews at a time by a short lease on the holder's row
// (cmi_renewing_until, set by a conditional UPDATE); it calls the server with nothing locked, and writes the result back only if the
// credential is still the one it read (compare and swap), so that a credential cleared meanwhile (the root ended or was canceled)
// is not brought back. A run that does not get the lease uses the token it has while that is still valid, or the one the renewing run
// has written, or waits a little. Where a transaction needs both the holder's row and its own, the holder's comes first (a cancel
// locks the root before its children), and each transaction is short, so a heartbeat is never kept waiting.

// renewLease is how long a renewal may take before another run may take over.
const renewLease = 30 * time.Second

// renewPoll is how long a run waits before it looks again for a renewal that another run is making.
const renewPoll = 100 * time.Millisecond

// RenewCMICredential renews the credential the claimed run uses; seen is the token the caller has. It returns the credential to use:
// the renewed one, or - when another run renewed it meanwhile, or is renewing it and the caller's token is still valid - that
// one. run.ErrCMICredentialUnusable when the credential is gone (cleared, or not openable) or cannot be renewed in time. Fenced by the
// claim.
func (s *Store) RenewCMICredential(ctx context.Context, c *run.Claim, seen *credential.Token, renew func(ctx context.Context, refreshToken string) (*credential.Token, error)) (*credential.Token, error) {
	for {
		sealed, cur, lease, holder, err := s.takeRenewal(ctx, c)
		if err != nil {
			return nil, err
		}
		switch {
		case cur != nil && seen != nil && cur.AccessToken != seen.AccessToken:
			if lease {
				if err := s.dropRenewal(ctx, c, holder); err != nil {
					return nil, err
				}
			}
			return cur, nil // renewed meanwhile by a run that shares it
		case !lease:
			if cur.ExpiresAt.IsZero() || s.now().Before(cur.ExpiresAt) {
				return cur, nil // another run is renewing and this token still works: use it meanwhile
			}
			// another run is renewing and this token has expired: look again shortly
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(renewPoll):
			}
			continue
		}
		next, rerr := renew(ctx, cur.RefreshToken) // nothing is locked here
		if rerr != nil {
			if err := s.dropRenewal(context.WithoutCancel(ctx), c, holder); err != nil && !errors.Is(err, run.ErrFenced) {
				return nil, err
			}
			return nil, rerr
		}
		if next.RefreshToken == "" {
			next.RefreshToken = cur.RefreshToken
		}
		if err := s.storeRenewed(ctx, c, holder, sealed, next); err != nil {
			return nil, err
		}
		return next, nil
	}
}

// takeRenewal reads the credential and tries to take the renewal lease on the holder's row, in one short transaction. lease says whether
// it was taken (and then cur is the credential to renew); without it cur is what is stored now, nil when nothing is.
func (s *Store) takeRenewal(ctx context.Context, c *run.Claim) (sealed []byte, cur *credential.Token, lease bool, holder string, err error) {
	err = s.InTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		if holder, err = credentialHolder(ctx, tx, c.RunID); err != nil {
			return err
		}
		// the holder's row first, then our own (owned): the order a cancel locks them in
		tag := tx.QueryRow(ctx, `UPDATE runs SET cmi_renewing_until = $2 WHERE id = $1 AND cmi_credential IS NOT NULL AND (cmi_renewing_until IS NULL OR cmi_renewing_until < $3) RETURNING cmi_credential`,
			holder, s.now().Add(renewLease), s.now())
		switch e := tag.Scan(&sealed); {
		case e == nil:
			lease = true
		case errors.Is(e, pgx.ErrNoRows):
			if e := tx.QueryRow(ctx, `SELECT cmi_credential FROM runs WHERE id = $1`, holder).Scan(&sealed); e != nil {
				return e
			}
		default:
			return e
		}
		if _, e := s.owned(ctx, tx, c); e != nil {
			return e
		}
		if len(sealed) == 0 {
			return run.ErrCMICredentialUnusable
		}
		return nil
	})
	if err != nil {
		return nil, nil, false, "", err
	}
	cur, err = s.openToken(sealed)
	return sealed, cur, lease, holder, err
}

// dropRenewal gives the lease back without a result.
func (s *Store) dropRenewal(ctx context.Context, c *run.Claim, holder string) error {
	return s.InTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE runs SET cmi_renewing_until = NULL WHERE id = $1`, holder); err != nil {
			return err
		}
		_, err := s.owned(ctx, tx, c)
		return err
	})
}

// storeRenewed writes the renewed credential if the holder still holds the one that was read (compare and swap) and gives the lease
// back. A credential that was cleared or replaced meanwhile is not restored: the renewed token is dropped and the caller has none.
func (s *Store) storeRenewed(ctx context.Context, c *run.Claim, holder string, was []byte, next *credential.Token) error {
	sealed, err := sealToken(s.Box, next)
	if err != nil {
		return err
	}
	return s.InTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE runs SET cmi_credential = $2, cmi_renewing_until = NULL WHERE id = $1 AND cmi_credential = $3`, holder, sealed, was)
		if err != nil {
			return err
		}
		if _, err := s.owned(ctx, tx, c); err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return run.ErrCMICredentialUnusable
		}
		return nil
	})
}
