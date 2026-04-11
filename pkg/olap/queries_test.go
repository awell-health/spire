package olap

import (
	"context"
	"math"
	"testing"
	"time"
)

// insertTestRuns inserts a standard set of test data into agent_runs_olap.
// Returns the time used as "now" for computing relative timestamps.
func insertTestRuns(t *testing.T, db *DB) time.Time {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// Run 1: bead b1, implement phase, failure (review_reject), 3 days ago
	// Run 2: bead b1, implement phase, success, 2 days ago
	// Run 3: bead b1, seal phase, success, 2 days ago + 2h (this makes b1 a "merge")
	// Run 4: bead b2, implement phase, success (no seal → not a deploy), 1 day ago
	// Run 5: bead b3, implement phase, failure (merge_conflict), 5 days ago
	runs := []struct {
		id, bead, formula, fv, phase, model, tower, result string
		cost, dur                                          float64
		reviews, readCalls, editCalls                      int
		failClass                                          *string
		started, completed                                 time.Time
		tokens                                             int64
	}{
		{"r1", "b1", "task-default", "3", "implement", "opus", "t1", "failure",
			0.30, 60.0, 0, 8, 3, strPtr("review_reject"),
			now.Add(-3 * 24 * time.Hour), now.Add(-3*24*time.Hour + 60*time.Second), 1500},
		{"r2", "b1", "task-default", "3", "implement", "opus", "t1", "success",
			0.50, 120.0, 2, 15, 7, nil,
			now.Add(-2 * 24 * time.Hour), now.Add(-2*24*time.Hour + 120*time.Second), 2000},
		{"r3", "b1", "task-default", "3", "seal", "opus", "t1", "success",
			0.10, 30.0, 0, 2, 0, nil,
			now.Add(-2*24*time.Hour + 2*time.Hour), now.Add(-2*24*time.Hour + 2*time.Hour + 30*time.Second), 500},
		{"r4", "b2", "bug-fix", "1", "implement", "sonnet", "t1", "success",
			0.20, 45.0, 1, 10, 4, nil,
			now.Add(-1 * 24 * time.Hour), now.Add(-1*24*time.Hour + 45*time.Second), 1200},
		{"r5", "b3", "task-default", "3", "implement", "sonnet", "t1", "failure",
			0.40, 90.0, 0, 5, 2, strPtr("merge_conflict"),
			now.Add(-5 * 24 * time.Hour), now.Add(-5*24*time.Hour + 90*time.Second), 800},
	}

	for _, r := range runs {
		_, err := db.SqlDB().ExecContext(ctx, `
			INSERT INTO agent_runs_olap (
				id, bead_id, formula_name, formula_version, phase, model, tower, repo,
				result, cost_usd, duration_seconds, review_rounds,
				read_calls, edit_calls, failure_class, total_tokens,
				started_at, completed_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.id, r.bead, r.formula, r.fv, r.phase, r.model, r.tower,
			r.result, r.cost, r.dur, r.reviews,
			r.readCalls, r.editCalls, r.failClass, r.tokens,
			r.started, r.completed,
		)
		if err != nil {
			t.Fatalf("insert run %s: %v", r.id, err)
		}
	}

	return now
}

func strPtr(s string) *string { return &s }

func TestQuerySummary(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	insertTestRuns(t, db)

	since := time.Now().Add(-30 * 24 * time.Hour)
	s, err := db.QuerySummary(since)
	if err != nil {
		t.Fatalf("QuerySummary: %v", err)
	}

	if s.TotalRuns != 5 {
		t.Errorf("TotalRuns: got %d, want 5", s.TotalRuns)
	}
	if s.Successes != 3 {
		t.Errorf("Successes: got %d, want 3", s.Successes)
	}
	if s.Failures != 2 {
		t.Errorf("Failures: got %d, want 2", s.Failures)
	}
	if s.SuccessRate != 60.0 {
		t.Errorf("SuccessRate: got %.1f, want 60.0", s.SuccessRate)
	}
	// Total cost = 0.30 + 0.50 + 0.10 + 0.20 + 0.40 = 1.50
	if math.Abs(s.TotalCostUSD-1.50) > 0.01 {
		t.Errorf("TotalCostUSD: got %.2f, want 1.50", s.TotalCostUSD)
	}
	// Avg cost = 1.50 / 5 = 0.30
	if math.Abs(s.AvgCostUSD-0.30) > 0.01 {
		t.Errorf("AvgCostUSD: got %.2f, want 0.30", s.AvgCostUSD)
	}
	// Avg duration = (60+120+30+45+90)/5 = 69.0
	if math.Abs(s.AvgDurationS-69.0) > 0.1 {
		t.Errorf("AvgDurationS: got %.1f, want 69.0", s.AvgDurationS)
	}
}

func TestQuerySummaryEmpty(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	s, err := db.QuerySummary(time.Now().Add(-24 * time.Hour))
	if err != nil {
		t.Fatalf("QuerySummary empty: %v", err)
	}
	if s.TotalRuns != 0 {
		t.Errorf("TotalRuns: got %d, want 0", s.TotalRuns)
	}
}

func TestQueryModelBreakdown(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	insertTestRuns(t, db)

	since := time.Now().Add(-30 * 24 * time.Hour)
	models, err := db.QueryModelBreakdown(since)
	if err != nil {
		t.Fatalf("QueryModelBreakdown: %v", err)
	}

	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}

	// Results ordered by run_count DESC: opus (3 runs) first, sonnet (2 runs) second
	opus := models[0]
	if opus.Model != "opus" {
		t.Errorf("first model: got %s, want opus", opus.Model)
	}
	if opus.RunCount != 3 {
		t.Errorf("opus RunCount: got %d, want 3", opus.RunCount)
	}
	// opus: 2 successes out of 3 = 66.7%
	if math.Abs(opus.SuccessRate-66.7) > 0.1 {
		t.Errorf("opus SuccessRate: got %.1f, want 66.7", opus.SuccessRate)
	}

	sonnet := models[1]
	if sonnet.Model != "sonnet" {
		t.Errorf("second model: got %s, want sonnet", sonnet.Model)
	}
	if sonnet.RunCount != 2 {
		t.Errorf("sonnet RunCount: got %d, want 2", sonnet.RunCount)
	}
	// sonnet: 1 success out of 2 = 50.0%
	if math.Abs(sonnet.SuccessRate-50.0) > 0.1 {
		t.Errorf("sonnet SuccessRate: got %.1f, want 50.0", sonnet.SuccessRate)
	}
}

func TestQueryPhaseBreakdown(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	insertTestRuns(t, db)

	since := time.Now().Add(-30 * 24 * time.Hour)
	phases, err := db.QueryPhaseBreakdown(since)
	if err != nil {
		t.Fatalf("QueryPhaseBreakdown: %v", err)
	}

	if len(phases) != 2 {
		t.Fatalf("expected 2 phases, got %d", len(phases))
	}

	// implement: 4 runs (2 success, 2 failure), seal: 1 run (1 success)
	impl := phases[0]
	if impl.Phase != "implement" {
		t.Errorf("first phase: got %s, want implement", impl.Phase)
	}
	if impl.RunCount != 4 {
		t.Errorf("implement RunCount: got %d, want 4", impl.RunCount)
	}
	if math.Abs(impl.SuccessRate-50.0) > 0.1 {
		t.Errorf("implement SuccessRate: got %.1f, want 50.0", impl.SuccessRate)
	}

	seal := phases[1]
	if seal.Phase != "seal" {
		t.Errorf("second phase: got %s, want seal", seal.Phase)
	}
	if seal.RunCount != 1 {
		t.Errorf("seal RunCount: got %d, want 1", seal.RunCount)
	}
	if seal.SuccessRate != 100.0 {
		t.Errorf("seal SuccessRate: got %.1f, want 100.0", seal.SuccessRate)
	}
}

func TestQueryFailures(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	insertTestRuns(t, db)

	since := time.Now().Add(-30 * 24 * time.Hour)
	failures, err := db.QueryFailures(since)
	if err != nil {
		t.Fatalf("QueryFailures: %v", err)
	}

	if len(failures) != 2 {
		t.Fatalf("expected 2 failure classes, got %d", len(failures))
	}

	// Both have count=1 so ordering may vary; check that both exist
	classes := map[string]int{}
	for _, f := range failures {
		classes[f.FailureClass] = f.Count
		if math.Abs(f.Percentage-50.0) > 0.1 {
			t.Errorf("failure %s percentage: got %.1f, want 50.0", f.FailureClass, f.Percentage)
		}
	}
	if classes["review_reject"] != 1 {
		t.Errorf("expected review_reject count=1, got %d", classes["review_reject"])
	}
	if classes["merge_conflict"] != 1 {
		t.Errorf("expected merge_conflict count=1, got %d", classes["merge_conflict"])
	}
}

func TestQueryTrends(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	insertTestRuns(t, db)

	since := time.Now().Add(-30 * 24 * time.Hour)
	trends, err := db.QueryTrends(since)
	if err != nil {
		t.Fatalf("QueryTrends: %v", err)
	}

	if len(trends) == 0 {
		t.Fatal("expected at least 1 weekly trend, got 0")
	}

	// All 5 runs are within the last 7 days, so likely 1 or 2 weeks
	totalRuns := 0
	totalMerges := 0
	for _, tr := range trends {
		totalRuns += tr.RunCount
		totalMerges += tr.MergeCount
		if tr.SuccessRate < 0 || tr.SuccessRate > 100 {
			t.Errorf("week %v: success rate %.1f out of range", tr.WeekStart, tr.SuccessRate)
		}
	}
	if totalRuns != 5 {
		t.Errorf("total runs across weeks: got %d, want 5", totalRuns)
	}
	// b1 has a successful seal → 1 merge
	if totalMerges != 1 {
		t.Errorf("total merges across weeks: got %d, want 1", totalMerges)
	}
}

func TestQueryDORA(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	insertTestRuns(t, db)

	// Refresh materialized views so weekly_merge_stats has data
	if err := RefreshMaterializedViews(context.Background(), db); err != nil {
		t.Fatalf("RefreshMaterializedViews: %v", err)
	}

	since := time.Now().Add(-30 * 24 * time.Hour)
	dora, err := db.QueryDORA(since)
	if err != nil {
		t.Fatalf("QueryDORA: %v", err)
	}

	// DeployFrequency: 1 merge in the data → should be > 0
	if dora.DeployFrequency <= 0 {
		t.Errorf("DeployFrequency: got %.2f, want > 0", dora.DeployFrequency)
	}

	// LeadTimeSeconds: b1 lead time = time from r1.started_at to r3.completed_at > 0
	if dora.LeadTimeSeconds <= 0 {
		t.Errorf("LeadTimeSeconds: got %.2f, want > 0", dora.LeadTimeSeconds)
	}

	// ChangeFailureRate: should be between 0 and 1
	if dora.ChangeFailureRate < 0 || dora.ChangeFailureRate > 1 {
		t.Errorf("ChangeFailureRate: got %.2f, want 0-1", dora.ChangeFailureRate)
	}
	// We have failures so rate should be > 0
	if dora.ChangeFailureRate == 0 {
		t.Errorf("ChangeFailureRate: got 0, expected > 0 (there are failures)")
	}

	// MTTRSeconds: b1 had a failure (r1) then a success (r2), so MTTR > 0
	if dora.MTTRSeconds <= 0 {
		t.Errorf("MTTRSeconds: got %.2f, want > 0", dora.MTTRSeconds)
	}
}

func TestQueryDORAEmpty(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	dora, err := db.QueryDORA(time.Now().Add(-24 * time.Hour))
	if err != nil {
		t.Fatalf("QueryDORA empty: %v", err)
	}
	if dora.DeployFrequency != 0 {
		t.Errorf("DeployFrequency: got %.2f, want 0", dora.DeployFrequency)
	}
	if dora.MTTRSeconds != 0 {
		t.Errorf("MTTRSeconds: got %.2f, want 0", dora.MTTRSeconds)
	}
}

func TestQueryToolUsage(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	today := time.Now().UTC().Truncate(24 * time.Hour)

	// Insert directly into tool_usage_stats
	_, err = db.SqlDB().ExecContext(ctx, `
		INSERT INTO tool_usage_stats (date, tower, formula_name, phase, total_runs, total_read, total_edit, total_tools)
		VALUES (?, 't1', 'task-default', 'implement', 10, 100, 30, 130),
		       (?, 't1', 'bug-fix', 'implement', 5, 40, 20, 60)`,
		today, today,
	)
	if err != nil {
		t.Fatalf("insert tool_usage_stats: %v", err)
	}

	since := time.Now().Add(-7 * 24 * time.Hour)
	usage, err := db.QueryToolUsage(since)
	if err != nil {
		t.Fatalf("QueryToolUsage: %v", err)
	}

	if len(usage) != 2 {
		t.Fatalf("expected 2 tool usage rows, got %d", len(usage))
	}

	// Ordered by total_tools DESC: task-default (130) first
	if usage[0].FormulaName != "task-default" {
		t.Errorf("first formula: got %s, want task-default", usage[0].FormulaName)
	}
	if usage[0].TotalRead != 100 {
		t.Errorf("TotalRead: got %d, want 100", usage[0].TotalRead)
	}
	if usage[0].TotalEdit != 30 {
		t.Errorf("TotalEdit: got %d, want 30", usage[0].TotalEdit)
	}
	// ReadRatio = 100/130 ≈ 0.769
	if math.Abs(usage[0].ReadRatio-0.769) > 0.01 {
		t.Errorf("ReadRatio: got %.3f, want ~0.769", usage[0].ReadRatio)
	}
}

func TestQueryBugCausality(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	weekStart := now.Truncate(7 * 24 * time.Hour)

	// Insert directly into failure_hotspots
	_, err = db.SqlDB().ExecContext(ctx, `
		INSERT INTO failure_hotspots (week_start, tower, bead_id, failure_class, attempt_count, last_failure_at)
		VALUES (?, 't1', 'spi-abc', 'review_reject', 5, ?),
		       (?, 't1', 'spi-def', 'merge_conflict', 3, ?)`,
		weekStart, now.Add(-1*time.Hour),
		weekStart, now.Add(-2*time.Hour),
	)
	if err != nil {
		t.Fatalf("insert failure_hotspots: %v", err)
	}

	bugs, err := db.QueryBugCausality(10)
	if err != nil {
		t.Fatalf("QueryBugCausality: %v", err)
	}

	if len(bugs) != 2 {
		t.Fatalf("expected 2 bug causality rows, got %d", len(bugs))
	}

	// Ordered by attempt_count DESC
	if bugs[0].BeadID != "spi-abc" {
		t.Errorf("first bead: got %s, want spi-abc", bugs[0].BeadID)
	}
	if bugs[0].AttemptCount != 5 {
		t.Errorf("AttemptCount: got %d, want 5", bugs[0].AttemptCount)
	}
	if bugs[1].BeadID != "spi-def" {
		t.Errorf("second bead: got %s, want spi-def", bugs[1].BeadID)
	}
	if bugs[1].AttemptCount != 3 {
		t.Errorf("AttemptCount: got %d, want 3", bugs[1].AttemptCount)
	}
}

func TestQueryCostTrend(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	insertTestRuns(t, db)

	trend, err := db.QueryCostTrend(30)
	if err != nil {
		t.Fatalf("QueryCostTrend: %v", err)
	}

	if len(trend) == 0 {
		t.Fatal("expected at least 1 cost trend point, got 0")
	}

	// Verify total cost across all days sums to 1.50
	var totalCost float64
	var totalRuns int
	for _, p := range trend {
		totalCost += p.TotalCost
		totalRuns += p.RunCount
	}
	if math.Abs(totalCost-1.50) > 0.01 {
		t.Errorf("total cost across days: got %.2f, want 1.50", totalCost)
	}
	if totalRuns != 5 {
		t.Errorf("total runs across days: got %d, want 5", totalRuns)
	}
}

func TestQueryCostTrendEmpty(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	trend, err := db.QueryCostTrend(30)
	if err != nil {
		t.Fatalf("QueryCostTrend empty: %v", err)
	}
	if len(trend) != 0 {
		t.Errorf("expected 0 cost trend points, got %d", len(trend))
	}
}

func TestNewTablesExistAfterInit(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	for _, tbl := range []string{"tool_usage_stats", "failure_hotspots"} {
		rows, err := db.QueryContext(ctx, "SELECT COUNT(*) FROM "+tbl)
		if err != nil {
			t.Errorf("query %s: %v", tbl, err)
			continue
		}
		rows.Close()
	}
}

func TestViewRefreshPopulatesNewTables(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	insertTestRuns(t, db)

	ctx := context.Background()
	if err := RefreshMaterializedViews(ctx, db); err != nil {
		t.Fatalf("RefreshMaterializedViews: %v", err)
	}

	// tool_usage_stats should have data from the test runs
	var toolCount int
	if err := db.SqlDB().QueryRowContext(ctx, "SELECT COUNT(*) FROM tool_usage_stats").Scan(&toolCount); err != nil {
		t.Fatalf("count tool_usage_stats: %v", err)
	}
	if toolCount == 0 {
		t.Error("expected rows in tool_usage_stats after view refresh")
	}

	// failure_hotspots should have data from the failed runs
	var failCount int
	if err := db.SqlDB().QueryRowContext(ctx, "SELECT COUNT(*) FROM failure_hotspots").Scan(&failCount); err != nil {
		t.Fatalf("count failure_hotspots: %v", err)
	}
	if failCount == 0 {
		t.Error("expected rows in failure_hotspots after view refresh")
	}
}
