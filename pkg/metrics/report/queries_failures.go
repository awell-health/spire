package report

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// QueryFailures returns the failure-classes breakdown and the top-N
// hotspot beads (repeated failures on the same bead).
func (r *SQLReader) QueryFailures(ctx context.Context, scope Scope, since, until time.Time) (FailuresBlock, error) {
	out := FailuresBlock{
		Classes:  []FailureClass{},
		Hotspots: []FailureHotspot{},
	}
	if r.DB == nil {
		return out, errNoDB
	}
	clause, scopeArgs := scope.beadIDClause("bead_id")

	// Failure classes.
	classesSQL := fmt.Sprintf(`
		SELECT
			COALESCE(NULLIF(failure_class, ''), 'unknown') AS class,
			COUNT(*) AS cnt
		FROM agent_runs_olap
		WHERE started_at >= ? AND started_at <= ?
		  AND result NOT IN ('success', 'skipped', 'approve', 'no_changes', '')%s
		GROUP BY 1
		ORDER BY cnt DESC
	`, clause)
	rows, err := r.DB.QueryContext(ctx, classesSQL, append([]any{since, until}, scopeArgs...)...)
	if err != nil {
		return out, fmt.Errorf("failures classes: %w", err)
	}
	var totalFailures int
	for rows.Next() {
		var fc FailureClass
		if err := rows.Scan(&fc.Class, &fc.Count); err != nil {
			rows.Close()
			return out, err
		}
		totalFailures += fc.Count
		out.Classes = append(out.Classes, fc)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	for i := range out.Classes {
		if totalFailures > 0 {
			out.Classes[i].Percentage = float64(out.Classes[i].Count) / float64(totalFailures)
		}
	}

	// Hotspots — top 5 beads with the most failures in-window.
	hotspotSQL := fmt.Sprintf(`
		SELECT
			COALESCE(bead_id, '') AS bead_id,
			COALESCE(NULLIF(MAX(failure_class), ''), 'unknown') AS last_class,
			COUNT(*) AS attempts,
			MAX(started_at) AS last_activity
		FROM agent_runs_olap
		WHERE started_at >= ? AND started_at <= ?
		  AND result NOT IN ('success', 'skipped', 'approve', 'no_changes', '')
		  AND bead_id IS NOT NULL AND bead_id != ''%s
		GROUP BY 1
		ORDER BY attempts DESC, last_activity DESC
		LIMIT 10
	`, clause)
	hrows, err := r.DB.QueryContext(ctx, hotspotSQL, append([]any{since, until}, scopeArgs...)...)
	if err != nil {
		return out, fmt.Errorf("failures hotspots: %w", err)
	}
	defer hrows.Close()
	for hrows.Next() {
		var (
			h    FailureHotspot
			last sql.NullTime
		)
		if err := hrows.Scan(&h.BeadID, &h.LastFailureClass, &h.Attempts, &last); err != nil {
			return out, err
		}
		if last.Valid {
			h.LastActivityIso = last.Time.UTC().Format(time.RFC3339)
		}
		out.Hotspots = append(out.Hotspots, h)
	}
	if err := hrows.Err(); err != nil {
		return out, err
	}
	return out, nil
}
