-- Migration 002: Add phase column to agent_runs.
-- Phase tracks which formula phase an agent run belongs to:
-- 'implement', 'review', 'build-fix', 'review-fix'.
-- Nullable — existing rows stay NULL (no backfill).

ALTER TABLE agent_runs ADD COLUMN phase VARCHAR(16) AFTER role;
CREATE INDEX idx_agent_runs_phase ON agent_runs (phase);
