package olap

import (
	"context"
	"database/sql"
	"time"
)

// DORAMetrics holds the four DORA performance metrics.
type DORAMetrics struct {
	DeployFrequency   float64 // deploys per week
	LeadTimeSeconds   float64 // avg seconds from first commit to deploy
	ChangeFailureRate float64 // ratio 0.0-1.0
	MTTRSeconds       float64 // mean time to recovery
}

// SummaryStats holds overall run statistics.
type SummaryStats struct {
	TotalRuns    int
	Successes    int
	Failures     int
	SuccessRate  float64
	AvgCostUSD   float64
	AvgDurationS float64
	TotalCostUSD float64
}

// ModelStats holds per-model aggregated statistics.
type ModelStats struct {
	Model        string
	RunCount     int
	SuccessRate  float64
	AvgCostUSD   float64
	AvgDurationS float64
	TotalTokens  int64
}

// PhaseStats holds per-phase aggregated statistics.
type PhaseStats struct {
	Phase        string
	RunCount     int
	SuccessRate  float64
	AvgCostUSD   float64
	AvgDurationS float64
}

// WeeklyTrend holds weekly aggregated metrics for trend display.
type WeeklyTrend struct {
	WeekStart    time.Time
	RunCount     int
	SuccessRate  float64
	TotalCostUSD float64
	MergeCount   int
}

// FailureStats holds failure breakdown by class.
type FailureStats struct {
	FailureClass string
	Count        int
	Percentage   float64
}

// ToolUsageStats holds tool call aggregates per formula/phase.
type ToolUsageStats struct {
	FormulaName string
	Phase       string
	TotalRead   int
	TotalEdit   int
	TotalTools  int
	ReadRatio   float64 // read / (read+edit)
}

// BugCausality holds failure hotspot data for repeated failures on a bead.
type BugCausality struct {
	BeadID       string
	FailureClass string
	AttemptCount int
	LastFailure  time.Time
}

// CostTrendPoint holds daily cost, token, and run count data.
type CostTrendPoint struct {
	Date             time.Time
	TotalCost        float64
	RunCount         int
	PromptTokens     int64
	CompletionTokens int64
}

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
			COUNT(DISTINCT CASE WHEN result = 'success' AND phase IN ('seal', 'merge') THEN bead_id END) AS merge_count
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
			COALESCE(failure_class, 'unknown') AS failure_class,
			COUNT(*) AS count,
			ROUND(100.0 * COUNT(*) / NULLIF(SUM(COUNT(*)) OVER (), 0), 1) AS percentage
		FROM agent_runs_olap
		WHERE started_at >= ?
		  AND result NOT IN ('success', 'skipped')
		GROUP BY failure_class
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

// ToolEventStats holds aggregated tool event statistics from the tool_events table.
type ToolEventStats struct {
	ToolName     string  `json:"tool_name"`
	Count        int     `json:"count"`
	AvgDurationMs float64 `json:"avg_duration_ms"`
	FailureCount int     `json:"failure_count"`
	Step         string  `json:"step,omitempty"`
}

// StepToolBreakdown holds per-step tool usage for the trace view.
type StepToolBreakdown struct {
	Step  string           `json:"step"`
	Tools []ToolEventStats `json:"tools"`
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
