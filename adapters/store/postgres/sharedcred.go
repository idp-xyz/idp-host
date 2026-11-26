package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-host/internal/credential"
	"idp-host/internal/run"
)

// Renewal of a credential that a run tree shares: the member's CMI credential (ADR-0001 D68, D69) and the run's delegated gateway
// credential (D76). One implementation serves both; a credKind says which columns and which errors.
//
// The credential is held by one run (the root's); its sub-agents reference it. A renewal calls the authorization server, which is slow
// and not ours: no row lock is held across that call. One run renews at a time by a short lease on the holder's row (set by a
// conditional UPDATE, compared with the database's clock); it calls the server with nothing locked, and writes the result back only
// if the credential is still the one it read (compare and swap), so that a credential cleared meanwhile (the root ended or was
// canceled) is not brought back. A run that does not get the lease uses the token it has while that is still valid, or the one the
// renewing run has written, or waits a little. Where a transaction needs both the holder's row and its own, the holder's comes first
// (a cancel locks the root before its children), and each transaction is short, so a heartbeat is never kept waiting.

// renewLease is how long a renewal may take before another run may take over.
const renewLease = 16 * time.Second // just above the longest renewal (the harness gives the call 15 s), so a waiter can take over from a crashed holder in its own budget

// renewPoll is how long a run waits before it looks again for a renewal that another run is making.
const renewPoll = 100 * time.Millisecond

// credSnap is what a credential's columns hold at one moment: sealed is the column the compare and swap watches (nil when the holder
// has no credential), the others complete it for the kinds that keep them apart.
type credSnap struct {
	sealed  []byte
	refresh []byte
	expires *time.Time
}

// credKind describes one shared credential in the runs table.
type credKind struct {
	refCol   string // the column of a sub-agent run that names the run holding the credential
	leaseCol string // the column of the holder that carries the renewal lease
	valueCol string // the column the compare and swap watches: NULL when the holder has no credential
	cols     string // what a read of the holder selects, in the order dests scans it
	dests    func(*credSnap) []any
	open     func(s *Store, snap credSnap) (*credential.Token, error)
	// write is the SET clause that stores a renewed token (without the lease) with its values, numbered from $2 (the holder is $1).
	write func(s *Store, next *credential.Token) (set string, args []any, err error)
	gone  func() error // the holder has no credential (never had one, or it was cleared)
}

// cmiKind is the member's credential for the CMI: one sealed document in cmi_credential (D68).
var cmiKind = credKind{
	refCol: "cmi_credential_ref", leaseCol: "cmi_renewing_until", valueCol: "cmi_credential", cols: "cmi_credential",
	dests: func(c *credSnap) []any { return []any{&c.sealed} },
	open:  func(s *Store, c credSnap) (*credential.Token, error) { return s.openToken(c.sealed) },
	write: func(s *Store, next *credential.Token) (string, []any, error) {
		sealed, err := sealToken(s.Box, next)
		return "cmi_credential = $2", []any{sealed}, err
	},
	gone: func() error { return run.ErrCMICredentialUnusable },
}

// gatewayKind is the run's delegated credential for the gateway: the access token, its expiry and the refresh token apart (D8, D38).
var gatewayKind = credKind{
	refCol: "credential_ref", leaseCol: "credential_renewing_until", valueCol: "credential", cols: "credential, credential_expires_at, credential_refresh",
	dests: func(c *credSnap) []any { return []any{&c.sealed, &c.expires, &c.refresh} },
	open: func(s *Store, c credSnap) (*credential.Token, error) {
		if s.Box == nil {
			return nil, errors.New("credential key not configured")
		}
		plain, err := s.Box.Open(c.sealed)
		if err != nil {
			return nil, &run.Failure{Code: run.FailCredential, Message: "the credential cannot be decrypted"}
		}
		out := &credential.Token{AccessToken: string(plain)}
		if c.expires != nil {
			out.ExpiresAt = *c.expires
		}
		if c.refresh != nil {
			rt, err := s.Box.Open(c.refresh)
			if err != nil {
				return nil, &run.Failure{Code: run.FailCredential, Message: "the credential cannot be decrypted"}
			}
			out.RefreshToken = string(rt)
		}
		return out, nil
	},
	write: func(s *Store, next *credential.Token) (string, []any, error) {
		if s.Box == nil {
			return "", nil, errors.New("credential key not configured")
		}
		sealed, err := s.Box.Seal([]byte(next.AccessToken))
		if err != nil {
			return "", nil, err
		}
		var refresh []byte
		if next.RefreshToken != "" {
			if refresh, err = s.Box.Seal([]byte(next.RefreshToken)); err != nil {
				return "", nil, err
			}
		}
		var exp any
		if !next.ExpiresAt.IsZero() {
			exp = next.ExpiresAt
		}
		return "credential = $2, credential_expires_at = $3, credential_refresh = coalesce($4, credential_refresh)", []any{sealed, exp, refresh}, nil
	},
	gone: func() error {
		return &run.Failure{Code: run.FailCredential, Message: "the run carries no gateway credential"}
	},
}

