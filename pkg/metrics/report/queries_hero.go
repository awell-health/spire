package report

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// QueryHeroActiveAgents returns (activeNow, highWater) — the distinct
// agent_name count currently active (within the last 24h) and the
// highest distinct agent count observed in any single week of the
// window. "Agents" here means distinct agent_name values from
// agent_runs_olap since that's the only stable identifier across the
// OLAP surface.
func (r *SQLReader) QueryHeroActiveAgents(ctx context.Context, scope Scope, since, until time.Time) (int, int, error) {
	if r.DB == nil {
		return 0, 0, errNoDB
	}
	clause, args := scope.beadIDClause("bead_id")

	// "Now" = distinct agents seen in the last 24h before `until`.
	liveSince := until.Add(-24 * time.Hour)
	liveArgs := append([]any{liveSince, until}, args...)
	var live sql.NullInt64
	err := r.DB.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT COUNT(DISTINCT COALESCE(agent_name, ''))
		FROM tool_events
		WHERE timestamp >= ? AND timestamp <= ?
		  AND agent_name IS NOT NULL AND agent_name != ''%s
	`, clause), liveArgs...).Scan(&live)
	if err != nil {
		// tool_events may not have a bead_id match for older data; ignore
		// empty-result error paths and fall back to 0.
		if err != sql.ErrNoRows {
			return 0, 0, fmt.Errorf("hero active agents (now): %w", err)
		}
	}

	// High-water = max distinct over each ISO week in the window.
	hwArgs := append([]any{since, until}, args...)
	var hw sql.NullInt64
	err = r.DB.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT COALESCE(MAX(weekly_count), 0) FROM (
			SELECT date_trunc('week', timestamp)::DATE AS wk,
			       COUNT(DISTINCT COALESCE(agent_name, '')) AS weekly_count
			FROM tool_events
			WHERE timestamp >= ? AND timestamp <= ?
			  AND agent_name IS NOT NULL AND agent_name != ''%s
			GROUP BY 1
		)
	`, clause), hwArgs...).Scan(&hw)
	if err != nil && err != sql.ErrNoRows {
		return 0, 0, fmt.Errorf("hero active agents (hw): %w", err)
	}

	return int(nullInt64(live)), int(nullInt64(hw)), nil
}

// QueryHeroCostByWeek returns the cost_usd sum for the current week
// (the 7 days ending at `until`) and the previous week (the prior 7).
// Used to compute cost/week and the WoW delta.
func (r *SQLReader) QueryHeroCostByWeek(ctx context.Context, scope Scope, since, until time.Time) (float64, float64, error) {
	if r.DB == nil {
		return 0, 0, errNoDB
	}
	clause, args := scope.beadIDClause("bead_id")
	thisStart := until.Add(-7 * 24 * time.Hour)
	prevStart := until.Add(-14 * 24 * time.Hour)
	prevEnd := thisStart

	thisArgs := append([]any{thisStart, until}, args...)
	prevArgs := append([]any{prevStart, prevEnd}, args...)

	var thisCost, prevCost sql.NullFloat64
	err := r.DB.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT COALESCE(SUM(cost_usd), 0) FROM agent_runs_olap
		WHERE started_at >= ? AND started_at < ?%s
	`, clause), thisArgs...).Scan(&thisCost)
	if err != nil {
		return 0, 0, fmt.Errorf("hero cost (this): %w", err)
	}
	err = r.DB.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT COALESCE(SUM(cost_usd), 0) FROM agent_runs_olap
		WHERE started_at >= ? AND started_at < ?%s
	`, clause), prevArgs...).Scan(&prevCost)
	if err != nil {
		return 0, 0, fmt.Errorf("hero cost (prev): %w", err)
	}
	// `since` is unused; it only exists on the interface for symmetry
	// with the other Query* methods and future range-based rollups.
	_ = since
	return nullFloat(thisCost), nullFloat(prevCost), nil
}

// QueryHeroMTTR returns MTTR (mean time to recovery) for the current
// 7-day window and the previous 7-day window. Defined as the average
// time from first failure to next success per bead within the window.
func (r *SQLReader) QueryHeroMTTR(ctx context.Context, scope Scope, since, until time.Time) (float64, float64, error) {
	if r.DB == nil {
		return 0, 0, errNoDB
	}
	clause, args := scope.beadIDClause("bead_id")
	thisStart := until.Add(-7 * 24 * time.Hour)
	prevStart := until.Add(-14 * 24 * time.Hour)
	prevEnd := thisStart

	q := fmt.Sprintf(`
		SELECT COALESCE(AVG(recovery_s), 0) FROM (
			SELECT
				epoch(MIN(CASE WHEN result = 'success' THEN completed_at END)) -
				epoch(MIN(CASE WHEN result NOT IN ('success', 'skipped') THEN started_at END)) AS recovery_s
			FROM agent_runs_olap
			WHERE started_at >= ? AND started_at < ?
			  AND bead_id IS NOT NULL%s
			GROUP BY bead_id
		) WHERE recovery_s IS NOT NULL AND recovery_s > 0
	`, clause)

	var this, prev sql.NullFloat64
	err := r.DB.QueryRowContext(ctx, q, append([]any{thisStart, until}, args...)...).Scan(&this)
	if err != nil {
		return 0, 0, fmt.Errorf("hero mttr (this): %w", err)
	}
	err = r.DB.QueryRowContext(ctx, q, append([]any{prevStart, prevEnd}, args...)...).Scan(&prev)
	if err != nil {
		return 0, 0, fmt.Errorf("hero mttr (prev): %w", err)
	}
	_ = since
	return nullFloat(this), nullFloat(prev), nil
}
