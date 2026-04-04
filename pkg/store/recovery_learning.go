package store

import (
	"database/sql"
	"time"
)

// RecoveryLearningRecord represents a row in the recovery_learnings table.
// Each row captures what was learned from a single recovery attempt,
// enabling cross-bead knowledge reuse during future recoveries.
// This is the table-backed counterpart to RecoveryLearning (metadata-based).
type RecoveryLearningRecord struct {
	ID              string    `db:"id"`
	RecoveryBead    string    `db:"recovery_bead"`
	SourceBead      string    `db:"source_bead"`
	FailureClass    string    `db:"failure_class"`
	FailureSig      string    `db:"failure_sig"`
	ResolutionKind  string    `db:"resolution_kind"`
	Outcome         string    `db:"outcome"` // "clean" | "dirty"
	LearningSummary string    `db:"learning_summary"`
	Reusable        bool      `db:"reusable"`
	ResolvedAt      time.Time `db:"resolved_at"`
}

// CreateRecoveryLearning inserts one recovery learning row.
func CreateRecoveryLearning(db *sql.DB, l RecoveryLearningRecord) error {
	_, err := db.Exec(`
		INSERT INTO recovery_learnings
			(id, recovery_bead, source_bead, failure_class, failure_sig,
			 resolution_kind, outcome, learning_summary, reusable, resolved_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.ID, l.RecoveryBead, l.SourceBead, l.FailureClass, l.FailureSig,
		l.ResolutionKind, l.Outcome, l.LearningSummary, l.Reusable, l.ResolvedAt,
	)
	return err
}

// FindRecoveryLearnings returns all reusable rows for a source bead + failure
// class, ordered by resolved_at DESC. Per-bead read path for collect_context.
func FindRecoveryLearnings(db *sql.DB, sourceBeadID, failureClass string) ([]RecoveryLearningRecord, error) {
	rows, err := db.Query(`
		SELECT id, recovery_bead, source_bead, failure_class, failure_sig,
		       resolution_kind, outcome, learning_summary, reusable, resolved_at
		FROM recovery_learnings
		WHERE source_bead = ? AND failure_class = ? AND reusable = TRUE
		ORDER BY resolved_at DESC`,
		sourceBeadID, failureClass,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRecoveryLearnings(rows)
}

// FindMatchingLearning returns the single best matching learning for a source
// bead + failure class. Returns nil (not an error) when no match exists.
func FindMatchingLearning(db *sql.DB, sourceBeadID, failureClass string) (*RecoveryLearningRecord, error) {
	results, err := FindRecoveryLearnings(db, sourceBeadID, failureClass)
	if err != nil || len(results) == 0 {
		return nil, err
	}
	return &results[0], nil
}

// FindCrossBeadLearnings returns reusable rows across ALL source beads for a
// failure class, ordered by resolved_at DESC with a limit. Cross-bead read
// path for collect_context.
func FindCrossBeadLearnings(db *sql.DB, failureClass string, limit int) ([]RecoveryLearningRecord, error) {
	rows, err := db.Query(`
		SELECT id, recovery_bead, source_bead, failure_class, failure_sig,
		       resolution_kind, outcome, learning_summary, reusable, resolved_at
		FROM recovery_learnings
		WHERE failure_class = ? AND reusable = TRUE
		ORDER BY resolved_at DESC
		LIMIT ?`,
		failureClass, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRecoveryLearnings(rows)
}

func scanRecoveryLearnings(rows *sql.Rows) ([]RecoveryLearningRecord, error) {
	var out []RecoveryLearningRecord
	for rows.Next() {
		var l RecoveryLearningRecord
		if err := rows.Scan(
			&l.ID, &l.RecoveryBead, &l.SourceBead, &l.FailureClass, &l.FailureSig,
			&l.ResolutionKind, &l.Outcome, &l.LearningSummary, &l.Reusable, &l.ResolvedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
