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
}

func cmdMetrics(args []string) error {
	var (
		flagJSON   bool
		flagBead   string
		flagModel  bool
		flagPhase  bool
		flagDORA   bool
		flagTrends bool
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
		case "--bead":
			if i+1 >= len(args) {
				return fmt.Errorf("--bead requires a value")
			}
			i++
			flagBead = args[i]
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire metrics [--bead <id>] [--model] [--phase] [--dora] [--trends] [--json]", args[i])
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
		return observability.MetricsBead(flagBead, flagJSON)
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
