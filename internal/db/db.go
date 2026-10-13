// Package db opens the daemon's SQLite config database and applies the
// schema migrations shared with the Rust implementation.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	_ "modernc.org/sqlite" // pure-Go SQLite driver ("sqlite")
)

// DSN builds a modernc.org/sqlite DSN for path with foreign keys on, WAL
// journaling and a 5s busy timeout (sqlx's defaults).
func DSN(path string) string {
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	return "file:" + path + "?" + q.Encode()
}

// Open opens (creating if needed) the SQLite database at path and runs migrations.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	pool, err := sql.Open("sqlite", DSN(path))
	if err != nil {
		return nil, err
	}
	pool.SetMaxOpenConns(5)
	if err := pool.PingContext(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if err := Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// OpenMemory opens a private in-memory database with migrations applied.
// Intended for tests.
func OpenMemory(ctx context.Context) (*sql.DB, error) {
	pool, err := sql.Open("sqlite", "file::memory:?_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	// A single connection keeps every query on the same in-memory database.
	pool.SetMaxOpenConns(1)
	if err := Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// Migrate applies every migration not yet recorded in `_migrations`.
func Migrate(ctx context.Context, pool *sql.DB) error {
	if _, err := pool.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS _migrations (
		id INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`); err != nil {
		return err
	}
	for i, m := range migrations {
		id := i + 1
		var applied int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM _migrations WHERE id = ?", id).Scan(&applied); err != nil {
			return err
		}
		if applied > 0 {
			continue
		}
		slog.Info("applying migration", "id", id)
		for _, stmt := range strings.Split(m, ";") {
			stmt = strings.TrimSpace(stmt)
			if stmt == "" {
				continue
			}
			if _, err := pool.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("migration %d: %w", id, err)
			}
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO _migrations (id) VALUES (?)", id); err != nil {
			return err
		}
	}
	return nil
}
