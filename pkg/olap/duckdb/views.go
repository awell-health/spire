//go:build cgo

package duckdb

import (
	"context"
	"database/sql"
	"fmt"
)

// viewRetentionDays is the rolling window (in days) for all materialized views.
// CLI and board queries assume a 90-day horizon; changing this single constant
// keeps every view definition in sync with that assumption.
const viewRetentionDays = 90

// RefreshMaterializedViews rebuilds the daily/weekly aggregation tables
// from the last N days of data in agent_runs_olap. Called after each ETL sync.
func RefreshMaterializedViews(ctx context.Context, db *DB) error {
	for _, v := range viewRefreshStatements() {
		if _, err := db.db.ExecContext(ctx, v); err != nil {
			return fmt.Errorf("olap refresh view: %w", err)
		}
	}
	return nil
}

// RefreshMaterializedViewsTx is the transaction-aware variant of
// RefreshMaterializedViews. Used by the path-based ETL (WriteFunc pattern)
// where all writes happen inside a transaction.
func RefreshMaterializedViewsTx(ctx context.Context, tx *sql.Tx) error {
	for _, v := range viewRefreshStatements() {
		if _, err := tx.ExecContext(ctx, v); err != nil {
			return fmt.Errorf("olap refresh view: %w", err)
		}
	}
	return nil
}

