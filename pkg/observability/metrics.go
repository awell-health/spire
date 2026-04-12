package observability

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
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

// PhaseBucketRow holds per-bucket token and cost totals for a single bead.
type PhaseBucketRow struct {
	Bucket    string  `json:"bucket"`
	TokensIn  int     `json:"tokens_in"`
	TokensOut int     `json:"tokens_out"`
	CostUSD   float64 `json:"cost_usd"`
}

// MetricsPhaseBuckets returns per-phase-bucket token and cost totals for a bead.
func MetricsPhaseBuckets(beadID string) ([]PhaseBucketRow, error) {
	query := fmt.Sprintf(`SELECT
		phase_bucket,
		COALESCE(SUM(context_tokens_in),0)  AS tokens_in,
		COALESCE(SUM(context_tokens_out),0) AS tokens_out,
		COALESCE(SUM(cost_usd),0)           AS cost_usd
	FROM agent_runs
	WHERE bead_id = '%s'
	  AND phase_bucket IS NOT NULL
	GROUP BY phase_bucket
	ORDER BY FIELD(phase_bucket, 'design', 'implement', 'review')`,
		SqlEsc(beadID))

	rows, err := QueryJSON(query)
	if err != nil {
		return nil, err
	}

	var result []PhaseBucketRow
	for _, row := range rows {
		result = append(result, PhaseBucketRow{
			Bucket:    ToString(row["phase_bucket"]),
			TokensIn:  ToInt(row["tokens_in"]),
			TokensOut: ToInt(row["tokens_out"]),
			CostUSD:   ToFloat(row["cost_usd"]),
		})
	}
	return result, nil
}

