//go:build cgo

package duckdb

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/olap"
)

// insertTestRuns inserts a standard set of test data into agent_runs_olap.
// Returns the time used as "now" for computing relative timestamps.
func insertTestRuns(t *testing.T, db *DB) time.Time {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// Run 1: bead b1, implement phase, failure (review_reject), 3 days ago
	// Run 2: bead b1, implement phase, success, 2 days ago
	// Run 3: bead b1, sage-review phase, success (sage approval → b1 is a "merge")
	// Run 4: bead b2, implement phase, success (no review → not a deploy), 1 day ago
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
		{"r3", "b1", "task-default", "3", "sage-review", "opus", "t1", "success",
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

	// implement: 4 runs (2 success, 2 failure), sage-review: 1 run (1 success)
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

	sageReview := phases[1]
	if sageReview.Phase != "sage-review" {
		t.Errorf("second phase: got %s, want sage-review", sageReview.Phase)
	}
	if sageReview.RunCount != 1 {
		t.Errorf("sage-review RunCount: got %d, want 1", sageReview.RunCount)
	}
	if sageReview.SuccessRate != 100.0 {
		t.Errorf("sage-review SuccessRate: got %.1f, want 100.0", sageReview.SuccessRate)
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
	// b1 has a successful sage-review → 1 merge
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

	// LeadTimeSeconds: b1 lead time = time from r1.started_at to r3.completed_at (sage-review) > 0
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
	var totalPrompt, totalCompletion int64
	for _, p := range trend {
		totalCost += p.TotalCost
		totalRuns += p.RunCount
		totalPrompt += p.PromptTokens
		totalCompletion += p.CompletionTokens
	}
	if math.Abs(totalCost-1.50) > 0.01 {
		t.Errorf("total cost across days: got %.2f, want 1.50", totalCost)
	}
	if totalRuns != 5 {
		t.Errorf("total runs across days: got %d, want 5", totalRuns)
	}
	// Test data sets total_tokens but not prompt_tokens/completion_tokens,
	// so the query's COALESCE(SUM(prompt_tokens), 0) returns 0 for both.
	if totalPrompt+totalCompletion != 0 {
		t.Errorf("token sums should be 0 (test data has no prompt/completion tokens): prompt=%d, completion=%d", totalPrompt, totalCompletion)
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

// insertTestToolEvents inserts tool events for testing query functions.
func insertTestToolEvents(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	events := []struct {
		sessionID, beadID, agentName, step, toolName, tower string
		durationMs                                          int
		success                                             bool
		ts                                                  time.Time
	}{
		{"s1", "spi-abc", "apprentice-0", "implement", "Read", "t1", 50, true, now.Add(-time.Hour)},
		{"s1", "spi-abc", "apprentice-0", "implement", "Read", "t1", 30, true, now.Add(-59 * time.Minute)},
		{"s1", "spi-abc", "apprentice-0", "implement", "Edit", "t1", 120, true, now.Add(-58 * time.Minute)},
		{"s1", "spi-abc", "apprentice-0", "implement", "Bash", "t1", 500, false, now.Add(-57 * time.Minute)},
		{"s2", "spi-abc", "sage-0", "review", "Read", "t1", 40, true, now.Add(-30 * time.Minute)},
		{"s2", "spi-abc", "sage-0", "review", "Grep", "t1", 80, true, now.Add(-29 * time.Minute)},
		{"s3", "spi-def", "apprentice-1", "implement", "Read", "t1", 60, true, now.Add(-10 * time.Minute)},
	}

	for _, e := range events {
		_, err := db.SqlDB().ExecContext(ctx, `
			INSERT INTO tool_events (session_id, bead_id, agent_name, step, tool_name, duration_ms, success, timestamp, tower)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			e.sessionID, e.beadID, e.agentName, e.step, e.toolName, e.durationMs, e.success, e.ts, e.tower,
		)
		if err != nil {
			t.Fatalf("insert tool event: %v", err)
		}
	}
}

func TestQueryToolEvents(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	insertTestToolEvents(t, db)

	since := time.Now().Add(-2 * time.Hour)
	stats, err := db.QueryToolEvents(since)
	if err != nil {
		t.Fatalf("QueryToolEvents: %v", err)
	}

	if len(stats) == 0 {
		t.Fatal("expected tool event stats, got 0")
	}

	// Read should be most frequent (4 events)
	if stats[0].ToolName != "Read" {
		t.Errorf("expected first tool to be Read (most frequent), got %s", stats[0].ToolName)
	}
	if stats[0].Count != 4 {
		t.Errorf("Read count: got %d, want 4", stats[0].Count)
	}
	if stats[0].FailureCount != 0 {
		t.Errorf("Read failure count: got %d, want 0", stats[0].FailureCount)
	}

	// Find Bash — it should have 1 failure
	var bashFound bool
	for _, s := range stats {
		if s.ToolName == "Bash" {
			bashFound = true
			if s.Count != 1 {
				t.Errorf("Bash count: got %d, want 1", s.Count)
			}
			if s.FailureCount != 1 {
				t.Errorf("Bash failure count: got %d, want 1", s.FailureCount)
			}
		}
	}
	if !bashFound {
		t.Error("expected Bash in results")
	}
}

func TestQueryToolEventsEmpty(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	stats, err := db.QueryToolEvents(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("QueryToolEvents empty: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("expected 0 stats, got %d", len(stats))
	}
}

func TestQueryToolEventsByBead(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	insertTestToolEvents(t, db)

	stats, err := db.QueryToolEventsByBead("spi-abc")
	if err != nil {
		t.Fatalf("QueryToolEventsByBead: %v", err)
	}

	if len(stats) == 0 {
		t.Fatal("expected stats for spi-abc, got 0")
	}

	// spi-abc has 6 events: Read(3), Edit(1), Bash(1), Grep(1)
	totalCount := 0
	for _, s := range stats {
		totalCount += s.Count
	}
	if totalCount != 6 {
		t.Errorf("total events for spi-abc: got %d, want 6", totalCount)
	}

	// spi-def should only have 1 event
	defStats, err := db.QueryToolEventsByBead("spi-def")
	if err != nil {
		t.Fatalf("QueryToolEventsByBead(spi-def): %v", err)
	}
	if len(defStats) != 1 {
		t.Errorf("expected 1 tool stat for spi-def, got %d", len(defStats))
	}

	// Nonexistent bead should return empty
	emptyStats, err := db.QueryToolEventsByBead("spi-nonexistent")
	if err != nil {
		t.Fatalf("QueryToolEventsByBead(nonexistent): %v", err)
	}
	if len(emptyStats) != 0 {
		t.Errorf("expected 0 stats for nonexistent bead, got %d", len(emptyStats))
	}
}

func TestQueryToolEventsByStep(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	insertTestToolEvents(t, db)

	steps, err := db.QueryToolEventsByStep("spi-abc")
	if err != nil {
		t.Fatalf("QueryToolEventsByStep: %v", err)
	}

	if len(steps) != 2 {
		t.Fatalf("expected 2 steps (implement, review), got %d", len(steps))
	}

	// Verify step grouping and ordering
	stepNames := map[string]int{}
	for _, s := range steps {
		stepNames[s.Step] = len(s.Tools)
	}

	// implement step: Read(2), Edit(1), Bash(1) = 3 distinct tools
	if stepNames["implement"] != 3 {
		t.Errorf("implement step: expected 3 tools, got %d", stepNames["implement"])
	}
	// review step: Read(1), Grep(1) = 2 distinct tools
	if stepNames["review"] != 2 {
		t.Errorf("review step: expected 2 tools, got %d", stepNames["review"])
	}

	// Within each step, tools should be ordered by count DESC
	for _, s := range steps {
		if s.Step == "implement" && len(s.Tools) > 0 {
			if s.Tools[0].ToolName != "Read" {
				t.Errorf("implement first tool: got %s, want Read (highest count)", s.Tools[0].ToolName)
			}
			if s.Tools[0].Count != 2 {
				t.Errorf("implement Read count: got %d, want 2", s.Tools[0].Count)
			}
		}
	}

	// Nonexistent bead returns empty
	emptySteps, err := db.QueryToolEventsByStep("spi-nonexistent")
	if err != nil {
		t.Fatalf("QueryToolEventsByStep(nonexistent): %v", err)
	}
	if len(emptySteps) != 0 {
		t.Errorf("expected 0 steps for nonexistent bead, got %d", len(emptySteps))
	}
}

// --- QueryToolSpansByBead ---

func TestQueryToolSpansByBead(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// Insert test spans for two beads.
	spans := []struct {
		traceID, spanID, parentID, beadID, spanName, kind string
		durationMs                                        int
		success                                           bool
		startTime, endTime                                time.Time
	}{
		{"t1", "s1", "", "spi-spans", "interaction", "interaction", 500, true, now, now.Add(500 * time.Millisecond)},
		{"t1", "s2", "s1", "spi-spans", "Read", "tool", 50, true, now.Add(10 * time.Millisecond), now.Add(60 * time.Millisecond)},
		{"t1", "s3", "s1", "spi-spans", "Bash", "tool", 100, false, now.Add(100 * time.Millisecond), now.Add(200 * time.Millisecond)},
		{"t2", "s4", "", "spi-other", "Edit", "tool", 80, true, now.Add(time.Second), now.Add(time.Second + 80*time.Millisecond)},
	}

	for _, s := range spans {
		_, err := db.SqlDB().ExecContext(ctx, `
			INSERT INTO tool_spans (trace_id, span_id, parent_span_id, session_id, bead_id,
				agent_name, step, span_name, kind, duration_ms, success, start_time, end_time, tower, attributes)
			VALUES (?, ?, ?, 'sess', ?, 'agent', 'implement', ?, ?, ?, ?, ?, ?, 'tower', '{}')`,
			s.traceID, s.spanID, s.parentID, s.beadID, s.spanName, s.kind,
			s.durationMs, s.success, s.startTime, s.endTime,
		)
		if err != nil {
			t.Fatalf("insert span %s: %v", s.spanID, err)
		}
	}

	// Query spans for spi-spans.
	results, err := db.QueryToolSpansByBead("spi-spans")
	if err != nil {
		t.Fatalf("QueryToolSpansByBead: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 spans for spi-spans, got %d", len(results))
	}

	// Should be ordered by start_time ASC.
	if results[0].SpanName != "interaction" {
		t.Errorf("first span = %q, want interaction", results[0].SpanName)
	}
	if results[1].SpanName != "Read" {
		t.Errorf("second span = %q, want Read", results[1].SpanName)
	}
	if results[2].SpanName != "Bash" {
		t.Errorf("third span = %q, want Bash", results[2].SpanName)
	}

	// Verify field values.
	if results[0].ParentSpanID != "" {
		t.Errorf("interaction parent = %q, want empty", results[0].ParentSpanID)
	}
	if results[1].ParentSpanID != "s1" {
		t.Errorf("Read parent = %q, want s1", results[1].ParentSpanID)
	}
	if results[2].Success {
		t.Error("Bash should have success=false")
	}
	if results[2].DurationMs != 100 {
		t.Errorf("Bash duration = %d, want 100", results[2].DurationMs)
	}
}

func TestQueryToolSpansByBead_Empty(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	results, err := db.QueryToolSpansByBead("spi-nonexistent")
	if err != nil {
		t.Fatalf("QueryToolSpansByBead: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 spans, got %d", len(results))
	}
}

// --- QueryAPIEventsByBead ---

func TestQueryAPIEventsByBead(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	events := []struct {
		sessionID, beadID, model, provider string
		durationMs                         int
		inputTokens, outputTokens          int64
		costUSD                            float64
	}{
		{"s1", "spi-api", "claude-opus-4-6", "claude", 1500, 5000, 2000, 0.12},
		{"s1", "spi-api", "claude-opus-4-6", "claude", 2000, 3000, 1000, 0.08},
		{"s2", "spi-api", "claude-sonnet-4-6", "claude", 800, 1000, 500, 0.02},
		{"s3", "spi-other", "codex-mini", "codex", 500, 800, 300, 0.01},
	}

	for _, e := range events {
		_, err := db.SqlDB().ExecContext(ctx, `
			INSERT INTO api_events (session_id, bead_id, agent_name, step, provider, model,
				duration_ms, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
				cost_usd, timestamp, tower)
			VALUES (?, ?, 'agent', 'impl', ?, ?, ?, ?, ?, 0, 0, ?, ?, 'tower')`,
			e.sessionID, e.beadID, e.provider, e.model,
			e.durationMs, e.inputTokens, e.outputTokens, e.costUSD, now,
		)
		if err != nil {
			t.Fatalf("insert api event: %v", err)
		}
	}

	// Query API events for spi-api.
	results, err := db.QueryAPIEventsByBead("spi-api")
	if err != nil {
		t.Fatalf("QueryAPIEventsByBead: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 model groups for spi-api, got %d", len(results))
	}

	// Ordered by count DESC: opus (2 events) first, sonnet (1 event) second.
	if results[0].Model != "claude-opus-4-6" {
		t.Errorf("first model = %q, want claude-opus-4-6", results[0].Model)
	}
	if results[0].Count != 2 {
		t.Errorf("opus count = %d, want 2", results[0].Count)
	}
	if results[0].TotalInputTokens != 8000 {
		t.Errorf("opus total_input_tokens = %d, want 8000", results[0].TotalInputTokens)
	}
	if results[0].TotalOutputTokens != 3000 {
		t.Errorf("opus total_output_tokens = %d, want 3000", results[0].TotalOutputTokens)
	}
	// Total cost for opus: 0.12 + 0.08 = 0.20
	if results[0].TotalCostUSD < 0.19 || results[0].TotalCostUSD > 0.21 {
		t.Errorf("opus total_cost_usd = %f, want ~0.20", results[0].TotalCostUSD)
	}

	if results[1].Model != "claude-sonnet-4-6" {
		t.Errorf("second model = %q, want claude-sonnet-4-6", results[1].Model)
	}
	if results[1].Count != 1 {
		t.Errorf("sonnet count = %d, want 1", results[1].Count)
	}
}

func TestQueryAPIEventsByBead_Empty(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	results, err := db.QueryAPIEventsByBead("spi-nonexistent")
	if err != nil {
		t.Fatalf("QueryAPIEventsByBead: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

func TestQueryRateLimitEvents(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// 2 rate-limit events within window + 1 old one outside the 24h window
	// + 1 regular api_request (must be excluded by event_type filter).
	rows := []struct {
		ts        time.Time
		eventType string
	}{
		{now.Add(-1 * time.Hour), "rate_limit"},
		{now.Add(-3 * time.Hour), "rate_limit"},
		{now.Add(-48 * time.Hour), "rate_limit"}, // outside 24h window
		{now.Add(-30 * time.Minute), "api_request"},
	}
	for i, r := range rows {
		if _, err := db.SqlDB().ExecContext(ctx, `
			INSERT INTO api_events (session_id, bead_id, agent_name, step, provider, model,
				duration_ms, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
				cost_usd, timestamp, tower, event_type, retry_count)
			VALUES (?, 'spi-rl', 'agent', 'impl', 'claude', 'claude-opus-4-7',
				0, 0, 0, 0, 0, 0, ?, 'tower', ?, 0)`,
			"s"+string(rune('a'+i)), r.ts, r.eventType,
		); err != nil {
			t.Fatalf("insert api event: %v", err)
		}
	}

	buckets, err := db.QueryRateLimitEvents(24 * time.Hour)
	if err != nil {
		t.Fatalf("QueryRateLimitEvents: %v", err)
	}

	total := 0
	for _, b := range buckets {
		total += b.Count
	}
	if total != 2 {
		t.Errorf("expected 2 rate_limit rows in 24h window, got %d (buckets=%+v)", total, buckets)
	}
}

func TestQueryRateLimitEvents_Empty(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	buckets, err := db.QueryRateLimitEvents(24 * time.Hour)
	if err != nil {
		t.Fatalf("QueryRateLimitEvents: %v", err)
	}
	if len(buckets) != 0 {
		t.Errorf("expected 0 buckets, got %d", len(buckets))
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

// TestViewRefreshDORA verifies that the weekly_merge_stats view correctly
// detects merges from sage-review and review (arbiter) phases.
func TestViewRefreshDORA(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// Insert 10 runs across 3 beads with realistic phase names:
	//   bead-a: implement(success) + sage-review(approve) → is a deploy
	//   bead-b: implement(success) + sage-review(approve) → is a deploy
	//   bead-c: implement(success) + sage-review(request_changes) → NOT a deploy
	runs := []struct {
		id, bead, phase, result string
		started, completed      time.Time
	}{
		// bead-a: deployed
		{"d1", "test-a001", "implement", "success", now.Add(-4 * 24 * time.Hour), now.Add(-4*24*time.Hour + time.Hour)},
		{"d2", "test-a001", "sage-review", "approve", now.Add(-4*24*time.Hour + 2*time.Hour), now.Add(-4*24*time.Hour + 3*time.Hour)},
		// bead-b: deployed (via arbiter path)
		{"d3", "test-a002", "implement", "success", now.Add(-3 * 24 * time.Hour), now.Add(-3*24*time.Hour + time.Hour)},
		{"d4", "test-a002", "sage-review", "request_changes", now.Add(-3*24*time.Hour + 2*time.Hour), now.Add(-3*24*time.Hour + 3*time.Hour)},
		{"d5", "test-a002", "review", "success", now.Add(-3*24*time.Hour + 4*time.Hour), now.Add(-3*24*time.Hour + 5*time.Hour)},
		// bead-c: NOT deployed (review rejected, no approval)
		{"d6", "test-a003", "implement", "success", now.Add(-2 * 24 * time.Hour), now.Add(-2*24*time.Hour + time.Hour)},
		{"d7", "test-a003", "sage-review", "request_changes", now.Add(-2*24*time.Hour + 2*time.Hour), now.Add(-2*24*time.Hour + 3*time.Hour)},
		// Additional successful implementations to pad data
		{"d8", "test-a004", "implement", "success", now.Add(-1 * 24 * time.Hour), now.Add(-1*24*time.Hour + time.Hour)},
		{"d9", "test-a005", "implement", "error", now.Add(-6 * time.Hour), now.Add(-5 * time.Hour)},
		{"d10", "test-a005", "implement", "success", now.Add(-4 * time.Hour), now.Add(-3 * time.Hour)},
	}

	for _, r := range runs {
		_, err := db.SqlDB().ExecContext(ctx, `
			INSERT INTO agent_runs_olap (id, bead_id, formula_name, phase, tower, repo, result, started_at, completed_at)
			VALUES (?, ?, 'task-default', ?, 'test-tower', 'test', ?, ?, ?)`,
			r.id, r.bead, r.phase, r.result, r.started, r.completed)
		if err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}

	// Refresh views
	if err := RefreshMaterializedViews(ctx, db); err != nil {
		t.Fatalf("RefreshMaterializedViews: %v", err)
	}

	// Query DORA
	since := now.Add(-30 * 24 * time.Hour)
	dora, err := db.QueryDORA(since)
	if err != nil {
		t.Fatalf("QueryDORA: %v", err)
	}

	// DeployFrequency: 2 merged beads (a and b) → should be > 0
	if dora.DeployFrequency <= 0 {
		t.Errorf("DeployFrequency: got %.2f, want > 0", dora.DeployFrequency)
	}

	// LeadTimeSeconds: should be > 0 (measured from first run to approval)
	if dora.LeadTimeSeconds <= 0 {
		t.Errorf("LeadTimeSeconds: got %.2f, want > 0", dora.LeadTimeSeconds)
	}

	// ChangeFailureRate: should be between 0 and 1 (bead-c and bead-a005 have failures)
	if dora.ChangeFailureRate < 0 || dora.ChangeFailureRate > 1 {
		t.Errorf("ChangeFailureRate: got %.2f, want 0-1", dora.ChangeFailureRate)
	}

	// Verify weekly_merge_stats directly
	var mergeCount, failureCount int
	err = db.SqlDB().QueryRowContext(ctx,
		"SELECT COALESCE(SUM(merge_count), 0), COALESCE(SUM(failure_count), 0) FROM weekly_merge_stats").
		Scan(&mergeCount, &failureCount)
	if err != nil {
		t.Fatalf("query weekly_merge_stats: %v", err)
	}
	// 2 beads deployed (test-a001 via sage approve, test-a002 via arbiter success)
	if mergeCount != 2 {
		t.Errorf("weekly_merge_stats merge_count: got %d, want 2", mergeCount)
	}
}

// TestViewRefreshFormulaStats verifies that the daily_formula_stats view
// correctly aggregates formula performance data.
func TestViewRefreshFormulaStats(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// Insert 5 runs of 'task-default' and 3 runs of 'bug-fix'
	runs := []struct {
		id, formula, result string
		cost                float64
		dur                 float64
		started             time.Time
	}{
		{"f1", "task-default", "success", 0.50, 120, now.Add(-2 * 24 * time.Hour)},
		{"f2", "task-default", "success", 0.40, 90, now.Add(-2*24*time.Hour + time.Hour)},
		{"f3", "task-default", "error", 0.30, 60, now.Add(-1 * 24 * time.Hour)},
		{"f4", "task-default", "success", 0.60, 150, now.Add(-1*24*time.Hour + time.Hour)},
		{"f5", "task-default", "success", 0.45, 100, now.Add(-6 * time.Hour)},
		{"f6", "bug-fix", "success", 0.20, 45, now.Add(-1 * 24 * time.Hour)},
		{"f7", "bug-fix", "error", 0.15, 30, now.Add(-12 * time.Hour)},
		{"f8", "bug-fix", "success", 0.25, 55, now.Add(-3 * time.Hour)},
	}

	for _, r := range runs {
		_, err := db.SqlDB().ExecContext(ctx, `
			INSERT INTO agent_runs_olap (id, bead_id, formula_name, formula_version, phase, tower, repo, result, cost_usd, duration_seconds, started_at, completed_at)
			VALUES (?, 'test-bead', ?, '3', 'implement', 'test-tower', 'test', ?, ?, ?, ?, ?)`,
			r.id, r.formula, r.result, r.cost, r.dur, r.started, r.started.Add(time.Duration(r.dur)*time.Second))
		if err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}

	// Refresh views
	if err := RefreshMaterializedViews(ctx, db); err != nil {
		t.Fatalf("RefreshMaterializedViews: %v", err)
	}

	// Query formula performance
	since := now.Add(-30 * 24 * time.Hour)
	stats, err := db.QueryFormulaPerformance(since)
	if err != nil {
		t.Fatalf("QueryFormulaPerformance: %v", err)
	}

	if len(stats) == 0 {
		t.Fatal("QueryFormulaPerformance returned no data (expected 'task-default' and 'bug-fix')")
	}

	// Find task-default stats
	var taskDefault *olap.FormulaStats
	for i := range stats {
		if stats[i].FormulaName == "task-default" {
			taskDefault = &stats[i]
			break
		}
	}
	if taskDefault == nil {
		t.Fatal("expected 'task-default' in formula performance results")
	}
	if taskDefault.TotalRuns != 5 {
		t.Errorf("task-default TotalRuns: got %d, want 5", taskDefault.TotalRuns)
	}
	if taskDefault.Successes != 4 {
		t.Errorf("task-default Successes: got %d, want 4", taskDefault.Successes)
	}

	// Verify daily_formula_stats has data
	var dailyCount int
	if err := db.SqlDB().QueryRowContext(ctx, "SELECT COUNT(*) FROM daily_formula_stats").Scan(&dailyCount); err != nil {
		t.Fatalf("count daily_formula_stats: %v", err)
	}
	if dailyCount == 0 {
		t.Error("expected rows in daily_formula_stats after view refresh")
	}
}

// TestViewRefreshFailureHotspots verifies that the failure_hotspots view
// correctly groups failures by class and excludes non-failure results.
func TestViewRefreshFailureHotspots(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// Insert runs with specific failure classes:
	//   3 timeout, 2 build_fail, 1 auth_fail, 1 unknown (empty class)
	// Also insert non-failure results that should be excluded:
	//   success, approve, no_changes, skipped
	runs := []struct {
		id, bead, result string
		failClass        *string
		started          time.Time
	}{
		{"h1", "test-h1", "timeout", strPtr("timeout"), now.Add(-3 * 24 * time.Hour)},
		{"h2", "test-h1", "timeout", strPtr("timeout"), now.Add(-2 * 24 * time.Hour)},
		{"h3", "test-h2", "timeout", strPtr("timeout"), now.Add(-1 * 24 * time.Hour)},
		{"h4", "test-h2", "error", strPtr("build_fail"), now.Add(-2 * 24 * time.Hour)},
		{"h5", "test-h3", "error", strPtr("build_fail"), now.Add(-1 * 24 * time.Hour)},
		{"h6", "test-h3", "error", strPtr("auth_fail"), now.Add(-12 * time.Hour)},
		{"h7", "test-h4", "error", strPtr(""), now.Add(-6 * time.Hour)},            // empty class → 'unknown'
		{"h8", "test-h4", "error", nil, now.Add(-5 * time.Hour)},                    // NULL class → 'unknown'
		// These should NOT appear in failure_hotspots:
		{"h9", "test-h5", "success", nil, now.Add(-4 * time.Hour)},
		{"h10", "test-h5", "approve", nil, now.Add(-3 * time.Hour)},
		{"h11", "test-h5", "no_changes", nil, now.Add(-2 * time.Hour)},
		{"h12", "test-h5", "skipped", nil, now.Add(-1 * time.Hour)},
	}

	for _, r := range runs {
		_, err := db.SqlDB().ExecContext(ctx, `
			INSERT INTO agent_runs_olap (id, bead_id, formula_name, phase, tower, repo, result, failure_class, started_at, completed_at)
			VALUES (?, ?, 'task-default', 'implement', 'test-tower', 'test', ?, ?, ?, ?)`,
			r.id, r.bead, r.result, r.failClass, r.started, r.started.Add(time.Minute))
		if err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}

	// Refresh views
	if err := RefreshMaterializedViews(ctx, db); err != nil {
		t.Fatalf("RefreshMaterializedViews: %v", err)
	}

	// Query failure hotspots
	bugs, err := db.QueryBugCausality(20)
	if err != nil {
		t.Fatalf("QueryBugCausality: %v", err)
	}

	// Collect failure classes and counts
	classCounts := map[string]int{}
	for _, b := range bugs {
		classCounts[b.FailureClass] += b.AttemptCount
	}

	// Verify specific failure classes appear (not all 'unknown')
	if classCounts["timeout"] != 3 {
		t.Errorf("timeout attempts: got %d, want 3", classCounts["timeout"])
	}
	if classCounts["build_fail"] != 2 {
		t.Errorf("build_fail attempts: got %d, want 2", classCounts["build_fail"])
	}
	if classCounts["auth_fail"] != 1 {
		t.Errorf("auth_fail attempts: got %d, want 1", classCounts["auth_fail"])
	}
	// Empty and NULL failure_class should both map to 'unknown'
	if classCounts["unknown"] != 2 {
		t.Errorf("unknown attempts: got %d, want 2 (from empty + NULL failure_class)", classCounts["unknown"])
	}

	// Verify non-failure results (success, approve, no_changes, skipped) are excluded
	totalAttempts := 0
	for _, c := range classCounts {
		totalAttempts += c
	}
	if totalAttempts != 8 {
		t.Errorf("total failure attempts: got %d, want 8 (excluding non-failure results)", totalAttempts)
	}

	// Also verify via QueryFailures
	since := now.Add(-30 * 24 * time.Hour)
	failures, err := db.QueryFailures(since)
	if err != nil {
		t.Fatalf("QueryFailures: %v", err)
	}

	failClasses := map[string]int{}
	for _, f := range failures {
		failClasses[f.FailureClass] = f.Count
	}
	if failClasses["timeout"] != 3 {
		t.Errorf("QueryFailures timeout: got %d, want 3", failClasses["timeout"])
	}
	if failClasses["build_fail"] != 2 {
		t.Errorf("QueryFailures build_fail: got %d, want 2", failClasses["build_fail"])
	}
}

// TestViewRefreshEmptyTables verifies that RefreshMaterializedViews handles
// empty data gracefully without errors.
func TestViewRefreshEmptyTables(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Refresh with no data — should not error
	if err := RefreshMaterializedViews(context.Background(), db); err != nil {
		t.Fatalf("RefreshMaterializedViews on empty DB: %v", err)
	}

	// All materialized views should exist but be empty
	tables := []string{
		"daily_formula_stats",
		"weekly_merge_stats",
		"phase_cost_breakdown",
		"tool_usage_stats",
		"failure_hotspots",
	}
	ctx := context.Background()
	for _, tbl := range tables {
		var count int
		if err := db.SqlDB().QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tbl).Scan(&count); err != nil {
			t.Errorf("count %s: %v", tbl, err)
		}
		if count != 0 {
			t.Errorf("%s: expected 0 rows on empty refresh, got %d", tbl, count)
		}
	}
}

// TestViewRefreshDORAWithApproveResult verifies that sage-review runs with
// result='approve' (the actual sage verdict value) are correctly detected as merges.
func TestViewRefreshDORAWithApproveResult(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// Insert a bead with sage-review that wrote 'approve' as result
	// (this is what the sage actually writes in production)
	_, err = db.SqlDB().ExecContext(ctx, `
		INSERT INTO agent_runs_olap (id, bead_id, formula_name, phase, tower, repo, result, started_at, completed_at)
		VALUES
			('ar1', 'test-approve', 'task-default', 'implement', 't1', 'test', 'success', ?, ?),
			('ar2', 'test-approve', 'task-default', 'sage-review', 't1', 'test', 'approve', ?, ?)`,
		now.Add(-2*time.Hour), now.Add(-time.Hour),
		now.Add(-30*time.Minute), now.Add(-15*time.Minute))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	if err := RefreshMaterializedViews(ctx, db); err != nil {
		t.Fatalf("RefreshMaterializedViews: %v", err)
	}

	var mergeCount int
	err = db.SqlDB().QueryRowContext(ctx,
		"SELECT COALESCE(SUM(merge_count), 0) FROM weekly_merge_stats").Scan(&mergeCount)
	if err != nil {
		t.Fatalf("query merge_count: %v", err)
	}
	if mergeCount != 1 {
		t.Errorf("merge_count for 'approve' result: got %d, want 1", mergeCount)
	}

	// Verify lead time is correct: from implement start to sage-review completion
	var avgLeadTime float64
	err = db.SqlDB().QueryRowContext(ctx,
		"SELECT COALESCE(AVG(avg_lead_time_s), 0) FROM weekly_merge_stats").Scan(&avgLeadTime)
	if err != nil {
		t.Fatalf("query lead_time: %v", err)
	}
	if avgLeadTime <= 0 {
		t.Errorf("avg_lead_time_s: got %.2f, want > 0", avgLeadTime)
	}

	// Also verify QueryTrends picks up the merge
	since := now.Add(-30 * 24 * time.Hour)
	trends, err := db.QueryTrends(since)
	if err != nil {
		t.Fatalf("QueryTrends: %v", err)
	}
	totalMerges := 0
	for _, tr := range trends {
		totalMerges += tr.MergeCount
	}
	if totalMerges != 1 {
		t.Errorf("QueryTrends total merges: got %d, want 1", totalMerges)
	}
}

// TestQueryFormulaPerformance_AvgReviewRoundsUndiluted verifies that
// avg_review_rounds reflects only rows with review_rounds > 0 (i.e. sage
// runs) and is not diluted by zero-valued apprentice/wizard rows. This
// mirrors the pattern already applied to daily_formula_stats.
func TestQueryFormulaPerformance_AvgReviewRoundsUndiluted(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// Seed 3 apprentice runs (review_rounds=0) and 1 sage run (review_rounds=3).
	// Dilution bug: AVG(review_rounds) over {0,0,0,3} = 0.75.
	// Correct:     AVG(CASE WHEN review_rounds > 0 ...) = 3.0.
	runs := []struct {
		id, phase, result string
		rounds            int
	}{
		{"a1", "implement", "success", 0},
		{"a2", "implement", "success", 0},
		{"a3", "implement", "failure", 0},
		{"a4", "sage-review", "approve", 3},
	}
	for _, r := range runs {
		_, err := db.SqlDB().ExecContext(ctx, `
			INSERT INTO agent_runs_olap (id, bead_id, formula_name, formula_version,
				phase, tower, repo, result, review_rounds, started_at, completed_at)
			VALUES (?, 'b1', 'review-heavy', '1', ?, 't1', '', ?, ?, ?, ?)`,
			r.id, r.phase, r.result, r.rounds,
			now.Add(-time.Hour), now.Add(-time.Hour+time.Minute))
		if err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}

	stats, err := db.QueryFormulaPerformance(now.Add(-24 * time.Hour))
	if err != nil {
		t.Fatalf("QueryFormulaPerformance: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("expected 1 formula row, got %d", len(stats))
	}
	got := stats[0]
	if got.FormulaName != "review-heavy" {
		t.Errorf("FormulaName: got %q, want review-heavy", got.FormulaName)
	}
	if got.TotalRuns != 4 {
		t.Errorf("TotalRuns: got %d, want 4", got.TotalRuns)
	}
	// Only the sage row contributes to avg_review_rounds (3.0 rounded to 1dp).
	if math.Abs(got.AvgReviewRounds-3.0) > 0.1 {
		t.Errorf("AvgReviewRounds: got %.2f, want 3.0 (undiluted by review_rounds=0 rows)", got.AvgReviewRounds)
	}
}

// TestQueryFormulaPerformance_ApproveCountsAsSuccess verifies that result
// 'approve' (the sage verdict for successful review) counts toward successes
// and success_rate alongside result 'success'. Mirrors the spi-wob6d fix in
// weekly_merge_stats where review-heavy formulas were under-reporting because
// sage approvals weren't being counted.
func TestQueryFormulaPerformance_ApproveCountsAsSuccess(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// 2 success + 2 approve + 1 failure → 4 of 5 = 80% success.
	// Before the fix: only 2/5 = 40% would be reported.
	runs := []struct {
		id, phase, result string
	}{
		{"s1", "implement", "success"},
		{"s2", "implement", "success"},
		{"s3", "sage-review", "approve"},
		{"s4", "sage-review", "approve"},
		{"s5", "implement", "failure"},
	}
	for _, r := range runs {
		_, err := db.SqlDB().ExecContext(ctx, `
			INSERT INTO agent_runs_olap (id, bead_id, formula_name, formula_version,
				phase, tower, repo, result, started_at, completed_at)
			VALUES (?, 'b1', 'review-heavy', '1', ?, 't1', '', ?, ?, ?)`,
			r.id, r.phase, r.result,
			now.Add(-time.Hour), now.Add(-time.Hour+time.Minute))
		if err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}

	stats, err := db.QueryFormulaPerformance(now.Add(-24 * time.Hour))
	if err != nil {
		t.Fatalf("QueryFormulaPerformance: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("expected 1 formula row, got %d", len(stats))
	}
	got := stats[0]
	if got.TotalRuns != 5 {
		t.Errorf("TotalRuns: got %d, want 5", got.TotalRuns)
	}
	if got.Successes != 4 {
		t.Errorf("Successes (success + approve): got %d, want 4", got.Successes)
	}
	if math.Abs(got.SuccessRate-80.0) > 0.1 {
		t.Errorf("SuccessRate: got %.1f, want 80.0 (counts approve alongside success)", got.SuccessRate)
	}
}
