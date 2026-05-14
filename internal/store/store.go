// Package store provides SQLite-backed persistence for the Discord bridge.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// SessionStatus tracks the lifecycle of a bridge session.
type SessionStatus string

const (
	StatusActive   SessionStatus = "active"
	StatusComplete SessionStatus = "complete"
	StatusDropped  SessionStatus = "dropped"
)

// SessionRecord maps a Discord thread to a Forge session.
type SessionRecord struct {
	DiscordThreadID string
	ForgeSessionID  string
	Status          SessionStatus
	CreatedAt       time.Time
	LastEventAt     time.Time
	LastEventID     string
}

// PendingMessage is an outbound retry queue entry.
type PendingMessage struct {
	DiscordMessageID string
	ThreadID         string
	Body             string
	RetryCount       int
	NextRetryAt      time.Time
}

// Store is the persistence interface for the bridge.
type Store interface {
	UpsertSession(ctx context.Context, s SessionRecord) error
	GetSessionByThread(ctx context.Context, threadID string) (*SessionRecord, error)
	ListActiveSessions(ctx context.Context) ([]SessionRecord, error)
	RecordEvent(ctx context.Context, sessionID, eventID, discordMsgID string) error
	EventSeen(ctx context.Context, sessionID, eventID string) (bool, error)
	EnqueuePending(ctx context.Context, item PendingMessage) error
	DequeueDuePending(ctx context.Context, now time.Time, limit int) ([]PendingMessage, error)
	Close() error
}

// SQLiteStore implements Store using SQLite via modernc.org/sqlite.
type SQLiteStore struct {
	db *sql.DB
}

// New opens (or creates) a SQLite database at the given path and runs migrations.
func New(dbPath string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return &SQLiteStore{db: db}, nil
}

func migrate(db *sql.DB) error {
	ddl := `
	CREATE TABLE IF NOT EXISTS sessions (
		discord_thread_id TEXT PRIMARY KEY,
		forge_session_id  TEXT NOT NULL,
		status            TEXT NOT NULL DEFAULT 'active',
		created_at        DATETIME NOT NULL DEFAULT (datetime('now')),
		last_event_at     DATETIME NOT NULL DEFAULT (datetime('now')),
		last_event_id     TEXT NOT NULL DEFAULT ''
	);

	CREATE INDEX IF NOT EXISTS idx_sessions_status ON sessions(status);

	CREATE TABLE IF NOT EXISTS messages (
		forge_session_id   TEXT NOT NULL,
		forge_event_id     TEXT PRIMARY KEY,
		discord_message_id TEXT NOT NULL,
		posted_at          DATETIME NOT NULL DEFAULT (datetime('now'))
	);

	CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(forge_session_id);

	CREATE TABLE IF NOT EXISTS pending (
		discord_message_id TEXT PRIMARY KEY,
		thread_id          TEXT NOT NULL,
		body               TEXT NOT NULL,
		retry_count        INTEGER NOT NULL DEFAULT 0,
		next_retry_at      DATETIME NOT NULL DEFAULT (datetime('now'))
	);

	CREATE INDEX IF NOT EXISTS idx_pending_next ON pending(next_retry_at);
	`
	_, err := db.Exec(ddl)
	return err
}

func (s *SQLiteStore) UpsertSession(ctx context.Context, rec SessionRecord) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (discord_thread_id, forge_session_id, status, created_at, last_event_at, last_event_id)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(discord_thread_id) DO UPDATE SET
			forge_session_id = excluded.forge_session_id,
			status           = excluded.status,
			last_event_at    = excluded.last_event_at,
			last_event_id    = excluded.last_event_id
	`, rec.DiscordThreadID, rec.ForgeSessionID, string(rec.Status),
		rec.CreatedAt.UTC(), rec.LastEventAt.UTC(), rec.LastEventID)
	return err
}

func (s *SQLiteStore) GetSessionByThread(ctx context.Context, threadID string) (*SessionRecord, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT discord_thread_id, forge_session_id, status, created_at, last_event_at, last_event_id
		FROM sessions WHERE discord_thread_id = ?`, threadID)

	var rec SessionRecord
	var status string
	if err := row.Scan(&rec.DiscordThreadID, &rec.ForgeSessionID, &status,
		&rec.CreatedAt, &rec.LastEventAt, &rec.LastEventID); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	rec.Status = SessionStatus(status)
	return &rec, nil
}

func (s *SQLiteStore) ListActiveSessions(ctx context.Context) ([]SessionRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT discord_thread_id, forge_session_id, status, created_at, last_event_at, last_event_id
		FROM sessions WHERE status = 'active'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SessionRecord
	for rows.Next() {
		var rec SessionRecord
		var status string
		if err := rows.Scan(&rec.DiscordThreadID, &rec.ForgeSessionID, &status,
			&rec.CreatedAt, &rec.LastEventAt, &rec.LastEventID); err != nil {
			return nil, err
		}
		rec.Status = SessionStatus(status)
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) RecordEvent(ctx context.Context, sessionID, eventID, discordMsgID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO messages (forge_session_id, forge_event_id, discord_message_id)
		VALUES (?, ?, ?)`, sessionID, eventID, discordMsgID)
	if err != nil {
		return err
	}

	// Also update last_event_id on the session
	_, err = s.db.ExecContext(ctx, `
		UPDATE sessions SET last_event_id = ?, last_event_at = datetime('now')
		WHERE forge_session_id = ?`, eventID, sessionID)
	return err
}

func (s *SQLiteStore) EventSeen(ctx context.Context, sessionID, eventID string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(1) FROM messages WHERE forge_session_id = ? AND forge_event_id = ?`,
		sessionID, eventID).Scan(&count)
	return count > 0, err
}

func (s *SQLiteStore) EnqueuePending(ctx context.Context, item PendingMessage) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO pending (discord_message_id, thread_id, body, retry_count, next_retry_at)
		VALUES (?, ?, ?, ?, ?)`,
		item.DiscordMessageID, item.ThreadID, item.Body, item.RetryCount, item.NextRetryAt.UTC())
	return err
}

func (s *SQLiteStore) DequeueDuePending(ctx context.Context, now time.Time, limit int) ([]PendingMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT discord_message_id, thread_id, body, retry_count, next_retry_at
		FROM pending
		WHERE next_retry_at <= ?
		ORDER BY next_retry_at ASC
		LIMIT ?`, now.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PendingMessage
	for rows.Next() {
		var p PendingMessage
		if err := rows.Scan(&p.DiscordMessageID, &p.ThreadID, &p.Body,
			&p.RetryCount, &p.NextRetryAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}

	// Delete dequeued items
	if len(out) > 0 {
		for _, p := range out {
			_, _ = s.db.ExecContext(ctx, `DELETE FROM pending WHERE discord_message_id = ?`, p.DiscordMessageID)
		}
	}

	return out, rows.Err()
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}
