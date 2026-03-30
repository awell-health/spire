package observability

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// MetricsRow holds a single row from agent_runs queries.
type MetricsRow = map[string]any

// MetricsSummary shows today + this week overview.
func MetricsSummary(jsonOut bool) error {
	// Dolt does not support UTC_DATE(). CURDATE() uses server-local time.
	todayQuery := `SELECT
		COUNT(*) as total,
		SUM(CASE WHEN result='success' THEN 1 ELSE 0 END) as succeeded,
		AVG(review_rounds) as avg_rounds,
		SUM(COALESCE(context_tokens_in,0)) as total_tokens_in,
		SUM(COALESCE(context_tokens_out,0)) as total_tokens_out
	FROM agent_runs
	WHERE DATE(started_at) = CURDATE()`

	weekQuery := `SELECT
		COUNT(*) as total,
		SUM(CASE WHEN result='success' THEN 1 ELSE 0 END) as succeeded,
		SUM(COALESCE(context_tokens_in,0)) as total_tokens_in,
		SUM(COALESCE(context_tokens_out,0)) as total_tokens_out
	FROM agent_runs
	WHERE started_at >= DATE_SUB(CURDATE(), INTERVAL 7 DAY)`

	breakdownQuery := `SELECT result, COUNT(*) as cnt
	FROM agent_runs
	WHERE started_at >= DATE_SUB(CURDATE(), INTERVAL 7 DAY)
	GROUP BY result
	ORDER BY cnt DESC`

	specsQuery := `SELECT
		spec_file,
		COUNT(*) as total,
		SUM(CASE WHEN result='success' THEN 1 ELSE 0 END) as succeeded
	FROM agent_runs
	WHERE spec_file IS NOT NULL AND spec_file != ''
		AND started_at >= DATE_SUB(CURDATE(), INTERVAL 30 DAY)
	GROUP BY spec_file
	HAVING total >= 3
	ORDER BY (succeeded * 100 / total) DESC
	LIMIT 10`

	todayRows, err := QueryJSON(todayQuery)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	weekRows, err := QueryJSON(weekQuery)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	breakdownRows, err := QueryJSON(breakdownQuery)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	specRows, err := QueryJSON(specsQuery)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}

	if jsonOut {
		out := map[string]any{
			"today":     FirstOr(todayRows),
			"week":      FirstOr(weekRows),
			"breakdown": breakdownRows,
			"top_specs": specRows,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	// Today
	today := FirstOr(todayRows)
	todayTotal := ToInt(today["total"])
	todaySuccess := ToInt(today["succeeded"])
	todayRate := Pct(todaySuccess, todayTotal)
	todayRounds := ToFloat(today["avg_rounds"])
	todayCost := EstimateCost(ToInt(today["total_tokens_in"]), ToInt(today["total_tokens_out"]), "")

	fmt.Printf("Today: %d tasks completed, %s success, avg %.1f review rounds, $%.0f est. cost\n",
		todayTotal, todayRate, todayRounds, todayCost)

	// This week
	week := FirstOr(weekRows)
	weekTotal := ToInt(week["total"])
	weekSuccess := ToInt(week["succeeded"])
	weekRate := Pct(weekSuccess, weekTotal)
	weekCost := EstimateCost(ToInt(week["total_tokens_in"]), ToInt(week["total_tokens_out"]), "")

	fmt.Printf("This week: %d tasks, %s success, $%.0f est. cost\n",
		weekTotal, weekRate, weekCost)

	// Breakdown
	if len(breakdownRows) > 0 {
		fmt.Println()
		fmt.Println("By result:")
		for _, row := range breakdownRows {
			result := ToString(row["result"])
			cnt := ToInt(row["cnt"])
			ratioStr := Pct(cnt, weekTotal)
			fmt.Printf("  %-20s %d (%s)\n", result, cnt, ratioStr)
		}
	}

	// Top specs
	if len(specRows) > 0 {
		fmt.Println()
		fmt.Println("Top specs:")
		for _, row := range specRows {
			spec := ToString(row["spec_file"])
			total := ToInt(row["total"])
			succeeded := ToInt(row["succeeded"])
			rate := Pct(succeeded, total)
			hint := ""
			if total > 0 && succeeded*100/total < 70 {
				hint = " <- needs better spec"
			}
			parts := strings.Split(spec, "/")
			name := parts[len(parts)-1]
			fmt.Printf("  %-30s %s success (%d runs)%s\n", name, rate, total, hint)
		}
	}

	if todayTotal == 0 && weekTotal == 0 {
		fmt.Println()
		fmt.Printf("%s(no agent runs recorded yet)%s\n", Dim, Reset)
	}

	return nil
}

// MetricsPhase shows a per-phase breakdown for the last 7 days.
func MetricsPhase(jsonOut bool) error {
	query := `SELECT
		phase,
		COUNT(*) as total,
		SUM(CASE WHEN result='success' THEN 1 ELSE 0 END) as succeeded,
		AVG(duration_seconds) as avg_duration,
		SUM(COALESCE(context_tokens_in,0)) as total_tokens_in,
		SUM(COALESCE(context_tokens_out,0)) as total_tokens_out,
		SUM(COALESCE(cost_usd,0)) as total_cost
	FROM agent_runs
	WHERE phase IS NOT NULL
		AND started_at >= DATE_SUB(CURDATE(), INTERVAL 7 DAY)
	GROUP BY phase
	ORDER BY total DESC`

	rows, err := QueryJSON(query)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}

	return RenderPhaseMetrics(rows, jsonOut, os.Stdout)
}

