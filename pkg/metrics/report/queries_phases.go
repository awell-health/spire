package report

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// QueryPhases returns the Phase Funnel panel rows: runs / successRate
// / avg cost / avg duration / reachedFromStart (fraction of beads
// that reached this phase at least once).
func (r *SQLReader) QueryPhases(ctx context.Context, scope Scope, since, until time.Time) ([]PhaseRow, error) {
	if r.DB == nil {
		return nil, errNoDB
	}
	clause, scopeArgs := scope.beadIDClause("bead_id")

	// First: per-phase aggregates.
	aggSQL := fmt.Sprintf(`
		SELECT
			COALESCE(phase, '') AS phase,
			COUNT(*) AS runs,
			COALESCE(
				SUM(CASE WHEN result = 'success' THEN 1 ELSE 0 END)::DOUBLE
				/ NULLIF(COUNT(*), 0), 0) AS success_rate,
			COALESCE(AVG(cost_usd), 0) AS avg_cost_usd,
			COALESCE(AVG(duration_seconds), 0) AS avg_duration_s
		FROM agent_runs_olap
		WHERE started_at >= ? AND started_at <= ?
		  AND phase IS NOT NULL AND phase != ''%s
		GROUP BY 1
		ORDER BY runs DESC
	`, clause)
	rows, err := r.DB.QueryContext(ctx, aggSQL, append([]any{since, until}, scopeArgs...)...)
	if err != nil {
		return nil, fmt.Errorf("phases agg: %w", err)
	}

	var phases []PhaseRow
	phaseIdx := make(map[string]int)
	for rows.Next() {
		var (
			p          PhaseRow
			successRt  sql.NullFloat64
			avgCost    sql.NullFloat64
			avgDur     sql.NullFloat64
		)
		if err := rows.Scan(&p.Phase, &p.Runs, &successRt, &avgCost, &avgDur); err != nil {
			rows.Close()
			return nil, err
		}
		p.SuccessRate = nullFloat(successRt)
		p.AvgCostUSD = nullFloat(avgCost)
		p.AvgDurationSeconds = nullFloat(avgDur)
		phaseIdx[p.Phase] = len(phases)
		phases = append(phases, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// reachedFromStart: distinct beads that touched this phase /
	// distinct beads that touched ANY phase. Matches the fraction
	// semantics in the TS contract.
	totalSQL := fmt.Sprintf(`
		SELECT COUNT(DISTINCT bead_id) FROM agent_runs_olap
		WHERE started_at >= ? AND started_at <= ?
		  AND bead_id IS NOT NULL AND bead_id != ''%s
	`, clause)
	var totalBeads sql.NullInt64
	if err := r.DB.QueryRowContext(ctx, totalSQL, append([]any{since, until}, scopeArgs...)...).Scan(&totalBeads); err != nil {
		return nil, fmt.Errorf("phases total beads: %w", err)
	}

	reachedSQL := fmt.Sprintf(`
		SELECT phase, COUNT(DISTINCT bead_id) FROM agent_runs_olap
		WHERE started_at >= ? AND started_at <= ?
		  AND bead_id IS NOT NULL AND bead_id != ''
		  AND phase IS NOT NULL AND phase != ''%s
		GROUP BY 1
	`, clause)
	rrows, err := r.DB.QueryContext(ctx, reachedSQL, append([]any{since, until}, scopeArgs...)...)
	if err != nil {
		return nil, fmt.Errorf("phases reached: %w", err)
	}
	defer rrows.Close()

	total := nullInt64(totalBeads)
	for rrows.Next() {
		var (
			phase string
			distinctBeads int64
		)
		if err := rrows.Scan(&phase, &distinctBeads); err != nil {
			return nil, err
		}
		idx, ok := phaseIdx[phase]
		if !ok {
			continue
		}
		if total > 0 {
			phases[idx].ReachedFromStart = float64(distinctBeads) / float64(total)
		}
	}
	if err := rrows.Err(); err != nil {
		return nil, err
	}
	return phases, nil
}
