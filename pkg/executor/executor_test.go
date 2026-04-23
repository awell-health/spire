package executor

import (
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/steveyegge/beads"
)

// TestLoadStateNilWhenMissing verifies LoadState returns nil when no state file exists.
func TestLoadStateNilWhenMissing(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	state, err := LoadState("wizard-spi-abc", configDirFn)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if state != nil {
		t.Fatalf("expected nil state for missing file, got %+v", state)
	}
}

// TestStatePathIsolation verifies different agents get different paths.
func TestStatePathIsolation(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	path1 := StatePath("wizard-spi-aaa", configDirFn)
	path2 := StatePath("wizard-spi-bbb", configDirFn)

	if path1 == path2 {
		t.Errorf("expected different paths, both got %q", path1)
	}
}

// TestSaveAndLoadState verifies round-trip state persistence.
func TestSaveAndLoadState(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	deps := &Deps{ConfigDir: configDirFn}
	state := &State{
		BeadID:    "spi-xyz",
		AgentName: "wizard-spi-xyz",
		Formula:   "spire-agent-work",
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}

	e := NewForTest("spi-xyz", "wizard-spi-xyz", state, deps)
	if err := e.saveState(); err != nil {
		t.Fatalf("saveState error: %v", err)
	}

	loaded, err := LoadState("wizard-spi-xyz", configDirFn)
	if err != nil {
		t.Fatalf("LoadState error: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected non-nil state after save")
	}
	if loaded.BeadID != state.BeadID {
		t.Errorf("BeadID = %q, want %q", loaded.BeadID, state.BeadID)
	}
}

// TestEnsureStepBeadsReconcileFromGraph verifies reconciliation from existing graph.
func TestEnsureStepBeadsReconcileFromGraph(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	existingSteps := []Bead{
		{ID: "spi-parent.1", Labels: []string{"workflow-step", "step:implement"}, Status: "in_progress"},
		{ID: "spi-parent.2", Labels: []string{"workflow-step", "step:review"}, Status: "open"},
		{ID: "spi-parent.3", Labels: []string{"workflow-step", "step:merge"}, Status: "open"},
	}

	stepCreatorCalled := false
	deps := &Deps{
		ConfigDir: configDirFn,
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id}, nil
		},
		GetChildren: func(parentID string) ([]Bead, error) {
			return existingSteps, nil
		},
		CreateStepBead: func(parentID, stepName string) (string, error) {
			stepCreatorCalled = true
			return "spi-new-" + stepName, nil
		},
		ActivateStepBead: func(stepID string) error { return nil },
		CloseStepBead:    func(stepID string) error { return nil },
		HasLabel: func(b Bead, prefix string) string {
			for _, l := range b.Labels {
				if len(l) > len(prefix) && l[:len(prefix)] == prefix {
					return l[len(prefix):]
				}
			}
			return ""
		},
		ContainsLabel: func(b Bead, label string) bool {
			for _, l := range b.Labels {
				if l == label {
					return true
				}
			}
			return false
		},
	}

	state := &State{
		BeadID:    "spi-parent",
		AgentName: "wizard-test",

	}

	e := NewForTest("spi-parent", "wizard-test", state, deps)

	if err := e.ensureStepBeads(); err != nil {
		t.Fatalf("ensureStepBeads error: %v", err)
	}

	if stepCreatorCalled {
		t.Error("stepCreator was called — reconciliation should have prevented new creation")
	}

	if len(e.state.StepBeadIDs) != 3 {
		t.Errorf("StepBeadIDs has %d entries, want 3", len(e.state.StepBeadIDs))
	}
	if got := e.state.StepBeadIDs["implement"]; got != "spi-parent.1" {
		t.Errorf("implement step bead ID = %q, want spi-parent.1", got)
	}
}

