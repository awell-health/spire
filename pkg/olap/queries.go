package olap

import (
	"context"
	"database/sql"
	"time"
)

// Shared types used by the queries below (DORAMetrics, SummaryStats,
// ModelStats, PhaseStats, WeeklyTrend, FailureStats, ToolUsageStats,
// BugCausality, CostTrendPoint, ToolEventStats, StepToolBreakdown,
// SpanRecord, APIEventStats) are declared in olap.go so both backend
// subpackages can consume them without import cycles.

// QueryDORA computes DORA metrics from weekly_merge_stats and agent_runs_olap.
func (d *DB) QueryDORA(since time.Time) (*DORAMetrics, error) {
	ctx := context.Background()
	m := &DORAMetrics{}

	// Deploy frequency, lead time, failure rate from weekly_merge_stats
	err := d.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(AVG(merge_count), 0),
			COALESCE(AVG(avg_lead_time_s), 0),
			CASE WHEN COALESCE(SUM(merge_count) + SUM(failure_count), 0) = 0 THEN 0.0
			     ELSE SUM(failure_count)::DOUBLE / (SUM(merge_count) + SUM(failure_count))
			END
		FROM weekly_merge_stats
		WHERE week_start >= ?
	`, since).Scan(&m.DeployFrequency, &m.LeadTimeSeconds, &m.ChangeFailureRate)
	if err != nil {
		return nil, err
	}

	// MTTR: avg time from first failure to next success per bead
	err = d.db.QueryRowContext(ctx, `
		SELECT COALESCE(AVG(recovery_s), 0) FROM (
			SELECT recovery_s FROM (
				SELECT
					bead_id,
					epoch(MIN(CASE WHEN result = 'success' THEN completed_at END)) -
					epoch(MIN(CASE WHEN result NOT IN ('success', 'skipped') THEN started_at END)) AS recovery_s
				FROM agent_runs_olap
				WHERE started_at >= ?
				  AND bead_id IS NOT NULL
				GROUP BY bead_id
			) sub
			WHERE recovery_s IS NOT NULL AND recovery_s > 0
		)
	`, since).Scan(&m.MTTRSeconds)
	if err != nil {
		return nil, err
	}

	return m, nil
}

// QuerySummary returns overall run statistics since the given time.
func (d *DB) QuerySummary(since time.Time) (*SummaryStats, error) {
	ctx := context.Background()
	s := &SummaryStats{}

	var avgCost, avgDur, totalCost sql.NullFloat64
	var successRate sql.NullFloat64

	err := d.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN result = 'success' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN result NOT IN ('success', 'skipped') THEN 1 ELSE 0 END), 0),
			ROUND(100.0 * SUM(CASE WHEN result = 'success' THEN 1 ELSE 0 END) / NULLIF(COUNT(*), 0), 1),
			COALESCE(AVG(cost_usd), 0),
			COALESCE(AVG(duration_seconds), 0),
			COALESCE(SUM(cost_usd), 0)
		FROM agent_runs_olap
		WHERE started_at >= ?
	`, since).Scan(&s.TotalRuns, &s.Successes, &s.Failures, &successRate, &avgCost, &avgDur, &totalCost)
	if err != nil {
		return nil, err
	}

	if successRate.Valid {
		s.SuccessRate = successRate.Float64
	}
	if avgCost.Valid {
		s.AvgCostUSD = avgCost.Float64
	}
	if avgDur.Valid {
		s.AvgDurationS = avgDur.Float64
	}
	if totalCost.Valid {
		s.TotalCostUSD = totalCost.Float64
	}

	return s, nil
}

