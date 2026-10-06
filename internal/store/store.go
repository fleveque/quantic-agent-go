// Package store keeps the agent's history in SQLite: every run, every tool
// call it made (the audit log of design N3), and what it produced.
//
// It uses modernc.org/sqlite, a pure-Go translation of SQLite, so the binary
// stays static with no C toolchain (design §3.7). The schema is a series of
// numbered migrations embedded in the binary and applied on Open.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver with database/sql
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store is the agent's database. It is safe for concurrent use: *sql.DB is a
// pool of connections, not a single one.
type Store struct {
	db  *sql.DB
	now func() time.Time // the clock, replaceable in tests
}

// Open opens (creating if needed) the database at path and brings its schema
// up to date.
//
// Three settings travel in the connection string, because database/sql opens
// several connections and each one needs them: foreign keys on (SQLite
// ignores REFERENCES otherwise), write-ahead logging (readers don't block the
// writer), and a busy timeout (a connection waits for a lock instead of
// failing at once).
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := "file:" + path +
		"?_pragma=foreign_keys(1)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: opening %s: %w", path, err)
	}
	s := &Store{db: db, now: time.Now}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// migrate applies every embedded migration the database hasn't seen, in
// order, each in its own transaction: a migration either applies completely
// and is recorded, or doesn't apply at all.
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("store: creating schema_migrations: %w", err)
	}

	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files) // 0001_..., 0002_...: the names are the order

	for _, file := range files {
		version, err := strconv.Atoi(strings.SplitN(strings.TrimPrefix(file, "migrations/"), "_", 2)[0])
		if err != nil {
			return fmt.Errorf("store: migration %s has no version number: %w", file, err)
		}
		var applied bool
		err = s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = ?)`, version).Scan(&applied)
		if err != nil {
			return fmt.Errorf("store: checking migration %d: %w", version, err)
		}
		if applied {
			continue
		}

		body, err := migrations.ReadFile(file)
		if err != nil {
			return err
		}
		err = s.inTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, string(body)); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, version, s.stamp())
			return err
		})
		if err != nil {
			return fmt.Errorf("store: applying migration %s: %w", file, err)
		}
	}
	return nil
}

// inTx runs fn in a transaction, committing if it returns nil and rolling
// back otherwise.
func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// Rollback after a successful Commit does nothing and returns
	// sql.ErrTxDone, so deferring it is safe and covers every early return,
	// and a panic in fn, too.
	defer tx.Rollback()

	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// stamp is the current time as stored: RFC 3339 in UTC, which sorts as text
// in the same order as time.
func (s *Store) stamp() string { return s.now().UTC().Format(time.RFC3339Nano) }

// ErrNotFound means no row matched.
var ErrNotFound = errors.New("store: not found")