func viewRefreshStatements() []string {
	return []string{
		// daily_formula_stats: delete + re-aggregate
		fmt.Sprintf(`DELETE FROM daily_formula_stats WHERE date >= current_date - INTERVAL %d DAY`, viewRetentionDays),
		fmt.Sprintf(`INSERT INTO daily_formula_stats
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
				AVG(CASE WHEN review_rounds > 0 THEN review_rounds END) AS avg_review_rounds
			FROM agent_runs_olap
			WHERE started_at >= current_date - INTERVAL %d DAY
			GROUP BY 1, 2, 3, 4, 5
		ON CONFLICT (date, formula_name, formula_version, tower, repo)
		DO UPDATE SET
			run_count = EXCLUDED.run_count,
			success_count = EXCLUDED.success_count,
			total_cost_usd = EXCLUDED.total_cost_usd,
			avg_duration_s = EXCLUDED.avg_duration_s,
			avg_review_rounds = EXCLUDED.avg_review_rounds`, viewRetentionDays),

		// weekly_merge_stats: delete + re-aggregate
		// Uses per-bead subquery to correctly compute DORA metrics:
		//   merge_count: distinct beads with approved review (sage-review or arbiter)
		//   failure_count: distinct beads with failures in agent-spawned phases
		//   avg_lead_time_s: avg time from first run to successful completion per bead
		//
		// Phase names come from the executor's recordAgentRun calls:
		//   'sage-review' — sage code review (result='approve' on approval)
		//   'review'      — arbiter escalation (result='success' on completion)
		//   'implement'   — apprentice implementation
		//   'fix'         — review-fix round
		// Op-kind steps (merge, close) don't spawn agents and have no agent_runs records.
		fmt.Sprintf(`DELETE FROM weekly_merge_stats WHERE week_start >= current_date - INTERVAL %d DAY`, viewRetentionDays),
		fmt.Sprintf(`INSERT INTO weekly_merge_stats
			SELECT
				week_start,
				tower,
				repo,
				SUM(is_merge) AS merge_count,
				SUM(is_failure) AS failure_count,
				AVG(CASE WHEN is_merge = 1 AND lead_time_s > 0 THEN lead_time_s END) AS avg_lead_time_s
			FROM (
				SELECT
					date_trunc('week', MIN(started_at))::DATE AS week_start,
					COALESCE(tower, '') AS tower,
					COALESCE(repo, '')  AS repo,
					MAX(CASE WHEN result IN ('success', 'approve') AND phase IN ('sage-review', 'review') THEN 1 ELSE 0 END) AS is_merge,
					MAX(CASE WHEN result NOT IN ('success', 'skipped', 'approve', 'no_changes', '') AND phase IN ('implement', 'sage-review', 'review', 'fix') THEN 1 ELSE 0 END) AS is_failure,
					epoch(MAX(CASE WHEN result IN ('success', 'approve') THEN completed_at END)) - epoch(MIN(started_at)) AS lead_time_s
				FROM agent_runs_olap
				WHERE started_at >= current_date - INTERVAL %d DAY
				  AND bead_id IS NOT NULL
				  AND bead_id != ''
				GROUP BY bead_id, COALESCE(tower, ''), COALESCE(repo, '')
			) per_bead
			GROUP BY week_start, tower, repo
		ON CONFLICT (week_start, tower, repo)
		DO UPDATE SET
			merge_count = EXCLUDED.merge_count,
			failure_count = EXCLUDED.failure_count,
			avg_lead_time_s = EXCLUDED.avg_lead_time_s`, viewRetentionDays),

		// phase_cost_breakdown: delete + re-aggregate
		fmt.Sprintf(`DELETE FROM phase_cost_breakdown WHERE date >= current_date - INTERVAL %d DAY`, viewRetentionDays),
		fmt.Sprintf(`INSERT INTO phase_cost_breakdown
			SELECT
				date_trunc('day', started_at)::DATE AS date,
				COALESCE(tower, '')        AS tower,
				COALESCE(formula_name, '') AS formula_name,
				COALESCE(phase, '')        AS phase,
				COUNT(*)      AS run_count,
				SUM(cost_usd) AS total_cost
			FROM agent_runs_olap
			WHERE started_at >= current_date - INTERVAL %d DAY
			GROUP BY 1, 2, 3, 4
		ON CONFLICT (date, tower, formula_name, phase)
		DO UPDATE SET
			run_count = EXCLUDED.run_count,
			total_cost = EXCLUDED.total_cost`, viewRetentionDays),

		// tool_usage_stats: delete + re-aggregate
		fmt.Sprintf(`DELETE FROM tool_usage_stats WHERE date >= current_date - INTERVAL %d DAY`, viewRetentionDays),
		fmt.Sprintf(`INSERT INTO tool_usage_stats
			SELECT
				date_trunc('day', started_at)::DATE AS date,
				COALESCE(tower, '') AS tower,
				COALESCE(formula_name, '') AS formula_name,
				COALESCE(phase, '') AS phase,
				COUNT(*) AS total_runs,
				SUM(COALESCE(read_calls, 0)) AS total_read,
				SUM(COALESCE(edit_calls, 0)) AS total_edit,
				SUM(COALESCE(read_calls, 0) + COALESCE(edit_calls, 0)) AS total_tools
			FROM agent_runs_olap
			WHERE started_at >= current_date - INTERVAL %d DAY
			GROUP BY 1, 2, 3, 4
		ON CONFLICT (date, tower, formula_name, phase)
		DO UPDATE SET
			total_runs = EXCLUDED.total_runs,
			total_read = EXCLUDED.total_read,
			total_edit = EXCLUDED.total_edit,
			total_tools = EXCLUDED.total_tools`, viewRetentionDays),

		// failure_hotspots: delete + re-aggregate
		// Excludes non-failure results: success, skipped, approve (sage verdict),
		// no_changes, and empty string. Uses NULLIF to coalesce empty failure_class
		// to 'unknown' (classifyFailure returns '' for non-failures that slip through).
		fmt.Sprintf(`DELETE FROM failure_hotspots WHERE week_start >= current_date - INTERVAL %d DAY`, viewRetentionDays),
		fmt.Sprintf(`INSERT INTO failure_hotspots
			SELECT
				date_trunc('week', started_at)::DATE AS week_start,
				COALESCE(tower, '') AS tower,
				COALESCE(bead_id, '') AS bead_id,
				COALESCE(NULLIF(failure_class, ''), 'unknown') AS failure_class,
				COUNT(*) AS attempt_count,
				MAX(started_at) AS last_failure_at
			FROM agent_runs_olap
			WHERE started_at >= current_date - INTERVAL %d DAY
			  AND result NOT IN ('success', 'skipped', 'approve', 'no_changes', '')
			GROUP BY 1, 2, 3, 4
		ON CONFLICT (week_start, tower, bead_id, failure_class)
		DO UPDATE SET
			attempt_count = EXCLUDED.attempt_count,
			last_failure_at = EXCLUDED.last_failure_at`, viewRetentionDays),
	}
}