// TestTransitionStepBead verifies phase transition closes/activates step beads.
func TestTransitionStepBead(t *testing.T) {
	var closed []string
	var activated []string

	// Step beads: design is in_progress (about to close), implement and review are open.
	stepStatuses := map[string]string{
		"spi-trans.step-1": "in_progress",
		"spi-trans.step-2": "open",
		"spi-trans.step-3": "open",
	}

	deps := &Deps{
		ConfigDir: func() (string, error) { return t.TempDir(), nil },
		GetBead: func(id string) (Bead, error) {
			status := stepStatuses[id]
			if status == "" {
				status = "open"
			}
			return Bead{ID: id, Status: status}, nil
		},
		CloseStepBead: func(stepID string) error {
			closed = append(closed, stepID)
			return nil
		},
		ActivateStepBead: func(stepID string) error {
			activated = append(activated, stepID)
			return nil
		},
	}

	state := &State{
		StepBeadIDs: map[string]string{
			"design":    "spi-trans.step-1",
			"implement": "spi-trans.step-2",
			"review":    "spi-trans.step-3",
		},
	}

	e := NewForTest("spi-trans", "wizard-trans", state, deps)

	e.transitionStepBead("design", "implement")

	if len(closed) != 1 || closed[0] != "spi-trans.step-1" {
		t.Errorf("closed = %v, want [spi-trans.step-1]", closed)
	}
	if len(activated) != 1 || activated[0] != "spi-trans.step-2" {
		t.Errorf("activated = %v, want [spi-trans.step-2]", activated)
	}
}

// TestTransitionStepBead_NoStepBeads is a no-op for legacy runs.
func TestTransitionStepBead_NoStepBeads(t *testing.T) {
	called := false
	deps := &Deps{
		ConfigDir: func() (string, error) { return t.TempDir(), nil },
		CloseStepBead: func(stepID string) error {
			called = true
			return nil
		},
		ActivateStepBead: func(stepID string) error {
			called = true
			return nil
		},
	}

	e := NewForTest("spi-noop", "wizard", &State{}, deps)
	e.transitionStepBead("design", "implement")

	if called {
		t.Error("step operations should not be called when no step beads exist")
	}
}

// TestEnsureAttemptBead_CreatesNew verifies attempt bead creation.
func TestEnsureAttemptBead_CreatesNew(t *testing.T) {
	dir := t.TempDir()
	var createdID string

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: "in_progress"}, nil
		},
		GetActiveAttempt: func(parentID string) (*Bead, error) {
			return nil, nil
		},
		CreateAttemptBead: func(parentID, agentName, model, branch string) (string, error) {
			createdID = parentID + ".attempt-1"
			return createdID, nil
		},
		CloseAttemptBead: func(attemptID, result string) error { return nil },
		HasLabel: func(b Bead, prefix string) string { return "" },
		AddLabel: func(id, label string) error { return nil },
	}

	state := &State{
		BeadID:        "spi-test",
		AgentName:     "wizard-test",
		StagingBranch: "feat/spi-test",
	}

	e := NewForTest("spi-test", "wizard-test", state, deps)
	if err := e.ensureAttemptBead(); err != nil {
		t.Fatalf("ensureAttemptBead: %v", err)
	}

	if e.state.AttemptBeadID != createdID {
		t.Errorf("AttemptBeadID = %q, want %q", e.state.AttemptBeadID, createdID)
	}
}

