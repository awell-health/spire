package dolt

import (
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/awell-health/spire/pkg/config"
)

// ErrMergeConstraintViolation signals that a merge left the database in a
// state that still violates foreign-key (or other) constraints. Callers can
// detect this with errors.Is to decide whether to treat an ownership
// enforcement failure as soft (warning) or hard (abort).
var ErrMergeConstraintViolation = errors.New("merge left unresolved constraint violations")

// ClusterFields are owned by the cluster (remote/theirs wins on conflict).
var ClusterFields = map[string]bool{
	"status":            true,
	"owner":             true,
	"assignee":          true,
	"closed_at":         true,
	"closed_by_session": true,
}

// IsClusterField returns true if the field is cluster-owned.
func IsClusterField(field string) bool {
	return ClusterFields[field]
}

// IsStatusRegression returns true if the status transition indicates lost work.
// Only flags transitions where active/closed work reverts to a less-progressed state.
func IsStatusRegression(from, to string) bool {
	switch {
	case from == "closed" && to != "closed":
		return true // closed work reopened by stale state
	case from == "in_progress" && to == "open":
		return true // active work lost to idle state
	default:
		return false
	}
}

// ClusterRegression describes a regression in a cluster-owned field after pull/merge.
type ClusterRegression struct {
	BeadID string
	// Cluster field snapshots from pre-pull state to restore.
	Status          string
	Owner           string
	Assignee        string
	ClosedAt        string // "" means NULL
	ClosedBySession string // "" means NULL
}

// sqlWithDB is a convenience wrapper that runs SQL against a specific database.
func sqlWithDB(dbName, query string) (string, error) {
	return SQL(query, false, dbName, nil)
}

// GetCurrentCommitHash returns the current HEAD commit hash for the given database.
func GetCurrentCommitHash(dbName string) string {
	q := fmt.Sprintf("USE `%s`; SELECT HASHOF('HEAD') AS value", dbName)
	out, err := RawQuery(q)
	if err != nil {
		return ""
	}
	return config.ExtractSQLValue(out)
}

// ApplyMergeOwnership runs after a pull or merge to enforce field-level ownership.
// It resolves dolt conflicts (if any) and scans for cluster-field regressions.
// dbName is the dolt database name; preCommit is the HEAD hash before the pull/merge.
func ApplyMergeOwnership(dbName, preCommit string) error {
	if preCommit == "" {
		return nil
	}

	// Phase 1: resolve any dolt conflicts on the issues table.
	resolved, err := ResolveIssueConflicts(dbName)
	if err != nil {
		log.Printf("[ownership] resolve conflicts: %s", err)
		return err
	}
	if resolved > 0 {
		log.Printf("[ownership] resolved %d conflict(s) with field-level ownership", resolved)
	}
	remaining, err := HasUnresolvedConflicts(dbName)
	if err != nil {
		return fmt.Errorf("check unresolved conflicts: %w", err)
	}
	if remaining > 0 {
		return fmt.Errorf("issues conflicts remain (%d unresolved)", remaining)
	}

	// Phase 1b: ensure the merge did not leave behind FK / other constraint
	// violations (e.g. orphaned label/comment/event rows pointing at a bead
	// that was deleted on one side and modified on the other).
	violations, err := HasUnresolvedConstraintViolations(dbName)
	if err != nil {
		return fmt.Errorf("check constraint violations: %w", err)
	}
	if violations > 0 {
		return fmt.Errorf("%w: %d violation(s) — inspect dolt_constraint_violations",
			ErrMergeConstraintViolation, violations)
	}

	// Phase 2: scan for cluster-field regressions.
	regressions, err := ScanClusterRegressions(dbName, preCommit)
	if err != nil {
		log.Printf("[ownership] scan regressions: %s", err)
		return err
	}
	if len(regressions) > 0 {
		if err := RepairClusterRegressions(dbName, regressions); err != nil {
			log.Printf("[ownership] repair regressions: %s", err)
			return err
		}
		log.Printf("[ownership] repaired %d cluster-field regression(s)", len(regressions))
	}

	return nil
}

// HasUnresolvedConflicts checks whether unresolved Dolt conflicts exist on the
// issues table. Returns the conflict row count and any error. If the conflict
// table does not exist (no conflicts), returns 0, nil.
func HasUnresolvedConflicts(dbName string) (int, error) {
	countQ := fmt.Sprintf("SELECT COUNT(*) AS c FROM `%s`.dolt_conflicts_issues", dbName)
	out, err := sqlWithDB(dbName, countQ)
	if err != nil {
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "doesn't exist") {
			return 0, nil
		}
		return 0, err
	}
	return ExtractCountValue(out), nil
}

