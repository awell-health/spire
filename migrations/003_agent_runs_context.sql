-- Migration 003: Add context fields to agent_runs.
-- Adds formula, branch, bead type, tower, and wave context to each run.
-- All nullable — existing rows stay NULL (no backfill).

ALTER TABLE agent_runs ADD COLUMN formula_name VARCHAR(64) AFTER phase;
ALTER TABLE agent_runs ADD COLUMN formula_version INT AFTER formula_name;
ALTER TABLE agent_runs ADD COLUMN branch VARCHAR(128) AFTER formula_version;
ALTER TABLE agent_runs ADD COLUMN commit_sha VARCHAR(40) AFTER branch;
ALTER TABLE agent_runs ADD COLUMN bead_type VARCHAR(32) AFTER commit_sha;
ALTER TABLE agent_runs ADD COLUMN tower VARCHAR(64) AFTER bead_type;
ALTER TABLE agent_runs ADD COLUMN parent_run_id VARCHAR(32) AFTER tower;
ALTER TABLE agent_runs ADD COLUMN wave_index INT AFTER parent_run_id;

CREATE INDEX idx_agent_runs_formula ON agent_runs (formula_name);
CREATE INDEX idx_agent_runs_bead_type ON agent_runs (bead_type);
CREATE INDEX idx_agent_runs_tower ON agent_runs (tower);