// QueryModelBreakdown returns per-model aggregated statistics.
func (d *DB) QueryModelBreakdown(since time.Time) ([]ModelStats, error) {
	ctx := context.Background()
	rows, err := d.db.QueryContext(ctx, `
		SELECT
			COALESCE(model, 'unknown') AS model,
			COUNT(*) AS run_count,
			ROUND(100.0 * SUM(CASE WHEN result = 'success' THEN 1 ELSE 0 END) / NULLIF(COUNT(*), 0), 1) AS success_rate,
			COALESCE(AVG(cost_usd), 0) AS avg_cost_usd,
			COALESCE(AVG(duration_seconds), 0) AS avg_duration_s,
			COALESCE(SUM(total_tokens), 0) AS total_tokens
		FROM agent_runs_olap
		WHERE started_at >= ?
		GROUP BY model
		ORDER BY run_count DESC
	`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ModelStats
	for rows.Next() {
		var s ModelStats
		if err := rows.Scan(&s.Model, &s.RunCount, &s.SuccessRate, &s.AvgCostUSD, &s.AvgDurationS, &s.TotalTokens); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// QueryPhaseBreakdown returns per-phase aggregated statistics.
func (d *DB) QueryPhaseBreakdown(since time.Time) ([]PhaseStats, error) {
	ctx := context.Background()
	rows, err := d.db.QueryContext(ctx, `
		SELECT
			COALESCE(phase, 'unknown') AS phase,
			COUNT(*) AS run_count,
			ROUND(100.0 * SUM(CASE WHEN result = 'success' THEN 1 ELSE 0 END) / NULLIF(COUNT(*), 0), 1) AS success_rate,
			COALESCE(AVG(cost_usd), 0) AS avg_cost_usd,
			COALESCE(AVG(duration_seconds), 0) AS avg_duration_s
		FROM agent_runs_olap
		WHERE started_at >= ?
		GROUP BY phase
		ORDER BY run_count DESC
	`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PhaseStats
	for rows.Next() {
		var s PhaseStats
		if err := rows.Scan(&s.Phase, &s.RunCount, &s.SuccessRate, &s.AvgCostUSD, &s.AvgDurationS); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// QueryTrends returns weekly aggregated metrics for trend display.
func (d *DB) QueryTrends(since time.Time) ([]WeeklyTrend, error) {
	ctx := context.Background()
	rows, err := d.db.QueryContext(ctx, `
		SELECT
			date_trunc('week', started_at)::DATE AS week_start,
			COUNT(*) AS run_count,
			ROUND(100.0 * SUM(CASE WHEN result = 'success' THEN 1 ELSE 0 END) / NULLIF(COUNT(*), 0), 1) AS success_rate,
			COALESCE(SUM(cost_usd), 0) AS total_cost_usd,
			COUNT(DISTINCT CASE WHEN result IN ('success', 'approve') AND phase IN ('sage-review', 'review') THEN bead_id END) AS merge_count
		FROM agent_runs_olap
		WHERE started_at >= ?
		GROUP BY 1
		ORDER BY 1 DESC
	`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []WeeklyTrend
	for rows.Next() {
		var t WeeklyTrend
		if err := rows.Scan(&t.WeekStart, &t.RunCount, &t.SuccessRate, &t.TotalCostUSD, &t.MergeCount); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// QueryFailures returns failure breakdown by failure class.
func (d *DB) QueryFailures(since time.Time) ([]FailureStats, error) {
	ctx := context.Background()
	rows, err := d.db.QueryContext(ctx, `
		SELECT
			COALESCE(NULLIF(failure_class, ''), 'unknown') AS failure_class,
			COUNT(*) AS count,
			ROUND(100.0 * COUNT(*) / NULLIF(SUM(COUNT(*)) OVER (), 0), 1) AS percentage
		FROM agent_runs_olap
		WHERE started_at >= ?
		  AND result NOT IN ('success', 'skipped', 'approve', 'no_changes', '')
		GROUP BY 1
		ORDER BY count DESC
	`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []FailureStats
	for rows.Next() {
		var s FailureStats
		if err := rows.Scan(&s.FailureClass, &s.Count, &s.Percentage); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// QueryToolUsage returns tool usage aggregates from the tool_usage_stats materialized view.
func (d *DB) QueryToolUsage(since time.Time) ([]ToolUsageStats, error) {
	ctx := context.Background()
	rows, err := d.db.QueryContext(ctx, `
		SELECT
			formula_name,
			phase,
			SUM(total_read) AS total_read,
			SUM(total_edit) AS total_edit,
			SUM(total_tools) AS total_tools,
			CASE WHEN SUM(total_tools) = 0 THEN 0.0
			     ELSE ROUND(SUM(total_read)::DOUBLE / SUM(total_tools), 3)
			END AS read_ratio
		FROM tool_usage_stats
		WHERE date >= ?::DATE
		GROUP BY formula_name, phase
		ORDER BY read_ratio DESC, SUM(total_tools) DESC, formula_name ASC, phase ASC
	`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ToolUsageStats
	for rows.Next() {
		var s ToolUsageStats
		if err := rows.Scan(&s.FormulaName, &s.Phase, &s.TotalRead, &s.TotalEdit, &s.TotalTools, &s.ReadRatio); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// QueryBugCausality returns the top failure hotspots ordered by attempt count.
func (d *DB) QueryBugCausality(limit int) ([]BugCausality, error) {
	ctx := context.Background()
	rows, err := d.db.QueryContext(ctx, `
		SELECT
			bead_id,
			failure_class,
			attempt_count,
			last_failure_at
		FROM failure_hotspots
		ORDER BY attempt_count DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []BugCausality
	for rows.Next() {
		var b BugCausality
		if err := rows.Scan(&b.BeadID, &b.FailureClass, &b.AttemptCount, &b.LastFailure); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// QueryToolEvents returns aggregated tool event stats since the given time.
func (d *DB) QueryToolEvents(since time.Time) ([]ToolEventStats, error) {
	ctx := context.Background()
	rows, err := d.db.QueryContext(ctx, `
		SELECT
			tool_name,
			COUNT(*) AS count,
			COALESCE(AVG(duration_ms), 0) AS avg_duration_ms,
			SUM(CASE WHEN NOT success THEN 1 ELSE 0 END) AS failure_count
		FROM tool_events
		WHERE timestamp >= ?
		GROUP BY tool_name
		ORDER BY count DESC
	`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ToolEventStats
	for rows.Next() {
		var s ToolEventStats
		if err := rows.Scan(&s.ToolName, &s.Count, &s.AvgDurationMs, &s.FailureCount); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// QueryToolEventsByBead returns tool event stats for a specific bead, grouped by tool and step.
func (d *DB) QueryToolEventsByBead(beadID string) ([]ToolEventStats, error) {
	ctx := context.Background()
	rows, err := d.db.QueryContext(ctx, `
		SELECT
			tool_name,
			COALESCE(step, '') AS step,
			COUNT(*) AS count,
			COALESCE(AVG(duration_ms), 0) AS avg_duration_ms,
			SUM(CASE WHEN NOT success THEN 1 ELSE 0 END) AS failure_count
		FROM tool_events
		WHERE bead_id = ?
		GROUP BY tool_name, step
		ORDER BY count DESC
	`, beadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ToolEventStats
	for rows.Next() {
		var s ToolEventStats
		if err := rows.Scan(&s.ToolName, &s.Step, &s.Count, &s.AvgDurationMs, &s.FailureCount); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// QueryToolEventsByStep returns per-step tool breakdowns for the trace view.
func (d *DB) QueryToolEventsByStep(beadID string) ([]StepToolBreakdown, error) {
	ctx := context.Background()
	rows, err := d.db.QueryContext(ctx, `
		SELECT
			COALESCE(step, 'unknown') AS step,
			tool_name,
			COUNT(*) AS count,
			COALESCE(AVG(duration_ms), 0) AS avg_duration_ms,
			SUM(CASE WHEN NOT success THEN 1 ELSE 0 END) AS failure_count
		FROM tool_events
		WHERE bead_id = ?
		GROUP BY step, tool_name
		ORDER BY step, count DESC
	`, beadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	stepMap := make(map[string][]ToolEventStats)
	var stepOrder []string
	for rows.Next() {
		var step string
		var s ToolEventStats
		if err := rows.Scan(&step, &s.ToolName, &s.Count, &s.AvgDurationMs, &s.FailureCount); err != nil {
			return nil, err
		}
		if _, exists := stepMap[step]; !exists {
			stepOrder = append(stepOrder, step)
		}
		stepMap[step] = append(stepMap[step], s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []StepToolBreakdown
	for _, step := range stepOrder {
		out = append(out, StepToolBreakdown{Step: step, Tools: stepMap[step]})
	}
	return out, nil
}

// QueryToolSpansByBead returns all spans for a bead, ordered by start_time.
// Used for the waterfall trace view.
func (d *DB) QueryToolSpansByBead(beadID string) ([]SpanRecord, error) {
	ctx := context.Background()
	rows, err := d.db.QueryContext(ctx, `
		SELECT
			COALESCE(trace_id, '') AS trace_id,
			COALESCE(span_id, '') AS span_id,
			COALESCE(parent_span_id, '') AS parent_span_id,
			COALESCE(span_name, '') AS span_name,
			COALESCE(kind, '') AS kind,
			COALESCE(duration_ms, 0) AS duration_ms,
			COALESCE(success, true) AS success,
			start_time,
			end_time,
			COALESCE(attributes, '{}') AS attributes
		FROM tool_spans
		WHERE bead_id = ?
		ORDER BY start_time ASC
	`, beadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SpanRecord
	for rows.Next() {
		var s SpanRecord
		if err := rows.Scan(&s.TraceID, &s.SpanID, &s.ParentSpanID, &s.SpanName,
			&s.Kind, &s.DurationMs, &s.Success, &s.StartTime, &s.EndTime, &s.Attributes); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// QueryAPIEventsByBead returns aggregated API event stats for a bead.
func (d *DB) QueryAPIEventsByBead(beadID string) ([]APIEventStats, error) {
	ctx := context.Background()
	rows, err := d.db.QueryContext(ctx, `
		SELECT
			COALESCE(model, 'unknown') AS model,
			COUNT(*) AS count,
			COALESCE(AVG(duration_ms), 0) AS avg_duration_ms,
			COALESCE(SUM(cost_usd), 0) AS total_cost_usd,
			COALESCE(SUM(input_tokens), 0) AS total_input_tokens,
			COALESCE(SUM(output_tokens), 0) AS total_output_tokens
		FROM api_events
		WHERE bead_id = ?
		GROUP BY model
		ORDER BY count DESC
	`, beadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []APIEventStats
	for rows.Next() {
		var s APIEventStats
		if err := rows.Scan(&s.Model, &s.Count, &s.AvgDurationMs, &s.TotalCostUSD,
			&s.TotalInputTokens, &s.TotalOutputTokens); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// QueryLifecycleForBead returns lifecycle timestamps and derived intervals
// for a single bead. Callers must tolerate zero-valued time fields (pre-
// feature beads may never have received ready/started stamps).
// Derived intervals are returned as *float64 so "missing" (bead not closed
// yet) is distinguishable from "zero" (instant close — rare but possible).
func (d *DB) QueryLifecycleForBead(beadID string) (*BeadLifecycleIntervals, error) {
	ctx := context.Background()
	var (
		out       BeadLifecycleIntervals
		beadType  sql.NullString
		filed     sql.NullTime
		ready     sql.NullTime
		started   sql.NullTime
		closed    sql.NullTime
		updated   sql.NullTime
		fileClose sql.NullFloat64
		readClose sql.NullFloat64
		startClos sql.NullFloat64
		queue     sql.NullFloat64
	)
	err := d.db.QueryRowContext(ctx, `
		SELECT
			bead_id,
			bead_type,
			filed_at,
			ready_at,
			started_at,
			closed_at,
			updated_at,
			CASE WHEN closed_at IS NOT NULL AND filed_at IS NOT NULL
			     THEN epoch(closed_at) - epoch(filed_at) END AS filed_to_closed_s,
			CASE WHEN closed_at IS NOT NULL AND ready_at IS NOT NULL
			     THEN epoch(closed_at) - epoch(ready_at) END AS ready_to_closed_s,
			CASE WHEN closed_at IS NOT NULL AND started_at IS NOT NULL
			     THEN epoch(closed_at) - epoch(started_at) END AS started_to_closed_s,
			CASE WHEN started_at IS NOT NULL AND ready_at IS NOT NULL
			     THEN epoch(started_at) - epoch(ready_at) END AS queue_s
		FROM bead_lifecycle_olap
		WHERE bead_id = ?
	`, beadID).Scan(
		&out.BeadID, &beadType,
		&filed, &ready, &started, &closed, &updated,
		&fileClose, &readClose, &startClos, &queue,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if beadType.Valid {
		out.BeadType = beadType.String
	}
	if filed.Valid {
		out.FiledAt = filed.Time
	}
	if ready.Valid {
		out.ReadyAt = ready.Time
	}
	if started.Valid {
		out.StartedAt = started.Time
	}
	if closed.Valid {
		out.ClosedAt = closed.Time
	}
	if updated.Valid {
		out.UpdatedAt = updated.Time
	}
	if fileClose.Valid {
		v := fileClose.Float64
		out.FiledToClosedSeconds = &v
	}
	if readClose.Valid {
		v := readClose.Float64
		out.ReadyToClosedSeconds = &v
	}
	if startClos.Valid {
		v := startClos.Float64
		out.StartedToClosedSeconds = &v
	}
	if queue.Valid {
		v := queue.Float64
		out.QueueSeconds = &v
	}
	return &out, nil
}

// QueryLifecycleByType returns P50/P95 timings per bead_type for beads closed
// in the window. Filters to closed_at IS NOT NULL so open beads don't skew
// the percentiles.
func (d *DB) QueryLifecycleByType(since time.Time) ([]LifecycleByType, error) {
	ctx := context.Background()
	rows, err := d.db.QueryContext(ctx, `
		SELECT
			COALESCE(bead_type, 'unknown') AS bead_type,
			COUNT(*) AS cnt,
			COALESCE(quantile_cont(epoch(closed_at) - epoch(filed_at), 0.50), 0) AS f2c_p50,
			COALESCE(quantile_cont(epoch(closed_at) - epoch(filed_at), 0.95), 0) AS f2c_p95,
			COALESCE(quantile_cont(
				CASE WHEN ready_at IS NOT NULL THEN epoch(closed_at) - epoch(ready_at) END, 0.50), 0) AS r2c_p50,
			COALESCE(quantile_cont(
				CASE WHEN ready_at IS NOT NULL THEN epoch(closed_at) - epoch(ready_at) END, 0.95), 0) AS r2c_p95,
			COALESCE(quantile_cont(
				CASE WHEN started_at IS NOT NULL THEN epoch(closed_at) - epoch(started_at) END, 0.50), 0) AS s2c_p50,
			COALESCE(quantile_cont(
				CASE WHEN started_at IS NOT NULL THEN epoch(closed_at) - epoch(started_at) END, 0.95), 0) AS s2c_p95,
			COALESCE(quantile_cont(
				CASE WHEN started_at IS NOT NULL AND ready_at IS NOT NULL
				     THEN epoch(started_at) - epoch(ready_at) END, 0.50), 0) AS q_p50,
			COALESCE(quantile_cont(
				CASE WHEN started_at IS NOT NULL AND ready_at IS NOT NULL
				     THEN epoch(started_at) - epoch(ready_at) END, 0.95), 0) AS q_p95
		FROM bead_lifecycle_olap
		WHERE closed_at IS NOT NULL
		  AND filed_at IS NOT NULL
		  AND closed_at >= ?
		GROUP BY 1
		ORDER BY cnt DESC
	`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LifecycleByType
	for rows.Next() {
		var r LifecycleByType
		if err := rows.Scan(
			&r.BeadType, &r.Count,
			&r.FiledToClosedP50, &r.FiledToClosedP95,
			&r.ReadyToClosedP50, &r.ReadyToClosedP95,
			&r.StartedToClosedP50, &r.StartedToClosedP95,
			&r.QueueP50, &r.QueueP95,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// QueryReviewFixCounts aggregates review / fix / arbiter run counts for a
// bead from agent_runs_olap (the live source of truth). Counts runs, not
// review-round beads, to match the phase/role vocabulary used elsewhere.
func (d *DB) QueryReviewFixCounts(beadID string) (*ReviewFixCounts, error) {
	ctx := context.Background()
	out := &ReviewFixCounts{BeadID: beadID}
	var maxRounds sql.NullInt64
	err := d.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN (role = 'sage' OR phase IN ('sage-review','review')) THEN 1 ELSE 0 END), 0) AS review_count,
			COALESCE(SUM(CASE WHEN phase = 'fix' THEN 1 ELSE 0 END), 0) AS fix_count,
			COALESCE(SUM(CASE WHEN role = 'arbiter' OR phase = 'arbiter' THEN 1 ELSE 0 END), 0) AS arbiter_count,
			MAX(review_rounds) AS max_review_rounds
		FROM agent_runs_olap
		WHERE bead_id = ?
	`, beadID).Scan(&out.ReviewCount, &out.FixCount, &out.ArbiterCount, &maxRounds)
	if err == sql.ErrNoRows {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	if maxRounds.Valid {
		out.MaxReviewRounds = int(maxRounds.Int64)
	}
	return out, nil
}

// QueryChildLifecycle returns lifecycle intervals for every bead whose
// bead_id starts with the parent's hierarchical prefix (parent-id + "."),
// ordered by filed_at. This surfaces step beads, attempt beads, and summoned
// sub-tasks under a wizard summon. Since the beads library owns dependency
// storage and we do not mirror `dependencies` into DuckDB, we rely on the
// hierarchical ID convention (`spi-xxx.1`, `spi-xxx.1.2`) — every child
// created via --parent inherits the parent's prefix.
func (d *DB) QueryChildLifecycle(parentID string) ([]BeadLifecycleIntervals, error) {
	ctx := context.Background()
	pattern := parentID + ".%"
	rows, err := d.db.QueryContext(ctx, `
		SELECT
			bead_id,
			COALESCE(bead_type, ''),
			filed_at,
			ready_at,
			started_at,
			closed_at,
			updated_at,
			CASE WHEN closed_at IS NOT NULL AND filed_at IS NOT NULL
			     THEN epoch(closed_at) - epoch(filed_at) END,
			CASE WHEN closed_at IS NOT NULL AND ready_at IS NOT NULL
			     THEN epoch(closed_at) - epoch(ready_at) END,
			CASE WHEN closed_at IS NOT NULL AND started_at IS NOT NULL
			     THEN epoch(closed_at) - epoch(started_at) END,
			CASE WHEN started_at IS NOT NULL AND ready_at IS NOT NULL
			     THEN epoch(started_at) - epoch(ready_at) END
		FROM bead_lifecycle_olap
		WHERE bead_id LIKE ?
		ORDER BY filed_at ASC, bead_id ASC
	`, pattern)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []BeadLifecycleIntervals
	for rows.Next() {
		var (
			r         BeadLifecycleIntervals
			filed     sql.NullTime
			ready     sql.NullTime
			started   sql.NullTime
			closed    sql.NullTime
			updated   sql.NullTime
			fileClose sql.NullFloat64
			readClose sql.NullFloat64
			startClos sql.NullFloat64
			queue     sql.NullFloat64
		)
		if err := rows.Scan(
			&r.BeadID, &r.BeadType,
			&filed, &ready, &started, &closed, &updated,
			&fileClose, &readClose, &startClos, &queue,
		); err != nil {
			return nil, err
		}
		if filed.Valid {
			r.FiledAt = filed.Time
		}
		if ready.Valid {
			r.ReadyAt = ready.Time
		}
		if started.Valid {
			r.StartedAt = started.Time
		}
		if closed.Valid {
			r.ClosedAt = closed.Time
		}
		if updated.Valid {
			r.UpdatedAt = updated.Time
		}
		if fileClose.Valid {
			v := fileClose.Float64
			r.FiledToClosedSeconds = &v
		}
		if readClose.Valid {
			v := readClose.Float64
			r.ReadyToClosedSeconds = &v
		}
		if startClos.Valid {
			v := startClos.Float64
			r.StartedToClosedSeconds = &v
		}
		if queue.Valid {
			v := queue.Float64
			r.QueueSeconds = &v
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// QueryCostTrend returns daily cost and run count for the last N days.
func (d *DB) QueryCostTrend(days int) ([]CostTrendPoint, error) {
	ctx := context.Background()
	since := time.Now().AddDate(0, 0, -days)

	rows, err := d.db.QueryContext(ctx, `
		SELECT
			date_trunc('day', started_at)::DATE AS date,
			COALESCE(SUM(cost_usd), 0) AS total_cost,
			COUNT(*) AS run_count,
			COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens,
			COALESCE(SUM(completion_tokens), 0) AS completion_tokens
		FROM agent_runs_olap
		WHERE started_at >= ?
		GROUP BY 1
		ORDER BY 1 DESC
	`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CostTrendPoint
	for rows.Next() {
		var p CostTrendPoint
		if err := rows.Scan(&p.Date, &p.TotalCost, &p.RunCount, &p.PromptTokens, &p.CompletionTokens); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
