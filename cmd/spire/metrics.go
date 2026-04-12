package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/observability"
	"github.com/awell-health/spire/pkg/olap"
	"github.com/spf13/cobra"
)

var metricsCmd = &cobra.Command{
	Use:   "metrics",
	Short: "Agent run metrics (--bead, --model, --phase, --dora, --trends, --json)",
	RunE: func(cmd *cobra.Command, args []string) error {
		var fullArgs []string
		if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
			fullArgs = append(fullArgs, "--json")
		}
		if model, _ := cmd.Flags().GetBool("model"); model {
			fullArgs = append(fullArgs, "--model")
		}
		if phase, _ := cmd.Flags().GetBool("phase"); phase {
			fullArgs = append(fullArgs, "--phase")
		}
		if dora, _ := cmd.Flags().GetBool("dora"); dora {
			fullArgs = append(fullArgs, "--dora")
		}
		if trends, _ := cmd.Flags().GetBool("trends"); trends {
			fullArgs = append(fullArgs, "--trends")
		}
		if v, _ := cmd.Flags().GetString("bead"); v != "" {
			fullArgs = append(fullArgs, "--bead", v)
		}
		if failures, _ := cmd.Flags().GetBool("failures"); failures {
			fullArgs = append(fullArgs, "--failures")
		}
		if tools, _ := cmd.Flags().GetBool("tools"); tools {
			fullArgs = append(fullArgs, "--tools")
		}
		if bugs, _ := cmd.Flags().GetBool("bugs"); bugs {
			fullArgs = append(fullArgs, "--bugs")
		}
		if fallback, _ := cmd.Flags().GetBool("fallback"); fallback {
			fullArgs = append(fullArgs, "--fallback")
		}
		return cmdMetrics(fullArgs)
	},
}

func init() {
	metricsCmd.Flags().Bool("json", false, "Output as JSON")
	metricsCmd.Flags().Bool("model", false, "Show model breakdown")
	metricsCmd.Flags().Bool("phase", false, "Show per-phase breakdown")
	metricsCmd.Flags().Bool("dora", false, "Show DORA metrics")
	metricsCmd.Flags().String("bead", "", "Show metrics for a specific bead")
	metricsCmd.Flags().Bool("trends", false, "Show week-over-week trend lines")
	metricsCmd.Flags().Bool("failures", false, "Show failure breakdown and retry rates")
	metricsCmd.Flags().Bool("tools", false, "Show tool usage per phase")
	metricsCmd.Flags().Bool("bugs", false, "Show bug causality top-5")
	metricsCmd.Flags().Bool("fallback", false, "Use Dolt fallback when DuckDB is unavailable (backward compat)")
}