// RenderPhaseMetrics formats phase metrics rows for display.
// Exported for testing — callers outside observability should use MetricsPhase.
func RenderPhaseMetrics(rows []MetricsRow, jsonOut bool, w *os.File) error {
	if jsonOut {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}

	if len(rows) == 0 {
		fmt.Fprintf(w, "%s(no per-phase data yet — phase tracking starts with new runs)%s\n", Dim, Reset)
		return nil
	}

	fmt.Fprintln(w, "Per-phase breakdown (this week):")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %-14s %5s %8s %10s %10s\n", "PHASE", "RUNS", "SUCCESS", "AVG DUR", "COST")
	fmt.Fprintf(w, "  %-14s %5s %8s %10s %10s\n", "─────", "────", "───────", "───────", "────")
	for _, row := range rows {
		phase := ToString(row["phase"])
		total := ToInt(row["total"])
		succeeded := ToInt(row["succeeded"])
		rate := Pct(succeeded, total)
		avgDur := ToFloat(row["avg_duration"])
		tokIn := ToInt(row["total_tokens_in"])
		tokOut := ToInt(row["total_tokens_out"])
		cost := EstimateCost(tokIn, tokOut, "")
		// Prefer recorded cost_usd if available.
		if rc := ToFloat(row["total_cost"]); rc > 0 {
			cost = rc
		}
		fmt.Fprintf(w, "  %-14s %5d %8s %8.0fs %9s\n",
			phase, total, rate, avgDur, fmtCost(cost))
	}

	return nil
}

// fmtCost formats a USD cost for display.
func fmtCost(cost float64) string {
	if cost < 1 {
		return fmt.Sprintf("$%.2f", cost)
	}
	return fmt.Sprintf("$%.0f", cost)
}

