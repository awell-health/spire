package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

// generateLearningID produces a short hex ID with a "lrn-" prefix.
func generateLearningID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "lrn-00000000"
	}
	return "lrn-" + hex.EncodeToString(b)
}

// WriteRecoveryLearning inserts a recovery learning record into the
// recovery_learnings table. If ID is empty, one is generated. If ResolvedAt
// is zero, it defaults to now. Returns the record ID.
func WriteRecoveryLearning(db *sql.DB, l RecoveryLearningRecord) (string, error) {
	if l.ID == "" {
		l.ID = generateLearningID()
	}
	if l.ResolvedAt.IsZero() {
		l.ResolvedAt = time.Now().UTC()
	}
	_, err := db.Exec(`
		INSERT INTO recovery_learnings
			(id, recovery_bead, source_bead, failure_class, failure_sig,
			 resolution_kind, outcome, learning_summary, reusable, resolved_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.ID, l.RecoveryBead, l.SourceBead, l.FailureClass, l.FailureSig,
		l.ResolutionKind, l.Outcome, l.LearningSummary, l.Reusable, l.ResolvedAt,
	)
	if err != nil {
		return "", fmt.Errorf("insert recovery_learnings: %w", err)
	}
	return l.ID, nil
}

// QueryCrossBeadLearnings returns up to limit reusable learnings matching the
// given failure class, across ALL source beads, most recent first.
//
// This queries bead metadata on closed recovery beads. When the
// recovery_learnings table is populated by the learn step, this function
// can be upgraded to use direct SQL for better performance.
func QueryCrossBeadLearnings(failureClass string, limit int) ([]RecoveryLearning, error) {
	if limit <= 0 {
		limit = 5
	}
	reusable := true
	return ListClosedRecoveryBeads(RecoveryLookupFilter{
		FailureClass: failureClass,
		Reusable:     &reusable,
		Limit:        limit,
	})
}
