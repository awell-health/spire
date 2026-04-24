-- Migration 006: Add auth_profile observability to agent_runs.
-- auth_profile is the credential slot active at run-start ("subscription" or "api-key").
-- auth_profile_final is written only when a mid-run 429 swap promotes subscription → api-key;
-- NULL (or equal to auth_profile) means no swap occurred.
-- Both nullable — existing rows stay NULL (no backfill).

ALTER TABLE agent_runs ADD COLUMN auth_profile TEXT NULL;
ALTER TABLE agent_runs ADD COLUMN auth_profile_final TEXT NULL;