// MetricsBead shows metrics for a specific bead or epic.
func MetricsBead(beadID string, jsonOut bool) error {
	query := fmt.Sprintf(`SELECT
		COUNT(*) as total,
		SUM(CASE WHEN result='success' THEN 1 ELSE 0 END) as succeeded,
		AVG(review_rounds) as avg_rounds,
		SUM(COALESCE(context_tokens_in,0)) as total_tokens_in,
		SUM(COALESCE(context_tokens_out,0)) as total_tokens_out,
		SUM(COALESCE(total_tokens,0)) as total_tokens,
		AVG(duration_seconds) as avg_duration,
		SUM(COALESCE(files_changed,0)) as total_files,
		SUM(COALESCE(lines_added,0)) as total_added,
		SUM(COALESCE(lines_removed,0)) as total_removed
	FROM agent_runs
	WHERE bead_id = '%s' OR epic_id = '%s'`,
		SqlEsc(beadID), SqlEsc(beadID))

	runsQuery := fmt.Sprintf(`SELECT id, bead_id, model, role, phase, result, review_rounds,
		context_tokens_in, context_tokens_out, duration_seconds, started_at
	FROM agent_runs
	WHERE bead_id = '%s' OR epic_id = '%s'
	ORDER BY started_at DESC
	LIMIT 20`, SqlEsc(beadID), SqlEsc(beadID))

	rows, err := QueryJSON(query)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	runsRows, err := QueryJSON(runsQuery)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}

	if jsonOut {
		out := map[string]any{
			"summary": FirstOr(rows),
			"runs":    runsRows,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	summary := FirstOr(rows)
	total := ToInt(summary["total"])
	succeeded := ToInt(summary["succeeded"])
	rate := Pct(succeeded, total)
	avgRounds := ToFloat(summary["avg_rounds"])
	tokensIn := ToInt(summary["total_tokens_in"])
	tokensOut := ToInt(summary["total_tokens_out"])
	cost := EstimateCost(tokensIn, tokensOut, "")
	avgDur := ToFloat(summary["avg_duration"])

	fmt.Printf("Bead: %s\n", beadID)
	fmt.Printf("  Runs: %d total, %d succeeded (%s)\n", total, succeeded, rate)
	fmt.Printf("  Avg review rounds: %.1f\n", avgRounds)
	fmt.Printf("  Avg duration: %.0fs\n", avgDur)
	fmt.Printf("  Total tokens: %dK in, %dK out\n", tokensIn/1000, tokensOut/1000)
	fmt.Printf("  Est. cost: $%.2f\n", cost)
	fmt.Printf("  Files changed: %d, +%d/-%d lines\n",
		ToInt(summary["total_files"]),
		ToInt(summary["total_added"]),
		ToInt(summary["total_removed"]))

	if len(runsRows) > 0 {
		fmt.Println()
		fmt.Println("  Recent runs:")
		for _, r := range runsRows {
			phase := ToString(r["phase"])
			if phase == "" {
				phase = "-"
			}
			fmt.Printf("    %-14s %-10s %-8s %-12s %-18s rounds=%d  %s\n",
				ToString(r["id"]),
				ToString(r["model"]),
				ToString(r["role"]),
				phase,
				ToString(r["result"]),
				ToInt(r["review_rounds"]),
				ToString(r["started_at"]),
			)
		}
	}

	if total == 0 {
		fmt.Printf("\n%s(no runs found for %s)%s\n", Dim, beadID, Reset)
	}

	return nil
}

// MetricsModel shows breakdown by model.
func MetricsModel(jsonOut bool) error {
	query := `SELECT
		model,
		role,
		COUNT(*) as total,
		SUM(CASE WHEN result='success' THEN 1 ELSE 0 END) as succeeded,
		AVG(COALESCE(context_tokens_in,0)) as avg_tokens_in,
		AVG(COALESCE(context_tokens_out,0)) as avg_tokens_out,
		SUM(COALESCE(context_tokens_in,0)) as total_tokens_in,
		SUM(COALESCE(context_tokens_out,0)) as total_tokens_out,
		AVG(duration_seconds) as avg_duration
	FROM agent_runs
	WHERE started_at >= DATE_SUB(CURDATE(), INTERVAL 7 DAY)
	GROUP BY model, role
	ORDER BY total DESC`

	rows, err := QueryJSON(query)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}

	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}

	if len(rows) == 0 {
		fmt.Printf("%s(no runs this week)%s\n", Dim, Reset)
		return nil
	}

	var totalCost float64
	var wizardCost, reviewCost float64

	fmt.Println("Model breakdown (this week):")
	fmt.Println()
	for _, row := range rows {
		model := ToString(row["model"])
		role := ToString(row["role"])
		total := ToInt(row["total"])
		avgIn := ToInt(row["avg_tokens_in"])
		avgOut := ToInt(row["avg_tokens_out"])
		tokIn := ToInt(row["total_tokens_in"])
		tokOut := ToInt(row["total_tokens_out"])
		succeeded := ToInt(row["succeeded"])
		rate := Pct(succeeded, total)
		costPerRun := EstimateCost(avgIn, avgOut, model)
		subtotal := EstimateCost(tokIn, tokOut, model)
		totalCost += subtotal
		if role == "wizard" {
			reviewCost += subtotal
		} else {
			wizardCost += subtotal
		}

		fmt.Printf("  %s (%s): %d runs, %s success, avg %dK tokens, ~$%.2f/run\n",
			model, role, total, rate, (avgIn+avgOut)/1000, costPerRun)
	}

	fmt.Printf("\nTotal cost this week: $%.0f (workers: $%.0f, wizard: $%.0f)\n",
		totalCost, wizardCost, reviewCost)

	return nil
}

