---
name: spire-conflicts
description: Resolve Dolt merge conflicts in Spire towers using direct `dolt sql` sessions instead of `spire board`, store APIs, or other schema-initializing paths. Use when `spire pull` or `spire sync --merge` leaves unresolved conflicts, when Dolt reports `dolt_conflicts` or `dolt_constraint_violations`, or when the user explicitly asks for `/spire-conflicts`.
---

# Spire Conflicts

Use this skill for Dolt data conflicts in Spire towers. The operating rule is:
work directly in `dolt sql`, keep the unresolved merge inside one Dolt session,
and do not route conflict repair through the board or beads store.

## Quick start

1. Identify the target tower database and whether the conflict already exists
   in the working set or needs to be reproduced.
2. Open a direct Dolt SQL session against that database.
   - Prefer the running Spire server:
     `dolt sql --host 127.0.0.1 --port 3307 --user root --no-tls --use-db <db>`
   - If there is no server, run `dolt sql` from the database directory.
3. Run `SET @@autocommit = 0;`
4. Inspect the conflict state:
   - `SELECT * FROM dolt_conflicts;`
   - `SELECT * FROM dolt_conflicts_<table>;`
   - `SELECT * FROM dolt_constraint_violations_<table>;`
   - `SELECT * FROM dolt_schema_conflicts;` when schema issues are suspected
5. Resolve the data, delete the corresponding rows from
   `dolt_conflicts_<table>`, then `CALL DOLT_ADD('-A')` and
   `CALL DOLT_COMMIT(...)`.
6. Do not report success until the relevant `dolt_conflicts*` and
   `dolt_constraint_violations*` queries return zero rows.

## Spire rules

- Prefer direct `dolt sql` over `spire sql` for conflict work. The point is to
  keep one Dolt session alive while conflicts are unresolved.
- Do not use `spire board`, `store.Open`, or other schema-initializing paths
  while conflicts exist. They can fail on unrelated writes and obscure the real
  problem.
- For `issues`, apply Spire's ownership rules:
  - Cluster-owned: `status`, `owner`, `assignee`, `closed_at`,
    `closed_by_session` -> remote / `their_*` wins.
  - User-owned: `title`, `description`, `design`, `acceptance_criteria`,
    `notes`, `priority`, `issue_type` -> local / `our_*` wins unless the user
    explicitly chooses otherwise.
  - Identity / lifecycle fields such as `id`, `created_at`, `created_by`, and
    `updated_at` should be preserved from the surviving row, not invented.
- For `comments`, `events`, and `labels`, treat FK violations as evidence that
  the parent `issues` row is still wrong. Fix the parent row first, then
  re-check violations.
- In delete-vs-modify conflicts, the surviving issue row may need to be
  recreated through `dolt_conflicts_issues` before the conflict row is deleted.

## Collaboration with the user

- Show the user a short summary of the conflicting row before applying a
  non-obvious resolution.
- If ownership rules fully determine the result, say so and proceed.
- If both sides changed the same user-owned field in materially different ways,
  stop and ask the user which value should survive.
- Before the final commit, summarize the SQL batch you are about to run and the
  expected surviving values.

## Avoid

- Running conflict repair in autocommit sessions.
- Using `@@dolt_force_transaction_commit = 1` unless the user explicitly asks
  for a risky last resort.
- Deleting constraint-violation rows as a first-line fix.
- Claiming the merge is resolved before conflicts and constraint violations are
  actually gone.

## Reference map

For SQL skeletons and Spire-specific playbooks, read
`references/dolt-conflict-playbook.md`.