// holderOf is the run whose row holds the credential of run id: itself, or the run its reference column names.
func holderOf(ctx context.Context, tx pgx.Tx, k credKind, id string) (string, error) {
	var ref *string
	if err := tx.QueryRow(ctx, `SELECT `+k.refCol+` FROM runs WHERE id = $1`, id).Scan(&ref); err != nil {
		return "", err
	}
	if ref != nil && *ref != "" {
		return *ref, nil
	}
	return id, nil
}

// credentialHolder is holderOf for the member's CMI credential.
func credentialHolder(ctx context.Context, tx pgx.Tx, id string) (string, error) {
	return holderOf(ctx, tx, cmiKind, id)
}

// RenewCMICredential renews the credential the claimed run uses; seen is the token the caller has. It returns the credential to use:
// the renewed one, or - when another run renewed it meanwhile, or is renewing it and the caller's token is still valid - that
// one. run.ErrCMICredentialUnusable when the credential is gone (cleared, or not openable) or cannot be renewed in time. Fenced by the
// claim.
func (s *Store) RenewCMICredential(ctx context.Context, c *run.Claim, seen *credential.Token, force bool, renew func(ctx context.Context, refreshToken string) (*credential.Token, error)) (*credential.Token, error) {
	return s.renewShared(ctx, c, cmiKind, seen, force, renew)
}

// RenewRunCredential is RenewCMICredential for the run's delegated gateway credential (D76): the credential of a sub-agent run is the
// root's, renewed once for the whole tree, with the same lease, compare and swap and lock order. Errors the authorization server gave
// (credential.ErrSessionEnded among them) are returned as they are; a credential that is gone or cannot be opened is a *run.Failure
// with the code CREDENTIAL_EXPIRED.
func (s *Store) RenewRunCredential(ctx context.Context, c *run.Claim, seen *credential.Token, force bool, renew func(ctx context.Context, refreshToken string) (*credential.Token, error)) (*credential.Token, error) {
	return s.renewShared(ctx, c, gatewayKind, seen, force, renew)
}

func (s *Store) renewShared(ctx context.Context, c *run.Claim, k credKind, seen *credential.Token, force bool, renew func(ctx context.Context, refreshToken string) (*credential.Token, error)) (*credential.Token, error) {
	for {
		snap, lease, holder, err := s.takeRenewal(ctx, c, k)
		if err != nil {
			return nil, err
		}
		cur, err := k.open(s, snap)
		if err != nil {
			if lease {
				_ = s.dropRenewal(context.WithoutCancel(ctx), c, k, holder) // a lease taken must not outlive a credential nobody can open
			}
			return nil, err
		}
		switch {
		case cur != nil && seen != nil && cur.AccessToken != seen.AccessToken:
			if lease {
				if err := s.dropRenewal(ctx, c, k, holder); err != nil {
					return nil, err
				}
			}
			return cur, nil // renewed meanwhile by a run that shares it
		case !lease:
			if !force && (cur.ExpiresAt.IsZero() || s.now().Before(cur.ExpiresAt)) {
				return cur, nil // another run is renewing and this token still works: use it meanwhile
			}
			// (a forced renewal is one the service refused the stored token for: it waits for the holder's result, it does not take that token again)
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
			if err := s.dropRenewal(context.WithoutCancel(ctx), c, k, holder); err != nil && !errors.Is(err, run.ErrFenced) {
				return nil, err
			}
			return nil, rerr
		}
		if next.RefreshToken == "" {
			next.RefreshToken = cur.RefreshToken
		}
		if err := s.storeRenewed(context.WithoutCancel(ctx), c, k, holder, snap.sealed, next); err != nil {
			return nil, err
		}
		return next, nil
	}
}

