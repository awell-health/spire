package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// metricsRow holds a single row from agent_runs queries.
type metricsRow = map[string]any

func cmdMetrics(args []string) error {
	var (
		flagJSON  bool
		flagBead  string
		flagModel bool
	)
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			flagJSON = true
		case "--model":
			flagModel = true
		case "--bead":
			if i+1 >= len(args) {
				return fmt.Errorf("--bead requires a value")
			}
			i++
			flagBead = args[i]
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire metrics [--bead <id>] [--model] [--json]", args[i])
		}
	}

	if flagBead != "" {
		return metricsBead(flagBead, flagJSON)
	}
	if flagModel {
		return metricsModel(flagJSON)
	}
	return metricsSummary(flagJSON)
}

// metricsSummary shows today + this week overview.
func metricsSummary(jsonOut bool) error {
	// Today's stats
	todayQuery := `SELECT
		COUNT(*) as total,
		SUM(CASE WHEN result='success' THEN 1 ELSE 0 END) as succeeded,
		AVG(review_rounds) as avg_rounds,
		SUM(COALESCE(context_tokens_in,0)) as total_tokens_in,
		SUM(COALESCE(context_tokens_out,0)) as total_tokens_out
	FROM agent_runs
	WHERE DATE(started_at) = CURDATE()`

	// This week's stats
	weekQuery := `SELECT
		COUNT(*) as total,
		SUM(CASE WHEN result='success' THEN 1 ELSE 0 END) as succeeded,
		SUM(COALESCE(context_tokens_in,0)) as total_tokens_in,
		SUM(COALESCE(context_tokens_out,0)) as total_tokens_out
	FROM agent_runs
	WHERE started_at >= DATE_SUB(CURDATE(), INTERVAL 7 DAY)`

	// Result breakdown (this week)
	breakdownQuery := `SELECT result, COUNT(*) as cnt
	FROM agent_runs
	WHERE started_at >= DATE_SUB(CURDATE(), INTERVAL 7 DAY)
	GROUP BY result
	ORDER BY cnt DESC`

	// Top specs
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

	todayRows, err := queryJSON(todayQuery)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	weekRows, err := queryJSON(weekQuery)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	breakdownRows, err := queryJSON(breakdownQuery)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	specRows, err := queryJSON(specsQuery)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}

	if jsonOut {
		out := map[string]any{
			"today":     firstOr(todayRows),
			"week":      firstOr(weekRows),
			"breakdown": breakdownRows,
			"top_specs": specRows,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	// Today
	today := firstOr(todayRows)
	todayTotal := toInt(today["total"])
	todaySuccess := toInt(today["succeeded"])
	todayRate := pct(todaySuccess, todayTotal)
	todayRounds := toFloat(today["avg_rounds"])
	todayCost := estimateCost(toInt(today["total_tokens_in"]), toInt(today["total_tokens_out"]), "")

	fmt.Printf("Today: %d tasks completed, %s success, avg %.1f review rounds, $%.0f est. cost\n",
		todayTotal, todayRate, todayRounds, todayCost)

	// This week
	week := firstOr(weekRows)
	weekTotal := toInt(week["total"])
	weekSuccess := toInt(week["succeeded"])
	weekRate := pct(weekSuccess, weekTotal)
	weekCost := estimateCost(toInt(week["total_tokens_in"]), toInt(week["total_tokens_out"]), "")

	fmt.Printf("This week: %d tasks, %s success, $%.0f est. cost\n",
		weekTotal, weekRate, weekCost)

	// Breakdown
	if len(breakdownRows) > 0 {
		fmt.Println()
		fmt.Println("By result:")
		for _, row := range breakdownRows {
			result := toString(row["result"])
			cnt := toInt(row["cnt"])
			ratioStr := pct(cnt, weekTotal)
			fmt.Printf("  %-20s %d (%s)\n", result, cnt, ratioStr)
		}
	}

	// Top specs
	if len(specRows) > 0 {
		fmt.Println()
		fmt.Println("Top specs:")
		for _, row := range specRows {
			spec := toString(row["spec_file"])
			total := toInt(row["total"])
			succeeded := toInt(row["succeeded"])
			rate := pct(succeeded, total)
			hint := ""
			if total > 0 && succeeded*100/total < 70 {
				hint = " <- needs better spec"
			}
			// Show just the filename, not full path
			parts := strings.Split(spec, "/")
			name := parts[len(parts)-1]
			fmt.Printf("  %-30s %s success (%d runs)%s\n", name, rate, total, hint)
		}
	}

	if todayTotal == 0 && weekTotal == 0 {
		fmt.Println()
		fmt.Printf("%s(no agent runs recorded yet)%s\n", dim, reset)
	}

	return nil
}

// metricsBead shows metrics for a specific bead or epic.
func metricsBead(beadID string, jsonOut bool) error {
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
		sqlEsc(beadID), sqlEsc(beadID))

	runsQuery := fmt.Sprintf(`SELECT id, bead_id, model, role, result, review_rounds,
		context_tokens_in, context_tokens_out, duration_seconds, started_at
	FROM agent_runs
	WHERE bead_id = '%s' OR epic_id = '%s'
	ORDER BY started_at DESC
	LIMIT 20`, sqlEsc(beadID), sqlEsc(beadID))

	rows, err := queryJSON(query)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	runsRows, err := queryJSON(runsQuery)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}

	if jsonOut {
		out := map[string]any{
			"summary": firstOr(rows),
			"runs":    runsRows,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	summary := firstOr(rows)
	total := toInt(summary["total"])
	succeeded := toInt(summary["succeeded"])
	rate := pct(succeeded, total)
	avgRounds := toFloat(summary["avg_rounds"])
	tokensIn := toInt(summary["total_tokens_in"])
	tokensOut := toInt(summary["total_tokens_out"])
	cost := estimateCost(tokensIn, tokensOut, "")
	avgDur := toFloat(summary["avg_duration"])

	fmt.Printf("Bead: %s\n", beadID)
	fmt.Printf("  Runs: %d total, %d succeeded (%s)\n", total, succeeded, rate)
	fmt.Printf("  Avg review rounds: %.1f\n", avgRounds)
	fmt.Printf("  Avg duration: %.0fs\n", avgDur)
	fmt.Printf("  Total tokens: %dK in, %dK out\n", tokensIn/1000, tokensOut/1000)
	fmt.Printf("  Est. cost: $%.2f\n", cost)
	fmt.Printf("  Files changed: %d, +%d/-%d lines\n",
		toInt(summary["total_files"]),
		toInt(summary["total_added"]),
		toInt(summary["total_removed"]))

	if len(runsRows) > 0 {
		fmt.Println()
		fmt.Println("  Recent runs:")
		for _, r := range runsRows {
			fmt.Printf("    %-14s %-10s %-8s %-18s rounds=%d  %s\n",
				toString(r["id"]),
				toString(r["model"]),
				toString(r["role"]),
				toString(r["result"]),
				toInt(r["review_rounds"]),
				toString(r["started_at"]),
			)
		}
	}

	if total == 0 {
		fmt.Printf("\n%s(no runs found for %s)%s\n", dim, beadID, reset)
	}

	return nil
}

// metricsModel shows breakdown by model.
func metricsModel(jsonOut bool) error {
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

	rows, err := queryJSON(query)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}

	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}

	if len(rows) == 0 {
		fmt.Printf("%s(no runs this week)%s\n", dim, reset)
		return nil
	}

	var totalCost float64
	var wizardCost, artificerCost float64

	fmt.Println("Model breakdown (this week):")
	fmt.Println()
	for _, row := range rows {
		model := toString(row["model"])
		role := toString(row["role"])
		total := toInt(row["total"])
		avgIn := toInt(row["avg_tokens_in"])
		avgOut := toInt(row["avg_tokens_out"])
		tokIn := toInt(row["total_tokens_in"])
		tokOut := toInt(row["total_tokens_out"])
		succeeded := toInt(row["succeeded"])
		rate := pct(succeeded, total)
		costPerRun := estimateCost(avgIn, avgOut, model)
		subtotal := estimateCost(tokIn, tokOut, model)
		totalCost += subtotal
		if role == "artificer" {
			artificerCost += subtotal
		} else {
			wizardCost += subtotal
		}

		fmt.Printf("  %s (%s): %d runs, %s success, avg %dK tokens, ~$%.2f/run\n",
			model, role, total, rate, (avgIn+avgOut)/1000, costPerRun)
	}

	fmt.Printf("\nTotal cost this week: $%.0f (wizards: $%.0f, artificer: $%.0f)\n",
		totalCost, wizardCost, artificerCost)

	return nil
}