// TestEnsureAttemptBead_RejectsActiveAttemptFromDifferentAgent guards
// the concurrency invariant in the task-keyed dispatch world: if a live
// attempt bead owned by a different agent exists for the task, the
// wizard MUST return an error instead of stomping it. The workload_intents
// PK prevents two intents with the same (task_id, seq), but an operator
// that re-creates a wizard pod under the same dispatch_seq must still
// reject on the in-pod side.
func TestEnsureAttemptBead_RejectsActiveAttemptFromDifferentAgent(t *testing.T) {
	dir := t.TempDir()

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: "in_progress"}, nil
		},
		GetActiveAttempt: func(parentID string) (*Bead, error) {
			return &Bead{ID: parentID + ".attempt-alien", Status: "in_progress"}, nil
		},
		HasLabel: func(b Bead, prefix string) string {
			if prefix == "agent:" {
				return "wizard-other"
			}
			return ""
		},
		CreateAttemptBead: func(parentID, agentName, model, branch string) (string, error) {
			t.Fatal("CreateAttemptBead should not be called when a foreign agent owns the attempt")
			return "", nil
		},
	}

	state := &State{BeadID: "spi-conflict", AgentName: "wizard-me"}
	e := NewForTest("spi-conflict", "wizard-me", state, deps)
	err := e.ensureAttemptBead()
	if err == nil {
		t.Fatal("ensureAttemptBead should error when attempt is owned by a different agent")
	}
}

// TestEnsureAttemptBead_CreatesAfterClosedAttempt confirms the
// retry-after-wizard-pod-death branch: prior closed attempts must not
// block a fresh attempt from being created. GetActiveAttempt returns
// nil (only open/in_progress attempts count), and CreateAttemptBead is
// called to mint a new one.
func TestEnsureAttemptBead_CreatesAfterClosedAttempt(t *testing.T) {
	dir := t.TempDir()
	var createdID string

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetBead: func(id string) (Bead, error) {
			// Persisted state points at a previous attempt that's now closed.
			return Bead{ID: id, Status: "closed"}, nil
		},
		GetActiveAttempt: func(parentID string) (*Bead, error) {
			// No active attempt; the prior one is closed.
			return nil, nil
		},
		CreateAttemptBead: func(parentID, agentName, model, branch string) (string, error) {
			createdID = parentID + ".attempt-2"
			return createdID, nil
		},
		HasLabel: func(b Bead, prefix string) string { return "" },
		AddLabel: func(id, label string) error { return nil },
	}

	state := &State{
		BeadID:        "spi-retry",
		AgentName:     "wizard-retry",
		StagingBranch: "feat/spi-retry",
		AttemptBeadID: "spi-retry.attempt-1", // stale reference to closed attempt
	}
	e := NewForTest("spi-retry", "wizard-retry", state, deps)
	if err := e.ensureAttemptBead(); err != nil {
		t.Fatalf("ensureAttemptBead: %v", err)
	}
	if e.state.AttemptBeadID != createdID {
		t.Errorf("AttemptBeadID = %q, want fresh %q", e.state.AttemptBeadID, createdID)
	}
}

// TestEnsureAttemptBead_ReusesSameAgentAttempt covers the claim-then-
// execute path: cmdClaim opened an attempt for this agent; the executor
// picks it up on first invocation rather than creating a second one.
func TestEnsureAttemptBead_ReusesSameAgentAttempt(t *testing.T) {
	dir := t.TempDir()

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: "in_progress"}, nil
		},
		GetActiveAttempt: func(parentID string) (*Bead, error) {
			return &Bead{ID: parentID + ".attempt-claim", Status: "in_progress"}, nil
		},
		HasLabel: func(b Bead, prefix string) string {
			if prefix == "agent:" {
				return "wizard-me"
			}
			return ""
		},
		CreateAttemptBead: func(parentID, agentName, model, branch string) (string, error) {
			t.Fatal("CreateAttemptBead should not be called when a same-agent attempt is active")
			return "", nil
		},
		AddLabel: func(id, label string) error { return nil },
	}

	state := &State{BeadID: "spi-reuse", AgentName: "wizard-me"}
	e := NewForTest("spi-reuse", "wizard-me", state, deps)
	if err := e.ensureAttemptBead(); err != nil {
		t.Fatalf("ensureAttemptBead: %v", err)
	}
	if e.state.AttemptBeadID != "spi-reuse.attempt-claim" {
		t.Errorf("AttemptBeadID = %q, want spi-reuse.attempt-claim", e.state.AttemptBeadID)
	}
}

