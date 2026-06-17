package tower

import (
	"fmt"
	"strings"
)

// LocalOnlyTables are Spire extension tables that hold local/derived data and
// must NOT be version-tracked in Dolt or synced to the DoltHub remote:
//
//   - agent_runs    — per-invocation execution metrics; continuously ETL'd to
//     the local DuckDB OLAP store, which is the durable analytics home.
//   - bead_lifecycle — first-transition timestamp sidecar; rebuilt from the
//     issues table by backfillBeadLifecycle.
//
// Both are written constantly but never "settle", so when they are tracked the
// Dolt working set is perpetually dirty: every commit (steward cycle, agent
// write) re-stages tens of thousands of rows for zero net change, and the
// daemon's dolt_pull/dolt_push merge them across the network remote (and the
// dirty set makes the pull fail with "cannot merge with uncommitted changes").
// Registering them in dolt_ignore keeps them as untracked working-set tables:
// writes never dirty the commit working set, and the data still persists on
// disk across server restarts.
//
// dolt_ignore only takes effect on a table that has NOT yet been committed, so
// the two code paths differ: preRegisterLocalOnlyIgnore handles fresh towers
// (register the pattern before the table is created); UntrackLocalOnlyTables
// handles existing towers (drop the already-committed table, then ignore, then
// recreate untracked).
var LocalOnlyTables = []string{"agent_runs", "bead_lifecycle"}

// preRegisterLocalOnlyIgnore registers dolt_ignore patterns for any LocalOnlyTable
// that does NOT yet exist, so a subsequent CREATE TABLE produces an untracked
// table. It is a no-op for tables that already exist (an existing tower), where
// the dolt_ignore pattern would have no effect — UntrackLocalOnlyTables owns
// that case. Idempotent: REPLACE INTO is a no-op when the pattern is present,
// and a "nothing to commit" on the commit is tolerated.
func preRegisterLocalOnlyIgnore(exec SQLExec, database string) error {
	var missing []string
	for _, t := range LocalOnlyTables {
		exists, err := tableExists(exec, database, t)
		if err != nil {
			return fmt.Errorf("preRegisterLocalOnlyIgnore: check %s: %w", t, err)
		}
		if !exists {
			missing = append(missing, t)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "USE `%s`;", database)
	for _, t := range missing {
		fmt.Fprintf(&b, " REPLACE INTO dolt_ignore VALUES ('%s', true);", t)
	}
	b.WriteString(" CALL DOLT_ADD('dolt_ignore');")
	if _, err := exec(b.String()); err != nil {
		return fmt.Errorf("preRegisterLocalOnlyIgnore: register patterns: %w", err)
	}
	if _, err := exec(fmt.Sprintf(
		"USE `%s`; CALL DOLT_COMMIT('-m', 'chore: dolt_ignore local-only tables before create')",
		database)); err != nil && !isNothingToCommit(err) {
		return fmt.Errorf("preRegisterLocalOnlyIgnore: commit: %w", err)
	}
	return nil
}

// UntrackLocalOnlyTables converts each currently-tracked LocalOnlyTable on an
// EXISTING tower into a dolt_ignore'd, untracked working-set table. Per the
// "drop & recreate empty" decision it does NOT preserve the Dolt copy of the
// data: agent_runs history lives in the DuckDB OLAP store, and bead_lifecycle
// is refilled by backfillBeadLifecycle. The recreated table is empty and
// carries only the canonical base schema; the caller's column-migration pass
// (ensureColumn) re-adds any migration-added columns.
//
// WARNING — propagation: dropping a tracked table commits a drop to the tower's
// Dolt history, which the daemon pushes to the DoltHub remote. Every tower
// sharing that remote converges to the untracked state on its next pull, and a
// peer that pulls before running this migration loses its copy. This is why the
// caller gates it behind an explicit opt-in rather than running it on every
// `spire up`.
//
// Idempotent: a table that is already untracked (absent from HEAD) is skipped,
// so re-running never drops a recreated untracked table.
func UntrackLocalOnlyTables(exec SQLExec, database string) error {
	for _, t := range LocalOnlyTables {
		tracked, err := tableTrackedAtHead(exec, database, t)
		if err != nil {
			return fmt.Errorf("UntrackLocalOnlyTables: check %s: %w", t, err)
		}
		if !tracked {
			continue
		}
		ddl, ok := localOnlyTableDDL(t)
		if !ok {
			return fmt.Errorf("UntrackLocalOnlyTables: no canonical DDL for %s", t)
		}
		if err := untrackOne(exec, database, t, ddl); err != nil {
			return fmt.Errorf("UntrackLocalOnlyTables: %s: %w", t, err)
		}
	}
	return nil
}

// untrackOne runs the validated drop-then-ignore-then-recreate sequence for a
// single tracked table. The drop MUST be committed while the table is still
// tracked — once the dolt_ignore pattern is present, Dolt refuses to stage the
// drop ("nothing to commit"), leaving the table in HEAD forever.
func untrackOne(exec SQLExec, database, table, ddl string) error {
	steps := []string{
		// 1. Drop the tracked table and commit (still tracked -> drop is staged).
		fmt.Sprintf("USE `%s`; DROP TABLE `%s`; CALL DOLT_ADD('-A'); CALL DOLT_COMMIT('-m', 'chore: untrack %s (drop from version control)')", database, table, table),
		// 2. Register the ignore pattern and commit it.
		fmt.Sprintf("USE `%s`; REPLACE INTO dolt_ignore VALUES ('%s', true); CALL DOLT_ADD('dolt_ignore'); CALL DOLT_COMMIT('-m', 'chore: dolt_ignore %s')", database, table, table),
		// 3. Recreate from the canonical base schema. The pattern is now active,
		//    so the new table is untracked. CREATE TABLE IF NOT EXISTS is safe.
		fmt.Sprintf("USE `%s`; %s", database, ddl),
	}
	for _, s := range steps {
		if _, err := exec(s); err != nil && !isNothingToCommit(err) {
			return err
		}
	}
	return nil
}

// tableExists reports whether table exists in database's working set.
func tableExists(exec SQLExec, database, table string) (bool, error) {
	out, err := exec(fmt.Sprintf("USE `%s`; SHOW TABLES LIKE '%s'", database, table))
	if err != nil {
		return false, err
	}
	return strings.Contains(out, table), nil
}

// tableTrackedAtHead reports whether table exists at HEAD (i.e. is version-
// tracked). A `SELECT ... AS OF 'HEAD'` against an untracked (working-set-only)
// or absent table errors with a not-found/does-not-exist message. Any other
// error is treated conservatively as "not tracked" so the destructive drop in
// untrackOne never runs on an ambiguous state.
func tableTrackedAtHead(exec SQLExec, database, table string) (bool, error) {
	_, err := exec(fmt.Sprintf("USE `%s`; SELECT 1 FROM `%s` AS OF 'HEAD' LIMIT 1", database, table))
	if err == nil {
		return true, nil
	}
	return false, nil
}

// localOnlyTableDDL returns the canonical CREATE TABLE DDL for a local-only
// extension table, sourced from the single spireExtensionTables registry.
func localOnlyTableDDL(table string) (string, bool) {
	for _, t := range spireExtensionTables {
		if t.name == table {
			return t.sql, true
		}
	}
	return "", false
}

// isNothingToCommit reports whether err is Dolt's benign "nothing to commit".
func isNothingToCommit(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "nothing to commit")
}
