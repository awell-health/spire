-- Migration 004: Add per-step review metrics to agent_runs.
-- review_step identifies the kind of review step (sage-review, fix, arbiter).
-- review_round is the 1-indexed round within the review cycle.
-- Both nullable — existing rows and non-review runs stay NULL.

ALTER TABLE agent_runs ADD COLUMN review_step VARCHAR(16) AFTER artificer_verdict;
ALTER TABLE agent_runs ADD COLUMN review_round INT AFTER review_step;

CREATE INDEX idx_agent_runs_review_step ON agent_runs (review_step);