// TestEnsureAttemptBead_ResumesExisting verifies resuming an existing attempt.
func TestEnsureAttemptBead_ResumesExisting(t *testing.T) {
	dir := t.TempDir()
	creatorCalled := false

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: "in_progress"}, nil
		},
		GetActiveAttempt: func(parentID string) (*Bead, error) {
			return nil, nil
		},
		CreateAttemptBead: func(parentID, agentName, model, branch string) (string, error) {
			creatorCalled = true
			return "", nil
		},
		CloseAttemptBead: func(attemptID, result string) error { return nil },
		HasLabel:         func(b Bead, prefix string) string { return "" },
		RemoveLabel:      func(id, label string) error { return nil },
		AddLabel:         func(id, label string) error { return nil },
	}

	state := &State{
		BeadID:        "spi-resume",
		AgentName:     "wizard-resume",
		AttemptBeadID: "spi-resume.attempt-1",
	}

	e := NewForTest("spi-resume", "wizard-resume", state, deps)
	if err := e.ensureAttemptBead(); err != nil {
		t.Fatalf("ensureAttemptBead: %v", err)
	}

	if creatorCalled {
		t.Error("creator should not be called when resuming")
	}
	if e.state.AttemptBeadID != "spi-resume.attempt-1" {
		t.Errorf("AttemptBeadID = %q, want spi-resume.attempt-1", e.state.AttemptBeadID)
	}
}

// TestCloseAttempt verifies attempt closing and state cleanup.
func TestCloseAttempt(t *testing.T) {
	var closedID, closedResult string

	deps := &Deps{
		ConfigDir: func() (string, error) { return t.TempDir(), nil },
		CloseAttemptBead: func(attemptID, result string) error {
			closedID = attemptID
			closedResult = result
			return nil
		},
	}

	state := &State{AttemptBeadID: "spi-close.attempt-1"}
	e := NewForTest("spi-close", "wizard", state, deps)

	e.closeAttempt("success: merged")

	if closedID != "spi-close.attempt-1" {
		t.Errorf("closedID = %q, want spi-close.attempt-1", closedID)
	}
	if closedResult != "success: merged" {
		t.Errorf("closedResult = %q, want 'success: merged'", closedResult)
	}
	if e.state.AttemptBeadID != "" {
		t.Errorf("AttemptBeadID should be cleared, got %q", e.state.AttemptBeadID)
	}
}

// TestCloseAttempt_Noop verifies no-op when no attempt exists.
func TestCloseAttempt_Noop(t *testing.T) {
	called := false
	deps := &Deps{
		ConfigDir: func() (string, error) { return t.TempDir(), nil },
		CloseAttemptBead: func(attemptID, result string) error {
			called = true
			return nil
		},
	}

	state := &State{AttemptBeadID: ""}
	e := NewForTest("spi-noop", "wizard", state, deps)
	e.closeAttempt("should not fire")

	if called {
		t.Error("closer should not be called when AttemptBeadID is empty")
	}
}

