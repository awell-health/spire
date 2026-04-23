package olap

// ClickHouse-compatible aggregate refresh DDL.
//
// views.go holds the DuckDB versions which use DELETE + INSERT … ON CONFLICT
// and DuckDB-specific functions like epoch() and INTERVAL N DAY. ClickHouse
// has no ON CONFLICT; instead the aggregate tables are ReplacingMergeTree
// keyed on synced_at (see clickhouse_schema.go) so a repeated INSERT for the
// same primary key deduplicates during background merges.
//
// Date arithmetic uses today() - N and toDate / toStartOfWeek; timestamp
// differences use dateDiff('second', …) instead of epoch().
//
// These statements are not currently wired into the refresh loop (the only
// production caller today is the DuckDB path via RefreshMaterializedViews).
// They are kept here so the ClickHouse backend has parity DDL ready for the
// cluster-side refresh driver; the smoke tower's primary read path is
// agent_runs_olap directly, not the aggregate tables.

func clickHouseViewRefreshStatements() []string {
	return []string{
		// daily_formula_stats — aggregate last viewRetentionDays days of runs.
		// Inserts a new "version" of each (date, formula, tower, repo) row;
		// ReplacingMergeTree keeps the latest by synced_at during merges.
		`INSERT INTO daily_formula_stats
			(date, formula_name, formula_version, tower, repo,
			 run_count, success_count, total_cost_usd, avg_duration_s, avg_review_rounds)
			SELECT
				toDate(started_at) AS date,
				formula_name,
				formula_version,
				tower,
				repo,
				count() AS run_count,
				countIf(result = 'success') AS success_count,
				sum(cost_usd) AS total_cost_usd,
				avg(duration_seconds) AS avg_duration_s,
				avgIf(review_rounds, review_rounds > 0) AS avg_review_rounds
			FROM agent_runs_olap
			WHERE started_at >= today() - 90
			GROUP BY date, formula_name, formula_version, tower, repo`,

		// weekly_merge_stats — DORA-style per-bead aggregation.
		`INSERT INTO weekly_merge_stats
			(week_start, tower, repo, merge_count, failure_count, avg_lead_time_s)
			SELECT
				week_start,
				tower,
				repo,
				sum(is_merge) AS merge_count,
				sum(is_failure) AS failure_count,
				avgIf(lead_time_s, is_merge = 1 AND lead_time_s > 0) AS avg_lead_time_s
			FROM (
				SELECT
					toStartOfWeek(min(started_at)) AS week_start,
					tower,
					repo,
					maxIf(1, result IN ('success', 'approve') AND phase IN ('sage-review', 'review')) AS is_merge,
					maxIf(1, result NOT IN ('success', 'skipped', 'approve', 'no_changes', '') AND phase IN ('implement', 'sage-review', 'review', 'fix')) AS is_failure,
					dateDiff('second', min(started_at), maxIf(completed_at, result IN ('success', 'approve'))) AS lead_time_s
				FROM agent_runs_olap
				WHERE started_at >= today() - 90
				  AND bead_id != ''
				GROUP BY bead_id, tower, repo
			)
			GROUP BY week_start, tower, repo`,

		// phase_cost_breakdown
		`INSERT INTO phase_cost_breakdown
			(date, tower, formula_name, phase, run_count, total_cost)
			SELECT
				toDate(started_at) AS date,
				tower,
				formula_name,
				phase,
				count() AS run_count,
				sum(cost_usd) AS total_cost
			FROM agent_runs_olap
			WHERE started_at >= today() - 90
			GROUP BY date, tower, formula_name, phase`,

		// tool_usage_stats
		`INSERT INTO tool_usage_stats
			(date, tower, formula_name, phase, total_runs, total_read, total_edit, total_tools)
			SELECT
				toDate(started_at) AS date,
				tower,
				formula_name,
				phase,
				count() AS total_runs,
				sum(read_calls) AS total_read,
				sum(edit_calls) AS total_edit,
				sum(read_calls + edit_calls) AS total_tools
			FROM agent_runs_olap
			WHERE started_at >= today() - 90
			GROUP BY date, tower, formula_name, phase`,

		// failure_hotspots — per-bead weekly failure aggregation.
		`INSERT INTO failure_hotspots
			(week_start, tower, bead_id, failure_class, attempt_count, last_failure_at)
			SELECT
				toStartOfWeek(started_at) AS week_start,
				tower,
				bead_id,
				if(failure_class = '', 'unknown', failure_class) AS failure_class,
				count() AS attempt_count,
				max(started_at) AS last_failure_at
			FROM agent_runs_olap
			WHERE started_at >= today() - 90
			  AND result NOT IN ('success', 'skipped', 'approve', 'no_changes', '')
			GROUP BY week_start, tower, bead_id, failure_class`,
	}
}
