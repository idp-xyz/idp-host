package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-host/internal/agui"
)

// Session groups a user's threads.
type Session struct {
	ID        string    `json:"sessionId"`
	Owner     string    `json:"owner"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"createdAt"`
}

// Thread is a conversation; its id may be chosen by the caller.
type Thread struct {
	ID         string    `json:"threadId"`
	SessionID  *string   `json:"sessionId"`
	Owner      string    `json:"owner"`
	AgentID    *string   `json:"agentId"`
	MessageSeq int64     `json:"messageCount"`
	CreatedAt  time.Time `json:"createdAt"`
}

// CreateSession creates a session for owner.
func (s *Store) CreateSession(ctx context.Context, tenant, owner, title string) (*Session, error) {
	if strings.TrimSpace(title) == "" || len(title) > 200 {
		return nil, fail("INVALID", "title is required (at most 200 characters)")
	}
	out := &Session{ID: NewUUID(), Owner: owner, Title: title}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO sessions (id, tenant_id, owner, title) VALUES ($1, $2, $3, $4) RETURNING created_at`, out.ID, tenant, owner, title).Scan(&out.CreatedAt)
	})
	return out, err
}

// GetSession returns a session of owner ("" = any owner).
func (s *Store) GetSession(ctx context.Context, tenant, owner, id string) (*Session, error) {
	var out Session
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT id::text, owner, title, created_at FROM sessions WHERE id = $1 AND ($2 = '' OR owner = $2)`, id, owner).
			Scan(&out.ID, &out.Owner, &out.Title, &out.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return fail("NOT_FOUND", "session not found")
		}
		return err
	})
	return &out, err
}

// ListSessions returns the owner's sessions, newest first.
func (s *Store) ListSessions(ctx context.Context, tenant, owner string) ([]Session, error) {
	out := []Session{}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id::text, owner, title, created_at FROM sessions WHERE ($1 = '' OR owner = $1) ORDER BY created_at DESC LIMIT 500`, owner)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var x Session
			if err := rows.Scan(&x.ID, &x.Owner, &x.Title, &x.CreatedAt); err != nil {
				return err
			}
			out = append(out, x)
		}
		return rows.Err()
	})
	return out, err
}

const threadCols = `id, session_id::text, owner, agent_id::text, message_seq, created_at`

func scanThread(row pgx.Row) (*Thread, error) {
	var t Thread
	if err := row.Scan(&t.ID, &t.SessionID, &t.Owner, &t.AgentID, &t.MessageSeq, &t.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fail("NOT_FOUND", "thread not found")
		}
		return nil, err
	}
	return &t, nil
}

// CreateThread creates a thread (id optional; a session may be attached).
func (s *Store) CreateThread(ctx context.Context, tenant, owner, id, sessionID string) (*Thread, error) {
	if id == "" {
		id = "thr_" + strings.ReplaceAll(NewUUID(), "-", "")
	}
	if !agui.ValidID(id) {
		return nil, fail("INVALID", "thread id must match ^[A-Za-z0-9_.:-]{1,128}$")
	}
	var out *Thread
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		var sid any
		if sessionID != "" {
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sessions WHERE id = $1 AND owner = $2)`, sessionID, owner).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return fail("NOT_FOUND", "session not found")
			}
			sid = sessionID
		}
		if _, err := tx.Exec(ctx, `INSERT INTO threads (tenant_id, id, session_id, owner) VALUES ($1, $2, $3, $4)`, tenant, id, sid, owner); err != nil {
			if isUniqueViolation(err) {
				return fail("CONFLICT", "thread %s exists", id)
			}
			return err
		}
		var err error
		out, err = scanThread(tx.QueryRow(ctx, `SELECT `+threadCols+` FROM threads WHERE id = $1`, id))
		return err
	})
	return out, err
}

// GetThread returns a thread of owner ("" = any owner).
func (s *Store) GetThread(ctx context.Context, tenant, owner, id string) (*Thread, error) {
	var out *Thread
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
		out, err = scanThread(tx.QueryRow(ctx, `SELECT `+threadCols+` FROM threads WHERE id = $1 AND ($2 = '' OR owner = $2)`, id, owner))
		return err
	})
	return out, err
}

// ListMessages returns the thread's messages in order.
func (s *Store) ListMessages(ctx context.Context, tenant, owner, threadID string) ([]agui.Message, error) {
	var out []agui.Message
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := scanThread(tx.QueryRow(ctx, `SELECT `+threadCols+` FROM threads WHERE id = $1 AND ($2 = '' OR owner = $2)`, threadID, owner)); err != nil {
			return err
		}
		var err error
		out, err = threadMessages(ctx, tx, threadID)
		return err
	})
	return out, err
}

func threadMessages(ctx context.Context, tx pgx.Tx, threadID string) ([]agui.Message, error) {
	rows, err := tx.Query(ctx, `SELECT message FROM thread_messages WHERE thread_id = $1 ORDER BY seq`, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []agui.Message{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var m agui.Message
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// appendMessages stores messages on the thread (ids must be new) and returns how many were added.
func appendMessages(ctx context.Context, tx pgx.Tx, tenant, threadID, runID string, msgs []agui.Message) (int, error) {
	n := 0
	for _, m := range msgs {
		var seq int64
		if err := tx.QueryRow(ctx, `UPDATE threads SET message_seq = message_seq + 1 WHERE id = $1 RETURNING message_seq`, threadID).Scan(&seq); err != nil {
			return n, err
		}
		raw, err := json.Marshal(m)
		if err != nil {
			return n, err
		}
		var rid any
		if runID != "" {
			rid = runID
		}
		if _, err := tx.Exec(ctx, `INSERT INTO thread_messages (tenant_id, thread_id, seq, id, run_id, message) VALUES ($1, $2, $3, $4, $5, $6)`,
			tenant, threadID, seq, m.ID, rid, raw); err != nil {
			if isUniqueViolation(err) {
				return n, fail("CONFLICT", "message %s already exists on the thread", m.ID)
			}
			return n, err
		}
		n++
	}
	return n, nil
}