// TestEnrichSubtasksWithChangeSpecs verifies enrichment flow.
func TestEnrichSubtasksWithChangeSpecs(t *testing.T) {
	comments := map[string][]string{}
	callCount := 0

	deps := &Deps{
		ConfigDir: func() (string, error) { return t.TempDir(), nil },
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Title: "Test epic"}, nil
		},
		GetComments: func(id string) ([]*beads.Comment, error) {
			var out []*beads.Comment
			for _, text := range comments[id] {
				out = append(out, &beads.Comment{Text: text})
			}
			return out, nil
		},
		AddComment: func(id, text string) error {
			comments[id] = append(comments[id], text)
			return nil
		},
		ClaudeRunner: func(args []string, dir string, _ io.Writer) ([]byte, error) {
			callCount++
			return []byte("**Change spec: fake**\n\n**Files to modify:**\n- foo.go"), nil
		},
		IsAttemptBead:     func(b Bead) bool { return false },
		IsStepBead:        func(b Bead) bool { return false },
		IsReviewRoundBead: func(b Bead) bool { return false },
	}

	state := &State{RepoPath: t.TempDir()}
	e := NewForTest("spi-enrich", "wizard", state, deps)

	children := []Bead{
		{ID: "spi-enrich.1", Title: "Subtask one"},
		{ID: "spi-enrich.2", Title: "Subtask two"},
	}

	if err := e.enrichSubtasksWithChangeSpecs(children, "", "", "claude-opus-4-6", 0); err != nil {
		t.Fatalf("enrichSubtasksWithChangeSpecs: %v", err)
	}

	if callCount != 2 {
		t.Errorf("claudeRunner called %d times, want 2", callCount)
	}

	// Each subtask should have a change spec comment
	for _, child := range children {
		found := false
		for _, c := range comments[child.ID] {
			if len(c) > 12 && c[:12] == "Change spec:" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("subtask %s missing change spec comment", child.ID)
		}
	}
}

// TestEnrichSkipsAlreadyEnriched verifies skip behavior.
func TestEnrichSkipsAlreadyEnriched(t *testing.T) {
	comments := map[string][]string{
		"spi-skip.1": {"Change spec:\n\nalready present"},
	}
	callCount := 0

	deps := &Deps{
		ConfigDir: func() (string, error) { return t.TempDir(), nil },
		GetComments: func(id string) ([]*beads.Comment, error) {
			var out []*beads.Comment
			for _, text := range comments[id] {
				out = append(out, &beads.Comment{Text: text})
			}
			return out, nil
		},
		AddComment: func(id, text string) error {
			comments[id] = append(comments[id], text)
			return nil
		},
		ClaudeRunner: func(args []string, dir string, _ io.Writer) ([]byte, error) {
			callCount++
			return []byte("**Change spec: new**"), nil
		},
		IsAttemptBead:     func(b Bead) bool { return false },
		IsStepBead:        func(b Bead) bool { return false },
		IsReviewRoundBead: func(b Bead) bool { return false },
	}

	state := &State{RepoPath: t.TempDir()}
	e := NewForTest("spi-skip", "wizard", state, deps)

	children := []Bead{
		{ID: "spi-skip.1", Title: "Already done"},
		{ID: "spi-skip.2", Title: "Needs spec"},
	}

	if err := e.enrichSubtasksWithChangeSpecs(children, "", "", "claude-opus-4-6", 0); err != nil {
		t.Fatalf("enrichSubtasksWithChangeSpecs: %v", err)
	}

	if callCount != 1 {
		t.Errorf("claudeRunner called %d times, want 1", callCount)
	}
}

// TestComputeWaves verifies wave computation.
func TestComputeWaves(t *testing.T) {
	deps := &Deps{
		GetChildren: func(parentID string) ([]Bead, error) {
			return []Bead{
				{ID: "task-1", Status: "open"},
				{ID: "task-2", Status: "open"},
				{ID: "task-3", Status: "closed"}, // should be excluded
			}, nil
		},
		GetBlockedIssues: func(filter beads.WorkFilter) ([]BoardBead, error) {
			return nil, nil // no deps = all in wave 0
		},
		IsAttemptBead:     func(b Bead) bool { return false },
		IsStepBead:        func(b Bead) bool { return false },
		IsReviewRoundBead: func(b Bead) bool { return false },
	}

	waves, err := ComputeWaves("epic-1", deps)
	if err != nil {
		t.Fatalf("ComputeWaves: %v", err)
	}

	if len(waves) != 1 {
		t.Fatalf("expected 1 wave, got %d", len(waves))
	}
	if len(waves[0]) != 2 {
		t.Errorf("wave 0 has %d tasks, want 2", len(waves[0]))
	}
}