// metricsJSONEncode writes v as indented JSON to stdout.
func metricsJSONEncode(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func cmdMetrics(args []string) error {
	var (
		flagJSON     bool
		flagBead     string
		flagModel    bool
		flagPhase    bool
		flagDORA     bool
		flagTrends   bool
		flagFailures bool
		flagTools    bool
		flagBugs     bool
		flagFallback bool
	)
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			flagJSON = true
		case "--model":
			flagModel = true
		case "--phase":
			flagPhase = true
		case "--dora":
			flagDORA = true
		case "--trends":
			flagTrends = true
		case "--failures":
			flagFailures = true
		case "--tools":
			flagTools = true
		case "--bugs":
			flagBugs = true
		case "--fallback":
			flagFallback = true
		case "--bead":
			if i+1 >= len(args) {
				return fmt.Errorf("--bead requires a value")
			}
			i++
			flagBead = args[i]
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire metrics [--bead <id>] [--model] [--phase] [--dora] [--trends] [--failures] [--tools] [--bugs] [--fallback] [--json]", args[i])
		}
	}

	// Open DuckDB OLAP database for fast analytical queries.
	var adb *olap.DB
	var olapErr error
	if tc, err := config.ActiveTowerConfig(); err == nil {
		if db, err := olap.Open(tc.OLAPPath()); err == nil {
			adb = db
		} else {
			olapErr = err
		}
	} else {
		olapErr = err
	}
	if adb != nil {
		defer adb.Close()
	} else if !flagFallback {
		// DuckDB is required unless --fallback is explicitly set.
		msg := "OLAP database unavailable"
		if olapErr != nil {
			msg += fmt.Sprintf(" (%v)", olapErr)
		}
		return fmt.Errorf("%s. Run `spire up` to start services, or use --fallback for Dolt queries", msg)
	}

	since := time.Now().AddDate(0, -3, 0) // 90-day default

	if flagTrends {
		if adb != nil {
			trends, err := adb.QueryTrends(since)
			if err != nil {
				return fmt.Errorf("metrics: %w", err)
			}
			if flagJSON {
				return metricsJSONEncode(trends)
			}
			renderWeeklyTrends(trends)
			return nil
		}
		// Fallback to Dolt
		result, err := observability.MetricsTrends(12)
		if err != nil {
			return fmt.Errorf("metrics: %w", err)
		}
		if flagJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(result)
		}
		renderTrendsFallback(result)
		return nil
	}

	if flagDORA {
		if adb != nil {
			dora, err := adb.QueryDORA(since)
			if err != nil {
				return fmt.Errorf("metrics: %w", err)
			}
			if flagJSON {
				return metricsJSONEncode(dora)
			}
			renderDORAStats(dora)
			return nil
		}
		return observability.MetricsDORA(observability.DORAOpts{
			JSONOut:   flagJSON,
			BeadID:    flagBead,
			ShowModel: flagModel,
			ShowPhase: flagPhase,
		})
	}

	if flagBead != "" {
		if adb != nil {
			return renderBeadMetrics(adb, flagBead, flagJSON)
		}
		if err := observability.MetricsBead(flagBead, flagJSON); err != nil {
			return err
		}
		steps, err := observability.MetricsStepDurations(flagBead)
		if err == nil && len(steps) > 0 {
			fmt.Println()
			fmt.Println("  Step durations:")
			for _, s := range steps {
				fmt.Printf("    %-14s %-12s %6.0fs  (attempt %d)\n",
					s.Step, s.Status, s.DurationS, s.Attempts)
			}
		}
		return nil
	}

	if flagFailures {
		if adb != nil {
			failures, err := adb.QueryFailures(since)
			if err != nil {
				return fmt.Errorf("metrics: %w", err)
			}
			if flagJSON {
				return metricsJSONEncode(failures)
			}
			renderFailureStats(failures)
			return nil
		}
		return renderFailuresFallback(flagJSON)
	}

	if flagTools {
		if adb != nil {
			// Try OTel-sourced tool_events first (richer data: per-tool duration, failure rate).
			otelTools, otelErr := adb.QueryToolEvents(since)
			if otelErr == nil && len(otelTools) > 0 {
				if flagJSON {
					return metricsJSONEncode(otelTools)
				}
				renderOTelToolStats(otelTools)
				return nil
			}

			// Fall back to old tool_usage_stats view for historical data.
			tools, err := adb.QueryToolUsage(since)
			if err != nil {
				return fmt.Errorf("metrics: %w", err)
			}
			if flagJSON {
				return metricsJSONEncode(tools)
			}
			renderToolStats(tools)
			return nil
		}
		return renderToolUsageFallback(flagJSON)
	}

	if flagBugs {
		if adb != nil {
			bugs, err := adb.QueryBugCausality(5)
			if err != nil {
				return fmt.Errorf("metrics: %w", err)
			}
			if flagJSON {
				return metricsJSONEncode(bugs)
			}
			renderBugStats(bugs)
			return nil
		}
		return renderBugCausalityFallback(flagJSON)
	}

	if flagModel {
		if adb != nil {
			models, err := adb.QueryModelBreakdown(since)
			if err != nil {
				return fmt.Errorf("metrics: %w", err)
			}
			if flagJSON {
				return metricsJSONEncode(models)
			}
			renderModelBreakdown(models)
			return nil
		}
		return observability.MetricsModel(flagJSON)
	}

	if flagPhase {
		if adb != nil {
			phases, err := adb.QueryPhaseBreakdown(since)
			if err != nil {
				return fmt.Errorf("metrics: %w", err)
			}
			if flagJSON {
				return metricsJSONEncode(phases)
			}
			renderPhaseBreakdown(phases)
			return nil
		}
		return observability.MetricsPhase(flagJSON)
	}

	// Default summary
	if adb != nil {
		stats, err := adb.QuerySummary(since)
		if err != nil {
			return fmt.Errorf("metrics: %w", err)
		}
		if flagJSON {
			return metricsJSONEncode(stats)
		}
		renderSummaryStats(stats)
		appendFormulaComparison()
		return nil
	}
	if err := observability.MetricsSummary(flagJSON); err != nil {
		return err
	}
	appendFormulaComparison()
	return nil
}

