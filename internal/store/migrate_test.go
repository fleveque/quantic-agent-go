package store

import (
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// These tests are inside package store, not store_test, because they reach
// for the *sql.DB under a Store: migrations are below the API the rest of the
// agent uses.

func openForTest(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func tables(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(),
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	return strings.Join(names, " ")
}

// Every migration must undo cleanly: all the way down leaves only goose's own
// table, and back up again rebuilds a usable schema. A Down section that's
// missing or wrong fails here, not on the day it's needed.
func TestMigrationsGoDownAndUpAgain(t *testing.T) {
	s := openForTest(t, filepath.Join(t.TempDir(), "agent.db"))
	ctx := t.Context()
	dir, _ := fs.Sub(migrations, "migrations")
	p, err := goose.NewProvider(goose.DialectSQLite3, s.db, dir)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := p.DownTo(ctx, 0); err != nil {
		t.Fatalf("down: %v", err)
	}
	if got := tables(t, s.db); got != "goose_db_version" {
		t.Errorf("after going down, tables = %q, want only goose_db_version", got)
	}

	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if _, err := s.StartRun(ctx, "research", "q", "m"); err != nil {
		t.Errorf("schema unusable after down and up: %v", err)
	}
}

// A database created by milestone 7, which tracked its migrations in
// schema_migrations, opens with goose without running 0001 again, and keeps
// its data.
func TestADatabaseFromBeforeGooseIsAdopted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	legacy, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// What milestone 7's Open left behind: its bookkeeping table, the 0001
	// schema (the file's Up section), and a run.
	body, err := os.ReadFile("migrations/0001_runs_calls_drafts.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, _, _ := strings.Cut(string(body), "-- +goose Down")
	for _, stmt := range []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`,
		`INSERT INTO schema_migrations VALUES (1, '2026-10-06T16:09:00Z')`,
		up,
		`INSERT INTO runs (kind, input, model, state, started_at) VALUES ('research', 'kept?', 'm', 'answered', '2026-10-06T16:09:00Z')`,
	} {
		if _, err := legacy.Exec(stmt); err != nil {
			t.Fatalf("building the old database: %v", err)
		}
	}
	legacy.Close()

	s := openForTest(t, path)

	run, err := s.Run(t.Context(), 1)
	if err != nil || run.Input != "kept?" {
		t.Errorf("run 1 = %+v, %v; want the run written before goose", run, err)
	}
	if got := tables(t, s.db); strings.Contains(got, "schema_migrations") {
		t.Errorf("tables = %q, want schema_migrations gone", got)
	}
	var applied string
	s.db.QueryRowContext(t.Context(),
		`SELECT group_concat(version_id, ',') FROM (SELECT version_id FROM goose_db_version WHERE version_id <= 1 ORDER BY id)`).Scan(&applied)
	if applied != "0,1" {
		t.Errorf("goose versions = %q, want 0,1", applied)
	}
}