// TestWizardPlanSkipsInternalDAGBeads verifies that wizardPlan does NOT
// short-circuit into enrichSubtasksWithChangeSpecs when the only children
// are internal DAG beads (step, attempt, review-round). This is the fix
// for spi-xjcqs: ensureStepBeads/ensureAttemptBead create these children
// BEFORE the plan phase runs, so without filtering, planning is always
// skipped.
func TestWizardPlanSkipsInternalDAGBeads(t *testing.T) {
	planCalled := false
	enrichCalled := false

	deps := &Deps{
		ConfigDir: func() (string, error) { return t.TempDir(), nil },
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Title: "Test epic", Priority: 1}, nil
		},
		GetDepsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			return nil, nil
		},
		GetComments: func(id string) ([]*beads.Comment, error) {
			return nil, nil
		},
		GetChildren: func(parentID string) ([]Bead, error) {
			// Return only internal DAG beads — the kind created by
			// ensureStepBeads and ensureAttemptBead before plan runs.
			return []Bead{
				{ID: parentID + ".step-design", Title: "design"},
				{ID: parentID + ".step-plan", Title: "plan"},
				{ID: parentID + ".step-implement", Title: "implement"},
				{ID: parentID + ".step-review", Title: "review"},
				{ID: parentID + ".step-merge", Title: "merge"},
				{ID: parentID + ".attempt-1", Title: "attempt-1"},
			}, nil
		},
		AddComment: func(id, text string) error { return nil },
		CreateBead: func(opts CreateOpts) (string, error) {
			return "spi-plan-child.1", nil
		},
		AddDep: func(issueID, depID string) error { return nil },
		ClaudeRunner: func(args []string, dir string, _ io.Writer) ([]byte, error) {
			// If Claude is invoked, planning was attempted (not skipped).
			planCalled = true
			return []byte(`{"title": "Subtask 1", "description": "Do the thing", "deps": [], "shared_files": [], "do_not_touch": []}`), nil
		},
		IsAttemptBead: func(b Bead) bool {
			return b.Title == "attempt-1"
		},
		IsStepBead: func(b Bead) bool {
			switch b.Title {
			case "design", "plan", "implement", "review", "merge":
				return true
			}
			return false
		},
		IsReviewRoundBead: func(b Bead) bool { return false },
		ParseIssueType: func(s string) beads.IssueType {
			return beads.IssueType(s)
		},
	}

	state := &State{RepoPath: t.TempDir()}
	e := NewForTest("spi-plan-dag", "wizard", state, deps)

	bead, _ := deps.GetBead("spi-plan-dag")
	err := e.wizardPlanEpic(bead, "claude-sonnet-4-6", 0)
	if err != nil {
		t.Fatalf("wizardPlanEpic: %v", err)
	}

	if !planCalled {
		t.Error("wizardPlan did not invoke Claude for planning — internal DAG beads were not filtered out")
	}
	_ = enrichCalled // enrichCalled would only be true if we had real children
}

