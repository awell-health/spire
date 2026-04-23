package executor

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/formula"
	"github.com/google/uuid"
)

// TestRecordAgentRunParentLinkage is the audit required by task spi-iylbac:
// every recordAgentRun call site inside a wizard-parented code path must
// pass withParentRun(e.currentRunID) so the apprentice/sage row links back
// to the wizard's own run record.
//
// The harness seeds a wizard root run (mirroring graph_interpreter.go:116,
// the sole site that assigns e.currentRunID), then exercises each dispatch
// code path, and asserts the child row's ParentRunID equals the wizard's
// run id and that the run id is a non-empty UUID.
func TestRecordAgentRunParentLinkage(t *testing.T) {
	tests := []struct {
		name string
		// invoke exercises the code path under test. It is called after the
		// harness has seeded e.currentRunID. Implementations may record any
		// number of rows; the harness inspects the tail for matching phase.
		invoke func(t *testing.T, e *Executor)
		// wantPhase narrows the child row the harness looks for (there may
		// be other rows, e.g. the wizard's own root run).
		wantPhase string
		wantRole  string
	}{
		{
			name: "skip phase (executor_review.recordSkipPhase)",
			invoke: func(t *testing.T, e *Executor) {
				e.recordSkipPhase("spi-child", "spi-epic", "plan-already-exists")
			},
			wantPhase: "skip",
			wantRole:  "wizard",
		},
		{
			name: "auto-approve phase (executor_review.recordAutoApprove)",
			invoke: func(t *testing.T, e *Executor) {
				e.recordAutoApprove("spi-child", "spi-epic")
			},
			wantPhase: "auto-approve",
			wantRole:  "wizard",
		},
		{
			name: "waitForHuman phase (executor_review.recordWaitForHuman)",
			invoke: func(t *testing.T, e *Executor) {
				start := time.Now().Add(-30 * time.Second)
				e.recordWaitForHuman("spi-child", "spi-epic", start, time.Now())
			},
			wantPhase: "waitForHuman",
			wantRole:  "wizard",
		},
		{
			name: "review phase (executor_review.recordReviewPhase)",
			invoke: func(t *testing.T, e *Executor) {
				// recordReviewPhase only emits if a review-loop entry was
				// previously marked; seed one so the row is produced.
				e.markReviewLoopEntry()
				e.graphState.Vars[reviewLoopEntryVar] = time.Now().Add(-60 * time.Second).UTC().Format(time.RFC3339)
				e.recordReviewPhase("spi-child", "spi-epic", time.Now())
			},
			wantPhase: "review",
			wantRole:  "wizard",
		},
		{
			name: "apprentice dispatch (graph_actions.wizardRunSpawn)",
			invoke: func(t *testing.T, e *Executor) {
				stepName := "implement"
				step := StepConfig{Action: "wizard.run", Flow: "implement"}
				result := wizardRunSpawn(e, stepName, step, e.graphState, agent.RoleApprentice, []string{"--apprentice"}, nil)
				if result.Error != nil {
					t.Fatalf("wizardRunSpawn: %v", result.Error)
				}
			},
			wantPhase: "implement",
			wantRole:  "apprentice",
		},
		{
			name: "sage dispatch (graph_actions.wizardRunSpawn)",
			invoke: func(t *testing.T, e *Executor) {
				stepName := "sage-review"
				step := StepConfig{Action: "wizard.run", Flow: "sage-review"}
				result := wizardRunSpawn(e, stepName, step, e.graphState, agent.RoleSage, nil, nil)
				if result.Error != nil {
					t.Fatalf("wizardRunSpawn: %v", result.Error)
				}
			},
			wantPhase: "sage-review",
			wantRole:  "sage",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doltDir := t.TempDir()
			t.Setenv("SPIRE_DOLT_DIR", doltDir)

			var recorded []AgentRun
			deps := &Deps{
				// RecordAgentRun returns a fresh UUID per row so the wizard's
				// root run id is a non-empty, unique value — matching the
				// behavior of the production metrics.Recorder.
				RecordAgentRun: func(run AgentRun) (string, error) {
					id := uuid.NewString()
					run.ID = id
					recorded = append(recorded, run)
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
				ConfigDir: func() (string, error) { return t.TempDir(), nil },
			}

			// Build a graph so GraphState.Vars is available for review-loop
			// entry tracking and step-state lookup.
			graph := &formula.FormulaStepGraph{
				Name:    "task-default",
				Version: 3,
				Steps: map[string]formula.StepConfig{
					"implement":   {Action: "wizard.run", Flow: "implement"},
					"sage-review": {Action: "wizard.run", Flow: "sage-review"},
				},
			}
			e := NewGraphForTest("spi-parent", "wizard-test", graph, nil, deps)

			// Seed the wizard's root run, mirroring graph_interpreter.go:116.
			// This is the line that makes e.currentRunID available as
			// ParentRunID for every subsequent child spawn.
			e.currentRunID = e.recordAgentRun(e.agentName, e.beadID, "", "claude-opus-4-7", "wizard", "execute", time.Now(), nil)

			if e.currentRunID == "" {
				t.Fatalf("wizard root run id is empty — harness did not seed currentRunID")
			}
			if _, err := uuid.Parse(e.currentRunID); err != nil {
				t.Fatalf("wizard root run id is not a valid UUID: %v (got %q)", err, e.currentRunID)
			}

			wizardRunID := e.currentRunID
			rootRowCount := len(recorded)

			// Exercise the code path under test.
			tt.invoke(t, e)

			// Find the matching child row emitted by the invocation (ignore
			// the root row recorded during harness setup).
			var child *AgentRun
			for i := rootRowCount; i < len(recorded); i++ {
				if recorded[i].Role == tt.wantRole && recorded[i].Phase == tt.wantPhase {
					child = &recorded[i]
					break
				}
			}
			if child == nil {
				t.Fatalf("no recorded row with role=%q phase=%q; recorded=%s",
					tt.wantRole, tt.wantPhase, summarizeRecorded(recorded[rootRowCount:]))
			}

			if child.ParentRunID == "" {
				t.Errorf("child row has empty ParentRunID — withParentRun(e.currentRunID) not threaded through this call site")
			}
			if child.ParentRunID != wizardRunID {
				t.Errorf("child ParentRunID = %q, want %q (wizard root run id)", child.ParentRunID, wizardRunID)
			}
		})
	}
}

func summarizeRecorded(rows []AgentRun) string {
	if len(rows) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, fmt.Sprintf("role=%s/phase=%s/parent=%s", r.Role, r.Phase, r.ParentRunID))
	}
	return fmt.Sprintf("[%s]", parts)
}
