package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// RecoveryLearningRow represents a row in the recovery_learnings SQL table.
// This is distinct from RecoveryLearning (in queries.go) which is a read model
// hydrated from bead metadata. RecoveryLearningRow is the durable, queryable
// record written by the learn step of the agentic recovery formula.
type RecoveryLearningRow struct {
	ID              string
	RecoveryBead    string
	SourceBead      string
	FailureClass    string
	FailureSig      string
	ResolutionKind  string // "reset" | "resummon" | "do_nothing" | "escalate" | "reset_to_step" | "verify_clean"
	Outcome         string // "clean" | "dirty" | "relapsed"
	LearningSummary string
	Reusable        bool
	ResolvedAt      time.Time
	ExpectedOutcome string // decide agent's prediction of what should happen
}

// WriteRecoveryLearning inserts a recovery learning into the recovery_learnings table.
func WriteRecoveryLearning(ctx context.Context, db *sql.DB, l RecoveryLearningRow) error {
	reusable := 0
	if l.Reusable {
		reusable = 1
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO recovery_learnings (id, recovery_bead, source_bead, failure_class, failure_sig, resolution_kind, outcome, learning_summary, reusable, resolved_at, expected_outcome) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.ID, l.RecoveryBead, l.SourceBead, l.FailureClass, l.FailureSig,
		l.ResolutionKind, l.Outcome, l.LearningSummary, reusable,
		l.ResolvedAt.UTC().Format("2006-01-02 15:04:05"),
		l.ExpectedOutcome,
	)
	if err != nil {
		return fmt.Errorf("insert recovery learning: %w", err)
	}
	return nil
}