// TestWizardPlanEnrichesRealChildren verifies that when real subtask
// children exist alongside internal DAG beads, wizardPlan correctly
// routes to enrichSubtasksWithChangeSpecs with only the real children.
func TestWizardPlanEnrichesRealChildren(t *testing.T) {
	enrichCalls := 0

	deps := &Deps{
		ConfigDir: func() (string, error) { return t.TempDir(), nil },
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Title: "Test epic", Priority: 1, Type: "epic"}, nil
		},
		GetDepsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			return nil, nil
		},
		GetComments: func(id string) ([]*beads.Comment, error) {
			return nil, nil
		},
		GetChildren: func(parentID string) ([]Bead, error) {
			// Mix of internal DAG beads and real subtask children.
			return []Bead{
				{ID: parentID + ".step-design", Title: "design"},
				{ID: parentID + ".step-plan", Title: "plan"},
				{ID: parentID + ".1", Title: "Real subtask A"},
				{ID: parentID + ".2", Title: "Real subtask B"},
				{ID: parentID + ".attempt-1", Title: "attempt-1"},
			}, nil
		},
		AddComment: func(id, text string) error { return nil },
		ClaudeRunner: func(args []string, dir string, _ io.Writer) ([]byte, error) {
			enrichCalls++
			return []byte("**Change spec: test**"), nil
		},
		IsAttemptBead: func(b Bead) bool {
			return b.Title == "attempt-1"
		},
		IsStepBead: func(b Bead) bool {
			switch b.Title {
			case "design", "plan", "implement", "review", "merge":
				return true
			}
			return false
		},
		IsReviewRoundBead: func(b Bead) bool { return false },
	}

	state := &State{RepoPath: t.TempDir()}
	e := NewForTest("spi-plan-mix", "wizard", state, deps)

	bead, _ := deps.GetBead("spi-plan-mix")
	err := e.wizardPlanEpic(bead, "claude-opus-4-6", 0)
	if err != nil {
		t.Fatalf("wizardPlanEpic: %v", err)
	}

	// enrichSubtasksWithChangeSpecs should be called once per real child
	if enrichCalls != 2 {
		t.Errorf("enrichSubtasksWithChangeSpecs invoked Claude %d times, want 2 (one per real subtask)", enrichCalls)
	}
}

// TestSaveStateRemovesWhenTerminated verifies that saveState removes state.json
// instead of writing it when the executor's terminated flag is set.
func TestSaveStateRemovesWhenTerminated(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	deps := &Deps{ConfigDir: configDirFn}
	state := &State{
		BeadID:    "spi-term",
		AgentName: "wizard-spi-term",
		Formula:   "test",
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}

	e := NewForTest("spi-term", "wizard-spi-term", state, deps)

	// First, save state normally — file should exist.
	if err := e.saveState(); err != nil {
		t.Fatalf("saveState (normal): %v", err)
	}
	path := StatePath("wizard-spi-term", configDirFn)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatal("state.json should exist after normal save")
	}

	// Now set terminated and save again — file should be removed.
	e.terminated = true
	if err := e.saveState(); err != nil {
		t.Fatalf("saveState (terminated): %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("state.json should be removed after terminated saveState")
	}
}

// TestSaveStateWritesWhenNotTerminated verifies that saveState writes state.json
// when terminated is false (the default).
func TestSaveStateWritesWhenNotTerminated(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	deps := &Deps{ConfigDir: configDirFn}
	state := &State{
		BeadID:    "spi-live",
		AgentName: "wizard-spi-live",
		Formula:   "test",
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}

	e := NewForTest("spi-live", "wizard-spi-live", state, deps)

	// terminated is false by default — saveState should write.
	if err := e.saveState(); err != nil {
		t.Fatalf("saveState: %v", err)
	}
	path := StatePath("wizard-spi-live", configDirFn)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatal("state.json should exist after normal save")
	}

	// Verify we can load it back.
	loaded, err := LoadState("wizard-spi-live", configDirFn)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected non-nil state")
	}
	if loaded.BeadID != "spi-live" {
		t.Errorf("BeadID = %q, want spi-live", loaded.BeadID)
	}
}

// runGitIn runs a git command in the given directory.
func runGitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

