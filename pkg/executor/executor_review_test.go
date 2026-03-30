package executor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/metrics"
)

// reviewTestEnv bundles the executor, dispatch log, and assertions for review walker tests.
type reviewTestEnv struct {
	executor   *Executor
	dispatched []agent.SpawnConfig
	labels     map[string]bool
	arbiterCalled bool
}

// setupReviewTest creates an executor wired with mock deps for testing executeReview.
// verdicts maps sage dispatch round → verdict ("approve" or "request_changes").
// arbiterDecision is returned when ReviewEscalateToArbiter is called.
func setupReviewTest(t *testing.T, verdicts map[int]string, arbiterDecision string) *reviewTestEnv {
	t.Helper()

	dir := t.TempDir()
	env := &reviewTestEnv{
		labels: make(map[string]bool),
	}

	sageCount := 0 // tracks how many times sage has been dispatched

	backend := &mockBackend{
		spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
			env.dispatched = append(env.dispatched, cfg)

			// After a sage dispatch, set labels to reflect the canned verdict.
			if cfg.Role == agent.RoleSage {
				round := sageCount
				sageCount++
				verdict, ok := verdicts[round]
				if !ok {
					verdict = "approve" // default to approve
				}
				// Clear previous verdict labels before setting new ones.
				delete(env.labels, "review-approved")
				if verdict == "approve" {
					env.labels["review-approved"] = true
				}
				// Store current verdict for GetReviewBeads.
				env.labels["_current_verdict"] = true
				env.labels["_verdict_value_"+verdict] = true
			}
			return &mockHandle{}, nil
		},
	}

	stepBeadCounter := 0
	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetBead: func(id string) (Bead, error) {
			b := Bead{ID: id, Status: "in_progress"}
			// Build labels from env.labels.
			for l := range env.labels {
				if !strings.HasPrefix(l, "_") {
					b.Labels = append(b.Labels, l)
				}
			}
			return b, nil
		},
		GetChildren: func(parentID string) ([]Bead, error) {
			return nil, nil
		},
		GetReviewBeads: func(parentID string) ([]Bead, error) {
			// Return a closed review bead with the latest verdict.
			for k := range env.labels {
				if strings.HasPrefix(k, "_verdict_value_") {
					verdict := strings.TrimPrefix(k, "_verdict_value_")
					return []Bead{{
						ID:     "review-round",
						Status: "closed",
						Labels: []string{"verdict:" + verdict},
					}}, nil
				}
			}
			return nil, nil
		},
		ContainsLabel: func(b Bead, label string) bool {
			for _, l := range b.Labels {
				if l == label {
					return true
				}
			}
			return false
		},
		HasLabel: func(b Bead, prefix string) string {
			for _, l := range b.Labels {
				if strings.HasPrefix(l, prefix) {
					return strings.TrimPrefix(l, prefix)
				}
			}
			return ""
		},
		CreateStepBead: func(parentID, stepName string) (string, error) {
			stepBeadCounter++
			return fmt.Sprintf("step-%s-%d", stepName, stepBeadCounter), nil
		},
		ActivateStepBead: func(stepID string) error { return nil },
		CloseStepBead:    func(stepID string) error { return nil },
		UpdateBead: func(id string, updates map[string]interface{}) error {
			return nil
		},
		AddComment: func(id, text string) error { return nil },
		AddLabel:   func(id, label string) error { return nil },
		ReviewEscalateToArbiter: func(beadID, reviewerName string, lastReview *Review, policy RevisionPolicy, log func(string, ...interface{})) error {
			env.arbiterCalled = true
			// Set arbiter decision labels.
			env.labels["arbiter-decision:"+arbiterDecision] = true
			if arbiterDecision == "merge" || arbiterDecision == "split" {
				env.labels["review-approved"] = true
			}
			return nil
		},
		ReviewBeadVerdict: func(b Bead) string {
			for _, l := range b.Labels {
				if strings.HasPrefix(l, "verdict:") {
					return strings.TrimPrefix(l, "verdict:")
				}
			}
			return ""
		},
		RecordAgentRun: func(run metrics.AgentRun) error { return nil },
		Spawner:        backend,
	}

	f := &formula.FormulaV2{
		Name:    "test-review",
		Version: 2,
		Phases: map[string]formula.PhaseConfig{
			"implement": {Role: "apprentice"},
			"review":    {Role: "sage"},
			"merge":     {Role: "skip", Behavior: "skip"},
		},
	}

	state := &State{
		BeadID:            "test-bead",
		AgentName:         "wizard-test",
		Phase:             "review",
		Subtasks:          make(map[string]SubtaskState),
		ReviewRounds:      0,
		ReviewStepBeadIDs: make(map[string]string),
	}

	env.executor = NewForTest("test-bead", "wizard-test", f, state, deps)
	return env
}

// dispatchedRoles returns the SpawnRole of each dispatched spawn config.
func (env *reviewTestEnv) dispatchedRoles() []agent.SpawnRole {
	var roles []agent.SpawnRole
	for _, d := range env.dispatched {
		roles = append(roles, d.Role)
	}
	return roles
}