// QueryJSON runs a SQL query via bd sql --json and returns parsed rows.
// Returns nil (not an error) when the agent_runs table doesn't exist yet.
func QueryJSON(query string) ([]MetricsRow, error) {
	cmd := exec.Command("bd", "sql", "--json", query)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	combined := stdout.String() + stderr.String()

	// If the table doesn't exist yet, return empty results gracefully.
	if err != nil {
		if strings.Contains(combined, "table not found") ||
			strings.Contains(combined, "doesn't exist") ||
			strings.Contains(combined, "agent_runs") {
			return nil, nil
		}
		return nil, fmt.Errorf("bd sql: %s\n%s", err, strings.TrimSpace(combined))
	}

	out := strings.TrimSpace(stdout.String())
	if out == "" || out == "[]" || out == "null" {
		return nil, nil
	}

	// bd sql --json may return an error object instead of rows.
	if strings.Contains(out, `"error"`) {
		var errObj map[string]string
		if json.Unmarshal([]byte(out), &errObj) == nil {
			if msg, ok := errObj["error"]; ok {
				if strings.Contains(msg, "table not found") {
					return nil, nil
				}
				return nil, fmt.Errorf("bd sql: %s", msg)
			}
		}
	}

	var rows []MetricsRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, fmt.Errorf("parse query result: %w", err)
	}
	return rows, nil
}

// EstimateCost returns estimated USD cost based on token counts.
// Pricing (rough per-token):
//   - Sonnet: $3/M input, $15/M output
//   - Opus:   $15/M input, $75/M output
func EstimateCost(tokensIn, tokensOut int, model string) float64 {
	inRate := 3.0  // $/M tokens — Sonnet default
	outRate := 15.0 // $/M tokens
	if strings.Contains(strings.ToLower(model), "opus") {
		inRate = 15.0
		outRate = 75.0
	}
	return (float64(tokensIn)*inRate + float64(tokensOut)*outRate) / 1_000_000
}

// Pct returns "X%" from succeeded/total, or "0%" if total is 0.
func Pct(n, total int) string {
	if total == 0 {
		return "0%"
	}
	return fmt.Sprintf("%d%%", n*100/total)
}

// FirstOr returns the first row or an empty map.
func FirstOr(rows []MetricsRow) MetricsRow {
	if len(rows) > 0 {
		return rows[0]
	}
	return MetricsRow{}
}

// ToInt converts a JSON value to int.
func ToInt(v any) int {
	if v == nil {
		return 0
	}
	switch val := v.(type) {
	case float64:
		return int(val)
	case int:
		return val
	case string:
		n, _ := strconv.Atoi(val)
		return n
	case json.Number:
		n, _ := val.Int64()
		return int(n)
	default:
		return 0
	}
}

// ToFloat converts a JSON value to float64.
func ToFloat(v any) float64 {
	if v == nil {
		return 0
	}
	switch val := v.(type) {
	case float64:
		return val
	case int:
		return float64(val)
	case string:
		f, _ := strconv.ParseFloat(val, 64)
		return f
	case json.Number:
		f, _ := val.Float64()
		return f
	default:
		return 0
	}
}

