package store

import (
	"database/sql"
	"fmt"
)

// FindLearningsByFailureClass returns reusable learnings across all source beads
// for the given failure class, ordered by recency. Used for cross-bead pattern
// matching during collect_context — labeled as "similar incidents" in the decide prompt.
func FindLearningsByFailureClass(db *sql.DB, failureClass string, limit int) ([]RecoveryLearning, error) {
	if limit <= 0 || limit > 20 {
		limit = 5
	}
	rows, err := db.Query(`
		SELECT id, recovery_bead, source_bead, failure_class, failure_sig,
		       resolution_kind, outcome, learning_summary, reusable, resolved_at
		FROM recovery_learnings
		WHERE failure_class = ? AND reusable = TRUE
		ORDER BY resolved_at DESC
		LIMIT ?`,
		failureClass, limit)
	if err != nil {
		return nil, fmt.Errorf("FindLearningsByFailureClass: %w", err)
	}
	defer rows.Close()
	return scanRecoveryLearnings(rows)
}

// scanRecoveryLearnings scans rows from the recovery_learnings table into
// RecoveryLearning structs. Shared by all recovery_learnings query functions.
func scanRecoveryLearnings(rows *sql.Rows) ([]RecoveryLearning, error) {
	var out []RecoveryLearning
	for rows.Next() {
		var rl RecoveryLearning
		var id int
		var outcome string
		if err := rows.Scan(
			&id, &rl.BeadID, &rl.SourceBead, &rl.FailureClass,
			&rl.FailureSignature, &rl.ResolutionKind, &outcome,
			&rl.LearningSummary, &rl.Reusable, &rl.ResolvedAt,
		); err != nil {
			return nil, fmt.Errorf("scan recovery learning: %w", err)
		}
		out = append(out, rl)
	}
	return out, rows.Err()
}
