package report

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// QueryFormulas returns the Formula Performance panel rows. Runs +
// successRate + cost/review-rounds come from agent_runs_olap, and
// the sparkline is 12 weeks of weekly run counts for each formula.
func (r *SQLReader) QueryFormulas(ctx context.Context, scope Scope, since, until time.Time) ([]FormulaRow, error) {
	if r.DB == nil {
		return nil, errNoDB
	}
	clause, scopeArgs := scope.beadIDClause("bead_id")

	// Totals (runs, success rate, cost, reviews per bead).
	totalsSQL := fmt.Sprintf(`
		SELECT
			COALESCE(formula_name, '') AS formula_name,
			COUNT(*) AS runs,
			COALESCE(
				SUM(CASE WHEN result = 'success' THEN 1 ELSE 0 END)::DOUBLE
				/ NULLIF(COUNT(*), 0), 0) AS success_rate,
			COALESCE(SUM(cost_usd), 0) AS cost_usd,
			COALESCE(AVG(CASE WHEN review_rounds > 0 THEN review_rounds END), 0) AS revs_per_bead
		FROM agent_runs_olap
		WHERE formula_name IS NOT NULL AND formula_name != ''
		  AND started_at >= ? AND started_at <= ?%s
		GROUP BY 1
		ORDER BY runs DESC
	`, clause)

	rows, err := r.DB.QueryContext(ctx, totalsSQL, append([]any{since, until}, scopeArgs...)...)
	if err != nil {
		return nil, fmt.Errorf("formulas totals: %w", err)
	}

	var formulas []FormulaRow
	formulaIndex := make(map[string]int)
	for rows.Next() {
		var (
			name         string
			runs         int
			successRate  sql.NullFloat64
			costUSD      sql.NullFloat64
			revsPerBead  sql.NullFloat64
		)
		if err := rows.Scan(&name, &runs, &successRate, &costUSD, &revsPerBead); err != nil {
			rows.Close()
			return nil, err
		}
		formulaIndex[name] = len(formulas)
		formulas = append(formulas, FormulaRow{
			Name:        name,
			Runs:        runs,
			SuccessRate: nullFloat(successRate),
			CostUSD:     nullFloat(costUSD),
			RevsPerBead: nullFloat(revsPerBead),
			Sparkline:   make([]float64, 12), // 12 zero-filled slots
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Sparkline: weekly run counts for the same formulas, backfilled
	// to 12 weeks. Widen the window to always cover 12 weeks.
	wideSince := since
	if t := until.AddDate(0, 0, -84); t.Before(wideSince) {
		wideSince = t
	}
	sparkSQL := fmt.Sprintf(`
		SELECT
			COALESCE(formula_name, '') AS formula_name,
			date_trunc('week', started_at)::DATE AS week_start,
			COUNT(*) AS runs
		FROM agent_runs_olap
		WHERE formula_name IS NOT NULL AND formula_name != ''
		  AND started_at >= ? AND started_at <= ?%s
		GROUP BY 1, 2
	`, clause)
	srows, err := r.DB.QueryContext(ctx, sparkSQL, append([]any{wideSince, until}, scopeArgs...)...)
	if err != nil {
		return nil, fmt.Errorf("formulas sparkline: %w", err)
	}
	defer srows.Close()

	// Build week-index map: the right-most slot is the most recent
	// week (startOfWeek(until)); slot 0 is 11 weeks before that.
	end := startOfWeek(until)
	weekIdx := make(map[time.Time]int, 12)
	for i := 0; i < 12; i++ {
		weekIdx[end.AddDate(0, 0, -7*(11-i))] = i
	}

	for srows.Next() {
		var (
			name string
			week time.Time
			runs int
		)
		if err := srows.Scan(&name, &week, &runs); err != nil {
			return nil, err
		}
		idx, ok := formulaIndex[name]
		if !ok {
			continue
		}
		slot, ok := weekIdx[week.UTC()]
		if !ok {
			continue
		}
		formulas[idx].Sparkline[slot] = float64(runs)
	}
	if err := srows.Err(); err != nil {
		return nil, err
	}
	return formulas, nil
}
