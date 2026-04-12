package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/olap"
)

// Test constants for metrics integration tests.
const (
	metricsTestTower   = "test-tower"
	metricsTestFormula = "task-default"
)

// setupMetricsTestDB creates a DuckDB at the path that cmdMetrics will resolve
// via tower config, populates it with known test data, and sets the required
// environment variables. Returns a cleanup function.
func setupMetricsTestDB(t *testing.T) (cleanup func()) {
	t.Helper()

	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "config")
	dataDir := filepath.Join(tmp, "data")
	towersDir := filepath.Join(configDir, "towers")

	if err := os.MkdirAll(towersDir, 0755); err != nil {
		t.Fatal(err)
	}

	// OLAPPath: <XDG_DATA_HOME>/spire/test-tower/analytics.db
	olapDir := filepath.Join(dataDir, "spire", metricsTestTower)
	if err := os.MkdirAll(olapDir, 0755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(olapDir, "analytics.db")

	// Write tower config.
	towerJSON := fmt.Sprintf(`{"name":"%s","project_id":"test","hub_prefix":"tst","database":"test"}`, metricsTestTower)
	if err := os.WriteFile(filepath.Join(towersDir, metricsTestTower+".json"), []byte(towerJSON), 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("SPIRE_CONFIG_DIR", configDir)
	t.Setenv("SPIRE_TOWER", metricsTestTower)
	t.Setenv("XDG_DATA_HOME", dataDir)

	// Open DuckDB and populate with test data.
	db, err := olap.Open(dbPath)
	if err != nil {
		t.Fatalf("Open DuckDB: %v", err)
	}

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	testBeadIDs := []string{"test-a001", "test-a002", "test-a003", "test-a004", "test-a005"}

	// 5 success runs (implement phase)
	for i := 0; i < 5; i++ {
		started := now.Add(-time.Duration(10-i) * time.Hour)
		completed := started.Add(90 * time.Second)
		_, err := db.SqlDB().ExecContext(ctx, `
			INSERT INTO agent_runs_olap (
				id, bead_id, formula_name, formula_version, phase, model, tower, repo,
				result, cost_usd, duration_seconds, review_rounds,
				read_calls, edit_calls, total_tokens,
				started_at, completed_at
			) VALUES (?, ?, ?, '3', 'implement', 'claude-opus-4-6', ?, 'test',
				'success', 0.15, 90.0, 1, 12, 5, 1500, ?, ?)`,
			fmt.Sprintf("run-impl-%d", i), testBeadIDs[i%5],
			metricsTestFormula, metricsTestTower,
			started, completed)
		if err != nil {
			t.Fatalf("insert impl run %d: %v", i, err)
		}
	}

	// 3 sage-review success runs (count as merges for DORA)
	for i := 0; i < 3; i++ {
		started := now.Add(-time.Duration(9-i) * time.Hour)
		completed := started.Add(30 * time.Second)
		_, err := db.SqlDB().ExecContext(ctx, `
			INSERT INTO agent_runs_olap (
				id, bead_id, formula_name, formula_version, phase, model, tower, repo,
				result, cost_usd, duration_seconds, review_rounds,
				read_calls, edit_calls, total_tokens,
				started_at, completed_at
			) VALUES (?, ?, ?, '3', 'sage-review', 'claude-opus-4-6', ?, 'test',
				'success', 0.05, 30.0, 0, 3, 0, 700, ?, ?)`,
			fmt.Sprintf("run-review-%d", i), testBeadIDs[i],
			metricsTestFormula, metricsTestTower,
			started, completed)
		if err != nil {
			t.Fatalf("insert review run %d: %v", i, err)
		}
	}

	// 2 build_fail runs
	for i := 0; i < 2; i++ {
		started := now.Add(-time.Duration(8-i) * time.Hour)
		completed := started.Add(60 * time.Second)
		_, err := db.SqlDB().ExecContext(ctx, `
			INSERT INTO agent_runs_olap (
				id, bead_id, formula_name, formula_version, phase, model, tower, repo,
				result, cost_usd, duration_seconds, failure_class, attempt_number,
				read_calls, edit_calls, total_tokens,
				started_at, completed_at
			) VALUES (?, ?, ?, '3', 'implement', 'claude-opus-4-6', ?, 'test',
				'error', 0.12, 60.0, 'build_fail', ?, 5, 2, 1100, ?, ?)`,
			fmt.Sprintf("run-fail-%d", i), testBeadIDs[3+i%2],
			metricsTestFormula, metricsTestTower,
			i+1, started, completed)
		if err != nil {
			t.Fatalf("insert fail run %d: %v", i, err)
		}
	}

	// 1 timeout run
	{
		started := now.Add(-7 * time.Hour)
		completed := started.Add(300 * time.Second)
		_, err := db.SqlDB().ExecContext(ctx, `
			INSERT INTO agent_runs_olap (
				id, bead_id, formula_name, formula_version, phase, model, tower, repo,
				result, cost_usd, duration_seconds, failure_class, attempt_number,
				read_calls, edit_calls, total_tokens,
				started_at, completed_at
			) VALUES ('run-timeout-0', ?, ?, '3', 'implement', 'claude-opus-4-6', ?, 'test',
				'timeout', 0.20, 300.0, 'timeout', 1, 2, 0, 800, ?, ?)`,
			testBeadIDs[4], metricsTestFormula, metricsTestTower,
			started, completed)
		if err != nil {
			t.Fatalf("insert timeout run: %v", err)
		}
	}

	// Refresh materialized views so DORA, bug causality etc. are populated.
	if err := olap.RefreshMaterializedViews(ctx, db); err != nil {
		t.Fatalf("RefreshMaterializedViews: %v", err)
	}

	// Insert tool_events for --tools flag.
	tools := []struct {
		name string
		dur  int
		ok   bool
	}{
		{"Read", 50, true},
		{"Edit", 120, true},
		{"Bash", 300, false},
		{"Read", 30, true},
		{"Grep", 15, true},
	}
	for i, tool := range tools {
		_, err := db.SqlDB().ExecContext(ctx, `
			INSERT INTO tool_events (session_id, bead_id, agent_name, step, tool_name, duration_ms, success, timestamp, tower, provider, event_kind)
			VALUES (?, 'test-a001', 'apprentice-0', 'implement', ?, ?, ?, ?, ?, 'claude', 'tool_result')`,
			fmt.Sprintf("sess-%d", i), tool.name, tool.dur, tool.ok,
			now.Add(-time.Duration(i)*time.Minute), metricsTestTower)
		if err != nil {
			t.Fatalf("insert tool event %d: %v", i, err)
		}
	}

	db.Close()

	return func() {
		// tmp dir is cleaned up by t.TempDir()
	}
}

// TestMetricsIntegration_DORA verifies --dora returns non-zero deploy_frequency.
func TestMetricsIntegration_DORA(t *testing.T) {
	cleanup := setupMetricsTestDB(t)
	defer cleanup()

	output, err := captureStdout(t, func() error {
		return cmdMetrics([]string{"--dora", "--json"})
	})
	if err != nil {
		t.Fatalf("cmdMetrics --dora: %v", err)
	}

	var dora struct {
		DeployFrequency   float64
		LeadTimeSeconds   float64
		ChangeFailureRate float64
		MTTRSeconds       float64
	}
	if err := json.Unmarshal([]byte(output), &dora); err != nil {
		t.Fatalf("unmarshal DORA JSON: %v\nraw output: %s", err, output)
	}
	if dora.DeployFrequency <= 0 {
		t.Errorf("DeployFrequency should be > 0, got %f", dora.DeployFrequency)
	}
}

// TestMetricsIntegration_Failures verifies --failures includes specific failure classes.
func TestMetricsIntegration_Failures(t *testing.T) {
	cleanup := setupMetricsTestDB(t)
	defer cleanup()

	output, err := captureStdout(t, func() error {
		return cmdMetrics([]string{"--failures", "--json"})
	})
	if err != nil {
		t.Fatalf("cmdMetrics --failures: %v", err)
	}

	var failures []struct {
		FailureClass string
		Count        int
		Percentage   float64
	}
	if err := json.Unmarshal([]byte(output), &failures); err != nil {
		t.Fatalf("unmarshal failures JSON: %v\nraw output: %s", err, output)
	}
	if len(failures) == 0 {
		t.Fatal("expected failure results, got none")
	}

	classes := make(map[string]int)
	for _, f := range failures {
		classes[f.FailureClass] = f.Count
	}
	if classes["build_fail"] < 1 {
		t.Errorf("expected build_fail in failures, got: %v", classes)
	}
	if classes["timeout"] < 1 {
		t.Errorf("expected timeout in failures, got: %v", classes)
	}
}

// TestMetricsIntegration_Tools verifies --tools returns non-empty tool stats.
func TestMetricsIntegration_Tools(t *testing.T) {
	cleanup := setupMetricsTestDB(t)
	defer cleanup()

	output, err := captureStdout(t, func() error {
		return cmdMetrics([]string{"--tools", "--json"})
	})
	if err != nil {
		t.Fatalf("cmdMetrics --tools: %v", err)
	}

	// ToolEventStats has json tags (tool_name, count, etc.)
	var tools []struct {
		ToolName     string  `json:"tool_name"`
		Count        int     `json:"count"`
		AvgDurationMs float64 `json:"avg_duration_ms"`
		FailureCount int     `json:"failure_count"`
	}
	if err := json.Unmarshal([]byte(output), &tools); err != nil {
		t.Fatalf("unmarshal tools JSON: %v\nraw output: %s", err, output)
	}
	if len(tools) == 0 {
		t.Fatal("expected tool results, got none")
	}

	toolMap := make(map[string]int)
	for _, tool := range tools {
		toolMap[tool.ToolName] = tool.Count
	}
	if toolMap["Read"] < 1 {
		t.Errorf("expected Read in tools, got: %v", toolMap)
	}
}

// TestMetricsIntegration_Bugs verifies --bugs returns failure hotspots.
func TestMetricsIntegration_Bugs(t *testing.T) {
	cleanup := setupMetricsTestDB(t)
	defer cleanup()

	output, err := captureStdout(t, func() error {
		return cmdMetrics([]string{"--bugs", "--json"})
	})
	if err != nil {
		t.Fatalf("cmdMetrics --bugs: %v", err)
	}

	var bugs []struct {
		BeadID       string
		FailureClass string
		AttemptCount int
	}
	if err := json.Unmarshal([]byte(output), &bugs); err != nil {
		t.Fatalf("unmarshal bugs JSON: %v\nraw output: %s", err, output)
	}
	if len(bugs) == 0 {
		t.Fatal("expected bug results, got none")
	}

	bugClasses := make(map[string]bool)
	for _, b := range bugs {
		bugClasses[b.FailureClass] = true
	}
	if !bugClasses["build_fail"] && !bugClasses["timeout"] {
		t.Errorf("expected build_fail or timeout in bugs, got: %v", bugClasses)
	}
}

// TestMetricsIntegration_Model verifies --model returns model breakdown.
func TestMetricsIntegration_Model(t *testing.T) {
	cleanup := setupMetricsTestDB(t)
	defer cleanup()

	output, err := captureStdout(t, func() error {
		return cmdMetrics([]string{"--model", "--json"})
	})
	if err != nil {
		t.Fatalf("cmdMetrics --model: %v", err)
	}

	var models []struct {
		Model       string
		RunCount    int
		SuccessRate float64
	}
	if err := json.Unmarshal([]byte(output), &models); err != nil {
		t.Fatalf("unmarshal model JSON: %v\nraw output: %s", err, output)
	}
	if len(models) == 0 {
		t.Fatal("expected model results, got none")
	}
	if models[0].RunCount <= 0 {
		t.Errorf("expected non-zero run count, got %d", models[0].RunCount)
	}
}

// TestMetricsIntegration_Phase verifies --phase returns phase breakdown.
func TestMetricsIntegration_Phase(t *testing.T) {
	cleanup := setupMetricsTestDB(t)
	defer cleanup()

	output, err := captureStdout(t, func() error {
		return cmdMetrics([]string{"--phase", "--json"})
	})
	if err != nil {
		t.Fatalf("cmdMetrics --phase: %v", err)
	}

	var phases []struct {
		Phase       string
		RunCount    int
		SuccessRate float64
	}
	if err := json.Unmarshal([]byte(output), &phases); err != nil {
		t.Fatalf("unmarshal phase JSON: %v\nraw output: %s", err, output)
	}
	if len(phases) == 0 {
		t.Fatal("expected phase results, got none")
	}

	phaseMap := make(map[string]int)
	for _, p := range phases {
		phaseMap[p.Phase] = p.RunCount
	}
	if phaseMap["implement"] <= 0 {
		t.Errorf("expected implement phase with runs, got: %v", phaseMap)
	}
}

// TestMetricsIntegration_DefaultSummary verifies the default (no flags) returns summary.
func TestMetricsIntegration_DefaultSummary(t *testing.T) {
	cleanup := setupMetricsTestDB(t)
	defer cleanup()

	output, err := captureStdout(t, func() error {
		return cmdMetrics([]string{"--json"})
	})
	if err != nil {
		t.Fatalf("cmdMetrics (default): %v", err)
	}

	var summary struct {
		TotalRuns   int
		Successes   int
		Failures    int
		SuccessRate float64
	}
	if err := json.Unmarshal([]byte(output), &summary); err != nil {
		t.Fatalf("unmarshal summary JSON: %v\nraw output: %s", err, output)
	}
	if summary.TotalRuns <= 0 {
		t.Errorf("expected non-zero total_runs, got %d", summary.TotalRuns)
	}
	if summary.Successes <= 0 {
		t.Errorf("expected non-zero successes, got %d", summary.Successes)
	}
}
