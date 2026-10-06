package store_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fleveque/quantic-agent/internal/agent"
	"github.com/fleveque/quantic-agent/internal/provenance"
	"github.com/fleveque/quantic-agent/internal/store"
)

// open gives each test its own database file, deleted with the test's
// temporary directory. A real file, not :memory:, so WAL and multiple
// connections behave as they do in production.
func open(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.db")
	s, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

var calendar = agent.Call{
	Tool:      "dividend_calendar",
	Arguments: json.RawMessage(`{"days":10}`),
	Result:    `{"from":"2026-10-06","days":10,"stocks":[{"symbol":"MSFT","ex_dividend_date":"2026-10-08"}]}`,
	Duration:  51 * time.Millisecond,
}

var refused = agent.Call{
	Tool:      "dividend_calendar",
	Arguments: json.RawMessage(`{"days":180}`),
	Result:    "error: tools: dividend_calendar arguments: days is 180; it can be at most 120",
	Failed:    true,
}

func TestARunRoundTrips(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()

	id, err := s.StartRun(ctx, "research", "What goes ex-dividend in the next 10 days?", "qwen3.5:9b")
	if err != nil {
		t.Fatal(err)
	}
	for seq, c := range []agent.Call{refused, calendar} {
		if err := s.RecordCall(ctx, id, seq, c); err != nil {
			t.Fatal(err)
		}
	}
	draft := &store.Draft{Content: "MSFT on Oct 8, in the next 4 months.", Findings: []provenance.Finding{
		{Text: "4", Offset: 29, Kind: provenance.KindNumber, Value: "4"},
	}}
	if err := s.Finish(ctx, id, store.StateUnverified, nil, draft); err != nil {
		t.Fatal(err)
	}

	got, err := s.Run(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.StateUnverified || got.Input != "What goes ex-dividend in the next 10 days?" || got.FinishedAt.IsZero() {
		t.Errorf("run = %+v", got)
	}
	// The audit log comes back in the order it was written, exactly.
	if len(got.Calls) != 2 || got.Calls[0].Result != refused.Result || !got.Calls[0].Failed ||
		string(got.Calls[1].Arguments) != `{"days":10}` || got.Calls[1].Duration != 51*time.Millisecond {
		t.Errorf("calls = %+v", got.Calls)
	}
	if got.Draft == nil || got.Draft.Content != draft.Content || len(got.Draft.Findings) != 1 || got.Draft.Findings[0].Text != "4" {
		t.Errorf("draft = %+v", got.Draft)
	}
	// Only the successful call feeds the manifest.
	if recs := got.Records(); len(recs) != 1 || recs[0].Result != calendar.Result {
		t.Errorf("records = %+v, want the one successful call", recs)
	}
}

// A stored run can be re-checked later against exactly what the agent saw.
func TestAStoredDraftCanBeRechecked(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	id, _ := s.StartRun(ctx, "research", "q", "m")
	s.RecordCall(ctx, id, 0, calendar)
	s.Finish(ctx, id, store.StateAnswered, nil, &store.Draft{Content: "MSFT goes ex-dividend on Oct 8."})

	run, _ := s.Run(ctx, id)
	m, err := provenance.NewManifest(run.Records()...)
	if err != nil {
		t.Fatal(err)
	}
	if findings := provenance.CheckProse(run.Draft.Content, m); len(findings) != 0 {
		t.Errorf("re-check found %v", findings)
	}
}

func TestMigrationsApplyOnce(t *testing.T) {
	_, path := open(t)

	// Opening the same file again must find the schema current and change nothing.
	again, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer again.Close()
	if _, err := again.StartRun(t.Context(), "research", "q", "m"); err != nil {
		t.Errorf("schema unusable after reopening: %v", err)
	}
}

// SQLite ignores REFERENCES unless foreign keys are switched on, per
// connection. This proves the setting reached the connection that did the
// write.
func TestForeignKeysAreEnforced(t *testing.T) {
	s, _ := open(t)

	err := s.RecordCall(t.Context(), 999, 0, calendar)

	if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Errorf("err = %v, want a foreign key violation for a run that doesn't exist", err)
	}
}

func TestACallIsRecordedOnce(t *testing.T) {
	s, _ := open(t)
	id, _ := s.StartRun(t.Context(), "research", "q", "m")
	s.RecordCall(t.Context(), id, 0, calendar)

	err := s.RecordCall(t.Context(), id, 0, calendar)

	if err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("err = %v, want the same sequence number refused", err)
	}
}

// Finish writes the draft and the run's state in one transaction. If the
// second write fails, the first must not survive.
func TestFinishIsAllOrNothing(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	id, _ := s.StartRun(ctx, "research", "q", "m")

	// A state the schema's CHECK constraint refuses: the draft insert
	// succeeds, then the run update fails.
	err := s.Finish(ctx, id, store.State("bogus"), nil, &store.Draft{Content: "an answer"})
	if err == nil || !strings.Contains(err.Error(), "CHECK") {
		t.Fatalf("err = %v, want the CHECK constraint to refuse the state", err)
	}

	run, err := s.Run(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != store.StateRunning || run.Draft != nil {
		t.Errorf("after a failed Finish: state %s, draft %+v; want running and no draft", run.State, run.Draft)
	}
}

func TestARunFinishesOnce(t *testing.T) {
	s, _ := open(t)
	id, _ := s.StartRun(t.Context(), "research", "q", "m")
	if err := s.Finish(t.Context(), id, store.StateFailed, errors.New("model server unavailable"), nil); err != nil {
		t.Fatal(err)
	}

	err := s.Finish(t.Context(), id, store.StateAnswered, nil, nil)

	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound for a run that already finished", err)
	}
	run, _ := s.Run(t.Context(), id)
	if run.State != store.StateFailed || run.Error != "model server unavailable" {
		t.Errorf("run = %s %q, want the first outcome kept", run.State, run.Error)
	}
}

func TestRunsNewestFirst(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	for i := range 3 {
		id, _ := s.StartRun(ctx, "research", fmt.Sprintf("question %d", i), "m")
		for seq := range i {
			s.RecordCall(ctx, id, seq, calendar)
		}
	}

	runs, err := s.Runs(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].Input != "question 2" || runs[0].CallCount != 2 || runs[1].Input != "question 1" {
		t.Errorf("runs = %+v, want questions 2 and 1 with their call counts", runs)
	}
	if _, err := s.Run(ctx, 99); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Run(99) err = %v, want ErrNotFound", err)
	}
}

// Several goroutines writing at once: SQLite allows one writer at a time, and
// the busy timeout makes the others wait their turn instead of failing.
func TestConcurrentWrites(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	id, _ := s.StartRun(ctx, "research", "q", "m")

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for seq := range 20 {
		wg.Go(func() { errs <- s.RecordCall(ctx, id, seq, calendar) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}

	run, _ := s.Run(ctx, id)
	if len(run.Calls) != 20 {
		t.Errorf("recorded %d calls, want 20", len(run.Calls))
	}
}