// TestTransitionStepBeadIdempotent verifies that transitionStepBead skips
// closing a step bead that is already closed (defense-in-depth).
func TestTransitionStepBeadIdempotent(t *testing.T) {
	dir := t.TempDir()

	closeCallCount := 0
	activateCallCount := 0

	beadStatuses := map[string]string{
		"step-review": "closed",      // already closed
		"step-merge":  "closed",      // already closed — should not be activated
	}

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetBead: func(id string) (Bead, error) {
			status := beadStatuses[id]
			if status == "" {
				status = "open"
			}
			return Bead{ID: id, Status: status}, nil
		},
		CloseStepBead: func(stepID string) error {
			closeCallCount++
			return nil
		},
		ActivateStepBead: func(stepID string) error {
			activateCallCount++
			return nil
		},
	}

	state := &State{
		BeadID:    "spi-idem",
		AgentName: "wizard-idem",
		StepBeadIDs: map[string]string{
			"review": "step-review",
			"merge":  "step-merge",
		},
	}

	e := NewForTest("spi-idem", "wizard-idem", state, deps)

	// Transition from review → merge, but both are already closed.
	e.transitionStepBead("review", "merge")

	if closeCallCount != 0 {
		t.Errorf("CloseStepBead called %d times, want 0 (review step already closed)", closeCallCount)
	}
	if activateCallCount != 0 {
		t.Errorf("ActivateStepBead called %d times, want 0 (merge step already closed)", activateCallCount)
	}
}

// TestTransitionStepBeadNormalPath verifies transitionStepBead closes and
// activates when beads are in normal (not-yet-closed) state.
func TestTransitionStepBeadNormalPath(t *testing.T) {
	dir := t.TempDir()

	var closedIDs, activatedIDs []string

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: "open"}, nil
		},
		CloseStepBead: func(stepID string) error {
			closedIDs = append(closedIDs, stepID)
			return nil
		},
		ActivateStepBead: func(stepID string) error {
			activatedIDs = append(activatedIDs, stepID)
			return nil
		},
	}

	state := &State{
		BeadID:    "spi-norm",
		AgentName: "wizard-norm",
		StepBeadIDs: map[string]string{
			"implement": "step-impl",
			"review":    "step-review",
		},
	}

	e := NewForTest("spi-norm", "wizard-norm", state, deps)

	e.transitionStepBead("implement", "review")

	if len(closedIDs) != 1 || closedIDs[0] != "step-impl" {
		t.Errorf("closed = %v, want [step-impl]", closedIDs)
	}
	if len(activatedIDs) != 1 || activatedIDs[0] != "step-review" {
		t.Errorf("activated = %v, want [step-review]", activatedIDs)
	}
}

// TestCloseAllOpenStepBeads verifies that closeAllOpenStepBeads closes only
// non-closed step beads.
func TestCloseAllOpenStepBeads(t *testing.T) {
	dir := t.TempDir()

	var closedIDs []string

	beadStatuses := map[string]string{
		"step-impl":   "closed",      // already closed
		"step-review": "in_progress", // should be closed
		"step-merge":  "open",        // should be closed
	}

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetBead: func(id string) (Bead, error) {
			status := beadStatuses[id]
			if status == "" {
				status = "open"
			}
			return Bead{ID: id, Status: status}, nil
		},
		CloseStepBead: func(stepID string) error {
			closedIDs = append(closedIDs, stepID)
			return nil
		},
		AddLabel: func(id, label string) error { return nil },
	}

	state := &State{
		BeadID:    "spi-cleanup",
		AgentName: "wizard-cleanup",
		StepBeadIDs: map[string]string{
			"implement": "step-impl",
			"review":    "step-review",
			"merge":     "step-merge",
		},
	}

	e := NewForTest("spi-cleanup", "wizard-cleanup", state, deps)

	e.closeAllOpenStepBeads()

	// Should close step-review and step-merge but NOT step-impl (already closed).
	if len(closedIDs) != 2 {
		t.Fatalf("expected 2 closed step beads, got %d: %v", len(closedIDs), closedIDs)
	}

	closedSet := make(map[string]bool)
	for _, id := range closedIDs {
		closedSet[id] = true
	}
	if closedSet["step-impl"] {
		t.Error("step-impl was closed but it was already closed")
	}
	if !closedSet["step-review"] {
		t.Error("step-review was not closed but should have been")
	}
	if !closedSet["step-merge"] {
		t.Error("step-merge was not closed but should have been")
	}
}