// ---------------------------------------------------------------------------
// OLAP rendering functions
// ---------------------------------------------------------------------------

func renderSummaryStats(s *olap.SummaryStats) {
	fmt.Println("Agent Runs (last 90 days)")
	fmt.Printf("  Total: %d   Success: %d   Failures: %d   (%.1f%% success rate)\n",
		s.TotalRuns, s.Successes, s.Failures, s.SuccessRate)
	fmt.Printf("  Avg cost: $%.4f   Avg duration: %.0fs   Total cost: $%.2f\n",
		s.AvgCostUSD, s.AvgDurationS, s.TotalCostUSD)
}

func renderModelBreakdown(models []olap.ModelStats) {
	if len(models) == 0 {
		fmt.Println("(no model data)")
		return
	}
	fmt.Println("Model breakdown (last 90 days)")
	fmt.Println()
	fmt.Printf("  %-25s %5s %8s %9s %8s %10s\n", "MODEL", "RUNS", "SUCCESS", "AVG COST", "AVG DUR", "TOKENS")
	fmt.Printf("  %-25s %5s %8s %9s %8s %10s\n", "─────", "────", "───────", "────────", "───────", "──────")
	for _, m := range models {
		fmt.Printf("  %-25s %5d %7.1f%% $%7.4f %7.0fs %10d\n",
			m.Model, m.RunCount, m.SuccessRate, m.AvgCostUSD, m.AvgDurationS, m.TotalTokens)
	}
}

func renderPhaseBreakdown(phases []olap.PhaseStats) {
	if len(phases) == 0 {
		fmt.Println("(no phase data)")
		return
	}
	fmt.Println("Per-phase breakdown (last 90 days)")
	fmt.Println()
	fmt.Printf("  %-14s %5s %8s %9s %8s\n", "PHASE", "RUNS", "SUCCESS", "AVG COST", "AVG DUR")
	fmt.Printf("  %-14s %5s %8s %9s %8s\n", "─────", "────", "───────", "────────", "───────")
	for _, p := range phases {
		fmt.Printf("  %-14s %5d %7.1f%% $%7.4f %7.0fs\n",
			p.Phase, p.RunCount, p.SuccessRate, p.AvgCostUSD, p.AvgDurationS)
	}
}

func renderDORAStats(d *olap.DORAMetrics) {
	fmt.Println("DORA Metrics (last 90 days)")
	fmt.Println()
	fmt.Printf("  Deploy Frequency:    %.1f deploys/week\n", d.DeployFrequency)
	leadH := d.LeadTimeSeconds / 3600
	fmt.Printf("  Lead Time:           %.0fs (%.1fh)\n", d.LeadTimeSeconds, leadH)
	fmt.Printf("  Change Failure Rate: %.1f%%\n", d.ChangeFailureRate*100)
	mttrH := d.MTTRSeconds / 3600
	fmt.Printf("  MTTR:                %.0fs (%.1fh)\n", d.MTTRSeconds, mttrH)
}

func renderWeeklyTrends(trends []olap.WeeklyTrend) {
	if len(trends) == 0 {
		fmt.Println("(no trend data)")
		return
	}
	fmt.Println("Weekly trends (last 90 days)")
	fmt.Println()
	fmt.Printf("  %-12s %5s %8s %10s %7s\n", "WEEK", "RUNS", "SUCCESS", "COST", "MERGES")
	fmt.Printf("  %-12s %5s %8s %10s %7s\n", "────", "────", "───────", "────", "──────")
	for _, t := range trends {
		fmt.Printf("  %-12s %5d %7.1f%% $%8.2f %7d\n",
			t.WeekStart.Format("2006-01-02"), t.RunCount, t.SuccessRate, t.TotalCostUSD, t.MergeCount)
	}
}

func renderFailureStats(failures []olap.FailureStats) {
	if len(failures) == 0 {
		fmt.Println("No failures in the last 90 days")
		return
	}
	fmt.Println("Failure breakdown (last 90 days)")
	fmt.Printf("  %-20s %5s %8s\n", "FAILURE CLASS", "COUNT", "PERCENT")
	fmt.Printf("  %-20s %5s %8s\n", "─────────────", "─────", "───────")
	for _, f := range failures {
		fmt.Printf("  %-20s %5d %7.1f%%\n", f.FailureClass, f.Count, f.Percentage)
	}
}

