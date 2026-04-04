package steward

import (
	"context"
	"database/sql"
	"time"
)

// MetricsSnapshot holds a point-in-time snapshot of agent run metrics,
// collected from the Dolt agent_runs table.
type MetricsSnapshot struct {
	// Counters
	TotalRuns      int64
	SuccessfulRuns int64
	FailedRuns     int64
	ActiveRuns     int64

	// DORA-adjacent
	MergesLast7Days  int64
	MergesLast30Days int64

	// Cost
	TotalTokensAllTime  int64
	TotalCostUSDAllTime float64

	// Per-formula breakdown
	FormulaStats []FormulaMetric
}

// FormulaMetric holds aggregated metrics for a single formula name+version.
type FormulaMetric struct {
	FormulaName    string
	FormulaVersion string
	RunCount       int64
	SuccessCount   int64
	AvgCostUSD     float64
	AvgDurationSec float64
}

// CollectMetrics executes SQL queries against agent_runs and populates a MetricsSnapshot.
// Uses a 5-second context deadline to avoid slow query hangs.
func CollectMetrics(ctx context.Context, db *sql.DB) (MetricsSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var s MetricsSnapshot

	// Query 1: aggregate run counts and cost totals.
	row := db.QueryRowContext(ctx, `
		SELECT
			COUNT(*) AS total,
			SUM(CASE WHEN result = 'success' THEN 1 ELSE 0 END),
			SUM(CASE WHEN result = 'failed'  THEN 1 ELSE 0 END),
			SUM(CASE WHEN result = 'running' THEN 1 ELSE 0 END),
			COALESCE(SUM(total_tokens), 0),
			COALESCE(SUM(cost_usd), 0.0)
		FROM agent_runs
	`)
	if err := row.Scan(&s.TotalRuns, &s.SuccessfulRuns, &s.FailedRuns,
		&s.ActiveRuns, &s.TotalTokensAllTime, &s.TotalCostUSDAllTime); err != nil {
		return s, err
	}

	// Query 2: merge frequency (phase='merge', result='success').
	row = db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN completed_at >= DATE_SUB(NOW(), INTERVAL 7 DAY) THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN completed_at >= DATE_SUB(NOW(), INTERVAL 30 DAY) THEN 1 ELSE 0 END), 0)
		FROM agent_runs
		WHERE phase = 'merge' AND result = 'success'
	`)
	if err := row.Scan(&s.MergesLast7Days, &s.MergesLast30Days); err != nil {
		return s, err
	}

	// Query 3: per-formula breakdown.
	rows, err := db.QueryContext(ctx, `
		SELECT
			COALESCE(formula_name, 'unknown'),
			COALESCE(CAST(formula_version AS CHAR), '0'),
			COUNT(*) AS run_count,
			SUM(CASE WHEN result = 'success' THEN 1 ELSE 0 END) AS success_count,
			COALESCE(AVG(cost_usd), 0.0) AS avg_cost,
			COALESCE(AVG(duration_seconds), 0) AS avg_duration
		FROM agent_runs
		WHERE formula_name IS NOT NULL AND formula_name != ''
		GROUP BY formula_name, formula_version
		ORDER BY run_count DESC
	`)
	if err != nil {
		return s, err
	}
	defer rows.Close()

	for rows.Next() {
		var f FormulaMetric
		if err := rows.Scan(&f.FormulaName, &f.FormulaVersion,
			&f.RunCount, &f.SuccessCount, &f.AvgCostUSD, &f.AvgDurationSec); err != nil {
			return s, err
		}
		s.FormulaStats = append(s.FormulaStats, f)
	}

	return s, rows.Err()
}
