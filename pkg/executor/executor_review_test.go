package executor

import (
	"testing"
	"time"
)

// TestRecordSkipPhase verifies that recordSkipPhase emits an agent_runs row
// with phase='skip', role='wizard', result='success', and the configured
// parent_run_id linkage.
func TestRecordSkipPhase(t *testing.T) {
	var recorded *AgentRun
	deps := &Deps{
		RecordAgentRun: func(run AgentRun) (string, error) {
			recorded = &run
			return "run-id", nil
		},
	}
	e := NewForTest("spi-test", "wizard-test", nil, deps)
	e.currentRunID = "parent-run-id"

	e.recordSkipPhase("spi-test", "spi-epic", "plan-already-exists")

	if recorded == nil {
		t.Fatal("RecordAgentRun was not called")
	}
	if recorded.Phase != "skip" {
		t.Errorf("Phase = %q, want %q", recorded.Phase, "skip")
	}
	if recorded.Role != "wizard" {
		t.Errorf("Role = %q, want %q", recorded.Role, "wizard")
	}
	if recorded.Result != "success" {
		t.Errorf("Result = %q, want %q", recorded.Result, "success")
	}
	if recorded.SkipReason != "plan-already-exists" {
		t.Errorf("SkipReason = %q, want %q", recorded.SkipReason, "plan-already-exists")
	}
	if recorded.ParentRunID != "parent-run-id" {
		t.Errorf("ParentRunID = %q, want %q", recorded.ParentRunID, "parent-run-id")
	}
	if recorded.EpicID != "spi-epic" {
		t.Errorf("EpicID = %q, want %q", recorded.EpicID, "spi-epic")
	}
}

// TestRecordAutoApprove verifies that recordAutoApprove emits an agent_runs
// row with phase='auto-approve', role='wizard', result='success', and the
// configured parent_run_id linkage.
func TestRecordAutoApprove(t *testing.T) {
	var recorded *AgentRun
	deps := &Deps{
		RecordAgentRun: func(run AgentRun) (string, error) {
			recorded = &run
			return "run-id", nil
		},
	}
	e := NewForTest("spi-test", "wizard-test", nil, deps)
	e.currentRunID = "parent-run-id"

	e.recordAutoApprove("spi-test", "")

	if recorded == nil {
		t.Fatal("RecordAgentRun was not called")
	}
	if recorded.Phase != "auto-approve" {
		t.Errorf("Phase = %q, want %q", recorded.Phase, "auto-approve")
	}
	if recorded.Role != "wizard" {
		t.Errorf("Role = %q, want %q", recorded.Role, "wizard")
	}
	if recorded.Result != "success" {
		t.Errorf("Result = %q, want %q", recorded.Result, "success")
	}
	if recorded.ParentRunID != "parent-run-id" {
		t.Errorf("ParentRunID = %q, want %q", recorded.ParentRunID, "parent-run-id")
	}
}

// TestRecordWaitForHuman verifies that recordWaitForHuman emits an
// agent_runs row with phase='waitForHuman', role='wizard', result='success',
// and working_seconds populated with the block duration.
func TestRecordWaitForHuman(t *testing.T) {
	var recorded *AgentRun
	deps := &Deps{
		RecordAgentRun: func(run AgentRun) (string, error) {
			recorded = &run
			return "run-id", nil
		},
	}
	e := NewForTest("spi-test", "wizard-test", nil, deps)
	e.currentRunID = "parent-run-id"

	start := time.Now().Add(-90 * time.Second)
	end := time.Now()
	e.recordWaitForHuman("spi-test", "", start, end)

	if recorded == nil {
		t.Fatal("RecordAgentRun was not called")
	}
	if recorded.Phase != "waitForHuman" {
		t.Errorf("Phase = %q, want %q", recorded.Phase, "waitForHuman")
	}
	if recorded.Role != "wizard" {
		t.Errorf("Role = %q, want %q", recorded.Role, "wizard")
	}
	if recorded.Result != "success" {
		t.Errorf("Result = %q, want %q", recorded.Result, "success")
	}
	if recorded.WorkingSeconds < 89 || recorded.WorkingSeconds > 91 {
		t.Errorf("WorkingSeconds = %d, want ~90", recorded.WorkingSeconds)
	}
	if recorded.ParentRunID != "parent-run-id" {
		t.Errorf("ParentRunID = %q, want %q", recorded.ParentRunID, "parent-run-id")
	}
}