func renderToolStats(tools []olap.ToolUsageStats) {
	if len(tools) == 0 {
		fmt.Println("No tool usage data yet")
		return
	}
	fmt.Println("Tool usage by formula/phase")
	fmt.Printf("  %-20s %-14s %6s %6s %6s %8s\n", "FORMULA", "PHASE", "READS", "EDITS", "TOTAL", "READ %")
	fmt.Printf("  %-20s %-14s %6s %6s %6s %8s\n", "───────", "─────", "─────", "─────", "─────", "──────")
	for _, t := range tools {
		fmt.Printf("  %-20s %-14s %6d %6d %6d %7.1f%%\n",
			t.FormulaName, t.Phase, t.TotalRead, t.TotalEdit, t.TotalTools, t.ReadRatio*100)
	}
}

func renderOTelToolStats(tools []olap.ToolEventStats) {
	if len(tools) == 0 {
		fmt.Println("No tool events recorded yet")
		return
	}
	fmt.Println("Tool usage (OTel pipeline)")
	fmt.Printf("  %-16s %7s %10s %9s\n", "TOOL", "CALLS", "AVG MS", "FAILURES")
	fmt.Printf("  %-16s %7s %10s %9s\n", "────", "─────", "──────", "────────")
	for _, t := range tools {
		fmt.Printf("  %-16s %7d %10.0f %9d\n",
			t.ToolName, t.Count, t.AvgDurationMs, t.FailureCount)
	}
}

func renderBugStats(bugs []olap.BugCausality) {
	if len(bugs) == 0 {
		fmt.Println("No failure hotspots detected")
		return
	}
	fmt.Println("Failure hotspots (top 5)")
	fmt.Printf("  %-14s %-18s %8s %s\n", "BEAD", "FAILURE CLASS", "ATTEMPTS", "LAST FAILURE")
	fmt.Printf("  %-14s %-18s %8s %s\n", "────", "─────────────", "────────", "────────────")
	for _, b := range bugs {
		fmt.Printf("  %-14s %-18s %8d %s\n",
			b.BeadID, b.FailureClass, b.AttemptCount, b.LastFailure.Format("2006-01-02 15:04"))
	}
}

// beadSummary holds bead-scoped metrics from DuckDB.
type beadSummary struct {
	BeadID       string  `json:"bead_id"`
	TotalRuns    int     `json:"total_runs"`
	Successes    int     `json:"successes"`
	Failures     int     `json:"failures"`
	SuccessRate  float64 `json:"success_rate"`
	AvgCostUSD   float64 `json:"avg_cost_usd"`
	AvgDurationS float64 `json:"avg_duration_s"`
	TotalCostUSD float64 `json:"total_cost_usd"`
}

