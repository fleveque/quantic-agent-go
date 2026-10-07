# Lesson 07 — A memory that can be audited

**Milestone 7** — the agent remembers. Every run, every tool call and every answer goes into SQLite as
it happens, and any past answer can be re-checked against exactly the data it was given. This milestone
brought the project's first dependency, the difference between a database and a connection, and the
moment my tests wrote fake runs into my real home directory.

*Also readable as a [formatted page](https://claude.ai/artifact/5KnF36mtBBgY3uXfiQ4q27). For the code itself, file by file, see the
[walkthrough](https://claude.ai/artifact/AAZi4mJ3GCd6zz6X6LYVM7).*

---

## The first dependency

Until now `go.mod` had no `require` lines at all: everything came from the standard library. The design
chose `modernc.org/sqlite`, a translation of SQLite's C source into Go, because it keeps the agent a
single static binary with no C compiler involved.

```
$ go get modernc.org/sqlite@latest
go: added modernc.org/sqlite v1.60.1
...
```

Nine more modules came with it, marked `// indirect`: my code never imports them; the driver does. The
driver itself was marked indirect too until the first file imported it, and then `go mod tidy` moved it
to a `require` line of its own. That's `go.mod` telling you what you *use* versus what your dependencies
use. `go.sum` records a checksum for
each, so a later download that doesn't match is refused.

The reason for picking this driver is a property, and properties rot unless something checks them. So
CI now also builds with cgo switched off:

```
$ CGO_ENABLED=0 go build -o agent ./cmd/agent && file agent
agent: ELF 64-bit LSB executable, x86-64, version 1 (SYSV), statically linked, Go
```

If a future dependency needs a C compiler, that step fails on the pull request that adds it.

## A blank import with a job

```go
import _ "modernc.org/sqlite" // registers the "sqlite" driver with database/sql
```

`database/sql` is the standard library's interface to every SQL database; it doesn't know any of them.
A driver package registers itself in its `init` function, which runs when the package is imported, and
the blank `_` imports it for exactly that side effect. Then `sql.Open("sqlite", dsn)` finds it by name.
It's the same trick as `import _ "embed"` from lesson 05, and the first time I'd met an import that
exists only to run code.

## A *sql.DB is not a connection

This was the lesson of the milestone. `sql.Open` returns a `*sql.DB`, and the name lies a little: it's a
*pool* of connections, opened and closed as needed. That matters for SQLite, because some of its
settings are per connection:

```sql
PRAGMA foreign_keys = ON;   -- without it, SQLite ignores REFERENCES entirely
```

Run that once after `sql.Open` and it applies to whichever connection happened to execute it, not to the
next one the pool hands out. So the settings go in the connection string, which every new connection
reads:

```go
dsn := "file:" + path +
	"?_pragma=foreign_keys(1)" +
	"&_pragma=journal_mode(WAL)" +
	"&_pragma=busy_timeout(5000)"
```

Each setting has a test, and each test was checked by taking its setting away. Without `foreign_keys`,
recording a tool call for a run that doesn't exist simply succeeds:

```
--- FAIL: TestForeignKeysAreEnforced (0.00s)
    store_test.go:128: err = <nil>, want a foreign key violation for a run that doesn't exist
```

Without `busy_timeout`, twenty goroutines recording calls at once collide on SQLite's single write lock,
in five runs out of five:

```
store_test.go:223: store: recording call 0 of run 1: database is locked (5) (SQLITE_BUSY)
```

With it, a writer that finds the lock taken waits up to five seconds for its turn.

## Migrations, embedded

The schema lives in `internal/store/migrations/0001_runs_calls_drafts.sql`, compiled into the binary
with `//go:embed`. On `Open`, each file whose number isn't yet in `schema_migrations` runs inside its own
transaction, together with the row that records it. Either a migration applied completely and is
recorded, or nothing happened. Remove the "already applied" check, and the second `Open` of the same
file tells you why it exists:

```
store: applying migration migrations/0001_runs_calls_drafts.sql: SQL logic error: table runs already exists (1)
```

I'd only ever used migrations through a framework, Rails' or Ecto's. Writing the forty lines myself
made it obvious how little magic there is: a table of applied versions, files sorted by name, a
transaction each.

## All or nothing

When a run ends, two things are written: its draft and its final state. If only one of them survives,
the history lies: a run "answered" with no answer, or an answer under a run still "running". So both go
in one transaction:

```go
func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	...
	defer tx.Rollback() // does nothing after a successful Commit
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
```

The `defer tx.Rollback()` is the Go idiom. Rolling back a committed transaction is a no-op, so one
deferred call covers every early return and even a panic. The test makes the second write fail on
purpose, with a state the schema's `CHECK` constraint refuses, and confirms the draft didn't survive.
Without the transaction, it did:

```
after a failed Finish: state running, draft &{Content:an answer ...}; want running and no draft
```

## Write it as it happens

The audit log (N3) is only useful if it's complete, and a run that crashes is exactly the one you'll want
to read later. So tool calls aren't saved at the end of a run; each is written the moment it completes,
through a hook the loop calls:

```go
Record func(ctx context.Context, seq int, c Call) error
```

If recording fails, the run stops. A call that can't be logged is a call that can't be audited, and the
design says every call is.

## Saving after Ctrl-C

When you press Ctrl-C, the run's context is cancelled, and every database call made with it fails. That
is precisely when the run's outcome, "interrupted", needs saving. Go 1.21 added the answer:

```go
saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
```

`WithoutCancel` makes a context that keeps the parent's values but ignores its cancellation; the timeout
then bounds the save. Swapping it for the plain `ctx` leaves the interrupted run marked "running"
forever. A test catches that.

## My tests wrote to my home directory

The research tests from milestone 5 don't pass `-db`, because `-db` didn't exist then. Once the agent
started recording runs, they quietly used the default: my real `~/.local/state/quantic-agent/agent.db`.
I found it by listing runs:

```
RUN  STARTED           STATE       CALLS  QUESTION
3    2026-10-06 16:08  unverified  1      q
2    2026-10-06 16:08  failed      0      q
1    2026-10-06 16:08  answered    1      Which companies go ex-dividend in the next 10 days?
```

Three fake runs in the real history. The fix is `TestMain`, a function a test file can declare that runs
once, before any test in the package. It points `XDG_STATE_HOME` at a temporary directory, so no test in
`cmd/agent` can reach the real one, including tests written later by someone who forgets `-db`. After
400 runs of the suite, the real directory still doesn't exist. Defaults that point at real data are a
hazard in tests; isolate the whole package, not each test.

## The payoff

```
$ agent -runs
RUN  STARTED           STATE        CALLS  QUESTION
3    2026-10-06 16:09  interrupted  1      What goes ex-dividend this week?
2    2026-10-06 16:09  unverified   1      Which stocks go ex-dividend in the next six months?
1    2026-10-06 16:09  answered     1      Which companies go ex-dividend in the next 10 days?

$ agent -run 2
run 2 · unverified · qwen3.5:9b · 2026-10-06 16:09:20
question: Which stocks go ex-dividend in the next six months?
call 0: dividend_calendar {"days":120} → 1356 bytes

The data provided covers the dividend calendar for the next 120 days (approximately 4 months) ...

agent: 1 figure(s) in the answer came from no tool result:
  "4" (number 4)
```

Run 2's answer is re-checked against the calendar as it was stored that afternoon, not as Quantic would
return it now. Tomorrow's calendar will be different; the record of what the agent saw won't be. That's
what makes an answer auditable after the fact.

## What I'm taking into milestone 8

- `*sql.DB` is a pool. Per-connection settings go in the DSN.
- One migration, one transaction, one row in `schema_migrations`.
- `defer tx.Rollback()` after `BeginTx`: one line covers every way out.
- Write audit records as events happen, and stop if you can't.
- `context.WithoutCancel` for the bookkeeping that must outlive a cancellation, with its own timeout.
- `TestMain` to keep a whole package's tests away from real state.
- Milestone 8 grows the loop: budgets, retries with backoff, and phases checkpointed in these tables so
  an interrupted run resumes instead of starting over.

---

**Previous:** [Lesson 06 — Every figure has a source](06-every-figure-has-a-source.md) ·
**Next:** [Lesson 08 — A loop that can stop](08-a-loop-that-can-stop.md)
