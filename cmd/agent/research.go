package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/fleveque/quantic-agent/internal/agent"
	"github.com/fleveque/quantic-agent/internal/mcp"
	"github.com/fleveque/quantic-agent/internal/provenance"
	"github.com/fleveque/quantic-agent/internal/store"
	"github.com/fleveque/quantic-agent/internal/tools"
)

// defaultDBPath follows the XDG base directory convention: run history is
// "state", data that persists between runs but isn't configuration.
func defaultDBPath() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "agent.db"
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "quantic-agent", "agent.db")
}

func openStore(ctx context.Context, path string) (*store.Store, error) {
	// 0o700: the run history is the operator's alone.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating the database directory: %w", err)
	}
	return store.Open(ctx, path)
}

type researchConfig struct {
	model    agent.Model
	mcpURL   string
	store    *store.Store
	question string // a new run's question
	resume   int64  // or a run to resume
	fail     func(error) int
}

// research answers one question in two phases, research then write (design
// §3.1), recording the run as it goes: the run when it starts, each tool call
// as it completes, a checkpoint when research is done, and the outcome and
// draft when it ends. A resumed run starts at the phase it had reached: one
// interrupted while writing doesn't call a single tool again.
func research(ctx context.Context, stdout, stderr io.Writer, cfg researchConfig) int {
	run, err := startOrResume(ctx, stderr, cfg)
	if err != nil {
		return cfg.fail(err)
	}
	gathered := agent.Research{Calls: run.Calls, Tokens: run.Tokens, Exhausted: run.Exhausted}

	// The outcome must be saved even when ctx has been cancelled (Ctrl-C):
	// that is exactly when "interrupted" needs recording. WithoutCancel keeps
	// ctx's values but drops its cancellation; the timeout bounds the save.
	finish := func(o store.Outcome) {
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := cfg.store.Finish(saveCtx, run.ID, o); err != nil {
			fmt.Fprintln(stderr, "agent: saving the run:", err)
		}
		fmt.Fprintf(stderr, "agent: run %d %s\n", run.ID, o.State)
		if o.Draft == nil {
			fmt.Fprintf(stderr, "agent: to carry on from where it stopped: agent -resume %d\n", run.ID)
		}
	}
	stopped := func(err error, tokens int) int {
		finish(store.Outcome{State: outcome(err), Err: err, Tokens: tokens})
		return cfg.fail(err)
	}

	if run.Phase == store.PhaseResearch {
		gathered, err = researchPhase(ctx, stderr, cfg, run.ID, run.Input, gathered)
		if err != nil {
			return stopped(err, gathered.Tokens)
		}
		if gathered.Exhausted != "" {
			fmt.Fprintf(stderr, "agent: research stopped when its %s budget ran out; writing from what it gathered\n", gathered.Exhausted)
		}
		if err := cfg.store.Checkpoint(ctx, run.ID, store.PhaseWrite, gathered.Tokens, gathered.Exhausted); err != nil {
			return stopped(err, gathered.Tokens)
		}
	}

	w := &agent.Writer{Model: cfg.model}
	draft, err := w.Write(ctx, run.Input, gathered)
	tokens := gathered.Tokens + draft.Tokens
	if err != nil {
		return stopped(err, tokens)
	}

	fmt.Fprintln(stdout, draft.Text)
	if draft.Truncated {
		fmt.Fprintln(stderr, "agent: the answer was truncated: the model hit its token limit")
	}
	findings, err := check(draft.Text, gathered.Calls)
	if err != nil {
		return stopped(err, tokens)
	}
	saved := &store.Draft{Content: draft.Text, Truncated: draft.Truncated, Findings: findings}
	if len(findings) > 0 {
		reportFindings(stderr, findings)
		finish(store.Outcome{State: store.StateUnverified, Draft: saved, Tokens: tokens})
		return exitUnverified
	}
	finish(store.Outcome{State: store.StateAnswered, Draft: saved, Tokens: tokens})
	return exitOK
}

// startOrResume records a new run, or claims the one being resumed.
func startOrResume(ctx context.Context, stderr io.Writer, cfg researchConfig) (store.Run, error) {
	if cfg.resume != 0 {
		run, err := cfg.store.Resume(ctx, cfg.resume)
		if err != nil {
			return store.Run{}, err
		}
		fmt.Fprintf(stderr, "agent: resuming run %d in its %s phase, with %d tool call(s) recorded\n", run.ID, run.Phase, len(run.Calls))
		return run, nil
	}
	modelName := ""
	if named, ok := cfg.model.(interface{ Model() string }); ok {
		modelName = named.Model()
	}
	id, err := cfg.store.StartRun(ctx, "research", cfg.question, modelName)
	if err != nil {
		return store.Run{}, err
	}
	return store.Run{ID: id, Input: cfg.question, Phase: store.PhaseResearch}, nil
}