// HasUnresolvedConstraintViolations returns the total number of rows across
// all per-table dolt_constraint_violations_* views — i.e. FK or other
// constraint violations introduced by a merge that still need attention.
// Returns 0 (no error) if the summary view does not exist.
func HasUnresolvedConstraintViolations(dbName string) (int, error) {
	q := fmt.Sprintf("SELECT COALESCE(SUM(num_violations), 0) AS c FROM `%s`.dolt_constraint_violations", dbName)
	out, err := sqlWithDB(dbName, q)
	if err != nil {
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "doesn't exist") {
			return 0, nil
		}
		return 0, err
	}
	return ExtractCountValue(out), nil
}

// issuesColumns returns the ordered list of column names on the issues table
// for the given database. Used to build schema-agnostic INSERTs that restore
// a locally-deleted row from the `their_*` side of a conflict.
func issuesColumns(dbName string) ([]string, error) {
	q := fmt.Sprintf(
		"SELECT COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = '%s' AND TABLE_NAME = 'issues' ORDER BY ORDINAL_POSITION",
		SQLEscape(dbName))
	out, err := sqlWithDB(dbName, q)
	if err != nil {
		return nil, fmt.Errorf("list issues columns: %w", err)
	}
	rows := ParseDoltRows(out, []string{"name"})
	cols := make([]string, 0, len(rows))
	for _, r := range rows {
		if c := r["name"]; c != "" {
			cols = append(cols, c)
		}
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("no columns found for issues table in %q", dbName)
	}
	return cols, nil
}

// ResolveIssueConflicts reads dolt_conflicts_issues and applies field-level
// ownership rules to produce resolved rows. All conflict resolutions are
// applied atomically in a single transaction — either every conflict is
// resolved and committed, or none are (the transaction is rolled back).
// Returns the number of conflicts resolved.
func ResolveIssueConflicts(dbName string) (int, error) {
	// Check if the conflicts table has any rows.
	countQ := fmt.Sprintf("SELECT COUNT(*) AS c FROM `%s`.dolt_conflicts_issues", dbName)
	out, err := sqlWithDB(dbName, countQ)
	if err != nil {
		// Table may not exist (no conflicts) — not an error.
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "doesn't exist") {
			return 0, nil
		}
		return 0, err
	}
	if ExtractCountValue(out) == 0 {
		return 0, nil
	}

	// Read all conflict rows.
	q := fmt.Sprintf(`SELECT
		base_id, our_id, their_id,
		base_status, our_status, their_status,
		base_owner, our_owner, their_owner,
		base_assignee, our_assignee, their_assignee,
		base_closed_at, our_closed_at, their_closed_at,
		base_closed_by_session, our_closed_by_session, their_closed_by_session,
		base_title, our_title, their_title,
		base_description, our_description, their_description,
		base_priority, our_priority, their_priority,
		base_issue_type, our_issue_type, their_issue_type
	FROM %[1]s.dolt_conflicts_issues`, dbName)

	out, err = sqlWithDB(dbName, q)
	if err != nil {
		return 0, fmt.Errorf("query conflicts: %w", err)
	}

	rows := ParseDoltRows(out, []string{
		"base_id", "our_id", "their_id",
		"base_status", "our_status", "their_status",
		"base_owner", "our_owner", "their_owner",
		"base_assignee", "our_assignee", "their_assignee",
		"base_closed_at", "our_closed_at", "their_closed_at",
		"base_closed_by_session", "our_closed_by_session", "their_closed_by_session",
		"base_title", "our_title", "their_title",
		"base_description", "our_description", "their_description",
		"base_priority", "our_priority", "their_priority",
		"base_issue_type", "our_issue_type", "their_issue_type",
	})

	if len(rows) == 0 {
		return 0, nil
	}

	// Fetch the issues-table column list once so delete-vs-modify conflicts
	// can be resolved with a schema-agnostic INSERT from the `their_*` side.
	cols, err := issuesColumns(dbName)
	if err != nil {
		return 0, err
	}

	// Build a single SQL batch with autocommit disabled so that writes
	// succeed even when dolt has unresolved conflicts (autocommit mode
	// rejects writes in that state). All statements run in one CLI
	// process to preserve the session/transaction context.
	var stmts []string
	stmts = append(stmts, "SET @@autocommit = 0")
	stmts = append(stmts, fmt.Sprintf("USE `%s`", dbName))

	resolved := 0
	for _, row := range rows {
		rowStmts, id, kind, ok := buildIssueConflictStatements(row, cols)
		if !ok {
			continue
		}
		stmts = append(stmts, rowStmts...)

		log.Printf("[ownership] resolving conflict: %s [%s] (cluster: status=%s, user: title=%q)",
			id, kind,
			Coalesce(row["their_status"], row["our_status"], "?"),
			Coalesce(row["our_title"], row["their_title"], "?"))
		resolved++
	}

	if resolved == 0 {
		return 0, nil
	}

	stmts = append(stmts, "CALL DOLT_ADD('-A')")
	stmts = append(stmts, fmt.Sprintf("CALL DOLT_COMMIT('-m', 'spire: field-level merge resolution (%d conflicts)')", resolved))

	batch := strings.Join(stmts, "; ")
	if _, err := RawQuery(batch); err != nil {
		return 0, fmt.Errorf("resolve conflicts: %w", err)
	}

	return resolved, nil
}

