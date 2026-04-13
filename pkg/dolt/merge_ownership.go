package dolt

import (
	"fmt"
	"log"
	"strings"

	"github.com/awell-health/spire/pkg/config"
)

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

	// Build a single SQL batch with autocommit disabled so that writes
	// succeed even when dolt has unresolved conflicts (autocommit mode
	// rejects writes in that state). All statements run in one CLI
	// process to preserve the session/transaction context.
	var stmts []string
	stmts = append(stmts, "SET @@autocommit = 0")
	stmts = append(stmts, fmt.Sprintf("USE `%s`", dbName))

	resolved := 0
	for _, row := range rows {
		id, updateStmt, deleteStmt, ok := buildIssueConflictStatements(row)
		if !ok {
			continue
		}
		stmts = append(stmts, updateStmt, deleteStmt)

		log.Printf("[ownership] resolving conflict: %s (cluster: status=%s, user: title=%q)",
			id, Coalesce(row["their_status"], "?"), Coalesce(row["our_title"], "?"))
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

func buildIssueConflictStatements(row map[string]string) (id, updateStmt, deleteStmt string, ok bool) {
	id = Coalesce(row["our_id"], row["their_id"], row["base_id"])
	if id == "" {
		return "", "", "", false
	}

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

	escapedID := SQLEscape(id)
	updateStmt = fmt.Sprintf("UPDATE issues SET %s WHERE id = '%s'",
		strings.Join(updates, ", "), escapedID)
	deleteStmt = fmt.Sprintf(
		"DELETE FROM dolt_conflicts_issues WHERE our_id = '%s' OR their_id = '%s' OR base_id = '%s'",
		escapedID, escapedID, escapedID)
	return id, updateStmt, deleteStmt, true
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