// queryJSON runs a SQL query via bd sql --json and returns parsed rows.
// Returns nil (not an error) when the agent_runs table doesn't exist yet.
func queryJSON(query string) ([]metricsRow, error) {
	cmd := exec.Command("bd", "sql", "--json", query)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	combined := stdout.String() + stderr.String()

	// If the table doesn't exist yet, return empty results gracefully
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

	// bd sql --json may return an error object instead of rows
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

	var rows []metricsRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, fmt.Errorf("parse query result: %w", err)
	}
	return rows, nil
}

// estimateCost returns estimated USD cost based on token counts.
// Pricing (rough per-token):
//   - Sonnet: $3/M input, $15/M output
//   - Opus:   $15/M input, $75/M output
func estimateCost(tokensIn, tokensOut int, model string) float64 {
	inRate := 3.0   // $/M tokens — Sonnet default
	outRate := 15.0  // $/M tokens
	if strings.Contains(strings.ToLower(model), "opus") {
		inRate = 15.0
		outRate = 75.0
	}
	return (float64(tokensIn)*inRate + float64(tokensOut)*outRate) / 1_000_000
}

// pct returns "X%" from succeeded/total, or "0%" if total is 0.
func pct(n, total int) string {
	if total == 0 {
		return "0%"
	}
	return fmt.Sprintf("%d%%", n*100/total)
}

func firstOr(rows []metricsRow) metricsRow {
	if len(rows) > 0 {
		return rows[0]
	}
	return metricsRow{}
}

func toInt(v any) int {
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

func toFloat(v any) float64 {
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

func toString(v any) string {
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

// sqlEsc escapes single quotes for SQL string literals.
func sqlEsc(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}