func TestReviewWalker_SingleRoundApprove(t *testing.T) {
	env := setupReviewTest(t, map[int]string{
		0: "approve",
	}, "")

	err := env.executor.executeReview("review", formula.PhaseConfig{Role: "sage"})
	if err != nil {
		t.Fatalf("executeReview returned error: %v", err)
	}

	// Expect: sage dispatched once, then merge (terminal, no spawn).
	roles := env.dispatchedRoles()
	if len(roles) != 1 {
		t.Fatalf("expected 1 dispatch, got %d: %v", len(roles), roles)
	}
	if roles[0] != agent.RoleSage {
		t.Errorf("dispatch[0] role = %v, want sage", roles[0])
	}

	if env.executor.state.ReviewRounds != 0 {
		t.Errorf("ReviewRounds = %d, want 0", env.executor.state.ReviewRounds)
	}
}

func TestReviewWalker_OneFixThenApprove(t *testing.T) {
	env := setupReviewTest(t, map[int]string{
		0: "request_changes",
		1: "approve",
	}, "")

	err := env.executor.executeReview("review", formula.PhaseConfig{Role: "sage"})
	if err != nil {
		t.Fatalf("executeReview returned error: %v", err)
	}

	// Expect: sage (round 0), apprentice (fix), sage (round 1), then merge terminal.
	roles := env.dispatchedRoles()
	expected := []agent.SpawnRole{agent.RoleSage, agent.RoleApprentice, agent.RoleSage}
	if len(roles) != len(expected) {
		t.Fatalf("expected %d dispatches, got %d: %v", len(expected), len(roles), roles)
	}
	for i, want := range expected {
		if roles[i] != want {
			t.Errorf("dispatch[%d] role = %v, want %v", i, roles[i], want)
		}
	}

	if env.executor.state.ReviewRounds != 1 {
		t.Errorf("ReviewRounds = %d, want 1", env.executor.state.ReviewRounds)
	}
}

func TestReviewWalker_MaxRoundsArbiterMerge(t *testing.T) {
	env := setupReviewTest(t, map[int]string{
		0: "request_changes",
		1: "request_changes",
		2: "request_changes",
		3: "request_changes", // sage fires once more after 3rd fix reset; arbiter needs sage-review completed
	}, "merge")

	err := env.executor.executeReview("review", formula.PhaseConfig{Role: "sage"})
	if err != nil {
		t.Fatalf("executeReview returned error: %v", err)
	}

	// After each fix, localCompleted is cleared, so sage-review fires again.
	// On round 3 (post-3rd fix), sage completes → arbiter condition (round >= max_rounds) fires.
	// Arbiter goes through ReviewEscalateToArbiter (not spawner), then merge terminal.
	roles := env.dispatchedRoles()
	expected := []agent.SpawnRole{
		agent.RoleSage, agent.RoleApprentice, // round 0
		agent.RoleSage, agent.RoleApprentice, // round 1
		agent.RoleSage, agent.RoleApprentice, // round 2
		agent.RoleSage, // round 3: sage fires, then arbiter (no spawn)
	}
	if len(roles) != len(expected) {
		t.Fatalf("expected %d dispatches, got %d: %v", len(expected), len(roles), roles)
	}
	for i, want := range expected {
		if roles[i] != want {
			t.Errorf("dispatch[%d] role = %v, want %v", i, roles[i], want)
		}
	}

	if !env.arbiterCalled {
		t.Error("expected ReviewEscalateToArbiter to be called")
	}

	if env.executor.state.ReviewRounds != 3 {
		t.Errorf("ReviewRounds = %d, want 3", env.executor.state.ReviewRounds)
	}
}

func TestReviewWalker_FixResetCorrectness(t *testing.T) {
	// This test verifies the fix reset logic directly: after completing sage-review
	// and fix, deleting both from localCompleted should make NextSteps return sage-review.
	graph, err := formula.LoadReviewPhaseFormula()
	if err != nil {
		t.Fatalf("load review formula: %v", err)
	}

	// Simulate: sage-review completed with request_changes, fix completed, both cleared.
	// Round incremented to 1, max_rounds=3 => should return sage-review.
	localCompleted := map[string]bool{}
	ctx := map[string]string{
		"verdict":    "request_changes",
		"round":      "1",
		"max_rounds": "3",
	}

	next, err := formula.NextSteps(graph, localCompleted, ctx)
	if err != nil {
		t.Fatalf("NextSteps error: %v", err)
	}

	// sage-review has no needs (entry point), no condition — always ready.
	found := false
	for _, step := range next {
		if step == "sage-review" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("after fix reset, NextSteps = %v, want sage-review in results", next)
	}

	// Also verify fix is NOT ready (needs sage-review completed).
	for _, step := range next {
		if step == "fix" {
			t.Error("fix should not be ready before sage-review is completed")
		}
	}
}
