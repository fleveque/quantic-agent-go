-- +goose Up

-- Milestone 8: a run goes through phases, research then write, and is saved
-- between them, so an interrupted run resumes from its last phase instead of
-- starting over (design §3.6). It also records what it spent.
ALTER TABLE runs ADD COLUMN phase TEXT NOT NULL DEFAULT 'research' CHECK (phase IN ('research', 'write', 'done'));
ALTER TABLE runs ADD COLUMN tokens INTEGER NOT NULL DEFAULT 0;   -- model tokens used, both phases
ALTER TABLE runs ADD COLUMN exhausted TEXT CHECK (exhausted IN ('calls', 'tokens')); -- the budget that cut research short

-- Runs from before phases that produced an answer are done. The others (failed
-- or interrupted) keep the default, research, so they can be resumed from
-- the calls they recorded.
UPDATE runs SET phase = 'done' WHERE state IN ('answered', 'unverified');

-- +goose Down
ALTER TABLE runs DROP COLUMN exhausted;
ALTER TABLE runs DROP COLUMN tokens;
ALTER TABLE runs DROP COLUMN phase;
