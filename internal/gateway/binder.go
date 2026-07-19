package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	_ "modernc.org/sqlite"
)

// SessionBinder maps a platform conversation key ("<platform>:<chatID>") to the
// Legion session it drives, plus the raw chat id needed for outbound delivery.
// The raw id lives only here, never in the core.
type SessionBinder interface {
	Resolve(ctx context.Context, platformKey string) (sessionID string, rawChatID string, ok bool, err error)
	Bind(ctx context.Context, platformKey string, sessionID string, rawChatID string) error
}

// SQLiteBinder is a SQLite-backed SessionBinder so bindings (and thus per-chat
// conversation continuity) survive a gateway restart.
type SQLiteBinder struct {
	db *sql.DB
}

// binderBusyTimeout is how long a blocked SQLite operation waits for a competing
// lock to clear before failing. Applied per-connection via the DSN so concurrent
// binds/resolves from multiple platform goroutines block-and-retry instead of
// surfacing an immediate SQLITE_BUSY. It only widens the retry window; a genuine
// lock still fails loudly once the timeout elapses.
const binderBusyTimeout = 5 * time.Second

// binderDSN augments a filesystem path with the busy_timeout PRAGMA applied to
// every physical connection the pool opens.
func binderDSN(path string) string {
	return path + "?_pragma=busy_timeout(" + strconv.Itoa(int(binderBusyTimeout.Milliseconds())) + ")"
}

// OpenSQLiteBinder opens (creating if needed) the binding database at path.
func OpenSQLiteBinder(ctx context.Context, path string) (*SQLiteBinder, error) {
	db, err := sql.Open("sqlite", binderDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open gateway sqlite %q: %w", path, err)
	}
	// Single-writer model: cap the pool at one connection so concurrent gateway
	// goroutines serialize instead of colliding on the file lock and returning
	// SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping gateway sqlite %q: %w", path, err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS channel_bindings (
			platform_key TEXT PRIMARY KEY,
			session_id   TEXT NOT NULL,
			raw_chat_id  TEXT NOT NULL,
			created_at   TEXT NOT NULL DEFAULT (datetime('now'))
		)
	`); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate channel_bindings: %w", err)
	}
	return &SQLiteBinder{db: db}, nil
}

// Resolve returns the binding for platformKey. ok is false with a nil error when
// no binding exists yet — a legitimate first-contact state the caller handles by
// creating a session.
func (b *SQLiteBinder) Resolve(ctx context.Context, platformKey string) (string, string, bool, error) {
	var sessionID, rawChatID string
	err := b.db.QueryRowContext(ctx, `
		SELECT session_id, raw_chat_id FROM channel_bindings WHERE platform_key = ?
	`, platformKey).Scan(&sessionID, &rawChatID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("resolve binding: %w", err)
	}
	return sessionID, rawChatID, true, nil
}

// Bind stores (or replaces) the binding for platformKey.
func (b *SQLiteBinder) Bind(ctx context.Context, platformKey, sessionID, rawChatID string) error {
	if _, err := b.db.ExecContext(ctx, `
		INSERT INTO channel_bindings (platform_key, session_id, raw_chat_id)
		VALUES (?, ?, ?)
		ON CONFLICT(platform_key) DO UPDATE SET
			session_id = excluded.session_id,
			raw_chat_id = excluded.raw_chat_id
	`, platformKey, sessionID, rawChatID); err != nil {
		return fmt.Errorf("bind binding: %w", err)
	}
	return nil
}

// Close releases the database.
func (b *SQLiteBinder) Close() error {
	if err := b.db.Close(); err != nil {
		return fmt.Errorf("close gateway sqlite: %w", err)
	}
	return nil
}