// buildIssueConflictStatements returns the SQL statements required to
// resolve a single row from dolt_conflicts_issues, plus the bead id the
// resolution targets and a short label describing the branch that was taken
// (for logging). Returns ok=false when the row has no usable id.
//
// The resolution branches by how each side diff'd the base row:
//
//	ours=modify,  theirs=modify  → field-level merge (cluster wins on
//	                                cluster fields, local wins on user fields)
//	ours=delete,  theirs=modify  → restore from `their_*` (cluster wins on
//	                                the existence of the bead) — this is the
//	                                case that used to silently no-op the
//	                                UPDATE and leave orphaned FK children.
//	ours=modify,  theirs=delete  → keep ours (ignore the cluster-side delete;
//	                                whoever modified locally still wants it).
//	ours=delete,  theirs=delete  → nothing to apply; just drop the conflict.
//
// In every branch the dolt_conflicts_issues row for the bead is deleted so
// the merge can commit.
func buildIssueConflictStatements(row map[string]string, issueColumns []string) (stmts []string, id string, kind string, ok bool) {
	ourID := nonNullValue(row["our_id"])
	theirID := nonNullValue(row["their_id"])
	baseID := nonNullValue(row["base_id"])

	id = firstNonEmpty(ourID, theirID, baseID)
	if id == "" {
		return nil, "", "", false
	}
	escapedID := SQLEscape(id)
	deleteConflictStmt := fmt.Sprintf(
		"DELETE FROM dolt_conflicts_issues WHERE our_id = '%s' OR their_id = '%s' OR base_id = '%s'",
		escapedID, escapedID, escapedID)

	switch {
	case ourID == "" && theirID != "":
		// Delete-vs-modify: the cluster still cares about this bead. Restore
		// it from the `their_*` snapshot using an INSERT that copies every
		// column — this preserves FK children (labels/comments/events/…)
		// that the cluster added alongside the modification.
		insert, insertOK := buildRestoreFromTheirsInsert(escapedID, issueColumns)
		if !insertOK {
			return nil, "", "", false
		}
		return []string{insert, deleteConflictStmt}, id, "restore-from-theirs", true

	case ourID != "" && theirID == "":
		// Modify-vs-delete: the local row still exists and has been modified;
		// ignore the cluster's delete and simply clear the conflict row.
		return []string{deleteConflictStmt}, id, "keep-ours", true

	case ourID == "" && theirID == "":
		// Both sides deleted — the row is already gone locally. Nothing to
		// apply apart from clearing the conflict entry.
		return []string{deleteConflictStmt}, id, "both-deleted", true

	default:
		// Modify-vs-modify: classic field-level ownership merge.
		return []string{buildFieldLevelMergeUpdate(row, escapedID), deleteConflictStmt}, id, "field-merge", true
	}
}

// buildFieldLevelMergeUpdate constructs the UPDATE used for modify-vs-modify
// conflicts: cluster-owned fields take `theirs`, user-owned fields take
// `ours`. `escapedID` must already be SQL-escaped.
func buildFieldLevelMergeUpdate(row map[string]string, escapedID string) string {
	updates := []string{
		// Cluster-owned fields: take theirs (remote)
		fmt.Sprintf("status = '%s'", SQLEscape(Coalesce(row["their_status"], row["our_status"]))),
		SQLNullableSet("owner", row["their_owner"], row["our_owner"]),
		SQLNullableSet("assignee", row["their_assignee"], row["our_assignee"]),
		SQLNullableSet("closed_at", row["their_closed_at"], row["our_closed_at"]),
		SQLNullableSet("closed_by_session", row["their_closed_by_session"], row["our_closed_by_session"]),
		// User-owned fields: take ours (local)
		fmt.Sprintf("title = '%s'", SQLEscape(Coalesce(row["our_title"], row["their_title"]))),
		fmt.Sprintf("description = '%s'", SQLEscape(Coalesce(row["our_description"], row["their_description"]))),
		fmt.Sprintf("priority = %s", Coalesce(row["our_priority"], row["their_priority"], "2")),
		fmt.Sprintf("issue_type = '%s'", SQLEscape(Coalesce(row["our_issue_type"], row["their_issue_type"]))),
	}
	return fmt.Sprintf("UPDATE issues SET %s WHERE id = '%s'",
		strings.Join(updates, ", "), escapedID)
}

