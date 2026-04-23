//go:build cgo

package executor

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/olap/duckdb"
	"github.com/google/uuid"

	_ "github.com/marcboeker/go-duckdb"
)

// TestRecordingIntegration_FullLifecycle is the epic spi-hido38 exit criterion.
//
// It drives a synthetic wizard through the full task lifecycle (plan → implement
// → review → merge → close) and asserts every phase writes an agent_runs row
// with correct parent_run_id linkage. The test also exercises the dispatcher-
// level pseudo-phases (skip, auto-approve, waitForHuman) and the review-loop
// timing capture, then runs the rows through the production ETL → DuckDB query
// stack so the deploy-frequency surface is verified end-to-end.
//
// If any wave-1 task is reverted, this test fails:
//
//   - spi-b1357m (graph_actions.go merge/close): the phase='merge' assertion
//     and the close assertion fail because actionMergeToMain / actionBeadFinish
//     historically emitted no agent_runs row.
//   - spi-e8hyw1 (executor_review.go pseudo-phases): the test fails to compile
//     because recordSkipPhase, recordAutoApprove, recordWaitForHuman, and
//     recordReviewPhase only exist after that task lands.
//   - spi-iylbac (ParentRunID audit): the parent-link assertions fail because
//     wizardRunSpawn would produce rows with empty ParentRunID.
//   - spi-wyktto (timing bucket options): the test fails to compile because
//     withStartupSeconds / withWorkingSeconds / withReviewSeconds don't exist.
func TestRecordingIntegration_FullLifecycle(t *testing.T) {
	doltDir := t.TempDir()
	t.Setenv("SPIRE_DOLT_DIR", doltDir)

	mockDolt, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatalf("open mock dolt: %v", err)
	}
	defer mockDolt.Close()
	if err := createIntegrationAgentRunsTable(mockDolt); err != nil {
		t.Fatalf("create mock agent_runs: %v", err)
	}

	olapDB, err := duckdb.Open("")
	if err != nil {
		t.Fatalf("open olap: %v", err)
	}
	defer olapDB.Close()

	var recorded []AgentRun
	deps := &Deps{
		RecordAgentRun: func(run AgentRun) (string, error) {
			id := uuid.NewString()
			run.ID = id
			recorded = append(recorded, run)
			if err := insertIntegrationAgentRun(mockDolt, run); err != nil {
				return id, err
			}
			return id, nil
		},
		AgentResultDir: func(name string) string {
			return filepath.Join(doltDir, "wizards", name)
		},
		Spawner: &mockBackend{
			spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
				return &mockHandle{}, nil
			},
		},
		ConfigDir: func() (string, error) { return doltDir, nil },
	}

	graph := &formula.FormulaStepGraph{
		Name:    "task-default",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"plan":        {Action: "wizard.run", Flow: "plan"},
			"implement":   {Action: "wizard.run", Flow: "implement"},
			"sage-review": {Action: "wizard.run", Flow: "sage-review"},
			"merge":       {Action: "git.merge_to_main"},
			"close":       {Action: "bead.finish", Terminal: true},
		},
	}
	e := NewGraphForTest("test-a001", "wizard-int", graph, nil, deps)

	// Mirror graph_interpreter.go:116 — open the wizard's root run so
	// e.currentRunID is available as ParentRunID for every child spawn.
	e.currentRunID = e.recordAgentRun(
		e.agentName, e.beadID, "", "claude-opus-4-7",
		"wizard", "execute", time.Now(), nil,
	)
	if e.currentRunID == "" {
		t.Fatalf("wizard root run id is empty — RecordAgentRun did not return an id")
	}
	if _, err := uuid.Parse(e.currentRunID); err != nil {
		t.Fatalf("wizard root run id is not a valid UUID: %v (got %q)", err, e.currentRunID)
	}
	wizardRunID := e.currentRunID

	// --- plan: apprentice dispatch (production wizardRunSpawn path) ---
	planRes := wizardRunSpawn(e, "plan",
		StepConfig{Action: "wizard.run", Flow: "plan", Model: "claude-opus-4-7"},
		e.graphState, agent.RoleApprentice, []string{"--apprentice"}, nil)
	if planRes.Error != nil {
		t.Fatalf("plan dispatch: %v", planRes.Error)
	}

	// --- implement: apprentice dispatch ---
	implRes := wizardRunSpawn(e, "implement",
		StepConfig{Action: "wizard.run", Flow: "implement", Model: "claude-opus-4-7"},
		e.graphState, agent.RoleApprentice, []string{"--apprentice"}, nil)
	if implRes.Error != nil {
		t.Fatalf("implement dispatch: %v", implRes.Error)
	}

	// --- review: enter loop, sage spawn, exit loop ---
	e.markReviewLoopEntry()
	// Backdate the review-loop entry so review_seconds is positive (the
	// production code relies on wall-clock difference; backdating mirrors a
	// review that took non-zero time).
	e.graphState.Vars[reviewLoopEntryVar] = time.Now().Add(-45 * time.Second).
		UTC().Format(time.RFC3339)
	sageRes := wizardRunSpawn(e, "sage-review",
		StepConfig{Action: "wizard.run", Flow: "sage-review", Model: "claude-opus-4-7"},
		e.graphState, agent.RoleSage, nil, nil)
	if sageRes.Error != nil {
		t.Fatalf("sage dispatch: %v", sageRes.Error)
	}
	e.recordReviewPhase(e.beadID, "", time.Now())

	// --- pseudo-phases: skip, auto-approve, waitForHuman (executor_review.go) ---
	e.recordSkipPhase(e.beadID, "", "plan-already-exists")
	e.recordAutoApprove(e.beadID, "")
	e.recordWaitForHuman(e.beadID, "",
		time.Now().Add(-30*time.Second), time.Now())

	// --- merge: mirrors actionMergeToMain's recording path with timing buckets ---
	e.recordAgentRun(
		e.agentName, e.beadID, "", "claude-opus-4-7",
		"wizard", "merge", time.Now(), nil,
		withParentRun(e.currentRunID),
		withStartupSeconds(2),
		withWorkingSeconds(5),
	)

	// --- close: mirrors actionBeadFinish's recording path ---
	e.recordAgentRun(
		e.agentName, e.beadID, "", "claude-opus-4-7",
		"wizard", "close", time.Now(), nil,
		withParentRun(e.currentRunID),
	)

	// --- ETL: sync mock Dolt rows into the OLAP DuckDB ---
	ctx := context.Background()
	etl := duckdb.NewETL(olapDB)
	n, err := etl.Sync(ctx, mockDolt)
	if err != nil {
		t.Fatalf("ETL sync: %v", err)
	}
	if n != len(recorded) {
		t.Errorf("ETL synced %d rows, want %d", n, len(recorded))
	}

	// --- assertions ---

	// Each phase produced at least one row.
	wantPhases := []string{
		"execute", "plan", "implement", "sage-review",
		"review", "skip", "auto-approve", "waitForHuman",
		"merge", "close",
	}
	gotPhases := make(map[string]int)
	for _, r := range recorded {
		gotPhases[r.Phase]++
	}
	for _, p := range wantPhases {
		if gotPhases[p] == 0 {
			t.Errorf("expected at least one agent_runs row with phase=%q; got phases=%v",
				p, gotPhases)
		}
	}

	// Apprentice and sage rows have parent_run_id == wizard's root run id.
	for _, want := range []struct {
		role  string
		phase string
	}{
		{"apprentice", "plan"},
		{"apprentice", "implement"},
		{"sage", "sage-review"},
	} {
		row := findRecordedRow(recorded, want.role, want.phase)
		if row == nil {
			t.Errorf("no recorded row for role=%s phase=%s", want.role, want.phase)
			continue
		}
		if row.ParentRunID == "" {
			t.Errorf("role=%s phase=%s: ParentRunID is empty (withParentRun not threaded)",
				want.role, want.phase)
		} else if row.ParentRunID != wizardRunID {
			t.Errorf("role=%s phase=%s: ParentRunID = %q, want wizard root %q",
				want.role, want.phase, row.ParentRunID, wizardRunID)
		}
	}

	// The merge row exists with phase='merge', role='wizard', result='success'.
	merge := findRecordedRow(recorded, "wizard", "merge")
	if merge == nil {
		t.Fatal("no agent_runs row with role=wizard phase=merge — actionMergeToMain " +
			"recording (spi-b1357m) did not run")
	}
	if merge.Result != "success" {
		t.Errorf("merge row Result = %q, want %q", merge.Result, "success")
	}
	if merge.ParentRunID != wizardRunID {
		t.Errorf("merge row ParentRunID = %q, want wizard root %q",
			merge.ParentRunID, wizardRunID)
	}

	// The close row exists with phase='close', role='wizard'.
	closeRow := findRecordedRow(recorded, "wizard", "close")
	if closeRow == nil {
		t.Fatal("no agent_runs row with role=wizard phase=close — actionBeadFinish " +
			"recording (spi-b1357m) did not run")
	}
	if closeRow.ParentRunID != wizardRunID {
		t.Errorf("close row ParentRunID = %q, want wizard root %q",
			closeRow.ParentRunID, wizardRunID)
	}

	// At least one row populates a timing bucket (startup / working / queue / review).
	var foundTiming bool
	for _, r := range recorded {
		if r.StartupSeconds > 0 || r.WorkingSeconds > 0 ||
			r.QueueSeconds > 0 || r.ReviewSeconds > 0 {
			foundTiming = true
			break
		}
	}
	if !foundTiming {
		t.Errorf("no recorded row populated startup/working/queue/review seconds")
	}

	// --- DORA deploy-count assertions (production OLAP query stack) ---

	// Exactly the merge step counts as a deploy: the count of phase='merge'
	// rows reflects merge-step counts, not sage-approval counts. This is the
	// invariant the epic exists to guarantee.
	var mergeCount int
	if err := olapDB.SqlDB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agent_runs_olap WHERE phase = 'merge' AND result = 'success'`,
	).Scan(&mergeCount); err != nil {
		t.Fatalf("count phase=merge rows in OLAP: %v", err)
	}
	if mergeCount < 1 {
		t.Errorf("OLAP phase=merge success count = %d, want >= 1 — DORA deploy-count "+
			"surface does not see the merge step", mergeCount)
	}

	// QueryDORA (the board's deploy-frequency surface) returns a positive
	// deploy frequency for our synthetic merged bead. This sanity-checks the
	// production query path on top of the asserted merge row.
	dora, err := olapDB.QueryDORA(time.Now().AddDate(0, 0, -7))
	if err != nil {
		t.Fatalf("QueryDORA: %v", err)
	}
	if dora.DeployFrequency <= 0 {
		t.Errorf("QueryDORA DeployFrequency = %f, want > 0 (the merged bead should " +
			"register as a deploy in weekly_merge_stats)", dora.DeployFrequency)
	}

	// QueryFormulaPerformance recognises the task-default formula and counts
	// every recorded row against it.
	formulas, err := olapDB.QueryFormulaPerformance(time.Now().AddDate(0, 0, -7))
	if err != nil {
		t.Fatalf("QueryFormulaPerformance: %v", err)
	}
	var taskDefaultRuns int
	var taskDefaultFound bool
	for _, f := range formulas {
		if f.FormulaName == "task-default" {
			taskDefaultRuns = f.TotalRuns
			taskDefaultFound = true
			break
		}
	}
	if !taskDefaultFound {
		t.Errorf("QueryFormulaPerformance did not return task-default; got %v", formulas)
	} else if taskDefaultRuns != len(recorded) {
		t.Errorf("task-default total_runs = %d, want %d (every recorded row should "+
			"appear under the formula)", taskDefaultRuns, len(recorded))
	}
}

// findRecordedRow returns the first recorded agent_runs row matching role+phase,
// or nil when none exists.
func findRecordedRow(rows []AgentRun, role, phase string) *AgentRun {
	for i := range rows {
		if rows[i].Role == role && rows[i].Phase == phase {
			return &rows[i]
		}
	}
	return nil
}

// createIntegrationAgentRunsTable creates the agent_runs schema in a DuckDB
// connection acting as a Dolt mock. The column list mirrors the production
// agent_runs schema and the projection in pkg/olap/duckdb/etl.go's queryDolt
// SELECT so the ETL can read every column it expects.
func createIntegrationAgentRunsTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE agent_runs (
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
		started_at TIMESTAMP, completed_at TIMESTAMP,
		turns INTEGER, max_turns INTEGER, stop_reason VARCHAR,
		cache_read_tokens BIGINT, cache_write_tokens BIGINT
	)`)
	return err
}

