package store

import (
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fleveque/quantic-agent/internal/agent"
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

// 0002 adds phases to runs that already exist: one that answered is done; one
// that failed stays in research, so it can be resumed.
func TestPhasesAreAddedToExistingRuns(t *testing.T) {
	s := openForTest(t, filepath.Join(t.TempDir(), "agent.db"))
	ctx := t.Context()
	dir, _ := fs.Sub(migrations, "migrations")
	p, err := goose.NewProvider(goose.DialectSQLite3, s.db, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(ctx, 1); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"answered", "failed"} {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO runs (kind, input, model, state, started_at) VALUES ('research', 'q', 'm', ?, '2026-10-06T16:09:00Z')`, state); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := p.Up(ctx); err != nil {
		t.Fatal(err)
	}

	runs, err := s.Runs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[1].Phase != PhaseDone || runs[0].Phase != PhaseResearch {
		t.Errorf("runs = %+v, want the answered run done and the failed one in research", runs)
	}
}

// 0003 rebuilds runs with foreign keys switched off for its connection. The
// rows that reference runs must survive, still valid, and every connection
// in the pool, including the one goose migrated with, must enforce foreign
// keys again afterwards.
func TestRebuildingRunsKeepsItsReferences(t *testing.T) {
	s := openForTest(t, filepath.Join(t.TempDir(), "agent.db"))
	ctx := t.Context()
	dir, _ := fs.Sub(migrations, "migrations")
	p, err := goose.NewProvider(goose.DialectSQLite3, s.db, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(ctx, 2); err != nil {
		t.Fatal(err)
	}
	id, _ := s.StartRun(ctx, "research", "q", "m")
	s.RecordCall(ctx, id, 0, agent.Call{Tool: "dividend_calendar", Arguments: []byte(`{}`), Result: `{}`})
	s.Finish(ctx, id, Outcome{State: StateAnswered, Draft: &Draft{Content: "kept"}})

	if _, err := p.Up(ctx); err != nil {
		t.Fatal(err)
	}

	run, err := s.Run(ctx, id)
	if err != nil || len(run.Calls) != 1 || run.Draft == nil || run.Draft.Content != "kept" {
		t.Errorf("run after the rebuild = %+v, %v", run, err)
	}
	rows, err := s.db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		t.Error("foreign_key_check found rows pointing at no run")
	}
	rows.Close()

	// Hold several connections at once, so the pool has to hand out every
	// idle one it has, and ask each.
	var conns []*sql.Conn
	for range 4 {
		c, err := s.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		conns = append(conns, c)
	}
	for i, c := range conns {
		var on bool
		if err := c.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&on); err != nil || !on {
			t.Errorf("connection %d: foreign_keys = %v, %v; want on", i, on, err)
		}
	}
}

// Going down from 0003 can't keep no_data, which the old CHECK refuses: those
// runs become failed, as they would have been.
func TestNoDataGoesBackToFailed(t *testing.T) {
	s := openForTest(t, filepath.Join(t.TempDir(), "agent.db"))
	ctx := t.Context()
	id, _ := s.StartRun(ctx, "research", "q", "m")
	if err := s.Finish(ctx, id, Outcome{State: StateNoData}); err != nil {
		t.Fatal(err)
	}
	dir, _ := fs.Sub(migrations, "migrations")
	p, _ := goose.NewProvider(goose.DialectSQLite3, s.db, dir)

	if _, err := p.DownTo(ctx, 2); err != nil {
		t.Fatal(err)
	}

	var state, why string
	s.db.QueryRowContext(ctx, `SELECT state, error FROM runs WHERE id = ?`, id).Scan(&state, &why)
	if state != "failed" || why != "research gathered no data" {
		t.Errorf("state %q, error %q; want failed, research gathered no data", state, why)
	}
}