// takeRenewal reads the credential and tries to take the renewal lease on the holder's row, in one short transaction. lease says whether
// it was taken (and then the snapshot is the credential to renew); without it the snapshot is what is stored now.
func (s *Store) takeRenewal(ctx context.Context, c *run.Claim, k credKind) (snap credSnap, lease bool, holder string, err error) {
	err = s.InTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		snap, lease = credSnap{}, false
		var err error
		if holder, err = holderOf(ctx, tx, k, c.RunID); err != nil {
			return err
		}
		// the holder's row first, then our own (owned): the order a cancel locks them in
		// the database's clock decides (the lease is shared by processes whose clocks differ)
		row := tx.QueryRow(ctx, `UPDATE runs SET `+k.leaseCol+` = now() + $2::interval WHERE id = $1 AND `+k.valueCol+` IS NOT NULL AND (`+k.leaseCol+` IS NULL OR `+k.leaseCol+` < now()) RETURNING `+k.cols,
			holder, fmt.Sprintf("%d seconds", int(renewLease/time.Second)))
		switch e := row.Scan(k.dests(&snap)...); {
		case e == nil:
			lease = true
		case errors.Is(e, pgx.ErrNoRows):
			snap = credSnap{}
			if e := tx.QueryRow(ctx, `SELECT `+k.cols+` FROM runs WHERE id = $1`, holder).Scan(k.dests(&snap)...); e != nil {
				return e
			}
		default:
			return e
		}
		if _, e := s.owned(ctx, tx, c); e != nil {
			return e
		}
		if len(snap.sealed) == 0 {
			return k.gone()
		}
		return nil
	})
	return snap, lease, holder, err
}

// dropRenewal gives the lease back without a result. It is not subject to the claim's fence: the lease is the caller's own, and a
// claim lost meanwhile must not leave it held for a full lease.
func (s *Store) dropRenewal(ctx context.Context, c *run.Claim, k credKind, holder string) error {
	return s.InTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE runs SET `+k.leaseCol+` = NULL WHERE id = $1`, holder)
		return err
	})
}

// storeRenewed writes the renewed credential if the holder still holds the one that was read (compare and swap) and gives the lease
// back. A credential that was cleared or replaced meanwhile is not restored: the renewed token is dropped and the caller has none.
func (s *Store) storeRenewed(ctx context.Context, c *run.Claim, k credKind, holder string, was []byte, next *credential.Token) error {
	set, args, err := k.write(s, next)
	if err != nil {
		return err
	}
	swapped := true
	err = s.InTenant(ctx, c.TenantID, func(tx pgx.Tx) error {
		// $1 the holder, $2.. the new values, the last one the value the swap compares with
		all := append(append([]any{holder}, args...), was)
		tag, err := tx.Exec(ctx, `UPDATE runs SET `+set+`, `+k.leaseCol+` = NULL WHERE id = $1 AND `+k.valueCol+` = $`+fmt.Sprint(len(all)), all...)
		if err != nil {
			return err
		}
		// no fence check: a renewal the authorization server has completed is stored even if the claim was lost meanwhile (with a rotating
		// refresh token the old one is dead, and not storing the new one would kill the credential the runs share); the compare and swap
		// already stops a stale write
		if tag.RowsAffected() == 0 {
			// the credential changed under the renewal: the lease is still given back (this transaction commits), so that it does not
			// stay on the row for a full term
			swapped = false
			_, err := tx.Exec(ctx, `UPDATE runs SET `+k.leaseCol+` = NULL WHERE id = $1`, holder)
			return err
		}
		return nil
	})
	if err == nil && !swapped {
		return k.gone()
	}
	return err
}
