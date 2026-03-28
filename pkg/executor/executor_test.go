package executor

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/formula"
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
		Phase:     "implement",
		Subtasks:  make(map[string]SubtaskState),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}

	e := NewForTest("spi-xyz", "wizard-spi-xyz", nil, state, deps)
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
	if loaded.Phase != state.Phase {
		t.Errorf("Phase = %q, want %q", loaded.Phase, state.Phase)
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

	f := &formula.FormulaV2{
		Name:    "test-formula",
		Version: 2,
		Phases: map[string]formula.PhaseConfig{
			"implement": {Role: "apprentice"},
			"review":    {Role: "sage"},
			"merge":     {Role: "wizard"},
		},
	}

	state := &State{
		BeadID:    "spi-parent",
		AgentName: "wizard-test",
		Subtasks:  make(map[string]SubtaskState),
	}

	e := NewForTest("spi-parent", "wizard-test", f, state, deps)

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

// TestEnsureStepBeadsCreatesWhenNoneExist verifies new step bead creation.
func TestEnsureStepBeadsCreatesWhenNoneExist(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	created := map[string]string{}
	activatedIDs := []string{}

	deps := &Deps{
		ConfigDir: configDirFn,
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id}, nil
		},
		GetChildren: func(parentID string) ([]Bead, error) {
			return nil, nil // no existing children
		},
		CreateStepBead: func(parentID, stepName string) (string, error) {
			id := "spi-new-" + stepName
			created[stepName] = id
			return id, nil
		},
		ActivateStepBead: func(stepID string) error {
			activatedIDs = append(activatedIDs, stepID)
			return nil
		},
		CloseStepBead: func(stepID string) error { return nil },
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

	f := &formula.FormulaV2{
		Name:    "test-formula",
		Version: 2,
		Phases: map[string]formula.PhaseConfig{
			"implement": {Role: "apprentice"},
			"review":    {Role: "sage"},
			"merge":     {Role: "wizard"},
		},
	}

	state := &State{
		BeadID:    "spi-create",
		AgentName: "wizard-test",
		Subtasks:  make(map[string]SubtaskState),
	}

	e := NewForTest("spi-create", "wizard-test", f, state, deps)

	if err := e.ensureStepBeads(); err != nil {
		t.Fatalf("ensureStepBeads error: %v", err)
	}

	if len(created) != 3 {
		t.Errorf("stepCreator called %d times, want 3", len(created))
	}
	if len(activatedIDs) != 1 || activatedIDs[0] != "spi-new-implement" {
		t.Errorf("stepActivator called with %v, want [spi-new-implement]", activatedIDs)
	}
	if len(e.state.StepBeadIDs) != 3 {
		t.Errorf("StepBeadIDs has %d entries, want 3", len(e.state.StepBeadIDs))
	}
}

