package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/fleveque/quantic-agent/internal/agent"
	"github.com/fleveque/quantic-agent/internal/provenance"
)

// State is where a run stands. The database refuses any other value (see the
// CHECK constraint in the migration).
type State string

const (
	StateRunning     State = "running"
	StateAnswered    State = "answered"    // every figure traced to a tool call
	StateUnverified  State = "unverified"  // answered, with figures no tool returned
	StateFailed      State = "failed"      // stopped by an error
	StateInterrupted State = "interrupted" // stopped by Ctrl-C or SIGTERM
)

// Run is one run as stored, with its tool calls and draft when read with Run.
type Run struct {
	ID         int64
	Kind       string
	Input      string
	Model      string
	State      State
	Error      string
	StartedAt  time.Time
	FinishedAt time.Time // zero while running
	CallCount  int

	Calls []agent.Call // in the order they were made
	Draft *Draft       // nil if the run produced nothing
}

// Draft is what a run produced and what the provenance check found in it.
type Draft struct {
	Content   string
	Truncated bool
	Findings  []provenance.Finding
	CreatedAt time.Time
}

// StartRun records a run as running and returns its id.
func (s *Store) StartRun(ctx context.Context, kind, input, model string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO runs (kind, input, model, state, started_at) VALUES (?, ?, ?, ?, ?)`,
		kind, input, model, StateRunning, s.stamp())
	if err != nil {
		return 0, fmt.Errorf("store: starting run: %w", err)
	}
	return res.LastInsertId()
}

// RecordCall appends one tool call to a run's audit log. It is called as each
// call completes, not at the end of the run, so a run that crashes or is
// stopped still leaves a record of everything it did.
func (s *Store) RecordCall(ctx context.Context, runID int64, seq int, c agent.Call) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tool_calls (run_id, seq, tool, arguments, result, failed, duration_ms, called_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		runID, seq, c.Tool, string(c.Arguments), c.Result, c.Failed, c.Duration.Milliseconds(), s.stamp())
	if err != nil {
		return fmt.Errorf("store: recording call %d of run %d: %w", seq, runID, err)
	}
	return nil
}

// Finish ends a run: its state, the error that stopped it if any, and its
// draft if it produced one. All of it is written in one transaction, so a run
// is never marked answered without its draft, or the reverse.
func (s *Store) Finish(ctx context.Context, runID int64, state State, runErr error, draft *Draft) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if draft != nil {
			findings, err := json.Marshal(draft.Findings)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO drafts (run_id, content, truncated, findings, created_at) VALUES (?, ?, ?, ?, ?)`,
				runID, draft.Content, draft.Truncated, string(findings), s.stamp()); err != nil {
				return fmt.Errorf("store: saving draft of run %d: %w", runID, err)
			}
		}

		var errText sql.NullString
		if runErr != nil {
			errText = sql.NullString{String: runErr.Error(), Valid: true}
		}
		// Only a running run can finish. Updating by id and state together
		// makes "finish twice" an error instead of a quiet overwrite.
		res, err := tx.ExecContext(ctx,
			`UPDATE runs SET state = ?, error = ?, finished_at = ? WHERE id = ? AND state = ?`,
			state, errText, s.stamp(), runID, StateRunning)
		if err != nil {
			return fmt.Errorf("store: finishing run %d: %w", runID, err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("store: run %d is not running: %w", runID, ErrNotFound)
		}
		return nil
	})
}

// Run reads one run with its calls and draft.
func (s *Store) Run(ctx context.Context, id int64) (Run, error) {
	runs, err := s.query(ctx, `WHERE r.id = ?`, id)
	if err != nil {
		return Run{}, err
	}
	if len(runs) == 0 {
		return Run{}, fmt.Errorf("store: run %d: %w", id, ErrNotFound)
	}
	r := runs[0]

	rows, err := s.db.QueryContext(ctx,
		`SELECT tool, arguments, result, failed, duration_ms FROM tool_calls WHERE run_id = ? ORDER BY seq`, id)
	if err != nil {
		return Run{}, fmt.Errorf("store: reading calls of run %d: %w", id, err)
	}
	defer rows.Close()
	for rows.Next() {
		var c agent.Call
		var args string
		var ms int64
		if err := rows.Scan(&c.Tool, &args, &c.Result, &c.Failed, &ms); err != nil {
			return Run{}, fmt.Errorf("store: reading a call of run %d: %w", id, err)
		}
		c.Arguments = json.RawMessage(args)
		c.Duration = time.Duration(ms) * time.Millisecond
		r.Calls = append(r.Calls, c)
	}
	if err := rows.Err(); err != nil {
		return Run{}, fmt.Errorf("store: reading calls of run %d: %w", id, err)
	}

	var d Draft
	var findings, created string
	err = s.db.QueryRowContext(ctx,
		`SELECT content, truncated, findings, created_at FROM drafts WHERE run_id = ?`, id).
		Scan(&d.Content, &d.Truncated, &findings, &created)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// No draft: the run failed or was stopped before answering.
	case err != nil:
		return Run{}, fmt.Errorf("store: reading draft of run %d: %w", id, err)
	default:
		if err := json.Unmarshal([]byte(findings), &d.Findings); err != nil {
			return Run{}, fmt.Errorf("store: reading findings of run %d: %w", id, err)
		}
		d.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		r.Draft = &d
	}
	return r, nil
}

// Runs lists the most recent runs, newest first, without their calls.
func (s *Store) Runs(ctx context.Context, limit int) ([]Run, error) {
	return s.query(ctx, `ORDER BY r.id DESC LIMIT ?`, limit)
}

// query reads runs with a call count; tail is the WHERE/ORDER BY part.
func (s *Store) query(ctx context.Context, tail string, args ...any) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT r.id, r.kind, r.input, r.model, r.state, r.error, r.started_at, r.finished_at,
		        (SELECT COUNT(*) FROM tool_calls c WHERE c.run_id = r.id)
		 FROM runs r `+tail, args...)
	if err != nil {
		return nil, fmt.Errorf("store: reading runs: %w", err)
	}
	defer rows.Close()

	var out []Run
	for rows.Next() {
		var r Run
		var errText, finished sql.NullString // columns that can be NULL
		var started string
		if err := rows.Scan(&r.ID, &r.Kind, &r.Input, &r.Model, &r.State, &errText, &started, &finished, &r.CallCount); err != nil {
			return nil, fmt.Errorf("store: reading a run: %w", err)
		}
		r.Error = errText.String
		r.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
		if finished.Valid {
			r.FinishedAt, _ = time.Parse(time.RFC3339Nano, finished.String)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Records returns a run's successful calls as provenance records: exactly
// what the run's manifest was built from. Re-checking a stored draft against
// them shows what the agent saw then, whatever the tools would return today.
func (r Run) Records() []provenance.Record {
	var out []provenance.Record
	for _, c := range r.Calls {
		if !c.Failed {
			out = append(out, provenance.Record{Tool: c.Tool, Result: c.Result})
		}
	}
	return out
}