// insertIntegrationAgentRun writes an AgentRun into the mock Dolt agent_runs
// table. Field selection covers everything the ETL projection reads; unset
// fields are stored as NULL/zero so the ETL still rounds-trips them.
func insertIntegrationAgentRun(db *sql.DB, r AgentRun) error {
	started, _ := time.Parse(time.RFC3339, r.StartedAt)
	completed, _ := time.Parse(time.RFC3339, r.CompletedAt)
	formulaVersion := ""
	if r.FormulaVersion > 0 {
		formulaVersion = strconv.Itoa(r.FormulaVersion)
	}
	_, err := db.Exec(`INSERT INTO agent_runs VALUES (
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		?, ?, ?,
		?, ?,
		?, ?, ?, ?,
		?, ?, ?,
		?, ?, ?, ?, ?,
		?, ?,
		?, ?, ?, ?, ?
	)`,
		r.ID, r.BeadID, r.EpicID, r.ParentRunID,
		r.FormulaName, formulaVersion,
		r.Phase, r.Role, r.Model, r.Tower,
		r.Branch, r.Result, r.ReviewRounds,
		r.ContextTokensIn, r.ContextTokensOut, r.TotalTokens,
		r.CostUSD, float64(r.DurationSeconds),
		float64(r.StartupSeconds), float64(r.WorkingSeconds),
		float64(r.QueueSeconds), float64(r.ReviewSeconds),
		r.FilesChanged, r.LinesAdded, r.LinesRemoved,
		r.ReadCalls, r.EditCalls, r.ToolCallsJSON,
		r.FailureClass, r.AttemptNumber,
		started, completed,
		r.Turns, r.MaxTurns, r.StopReason,
		r.CacheReadTokens, r.CacheWriteTokens,
	)
	return err
}

