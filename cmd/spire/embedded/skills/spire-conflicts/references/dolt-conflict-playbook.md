# Dolt conflict playbook for Spire

Use this file when the user has an active Dolt merge conflict and the generic
skill overview is not enough.

## Session skeleton

When the conflict already exists in the working set:

```sql
SET @@autocommit = 0;

SELECT * FROM dolt_conflicts;
SELECT * FROM dolt_conflicts_issues;
SELECT COUNT(*) FROM dolt_constraint_violations_labels;
SELECT COUNT(*) FROM dolt_constraint_violations_comments;
SELECT COUNT(*) FROM dolt_constraint_violations_events;

-- apply your resolution here

CALL DOLT_ADD('-A');
CALL DOLT_COMMIT('-m', 'resolve spire dolt conflicts');
```

If you are reproducing the merge inside SQL, do that after `SET @@autocommit = 0`
so Dolt leaves the conflict tables available in the same session.

## Default inspection queries

```sql
SELECT * FROM dolt_conflicts;
SELECT * FROM dolt_conflicts_issues\G
SELECT * FROM dolt_schema_conflicts;
```

For a specific issue:

```sql
SELECT *
FROM dolt_conflicts_issues
WHERE COALESCE(NULLIF(our_id, 'NULL'), NULLIF(their_id, 'NULL'), NULLIF(base_id, 'NULL')) = '<issue-id>';
```

Constraint violations that commonly matter after `issues` conflicts:

```sql
SELECT * FROM dolt_constraint_violations_labels WHERE issue_id = '<issue-id>';
SELECT * FROM dolt_constraint_violations_comments WHERE issue_id = '<issue-id>';
SELECT * FROM dolt_constraint_violations_events WHERE issue_id = '<issue-id>';
```

## Spire issue ownership

Use these merge rules unless the human explicitly overrides them:

- Cluster wins:
  - `status`
  - `owner`
  - `assignee`
  - `closed_at`
  - `closed_by_session`
- User wins:
  - `title`
  - `description`
  - `design`
  - `acceptance_criteria`
  - `notes`
  - `priority`
  - `issue_type`
- Preserve from the surviving row:
  - `id`
  - `created_at`
  - `created_by`
  - `updated_at`

## Common failure shapes

### Delete vs modify on `issues`

Symptoms:

- `our_id = NULL`, `their_id = <issue-id>` or the inverse
- FK violations in `labels`, `comments`, or `events`

Approach:

1. Resolve the `issues` conflict first.
2. If the surviving row does not exist in `issues`, update the conflict row's
   `our_*` values so the row can be recreated, then delete the conflict row.
3. Re-check constraint violations. Do not delete child rows until you have
   confirmed the parent issue row is correct.

### Board/store failure while conflict exists

Symptoms:

- `spire board` fails on unrelated writes such as `dolt_ignore`
- schema init errors hide the actual conflict

Approach:

1. Stop using board/store paths for the repair.
2. Switch to direct `dolt sql`.
3. Resolve the actual `dolt_conflicts*` / `dolt_constraint_violations*` rows.

## Communication pattern

Before the final commit, tell the user:

1. Which issue or table is conflicted.
2. Which side wins for each changed field.
3. Whether any remaining ambiguity needs a human decision.
4. The exact SQL batch you plan to run.