// GetBeadLearnings returns per-bead reusable learnings for a specific source bead
// and failure class, ordered by resolved_at DESC.
func GetBeadLearnings(ctx context.Context, db *sql.DB, sourceBeadID, failureClass string) ([]RecoveryLearningRow, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, recovery_bead, source_bead, failure_class, failure_sig, resolution_kind, outcome, learning_summary, reusable, resolved_at, expected_outcome
		 FROM recovery_learnings
		 WHERE source_bead = ? AND failure_class = ? AND reusable = TRUE
		 ORDER BY resolved_at DESC`,
		sourceBeadID, failureClass,
	)
	if err != nil {
		return nil, fmt.Errorf("query bead learnings: %w", err)
	}
	defer rows.Close()
	return scanLearningRows(rows)
}

// GetCrossBeadLearnings returns reusable learnings across all beads for a failure
// class, ordered by resolved_at DESC, limited to the specified count.
func GetCrossBeadLearnings(ctx context.Context, db *sql.DB, failureClass string, limit int) ([]RecoveryLearningRow, error) {
	if limit <= 0 {
		limit = 5
	}
	rows, err := db.QueryContext(ctx,
		`SELECT id, recovery_bead, source_bead, failure_class, failure_sig, resolution_kind, outcome, learning_summary, reusable, resolved_at, expected_outcome
		 FROM recovery_learnings
		 WHERE failure_class = ? AND reusable = TRUE
		 ORDER BY resolved_at DESC
		 LIMIT ?`,
		failureClass, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query cross-bead learnings: %w", err)
	}
	defer rows.Close()
	return scanLearningRows(rows)
}

// scanLearningRows scans SQL result rows into RecoveryLearningRow slices.
func scanLearningRows(rows *sql.Rows) ([]RecoveryLearningRow, error) {
	var results []RecoveryLearningRow
	for rows.Next() {
		var r RecoveryLearningRow
		var reusable int
		var resolvedAt string
		var failureSig sql.NullString
		var learningSummary sql.NullString
		var expectedOutcome sql.NullString
		if err := rows.Scan(
			&r.ID, &r.RecoveryBead, &r.SourceBead, &r.FailureClass,
			&failureSig, &r.ResolutionKind, &r.Outcome,
			&learningSummary, &reusable, &resolvedAt, &expectedOutcome,
		); err != nil {
			return nil, fmt.Errorf("scan recovery learning row: %w", err)
		}
		r.Reusable = reusable != 0
		r.ResolvedAt, _ = time.Parse("2006-01-02 15:04:05", resolvedAt)
		if failureSig.Valid {
			r.FailureSig = failureSig.String
		}
		if learningSummary.Valid {
			r.LearningSummary = learningSummary.String
		}
		if expectedOutcome.Valid {
			r.ExpectedOutcome = expectedOutcome.String
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// getDB extracts the *sql.DB from the active beads store via the DB() accessor.
func getDB() (*sql.DB, error) {
	s, _, err := getStore()
	if err != nil {
		return nil, err
	}
	type dbAccessor interface {
		DB() *sql.DB
	}
	accessor, ok := s.(dbAccessor)
	if !ok {
		return nil, fmt.Errorf("store does not support direct SQL access")
	}
	return accessor.DB(), nil
}

// WriteRecoveryLearningAuto writes a recovery learning using the active store's DB.
func WriteRecoveryLearningAuto(l RecoveryLearningRow) error {
	db, err := getDB()
	if err != nil {
		return fmt.Errorf("get db for recovery learning write: %w", err)
	}
	return WriteRecoveryLearning(context.Background(), db, l)
}

// GetBeadLearningsAuto queries bead-specific learnings using the active store's DB.
func GetBeadLearningsAuto(sourceBeadID, failureClass string) ([]RecoveryLearningRow, error) {
	db, err := getDB()
	if err != nil {
		return nil, fmt.Errorf("get db for bead learnings: %w", err)
	}
	return GetBeadLearnings(context.Background(), db, sourceBeadID, failureClass)
}

// GetCrossBeadLearningsAuto queries cross-bead learnings using the active store's DB.
func GetCrossBeadLearningsAuto(failureClass string, limit int) ([]RecoveryLearningRow, error) {
	db, err := getDB()
	if err != nil {
		return nil, fmt.Errorf("get db for cross-bead learnings: %w", err)
	}
	return GetCrossBeadLearnings(context.Background(), db, failureClass, limit)
}

// LearningStats summarizes outcome statistics for a failure class.
type LearningStats struct {
	FailureClass       string
	TotalRecoveries    int
	ActionStats        []ActionOutcomeStat // per-action breakdown
	PredictionAccuracy float64             // fraction where outcome matched expected_outcome
}

// ActionOutcomeStat holds per-action outcome counts for a failure class.
type ActionOutcomeStat struct {
	ResolutionKind string  // e.g., "resummon", "reset", "triage"
	Total          int
	CleanCount     int     // outcome = "clean"
	DirtyCount     int     // outcome = "dirty"
	RelapsedCount  int     // outcome = "relapsed"
	SuccessRate    float64 // CleanCount / Total
}

// GetLearningStats returns aggregate outcome statistics for a failure class.
// Used by the decide prompt to show historical success rates per action.
func GetLearningStats(ctx context.Context, db *sql.DB, failureClass string) (*LearningStats, error) {
	// Query per-action outcome counts.
	rows, err := db.QueryContext(ctx,
		`SELECT resolution_kind, outcome, COUNT(*) FROM recovery_learnings
		 WHERE failure_class = ? GROUP BY resolution_kind, outcome`,
		failureClass,
	)
	if err != nil {
		return nil, fmt.Errorf("query learning stats: %w", err)
	}
	defer rows.Close()

	// Accumulate counts keyed by resolution_kind.
	type outcomeCount struct {
		clean, dirty, relapsed, other int
	}
	byAction := make(map[string]*outcomeCount)
	total := 0

	for rows.Next() {
		var kind, outcome string
		var cnt int
		if err := rows.Scan(&kind, &outcome, &cnt); err != nil {
			return nil, fmt.Errorf("scan learning stats row: %w", err)
		}
		oc, ok := byAction[kind]
		if !ok {
			oc = &outcomeCount{}
			byAction[kind] = oc
		}
		switch outcome {
		case "clean":
			oc.clean += cnt
		case "dirty":
			oc.dirty += cnt
		case "relapsed":
			oc.relapsed += cnt
		default:
			oc.other += cnt
		}
		total += cnt
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate learning stats rows: %w", err)
	}

	stats := &LearningStats{
		FailureClass:    failureClass,
		TotalRecoveries: total,
	}

	for kind, oc := range byAction {
		actionTotal := oc.clean + oc.dirty + oc.relapsed + oc.other
		successRate := 0.0
		if actionTotal > 0 {
			successRate = float64(oc.clean) / float64(actionTotal)
		}
		stats.ActionStats = append(stats.ActionStats, ActionOutcomeStat{
			ResolutionKind: kind,
			Total:          actionTotal,
			CleanCount:     oc.clean,
			DirtyCount:     oc.dirty,
			RelapsedCount:  oc.relapsed,
			SuccessRate:    successRate,
		})
	}

	// Compute prediction accuracy: fraction where outcome matched expected_outcome.
	var matchCount, totalWithExpected int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM recovery_learnings WHERE failure_class = ? AND expected_outcome != '' AND outcome = expected_outcome`,
		failureClass,
	).Scan(&matchCount)
	if err != nil {
		return stats, nil // non-fatal — stats are still useful without prediction accuracy
	}
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM recovery_learnings WHERE failure_class = ? AND expected_outcome != ''`,
		failureClass,
	).Scan(&totalWithExpected)
	if err != nil {
		return stats, nil
	}
	if totalWithExpected > 0 {
		stats.PredictionAccuracy = float64(matchCount) / float64(totalWithExpected)
	}

	return stats, nil
}

// GetLearningStatsAuto wraps GetLearningStats using the active store's DB.
func GetLearningStatsAuto(failureClass string) (*LearningStats, error) {
	db, err := getDB()
	if err != nil {
		return nil, fmt.Errorf("get db for learning stats: %w", err)
	}
	return GetLearningStats(context.Background(), db, failureClass)
}

// UpdateLearningOutcome updates the outcome column for a recovery learning row
// identified by the recovery bead ID. Used by relapse detection to mark
// previously "clean" outcomes as "relapsed".
func UpdateLearningOutcome(ctx context.Context, db *sql.DB, recoveryBeadID, outcome string) error {
	_, err := db.ExecContext(ctx,
		`UPDATE recovery_learnings SET outcome = ? WHERE recovery_bead = ?`,
		outcome, recoveryBeadID,
	)
	if err != nil {
		return fmt.Errorf("update learning outcome for %s: %w", recoveryBeadID, err)
	}
	return nil
}

// UpdateLearningOutcomeAuto updates the outcome using the active store's DB.
func UpdateLearningOutcomeAuto(recoveryBeadID, outcome string) error {
	db, err := getDB()
	if err != nil {
		return fmt.Errorf("get db for learning outcome update: %w", err)
	}
	return UpdateLearningOutcome(context.Background(), db, recoveryBeadID, outcome)
}
