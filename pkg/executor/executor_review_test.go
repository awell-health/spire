package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/metrics"
)

// reviewTestEnv bundles the executor, dispatch log, and assertions for review walker tests.
type reviewTestEnv struct {
	executor       *Executor
	dispatched     []agent.SpawnConfig
	labels         map[string]bool
	currentVerdict string // latest sage verdict, set by spawnFn — avoids map key accumulation bugs
	arbiterCalled  bool
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
				// Store current verdict in a dedicated field — avoids stale
				// _verdict_value_* label accumulation and Go map iteration flakiness.
				env.currentVerdict = verdict
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
			if env.currentVerdict != "" {
				return []Bead{{
					ID:     "review-round",
					Status: "closed",
					Labels: []string{"verdict:" + env.currentVerdict},
				}}, nil
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

	result, err := env.executor.executeReview("review", formula.PhaseConfig{Role: "sage"})
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

	// Verify GraphResult.
	if result == nil {
		t.Fatal("expected non-nil GraphResult")
	}
	if result.TerminalStep != "merge" {
		t.Errorf("TerminalStep = %q, want %q", result.TerminalStep, "merge")
	}
	if result.Outputs["verdict"] != "approve" {
		t.Errorf("Outputs[verdict] = %q, want %q", result.Outputs["verdict"], "approve")
	}
	if result.GraphName != "review-phase" {
		t.Errorf("GraphName = %q, want %q", result.GraphName, "review-phase")
	}
}

func TestReviewWalker_OneFixThenApprove(t *testing.T) {
	env := setupReviewTest(t, map[int]string{
		0: "request_changes",
		1: "approve",
	}, "")

	result, err := env.executor.executeReview("review", formula.PhaseConfig{Role: "sage"})
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

	if result == nil {
		t.Fatal("expected non-nil GraphResult")
	}
	if result.TerminalStep != "merge" {
		t.Errorf("TerminalStep = %q, want %q", result.TerminalStep, "merge")
	}
}

func TestReviewWalker_MaxRoundsArbiterMerge(t *testing.T) {
	env := setupReviewTest(t, map[int]string{
		0: "request_changes",
		1: "request_changes",
		2: "request_changes",
		3: "request_changes", // sage fires once more after 3rd fix reset; arbiter needs sage-review completed
	}, "merge")

	result, err := env.executor.executeReview("review", formula.PhaseConfig{Role: "sage"})
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

	// Verify GraphResult.
	if result == nil {
		t.Fatal("expected non-nil GraphResult")
	}
	if result.Outputs["arbiter_decision"] != "merge" {
		t.Errorf("Outputs[arbiter_decision] = %q, want %q", result.Outputs["arbiter_decision"], "merge")
	}
	if result.Outputs["rounds_used"] != "3" {
		t.Errorf("Outputs[rounds_used] = %q, want %q", result.Outputs["rounds_used"], "3")
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
	// Round incremented to 1, max_review_rounds=3 => should return sage-review.
	localCompleted := map[string]bool{}
	ctx := map[string]string{
		"verdict":           "request_changes",
		"round":             "1",
		"review_round":      "1",
		"max_review_rounds": "3",
		"max_rounds":        "3",
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

// --- Edge case tests ---

// TestReviewWalker_SageExitError verifies that the walker continues when
// the sage process exits with an error but the verdict is still readable.
func TestReviewWalker_SageExitError(t *testing.T) {
	env := setupReviewTest(t, map[int]string{0: "approve"}, "")

	// Override spawner: sage returns waitErr but verdict labels are set.
	sageCount := 0
	env.executor.deps.Spawner = &mockBackend{
		spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
			env.dispatched = append(env.dispatched, cfg)
			if cfg.Role == agent.RoleSage {
				sageCount++
				delete(env.labels, "review-approved")
				env.labels["review-approved"] = true
				env.currentVerdict = "approve"
				return &mockHandle{waitErr: fmt.Errorf("sage crashed")}, nil
			}
			return &mockHandle{}, nil
		},
	}

	_, err := env.executor.executeReview("review", formula.PhaseConfig{Role: "sage"})
	if err != nil {
		t.Fatalf("executeReview should succeed despite sage exit error: %v", err)
	}

	roles := env.dispatchedRoles()
	if len(roles) != 1 || roles[0] != agent.RoleSage {
		t.Fatalf("expected [sage], got %v", roles)
	}
	if env.executor.state.ReviewRounds != 0 {
		t.Errorf("ReviewRounds = %d, want 0", env.executor.state.ReviewRounds)
	}
}

// TestReviewWalker_SageErrorNoVerdict verifies that the walker returns a
// "review graph stuck" error when the sage crashes and no verdict is available.
func TestReviewWalker_SageErrorNoVerdict(t *testing.T) {
	env := setupReviewTest(t, nil, "")

	// Override spawner: sage crashes, no verdict labels set.
	env.executor.deps.Spawner = &mockBackend{
		spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
			env.dispatched = append(env.dispatched, cfg)
			if cfg.Role == agent.RoleSage {
				// Don't set any verdict labels or currentVerdict.
				return &mockHandle{waitErr: fmt.Errorf("sage crashed")}, nil
			}
			return &mockHandle{}, nil
		},
	}
	// Ensure GetReviewBeads returns nothing (no persisted verdict).
	env.executor.deps.GetReviewBeads = func(parentID string) ([]Bead, error) {
		return nil, nil
	}

	result, err := env.executor.executeReview("review", formula.PhaseConfig{Role: "sage"})
	if err == nil {
		t.Fatal("expected error from executeReview, got nil")
	}
	if !strings.Contains(err.Error(), "review graph stuck") {
		t.Errorf("expected 'review graph stuck' error, got: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result on error, got %+v", result)
	}
}

// TestReviewWalker_FixApprenticeError verifies that the walker continues
// to the next sage round when the fix apprentice exits with an error.
func TestReviewWalker_FixApprenticeError(t *testing.T) {
	env := setupReviewTest(t, map[int]string{
		0: "request_changes",
		1: "approve",
	}, "")

	// Override spawner: apprentice returns waitErr, sage works normally.
	sageCount := 0
	env.executor.deps.Spawner = &mockBackend{
		spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
			env.dispatched = append(env.dispatched, cfg)
			if cfg.Role == agent.RoleSage {
				round := sageCount
				sageCount++
				verdict := "approve"
				if v, ok := map[int]string{0: "request_changes", 1: "approve"}[round]; ok {
					verdict = v
				}
				delete(env.labels, "review-approved")
				if verdict == "approve" {
					env.labels["review-approved"] = true
				}
				env.currentVerdict = verdict
				return &mockHandle{}, nil
			}
			if cfg.Role == agent.RoleApprentice {
				return &mockHandle{waitErr: fmt.Errorf("fix apprentice failed")}, nil
			}
			return &mockHandle{}, nil
		},
	}

	_, err := env.executor.executeReview("review", formula.PhaseConfig{Role: "sage"})
	if err != nil {
		t.Fatalf("executeReview should succeed despite fix error: %v", err)
	}

	// sage (round 0), apprentice (fix, errors), sage (round 1, approves), merge (terminal).
	roles := env.dispatchedRoles()
	expected := []agent.SpawnRole{agent.RoleSage, agent.RoleApprentice, agent.RoleSage}
	if len(roles) != len(expected) {
		t.Fatalf("expected %d dispatches, got %d: %v", len(expected), len(roles), roles)
	}
	for i, want := range expected {
		if roles[i] != want {
			t.Errorf("dispatch[%d] = %v, want %v", i, roles[i], want)
		}
	}
	if env.executor.state.ReviewRounds != 1 {
		t.Errorf("ReviewRounds = %d, want 1", env.executor.state.ReviewRounds)
	}
}

// TestReviewWalker_StaleSubStepBeadsReopened verifies that ensureReviewSubStepBeads
// reopens closed sub-step beads from a prior run via GetChildren reconciliation.
func TestReviewWalker_StaleSubStepBeadsReopened(t *testing.T) {
	env := setupReviewTest(t, map[int]string{0: "approve"}, "")

	// Track bead statuses and activation calls.
	beadStatuses := map[string]string{
		"stale-sage-review": "closed",
		"stale-fix":         "closed",
		"stale-arbiter":     "closed",
		"stale-merge":       "closed",
		"stale-discard":     "closed",
	}
	var activatedBeads []string

	// Override GetChildren to return stale closed children.
	env.executor.deps.GetChildren = func(parentID string) ([]Bead, error) {
		return []Bead{
			{ID: "stale-sage-review", Status: "closed", Labels: []string{"review-substep", "step:sage-review"}},
			{ID: "stale-fix", Status: "closed", Labels: []string{"review-substep", "step:fix"}},
			{ID: "stale-arbiter", Status: "closed", Labels: []string{"review-substep", "step:arbiter"}},
			{ID: "stale-merge", Status: "closed", Labels: []string{"review-substep", "step:merge"}},
			{ID: "stale-discard", Status: "closed", Labels: []string{"review-substep", "step:discard"}},
		}, nil
	}

	// Override GetBead to use beadStatuses for sub-step beads.
	env.executor.deps.GetBead = func(id string) (Bead, error) {
		b := Bead{ID: id, Status: "in_progress"}
		if s, ok := beadStatuses[id]; ok {
			b.Status = s
		}
		// Main bead: include labels from env.labels.
		if id == "test-bead" {
			for l := range env.labels {
				if !strings.HasPrefix(l, "_") {
					b.Labels = append(b.Labels, l)
				}
			}
		}
		return b, nil
	}

	// Track ActivateStepBead calls and update mock status.
	env.executor.deps.ActivateStepBead = func(stepID string) error {
		activatedBeads = append(activatedBeads, stepID)
		beadStatuses[stepID] = "in_progress"
		return nil
	}

	// Start with empty ReviewStepBeadIDs to trigger GetChildren reconciliation.
	env.executor.state.ReviewStepBeadIDs = make(map[string]string)

	_, err := env.executor.executeReview("review", formula.PhaseConfig{Role: "sage"})
	if err != nil {
		t.Fatalf("executeReview returned error: %v", err)
	}

	// Verify all 5 stale beads were reopened during reconciliation.
	staleIDs := []string{"stale-sage-review", "stale-fix", "stale-arbiter", "stale-merge", "stale-discard"}
	for _, id := range staleIDs {
		found := false
		for _, a := range activatedBeads {
			if a == id {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("stale bead %s was not reopened via ActivateStepBead", id)
		}
	}

	// Verify the review completed (sage approve → merge).
	roles := env.dispatchedRoles()
	if len(roles) != 1 || roles[0] != agent.RoleSage {
		t.Fatalf("expected [sage], got %v", roles)
	}
}

// --- Stress test ---

// TestReviewWalker_ThreeRoundsStress verifies 3 full review rounds with
// different verdicts, checking round counter, dispatch sequence, and
// sub-step bead lifecycle at each step.
func TestReviewWalker_ThreeRoundsStress(t *testing.T) {
	env := setupReviewTest(t, map[int]string{
		0: "request_changes",
		1: "request_changes",
		2: "approve",
	}, "")

	// Pre-populate ReviewStepBeadIDs for sub-step lifecycle tracking.
	env.executor.state.ReviewStepBeadIDs = map[string]string{
		"sage-review": "sub-sage",
		"fix":         "sub-fix",
		"arbiter":     "sub-arbiter",
		"merge":       "sub-merge",
		"discard":     "sub-discard",
	}

	var activations, closes []string
	env.executor.deps.ActivateStepBead = func(stepID string) error {
		activations = append(activations, stepID)
		return nil
	}
	env.executor.deps.CloseStepBead = func(stepID string) error {
		closes = append(closes, stepID)
		return nil
	}

	_, err := env.executor.executeReview("review", formula.PhaseConfig{Role: "sage"})
	if err != nil {
		t.Fatalf("executeReview returned error: %v", err)
	}

	// Verify dispatch sequence: sage, fix, sage, fix, sage (then merge terminal).
	roles := env.dispatchedRoles()
	expected := []agent.SpawnRole{
		agent.RoleSage, agent.RoleApprentice,
		agent.RoleSage, agent.RoleApprentice,
		agent.RoleSage,
	}
	if len(roles) != len(expected) {
		t.Fatalf("expected %d dispatches, got %d: %v", len(expected), len(roles), roles)
	}
	for i, want := range expected {
		if roles[i] != want {
			t.Errorf("dispatch[%d] = %v, want %v", i, roles[i], want)
		}
	}

	if env.executor.state.ReviewRounds != 2 {
		t.Errorf("ReviewRounds = %d, want 2", env.executor.state.ReviewRounds)
	}
	if env.arbiterCalled {
		t.Error("arbiter should not be called when final round approves")
	}

	// Verify sub-step bead lifecycle:
	// sage-review: activated 3 times (rounds 0, 1, 2), closed 3 times
	// fix: activated 2 times (rounds 0, 1), closed 2 times
	// merge: activated 1 time (terminal), closed 1 time (after terminal exec)
	if n := countStr(activations, "sub-sage"); n != 3 {
		t.Errorf("sage-review activated %d times, want 3", n)
	}
	if n := countStr(closes, "sub-sage"); n != 3 {
		t.Errorf("sage-review closed %d times, want 3", n)
	}
	if n := countStr(activations, "sub-fix"); n != 2 {
		t.Errorf("fix activated %d times, want 2", n)
	}
	if n := countStr(closes, "sub-fix"); n != 2 {
		t.Errorf("fix closed %d times, want 2", n)
	}
	if n := countStr(activations, "sub-merge"); n != 1 {
		t.Errorf("merge activated %d times, want 1", n)
	}
	if n := countStr(closes, "sub-merge"); n != 1 {
		t.Errorf("merge closed %d times, want 1", n)
	}
}

// --- Resume tests ---

// TestReviewWalker_ResumeAfterSage verifies that a wizard resuming after
// sage-review completed (but before fix) picks up from the correct step.
// Sage should NOT be re-dispatched; fix should be the first dispatch.
func TestReviewWalker_ResumeAfterSage(t *testing.T) {
	env := setupReviewTest(t, map[int]string{
		0: "approve", // first sage dispatch in this test (round 1) approves
	}, "")

	// Pre-populate sub-step beads: sage-review is closed (completed in prior session).
	env.executor.state.ReviewStepBeadIDs = map[string]string{
		"sage-review": "sub-sage",
		"fix":         "sub-fix",
		"arbiter":     "sub-arbiter",
		"merge":       "sub-merge",
		"discard":     "sub-discard",
	}
	beadStatuses := map[string]string{
		"sub-sage": "closed", // sage ran in prior session
	}

	// Override GetBead: sub-step beads use beadStatuses, main bead uses env.labels.
	env.executor.deps.GetBead = func(id string) (Bead, error) {
		if id == "test-bead" {
			b := Bead{ID: id, Status: "in_progress"}
			for l := range env.labels {
				if !strings.HasPrefix(l, "_") {
					b.Labels = append(b.Labels, l)
				}
			}
			return b, nil
		}
		status := "in_progress"
		if s, ok := beadStatuses[id]; ok {
			status = s
		}
		return Bead{ID: id, Status: status}, nil
	}

	// GetReviewBeads returns the prior sage's verdict (request_changes).
	env.executor.deps.GetReviewBeads = func(parentID string) ([]Bead, error) {
		return []Bead{{
			ID:     "prior-review",
			Status: "closed",
			Labels: []string{"verdict:request_changes"},
		}}, nil
	}

	_, err := env.executor.executeReview("review", formula.PhaseConfig{Role: "sage"})
	if err != nil {
		t.Fatalf("executeReview returned error: %v", err)
	}

	// First dispatch should be apprentice (fix), NOT sage.
	// Then sage (round 1, approves), then merge (terminal, no spawn).
	roles := env.dispatchedRoles()
	expected := []agent.SpawnRole{agent.RoleApprentice, agent.RoleSage}
	if len(roles) != len(expected) {
		t.Fatalf("expected %d dispatches, got %d: %v", len(expected), len(roles), roles)
	}
	for i, want := range expected {
		if roles[i] != want {
			t.Errorf("dispatch[%d] = %v, want %v", i, roles[i], want)
		}
	}

	if env.executor.state.ReviewRounds != 1 {
		t.Errorf("ReviewRounds = %d, want 1", env.executor.state.ReviewRounds)
	}
}

// TestReviewWalker_ResumeAfterSageMaxRounds verifies that resuming after sage
// when round >= max_rounds triggers the arbiter instead of fix.
func TestReviewWalker_ResumeAfterSageMaxRounds(t *testing.T) {
	env := setupReviewTest(t, nil, "merge")

	// Set round to 3 (>= max_rounds=3) — arbiter should trigger.
	env.executor.state.ReviewRounds = 3

	// Pre-populate sub-step beads: sage-review is closed.
	env.executor.state.ReviewStepBeadIDs = map[string]string{
		"sage-review": "sub-sage",
		"fix":         "sub-fix",
		"arbiter":     "sub-arbiter",
		"merge":       "sub-merge",
		"discard":     "sub-discard",
	}
	beadStatuses := map[string]string{
		"sub-sage": "closed",
	}

	env.executor.deps.GetBead = func(id string) (Bead, error) {
		if id == "test-bead" {
			b := Bead{ID: id, Status: "in_progress"}
			for l := range env.labels {
				if !strings.HasPrefix(l, "_") {
					b.Labels = append(b.Labels, l)
				}
			}
			return b, nil
		}
		status := "in_progress"
		if s, ok := beadStatuses[id]; ok {
			status = s
		}
		return Bead{ID: id, Status: status}, nil
	}

	// Prior sage verdict: request_changes.
	env.executor.deps.GetReviewBeads = func(parentID string) ([]Bead, error) {
		return []Bead{{
			ID:     "prior-review",
			Status: "closed",
			Labels: []string{"verdict:request_changes"},
		}}, nil
	}

	_, err := env.executor.executeReview("review", formula.PhaseConfig{Role: "sage"})
	if err != nil {
		t.Fatalf("executeReview returned error: %v", err)
	}

	// Sage should NOT be dispatched (already completed).
	// Arbiter fires via ReviewEscalateToArbiter (not spawner), then merge terminal.
	roles := env.dispatchedRoles()
	if len(roles) != 0 {
		t.Fatalf("expected 0 spawner dispatches (arbiter uses dep, not spawner), got %d: %v", len(roles), roles)
	}

	if !env.arbiterCalled {
		t.Error("expected ReviewEscalateToArbiter to be called")
	}

	// Round counter unchanged (no fix step).
	if env.executor.state.ReviewRounds != 3 {
		t.Errorf("ReviewRounds = %d, want 3", env.executor.state.ReviewRounds)
	}
}

// TestReviewWalker_CustomGraphName verifies that executeReview loads a custom
// step-graph formula from disk when PhaseConfig.Graph is set.
func TestReviewWalker_CustomGraphName(t *testing.T) {
	env := setupReviewTest(t, map[int]string{
		0: "approve",
	}, "")

	// Create a custom formula on disk that is a copy of review-phase.
	tmpDir := t.TempDir()
	formulaDir := filepath.Join(tmpDir, "formulas")
	os.MkdirAll(formulaDir, 0755)

	// Load the embedded review-phase formula content and write it as custom-review.
	embeddedGraph, err := formula.LoadReviewPhaseFormula()
	if err != nil {
		t.Fatalf("load embedded review formula: %v", err)
	}
	_ = embeddedGraph // just verify it loads

	// Write a minimal custom step-graph formula.
	customTOML := `name = "custom-review"
description = "Custom review graph for testing"
version = 3

[vars.bead_id]
description = "The bead being reviewed"
required = true

[vars.branch]
description = "Staging branch"
required = true

[vars.max_rounds]
description = "Maximum review rounds"
default = "3"

[steps.sage-review]
description = "Sage reviews staging branch diff"
role = "sage"
title = "Sage review"
timeout = "10m"
model = "claude-opus-4-6"
verdict_only = true

[steps.fix]
description = "Fix apprentice addresses review feedback"
needs = ["sage-review"]
condition = "verdict == request_changes && round < max_rounds"
role = "apprentice"
title = "Fix"
timeout = "15m"

[steps.arbiter]
description = "Arbiter makes final decision"
needs = ["sage-review"]
condition = "verdict == request_changes && round >= max_rounds"
role = "arbiter"
title = "Arbiter"
timeout = "10m"

[steps.merge]
description = "Merge staging to main"
needs = ["sage-review", "arbiter"]
condition = "verdict == approve || arbiter_decision == merge || arbiter_decision == split"
terminal = true
title = "Merge to main"

[steps.discard]
description = "Delete branch without merging"
needs = ["arbiter"]
condition = "arbiter_decision == discard"
terminal = true
title = "Discard branch"

[outputs.outcome]
type = "enum"
values = ["merge", "discard"]
description = "Terminal review outcome"
`
	if err := os.WriteFile(filepath.Join(formulaDir, "custom-review.formula.toml"), []byte(customTOML), 0644); err != nil {
		t.Fatalf("write custom formula: %v", err)
	}

	// Set BEADS_DIR so FindFormula resolves the custom formula.
	t.Setenv("BEADS_DIR", tmpDir)

	result, err := env.executor.executeReview("review", formula.PhaseConfig{
		Role:  "sage",
		Graph: "custom-review",
	})
	if err != nil {
		t.Fatalf("executeReview with custom graph returned error: %v", err)
	}

	if result == nil {
		t.Fatal("expected non-nil GraphResult")
	}
	if result.GraphName != "custom-review" {
		t.Errorf("GraphName = %q, want %q", result.GraphName, "custom-review")
	}
	if result.TerminalStep != "merge" {
		t.Errorf("TerminalStep = %q, want %q", result.TerminalStep, "merge")
	}
}

// TestReviewWalker_GraphResultOutputs verifies that all four output fields
// are populated in the GraphResult after a single-round approve flow.
func TestReviewWalker_GraphResultOutputs(t *testing.T) {
	env := setupReviewTest(t, map[int]string{
		0: "approve",
	}, "")

	result, err := env.executor.executeReview("review", formula.PhaseConfig{Role: "sage"})
	if err != nil {
		t.Fatalf("executeReview returned error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil GraphResult")
	}

	// Verify all four output fields exist.
	for _, key := range []string{"outcome", "verdict", "arbiter_decision", "rounds_used"} {
		if _, ok := result.Outputs[key]; !ok {
			t.Errorf("missing output key %q in GraphResult.Outputs", key)
		}
	}

	if result.Outputs["outcome"] != "merge" {
		t.Errorf("Outputs[outcome] = %q, want %q", result.Outputs["outcome"], "merge")
	}
	if result.Outputs["verdict"] != "approve" {
		t.Errorf("Outputs[verdict] = %q, want %q", result.Outputs["verdict"], "approve")
	}
	// arbiter_decision should be empty (no arbiter invoked).
	if result.Outputs["rounds_used"] != "0" {
		t.Errorf("Outputs[rounds_used] = %q, want %q", result.Outputs["rounds_used"], "0")
	}
}

// countStr counts occurrences of val in slice.
func countStr(slice []string, val string) int {
	n := 0
	for _, s := range slice {
		if s == val {
			n++
		}
	}
	return n
}
