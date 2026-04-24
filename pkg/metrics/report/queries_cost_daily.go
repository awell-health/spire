package report

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// QueryCostDaily returns one row per day in the window with total
// cost, tokens, run count, and the top 3 most expensive runs of the
// day. Days with zero runs are omitted.
func (r *SQLReader) QueryCostDaily(ctx context.Context, scope Scope, since, until time.Time) ([]CostDay, error) {
	if r.DB == nil {
		return nil, errNoDB
	}
	clause, scopeArgs := scope.beadIDClause("bead_id")

	totalsSQL := fmt.Sprintf(`
		SELECT
			date_trunc('day', started_at)::DATE AS day,
			COALESCE(SUM(cost_usd), 0) AS cost_usd,
			COALESCE(SUM(total_tokens), 0) AS tokens,
			COUNT(*) AS runs
		FROM agent_runs_olap
		WHERE started_at >= ? AND started_at <= ?%s
		GROUP BY 1
		ORDER BY 1
	`, clause)

	rows, err := r.DB.QueryContext(ctx, totalsSQL, append([]any{since, until}, scopeArgs...)...)
	if err != nil {
		return nil, fmt.Errorf("cost daily totals: %w", err)
	}

	var days []CostDay
	dayIndex := make(map[string]int)
	for rows.Next() {
		var (
			day    time.Time
			cost   sql.NullFloat64
			tokens sql.NullInt64
			runs   int
		)
		if err := rows.Scan(&day, &cost, &tokens, &runs); err != nil {
			rows.Close()
			return nil, err
		}
		dayStr := day.UTC().Format("2006-01-02")
		dayIndex[dayStr] = len(days)
		days = append(days, CostDay{
			Date:    dayStr,
			CostUSD: nullFloat(cost),
			Tokens:  nullInt64(tokens),
			Runs:    runs,
			TopRuns: []CostDayTopRun{},
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Top 3 runs per day by cost_usd. Cheap single query returning
	// all candidates; we pick top-3 in Go to avoid DuckDB-specific
	// QUALIFY ROW_NUMBER() wiring.
	topSQL := fmt.Sprintf(`
		SELECT
			date_trunc('day', started_at)::DATE AS day,
			COALESCE(id, '') AS run_id,
			COALESCE(bead_id, '') AS bead_id,
			COALESCE(cost_usd, 0) AS cost_usd
		FROM agent_runs_olap
		WHERE started_at >= ? AND started_at <= ?%s
		  AND cost_usd > 0
	`, clause)
	trows, err := r.DB.QueryContext(ctx, topSQL, append([]any{since, until}, scopeArgs...)...)
	if err != nil {
		return nil, fmt.Errorf("cost daily top runs: %w", err)
	}
	defer trows.Close()

	perDay := make(map[string][]CostDayTopRun)
	for trows.Next() {
		var (
			day   time.Time
			run   CostDayTopRun
			cost  sql.NullFloat64
		)
		if err := trows.Scan(&day, &run.RunID, &run.BeadID, &cost); err != nil {
			return nil, err
		}
		run.CostUSD = nullFloat(cost)
		perDay[day.UTC().Format("2006-01-02")] = append(perDay[day.UTC().Format("2006-01-02")], run)
	}
	if err := trows.Err(); err != nil {
		return nil, err
	}
	for day, runs := range perDay {
		sort.Slice(runs, func(i, j int) bool { return runs[i].CostUSD > runs[j].CostUSD })
		if len(runs) > 3 {
			runs = runs[:3]
		}
		idx, ok := dayIndex[day]
		if !ok {
			continue
		}
		days[idx].TopRuns = runs
	}
	return days, nil
}
