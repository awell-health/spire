package olap

import (
	"context"
	"fmt"
)

// RefreshMaterializedViews rebuilds the daily/weekly aggregation tables
// from the last 7 days of data in agent_runs_olap. Called after each ETL sync.
func RefreshMaterializedViews(ctx context.Context, db *DB) error {
	for _, v := range viewRefreshStatements() {
		if _, err := db.db.ExecContext(ctx, v); err != nil {
			return fmt.Errorf("olap refresh view: %w", err)
		}
	}
	return nil
}

func viewRefreshStatements() []string {
	return []string{
		// daily_formula_stats: delete last 7 days and re-aggregate
		`DELETE FROM daily_formula_stats WHERE date >= current_date - INTERVAL 7 DAY`,
		`INSERT INTO daily_formula_stats
			SELECT
				date_trunc('day', started_at)::DATE AS date,
				COALESCE(formula_name, '')  AS formula_name,
				COALESCE(formula_version, '') AS formula_version,
				COALESCE(tower, '')         AS tower,
				COALESCE(repo, '')          AS repo,
				COUNT(*)                                            AS run_count,
				SUM(CASE WHEN result = 'success' THEN 1 ELSE 0 END) AS success_count,
				SUM(cost_usd)                                       AS total_cost_usd,
				AVG(duration_seconds)                                AS avg_duration_s,
				AVG(review_rounds)                                   AS avg_review_rounds
			FROM agent_runs_olap
			WHERE started_at >= current_date - INTERVAL 7 DAY
			GROUP BY 1, 2, 3, 4, 5
		ON CONFLICT (date, formula_name, formula_version, tower, repo)
		DO UPDATE SET
			run_count = EXCLUDED.run_count,
			success_count = EXCLUDED.success_count,
			total_cost_usd = EXCLUDED.total_cost_usd,
			avg_duration_s = EXCLUDED.avg_duration_s,
			avg_review_rounds = EXCLUDED.avg_review_rounds`,

		// weekly_merge_stats: delete last 4 weeks and re-aggregate
		`DELETE FROM weekly_merge_stats WHERE week_start >= current_date - INTERVAL 28 DAY`,
		`INSERT INTO weekly_merge_stats
			SELECT
				date_trunc('week', started_at)::DATE AS week_start,
				COALESCE(tower, '') AS tower,
				COALESCE(repo, '')  AS repo,
				SUM(CASE WHEN result = 'success' AND phase = 'merge' THEN 1 ELSE 0 END) AS merge_count,
				SUM(CASE WHEN result != 'success' THEN 1 ELSE 0 END)                     AS failure_count,
				AVG(duration_seconds)                                                      AS avg_lead_time_s
			FROM agent_runs_olap
			WHERE started_at >= current_date - INTERVAL 28 DAY
			GROUP BY 1, 2, 3
		ON CONFLICT (week_start, tower, repo)
		DO UPDATE SET
			merge_count = EXCLUDED.merge_count,
			failure_count = EXCLUDED.failure_count,
			avg_lead_time_s = EXCLUDED.avg_lead_time_s`,

		// phase_cost_breakdown: delete last 7 days and re-aggregate
		`DELETE FROM phase_cost_breakdown WHERE date >= current_date - INTERVAL 7 DAY`,
		`INSERT INTO phase_cost_breakdown
			SELECT
				date_trunc('day', started_at)::DATE AS date,
				COALESCE(tower, '')        AS tower,
				COALESCE(formula_name, '') AS formula_name,
				COALESCE(phase, '')        AS phase,
				COUNT(*)      AS run_count,
				SUM(cost_usd) AS total_cost
			FROM agent_runs_olap
			WHERE started_at >= current_date - INTERVAL 7 DAY
			GROUP BY 1, 2, 3, 4
		ON CONFLICT (date, tower, formula_name, phase)
		DO UPDATE SET
			run_count = EXCLUDED.run_count,
			total_cost = EXCLUDED.total_cost`,
	}
}