// buildRestoreFromTheirsInsert constructs an INSERT that restores a bead
// from the `their_*` snapshot of a conflict row. Copying every column keeps
// us schema-independent — we don't need to be updated when new columns are
// added to the issues table. `escapedID` must already be SQL-escaped.
func buildRestoreFromTheirsInsert(escapedID string, issueColumns []string) (string, bool) {
	if len(issueColumns) == 0 {
		return "", false
	}
	theirCols := make([]string, len(issueColumns))
	for i, c := range issueColumns {
		theirCols[i] = "their_" + c
	}
	return fmt.Sprintf(
		"INSERT INTO issues (%s) SELECT %s FROM dolt_conflicts_issues WHERE their_id = '%s'",
		strings.Join(issueColumns, ", "),
		strings.Join(theirCols, ", "),
		escapedID,
	), true
}

// nonNullValue normalises a conflict-row cell, treating dolt's "NULL"
// sentinel as absent (same rule as Coalesce).
func nonNullValue(s string) string {
	if s == "" || strings.EqualFold(s, "NULL") {
		return ""
	}
	return s
}

// firstNonEmpty returns the first argument that is non-empty, or "" if none.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ScanClusterRegressions compares the pre-pull state to HEAD and finds
// cluster-owned fields that regressed (e.g. closed->open, lost owner/assignee).
// Returns a snapshot of the pre-pull cluster fields for each regressed bead.
func ScanClusterRegressions(dbName, preCommit string) ([]ClusterRegression, error) {
	q := fmt.Sprintf(`SELECT from_id, to_id, diff_type,
		from_status, to_status,
		from_owner, to_owner,
		from_assignee, to_assignee,
		from_closed_at, to_closed_at,
		from_closed_by_session, to_closed_by_session
	FROM dolt_diff('%s', 'HEAD', 'issues')
	WHERE diff_type = 'modified'`, SQLEscape(preCommit))

	out, err := sqlWithDB(dbName, q)
	if err != nil {
		return nil, fmt.Errorf("diff query: %w", err)
	}

	rows := ParseDoltRows(out, []string{
		"from_id", "to_id", "diff_type",
		"from_status", "to_status",
		"from_owner", "to_owner",
		"from_assignee", "to_assignee",
		"from_closed_at", "to_closed_at",
		"from_closed_by_session", "to_closed_by_session",
	})

	var regressions []ClusterRegression
	for _, row := range rows {
		id := Coalesce(row["to_id"], row["from_id"])
		fromStatus := row["from_status"]
		toStatus := row["to_status"]

		if fromStatus != "" && toStatus != "" && fromStatus != toStatus && IsStatusRegression(fromStatus, toStatus) {
			// Capture ALL cluster fields from the pre-pull state so we restore
			// the full cluster snapshot, not just status.
			regressions = append(regressions, ClusterRegression{
				BeadID:          id,
				Status:          fromStatus,
				Owner:           row["from_owner"],
				Assignee:        row["from_assignee"],
				ClosedAt:        row["from_closed_at"],
				ClosedBySession: row["from_closed_by_session"],
			})
		}
	}

	return regressions, nil
}

// RepairClusterRegressions restores the pre-pull cluster field values for regressed beads.
func RepairClusterRegressions(dbName string, regressions []ClusterRegression) error {
	for _, r := range regressions {
		updates := []string{
			fmt.Sprintf("status = '%s'", SQLEscape(r.Status)),
			SQLNullableSet("owner", r.Owner, ""),
			SQLNullableSet("assignee", r.Assignee, ""),
			SQLNullableSet("closed_at", r.ClosedAt, ""),
			SQLNullableSet("closed_by_session", r.ClosedBySession, ""),
		}
		updateSQL := fmt.Sprintf("UPDATE `%s`.issues SET %s WHERE id = '%s'",
			dbName, strings.Join(updates, ", "), SQLEscape(r.BeadID))
		if _, err := sqlWithDB(dbName, updateSQL); err != nil {
			log.Printf("[ownership] repair %s: %s", r.BeadID, err)
			continue
		}
		log.Printf("[ownership] repaired: %s (restored cluster state: status=%s, owner=%s)",
			r.BeadID, r.Status, r.Owner)
	}

	// Commit repairs.
	commitSQL := fmt.Sprintf("USE `%s`; CALL DOLT_ADD('-A'); CALL DOLT_COMMIT('-m', 'spire: repair %d cluster-field regression(s)')",
		dbName, len(regressions))
	if _, err := RawQuery(commitSQL); err != nil {
		// Non-fatal — may fail if no actual changes (e.g. already at correct state).
		log.Printf("[ownership] commit regressions: %s", err)
	}

	return nil
}
