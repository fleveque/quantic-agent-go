-- A run is one task the agent attempted: for now, one -research question.
CREATE TABLE runs (
    id          INTEGER PRIMARY KEY,
    kind        TEXT    NOT NULL,                 -- 'research'
    input       TEXT    NOT NULL,                 -- the question
    model       TEXT    NOT NULL,
    state       TEXT    NOT NULL CHECK (state IN ('running', 'answered', 'unverified', 'failed', 'interrupted')),
    error       TEXT,                             -- why it failed, when it did
    started_at  TEXT    NOT NULL,                 -- RFC 3339, UTC
    finished_at TEXT
);

-- The audit log (design N3): every tool call a run made, in order, with the
-- exact result the model was shown. A draft's figures are checked against
-- these rows, so they are never edited after they are written.
CREATE TABLE tool_calls (
    id          INTEGER PRIMARY KEY,
    run_id      INTEGER NOT NULL REFERENCES runs (id),
    seq         INTEGER NOT NULL,                 -- 0, 1, 2... within the run
    tool        TEXT    NOT NULL,
    arguments   TEXT    NOT NULL,                 -- JSON, as the model sent it
    result      TEXT    NOT NULL,                 -- the tool's output, or the error the model was shown
    failed      INTEGER NOT NULL CHECK (failed IN (0, 1)),
    duration_ms INTEGER NOT NULL,
    called_at   TEXT    NOT NULL,
    UNIQUE (run_id, seq)
);

-- What a run produced, and what the provenance check made of it.
CREATE TABLE drafts (
    id          INTEGER PRIMARY KEY,
    run_id      INTEGER NOT NULL UNIQUE REFERENCES runs (id),
    content     TEXT    NOT NULL,
    truncated   INTEGER NOT NULL CHECK (truncated IN (0, 1)),
    findings    TEXT    NOT NULL,                 -- JSON array: figures no tool call accounts for
    created_at  TEXT    NOT NULL
);
