package olap

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"
)

// Test constants for reuse across integration tests.
const (
	testTower   = "test-tower"
	testRepo    = "test"
	testFormula = "task-default"
)

var testBeadIDs = []string{"test-a001", "test-a002", "test-a003", "test-a004", "test-a005"}

// createMockDolt creates an in-memory DuckDB acting as a mock Dolt source
// with agent_runs schema and known test data. Returns the mock DB connection.
func createMockDolt(t *testing.T) (*sql.DB, time.Time) {
	t.Helper()

	mockDolt, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatalf("Open mock dolt: %v", err)
	}

	_, err = mockDolt.Exec(`CREATE TABLE agent_runs (
		id VARCHAR PRIMARY KEY, bead_id VARCHAR, epic_id VARCHAR,
		parent_run_id VARCHAR, formula_name VARCHAR, formula_version VARCHAR,
		phase VARCHAR, role VARCHAR, model VARCHAR, tower VARCHAR,
		branch VARCHAR, result VARCHAR, review_rounds INTEGER,
		context_tokens_in BIGINT, context_tokens_out BIGINT, total_tokens BIGINT,
		cost_usd DOUBLE, duration_seconds DOUBLE,
		startup_seconds DOUBLE, working_seconds DOUBLE, queue_seconds DOUBLE, review_seconds DOUBLE,
		files_changed INTEGER, lines_added INTEGER, lines_removed INTEGER,
		read_calls INTEGER, edit_calls INTEGER, tool_calls_json TEXT,
		failure_class VARCHAR, attempt_number INTEGER,
		started_at TIMESTAMP, completed_at TIMESTAMP
	)`)
	if err != nil {
		t.Fatalf("create mock agent_runs: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)

	// 5 runs with formula_name='task-default', phase='implement', result='success'
	for i := 0; i < 5; i++ {
		beadID := testBeadIDs[i%len(testBeadIDs)]
		started := now.Add(-time.Duration(10-i) * time.Hour)
		completed := started.Add(90 * time.Second)
		_, err = mockDolt.Exec(`INSERT INTO agent_runs VALUES (?, ?, NULL, NULL, ?, '3', 'implement', 'apprentice', 'claude-opus-4-6', ?, 'feat/test', 'success', 1, 1000, 500, 1500, 0.15, 90.0, 5.0, 80.0, 3.0, 2.0, 3, 50, 20, 12, 5, '{"Read":12,"Edit":5}', NULL, 1, ?, ?)`,
			fmt.Sprintf("run-impl-%d", i), beadID, testFormula, testTower, started, completed)
		if err != nil {
			t.Fatalf("insert impl run %d: %v", i, err)
		}
	}

	// 3 runs with phase='sage-review', result='success' (these count as merges for DORA)
	for i := 0; i < 3; i++ {
		beadID := testBeadIDs[i]
		started := now.Add(-time.Duration(9-i) * time.Hour)
		completed := started.Add(30 * time.Second)
		_, err = mockDolt.Exec(`INSERT INTO agent_runs VALUES (?, ?, NULL, NULL, ?, '3', 'sage-review', 'sage', 'claude-opus-4-6', ?, 'feat/test', 'success', 0, 500, 200, 700, 0.05, 30.0, 2.0, 25.0, 2.0, 1.0, 0, 0, 0, 3, 0, '{"Read":3}', NULL, 1, ?, ?)`,
			fmt.Sprintf("run-review-%d", i), beadID, testFormula, testTower, started, completed)
		if err != nil {
			t.Fatalf("insert review run %d: %v", i, err)
		}
	}

	// 2 runs with result='error', failure_class='build_fail'
	for i := 0; i < 2; i++ {
		beadID := testBeadIDs[3+i%2]
		started := now.Add(-time.Duration(8-i) * time.Hour)
		completed := started.Add(60 * time.Second)
		_, err = mockDolt.Exec(`INSERT INTO agent_runs VALUES (?, ?, NULL, NULL, ?, '3', 'implement', 'apprentice', 'claude-opus-4-6', ?, 'feat/test', 'error', 0, 800, 300, 1100, 0.12, 60.0, 3.0, 50.0, 5.0, 2.0, 1, 10, 5, 5, 2, '{"Read":5,"Edit":2}', 'build_fail', ?, ?, ?)`,
			fmt.Sprintf("run-fail-%d", i), beadID, testFormula, testTower, i+1, started, completed)
		if err != nil {
			t.Fatalf("insert fail run %d: %v", i, err)
		}
	}

	// 1 run with result='timeout', failure_class='timeout'
	{
		started := now.Add(-7 * time.Hour)
		completed := started.Add(300 * time.Second)
		_, err = mockDolt.Exec(`INSERT INTO agent_runs VALUES (?, ?, NULL, NULL, ?, '3', 'implement', 'apprentice', 'claude-opus-4-6', ?, 'feat/test', 'timeout', 0, 600, 200, 800, 0.20, 300.0, 5.0, 280.0, 10.0, 5.0, 0, 0, 0, 2, 0, '{"Read":2}', 'timeout', 1, ?, ?)`,
			"run-timeout-0", testBeadIDs[4], testFormula, testTower, started, completed)
		if err != nil {
			t.Fatalf("insert timeout run: %v", err)
		}
	}

	return mockDolt, now
}

// TestETLViewsQueries_Integration runs the full ETL→views→queries pipeline
// and verifies that all query functions return correct, non-zero results.
func TestETLViewsQueries_Integration(t *testing.T) {
	// 1. Set up OLAP and mock Dolt.
	olapDB, err := Open("")
	if err != nil {
		t.Fatalf("Open olap: %v", err)
	}
	defer olapDB.Close()

	mockDolt, now := createMockDolt(t)
	defer mockDolt.Close()

	// 2. Run ETL sync.
	ctx := context.Background()
	etl := NewETL(olapDB)
	n, err := etl.Sync(ctx, mockDolt)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// We inserted 5+3+2+1 = 11 rows.
	if n != 11 {
		t.Errorf("expected 11 rows synced, got %d", n)
	}

	// 3. Verify agent_runs_olap has correct data including repo and formula_name.
	var totalRows int
	if err := olapDB.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM agent_runs_olap").Scan(&totalRows); err != nil {
		t.Fatalf("count agent_runs_olap: %v", err)
	}
	if totalRows != 11 {
		t.Errorf("expected 11 rows in agent_runs_olap, got %d", totalRows)
	}

	// Verify repo is derived from bead_id prefix.
	var repoCount int
	if err := olapDB.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM agent_runs_olap WHERE repo = ?", testRepo,
	).Scan(&repoCount); err != nil {
		t.Fatalf("count repo: %v", err)
	}
	if repoCount != 11 {
		t.Errorf("expected all 11 rows with repo=%q, got %d", testRepo, repoCount)
	}

	// Verify formula_name is populated.
	var formulaCount int
	if err := olapDB.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM agent_runs_olap WHERE formula_name = ?", testFormula,
	).Scan(&formulaCount); err != nil {
		t.Fatalf("count formula: %v", err)
	}
	if formulaCount != 11 {
		t.Errorf("expected all 11 rows with formula_name=%q, got %d", testFormula, formulaCount)
	}

	// 4. Views were refreshed by ETL.Sync. Now run queries.

	since := now.Add(-30 * 24 * time.Hour) // far back enough

	// --- QueryDORA ---
	dora, err := olapDB.QueryDORA(since)
	if err != nil {
		t.Fatalf("QueryDORA: %v", err)
	}
	if dora.DeployFrequency <= 0 {
		t.Errorf("DORA deploy_frequency should be > 0, got %f", dora.DeployFrequency)
	}
	// We have 3 beads with sage-review success → merge_count should be > 0
	t.Logf("DORA: deploy_freq=%.2f, lead_time=%.0fs, cfr=%.2f, mttr=%.0fs",
		dora.DeployFrequency, dora.LeadTimeSeconds, dora.ChangeFailureRate, dora.MTTRSeconds)

	// --- QueryFormulaPerformance ---
	formulas, err := olapDB.QueryFormulaPerformance(since)
	if err != nil {
		t.Fatalf("QueryFormulaPerformance: %v", err)
	}
	if len(formulas) == 0 {
		t.Fatal("expected formula performance results, got none")
	}

	var foundTaskDefault bool
	for _, f := range formulas {
		if f.FormulaName == testFormula {
			foundTaskDefault = true
			// We inserted 11 total runs with formula_name='task-default'.
			if f.TotalRuns != 11 {
				t.Errorf("formula %q total_runs=%d, want 11", testFormula, f.TotalRuns)
			}
			break
		}
	}
	if !foundTaskDefault {
		t.Errorf("expected formula %q in results, got: %+v", testFormula, formulas)
	}

	// --- QueryBugCausality ---
	bugs, err := olapDB.QueryBugCausality(10)
	if err != nil {
		t.Fatalf("QueryBugCausality: %v", err)
	}
	if len(bugs) == 0 {
		t.Fatal("expected bug causality results, got none")
	}

	bugClasses := make(map[string]int)
	for _, b := range bugs {
		bugClasses[b.FailureClass] += b.AttemptCount
	}
	if bugClasses["build_fail"] < 1 {
		t.Errorf("expected build_fail in bug causality, got: %v", bugClasses)
	}
	if bugClasses["timeout"] < 1 {
		t.Errorf("expected timeout in bug causality, got: %v", bugClasses)
	}
	// Verify 'unknown' is not the only class.
	if _, hasUnknown := bugClasses["unknown"]; hasUnknown && len(bugClasses) == 1 {
		t.Error("bug causality should have specific failure classes, not just 'unknown'")
	}

	// --- QueryFailures ---
	failures, err := olapDB.QueryFailures(since)
	if err != nil {
		t.Fatalf("QueryFailures: %v", err)
	}
	if len(failures) == 0 {
		t.Fatal("expected failure results, got none")
	}

	failClasses := make(map[string]int)
	for _, f := range failures {
		failClasses[f.FailureClass] = f.Count
	}
	if failClasses["build_fail"] != 2 {
		t.Errorf("expected build_fail count=2, got %d", failClasses["build_fail"])
	}
	if failClasses["timeout"] != 1 {
		t.Errorf("expected timeout count=1, got %d", failClasses["timeout"])
	}

	// --- QuerySummary ---
	summary, err := olapDB.QuerySummary(since)
	if err != nil {
		t.Fatalf("QuerySummary: %v", err)
	}
	if summary.TotalRuns != 11 {
		t.Errorf("summary total_runs=%d, want 11", summary.TotalRuns)
	}
	if summary.Successes != 8 {
		t.Errorf("summary successes=%d, want 8 (5 impl + 3 review)", summary.Successes)
	}
	if summary.Failures != 3 {
		t.Errorf("summary failures=%d, want 3 (2 error + 1 timeout)", summary.Failures)
	}

	// --- QueryModelBreakdown ---
	models, err := olapDB.QueryModelBreakdown(since)
	if err != nil {
		t.Fatalf("QueryModelBreakdown: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("expected model breakdown results, got none")
	}
	if models[0].Model != "claude-opus-4-6" {
		t.Errorf("expected first model to be claude-opus-4-6, got %q", models[0].Model)
	}

	// --- QueryPhaseBreakdown ---
	phases, err := olapDB.QueryPhaseBreakdown(since)
	if err != nil {
		t.Fatalf("QueryPhaseBreakdown: %v", err)
	}
	if len(phases) == 0 {
		t.Fatal("expected phase breakdown results, got none")
	}
	phaseMap := make(map[string]int)
	for _, p := range phases {
		phaseMap[p.Phase] = p.RunCount
	}
	if phaseMap["implement"] != 8 {
		t.Errorf("implement phase count=%d, want 8", phaseMap["implement"])
	}
	if phaseMap["sage-review"] != 3 {
		t.Errorf("sage-review phase count=%d, want 3", phaseMap["sage-review"])
	}

	// --- QueryTrends ---
	trends, err := olapDB.QueryTrends(since)
	if err != nil {
		t.Fatalf("QueryTrends: %v", err)
	}
	if len(trends) == 0 {
		t.Fatal("expected trend results, got none")
	}
	// All runs are from today → one week entry.
	totalTrendRuns := 0
	for _, tr := range trends {
		totalTrendRuns += tr.RunCount
	}
	if totalTrendRuns != 11 {
		t.Errorf("total trend runs=%d, want 11", totalTrendRuns)
	}
}