// renderBeadMetrics queries agent_runs_olap for a specific bead and renders the result.
func renderBeadMetrics(adb *olap.DB, beadID string, jsonOut bool) error {
	ctx := context.Background()

	var successRate, avgCost, avgDur, totalCost sql.NullFloat64
	s := beadSummary{BeadID: beadID}

	err := adb.SqlDB().QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN result = 'success' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN result NOT IN ('success', 'skipped') THEN 1 ELSE 0 END), 0),
			ROUND(100.0 * SUM(CASE WHEN result = 'success' THEN 1 ELSE 0 END) / NULLIF(COUNT(*), 0), 1),
			COALESCE(AVG(cost_usd), 0),
			COALESCE(AVG(duration_seconds), 0),
			COALESCE(SUM(cost_usd), 0)
		FROM agent_runs_olap
		WHERE bead_id = ?
	`, beadID).Scan(&s.TotalRuns, &s.Successes, &s.Failures, &successRate, &avgCost, &avgDur, &totalCost)
	if err != nil {
		return fmt.Errorf("metrics bead: %w", err)
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

	if jsonOut {
		return metricsJSONEncode(s)
	}

	fmt.Printf("Bead: %s\n", beadID)
	fmt.Printf("  Runs: %d total, %d succeeded, %d failed (%.1f%% success)\n",
		s.TotalRuns, s.Successes, s.Failures, s.SuccessRate)
	fmt.Printf("  Avg cost: $%.4f   Avg duration: %.0fs   Total cost: $%.2f\n",
		s.AvgCostUSD, s.AvgDurationS, s.TotalCostUSD)

	if s.TotalRuns == 0 {
		fmt.Printf("\n  (no runs found for %s)\n", beadID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Formula comparison (already uses DuckDB — kept as-is)
// ---------------------------------------------------------------------------

func appendFormulaComparison() {
	tc, err := config.ActiveTowerConfig()
	if err != nil {
		return
	}
	adb, err := olap.Open(tc.OLAPPath())
	if err != nil {
		return
	}
	defer adb.Close()

	since := time.Now().AddDate(0, -3, 0) // 90-day window
	rows, err := adb.QueryFormulaPerformance(since)
	if err != nil || len(rows) == 0 {
		return
	}
	renderFormulaComparison(os.Stdout, rows)
}

func renderFormulaComparison(w io.Writer, rows []olap.FormulaStats) {
	fmt.Fprintln(w, "\nFormula Performance (last 90 days)")
	fmt.Fprintf(w, "%-28s %-10s %5s  %8s  %9s  %7s  %8s\n",
		"Formula", "Version", "Runs", "Success%", "Avg Cost", "Rounds", "30d Runs")
	fmt.Fprintln(w, strings.Repeat("─", 80))
	for _, r := range rows {
		fmt.Fprintf(w, "%-28s %-10s %5d  %7.1f%%  $%7.4f  %7.1f  %8d\n",
			r.FormulaName, r.FormulaVersion, r.TotalRuns,
			r.SuccessRate, r.AvgCostUSD, r.AvgReviewRounds, r.RunsLast30d)
	}
}

// ---------------------------------------------------------------------------
// Dolt fallback rendering functions (used when DuckDB is unavailable)
// ---------------------------------------------------------------------------

// renderFailuresFallback uses the old observability package to show failures.
func renderFailuresFallback(jsonOut bool) error {
	retries, err := observability.MetricsRetry()
	if err != nil {
		return fmt.Errorf("metrics retries: %w", err)
	}
	failures, err := observability.MetricsFailureBreakdown()
	if err != nil {
		return fmt.Errorf("metrics failures: %w", err)
	}

	if jsonOut {
		out := map[string]any{
			"retries":  retries,
			"failures": failures,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if len(retries) > 0 {
		fmt.Println("Retry rates (last 30 days)")
		fmt.Printf("  %-14s %5s %7s %11s\n", "PHASE", "RUNS", "RETRIES", "MAX ATTEMPT")
		fmt.Printf("  %-14s %5s %7s %11s\n", "─────", "────", "───────", "───────────")
		for _, r := range retries {
			fmt.Printf("  %-14s %5d %7d %11d\n",
				r.Phase, r.TotalRuns, r.Retries, r.MaxAttempts)
		}
	} else {
		fmt.Println("No retries in the last 30 days")
	}

	if len(failures) > 0 {
		fmt.Println()
		fmt.Println("Failure breakdown (last 30 days)")
		fmt.Printf("  %-18s %-14s %5s\n", "FAILURE CLASS", "PHASE", "COUNT")
		fmt.Printf("  %-18s %-14s %5s\n", "─────────────", "─────", "─────")
		for _, f := range failures {
			fmt.Printf("  %-18s %-14s %5d\n", f.FailureClass, f.Phase, f.Count)
		}
	}

	return nil
}

// renderToolUsageFallback uses the old observability package to show tool usage.
func renderToolUsageFallback(jsonOut bool) error {
	usage, err := observability.MetricsToolUsage()
	if err != nil {
		return fmt.Errorf("metrics tools: %w", err)
	}
	thrashing, err := observability.MetricsThrashingDetection()
	if err != nil {
		return fmt.Errorf("metrics thrashing: %w", err)
	}

	if jsonOut {
		out := map[string]any{
			"tool_usage": usage,
			"thrashing":  thrashing,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if len(usage) > 0 {
		fmt.Println("Tool usage per phase (last 30 days)")
		fmt.Printf("  %-14s %5s %9s %9s %9s\n", "PHASE", "RUNS", "AVG READS", "AVG EDITS", "MAX READS")
		fmt.Printf("  %-14s %5s %9s %9s %9s\n", "─────", "────", "─────────", "─────────", "─────────")
		for _, u := range usage {
			fmt.Printf("  %-14s %5d %9.0f %9.0f %9d\n",
				u.Phase, u.TotalRuns, u.AvgReads, u.AvgEdits, u.MaxReads)
		}
	} else {
		fmt.Println("No tool usage data yet (tool_calls tracking starts with new runs)")
	}

	if len(thrashing) > 0 {
		fmt.Println()
		fmt.Println("Thrashing detected (>100 reads):")
		for _, t := range thrashing {
			fmt.Printf("  %s  %-14s  reads=%d edits=%d  bead=%s\n",
				t.RunID, t.Phase, t.ReadCalls, t.EditCalls, t.BeadID)
		}
	}

	return nil
}

// renderBugCausalityFallback uses the old observability package to show bug causality.
func renderBugCausalityFallback(jsonOut bool) error {
	rows, err := observability.MetricsBugCausality(30)
	if err != nil {
		return fmt.Errorf("metrics bugs: %w", err)
	}

	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}

	if len(rows) == 0 {
		fmt.Println("No bug causality data (bugs need caused-by deps to appear here)")
		return nil
	}

	fmt.Println("Top bug-producing beads (last 30 days)")
	fmt.Printf("  %-14s %5s  %s\n", "BEAD", "BUGS", "TITLE")
	fmt.Printf("  %-14s %5s  %s\n", "────", "────", "─────")
	for _, r := range rows {
		title := r.SourceTitle
		if len(title) > 50 {
			title = title[:47] + "..."
		}
		fmt.Printf("  %-14s %5d  %s\n", r.SourceBeadID, r.BugCount, title)
	}
	return nil
}

// renderTrendsFallback prints each trend series as a formatted table (Dolt fallback).
func renderTrendsFallback(r *observability.TrendResult) {
	type series struct {
		title  string
		unit   string
		data   []observability.WeekTrend
		fmtVal func(float64) string
	}

	sets := []series{
		{"Merge frequency", "Merges", r.MergeFrequency, func(v float64) string { return fmt.Sprintf("%.0f", v) }},
		{"Review friction — avg review rounds/week", "Rounds", r.ReviewFriction, func(v float64) string { return fmt.Sprintf("%.1f", v) }},
		{"Cost per merge (USD)", "Cost", r.CostPerMerge, func(v float64) string {
			if v < 1 {
				return fmt.Sprintf("$%.2f", v)
			}
			return fmt.Sprintf("$%.0f", v)
		}},
		{"Success rate (%)", "Rate", r.SuccessRate, func(v float64) string { return fmt.Sprintf("%.1f%%", v) }},
		{"Lead time P50 (hours)", "Hours", r.LeadTimeP50, func(v float64) string { return fmt.Sprintf("%.1f", v) }},
	}

	for si, s := range sets {
		if len(s.data) == 0 {
			continue
		}
		if si > 0 {
			fmt.Println()
		}
		fmt.Printf("%s — last %d weeks\n", s.title, len(s.data))
		fmt.Printf("  %-10s %10s %10s   %s\n", "Week", s.unit, "WoW", "Trend")
		fmt.Printf("  %-10s %10s %10s   %s\n", "────", "─────", "───", "─────")

		spark := trendSparkline(s.data)

		for i, w := range s.data {
			wow := "—"
			if !math.IsNaN(w.PctChange) {
				if w.PctChange >= 0 {
					wow = fmt.Sprintf("+%.1f%%", w.PctChange)
				} else {
					wow = fmt.Sprintf("%.1f%%", w.PctChange)
				}
			}
			trendCol := ""
			if i == 0 && spark != "" {
				trendCol = spark
			}
			fmt.Printf("  %-10s %10s %10s   %s\n", w.Week, s.fmtVal(w.Value), wow, trendCol)
		}
	}
}

// trendSparkline computes a sparkline over the most-recent 8 values, reversed
// so the visual reads oldest→newest (left→right).
func trendSparkline(data []observability.WeekTrend) string {
	n := len(data)
	if n == 0 {
		return ""
	}
	take := 8
	if n < take {
		take = n
	}
	vals := make([]float64, take)
	for i := 0; i < take; i++ {
		vals[take-1-i] = data[i].Value
	}
	return observability.Sparkline(vals)
}
