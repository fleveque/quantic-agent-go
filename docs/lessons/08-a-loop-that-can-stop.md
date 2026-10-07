# Lesson 08 — A loop that can stop

**Milestone 8** — the research loop grows up. Migrations move to a library, after I'd written them by
hand. The agent researches within a budget, then writes in a separate step that can't call tools. It
waits out Quantic's rate limit, and saves itself between the two steps, so a run stopped halfway
carries on later instead of starting over. Along the way: a library that didn't do what its design
said it did, and a measurement that didn't come out the way the design expected.

*Also readable as a [formatted page](https://claude.ai/artifact/UnhJ2YEq4FN36mFYnxCXJh). For the code itself, file by file, see the
[walkthrough](https://claude.ai/artifact/45CMTM8BqLHk7KQRo19JGh).*

---

## Migrations, by hand and with goose

In milestone 7 I wrote migrations myself: forty lines that kept a table of applied versions and ran each
new SQL file in a transaction. Then I asked what Go projects usually do. The answer was a library,
`golang-migrate/migrate` or `pressly/goose`, so this milestone switches to goose, to have done both.

| | By hand (milestone 7) | goose (milestone 8) |
|---|---|---|
| Bookkeeping table | `schema_migrations`, mine | `goose_db_version`, goose's |
| Files | `0001_*.sql`, plain SQL | the same files, with `-- +goose Up` and `-- +goose Down` |
| Undo a migration | no | `Down`, `DownTo(version)` |
| Embedded in the binary | `//go:embed`, then my loop | `//go:embed`, then `goose.NewProvider(..., fs)` |
| A command-line tool | no | `goose status`, `goose up`, ... |
| Two processes migrating at once | collide | **also collide** (see below) |
| Code in `internal/store` | ~40 lines | ~20, plus the two things below |

The library is a `Provider` built from a dialect, the `*sql.DB` and an `fs.FS`:

```go
dir, err := fs.Sub(migrations, "migrations")
p, err := goose.NewProvider(goose.DialectSQLite3, s.db, dir)
if _, err := p.Up(ctx); err != nil { ... }
```

`fs.Sub` is the standard library's way of saying "this subdirectory of that filesystem, as a
filesystem of its own": the embedded files live under `migrations/`, and goose wants them at the root.

### Every migration must be able to go back down

A `Down` section is code like any other, so it gets a test: take every migration down to version 0,
check only goose's own table is left, and come back up. Deleting one `DROP TABLE` line shows why:

```
--- FAIL: TestMigrationsGoDownAndUpAgain (0.00s)
    migrate_test.go:61: after going down, tables = "goose_db_version tool_calls", want only goose_db_version
    migrate_test.go:65: up again: partial migration error (type:sql,version:1): SQL logic error: table tool_calls already exists (1)
```

### Databases that came before the library

A database created by milestone 7 has its history in `schema_migrations`, which goose doesn't read.
Opened as it is, goose sees version 0 and runs `0001` again:

```
Open: store: migrating: partial migration error (type:sql,version:1): SQL logic error: table runs already exists (1)
```

So `Open` hands it over once: in one transaction, create goose's table, copy the versions across, drop
the old table. Every project that adopts a migration library on a live database has this step;
the library can't know what came before it.

## The library didn't lock

The design said goose would bring "locking across processes". In milestone 7 I'd measured my own
version: two processes opening a *new* database at the same moment sometimes collided. I expected goose
to fix that. Before relying on it, I read its `lock` package: lockers for Postgres and MySQL, nothing
for SQLite. Those two are servers, and a server can hold a lock for whoever asks. SQLite is a file.

Then I measured, with real processes:

| | Failed opens |
|---|---|
| Hand-written migrations | 6–7 of 40 |
| goose, no lock | 11–13 of 40 |
| goose, with a file lock | 0 of 200 |

```
round 0: exit status 1: store: migrating: partial migration error (type:sql,version:1): SQL logic error: table runs already exists (1)
round 2: exit status 1: store: migrating: failed to initialize: database is locked (5) (SQLITE_BUSY)
```

Worse than mine, not better. The fix is the oldest trick there is: a lock file next to the database,
`agent.db.lock`, held with `flock(2)` while migrating. The kernel releases it when the process ends,
however it ends, so a crash can't leave it held.

```go
err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
```

`LOCK_NB` means "don't wait": the call fails at once with `EWOULDBLOCK` if someone else holds it, and
my code waits 10ms and tries again, checking `ctx` each time. A *blocking* `flock` would wait in the
kernel, where Ctrl-C can't reach it through a context.

### A test that starts processes

The race is between processes, so the test needs processes. Go's trick: the test binary runs itself.
`os.Args[0]` is the compiled test binary, and `-test.run` selects one test in it:

```go
cmd := exec.Command(os.Args[0], "-test.run=^TestHelperOpen$")
cmd.Env = append(os.Environ(), helperEnv+"="+path)
```

`TestHelperOpen` skips itself unless that variable is set, so in a normal run it does nothing; as a
child, it opens the database and exits. 32 processes, 8 rounds of 4, about a quarter of a second.

## Waiting out a rate limit

Anonymous callers get 60 requests a minute from Quantic. I read the server's code to see what "a
minute" means: fixed windows, counted from the clock's minute, and no `Retry-After` header. So a refused
client may have to wait up to a full minute, and nothing tells it how long.

The usual answer is exponential backoff: wait 1s, then 2, 4, 8, 16, capped at 30. Plus **jitter**, a
random part, so clients refused together don't all come back together:

```go
func (b Backoff) wait(n int) time.Duration {
	d := b.Base << (n - 1) // Base × 2ⁿ⁻¹
	if d > b.Max || d <= 0 { // d <= 0: the shift overflowed
		d = b.Max
	}
	return d/2 + rand.N(d/2+1)
}
```

This is "equal jitter": always at least half the step. And that's where my first version was wrong. I
had picked 8 attempts because 1+2+4+8+16+30+30 is 91 seconds, more than a minute. But with jitter, the
waits can add up to half of that. The test that checks the *shortest* schedule caught it:

```
--- FAIL: TestDefaultBackoffOutlastsAMinute (0.00s)
    backoff_test.go:36: the retries can give up after 45.5s of waiting; a refused client may need a minute
```

Nine attempts: at least 60.5s, at most 121.

`d <= 0` is there because `<<` doesn't stop at the edge: shift a `time.Duration` (an `int64`) far
enough, and it becomes zero or negative. Waiting must also end with the run, so it's a timer in a
`select`, not a `time.Sleep`:

```go
timer := time.NewTimer(wait)
select {
case <-ctx.Done():
	timer.Stop()
	return message{}, fmt.Errorf("mcp: %s: waiting to retry: %w", msg.Method, ctx.Err())
case <-timer.C:
}
```

The test sets a one-hour wait, cancels as it starts, and checks the call returns at once.

Only `429` is retried. The agent's tools only read, so sending a request twice can't do anything twice;
for anything else, a retry a few seconds later would just fail again.

## Running out is not failing

Milestone 5's loop returned `ErrTooManyCalls` when the model kept asking. The design says otherwise:
running out of budget is a normal end, and writing goes ahead with what was gathered. So the error
became a field:

```go
type Research struct {
	Calls     []Call
	Tokens    int
	Exhausted Limit // "" when the model finished on its own
}
```

`Limit` is `type Limit string` with two constants, `LimitCalls` and `LimitTokens`: a string the
compiler won't let me mix up with any other string.

Two small things I used for the first time. `cmp.Or` returns its first argument that isn't the zero
value, which is exactly "0 means the default":

```go
Calls: cmp.Or(r.Budget.Calls, DefaultBudget.Calls),
```

And tokens: Ollama reports `prompt_eval_count` and `eval_count` per call. The prompt count includes the
whole conversation again every turn, so the sum is the work the GPU did, not the length of the
conversation. A one-calendar question used 1,800 to 3,500 tokens; the budget is 16,000.

## Two phases, measured

The design's core idea is that research and writing are different jobs. Research gets tools and
freedom. Writing gets the question and the data, and *no tools*: it can't fetch, so it can't wander.
Milestone 8 builds that: `Researcher.Research`, then `Writer.Write`.

I measured it against milestone 7's single loop, on the same three questions, three runs each, against
live data, within four minutes so every run saw the same calendar:

| | Answers with every figure traced |
|---|---|
| Milestone 7, one loop | 6 of 9 |
| Milestone 8, writer sees the data only | 6 of 9 |
| ... without the "don't derive figures" sentence | 7 of 9 |
| ... writer sees the research conversation instead | 6 of 9 |

No winner. Nine runs can't separate them, and they fail in different ways. What the runs did show is
more useful than the scores:

- The writer said "ten stocks" three times. There are nine. The validator didn't notice, because it
  reads digits, and "ten" isn't one. Milestone 7's "four months" had slipped through the same way.
- The writer doesn't know what day it is. One answer worked from "the current date (October 2023)".
- The version that saw the research conversation read most like milestone 7, and twice told the reader
  to buy before the ex-dividend date. That's the sentence I removed from the tool's description in
  milestone 5, coming back through the model.

So the writer stays as designed, and the first two findings are the next things to fix. All four
runs are in [`docs/benchmarks/2026-10-07-writer/`](../benchmarks/2026-10-07-writer/README.md).

## Checkpoints, and claiming a run

A run is saved when research ends: `runs.phase` goes from `research` to `write`. Tool calls were
already saved as they happened (milestone 7). So `agent -resume N` knows where to start:

- stopped while **writing**: write from the stored calls. No tool is called again.
- stopped during **research**: replay the stored calls to the model, as if it had just made them, and
  carry on with what's left of the budget.

Who gets to resume? If I run `-resume 5` twice at once, only one should get the run. One `UPDATE`
does it:

```sql
UPDATE runs SET state = 'running', error = NULL, finished_at = NULL
 WHERE id = ? AND state IN ('interrupted', 'failed') AND phase != 'done'
```

SQLite runs one write at a time. The first `UPDATE` changes the row; the second finds it already
`running`, matches nothing, and `RowsAffected` is 0. No lock, no read-then-write gap: the condition
*is* the claim. The test starts ten goroutines on the same run: exactly one wins. Drop the condition
and all ten do.

### The slice that wasn't mine

Resuming passes the stored calls in, and research appends new ones. My first thought was
`research.Calls = prior.Calls`, then append. But a slice is a view of an array, and `append` writes
into that array's spare capacity if it has any, the caller's array. The caller would never see it in
its own slice's length, which is why my first test, checking `len(prior.Calls)`, passed either way. The
test now gives the slice spare room and looks past its end:

```
--- FAIL: TestResearchResumesFromRecordedCalls (0.00s)
    loop_test.go:296: Research wrote dividend_calendar into the caller's backing array
```

`slices.Clone(prior.Calls)` gives research a copy of its own. In Elixir this can't happen; lists are
immutable. In Ruby, `+=` makes a new array and `<<` mutates in place; Go's `append` is either,
depending on capacity, which you usually can't see.

## The payoff

A real run, stopped with Ctrl-C while the model was writing:

```
$ agent -research "Which companies go ex-dividend in the next 10 days?"
tool dividend_calendar {"days":10} → 810 bytes (44ms)
                                 # Ctrl-C a second later (sent as kill -INT from a script)
agent: run 1 interrupted
agent: to carry on from where it stopped: agent -resume 1
agent: stopped; the request in flight was cancelled
$ nvidia-smi --query-gpu=utilization.gpu --format=csv,noheader   # two seconds later
0 %
$ agent -runs
RUN  STARTED           STATE        PHASE  CALLS  TOKENS  QUESTION
1    2026-10-07 10:33  interrupted  write  1      1355    Which companies go ex-dividend in the next 10 days?
```

Then resumed, with the MCP address pointed at nothing, to prove it doesn't need Quantic any more:

```
$ agent -resume 1 -mcp http://127.0.0.1:1/mcp
agent: resuming run 1 in its write phase, with 1 tool call(s) recorded
Within the 10-day period starting from 2026-10-07, the following companies go ex-dividend:

*   **Microsoft (MSFT)**: Ex-dividend date is 2026-10-08; Sector: Technology; Frequency: quarterly.
*   **Johnson & Johnson (JNJ)**: Ex-dividend date is 2026-10-08; Sector: Healthcare; Frequency: quarterly.
*   **Apple Inc. (AAPL)**: Ex-dividend date is 2026-10-09; Sector: Technology; Frequency: quarterly.
*   **Iberdrola (IBE.MC)**: Ex-dividend date is 2026-10-10; Sector: Utilities; Frequency: semi_annual.

Companies listed with ex-dividend dates of 2026-10-16 (Procter & Gamble) and 2026-10-17 (Coca-Cola) occur outside the specified 10-day window. ...
agent: run 1 answered
$ agent -runs
RUN  STARTED           STATE     PHASE  CALLS  TOKENS  QUESTION
1    2026-10-07 10:33  answered  done   1      1980    Which companies go ex-dividend in the next 10 days?
```

The GPU is free the moment I stop the agent, and nothing is lost. That's the shared-desktop
requirement from the start of the project, working.

## What I'm taking into milestone 9

- Check a library's claims in its source before building on them. Then measure.
- A test can start real processes by running its own binary: `os.Args[0]` and `-test.run`.
- Backoff with jitter: test the *shortest* schedule, not the longest.
- Waits go in a `select` with `ctx.Done()`, never a bare `time.Sleep`.
- Running out of budget is a result, not an error.
- A conditional `UPDATE` with `RowsAffected` is a claim; no lock needed.
- `append` can write into someone else's array. `slices.Clone` when the slice isn't yours.
- A design can be right and still not be measurably better yet. Write down what the measurement said.

---

**Previous:** [Lesson 07 — A memory that can be audited](07-a-memory-that-can-be-audited.md) ·
**Next:** Lesson 09 — the worker pool *(not written yet)*