// TestConcurrentWriters verifies that 5 goroutines can each write 100 events
// via the persistent DB's WithWriteLock without errors, and all 500 events land.
//
// Note: we use the persistent *DB + WithWriteLock pattern (Go mutex) rather than
// WriteFunc (file-level open→write→close) because DuckDB's CGO driver does not
// support truly concurrent file opens from multiple goroutines safely. In
// production, the daemon serializes all writes through DuckWriter.Submit which
// uses WithWriteLock. WriteFunc is tested sequentially in TestWriteFuncSequential.
func TestConcurrentWriters(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	const goroutines = 5
	const eventsPerGoroutine = 100
	const totalExpected = goroutines * eventsPerGoroutine // 500

	var wg sync.WaitGroup
	errs := make(chan error, totalExpected)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(gID int) {
			defer wg.Done()
			for i := 0; i < eventsPerGoroutine; i++ {
				err := db.WithWriteLock(func(sqlDB *sql.DB) error {
					_, err := sqlDB.Exec(`INSERT INTO tool_events
						(session_id, bead_id, agent_name, step, tool_name, duration_ms, success, timestamp, tower)
						VALUES (?, ?, 'test-agent', 'implement', 'Read', 10, true, current_timestamp, ?)`,
						fmt.Sprintf("sess-g%d", gID),
						fmt.Sprintf("test-concurrent-%d-%d", gID, i),
						testTower,
					)
					return err
				})
				if err != nil {
					errs <- fmt.Errorf("goroutine %d, event %d: %w", gID, i, err)
				}
			}
		}(g)
	}

	wg.Wait()
	close(errs)

	// Check no errors propagated.
	var errCount int
	for err := range errs {
		errCount++
		if errCount <= 3 {
			t.Errorf("write error: %v", err)
		}
	}
	if errCount > 0 {
		t.Errorf("total write errors: %d (first 3 shown above)", errCount)
	}

	// Verify all 500 events were written.
	ctx := context.Background()
	var count int
	if err := db.SqlDB().QueryRowContext(ctx, "SELECT COUNT(*) FROM tool_events").Scan(&count); err != nil {
		t.Fatalf("count tool_events: %v", err)
	}

	if count != totalExpected {
		t.Errorf("expected %d events, got %d", totalExpected, count)
	}
}