// ToString converts a JSON value to string.
func ToString(v any) string {
	if v == nil {
		return ""
	}
	switch val := v.(type) {
	case string:
		return val
	case float64:
		if val == float64(int(val)) {
			return strconv.Itoa(int(val))
		}
		return fmt.Sprintf("%.2f", val)
	default:
		return fmt.Sprintf("%v", val)
	}
}

// RunsForBead returns recent agent runs for a bead (up to 10).
func RunsForBead(beadID string) ([]MetricsRow, error) {
	query := fmt.Sprintf(`SELECT id, agent_name, model, role, result,
		duration_seconds, started_at
	FROM agent_runs
	WHERE bead_id = '%s' OR epic_id = '%s'
	ORDER BY started_at DESC
	LIMIT 10`, SqlEsc(beadID), SqlEsc(beadID))
	return QueryJSON(query)
}

// SqlEsc escapes single quotes for SQL string literals.
func SqlEsc(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// DORAResult holds all four DORA metrics for JSON output.
type DORAResult struct {
	DeploymentFrequency []DORAWeekCount `json:"deployment_frequency"`
	LeadTime            *DORAStats      `json:"lead_time"`
	ChangeFailureRate   *DORAFailRate   `json:"change_failure_rate"`
	MTTR                *DORAStats      `json:"mttr"`
}

// DORAWeekCount is a single week's merge count.
type DORAWeekCount struct {
	Week   string `json:"week"`
	Merged int    `json:"merged"`
}

// DORAStats holds avg/p50/p90 and count.
type DORAStats struct {
	AvgHours float64 `json:"avg_hours"`
	P50Hours float64 `json:"p50_hours"`
	P90Hours float64 `json:"p90_hours"`
	Count    int     `json:"count"`
}

// DORAFailRate holds change failure rate data.
type DORAFailRate struct {
	TotalClosed int     `json:"total_closed"`
	Failures    int     `json:"failures"`
	Rate        float64 `json:"rate"`
}

// MetricsDORA computes and displays DORA metrics from the bead graph.
func MetricsDORA(jsonOut bool) error {
	result := &DORAResult{}

	// 1. Deployment Frequency — merges per week (last 28 days)
	dfQuery := `SELECT
		YEARWEEK(closed_at, 1) as week,
		COUNT(*) as merged
	FROM issues
	WHERE status = 'closed'
		AND closed_at IS NOT NULL
		AND issue_type NOT IN ('design', 'epic')
		AND closed_at >= DATE_SUB(CURDATE(), INTERVAL 28 DAY)
	GROUP BY YEARWEEK(closed_at, 1)
	ORDER BY week`

	dfRows, err := QueryJSON(dfQuery)
	if err != nil {
		return fmt.Errorf("dora deployment frequency: %w", err)
	}
	for _, row := range dfRows {
		yw := ToString(row["week"])
		// YEARWEEK returns YYYYWW integer; format as YYYY-Wnn
		if len(yw) >= 6 {
			yw = yw[:4] + "-W" + yw[4:]
		}
		result.DeploymentFrequency = append(result.DeploymentFrequency, DORAWeekCount{
			Week:   yw,
			Merged: ToInt(row["merged"]),
		})
	}

	// 2. Lead Time for Changes — hours from created_at to closed_at
	ltQuery := `SELECT
		TIMESTAMPDIFF(HOUR, created_at, closed_at) as lead_time_hours
	FROM issues
	WHERE status = 'closed'
		AND closed_at IS NOT NULL
		AND issue_type NOT IN ('design', 'epic')
		AND closed_at >= DATE_SUB(CURDATE(), INTERVAL 28 DAY)
	ORDER BY lead_time_hours`

	ltRows, err := QueryJSON(ltQuery)
	if err != nil {
		return fmt.Errorf("dora lead time: %w", err)
	}
	if len(ltRows) > 0 {
		vals := make([]float64, len(ltRows))
		for i, row := range ltRows {
			vals[i] = ToFloat(row["lead_time_hours"])
		}
		result.LeadTime = &DORAStats{
			AvgHours: avg(vals),
			P50Hours: percentile(vals, 0.50),
			P90Hours: percentile(vals, 0.90),
			Count:    len(vals),
		}
	}

	// 3. Change Failure Rate — needs-human label OR review_rounds > 2
	// Run two separate queries to handle agent_runs table possibly missing.
	cfrBaseQuery := `SELECT COUNT(DISTINCT id) as total_closed
	FROM issues
	WHERE status = 'closed'
		AND closed_at IS NOT NULL
		AND issue_type NOT IN ('design', 'epic')
		AND closed_at >= DATE_SUB(CURDATE(), INTERVAL 28 DAY)`

	cfrBaseRows, err := QueryJSON(cfrBaseQuery)
	if err != nil {
		return fmt.Errorf("dora cfr base: %w", err)
	}
	totalClosed := ToInt(FirstOr(cfrBaseRows)["total_closed"])

	// Beads with needs-human label
	cfrLabelQuery := `SELECT COUNT(DISTINCT i.id) as failures
	FROM issues i
	JOIN labels l ON l.issue_id = i.id AND l.label = 'needs-human'
	WHERE i.status = 'closed'
		AND i.closed_at IS NOT NULL
		AND i.issue_type NOT IN ('design', 'epic')
		AND i.closed_at >= DATE_SUB(CURDATE(), INTERVAL 28 DAY)`

	labelRows, err := QueryJSON(cfrLabelQuery)
	if err != nil {
		return fmt.Errorf("dora cfr labels: %w", err)
	}
	labelFailures := ToInt(FirstOr(labelRows)["failures"])

	// Beads with review_rounds > 2 (agent_runs may not exist)
	cfrRunsQuery := `SELECT COUNT(DISTINCT ar.bead_id) as failures
	FROM agent_runs ar
	JOIN issues i ON i.id = ar.bead_id
	WHERE i.status = 'closed'
		AND i.closed_at IS NOT NULL
		AND i.issue_type NOT IN ('design', 'epic')
		AND i.closed_at >= DATE_SUB(CURDATE(), INTERVAL 28 DAY)
		AND ar.review_rounds > 2`

	runsRows, err := QueryJSON(cfrRunsQuery)
	if err != nil {
		// agent_runs table may not exist — ignore
		runsRows = nil
	}
	runsFailures := ToInt(FirstOr(runsRows)["failures"])

	// Combine: count distinct failures from either source.
	// Use a union query for accurate distinct count if both sources work.
	failures := labelFailures
	if runsFailures > 0 {
		// For accuracy, run a union query
		cfrUnionQuery := `SELECT COUNT(*) as failures FROM (
			SELECT DISTINCT i.id
			FROM issues i
			JOIN labels l ON l.issue_id = i.id AND l.label = 'needs-human'
			WHERE i.status = 'closed'
				AND i.closed_at IS NOT NULL
				AND i.issue_type NOT IN ('design', 'epic')
				AND i.closed_at >= DATE_SUB(CURDATE(), INTERVAL 28 DAY)
			UNION
			SELECT DISTINCT ar.bead_id as id
			FROM agent_runs ar
			JOIN issues i ON i.id = ar.bead_id
			WHERE i.status = 'closed'
				AND i.closed_at IS NOT NULL
				AND i.issue_type NOT IN ('design', 'epic')
				AND i.closed_at >= DATE_SUB(CURDATE(), INTERVAL 28 DAY)
				AND ar.review_rounds > 2
		) combined`

		unionRows, uerr := QueryJSON(cfrUnionQuery)
		if uerr == nil && len(unionRows) > 0 {
			failures = ToInt(FirstOr(unionRows)["failures"])
		}
		// fallback: just add them (may overcount slightly)
		if uerr != nil {
			failures = labelFailures + runsFailures
		}
	}

	if totalClosed > 0 {
		result.ChangeFailureRate = &DORAFailRate{
			TotalClosed: totalClosed,
			Failures:    failures,
			Rate:        float64(failures) * 100 / float64(totalClosed),
		}
	}

	// 4. Mean Time to Recovery — needs-human escalation to close
	mttrQuery := `SELECT
		i.id,
		TIMESTAMPDIFF(HOUR, MIN(c.created_at), i.closed_at) as recovery_hours
	FROM issues i
	JOIN labels l ON l.issue_id = i.id AND l.label = 'needs-human'
	JOIN comments c ON c.issue_id = i.id AND c.text LIKE '%needs-human%'
	WHERE i.status = 'closed'
		AND i.closed_at IS NOT NULL
		AND i.closed_at >= DATE_SUB(CURDATE(), INTERVAL 28 DAY)
	GROUP BY i.id, i.closed_at
	ORDER BY recovery_hours`

	mttrRows, err := QueryJSON(mttrQuery)
	if err != nil {
		// If the query fails (e.g. no labels/comments tables), just skip MTTR
		mttrRows = nil
	}
	if len(mttrRows) > 0 {
		vals := make([]float64, len(mttrRows))
		for i, row := range mttrRows {
			vals[i] = ToFloat(row["recovery_hours"])
		}
		result.MTTR = &DORAStats{
			AvgHours: avg(vals),
			P50Hours: percentile(vals, 0.50),
			P90Hours: percentile(vals, 0.90),
			Count:    len(vals),
		}
	}

	// Render
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	return renderDORAText(result)
}

func renderDORAText(r *DORAResult) error {
	fmt.Println("DORA Metrics (last 28 days)")
	fmt.Println()

	// Deployment Frequency
	fmt.Println("Deployment Frequency:")
	if len(r.DeploymentFrequency) == 0 {
		fmt.Printf("  %s(no merges in period)%s\n", Dim, Reset)
	} else {
		var total int
		for _, wk := range r.DeploymentFrequency {
			fmt.Printf("  %-12s %d merges\n", wk.Week+":", wk.Merged)
			total += wk.Merged
		}
		avgPerWeek := float64(total) / float64(len(r.DeploymentFrequency))
		fmt.Printf("  Avg: %.1f merges/week\n", avgPerWeek)
	}
	fmt.Println()

	// Lead Time
	fmt.Println("Lead Time for Changes:")
	if r.LeadTime == nil {
		fmt.Printf("  %s(no data)%s\n", Dim, Reset)
	} else {
		fmt.Printf("  Avg: %.1fh   P50: %.1fh   P90: %.1fh   (%d beads)\n",
			r.LeadTime.AvgHours, r.LeadTime.P50Hours, r.LeadTime.P90Hours, r.LeadTime.Count)
	}
	fmt.Println()

	// Change Failure Rate
	fmt.Println("Change Failure Rate:")
	if r.ChangeFailureRate == nil {
		fmt.Printf("  %s(no data)%s\n", Dim, Reset)
	} else {
		fmt.Printf("  %d of %d (%.0f%%) required human intervention\n",
			r.ChangeFailureRate.Failures, r.ChangeFailureRate.TotalClosed,
			r.ChangeFailureRate.Rate)
	}
	fmt.Println()

	// MTTR
	fmt.Println("Mean Time to Recovery:")
	if r.MTTR == nil {
		fmt.Printf("  %s(no escalations in period)%s\n", Dim, Reset)
	} else {
		fmt.Printf("  Avg: %.1fh   P50: %.1fh   (%d incidents)\n",
			r.MTTR.AvgHours, r.MTTR.P50Hours, r.MTTR.Count)
	}

	return nil
}

// percentile computes the p-th percentile from a sorted slice using linear interpolation.
// p must be between 0 and 1. The input slice must be sorted in ascending order.
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	// Use the "exclusive" method: rank = p * (n + 1)
	rank := p * float64(n-1)
	lower := int(rank)
	if lower >= n-1 {
		return sorted[n-1]
	}
	frac := rank - float64(lower)
	return sorted[lower] + frac*(sorted[lower+1]-sorted[lower])
}

// avg computes the arithmetic mean of a float64 slice.
func avg(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}
