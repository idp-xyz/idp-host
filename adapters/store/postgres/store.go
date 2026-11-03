// Package postgres is the HOST's database: a pool that refuses roles able to bypass row-level
// security, embedded migrations, tenant-bound transactions, and the repositories of agents,
// releases, sessions, threads, runs, events and tool calls (ADR-0001 D3, D8, D12).
package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"idp-host/internal/credential"
	"idp-host/migrations"
)

// AppRole is the NOLOGIN group role the service's login role must belong to.
const AppRole = "idp_host_app"

// Error is a store failure callers can map to a response: a stable code and a safe message.
type Error struct {
	Code    string // NOT_FOUND, CONFLICT, FORBIDDEN, INVALID, GONE
	Message string
}

// Error implements error.
func (e *Error) Error() string { return e.Code + ": " + e.Message }

func fail(code, format string, a ...any) error {
	return &Error{Code: code, Message: fmt.Sprintf(format, a...)}
}

// Store is the HOST database.
type Store struct {
	Pool *pgxpool.Pool
	Box  *credential.Box  // encrypts run credentials; nil means runs cannot carry credentials
	Now  func() time.Time // injectable clock (tests)
}

// Open connects as the service role and verifies that RLS applies to it.
func Open(ctx context.Context, url string, box *credential.Box) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	var super, bypass bool
	err = pool.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("database unreachable or role unknown: %w", err)
	}
	if super || bypass {
		pool.Close()
		return nil, errors.New("refusing to run as a superuser or BYPASSRLS role: row-level security would not apply; connect as a member of " + AppRole)
	}
	var version int
	if err := pool.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		pool.Close()
		return nil, fmt.Errorf("schema not migrated (run idp-host migrate): %w", err)
	}
	if want := latest(); version != want {
		pool.Close()
		return nil, fmt.Errorf("schema at version %d, binary needs %d (run idp-host migrate)", version, want)
	}
	return &Store{Pool: pool, Box: box, Now: time.Now}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.Pool.Close() }

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

type migration struct {
	version int
	name    string
	sql     string
}

func load() []migration {
	entries, _ := fs.ReadDir(migrations.FS, ".")
	var out []migration
	for _, e := range entries {
		var v int
		if _, err := fmt.Sscanf(e.Name(), "%04d_", &v); err != nil || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, _ := migrations.FS.ReadFile(e.Name())
		out = append(out, migration{v, e.Name(), string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out
}

func latest() int {
	m := load()
	if len(m) == 0 {
		return 0
	}
	return m[len(m)-1].version
}

// Migrate applies pending migrations as the schema owner, one transaction each, serialized by an
// advisory lock.
func Migrate(ctx context.Context, ownerURL string) (applied []string, err error) {
	conn, err := pgx.Connect(ctx, ownerURL)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(72094)`); err != nil {
		return nil, err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(72094)`)
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version int PRIMARY KEY, name text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return nil, err
	}
	var current int
	if err := conn.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return nil, err
	}
	for _, m := range load() {
		if m.version <= current {
			continue
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return applied, err
		}
		if _, err := tx.Exec(ctx, m.sql); err != nil {
			_ = tx.Rollback(ctx)
			return applied, fmt.Errorf("%s: %w", m.name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, m.version, m.name); err != nil {
			_ = tx.Rollback(ctx)
			return applied, err
		}
		if _, err := tx.Exec(ctx, `GRANT SELECT ON schema_migrations TO `+AppRole); err != nil {
			_ = tx.Rollback(ctx)
			return applied, err
		}
		if err := tx.Commit(ctx); err != nil {
			return applied, err
		}
		applied = append(applied, m.name)
	}
	return applied, nil
}

// InTenant runs fn in a transaction whose row-level security is bound to tenantID.
func (s *Store) InTenant(ctx context.Context, tenantID string, fn func(tx pgx.Tx) error) error {
	if tenantID == "" {
		return fail("FORBIDDEN", "tenant required")
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
			return err
		}
		return fn(tx)
	})
}

// inWorker runs fn in a transaction that may see runs of every tenant (claiming only).
func (s *Store) inWorker(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.role', 'worker', true)`); err != nil {
			return err
		}
		return fn(tx)
	})
}

// SeedTenant creates (or finds) a tenant as the schema owner.
func SeedTenant(ctx context.Context, ownerURL, id, slug string) (string, error) {
	conn, err := pgx.Connect(ctx, ownerURL)
	if err != nil {
		return "", err
	}
	defer conn.Close(ctx)
	if id == "" {
		id = NewUUID()
	}
	var out string
	err = conn.QueryRow(ctx, `INSERT INTO tenants (id, slug) VALUES ($1, $2) ON CONFLICT (slug) DO UPDATE SET slug = EXCLUDED.slug RETURNING id::text`, id, slug).Scan(&out)
	return out, err
}

var identRE = regexp.MustCompile(`^[a-z0-9_]{1,63}$`)

// EnsureLogin creates or updates a login role that is a member of idp_host_app (owner connection).
func EnsureLogin(ctx context.Context, ownerURL, user, password string) error {
	if !identRE.MatchString(user) {
		return errors.New("invalid role name")
	}
	conn, err := pgx.Connect(ctx, ownerURL)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, user).Scan(&exists); err != nil {
		return err
	}
	verb := "CREATE"
	if exists {
		verb = "ALTER"
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf(`%s ROLE %s LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '%s'`, verb, user, strings.ReplaceAll(password, "'", "''"))); err != nil {
		return err
	}
	_, err = conn.Exec(ctx, fmt.Sprintf(`GRANT %s TO %s`, AppRole, user))
	return err
}

// NewUUID returns a random UUID v4 string.
func NewUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// IsUUID reports whether s looks like a UUID.
func IsUUID(s string) bool { return uuidRE.MatchString(strings.ToLower(s)) }

// Audit records an administrative action in the current tenant transaction, so the change and
// its record commit together. The service role can only insert audit rows.
func Audit(ctx context.Context, tx pgx.Tx, tenantID, subject, action, resource string, detail any) error {
	var d []byte
	if detail != nil {
		var err error
		if d, err = json.Marshal(detail); err != nil {
			return err
		}
	}
	var res any
	if resource != "" {
		res = resource
	}
	_, err := tx.Exec(ctx, `INSERT INTO audit_events (tenant_id, subject, action, resource_id, detail) VALUES ($1, $2, $3, $4, $5)`,
		tenantID, subject, action, res, d)
	return err
}

// AuditEvent is one administrative action.
type AuditEvent struct {
	ID         int64           `json:"id"`
	At         time.Time       `json:"at"`
	Subject    string          `json:"subject"`
	Action     string          `json:"action"`
	ResourceID *string         `json:"resourceId"`
	Detail     json.RawMessage `json:"detail"`
}

// ListAudit returns the most recent audit events of the tenant.
func (s *Store) ListAudit(ctx context.Context, tenant string, limit int) ([]AuditEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	out := []AuditEvent{}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, at, subject, action, resource_id, detail FROM audit_events ORDER BY id DESC LIMIT $1`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e AuditEvent
			if err := rows.Scan(&e.ID, &e.At, &e.Subject, &e.Action, &e.ResourceID, &e.Detail); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// constraintOf names the violated constraint of a unique violation ("" otherwise).
func constraintOf(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

// errRetry asks the caller to run the transaction again: a concurrent transaction created the row
// this one was about to create, and the second attempt will find it.
var errRetry = errors.New("retry transaction")

func isRLSViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42501"
}
