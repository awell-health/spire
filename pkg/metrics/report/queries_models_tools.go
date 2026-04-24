package report

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// QueryModels returns one row per model: runs, success rate, total
// cost, avg duration, total tokens.
func (r *SQLReader) QueryModels(ctx context.Context, scope Scope, since, until time.Time) ([]ModelRow, error) {
	if r.DB == nil {
		return nil, errNoDB
	}
	clause, scopeArgs := scope.beadIDClause("bead_id")

	q := fmt.Sprintf(`
		SELECT
			COALESCE(NULLIF(model, ''), 'unknown') AS model,
			COUNT(*) AS runs,
			COALESCE(
				SUM(CASE WHEN result = 'success' THEN 1 ELSE 0 END)::DOUBLE
				/ NULLIF(COUNT(*), 0), 0) AS success_rate,
			COALESCE(SUM(cost_usd), 0) AS cost_usd,
			COALESCE(AVG(duration_seconds), 0) AS avg_dur_s,
			COALESCE(SUM(total_tokens), 0) AS total_tokens
		FROM agent_runs_olap
		WHERE started_at >= ? AND started_at <= ?%s
		GROUP BY 1
		ORDER BY runs DESC
	`, clause)
	rows, err := r.DB.QueryContext(ctx, q, append([]any{since, until}, scopeArgs...)...)
	if err != nil {
		return nil, fmt.Errorf("models: %w", err)
	}
	defer rows.Close()

	var out []ModelRow
	for rows.Next() {
		var (
			m       ModelRow
			succRt  sql.NullFloat64
			costUSD sql.NullFloat64
			avgDur  sql.NullFloat64
			toks    sql.NullInt64
		)
		if err := rows.Scan(&m.Model, &m.Runs, &succRt, &costUSD, &avgDur, &toks); err != nil {
			return nil, err
		}
		m.SuccessRate = nullFloat(succRt)
		m.CostUSD = nullFloat(costUSD)
		m.AvgDurationSeconds = nullFloat(avgDur)
		m.TotalTokens = nullInt64(toks)
		out = append(out, m)
	}
	return out, rows.Err()
}

// QueryTools returns one row per tool: total calls, failures, average
// duration (ms). Sourced from tool_events.
func (r *SQLReader) QueryTools(ctx context.Context, scope Scope, since, until time.Time) ([]ToolRow, error) {
	if r.DB == nil {
		return nil, errNoDB
	}
	clause, scopeArgs := scope.beadIDClause("bead_id")

	q := fmt.Sprintf(`
		SELECT
			COALESCE(NULLIF(tool_name, ''), 'unknown') AS tool,
			COUNT(*) AS calls,
			SUM(CASE WHEN NOT success THEN 1 ELSE 0 END) AS failures,
			COALESCE(AVG(duration_ms), 0) AS avg_ms
		FROM tool_events
		WHERE timestamp >= ? AND timestamp <= ?%s
		GROUP BY 1
		ORDER BY calls DESC
	`, clause)
	rows, err := r.DB.QueryContext(ctx, q, append([]any{since, until}, scopeArgs...)...)
	if err != nil {
		return nil, fmt.Errorf("tools: %w", err)
	}
	defer rows.Close()

	var out []ToolRow
	for rows.Next() {
		var (
			t   ToolRow
			avg sql.NullFloat64
		)
		if err := rows.Scan(&t.Tool, &t.Calls, &t.Failures, &avg); err != nil {
			return nil, err
		}
		t.AvgDurationMs = nullFloat(avg)
		out = append(out, t)
	}
	return out, rows.Err()
}
