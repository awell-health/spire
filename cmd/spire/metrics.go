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
	Short: "Agent run metrics (--bead, --model, --phase, --dora, --trends, --lifecycle-by-type, --json)",
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
		if lt, _ := cmd.Flags().GetBool("lifecycle-by-type"); lt {
			fullArgs = append(fullArgs, "--lifecycle-by-type")
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
	metricsCmd.Flags().Bool("lifecycle-by-type", false, "Show P50/P95 bead lifecycle timings grouped by bead_type")
}

// metricsJSONEncode writes v as indented JSON to stdout.
func metricsJSONEncode(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func cmdMetrics(args []string) error {
	var (
		flagJSON            bool
		flagBead            string
		flagModel           bool
		flagPhase           bool
		flagDORA            bool
		flagTrends          bool
		flagFailures        bool
		flagTools           bool
		flagBugs            bool
		flagFallback        bool
		flagLifecycleByType bool
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
		case "--lifecycle-by-type":
			flagLifecycleByType = true
		case "--bead":
			if i+1 >= len(args) {
				return fmt.Errorf("--bead requires a value")
			}
			i++
			flagBead = args[i]
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire metrics [--bead <id>] [--model] [--phase] [--dora] [--trends] [--failures] [--tools] [--bugs] [--lifecycle-by-type] [--fallback] [--json]", args[i])
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

	if flagLifecycleByType {
		if adb == nil {
			return fmt.Errorf("metrics --lifecycle-by-type requires the DuckDB OLAP database; no Dolt fallback available")
		}
		rows, err := adb.QueryLifecycleByType(since)
		if err != nil {
			return fmt.Errorf("metrics lifecycle-by-type: %w", err)
		}
		if flagJSON {
			return metricsJSONEncode(rows)
		}
		renderLifecycleByType(rows)
		return nil
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
	BeadID       string                          `json:"bead_id"`
	TotalRuns    int                             `json:"total_runs"`
	Successes    int                             `json:"successes"`
	Failures     int                             `json:"failures"`
	SuccessRate  float64                         `json:"success_rate"`
	AvgCostUSD   float64                         `json:"avg_cost_usd"`
	AvgDurationS float64                         `json:"avg_duration_s"`
	TotalCostUSD float64                         `json:"total_cost_usd"`
	Lifecycle    *olap.BeadLifecycleIntervals    `json:"lifecycle,omitempty"`
	ReviewFix    *olap.ReviewFixCounts           `json:"review_fix_counts,omitempty"`
	Children     []olap.BeadLifecycleIntervals   `json:"children,omitempty"`
}

// beadMetricsReader is the narrow read surface `spire metrics --bead`
// consumes. Defined as an interface so the CLI test layer can inject a
// fake and exercise the bead rendering path without opening
// analytics.db (the layering rule pinned in spi-9h5rt). The default
// implementation (*duckdbBeadReader) wraps *olap.DB and lives below.
type beadMetricsReader interface {
	BeadRunSummary(beadID string) (beadRunStats, error)
	QueryLifecycleForBead(beadID string) (*olap.BeadLifecycleIntervals, error)
	QueryReviewFixCounts(beadID string) (*olap.ReviewFixCounts, error)
	QueryChildLifecycle(parentID string) ([]olap.BeadLifecycleIntervals, error)
}

// beadRunStats holds the derived per-bead run aggregation the bead
// summary renders. Factored out of beadSummary so the reader seam can
// return it without pulling in the lifecycle blocks.
type beadRunStats struct {
	TotalRuns    int
	Successes    int
	Failures     int
	SuccessRate  float64
	AvgCostUSD   float64
	AvgDurationS float64
	TotalCostUSD float64
}

// duckdbBeadReader adapts *olap.DB to the beadMetricsReader seam.
// Keeps the raw agent_runs_olap SQL co-located with the other *olap.DB
// methods the command uses; a future refactor can promote this to a
// first-class MetricsReader method on the storage package itself.
type duckdbBeadReader struct{ db *olap.DB }

func (r duckdbBeadReader) BeadRunSummary(beadID string) (beadRunStats, error) {
	ctx := context.Background()
	var stats beadRunStats
	var successRate, avgCost, avgDur, totalCost sql.NullFloat64
	err := r.db.SqlDB().QueryRowContext(ctx, `
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
	`, beadID).Scan(&stats.TotalRuns, &stats.Successes, &stats.Failures,
		&successRate, &avgCost, &avgDur, &totalCost)
	if err != nil {
		return stats, err
	}
	if successRate.Valid {
		stats.SuccessRate = successRate.Float64
	}
	if avgCost.Valid {
		stats.AvgCostUSD = avgCost.Float64
	}
	if avgDur.Valid {
		stats.AvgDurationS = avgDur.Float64
	}
	if totalCost.Valid {
		stats.TotalCostUSD = totalCost.Float64
	}
	return stats, nil
}

func (r duckdbBeadReader) QueryLifecycleForBead(beadID string) (*olap.BeadLifecycleIntervals, error) {
	return r.db.QueryLifecycleForBead(beadID)
}

func (r duckdbBeadReader) QueryReviewFixCounts(beadID string) (*olap.ReviewFixCounts, error) {
	return r.db.QueryReviewFixCounts(beadID)
}

func (r duckdbBeadReader) QueryChildLifecycle(parentID string) ([]olap.BeadLifecycleIntervals, error) {
	return r.db.QueryChildLifecycle(parentID)
}

// renderBeadMetrics queries agent_runs_olap for a specific bead and renders
// the result, then overlays lifecycle timings, review/fix counts, and a child
// drill-down (workflow step / attempt / sub-task timings). Any of those
// secondary blocks may be empty if no lifecycle row exists yet for the bead.
func renderBeadMetrics(adb *olap.DB, beadID string, jsonOut bool) error {
	return renderBeadMetricsFromReader(duckdbBeadReader{db: adb}, beadID, jsonOut)
}

// renderBeadMetricsFromReader is the reader-interface entry point.
// Extracted so CLI regression tests can exercise the rendering path
// against a fake reader without a DuckDB dependency.
func renderBeadMetricsFromReader(r beadMetricsReader, beadID string, jsonOut bool) error {
	s := beadSummary{BeadID: beadID}

	stats, err := r.BeadRunSummary(beadID)
	if err != nil {
		return fmt.Errorf("metrics bead: %w", err)
	}
	s.TotalRuns = stats.TotalRuns
	s.Successes = stats.Successes
	s.Failures = stats.Failures
	s.SuccessRate = stats.SuccessRate
	s.AvgCostUSD = stats.AvgCostUSD
	s.AvgDurationS = stats.AvgDurationS
	s.TotalCostUSD = stats.TotalCostUSD

	// Fetch lifecycle timings, review/fix counts, and child breakdown.
	// Errors are non-fatal — the bead summary still renders if the lifecycle
	// sidecar hasn't been populated yet (fresh tower pre-backfill).
	if lc, err := r.QueryLifecycleForBead(beadID); err == nil {
		s.Lifecycle = lc
	}
	if rf, err := r.QueryReviewFixCounts(beadID); err == nil {
		s.ReviewFix = rf
	}
	if kids, err := r.QueryChildLifecycle(beadID); err == nil {
		s.Children = kids
	}

	if jsonOut {
		return metricsJSONEncode(s)
	}

	fmt.Printf("Bead: %s\n", beadID)
	fmt.Printf("  Runs: %d total, %d succeeded, %d failed (%.1f%% success)\n",
		s.TotalRuns, s.Successes, s.Failures, s.SuccessRate)
	fmt.Printf("  Avg cost: $%.4f   Avg duration: %.0fs   Total cost: $%.2f\n",
		s.AvgCostUSD, s.AvgDurationS, s.TotalCostUSD)

	renderBeadLifecycleBlock(s.Lifecycle)
	renderReviewFixBlock(s.ReviewFix)
	renderChildLifecycleBlock(s.Children)

	if s.TotalRuns == 0 && s.Lifecycle == nil {
		fmt.Printf("\n  (no runs or lifecycle data found for %s)\n", beadID)
	}
	return nil
}

// renderBeadLifecycleBlock prints the canonical lifecycle timings for a bead.
// Each interval is shown as "—" when unknown (pre-feature bead or in-flight)
// so NULL is distinguishable from a genuine 0s duration.
func renderBeadLifecycleBlock(lc *olap.BeadLifecycleIntervals) {
	if lc == nil {
		return
	}
	fmt.Println()
	fmt.Println("  Lifecycle:")
	if lc.BeadType != "" {
		fmt.Printf("    Type:              %s\n", lc.BeadType)
	}
	fmt.Printf("    Filed:             %s\n", fmtTime(lc.FiledAt))
	fmt.Printf("    Ready:             %s\n", fmtTime(lc.ReadyAt))
	fmt.Printf("    Started:           %s\n", fmtTime(lc.StartedAt))
	fmt.Printf("    Closed:            %s\n", fmtTime(lc.ClosedAt))
	fmt.Printf("    Filed → closed:    %s\n", fmtSecondsPtr(lc.FiledToClosedSeconds))
	fmt.Printf("    Ready → closed:    %s\n", fmtSecondsPtr(lc.ReadyToClosedSeconds))
	fmt.Printf("    Started → closed:  %s   (execution time)\n", fmtSecondsPtr(lc.StartedToClosedSeconds))
	fmt.Printf("    Queue (ready→start): %s\n", fmtSecondsPtr(lc.QueueSeconds))
}

// renderReviewFixBlock prints review / fix / arbiter counts derived from
// agent_runs_olap. Skipped when all counters are zero — a bead with no
// review activity doesn't deserve an empty block.
func renderReviewFixBlock(rf *olap.ReviewFixCounts) {
	if rf == nil {
		return
	}
	if rf.ReviewCount == 0 && rf.FixCount == 0 && rf.ArbiterCount == 0 && rf.MaxReviewRounds == 0 {
		return
	}
	fmt.Println()
	fmt.Println("  Review dynamics:")
	fmt.Printf("    Sage reviews:      %d\n", rf.ReviewCount)
	fmt.Printf("    Fix loops:         %d\n", rf.FixCount)
	fmt.Printf("    Arbiter rounds:    %d\n", rf.ArbiterCount)
	if rf.MaxReviewRounds > 0 {
		fmt.Printf("    Max review round:  %d\n", rf.MaxReviewRounds)
	}
}

// renderChildLifecycleBlock prints the workflow step / attempt / sub-task
// breakdown for a parent bead. Drives the "where did the time go?" drill-down.
func renderChildLifecycleBlock(kids []olap.BeadLifecycleIntervals) {
	if len(kids) == 0 {
		return
	}
	fmt.Println()
	fmt.Println("  Child timings:")
	fmt.Printf("    %-24s %-10s %10s %10s %10s %10s\n",
		"BEAD", "TYPE", "F→C", "R→C", "S→C", "QUEUE")
	fmt.Printf("    %-24s %-10s %10s %10s %10s %10s\n",
		"────", "────", "───", "───", "───", "─────")
	for _, k := range kids {
		fmt.Printf("    %-24s %-10s %10s %10s %10s %10s\n",
			truncate(k.BeadID, 24),
			truncate(k.BeadType, 10),
			fmtSecondsPtr(k.FiledToClosedSeconds),
			fmtSecondsPtr(k.ReadyToClosedSeconds),
			fmtSecondsPtr(k.StartedToClosedSeconds),
			fmtSecondsPtr(k.QueueSeconds),
		)
	}
}

// renderLifecycleByType prints the per-bead-type P50/P95 lifecycle rollup.
func renderLifecycleByType(rows []olap.LifecycleByType) {
	if len(rows) == 0 {
		fmt.Println("(no closed beads with lifecycle data in the last 90 days)")
		return
	}
	fmt.Println("Bead lifecycle by type (last 90 days, closed beads only)")
	fmt.Println()
	fmt.Printf("  %-10s %5s  %9s %9s  %9s %9s  %9s %9s  %8s %8s\n",
		"TYPE", "CNT",
		"F→C P50", "F→C P95",
		"R→C P50", "R→C P95",
		"S→C P50", "S→C P95",
		"Q P50", "Q P95")
	fmt.Printf("  %-10s %5s  %9s %9s  %9s %9s  %9s %9s  %8s %8s\n",
		"────", "───",
		"───────", "───────",
		"───────", "───────",
		"───────", "───────",
		"─────", "─────")
	for _, r := range rows {
		fmt.Printf("  %-10s %5d  %9s %9s  %9s %9s  %9s %9s  %8s %8s\n",
			truncate(r.BeadType, 10), r.Count,
			fmtSeconds(r.FiledToClosedP50), fmtSeconds(r.FiledToClosedP95),
			fmtSeconds(r.ReadyToClosedP50), fmtSeconds(r.ReadyToClosedP95),
			fmtSeconds(r.StartedToClosedP50), fmtSeconds(r.StartedToClosedP95),
			fmtSeconds(r.QueueP50), fmtSeconds(r.QueueP95),
		)
	}
}

// fmtTime renders a zero-value time as "—" so missing stamps are visually
// distinct from epoch zero.
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

// fmtSecondsPtr formats a nullable duration in seconds. Nil renders as "—"
// so pre-feature beads and in-flight beads don't misreport as 0s.
func fmtSecondsPtr(s *float64) string {
	if s == nil {
		return "—"
	}
	return fmtSeconds(*s)
}

// fmtSeconds formats a duration in seconds using the most-significant unit.
// Small (<120s) values stay as seconds; minutes/hours for larger.
func fmtSeconds(sec float64) string {
	if sec < 0 {
		return "—"
	}
	if sec < 120 {
		return fmt.Sprintf("%.0fs", sec)
	}
	if sec < 7200 {
		return fmt.Sprintf("%.1fm", sec/60)
	}
	return fmt.Sprintf("%.2fh", sec/3600)
}

// truncate shortens s to n runes with ellipsis when longer, else pads with
// spaces. Used for fixed-width columns in lifecycle rendering.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
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
