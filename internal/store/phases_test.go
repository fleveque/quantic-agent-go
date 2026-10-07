package store_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/fleveque/quantic-agent/internal/agent"
	"github.com/fleveque/quantic-agent/internal/store"
)

// A run interrupted while writing resumes in the write phase, with what
// research gathered and spent.
func TestAnInterruptedRunResumesFromItsCheckpoint(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	id, _ := s.StartRun(ctx, "research", "q", "m")
	s.RecordCall(ctx, id, 0, calendar)
	if err := s.Checkpoint(ctx, id, store.PhaseWrite, 2100, agent.LimitCalls); err != nil {
		t.Fatal(err)
	}
	s.Finish(ctx, id, store.Outcome{State: store.StateInterrupted, Err: context.Canceled, Tokens: 2100})

	run, err := s.Resume(ctx, id)

	if err != nil {
		t.Fatal(err)
	}
	if run.State != store.StateRunning || run.Phase != store.PhaseWrite || run.Tokens != 2100 ||
		run.Exhausted != agent.LimitCalls || run.Error != "" || !run.FinishedAt.IsZero() || len(run.Calls) != 1 {
		t.Errorf("resumed run = %+v", run)
	}
	// And it can finish, this time with an answer.
	if err := s.Finish(ctx, id, store.Outcome{State: store.StateAnswered, Draft: &store.Draft{Content: "a"}, Tokens: 3000}); err != nil {
		t.Errorf("finishing the resumed run: %v", err)
	}
}

func TestOnlyAnUnfinishedRunCanBeResumed(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	answered, _ := s.StartRun(ctx, "research", "q", "m")
	s.Finish(ctx, answered, store.Outcome{State: store.StateAnswered, Draft: &store.Draft{Content: "a"}})
	running, _ := s.StartRun(ctx, "research", "q", "m")
	// Failed, but after writing its answer: there is nothing left to do.
	written, _ := s.StartRun(ctx, "research", "q", "m")
	s.Finish(ctx, written, store.Outcome{State: store.StateFailed, Draft: &store.Draft{Content: "a"}})

	for name, id := range map[string]int64{"answered": answered, "running": running, "written": written, "missing": 99} {
		if _, err := s.Resume(ctx, id); !errors.Is(err, store.ErrNotResumable) {
			t.Errorf("%s: err = %v, want ErrNotResumable", name, err)
		}
	}
}

// Two resumes of the same run at once: exactly one gets it.
func TestAResumeIsClaimedOnce(t *testing.T) {
	s, _ := open(t)
	ctx := t.Context()
	id, _ := s.StartRun(ctx, "research", "q", "m")
	s.Finish(ctx, id, store.Outcome{State: store.StateInterrupted})

	var wg sync.WaitGroup
	var won atomic.Int32
	for range 10 {
		wg.Go(func() {
			if _, err := s.Resume(ctx, id); err == nil {
				won.Add(1)
			} else if !errors.Is(err, store.ErrNotResumable) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Errorf("%d resumes succeeded, want exactly 1", won.Load())
	}
}
