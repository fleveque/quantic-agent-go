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
	"time"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
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
	if err := s.migrate(ctx, path+".lock"); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// migrate brings the schema up to date with goose, which applies every
// embedded migration the database hasn't seen, in order, each in its own
// transaction, and records it in goose_db_version. One process migrates at a
// time (see lockFile); the others wait, then find nothing left to do.
func (s *Store) migrate(ctx context.Context, lockPath string) error {
	unlock, err := lockFile(ctx, lockPath)
	if err != nil {
		return err
	}
	defer unlock()

	if err := s.adopt(ctx); err != nil {
		return err
	}

	dir, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return err
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, s.db, dir)
	if err != nil {
		return fmt.Errorf("store: preparing migrations: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("store: migrating: %w", err)
	}
	return nil
}

// adopt hands a database migrated by milestone 7's hand-written code over to
// goose. Those databases record applied versions in schema_migrations, which
// goose doesn't read: left alone, goose would see version 0 and run 0001
// again, failing on "table runs already exists". So, once, in one
// transaction: create goose's version table, copy the versions across, and
// drop the old table. A database without schema_migrations is left alone.
func (s *Store) adopt(ctx context.Context) error {
	var legacy bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations')`).Scan(&legacy)
	if err != nil || !legacy {
		return err
	}
	versions, err := database.NewStore(database.DialectSQLite3, goose.DefaultTablename)
	if err != nil {
		return err
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
		if err != nil {
			return err
		}
		var applied []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return err
			}
			applied = append(applied, v)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		if err := versions.CreateVersionTable(ctx, tx); err != nil {
			return err
		}
		// Version 0 is the row goose writes when it creates the table.
		for _, v := range append([]int64{0}, applied...) {
			if err := versions.Insert(ctx, tx, database.InsertRequest{Version: v}); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `DROP TABLE schema_migrations`)
		return err
	})
	if err != nil {
		return fmt.Errorf("store: handing schema_migrations over to goose: %w", err)
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