// fmtTokens formats a token count with comma separators.
func fmtTokens(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	return strings.Join(parts, ",")
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

	// Fetch per-phase-bucket breakdown (non-fatal on error).
	buckets, _ := MetricsPhaseBuckets(beadID)

	if jsonOut {
		out := map[string]any{
			"summary": FirstOr(rows),
			"runs":    runsRows,
		}
		if len(buckets) > 0 {
			out["phase_buckets"] = buckets
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

	// Render per-phase-bucket breakdown if available.
	if len(buckets) > 0 {
		fmt.Println()
		for _, b := range buckets {
			fmt.Printf("  %s  %-10s %6s in  / %6s out  — %s%s\n",
				Dim, b.Bucket+":", fmtTokens(b.TokensIn), fmtTokens(b.TokensOut), fmtCost(b.CostUSD), Reset)
		}
		fmt.Printf("  %s  ─────────────────────────────────────────%s\n", Dim, Reset)
		fmt.Printf("  %s  %-10s %6s in  / %6s out  — %s%s\n",
			Dim, "total:", fmtTokens(tokensIn), fmtTokens(tokensOut), fmtCost(cost), Reset)
	}

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

// StepRunRow holds a single agent_runs row for per-step metrics aggregation.
type StepRunRow struct {
	Phase        string  `json:"phase"`
	PhaseBucket  string  `json:"phase_bucket"`
	Duration     int     `json:"duration_seconds"`
	CostUSD      float64 `json:"cost_usd"`
	TokensIn     int     `json:"tokens_in"`
	TokensOut    int     `json:"tokens_out"`
	ToolCallsJSON string `json:"tool_calls_json"`
	StartedAt    string  `json:"started_at"`
	CompletedAt  string  `json:"completed_at"`
}

// StepMetricsForBead returns individual agent_runs rows for a bead, ordered by
// started_at. The caller aggregates these into per-step metrics using phase mapping.
func StepMetricsForBead(beadID string) ([]StepRunRow, error) {
	query := fmt.Sprintf(`SELECT phase, phase_bucket, duration_seconds, cost_usd,
		context_tokens_in, context_tokens_out, tool_calls_json,
		started_at, completed_at
	FROM agent_runs
	WHERE bead_id = '%s'
	ORDER BY started_at`, SqlEsc(beadID))

	rows, err := QueryJSON(query)
	if err != nil {
		return nil, err
	}

	var result []StepRunRow
	for _, row := range rows {
		result = append(result, StepRunRow{
			Phase:        ToString(row["phase"]),
			PhaseBucket:  ToString(row["phase_bucket"]),
			Duration:     ToInt(row["duration_seconds"]),
			CostUSD:      ToFloat(row["cost_usd"]),
			TokensIn:     ToInt(row["context_tokens_in"]),
			TokensOut:    ToInt(row["context_tokens_out"]),
			ToolCallsJSON: ToString(row["tool_calls_json"]),
			StartedAt:    ToString(row["started_at"]),
			CompletedAt:  ToString(row["completed_at"]),
		})
	}
	return result, nil
}

// SqlEsc escapes single quotes for SQL string literals.
func SqlEsc(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// DORAResult holds DORA metrics and additional DAG-derived metrics.
type DORAResult struct {
	// Classic DORA four
	DeploymentFrequency []DORAWeekCount  `json:"deployment_frequency"`
	LeadTime            *DORAStats       `json:"lead_time"`
	ChangeFailureRate   *DORAFailRate    `json:"change_failure_rate"`
	MTTR                *DORAStats       `json:"mttr"`
	// DAG metrics
	RetryRate      *RetryStats      `json:"retry_rate,omitempty"`
	ReviewFriction *ReviewStats     `json:"review_friction,omitempty"`
	EscalationRate *EscalationStats `json:"escalation_rate,omitempty"`
	ModelEfficiency []ModelStats    `json:"model_efficiency,omitempty"`
	PhaseDuration  []PhaseStats     `json:"phase_duration,omitempty"`
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
	TotalAttempts int     `json:"total_attempts"`
	Failures      int     `json:"failures"`
	Rate          float64 `json:"rate"`
}

// RetryStats holds retry rate metrics.
type RetryStats struct {
	TotalParents  int     `json:"total_parents"`
	TotalAttempts int     `json:"total_attempts"`
	AvgAttempts   float64 `json:"avg_attempts"`
	MaxAttempts   int     `json:"max_attempts"`
}

// ReviewStats holds review friction metrics.
type ReviewStats struct {
	TotalReviews   int     `json:"total_reviews"`
	AvgPerParent   float64 `json:"avg_per_parent"`
	AvgDurationH   float64 `json:"avg_duration_hours"`
	P50DurationH   float64 `json:"p50_duration_hours"`
	ParentsWithRev int     `json:"parents_with_reviews"`
}

// EscalationStats holds arbiter escalation metrics.
type EscalationStats struct {
	TotalParents int     `json:"total_parents"`
	Escalated    int     `json:"escalated"`
	Rate         float64 `json:"rate"`
}

// ModelStats holds per-model success rate.
type ModelStats struct {
	Model      string  `json:"model"`
	Total      int     `json:"total"`
	Succeeded  int     `json:"succeeded"`
	SuccessRate float64 `json:"success_rate"`
}

// PhaseStats holds per-phase duration metrics.
type PhaseStats struct {
	Phase      string  `json:"phase"`
	Count      int     `json:"count"`
	AvgHours   float64 `json:"avg_hours"`
	P50Hours   float64 `json:"p50_hours"`
	P90Hours   float64 `json:"p90_hours"`
}

// DORAOpts controls what MetricsDORA computes and renders.
type DORAOpts struct {
	JSONOut    bool
	BeadID     string // scope to a single parent bead
	ShowModel  bool   // include model efficiency breakdown
	ShowPhase  bool   // include phase duration breakdown
}

// failureResults are attempt results counted as failures for DORA metrics.
// This deliberately includes "error" and "test_failure" beyond the spec's
// "failure or timeout" — these represent agent infrastructure errors and
// test suite failures that are equally indicative of change quality issues.
var failureResults = map[string]bool{
	"failure": true, "timeout": true, "error": true,
	"test_failure": true, "review_rejected": true,
}

// MetricsDORA computes and displays DORA metrics from the bead DAG.
// All data is derived from attempt, review-round, and step child beads —
// no SQL queries against issues/labels tables.
func MetricsDORA(opts DORAOpts) error {
	type parentData struct {
		parent   store.BoardBead
		children []store.BoardBead
	}

	// --- Fetch parent beads ---
	var parents []store.BoardBead
	if opts.BeadID != "" {
		// Single-bead mode: use the specified bead as the sole parent.
		all, err := store.ListBoardBeads(beads.IssueFilter{
			IDs: []string{opts.BeadID},
		})
		if err != nil {
			return fmt.Errorf("dora: fetch bead %s: %w", opts.BeadID, err)
		}
		parents = all
	} else {
		cutoff := time.Now().AddDate(0, 0, -28)
		all, err := store.ListBoardBeads(beads.IssueFilter{
			Status:      store.StatusPtr(beads.StatusClosed),
			ClosedAfter: &cutoff,
		})
		if err != nil {
			return fmt.Errorf("dora: fetch closed beads: %w", err)
		}
		// Filter out non-work bead types (design, epic, attempt, review-round, step).
		skipTypes := map[string]bool{"design": true, "epic": true}
		for _, b := range all {
			if skipTypes[b.Type] {
				continue
			}
			if store.IsAttemptBoardBead(b) || store.IsReviewRoundBoardBead(b) || store.IsStepBoardBead(b) {
				continue
			}
			parents = append(parents, b)
		}
	}

	if len(parents) == 0 {
		result := &DORAResult{}
		return renderDORAOutput(result, opts)
	}

	// --- Batch-fetch children as BoardBeads (for timestamps) ---
	parentIDs := make([]string, len(parents))
	for i, p := range parents {
		parentIDs[i] = p.ID
	}
	childMap, err := store.GetChildrenBoardBatch(parentIDs)
	if err != nil {
		return fmt.Errorf("dora: fetch children: %w", err)
	}

	result := computeDORA(parents, childMap, opts)
	return renderDORAOutput(result, opts)
}

// attemptInfo pairs a BoardBead with its extracted result string.
type attemptInfo struct {
	bead   store.BoardBead
	result string
}

// computeDORA computes all DORA and DAG metrics from pre-fetched parent beads
// and their children. Separated from MetricsDORA for testability.
func computeDORA(parents []store.BoardBead, childMap map[string][]store.BoardBead, opts DORAOpts) *DORAResult {
	// --- Classify children and extract attempt results ---
	parentAttempts := make(map[string][]attemptInfo)
	parentReviews := make(map[string][]store.BoardBead)
	parentSteps := make(map[string][]store.BoardBead)

	for _, p := range parents {
		children := childMap[p.ID]
		for _, c := range children {
			if store.IsAttemptBoardBead(c) {
				// Extract result from label, fall back to comments for legacy data.
				res := c.HasLabelPrefix("result:")
				if res == "" {
					// Convert to lightweight Bead for AttemptResult helper.
					lb := store.Bead{ID: c.ID, Labels: c.Labels}
					res = store.AttemptResult(lb)
				}
				parentAttempts[p.ID] = append(parentAttempts[p.ID], attemptInfo{bead: c, result: res})
			} else if store.IsReviewRoundBoardBead(c) {
				parentReviews[p.ID] = append(parentReviews[p.ID], c)
			} else if store.IsStepBoardBead(c) {
				parentSteps[p.ID] = append(parentSteps[p.ID], c)
			}
		}
		// Sort attempts by UpdatedAt ascending for ordering.
		atts := parentAttempts[p.ID]
		sort.Slice(atts, func(i, j int) bool {
			return atts[i].bead.UpdatedAt < atts[j].bead.UpdatedAt
		})
	}

	// --- Compute metrics ---
	result := &DORAResult{}

	// 1. Merge Frequency — every closed parent bead is a merge event, grouped by week.
	weekCounts := map[string]int{}
	var successParents []store.BoardBead
	for _, p := range parents {
		// Every closed parent bead = a merge event.
		ts := p.ClosedAt
		if ts == "" {
			ts = p.UpdatedAt
		}
		weekCounts[weekKey(ts)]++

		// Track successful-attempt parents separately for lead time.
		atts := parentAttempts[p.ID]
		if len(atts) == 0 {
			continue
		}
		if atts[len(atts)-1].result == "success" {
			successParents = append(successParents, p)
		}
	}
	// Sort weeks and build result.
	weeks := sortedKeys(weekCounts)
	for _, wk := range weeks {
		result.DeploymentFrequency = append(result.DeploymentFrequency, DORAWeekCount{
			Week:   wk,
			Merged: weekCounts[wk],
		})
	}

	// 2. Lead Time — first attempt CreatedAt → last successful attempt UpdatedAt.
	var leadTimes []float64
	for _, p := range successParents {
		atts := parentAttempts[p.ID]
		if len(atts) == 0 {
			continue
		}
		first := atts[0].bead
		// Find last successful attempt.
		var lastSuccess *store.BoardBead
		for i := len(atts) - 1; i >= 0; i-- {
			if atts[i].result == "success" {
				lastSuccess = &atts[i].bead
				break
			}
		}
		if lastSuccess == nil {
			continue
		}
		start := parseTime(first.CreatedAt)
		// Use ClosedAt of the last successful attempt if available, else UpdatedAt.
		endTS := lastSuccess.ClosedAt
		if endTS == "" {
			endTS = lastSuccess.UpdatedAt
		}
		end := parseTime(endTS)
		if !start.IsZero() && !end.IsZero() && end.After(start) {
			leadTimes = append(leadTimes, end.Sub(start).Hours())
		}
	}
	sortFloats(leadTimes)
	if len(leadTimes) > 0 {
		result.LeadTime = &DORAStats{
			AvgHours: avg(leadTimes),
			P50Hours: percentile(leadTimes, 0.50),
			P90Hours: percentile(leadTimes, 0.90),
			Count:    len(leadTimes),
		}
	}

	// 3. Change Failure Rate — failed attempts / total attempts.
	var totalAttempts, failedAttempts int
	for _, atts := range parentAttempts {
		for _, a := range atts {
			totalAttempts++
			if failureResults[a.result] {
				failedAttempts++
			}
		}
	}
	if totalAttempts > 0 {
		result.ChangeFailureRate = &DORAFailRate{
			TotalAttempts: totalAttempts,
			Failures:      failedAttempts,
			Rate:          float64(failedAttempts) * 100 / float64(totalAttempts),
		}
	}

	// 4. MTTR — time between a failed attempt closing and the next successful attempt on same parent.
	var recoveryTimes []float64
	for _, p := range parents {
		atts := parentAttempts[p.ID]
		for i := 0; i < len(atts)-1; i++ {
			if !failureResults[atts[i].result] {
				continue
			}
			// Find next successful attempt.
			for j := i + 1; j < len(atts); j++ {
				if atts[j].result == "success" {
					failEnd := parseTime(atts[i].bead.ClosedAt)
					if failEnd.IsZero() {
						failEnd = parseTime(atts[i].bead.UpdatedAt)
					}
					successEnd := parseTime(atts[j].bead.ClosedAt)
					if successEnd.IsZero() {
						successEnd = parseTime(atts[j].bead.UpdatedAt)
					}
					if !failEnd.IsZero() && !successEnd.IsZero() && successEnd.After(failEnd) {
						recoveryTimes = append(recoveryTimes, successEnd.Sub(failEnd).Hours())
					}
					break
				}
			}
		}
	}
	sortFloats(recoveryTimes)
	if len(recoveryTimes) > 0 {
		result.MTTR = &DORAStats{
			AvgHours: avg(recoveryTimes),
			P50Hours: percentile(recoveryTimes, 0.50),
			P90Hours: percentile(recoveryTimes, 0.90),
			Count:    len(recoveryTimes),
		}
	}

	// 5. Retry Rate — attempts per parent.
	parentsWithAttempts := 0
	maxAttempts := 0
	for _, atts := range parentAttempts {
		if len(atts) == 0 {
			continue
		}
		parentsWithAttempts++
		if len(atts) > maxAttempts {
			maxAttempts = len(atts)
		}
	}
	if parentsWithAttempts > 0 {
		result.RetryRate = &RetryStats{
			TotalParents:  parentsWithAttempts,
			TotalAttempts: totalAttempts,
			AvgAttempts:   float64(totalAttempts) / float64(parentsWithAttempts),
			MaxAttempts:   maxAttempts,
		}
	}

	// 6. Review Friction — review-round count and avg time open.
	var reviewDurations []float64
	totalReviews := 0
	parentsWithReviews := 0
	for _, reviews := range parentReviews {
		if len(reviews) == 0 {
			continue
		}
		parentsWithReviews++
		totalReviews += len(reviews)
		for _, r := range reviews {
			start := parseTime(r.CreatedAt)
			endTS := r.ClosedAt
			if endTS == "" {
				endTS = r.UpdatedAt
			}
			end := parseTime(endTS)
			if !start.IsZero() && !end.IsZero() && end.After(start) {
				reviewDurations = append(reviewDurations, end.Sub(start).Hours())
			}
		}
	}
	sortFloats(reviewDurations)
	if totalReviews > 0 {
		avgPerParent := float64(totalReviews) / float64(parentsWithReviews)
		result.ReviewFriction = &ReviewStats{
			TotalReviews:   totalReviews,
			AvgPerParent:   avgPerParent,
			AvgDurationH:   avg(reviewDurations),
			P50DurationH:   percentile(reviewDurations, 0.50),
			ParentsWithRev: parentsWithReviews,
		}
	}

	// 7. Escalation Rate — arbiter step beads activated.
	escalated := 0
	for _, steps := range parentSteps {
		for _, s := range steps {
			phase := s.HasLabelPrefix("step:")
			if phase == "arbiter" && s.Status != "open" {
				escalated++
				break // count each parent only once
			}
		}
	}
	if len(parents) > 0 {
		result.EscalationRate = &EscalationStats{
			TotalParents: len(parents),
			Escalated:    escalated,
			Rate:         float64(escalated) * 100 / float64(len(parents)),
		}
	}

	// 8. Model Efficiency — success rate by model label on attempt beads.
	if opts.ShowModel {
		modelTotal := map[string]int{}
		modelSuccess := map[string]int{}
		for _, atts := range parentAttempts {
			for _, a := range atts {
				model := a.bead.HasLabelPrefix("model:")
				if model == "" {
					model = "unknown"
				}
				modelTotal[model]++
				if a.result == "success" {
					modelSuccess[model]++
				}
			}
		}
		for _, m := range sortedKeys(modelTotal) {
			total := modelTotal[m]
			succeeded := modelSuccess[m]
			result.ModelEfficiency = append(result.ModelEfficiency, ModelStats{
				Model:       m,
				Total:       total,
				Succeeded:   succeeded,
				SuccessRate: float64(succeeded) * 100 / float64(total),
			})
		}
	}

	// 9. Phase Duration — step bead open duration by phase.
	if opts.ShowPhase {
		phaseDurations := map[string][]float64{}
		for _, steps := range parentSteps {
			for _, s := range steps {
				if s.Status != "closed" {
					continue
				}
				phase := s.HasLabelPrefix("step:")
				if phase == "" {
					continue
				}
				start := parseTime(s.CreatedAt)
				endTS := s.ClosedAt
				if endTS == "" {
					endTS = s.UpdatedAt
				}
				end := parseTime(endTS)
				if !start.IsZero() && !end.IsZero() && end.After(start) {
					phaseDurations[phase] = append(phaseDurations[phase], end.Sub(start).Hours())
				}
			}
		}
		for _, phase := range sortedKeys(phaseDurations) {
			vals := phaseDurations[phase]
			sortFloats(vals)
			result.PhaseDuration = append(result.PhaseDuration, PhaseStats{
				Phase:    phase,
				Count:    len(vals),
				AvgHours: avg(vals),
				P50Hours: percentile(vals, 0.50),
				P90Hours: percentile(vals, 0.90),
			})
		}
	}

	return result
}

func renderDORAOutput(result *DORAResult, opts DORAOpts) error {
	if opts.JSONOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}
	return renderDORAText(result, opts)
}

func renderDORAText(r *DORAResult, opts DORAOpts) error {
	fmt.Println("DORA Metrics (last 28 days)")
	fmt.Println()

	// Merge Frequency
	fmt.Println("Merge Frequency:")
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
		fmt.Printf("  %d of %d attempts failed (%.0f%%)\n",
			r.ChangeFailureRate.Failures, r.ChangeFailureRate.TotalAttempts,
			r.ChangeFailureRate.Rate)
	}
	fmt.Println()

	// MTTR
	fmt.Println("Mean Time to Recovery:")
	if r.MTTR == nil {
		fmt.Printf("  %s(no recovery events in period)%s\n", Dim, Reset)
	} else {
		fmt.Printf("  Avg: %.1fh   P50: %.1fh   P90: %.1fh   (%d recoveries)\n",
			r.MTTR.AvgHours, r.MTTR.P50Hours, r.MTTR.P90Hours, r.MTTR.Count)
	}
	fmt.Println()

	// Retry Rate
	fmt.Println("Retry Rate:")
	if r.RetryRate == nil {
		fmt.Printf("  %s(no data)%s\n", Dim, Reset)
	} else {
		fmt.Printf("  %.1f attempts/bead avg   max %d   (%d beads, %d attempts)\n",
			r.RetryRate.AvgAttempts, r.RetryRate.MaxAttempts,
			r.RetryRate.TotalParents, r.RetryRate.TotalAttempts)
	}
	fmt.Println()

	// Review Friction
	fmt.Println("Review Friction:")
	if r.ReviewFriction == nil {
		fmt.Printf("  %s(no reviews in period)%s\n", Dim, Reset)
	} else {
		fmt.Printf("  %.1f reviews/bead avg   avg duration %.1fh   P50 %.1fh   (%d reviews across %d beads)\n",
			r.ReviewFriction.AvgPerParent, r.ReviewFriction.AvgDurationH,
			r.ReviewFriction.P50DurationH, r.ReviewFriction.TotalReviews,
			r.ReviewFriction.ParentsWithRev)
	}
	fmt.Println()

	// Escalation Rate
	fmt.Println("Escalation Rate:")
	if r.EscalationRate == nil {
		fmt.Printf("  %s(no data)%s\n", Dim, Reset)
	} else {
		fmt.Printf("  %d of %d beads escalated to arbiter (%.0f%%)\n",
			r.EscalationRate.Escalated, r.EscalationRate.TotalParents,
			r.EscalationRate.Rate)
	}

	// Model Efficiency (only with --model)
	if opts.ShowModel && len(r.ModelEfficiency) > 0 {
		fmt.Println()
		fmt.Println("Model Efficiency:")
		for _, m := range r.ModelEfficiency {
			fmt.Printf("  %-30s %d/%d (%.0f%% success)\n",
				m.Model, m.Succeeded, m.Total, m.SuccessRate)
		}
	}

	// Phase Duration (only with --phase)
	if opts.ShowPhase && len(r.PhaseDuration) > 0 {
		fmt.Println()
		fmt.Println("Phase Duration:")
		fmt.Printf("  %-14s %5s %8s %8s %8s\n", "PHASE", "COUNT", "AVG", "P50", "P90")
		fmt.Printf("  %-14s %5s %8s %8s %8s\n", "─────", "─────", "───", "───", "───")
		for _, p := range r.PhaseDuration {
			fmt.Printf("  %-14s %5d %7.1fh %7.1fh %7.1fh\n",
				p.Phase, p.Count, p.AvgHours, p.P50Hours, p.P90Hours)
		}
	}

	return nil
}

// --- helpers ---

// parseTime parses an RFC3339 timestamp string. Returns zero time on failure.
func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// weekKey returns a YYYY-Wnn week key from an RFC3339 timestamp.
func weekKey(ts string) string {
	t := parseTime(ts)
	if t.IsZero() {
		return "unknown"
	}
	y, w := t.ISOWeek()
	return fmt.Sprintf("%d-W%02d", y, w)
}

// sortedKeys returns the sorted keys of a map.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sortFloats sorts a float64 slice in place (ascending).
func sortFloats(vals []float64) {
	sort.Float64s(vals)
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

// --- Stream 1: Failure and retry visibility ---

// RetryRow holds per-phase retry counts.
type RetryRow struct {
	Phase        string `json:"phase"`
	TotalRuns    int    `json:"total_runs"`
	Retries      int    `json:"retries"`       // runs with attempt_number > 1
	MaxAttempts  int    `json:"max_attempts"`
}

// MetricsRetry returns steps with retries (attempt_number > 1), grouped by phase.
func MetricsRetry() ([]RetryRow, error) {
	query := `SELECT
		phase,
		COUNT(*) as total_runs,
		SUM(CASE WHEN attempt_number > 1 THEN 1 ELSE 0 END) as retries,
		MAX(COALESCE(attempt_number, 1)) as max_attempts
	FROM agent_runs
	WHERE phase IS NOT NULL
		AND started_at >= DATE_SUB(CURDATE(), INTERVAL 30 DAY)
	GROUP BY phase
	HAVING retries > 0
	ORDER BY retries DESC`

	rows, err := QueryJSON(query)
	if err != nil {
		return nil, err
	}
	var result []RetryRow
	for _, r := range rows {
		result = append(result, RetryRow{
			Phase:       ToString(r["phase"]),
			TotalRuns:   ToInt(r["total_runs"]),
			Retries:     ToInt(r["retries"]),
			MaxAttempts: ToInt(r["max_attempts"]),
		})
	}
	return result, nil
}

// FailureBreakdownRow holds per-failure-class counts.
type FailureBreakdownRow struct {
	FailureClass string `json:"failure_class"`
	Count        int    `json:"count"`
	Phase        string `json:"phase"`
}

// MetricsFailureBreakdown returns failure counts grouped by failure_class and phase.
func MetricsFailureBreakdown() ([]FailureBreakdownRow, error) {
	query := `SELECT
		COALESCE(failure_class, 'none') as failure_class,
		phase,
		COUNT(*) as cnt
	FROM agent_runs
	WHERE failure_class IS NOT NULL AND failure_class != ''
		AND started_at >= DATE_SUB(CURDATE(), INTERVAL 30 DAY)
	GROUP BY failure_class, phase
	ORDER BY cnt DESC`

	rows, err := QueryJSON(query)
	if err != nil {
		return nil, err
	}
	var result []FailureBreakdownRow
	for _, r := range rows {
		result = append(result, FailureBreakdownRow{
			FailureClass: ToString(r["failure_class"]),
			Phase:        ToString(r["phase"]),
			Count:        ToInt(r["cnt"]),
		})
	}
	return result, nil
}

// --- Stream 2: Step duration surfacing ---

// StepDurationRow holds per-step timing for a bead's graph execution.
type StepDurationRow struct {
	Step       string  `json:"step"`
	Status     string  `json:"status"`
	DurationS  float64 `json:"duration_seconds"`
	Attempts   int     `json:"attempts"`
}

// MetricsStepDurations reads graph state JSON for a bead and extracts per-step
// StartedAt/CompletedAt to return a duration breakdown. This doesn't need the
// agent_runs table — the data lives in the graph state on disk.
func MetricsStepDurations(beadID string) ([]StepDurationRow, error) {
	// Query agent_runs for per-step records with timing data.
	query := fmt.Sprintf(`SELECT
		phase as step,
		result as status,
		COALESCE(duration_seconds, 0) as duration_s,
		COALESCE(attempt_number, 1) as attempts
	FROM agent_runs
	WHERE (bead_id = '%s' OR epic_id = '%s')
		AND phase IS NOT NULL
	ORDER BY started_at ASC`,
		SqlEsc(beadID), SqlEsc(beadID))

	rows, err := QueryJSON(query)
	if err != nil {
		return nil, err
	}
	var result []StepDurationRow
	for _, r := range rows {
		result = append(result, StepDurationRow{
			Step:      ToString(r["step"]),
			Status:    ToString(r["status"]),
			DurationS: ToFloat(r["duration_s"]),
			Attempts:  ToInt(r["attempts"]),
		})
	}
	return result, nil
}

// --- Stream 3: Tool call tracking ---

// ToolUsageRow holds average tool calls per phase.
type ToolUsageRow struct {
	Phase       string  `json:"phase"`
	AvgReads    float64 `json:"avg_reads"`
	AvgEdits    float64 `json:"avg_edits"`
	TotalRuns   int     `json:"total_runs"`
	MaxReads    int     `json:"max_reads"`
}

// MetricsToolUsage returns average read/edit tool calls per phase for the last 30 days.
func MetricsToolUsage() ([]ToolUsageRow, error) {
	query := `SELECT
		phase,
		AVG(COALESCE(read_calls, 0)) as avg_reads,
		AVG(COALESCE(edit_calls, 0)) as avg_edits,
		COUNT(*) as total_runs,
		MAX(COALESCE(read_calls, 0)) as max_reads
	FROM agent_runs
	WHERE phase IS NOT NULL
		AND (read_calls IS NOT NULL OR edit_calls IS NOT NULL)
		AND started_at >= DATE_SUB(CURDATE(), INTERVAL 30 DAY)
	GROUP BY phase
	ORDER BY avg_reads DESC`

	rows, err := QueryJSON(query)
	if err != nil {
		return nil, err
	}
	var result []ToolUsageRow
	for _, r := range rows {
		result = append(result, ToolUsageRow{
			Phase:     ToString(r["phase"]),
			AvgReads:  ToFloat(r["avg_reads"]),
			AvgEdits:  ToFloat(r["avg_edits"]),
			TotalRuns: ToInt(r["total_runs"]),
			MaxReads:  ToInt(r["max_reads"]),
		})
	}
	return result, nil
}

// ThrashingRun identifies runs with abnormally high read counts.
type ThrashingRun struct {
	RunID     string `json:"run_id"`
	BeadID    string `json:"bead_id"`
	Phase     string `json:"phase"`
	ReadCalls int    `json:"read_calls"`
	EditCalls int    `json:"edit_calls"`
}

// MetricsThrashingDetection flags runs where read_calls > 100 (potential thrashing).
func MetricsThrashingDetection() ([]ThrashingRun, error) {
	query := `SELECT
		id, bead_id, phase,
		COALESCE(read_calls, 0) as read_calls,
		COALESCE(edit_calls, 0) as edit_calls
	FROM agent_runs
	WHERE read_calls > 100
		AND started_at >= DATE_SUB(CURDATE(), INTERVAL 30 DAY)
	ORDER BY read_calls DESC
	LIMIT 20`

	rows, err := QueryJSON(query)
	if err != nil {
		return nil, err
	}
	var result []ThrashingRun
	for _, r := range rows {
		result = append(result, ThrashingRun{
			RunID:     ToString(r["id"]),
			BeadID:    ToString(r["bead_id"]),
			Phase:     ToString(r["phase"]),
			ReadCalls: ToInt(r["read_calls"]),
			EditCalls: ToInt(r["edit_calls"]),
		})
	}
	return result, nil
}

// --- Stream 4: Bug causality ---

// BugCausalityRow holds a source bead and its bug count.
type BugCausalityRow struct {
	SourceBeadID string `json:"source_bead_id"`
	SourceTitle  string `json:"source_title"`
	BugCount     int    `json:"bug_count"`
	FormulaName  string `json:"formula_name,omitempty"`
}

// MetricsBugCausality returns top beads that produced the most bugs (via caused-by deps)
// in the last N days. Uses the store API to query dep relationships.
func MetricsBugCausality(days int) ([]BugCausalityRow, error) {
	if days <= 0 {
		days = 30
	}
	// Find recent bug beads.
	bugFilter := store.BugFilter("bug", days)
	bugs, err := store.ListBeads(bugFilter)
	if err != nil {
		return nil, fmt.Errorf("list bug beads: %w", err)
	}

	// For each bug, look up its caused-by deps.
	sourceCount := make(map[string]int)
	sourceTitles := make(map[string]string)
	for _, bug := range bugs {
		causers, err := store.GetCausedByDeps(bug.ID)
		if err != nil {
			continue // skip on error
		}
		for _, causer := range causers {
			sourceCount[causer.ID]++
			if _, ok := sourceTitles[causer.ID]; !ok {
				sourceTitles[causer.ID] = causer.Title
			}
		}
	}

	// Build result sorted by bug count descending.
	var result []BugCausalityRow
	for id, count := range sourceCount {
		result = append(result, BugCausalityRow{
			SourceBeadID: id,
			SourceTitle:  sourceTitles[id],
			BugCount:     count,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].BugCount > result[j].BugCount
	})
	if len(result) > 5 {
		result = result[:5]
	}
	return result, nil
}