// TestTransitionStepBead verifies phase transition closes/activates step beads.
func TestTransitionStepBead(t *testing.T) {
	var closed []string
	var activated []string

	deps := &Deps{
		ConfigDir: func() (string, error) { return t.TempDir(), nil },
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

	e := NewForTest("spi-trans", "wizard-trans", nil, state, deps)

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

	e := NewForTest("spi-noop", "wizard", nil, &State{}, deps)
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

	e := NewForTest("spi-test", "wizard-test", nil, state, deps)
	if err := e.ensureAttemptBead(); err != nil {
		t.Fatalf("ensureAttemptBead: %v", err)
	}

	if e.state.AttemptBeadID != createdID {
		t.Errorf("AttemptBeadID = %q, want %q", e.state.AttemptBeadID, createdID)
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

	e := NewForTest("spi-resume", "wizard-resume", nil, state, deps)
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
	e := NewForTest("spi-close", "wizard", nil, state, deps)

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
	e := NewForTest("spi-noop", "wizard", nil, state, deps)
	e.closeAttempt("should not fire")

	if called {
		t.Error("closer should not be called when AttemptBeadID is empty")
	}
}

// TestPhaseNavigation verifies advancePhase and nextPhase.
func TestPhaseNavigation(t *testing.T) {
	f := &formula.FormulaV2{
		Name:    "test",
		Version: 2,
		Phases: map[string]formula.PhaseConfig{
			"design":    {Role: "wizard"},
			"implement": {Role: "apprentice"},
			"review":    {Role: "sage"},
		},
	}

	deps := &Deps{ConfigDir: func() (string, error) { return t.TempDir(), nil }}
	state := &State{Phase: "design"}
	e := NewForTest("spi-nav", "wizard", f, state, deps)

	// nextPhase
	if next := e.nextPhase("design"); next != "implement" {
		t.Errorf("nextPhase(design) = %q, want implement", next)
	}
	if next := e.nextPhase("review"); next != "" {
		t.Errorf("nextPhase(review) = %q, want empty", next)
	}

	// advancePhase
	if !e.advancePhase() {
		t.Error("advancePhase should return true")
	}
	if e.state.Phase != "implement" {
		t.Errorf("phase = %q, want implement", e.state.Phase)
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
		ClaudeRunner: func(args []string, dir string) ([]byte, error) {
			callCount++
			return []byte("**Change spec: fake**\n\n**Files to modify:**\n- foo.go"), nil
		},
		IsAttemptBead:     func(b Bead) bool { return false },
		IsStepBead:        func(b Bead) bool { return false },
		IsReviewRoundBead: func(b Bead) bool { return false },
	}

	state := &State{RepoPath: t.TempDir()}
	e := NewForTest("spi-enrich", "wizard", nil, state, deps)

	children := []Bead{
		{ID: "spi-enrich.1", Title: "Subtask one"},
		{ID: "spi-enrich.2", Title: "Subtask two"},
	}

	pc := formula.PhaseConfig{Model: "claude-opus-4-6"}
	if err := e.enrichSubtasksWithChangeSpecs(children, "", "", pc); err != nil {
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
		ClaudeRunner: func(args []string, dir string) ([]byte, error) {
			callCount++
			return []byte("**Change spec: new**"), nil
		},
		IsAttemptBead:     func(b Bead) bool { return false },
		IsStepBead:        func(b Bead) bool { return false },
		IsReviewRoundBead: func(b Bead) bool { return false },
	}

	state := &State{RepoPath: t.TempDir()}
	e := NewForTest("spi-skip", "wizard", nil, state, deps)

	children := []Bead{
		{ID: "spi-skip.1", Title: "Already done"},
		{ID: "spi-skip.2", Title: "Needs spec"},
	}

	pc := formula.PhaseConfig{Model: "claude-opus-4-6"}
	if err := e.enrichSubtasksWithChangeSpecs(children, "", "", pc); err != nil {
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
		ClaudeRunner: func(args []string, dir string) ([]byte, error) {
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
	e := NewForTest("spi-plan-dag", "wizard", nil, state, deps)

	pc := formula.PhaseConfig{Model: "claude-sonnet-4-6"}
	err := e.wizardPlan(pc)
	if err != nil {
		t.Fatalf("wizardPlan: %v", err)
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
			return Bead{ID: id, Title: "Test epic", Priority: 1}, nil
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
		ClaudeRunner: func(args []string, dir string) ([]byte, error) {
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
	e := NewForTest("spi-plan-mix", "wizard", nil, state, deps)

	pc := formula.PhaseConfig{Model: "claude-opus-4-6"}
	err := e.wizardPlan(pc)
	if err != nil {
		t.Fatalf("wizardPlan: %v", err)
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
		Phase:     "merge",
		Subtasks:  make(map[string]SubtaskState),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}

	e := NewForTest("spi-term", "wizard-spi-term", nil, state, deps)

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
		Phase:     "implement",
		Subtasks:  make(map[string]SubtaskState),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}

	e := NewForTest("spi-live", "wizard-spi-live", nil, state, deps)

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

// TestExecuteSequential verifies that executeSequential dispatches subtasks
// one at a time, in wave order, merging each to staging and closing each
// subtask before advancing to the next.
func TestExecuteSequential(t *testing.T) {
	repoDir := initSeqTestRepo(t)
	configDir := t.TempDir()
	configDirFn := func() (string, error) { return configDir, nil }

	var spawnOrder []string
	var closedBeads []string

	deps := &Deps{
		ConfigDir: configDirFn,
		GetChildren: func(parentID string) ([]Bead, error) {
			return []Bead{
				{ID: "seq-1", Status: "open"},
				{ID: "seq-2", Status: "open"},
				{ID: "seq-3", Status: "open"},
			}, nil
		},
		GetBlockedIssues: func(filter beads.WorkFilter) ([]BoardBead, error) {
			return nil, nil // no deps = all in wave 0
		},
		IsAttemptBead:     func(b Bead) bool { return false },
		IsStepBead:        func(b Bead) bool { return false },
		IsReviewRoundBead: func(b Bead) bool { return false },
		UpdateBead: func(id string, updates map[string]interface{}) error {
			return nil
		},
		CloseBead: func(id string) error {
			closedBeads = append(closedBeads, id)
			return nil
		},
		ActiveTowerConfig: func() (*TowerConfig, error) { return nil, nil },
		ArchmageGitEnv:    func(tower *TowerConfig) []string { return os.Environ() },
		AddLabel:          func(id, label string) error { return nil },
		RemoveLabel:       func(id, label string) error { return nil },
		Spawner: &mockBackend{
			spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
				spawnOrder = append(spawnOrder, cfg.BeadID)
				// Simulate the apprentice creating a feat branch with a commit.
				branch := "feat/" + cfg.BeadID
				runGitIn(t, repoDir, "branch", branch)
				return &mockHandle{}, nil
			},
		},
	}

	f := &formula.FormulaV2{
		Name:    "test-sequential",
		Version: 2,
		Phases: map[string]formula.PhaseConfig{
			"implement": {
				Role:     "apprentice",
				Dispatch: "sequential",
			},
		},
	}

	state := &State{
		BeadID:        "spi-seq",
		AgentName:     "wizard-seq",
		Subtasks:      make(map[string]SubtaskState),
		RepoPath:      repoDir,
		BaseBranch:    "main",
		StagingBranch: "staging/spi-seq",
	}

	e := NewForTest("spi-seq", "wizard-seq", f, state, deps)

	pc := formula.PhaseConfig{
		Role:     "apprentice",
		Dispatch: "sequential",
	}

	err := e.executeSequential("implement", pc)

	// With no real commits on feat branches, the merge is a no-op (already
	// up to date). All 3 subtasks should be dispatched and closed.
	if err != nil {
		t.Fatalf("executeSequential error: %v", err)
	}

	// Verify all 3 subtasks were dispatched in order.
	if len(spawnOrder) != 3 {
		t.Fatalf("expected 3 spawns, got %d: %v", len(spawnOrder), spawnOrder)
	}
	for i, want := range []string{"seq-1", "seq-2", "seq-3"} {
		if spawnOrder[i] != want {
			t.Errorf("spawn[%d] = %q, want %q", i, spawnOrder[i], want)
		}
	}

	// Verify all subtasks were closed.
	if len(closedBeads) != 3 {
		t.Fatalf("expected 3 closed beads, got %d: %v", len(closedBeads), closedBeads)
	}

	// Verify subtask states are all "closed".
	for _, id := range []string{"seq-1", "seq-2", "seq-3"} {
		st, ok := e.state.Subtasks[id]
		if !ok {
			t.Errorf("missing subtask state for %s", id)
			continue
		}
		if st.Status != "closed" {
			t.Errorf("subtask %s status = %q, want closed", id, st.Status)
		}
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

// initSeqTestRepo creates a test repo with a bare remote for push tests.
func initSeqTestRepo(t *testing.T) string {
	t.Helper()
	repoDir := initSeamTestRepo(t)
	bareDir := t.TempDir()
	runGitIn(t, bareDir, "init", "--bare")
	runGitIn(t, repoDir, "remote", "add", "origin", bareDir)
	runGitIn(t, repoDir, "push", "-u", "origin", "main")
	return repoDir
}

// TestExecuteSequential_SkipsCompleted verifies that sequential dispatch
// skips subtasks that are already marked as closed in the executor state.
func TestExecuteSequential_SkipsCompleted(t *testing.T) {
	repoDir := initSeqTestRepo(t)
	configDir := t.TempDir()
	configDirFn := func() (string, error) { return configDir, nil }

	var spawnOrder []string

	deps := &Deps{
		ConfigDir: configDirFn,
		GetChildren: func(parentID string) ([]Bead, error) {
			return []Bead{
				{ID: "seq-1", Status: "open"},
				{ID: "seq-2", Status: "open"},
			}, nil
		},
		GetBlockedIssues: func(filter beads.WorkFilter) ([]BoardBead, error) {
			return nil, nil
		},
		IsAttemptBead:     func(b Bead) bool { return false },
		IsStepBead:        func(b Bead) bool { return false },
		IsReviewRoundBead: func(b Bead) bool { return false },
		UpdateBead: func(id string, updates map[string]interface{}) error {
			return nil
		},
		CloseBead:         func(id string) error { return nil },
		ActiveTowerConfig: func() (*TowerConfig, error) { return nil, nil },
		ArchmageGitEnv:    func(tower *TowerConfig) []string { return os.Environ() },
		AddLabel:          func(id, label string) error { return nil },
		RemoveLabel:       func(id, label string) error { return nil },
		Spawner: &mockBackend{
			spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
				spawnOrder = append(spawnOrder, cfg.BeadID)
				branch := "feat/" + cfg.BeadID
				runGitIn(t, repoDir, "branch", branch)
				return &mockHandle{}, nil
			},
		},
	}

	f := &formula.FormulaV2{
		Name:    "test-sequential",
		Version: 2,
		Phases: map[string]formula.PhaseConfig{
			"implement": {
				Role:     "apprentice",
				Dispatch: "sequential",
			},
		},
	}

	state := &State{
		BeadID:     "spi-seq-skip",
		AgentName:  "wizard-seq-skip",
		Subtasks: map[string]SubtaskState{
			"seq-1": {Status: "closed", Branch: "feat/seq-1", Agent: "old-agent"},
		},
		RepoPath:      repoDir,
		BaseBranch:    "main",
		StagingBranch: "staging/spi-seq-skip",
	}

	e := NewForTest("spi-seq-skip", "wizard-seq-skip", f, state, deps)

	pc := formula.PhaseConfig{
		Role:     "apprentice",
		Dispatch: "sequential",
	}

	err := e.executeSequential("implement", pc)
	if err != nil {
		t.Fatalf("executeSequential error: %v", err)
	}

	// seq-1 should be skipped (already closed), only seq-2 should be spawned
	if len(spawnOrder) != 1 {
		t.Fatalf("expected 1 spawn (seq-1 skipped), got %d: %v", len(spawnOrder), spawnOrder)
	}
	if spawnOrder[0] != "seq-2" {
		t.Errorf("first spawn should be seq-2 (skipping seq-1), got %q", spawnOrder[0])
	}
}

// TestExecuteSequential_NoSubtasks verifies that sequential dispatch returns
// nil when there are no open subtasks.
func TestExecuteSequential_NoSubtasks(t *testing.T) {
	dir := t.TempDir()

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetChildren: func(parentID string) ([]Bead, error) {
			return nil, nil
		},
		GetBlockedIssues: func(filter beads.WorkFilter) ([]BoardBead, error) {
			return nil, nil
		},
		IsAttemptBead:     func(b Bead) bool { return false },
		IsStepBead:        func(b Bead) bool { return false },
		IsReviewRoundBead: func(b Bead) bool { return false },
	}

	state := &State{
		BeadID:    "spi-seq-empty",
		AgentName: "wizard-seq-empty",
		Subtasks:  make(map[string]SubtaskState),
	}

	e := NewForTest("spi-seq-empty", "wizard-seq-empty", nil, state, deps)
	pc := formula.PhaseConfig{Dispatch: "sequential"}

	err := e.executeSequential("implement", pc)
	if err != nil {
		t.Fatalf("expected nil error for no subtasks, got: %v", err)
	}
}

// Suppress unused import warnings
var (
	_ = os.Getenv
	_ = agent.RoleApprentice
)
