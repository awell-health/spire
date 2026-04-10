package main

import (
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
		case "--bead":
			if i+1 >= len(args) {
				return fmt.Errorf("--bead requires a value")
			}
			i++
			flagBead = args[i]
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire metrics [--bead <id>] [--model] [--phase] [--dora] [--trends] [--failures] [--tools] [--bugs] [--json]", args[i])
		}
	}

	if flagTrends {
		result, err := observability.MetricsTrends(12)
		if err != nil {
			return fmt.Errorf("metrics: %w", err)
		}
		if flagJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(result)
		}
		renderTrends(result)
		return nil
	}

	if flagDORA {
		return observability.MetricsDORA(observability.DORAOpts{
			JSONOut:   flagJSON,
			BeadID:    flagBead,
			ShowModel: flagModel,
			ShowPhase: flagPhase,
		})
	}
	if flagBead != "" {
		if err := observability.MetricsBead(flagBead, flagJSON); err != nil {
			return err
		}
		// Show per-step duration breakdown for bead.
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
		return renderFailures(flagJSON)
	}
	if flagTools {
		return renderToolUsage(flagJSON)
	}
	if flagBugs {
		return renderBugCausality(flagJSON)
	}
	if flagModel {
		return observability.MetricsModel(flagJSON)
	}
	if flagPhase {
		return observability.MetricsPhase(flagJSON)
	}
	if err := observability.MetricsSummary(flagJSON); err != nil {
		return err
	}
	appendFormulaComparison()
	return nil
}

// renderFailures shows failure breakdown and retry rates.
func renderFailures(jsonOut bool) error {
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

// renderToolUsage shows tool call statistics per phase.
func renderToolUsage(jsonOut bool) error {
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

// renderBugCausality shows top beads that produced bugs.
func renderBugCausality(jsonOut bool) error {
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

// renderTrends prints each trend series as a formatted table.
func renderTrends(r *observability.TrendResult) {
	type series struct {
		title    string
		unit     string
		data     []observability.WeekTrend
		fmtVal   func(float64) string
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

		// Build sparkline from oldest-to-newest (last 8 values).
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
	// data is most-recent-first; take the first `take` entries and reverse.
	vals := make([]float64, take)
	for i := 0; i < take; i++ {
		vals[take-1-i] = data[i].Value
	}
	return observability.Sparkline(vals)
}