// TestReviewLoopTiming verifies markReviewLoopEntry / reviewLoopSeconds
// produce non-zero durations and are one-shot (reading consumes the entry).
func TestReviewLoopTiming(t *testing.T) {
	e := NewGraphForTest("spi-test", "wizard-test", nil, &GraphState{Vars: map[string]string{}}, &Deps{})

	if secs := e.reviewLoopSeconds(); secs != 0 {
		t.Errorf("reviewLoopSeconds before mark = %v, want 0", secs)
	}

	e.markReviewLoopEntry()
	// Backdate so elapsed is measurable in the test window.
	e.graphState.Vars[reviewLoopEntryVar] = time.Now().Add(-50 * time.Second).UTC().Format(time.RFC3339)

	secs := e.reviewLoopSeconds()
	if secs < 49 || secs > 51 {
		t.Errorf("reviewLoopSeconds = %v, want ~50", secs)
	}

	// Reading consumes the entry — subsequent reads return 0.
	if again := e.reviewLoopSeconds(); again != 0 {
		t.Errorf("reviewLoopSeconds after consumed = %v, want 0", again)
	}
}

// TestRecordReviewPhase verifies recordReviewPhase emits a review row with
// review_seconds populated when there is an entry, and emits nothing when
// there is no entry.
func TestRecordReviewPhase(t *testing.T) {
	var recorded *AgentRun
	deps := &Deps{
		RecordAgentRun: func(run AgentRun) (string, error) {
			recorded = &run
			return "run-id", nil
		},
	}
	e := NewGraphForTest("spi-test", "wizard-test", nil,
		&GraphState{Vars: map[string]string{}}, deps)

	// No entry → no record emitted.
	e.recordReviewPhase("spi-test", "", time.Now())
	if recorded != nil {
		t.Fatalf("unexpected record emitted without entry: %+v", recorded)
	}

	// Mark entry and backdate it, then emit.
	e.markReviewLoopEntry()
	e.graphState.Vars[reviewLoopEntryVar] = time.Now().Add(-120 * time.Second).UTC().Format(time.RFC3339)

	e.recordReviewPhase("spi-test", "", time.Now())
	if recorded == nil {
		t.Fatal("RecordAgentRun was not called after mark")
	}
	if recorded.Phase != "review" {
		t.Errorf("Phase = %q, want %q", recorded.Phase, "review")
	}
	if recorded.Role != "wizard" {
		t.Errorf("Role = %q, want %q", recorded.Role, "wizard")
	}
	if recorded.ReviewSeconds < 119 || recorded.ReviewSeconds > 121 {
		t.Errorf("ReviewSeconds = %d, want ~120", recorded.ReviewSeconds)
	}
}

// TestIsReviewSubgraphStep exercises the predicate that detects the review
// sub-graph step. Both the typed Graph field and the With map override are
// accepted (matching how formulas surface the graph name).
func TestIsReviewSubgraphStep(t *testing.T) {
	tests := []struct {
		name string
		step StepConfig
		want bool
	}{
		{"typed graph field", StepConfig{Action: "graph.run", Graph: "subgraph-review"}, true},
		{"with-map graph key", StepConfig{Action: "graph.run", With: map[string]string{"graph": "subgraph-review"}}, true},
		{"unrelated sub-graph", StepConfig{Action: "graph.run", Graph: "subgraph-implement"}, false},
		{"not a graph.run step", StepConfig{Action: "wizard.run", Flow: "sage-review"}, false},
		{"empty step", StepConfig{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isReviewSubgraphStep(tt.step); got != tt.want {
				t.Errorf("isReviewSubgraphStep = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestWaitForHumanVarKey verifies the key is unique per step name.
func TestWaitForHumanVarKey(t *testing.T) {
	if waitForHumanVarKey("approve-production") == waitForHumanVarKey("approve-staging") {
		t.Error("expected distinct keys for distinct step names")
	}
}
