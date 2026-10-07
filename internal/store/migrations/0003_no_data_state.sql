-- +goose NO TRANSACTION
-- +goose Up

-- A run whose research gathered nothing ends in its own state, no_data,
-- instead of writing an answer about nothing. Adding a value to a CHECK
-- constraint means rebuilding the table: SQLite can't alter a constraint.
--
-- This is SQLite's documented recipe (sqlite.org/lang_altertable.html,
-- section 7). tool_calls and drafts reference runs, so dropping runs would
-- fail with foreign keys on; they must be switched off, and that is only
-- possible outside a transaction. Hence NO TRANSACTION, and an explicit
-- BEGIN/COMMIT around the rebuild. goose runs a file like this on one
-- connection, so the PRAGMAs and the statements share it.
PRAGMA foreign_keys = OFF;
BEGIN;
CREATE TABLE runs_new (
    id          INTEGER PRIMARY KEY,
    kind        TEXT    NOT NULL,
    input       TEXT    NOT NULL,
    model       TEXT    NOT NULL,
    state       TEXT    NOT NULL CHECK (state IN ('running', 'answered', 'unverified', 'failed', 'interrupted', 'no_data')),
    error       TEXT,
    started_at  TEXT    NOT NULL,
    finished_at TEXT,
    phase       TEXT    NOT NULL DEFAULT 'research' CHECK (phase IN ('research', 'write', 'done')),
    tokens      INTEGER NOT NULL DEFAULT 0,
    exhausted   TEXT    CHECK (exhausted IN ('calls', 'tokens'))
);
INSERT INTO runs_new (id, kind, input, model, state, error, started_at, finished_at, phase, tokens, exhausted)
    SELECT id, kind, input, model, state, error, started_at, finished_at, phase, tokens, exhausted FROM runs;
DROP TABLE runs;
ALTER TABLE runs_new RENAME TO runs;
COMMIT;
PRAGMA foreign_keys = ON;

-- +goose Down
PRAGMA foreign_keys = OFF;
BEGIN;
-- The old CHECK has no no_data: those runs go back to what they were before
-- this migration, failed.
UPDATE runs SET state = 'failed', error = coalesce(error, 'research gathered no data') WHERE state = 'no_data';
CREATE TABLE runs_old (
    id          INTEGER PRIMARY KEY,
    kind        TEXT    NOT NULL,
    input       TEXT    NOT NULL,
    model       TEXT    NOT NULL,
    state       TEXT    NOT NULL CHECK (state IN ('running', 'answered', 'unverified', 'failed', 'interrupted')),
    error       TEXT,
    started_at  TEXT    NOT NULL,
    finished_at TEXT,
    phase       TEXT    NOT NULL DEFAULT 'research' CHECK (phase IN ('research', 'write', 'done')),
    tokens      INTEGER NOT NULL DEFAULT 0,
    exhausted   TEXT    CHECK (exhausted IN ('calls', 'tokens'))
);
INSERT INTO runs_old (id, kind, input, model, state, error, started_at, finished_at, phase, tokens, exhausted)
    SELECT id, kind, input, model, state, error, started_at, finished_at, phase, tokens, exhausted FROM runs;
DROP TABLE runs;
ALTER TABLE runs_old RENAME TO runs;
COMMIT;
PRAGMA foreign_keys = ON;