// researchPhase lets the model call Quantic's tools, recording each call as
// it completes, and continuing from prior when resuming.
func researchPhase(ctx context.Context, stderr io.Writer, cfg researchConfig, runID int64, question string, prior agent.Research) (agent.Research, error) {
	// The token, if any, comes only from the environment: a secret on the
	// command line ends up in shell history and in ps.
	server := mcp.New(cfg.mcpURL, os.Getenv("QUANTIC_MCP_TOKEN"))
	server.OnRetry = func(method string, retry int, wait time.Duration) {
		fmt.Fprintf(stderr, "agent: Quantic's rate limit; retry %d of %s in %s\n", retry, method, wait.Round(100*time.Millisecond))
	}
	if _, err := server.Initialize(ctx); err != nil {
		return prior, err
	}

	r := &agent.Researcher{
		Model:  cfg.model,
		Server: server,
		Tools:  []tools.Tool{tools.DividendCalendar},
		Record: func(ctx context.Context, seq int, c agent.Call) error {
			return cfg.store.RecordCall(ctx, runID, seq, c)
		},
	}
	gathered, err := r.Research(ctx, question, prior)
	for _, c := range gathered.Calls[len(prior.Calls):] {
		fmt.Fprintf(stderr, "tool %s %s → %s (%s)\n", c.Tool, c.Arguments, callSummary(c), c.Duration.Round(time.Millisecond))
	}
	return gathered, err
}

// outcome is the state a run ends in when err stopped it.
func outcome(err error) store.State {
	if errors.Is(err, context.Canceled) {
		return store.StateInterrupted
	}
	return store.StateFailed
}

// check verifies every figure in text against the successful calls' results
// (design N1).
func check(text string, calls []agent.Call) ([]provenance.Finding, error) {
	var records []provenance.Record
	for _, c := range calls {
		if !c.Failed {
			records = append(records, provenance.Record{Tool: c.Tool, Result: c.Result})
		}
	}
	m, err := provenance.NewManifest(records...)
	if err != nil {
		return nil, err
	}
	return provenance.CheckProse(text, m), nil
}

func reportFindings(w io.Writer, findings []provenance.Finding) {
	fmt.Fprintf(w, "agent: %d figure(s) in the answer came from no tool result:\n", len(findings))
	for _, f := range findings {
		fmt.Fprintf(w, "  %s\n", f)
	}
}

// showRuns lists the most recent runs.
func showRuns(ctx context.Context, stdout, stderr io.Writer, db *store.Store) int {
	runs, err := db.Runs(ctx, 20)
	if err != nil {
		fmt.Fprintln(stderr, "agent:", err)
		return exitFailed
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tSTARTED\tSTATE\tPHASE\tCALLS\tTOKENS\tQUESTION")
	for _, r := range runs {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%d\t%d\t%s\n", r.ID, r.StartedAt.Local().Format("2006-01-02 15:04"),
			r.State, r.Phase, r.CallCount, r.Tokens, shorten(r.Input, 60))
	}
	tw.Flush()
	return exitOK
}

// showRun prints one run and re-checks its answer against the tool results
// it was given, as stored: the audit log answering "where did this come from?"
// after the fact (design N3).
func showRun(ctx context.Context, stdout, stderr io.Writer, db *store.Store, id int64) int {
	r, err := db.Run(ctx, id)
	if err != nil {
		fmt.Fprintln(stderr, "agent:", err)
		return exitFailed
	}
	fmt.Fprintf(stdout, "run %d · %s · %s · %s\n", r.ID, r.State, r.Model, r.StartedAt.Local().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(stdout, "question: %s\n", r.Input)
	fmt.Fprintf(stdout, "phase: %s · %d tokens\n", r.Phase, r.Tokens)
	if r.Exhausted != "" {
		fmt.Fprintf(stdout, "research stopped early: its %s budget ran out\n", r.Exhausted)
	}
	if r.Error != "" {
		fmt.Fprintf(stdout, "error: %s\n", r.Error)
	}
	for i, c := range r.Calls {
		fmt.Fprintf(stdout, "call %d: %s %s → %s\n", i, c.Tool, c.Arguments, callSummary(c))
	}
	if r.Draft == nil {
		return exitOK
	}
	fmt.Fprintf(stdout, "\n%s\n\n", r.Draft.Content)

	findings, err := check(r.Draft.Content, r.Calls)
	if err != nil {
		fmt.Fprintln(stderr, "agent:", err)
		return exitFailed
	}
	if len(findings) == 0 {
		fmt.Fprintln(stdout, "provenance, re-checked now: every figure traces to a stored tool result")
		return exitOK
	}
	reportFindings(stdout, findings)
	return exitUnverified
}

func shorten(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
