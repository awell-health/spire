//go:build cgo

package duckdb

import (
	"context"
	"database/sql"
	"time"

	"github.com/awell-health/spire/pkg/olap"
)

// QueryDORA computes DORA metrics from weekly_merge_stats and agent_runs_olap.
func (d *DB) QueryDORA(since time.Time) (*olap.DORAMetrics, error) {
	ctx := context.Background()
	m := &olap.DORAMetrics{}

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
func (d *DB) QuerySummary(since time.Time) (*olap.SummaryStats, error) {
	ctx := context.Background()
	s := &olap.SummaryStats{}

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
func (d *DB) QueryModelBreakdown(since time.Time) ([]olap.ModelStats, error) {
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

	var out []olap.ModelStats
	for rows.Next() {
		var s olap.ModelStats
		if err := rows.Scan(&s.Model, &s.RunCount, &s.SuccessRate, &s.AvgCostUSD, &s.AvgDurationS, &s.TotalTokens); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// QueryPhaseBreakdown returns per-phase aggregated statistics.
func (d *DB) QueryPhaseBreakdown(since time.Time) ([]olap.PhaseStats, error) {
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

	var out []olap.PhaseStats
	for rows.Next() {
		var s olap.PhaseStats
		if err := rows.Scan(&s.Phase, &s.RunCount, &s.SuccessRate, &s.AvgCostUSD, &s.AvgDurationS); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// QueryTrends returns weekly aggregated metrics for trend display.
func (d *DB) QueryTrends(since time.Time) ([]olap.WeeklyTrend, error) {
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

	var out []olap.WeeklyTrend
	for rows.Next() {
		var t olap.WeeklyTrend
		if err := rows.Scan(&t.WeekStart, &t.RunCount, &t.SuccessRate, &t.TotalCostUSD, &t.MergeCount); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// QueryFailures returns failure breakdown by failure class.
func (d *DB) QueryFailures(since time.Time) ([]olap.FailureStats, error) {
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

	var out []olap.FailureStats
	for rows.Next() {
		var s olap.FailureStats
		if err := rows.Scan(&s.FailureClass, &s.Count, &s.Percentage); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// QueryToolUsage returns tool usage aggregates from the tool_usage_stats materialized view.
func (d *DB) QueryToolUsage(since time.Time) ([]olap.ToolUsageStats, error) {
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

	var out []olap.ToolUsageStats
	for rows.Next() {
		var s olap.ToolUsageStats
		if err := rows.Scan(&s.FormulaName, &s.Phase, &s.TotalRead, &s.TotalEdit, &s.TotalTools, &s.ReadRatio); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// QueryBugCausality returns the top failure hotspots ordered by attempt count.
func (d *DB) QueryBugCausality(limit int) ([]olap.BugCausality, error) {
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

	var out []olap.BugCausality
	for rows.Next() {
		var b olap.BugCausality
		if err := rows.Scan(&b.BeadID, &b.FailureClass, &b.AttemptCount, &b.LastFailure); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// QueryToolEvents returns aggregated tool event stats since the given time.
func (d *DB) QueryToolEvents(since time.Time) ([]olap.ToolEventStats, error) {
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

	var out []olap.ToolEventStats
	for rows.Next() {
		var s olap.ToolEventStats
		if err := rows.Scan(&s.ToolName, &s.Count, &s.AvgDurationMs, &s.FailureCount); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// QueryToolEventsByBead returns tool event stats for a specific bead, grouped by tool and step.
func (d *DB) QueryToolEventsByBead(beadID string) ([]olap.ToolEventStats, error) {
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

	var out []olap.ToolEventStats
	for rows.Next() {
		var s olap.ToolEventStats
		if err := rows.Scan(&s.ToolName, &s.Step, &s.Count, &s.AvgDurationMs, &s.FailureCount); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// QueryToolEventsByStep returns per-step tool breakdowns for the trace view.
func (d *DB) QueryToolEventsByStep(beadID string) ([]olap.StepToolBreakdown, error) {
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

	stepMap := make(map[string][]olap.ToolEventStats)
	var stepOrder []string
	for rows.Next() {
		var step string
		var s olap.ToolEventStats
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

	var out []olap.StepToolBreakdown
	for _, step := range stepOrder {
		out = append(out, olap.StepToolBreakdown{Step: step, Tools: stepMap[step]})
	}
	return out, nil
}

// QueryToolSpansByBead returns all spans for a bead, ordered by start_time.
// Used for the waterfall trace view.
func (d *DB) QueryToolSpansByBead(beadID string) ([]olap.SpanRecord, error) {
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

	return scanSpans(rows)
}

// QuerySpans returns every span for a single trace_id, ordered by start_time.
// Counterpart to QueryToolSpansByBead, used by the trace browser when the
// caller already has a trace_id (e.g. follow-up from QueryTraces).
func (d *DB) QuerySpans(ctx context.Context, traceID string) ([]olap.SpanRecord, error) {
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
		WHERE trace_id = ?
		ORDER BY start_time ASC
	`, traceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanSpans(rows)
}

// QueryTraces returns trace summaries matching the filter. Defaults to a 200-row
// cap when filter.Limit is 0; BeadID and the half-open Since/Until window narrow
// the result set. Each summary aggregates over all spans sharing a trace_id.
func (d *DB) QueryTraces(ctx context.Context, filter olap.TraceFilter) ([]olap.TraceSummary, error) {
	const defaultLimit = 200
	limit := filter.Limit
	if limit <= 0 {
		limit = defaultLimit
	}

	args := []any{}
	whereParts := []string{"trace_id IS NOT NULL", "trace_id != ''"}
	if filter.BeadID != "" {
		whereParts = append(whereParts, "bead_id = ?")
		args = append(args, filter.BeadID)
	}
	if !filter.Since.IsZero() {
		whereParts = append(whereParts, "start_time >= ?")
		args = append(args, filter.Since)
	}
	if !filter.Until.IsZero() {
		whereParts = append(whereParts, "start_time < ?")
		args = append(args, filter.Until)
	}
	args = append(args, limit)

	where := whereParts[0]
	for _, p := range whereParts[1:] {
		where += " AND " + p
	}

	q := `
		SELECT
			COALESCE(trace_id, '')                                  AS trace_id,
			COALESCE(MIN(bead_id), '')                              AS bead_id,
			COALESCE(MIN(CASE WHEN parent_span_id IS NULL OR parent_span_id = '' THEN span_name END), '') AS root_span,
			COUNT(*)                                                AS span_count,
			COALESCE(SUM(duration_ms), 0)                           AS duration_ms,
			MIN(start_time)                                         AS start_time,
			MAX(end_time)                                           AS end_time,
			COALESCE(BOOL_AND(success), TRUE)                       AS success
		FROM tool_spans
		WHERE ` + where + `
		GROUP BY trace_id
		ORDER BY start_time DESC
		LIMIT ?
	`
	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []olap.TraceSummary
	for rows.Next() {
		var s olap.TraceSummary
		if err := rows.Scan(&s.TraceID, &s.BeadID, &s.RootSpan, &s.SpanCount,
			&s.DurationMs, &s.StartTime, &s.EndTime, &s.Success); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// scanSpans drains rows into a []olap.SpanRecord using the column ordering of
// the QuerySpans / QueryToolSpansByBead SELECT lists. Both call sites share the
// same projection so the two paths cannot drift.
func scanSpans(rows *sql.Rows) ([]olap.SpanRecord, error) {
	var out []olap.SpanRecord
	for rows.Next() {
		var s olap.SpanRecord
		if err := rows.Scan(&s.TraceID, &s.SpanID, &s.ParentSpanID, &s.SpanName,
			&s.Kind, &s.DurationMs, &s.Success, &s.StartTime, &s.EndTime, &s.Attributes); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// QueryAPIEventsByBead returns aggregated API event stats for a bead.
func (d *DB) QueryAPIEventsByBead(beadID string) ([]olap.APIEventStats, error) {
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

	var out []olap.APIEventStats
	for rows.Next() {
		var s olap.APIEventStats
		if err := rows.Scan(&s.Model, &s.Count, &s.AvgDurationMs, &s.TotalCostUSD,
			&s.TotalInputTokens, &s.TotalOutputTokens); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// QueryCostTrend returns daily cost and run count for the last N days.
func (d *DB) QueryCostTrend(days int) ([]olap.CostTrendPoint, error) {
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

	var out []olap.CostTrendPoint
	for rows.Next() {
		var p olap.CostTrendPoint
		if err := rows.Scan(&p.Date, &p.TotalCost, &p.RunCount, &p.PromptTokens, &p.CompletionTokens); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
