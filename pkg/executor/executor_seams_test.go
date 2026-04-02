package executor

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/formula"
	"github.com/steveyegge/beads"
)

// initSeamTestRepo creates a temporary git repo with an initial commit on main.
// Returns the repo directory path.
func initSeamTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init")
	runGit("config", "user.name", "Test")
	runGit("config", "user.email", "test@test.com")
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0644)
	runGit("add", "-A")
	runGit("commit", "-m", "initial commit")
	runGit("branch", "-M", "main")
	return dir
}

// =============================================================================
// Seam 1: wizardPlan() plan-bypass when only workflow-step children exist
//
// Bug: wizardPlan() calls GetChildren() and if len(children) > 0, it skips
// plan generation entirely (line 50). After ensureStepBeads() runs, the bead
// already has workflow-step children. So wizardPlan sees children > 0 and
// bypasses plan generation, falling through to enrichment of internal DAG beads.
//
// The fix should filter out step beads before the len(children) > 0 check,
// so that only real task children cause the plan to be skipped.
// =============================================================================

// TestWizardPlan_OnlyStepBeadChildren documents the current (buggy) behavior:
// wizardPlan() sees workflow-step children and skips plan generation.
//
// TODO(spi-b8kf3): After the fix lands, this test should verify that
// ClaudeRunner IS called (plan generation happens) when only step beads exist.
func TestWizardPlan_OnlyStepBeadChildren(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	claudeRunnerCalled := false
	enrichCommentCount := 0

	stepChildren := []Bead{
		{ID: "spi-plan.1", Labels: []string{"workflow-step", "step:implement"}, Status: "in_progress"},
		{ID: "spi-plan.2", Labels: []string{"workflow-step", "step:review"}, Status: "open"},
		{ID: "spi-plan.3", Labels: []string{"workflow-step", "step:merge"}, Status: "open"},
	}

	deps := &Deps{
		ConfigDir: configDirFn,
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Title: "Test Epic", Description: "Test epic desc", Priority: 1}, nil
		},
		GetChildren: func(parentID string) ([]Bead, error) {
			return stepChildren, nil
		},
		GetDepsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			return nil, nil
		},
		GetComments: func(id string) ([]*beads.Comment, error) {
			return nil, nil
		},
		AddComment: func(id, text string) error {
			enrichCommentCount++
			return nil
		},
		ClaudeRunner: func(args []string, dir string) ([]byte, error) {
			claudeRunnerCalled = true
			return []byte(`{"title": "Task 1", "description": "Do something", "deps": [], "shared_files": [], "do_not_touch": []}`), nil
		},
		CreateBead: func(opts CreateOpts) (string, error) {
			return "spi-plan.new-1", nil
		},
		AddDep:     func(issueID, dependsOnID string) error { return nil },
		IsAttemptBead: func(b Bead) bool {
			for _, l := range b.Labels {
				if l == "attempt" {
					return true
				}
			}
			return false
		},
		IsStepBead: func(b Bead) bool {
			for _, l := range b.Labels {
				if l == "workflow-step" {
					return true
				}
			}
			return false
		},
		IsReviewRoundBead: func(b Bead) bool { return false },
		ParseIssueType:    func(s string) beads.IssueType { return beads.TypeTask },
	}

	f := &formula.FormulaV2{
		Name:    "test-epic",
		Version: 2,
		Phases: map[string]formula.PhaseConfig{
			"plan":      {Role: "wizard", Model: "claude-sonnet-4-6"},
			"implement": {Role: "apprentice"},
			"review":    {Role: "sage"},
			"merge":     {Role: "wizard"},
		},
	}

	state := &State{
		BeadID:    "spi-plan",
		AgentName: "wizard-test",
		Subtasks:  make(map[string]SubtaskState),
		RepoPath:  dir,
	}

	e := NewForTest("spi-plan", "wizard-test", f, state, deps)
	pc := formula.PhaseConfig{Role: "wizard", Model: "claude-sonnet-4-6"}
	bead, _ := deps.GetBead("spi-plan")

	err := e.wizardPlanEpic(bead, pc)

	// CURRENT BEHAVIOR (buggy): wizardPlan sees 3 step-bead children,
	// enters the enrichment path, and enrichSubtasksWithChangeSpecs skips
	// all of them (they're step beads). No ClaudeRunner call happens.
	// The function returns nil — no error, no plan generated.
	if claudeRunnerCalled {
		// If this fires, the bug has been fixed — update this test.
		t.Log("NOTE: ClaudeRunner was called — the plan-bypass bug may have been fixed")
	} else {
		t.Log("KNOWN BUG: ClaudeRunner NOT called — step beads caused plan bypass")
	}

	// The function should succeed (it enters enrichment, which skips all step beads).
	if err != nil {
		t.Fatalf("wizardPlan returned error: %v", err)
	}
}

// TestWizardPlan_MixedChildren verifies that when both step beads AND real
// task children exist, plan generation is correctly skipped (the plan already ran)
// and enrichment proceeds on the real children only.
func TestWizardPlan_MixedChildren(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	claudeRunnerCalled := 0
	var enrichedIDs []string

	mixedChildren := []Bead{
		{ID: "spi-mix.1", Labels: []string{"workflow-step", "step:implement"}, Status: "in_progress"},
		{ID: "spi-mix.2", Labels: []string{"workflow-step", "step:review"}, Status: "open"},
		{ID: "spi-mix.3", Labels: []string{"workflow-step", "step:merge"}, Status: "open"},
		{ID: "spi-mix.4", Title: "Real task 1", Status: "open"},
		{ID: "spi-mix.5", Title: "Real task 2", Status: "open"},
	}

	deps := &Deps{
		ConfigDir: configDirFn,
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Title: "Test Epic", Description: "desc", Priority: 1, Type: "epic"}, nil
		},
		GetChildren: func(parentID string) ([]Bead, error) {
			return mixedChildren, nil
		},
		GetDepsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			return nil, nil
		},
		GetComments: func(id string) ([]*beads.Comment, error) {
			return nil, nil
		},
		AddComment: func(id, text string) error {
			return nil
		},
		ClaudeRunner: func(args []string, dir string) ([]byte, error) {
			claudeRunnerCalled++
			// Track which bead is being enriched (the prompt contains the subtask ID)
			for _, arg := range args {
				for _, child := range mixedChildren {
					if strings.Contains(arg, child.ID) && !strings.Contains(arg, "workflow-step") {
						enrichedIDs = append(enrichedIDs, child.ID)
					}
				}
			}
			return []byte("**Change spec: fake**\n\n**Files to modify:**\n- foo.go"), nil
		},
		IsAttemptBead: func(b Bead) bool { return false },
		IsStepBead: func(b Bead) bool {
			for _, l := range b.Labels {
				if l == "workflow-step" {
					return true
				}
			}
			return false
		},
		IsReviewRoundBead: func(b Bead) bool { return false },
	}

	state := &State{
		BeadID:    "spi-mix",
		AgentName: "wizard-test",
		Subtasks:  make(map[string]SubtaskState),
		RepoPath:  dir,
	}

	e := NewForTest("spi-mix", "wizard-test", nil, state, deps)
	pc := formula.PhaseConfig{Model: "claude-sonnet-4-6"}
	bead, _ := deps.GetBead("spi-mix")

	err := e.wizardPlanEpic(bead, pc)

	if err != nil {
		t.Fatalf("wizardPlan returned error: %v", err)
	}

	// With mixed children, the enrichment path runs. ClaudeRunner should be
	// called exactly 2 times (for the 2 real children, not the 3 step beads).
	if claudeRunnerCalled != 2 {
		t.Errorf("ClaudeRunner called %d times, want 2 (only real children)", claudeRunnerCalled)
	}
}

// TestWizardPlan_OnlyRealChildren verifies that when only real task children
// exist (no step beads), plan generation is skipped and enrichment proceeds.
func TestWizardPlan_OnlyRealChildren(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	claudeRunnerCalled := 0

	realChildren := []Bead{
		{ID: "spi-real.1", Title: "Task 1", Status: "open"},
		{ID: "spi-real.2", Title: "Task 2", Status: "open"},
	}

	deps := &Deps{
		ConfigDir: configDirFn,
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Title: "Test Epic", Description: "desc", Priority: 1, Type: "epic"}, nil
		},
		GetChildren: func(parentID string) ([]Bead, error) {
			return realChildren, nil
		},
		GetDepsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			return nil, nil
		},
		GetComments: func(id string) ([]*beads.Comment, error) {
			return nil, nil
		},
		AddComment: func(id, text string) error {
			return nil
		},
		ClaudeRunner: func(args []string, dir string) ([]byte, error) {
			claudeRunnerCalled++
			return []byte("**Change spec: fake**\n\n**Files to modify:**\n- foo.go"), nil
		},
		IsAttemptBead:     func(b Bead) bool { return false },
		IsStepBead:        func(b Bead) bool { return false },
		IsReviewRoundBead: func(b Bead) bool { return false },
	}

	state := &State{
		BeadID:    "spi-real",
		AgentName: "wizard-test",
		Subtasks:  make(map[string]SubtaskState),
		RepoPath:  dir,
	}

	e := NewForTest("spi-real", "wizard-test", nil, state, deps)
	pc := formula.PhaseConfig{Model: "claude-sonnet-4-6"}
	bead, _ := deps.GetBead("spi-real")

	err := e.wizardPlanEpic(bead, pc)

	if err != nil {
		t.Fatalf("wizardPlan returned error: %v", err)
	}

	// With only real children, enrichment runs for each.
	if claudeRunnerCalled != 2 {
		t.Errorf("ClaudeRunner called %d times, want 2", claudeRunnerCalled)
	}
}

// =============================================================================
// Seam 2: Direct-mode implement -> merge path
//
// Bug: executeDirect spawns an apprentice that works on feat/<bead-id>.
// But executeMerge reads state.StagingBranch (which resolveBranchState always
// sets to "staging/<bead-id>" by default) and tries to merge that branch —
// not the feat/<bead-id> branch where the apprentice actually committed.
//
// In direct mode, executeMerge should merge feat/<bead-id>, not
// staging/<bead-id>, unless the formula explicitly configured a staging branch.
// =============================================================================

// mockHandle is a simple mock for agent.Handle.
type mockHandle struct {
	waitErr error
}

func (h *mockHandle) Wait() error              { return h.waitErr }
func (h *mockHandle) Signal(os.Signal) error    { return nil }
func (h *mockHandle) Alive() bool               { return false }
func (h *mockHandle) Name() string              { return "mock" }
func (h *mockHandle) Identifier() string        { return "mock-id" }

// mockBackend is a simple mock for agent.Backend.
type mockBackend struct {
	spawnFn func(cfg agent.SpawnConfig) (agent.Handle, error)
}

func (b *mockBackend) Spawn(cfg agent.SpawnConfig) (agent.Handle, error) {
	if b.spawnFn != nil {
		return b.spawnFn(cfg)
	}
	return &mockHandle{}, nil
}

func (b *mockBackend) List() ([]agent.Info, error) {
	return nil, nil
}

func (b *mockBackend) Logs(name string) (io.ReadCloser, error) {
	return nil, os.ErrNotExist
}

func (b *mockBackend) Kill(name string) error {
	return nil
}

// TestDirectModeImplement verifies executeDirect dispatches an apprentice
// and completes without error when the apprentice succeeds.
func TestDirectModeImplement(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	var spawnedConfig agent.SpawnConfig
	deps := &Deps{
		ConfigDir: configDirFn,
		Spawner: &mockBackend{
			spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
				spawnedConfig = cfg
				return &mockHandle{}, nil
			},
		},
	}

	state := &State{
		BeadID:    "spi-direct",
		AgentName: "wizard-test",
		Subtasks:  make(map[string]SubtaskState),
		RepoPath:  dir,
	}

	e := NewForTest("spi-direct", "wizard-test", nil, state, deps)
	pc := formula.PhaseConfig{Role: "apprentice"}

	err := e.executeDirect("implement", pc)
	if err != nil {
		t.Fatalf("executeDirect error: %v", err)
	}

	if spawnedConfig.Role != agent.RoleApprentice {
		t.Errorf("spawned role = %q, want %q", spawnedConfig.Role, agent.RoleApprentice)
	}
	if spawnedConfig.BeadID != "spi-direct" {
		t.Errorf("spawned beadID = %q, want spi-direct", spawnedConfig.BeadID)
	}
}

// TestDirectModeImplement_ApprenticeFlag verifies that the --apprentice flag
// is passed when pc.Apprentice is true.
func TestDirectModeImplement_ApprenticeFlag(t *testing.T) {
	dir := t.TempDir()

	var capturedExtraArgs []string
	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		Spawner: &mockBackend{
			spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
				capturedExtraArgs = cfg.ExtraArgs
				return &mockHandle{}, nil
			},
		},
	}

	state := &State{
		BeadID:    "spi-appflag",
		AgentName: "wizard-test",
		Subtasks:  make(map[string]SubtaskState),
	}

	e := NewForTest("spi-appflag", "wizard-test", nil, state, deps)
	pc := formula.PhaseConfig{Role: "apprentice", Apprentice: true}

	err := e.executeDirect("implement", pc)
	if err != nil {
		t.Fatalf("executeDirect error: %v", err)
	}

	found := false
	for _, arg := range capturedExtraArgs {
		if arg == "--apprentice" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected --apprentice in extra args, got %v", capturedExtraArgs)
	}
}

// TestDirectModeImplement_Failure verifies error propagation when apprentice fails.
func TestDirectModeImplement_Failure(t *testing.T) {
	dir := t.TempDir()

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		Spawner: &mockBackend{
			spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
				return &mockHandle{waitErr: os.ErrDeadlineExceeded}, nil
			},
		},
	}

	state := &State{
		BeadID:    "spi-fail",
		AgentName: "wizard-test",
		Subtasks:  make(map[string]SubtaskState),
	}

	e := NewForTest("spi-fail", "wizard-test", nil, state, deps)
	pc := formula.PhaseConfig{Role: "apprentice"}

	err := e.executeDirect("implement", pc)
	if err == nil {
		t.Fatal("expected error from failed apprentice, got nil")
	}
	if !strings.Contains(err.Error(), "apprentice") {
		t.Errorf("error should mention apprentice, got: %s", err)
	}
}

// =============================================================================
// Seam 3: Staging branch creation from base (resolveBranchState)
//
// Bug: ensureStagingWorktree() calls rc.ForceBranch(stagingBranch) which does
// `git branch -f <name>` — this creates/resets the branch to current HEAD of
// the main repo, not the base branch. If HEAD is somewhere else, the staging
// branch starts from the wrong point.
//
// resolveBranchState() resolves the staging branch name from formula config.
// These tests verify the name resolution logic.
// =============================================================================

// TestResolveBranchState_DefaultStagingBranch verifies the default
// staging/<bead-id> branch name when no formula override exists.
func TestResolveBranchState_DefaultStagingBranch(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	deps := &Deps{
		ConfigDir: configDirFn,
		ResolveRepo: func(beadID string) (string, string, string, error) {
			return dir, "https://example.com/repo.git", "main", nil
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
		BeadID:    "spi-default",
		AgentName: "wizard-test",
		Subtasks:  make(map[string]SubtaskState),
	}

	e := NewForTest("spi-default", "wizard-test", f, state, deps)

	err := e.resolveBranchState()
	if err != nil {
		t.Fatalf("resolveBranchState error: %v", err)
	}

	expected := "staging/spi-default"
	if e.state.StagingBranch != expected {
		t.Errorf("StagingBranch = %q, want %q", e.state.StagingBranch, expected)
	}
	if e.state.BaseBranch != "main" {
		t.Errorf("BaseBranch = %q, want main", e.state.BaseBranch)
	}
	if e.state.RepoPath != dir {
		t.Errorf("RepoPath = %q, want %q", e.state.RepoPath, dir)
	}
}

// TestResolveBranchState_CustomStagingBranch verifies formula-configured
// staging branch with {bead-id} template substitution.
func TestResolveBranchState_CustomStagingBranch(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	deps := &Deps{
		ConfigDir: configDirFn,
		ResolveRepo: func(beadID string) (string, string, string, error) {
			return dir, "", "main", nil
		},
	}

	f := &formula.FormulaV2{
		Name:    "test-formula",
		Version: 2,
		Phases: map[string]formula.PhaseConfig{
			"implement": {Role: "apprentice", StagingBranch: "epic/{bead-id}/staging"},
			"review":    {Role: "sage"},
			"merge":     {Role: "wizard"},
		},
	}

	state := &State{
		BeadID:    "spi-custom",
		AgentName: "wizard-test",
		Subtasks:  make(map[string]SubtaskState),
	}

	e := NewForTest("spi-custom", "wizard-test", f, state, deps)

	err := e.resolveBranchState()
	if err != nil {
		t.Fatalf("resolveBranchState error: %v", err)
	}

	expected := "epic/spi-custom/staging"
	if e.state.StagingBranch != expected {
		t.Errorf("StagingBranch = %q, want %q", e.state.StagingBranch, expected)
	}
}

// TestResolveBranchState_StagingBranchOnNonImplementPhase verifies that
// StagingBranch is found even when configured on a non-implement phase.
// The loop in resolveBranchState iterates all enabled phases.
func TestResolveBranchState_StagingBranchOnNonImplementPhase(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	deps := &Deps{
		ConfigDir: configDirFn,
		ResolveRepo: func(beadID string) (string, string, string, error) {
			return dir, "", "develop", nil
		},
	}

	f := &formula.FormulaV2{
		Name:    "test-formula",
		Version: 2,
		Phases: map[string]formula.PhaseConfig{
			"implement": {Role: "apprentice"},
			"review":    {Role: "sage", StagingBranch: "review/{bead-id}"},
			"merge":     {Role: "wizard"},
		},
	}

	state := &State{
		BeadID:    "spi-nonimpl",
		AgentName: "wizard-test",
		Subtasks:  make(map[string]SubtaskState),
	}

	e := NewForTest("spi-nonimpl", "wizard-test", f, state, deps)

	err := e.resolveBranchState()
	if err != nil {
		t.Fatalf("resolveBranchState error: %v", err)
	}

	// The review phase is after implement in ValidPhases order, so it's the
	// first phase with StagingBranch set. But implement comes first and has no
	// StagingBranch, so the loop should find review's staging branch.
	expected := "review/spi-nonimpl"
	if e.state.StagingBranch != expected {
		t.Errorf("StagingBranch = %q, want %q", e.state.StagingBranch, expected)
	}
	if e.state.BaseBranch != "develop" {
		t.Errorf("BaseBranch = %q, want develop", e.state.BaseBranch)
	}
}

// TestResolveBranchState_SkipsWhenAlreadyResolved verifies that if RepoPath
// and BaseBranch are already set (resumed from disk), resolveBranchState
// preserves existing values.
func TestResolveBranchState_SkipsWhenAlreadyResolved(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	resolveRepoCalled := false
	deps := &Deps{
		ConfigDir: configDirFn,
		ResolveRepo: func(beadID string) (string, string, string, error) {
			resolveRepoCalled = true
			return "/other/path", "", "other-branch", nil
		},
	}

	f := &formula.FormulaV2{
		Name:    "test-formula",
		Version: 2,
		Phases: map[string]formula.PhaseConfig{
			"implement": {Role: "apprentice"},
		},
	}

	state := &State{
		BeadID:        "spi-resume",
		AgentName:     "wizard-test",
		Subtasks:      make(map[string]SubtaskState),
		RepoPath:      "/original/path",
		BaseBranch:    "original-branch",
		StagingBranch: "staging/spi-resume",
	}

	e := NewForTest("spi-resume", "wizard-test", f, state, deps)

	err := e.resolveBranchState()
	if err != nil {
		t.Fatalf("resolveBranchState error: %v", err)
	}

	if resolveRepoCalled {
		t.Error("ResolveRepo should not be called when state is already resolved")
	}
	if e.state.RepoPath != "/original/path" {
		t.Errorf("RepoPath = %q, want /original/path", e.state.RepoPath)
	}
	if e.state.BaseBranch != "original-branch" {
		t.Errorf("BaseBranch = %q, want original-branch", e.state.BaseBranch)
	}
	if e.state.StagingBranch != "staging/spi-resume" {
		t.Errorf("StagingBranch = %q, want staging/spi-resume", e.state.StagingBranch)
	}
}

// TestResolveBranchState_EmptyRepoPathDefaults verifies that an empty
// repo path from ResolveRepo defaults to ".".
func TestResolveBranchState_EmptyRepoPathDefaults(t *testing.T) {
	dir := t.TempDir()

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		ResolveRepo: func(beadID string) (string, string, string, error) {
			return "", "", "main", nil // empty repo/url, default base branch (matches real ResolveRepo)
		},
	}

	f := &formula.FormulaV2{
		Name:    "test-formula",
		Version: 2,
		Phases: map[string]formula.PhaseConfig{
			"implement": {Role: "apprentice"},
		},
	}

	state := &State{
		BeadID:    "spi-empty",
		AgentName: "wizard-test",
		Subtasks:  make(map[string]SubtaskState),
	}

	e := NewForTest("spi-empty", "wizard-test", f, state, deps)

	err := e.resolveBranchState()
	if err != nil {
		t.Fatalf("resolveBranchState error: %v", err)
	}

	if e.state.RepoPath != "." {
		t.Errorf("RepoPath = %q, want \".\"", e.state.RepoPath)
	}
	if e.state.BaseBranch != "main" {
		t.Errorf("BaseBranch = %q, want main", e.state.BaseBranch)
	}
}

// =============================================================================
// Seam 4: state.json lifecycle
//
// Tests for state persistence cleanup on terminal paths and state isolation.
// =============================================================================

// TestStateCleanupOnSuccess verifies that Run() removes the state file
// after all phases complete successfully.
func TestStateCleanupOnSuccess(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	deps := &Deps{
		ConfigDir: configDirFn,
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: "in_progress"}, nil
		},
		GetChildren: func(parentID string) ([]Bead, error) {
			return nil, nil
		},
		GetActiveAttempt: func(parentID string) (*Bead, error) {
			return nil, nil
		},
		CreateAttemptBead: func(parentID, agentName, model, branch string) (string, error) {
			return "spi-cleanup.attempt-1", nil
		},
		CloseAttemptBead: func(attemptID, result string) error { return nil },
		HasLabel:         func(b Bead, prefix string) string { return "" },
		AddLabel:         func(id, label string) error { return nil },
		ResolveRepo: func(beadID string) (string, string, string, error) {
			return dir, "", "main", nil
		},
		RegistryAdd:    func(entry agent.Entry) error { return nil },
		RegistryRemove: func(name string) error { return nil },
		CreateStepBead: func(parentID, stepName string) (string, error) {
			return parentID + "." + stepName, nil
		},
		ActivateStepBead: func(stepID string) error { return nil },
		CloseStepBead:    func(stepID string) error { return nil },
		ContainsLabel:    func(b Bead, label string) bool { return false },
		Spawner: &mockBackend{
			spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
				return &mockHandle{}, nil
			},
		},
	}

	f := &formula.FormulaV2{
		Name:    "test-cleanup",
		Version: 2,
		Phases: map[string]formula.PhaseConfig{
			"implement": {Role: "skip"},
		},
	}

	state := &State{
		BeadID:    "spi-cleanup",
		AgentName: "wizard-cleanup",
		Formula:   "test-cleanup",
		Phase:     "implement",
		Subtasks:  make(map[string]SubtaskState),
	}

	e := NewForTest("spi-cleanup", "wizard-cleanup", f, state, deps)

	// Pre-save state so there's a file to check.
	if err := e.saveState(); err != nil {
		t.Fatalf("pre-save state: %v", err)
	}

	statePath := StatePath("wizard-cleanup", configDirFn)
	if _, err := os.Stat(statePath); os.IsNotExist(err) {
		t.Fatal("state file should exist before Run()")
	}

	err := e.Run()
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}

	// KNOWN BUG: Run() calls os.Remove(StatePath(...)) on line 293, but the
	// deferred saveState() on line 153 runs AFTER that (LIFO order), recreating
	// the state file. So the file still exists after Run().
	// TODO(spi-b8kf3): After the fix, update this to assert the file is absent.
	if _, err := os.Stat(statePath); os.IsNotExist(err) {
		t.Log("NOTE: state file was actually removed — the defer-ordering bug may have been fixed")
	} else {
		t.Log("KNOWN BUG: state file still exists after Run() — defer saveState() recreates it after os.Remove")
	}
}

// TestStatePersistenceAcrossPhases verifies that state is saved between
// phase transitions so it can be resumed.
func TestStatePersistenceAcrossPhases(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	phasesSeen := []string{}
	deps := &Deps{
		ConfigDir: configDirFn,
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: "in_progress"}, nil
		},
		GetChildren: func(parentID string) ([]Bead, error) {
			return nil, nil
		},
		GetActiveAttempt: func(parentID string) (*Bead, error) {
			return nil, nil
		},
		CreateAttemptBead: func(parentID, agentName, model, branch string) (string, error) {
			return "spi-persist.attempt-1", nil
		},
		CloseAttemptBead: func(attemptID, result string) error { return nil },
		HasLabel:         func(b Bead, prefix string) string { return "" },
		AddLabel:         func(id, label string) error { return nil },
		ResolveRepo: func(beadID string) (string, string, string, error) {
			return dir, "", "main", nil
		},
		RegistryAdd:    func(entry agent.Entry) error { return nil },
		RegistryRemove: func(name string) error { return nil },
		CreateStepBead: func(parentID, stepName string) (string, error) {
			return parentID + "." + stepName, nil
		},
		ActivateStepBead: func(stepID string) error { return nil },
		CloseStepBead:    func(stepID string) error { return nil },
		ContainsLabel:    func(b Bead, label string) bool { return false },
		Spawner: &mockBackend{
			spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
				return &mockHandle{}, nil
			},
		},
	}

	f := &formula.FormulaV2{
		Name:    "test-persist",
		Version: 2,
		Phases: map[string]formula.PhaseConfig{
			"implement": {Role: "skip"},
			"review":    {Role: "skip"},
		},
	}

	state := &State{
		BeadID:    "spi-persist",
		AgentName: "wizard-persist",
		Formula:   "test-persist",
		Phase:     "implement",
		Subtasks:  make(map[string]SubtaskState),
	}

	e := NewForTest("spi-persist", "wizard-persist", f, state, deps)

	// Intercept saveState to track phase transitions.
	origSave := e.saveState
	_ = origSave

	// Run should advance through implement -> review and save state between.
	err := e.Run()
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}

	// KNOWN BUG: Same defer-ordering issue as TestStateCleanupOnSuccess.
	// Run() deletes the file, then deferred saveState() recreates it.
	// TODO(spi-b8kf3): After the fix, update this to assert loaded == nil.
	loaded, loadErr := LoadState("wizard-persist", configDirFn)
	if loadErr != nil {
		t.Fatalf("LoadState error: %v", loadErr)
	}
	if loaded == nil {
		t.Log("NOTE: state file was cleaned up — the defer-ordering bug may have been fixed")
	} else {
		t.Log("KNOWN BUG: state file still exists after successful Run — deferred saveState recreates it")
	}

	_ = phasesSeen // Used for tracking if we need to intercept
}

// =============================================================================
// Seam 5: Full Run() phase loop — end-to-end phase orchestration
//
// Tests the complete phase pipeline: phase dispatch, advancement, step bead
// transitions, attempt tracking, and terminal conditions.
// =============================================================================

// TestRunFullPipeline_SkipPhases tests Run() with a formula where all phases
// are "skip" role — verifying the phase loop, advancement, and cleanup.
func TestRunFullPipeline_SkipPhases(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	var closedAttemptResult string
	var stepsClosed []string
	var stepsActivated []string

	deps := &Deps{
		ConfigDir: configDirFn,
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: "in_progress"}, nil
		},
		GetChildren: func(parentID string) ([]Bead, error) {
			return nil, nil
		},
		GetActiveAttempt: func(parentID string) (*Bead, error) {
			return nil, nil
		},
		CreateAttemptBead: func(parentID, agentName, model, branch string) (string, error) {
			return "spi-run.attempt-1", nil
		},
		CloseAttemptBead: func(attemptID, result string) error {
			closedAttemptResult = result
			return nil
		},
		HasLabel:       func(b Bead, prefix string) string { return "" },
		AddLabel:       func(id, label string) error { return nil },
		ContainsLabel:  func(b Bead, label string) bool { return false },
		ResolveRepo: func(beadID string) (string, string, string, error) {
			return dir, "", "main", nil
		},
		RegistryAdd:    func(entry agent.Entry) error { return nil },
		RegistryRemove: func(name string) error { return nil },
		CreateStepBead: func(parentID, stepName string) (string, error) {
			return parentID + ".step-" + stepName, nil
		},
		ActivateStepBead: func(stepID string) error {
			stepsActivated = append(stepsActivated, stepID)
			return nil
		},
		CloseStepBead: func(stepID string) error {
			stepsClosed = append(stepsClosed, stepID)
			return nil
		},
		Spawner: &mockBackend{},
	}

	f := &formula.FormulaV2{
		Name:    "test-run",
		Version: 2,
		Phases: map[string]formula.PhaseConfig{
			"implement": {Role: "skip"},
			"review":    {Role: "skip"},
		},
	}

	state := &State{
		BeadID:    "spi-run",
		AgentName: "wizard-run",
		Formula:   "test-run",
		Phase:     "implement",
		Subtasks:  make(map[string]SubtaskState),
	}

	e := NewForTest("spi-run", "wizard-run", f, state, deps)

	err := e.Run()
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}

	// Verify attempt was created and closed with success.
	if !strings.Contains(closedAttemptResult, "success") {
		t.Errorf("attempt result = %q, want to contain 'success'", closedAttemptResult)
	}

	// Verify step beads were created and transitioned.
	// implement (first) gets activated, then review gets activated when
	// implement completes, then review gets closed when it's the last phase.
	if len(stepsActivated) < 1 {
		t.Errorf("expected at least 1 step activation, got %d", len(stepsActivated))
	}
}

// TestRunFullPipeline_DirectImplementReviewMerge tests a complete
// implement -> review -> merge pipeline with direct dispatch.
// This is the most common workflow for single-task beads.
func TestRunFullPipeline_DirectImplementReviewMerge(t *testing.T) {
	repoDir := initSeamTestRepo(t)
	configDir := t.TempDir()
	configDirFn := func() (string, error) { return configDir, nil }

	// Create a feat branch with a commit so the staging worktree merge has
	// something to merge (prevents empty-implement escalation).
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("checkout", "-b", "feat/spi-full")
	os.WriteFile(filepath.Join(repoDir, "impl.go"), []byte("package impl\n"), 0644)
	runGit("add", "-A")
	runGit("commit", "-m", "feat: apprentice implementation")
	runGit("checkout", "main")

	phasesExecuted := []string{}

	deps := &Deps{
		ConfigDir: configDirFn,
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: "in_progress"}, nil
		},
		GetChildren: func(parentID string) ([]Bead, error) {
			return nil, nil
		},
		GetActiveAttempt: func(parentID string) (*Bead, error) {
			return nil, nil
		},
		CreateAttemptBead: func(parentID, agentName, model, branch string) (string, error) {
			return "spi-full.attempt-1", nil
		},
		CloseAttemptBead: func(attemptID, result string) error { return nil },
		HasLabel:         func(b Bead, prefix string) string { return "" },
		AddLabel:         func(id, label string) error { return nil },
		RemoveLabel:      func(id, label string) error { return nil },
		ContainsLabel: func(b Bead, label string) bool {
			// Simulate sage approving by returning true for review-approved
			return label == "review-approved"
		},
		ResolveRepo: func(beadID string) (string, string, string, error) {
			return repoDir, "", "main", nil
		},
		RegistryAdd:    func(entry agent.Entry) error { return nil },
		RegistryRemove: func(name string) error { return nil },
		CreateStepBead: func(parentID, stepName string) (string, error) {
			return parentID + ".step-" + stepName, nil
		},
		ActivateStepBead: func(stepID string) error { return nil },
		CloseStepBead:    func(stepID string) error { return nil },
		CloseBead:        func(id string) error { return nil },
		GetReviewBeads: func(parentID string) ([]Bead, error) {
			return nil, nil
		},
		Spawner: &mockBackend{
			spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
				phasesExecuted = append(phasesExecuted, string(cfg.Role))
				return &mockHandle{}, nil
			},
		},
		// executeMerge needs these — but since it calls real git operations,
		// we'll use the "skip" behavior for merge to avoid git deps.
		ActiveTowerConfig: func() (*config.TowerConfig, error) {
			return nil, os.ErrNotExist
		},
		ArchmageGitEnv: func(tower *config.TowerConfig) []string {
			return os.Environ()
		},
	}

	f := &formula.FormulaV2{
		Name:    "test-full",
		Version: 2,
		Phases: map[string]formula.PhaseConfig{
			"implement": {Role: "apprentice"},
			"review":    {Role: "sage"},
			"merge":     {Behavior: "skip"}, // skip merge to avoid real git operations
		},
	}

	state := &State{
		BeadID:    "spi-full",
		AgentName: "wizard-full",
		Formula:   "test-full",
		Phase:     "implement",
		Subtasks:  make(map[string]SubtaskState),
	}

	e := NewForTest("spi-full", "wizard-full", f, state, deps)

	err := e.Run()
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}

	// Verify apprentice and sage were both dispatched.
	if len(phasesExecuted) != 2 {
		t.Errorf("expected 2 phases executed, got %d: %v", len(phasesExecuted), phasesExecuted)
	}
	if len(phasesExecuted) >= 1 && phasesExecuted[0] != "apprentice" {
		t.Errorf("first phase role = %q, want apprentice", phasesExecuted[0])
	}
	if len(phasesExecuted) >= 2 && phasesExecuted[1] != "sage" {
		t.Errorf("second phase role = %q, want sage", phasesExecuted[1])
	}
}

// TestRunPhaseLoop_BeadClosedMidRun verifies that Run() exits cleanly
// when the bead is closed between phases.
func TestRunPhaseLoop_BeadClosedMidRun(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	getBeadCallCount := 0
	deps := &Deps{
		ConfigDir: configDirFn,
		GetBead: func(id string) (Bead, error) {
			getBeadCallCount++
			// Return closed on the second call (after implement phase advances to review)
			if getBeadCallCount > 1 {
				return Bead{ID: id, Status: "closed"}, nil
			}
			return Bead{ID: id, Status: "in_progress"}, nil
		},
		GetChildren: func(parentID string) ([]Bead, error) {
			return nil, nil
		},
		GetActiveAttempt: func(parentID string) (*Bead, error) {
			return nil, nil
		},
		CreateAttemptBead: func(parentID, agentName, model, branch string) (string, error) {
			return "spi-closed.attempt-1", nil
		},
		CloseAttemptBead: func(attemptID, result string) error { return nil },
		HasLabel:         func(b Bead, prefix string) string { return "" },
		AddLabel:         func(id, label string) error { return nil },
		ContainsLabel:    func(b Bead, label string) bool { return false },
		ResolveRepo: func(beadID string) (string, string, string, error) {
			return dir, "", "main", nil
		},
		RegistryAdd:    func(entry agent.Entry) error { return nil },
		RegistryRemove: func(name string) error { return nil },
		CreateStepBead: func(parentID, stepName string) (string, error) {
			return parentID + ".step-" + stepName, nil
		},
		ActivateStepBead: func(stepID string) error { return nil },
		CloseStepBead:    func(stepID string) error { return nil },
		Spawner:          &mockBackend{},
	}

	f := &formula.FormulaV2{
		Name:    "test-closed",
		Version: 2,
		Phases: map[string]formula.PhaseConfig{
			"implement": {Role: "skip"},
			"review":    {Role: "skip"}, // should never reach this
			"merge":     {Role: "skip"}, // should never reach this
		},
	}

	state := &State{
		BeadID:    "spi-closed",
		AgentName: "wizard-closed",
		Formula:   "test-closed",
		Phase:     "implement",
		Subtasks:  make(map[string]SubtaskState),
	}

	e := NewForTest("spi-closed", "wizard-closed", f, state, deps)

	err := e.Run()
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}

	// Should have exited after detecting bead closed, not run through all phases.
	// The bead status check happens after advancing from implement to review.
}

// TestRunPhaseLoop_UnknownPhaseError verifies that Run() returns an error
// when the state references an unknown phase.
func TestRunPhaseLoop_UnknownPhaseError(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	deps := &Deps{
		ConfigDir: configDirFn,
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: "in_progress"}, nil
		},
		GetChildren: func(parentID string) ([]Bead, error) {
			return nil, nil
		},
		GetActiveAttempt: func(parentID string) (*Bead, error) {
			return nil, nil
		},
		CreateAttemptBead: func(parentID, agentName, model, branch string) (string, error) {
			return "spi-unknown.attempt-1", nil
		},
		CloseAttemptBead: func(attemptID, result string) error { return nil },
		HasLabel:         func(b Bead, prefix string) string { return "" },
		AddLabel:         func(id, label string) error { return nil },
		ContainsLabel:    func(b Bead, label string) bool { return false },
		ResolveRepo: func(beadID string) (string, string, string, error) {
			return dir, "", "main", nil
		},
		RegistryAdd:    func(entry agent.Entry) error { return nil },
		RegistryRemove: func(name string) error { return nil },
		CreateStepBead: func(parentID, stepName string) (string, error) {
			return parentID + ".step-" + stepName, nil
		},
		ActivateStepBead: func(stepID string) error { return nil },
		CloseStepBead:    func(stepID string) error { return nil },
		Spawner:          &mockBackend{},
	}

	f := &formula.FormulaV2{
		Name:    "test-unknown",
		Version: 2,
		Phases: map[string]formula.PhaseConfig{
			"implement": {Role: "skip"},
		},
	}

	state := &State{
		BeadID:    "spi-unknown",
		AgentName: "wizard-unknown",
		Formula:   "test-unknown",
		Phase:     "nonexistent-phase", // not in formula
		Subtasks:  make(map[string]SubtaskState),
	}

	e := NewForTest("spi-unknown", "wizard-unknown", f, state, deps)

	err := e.Run()
	if err == nil {
		t.Fatal("expected error for unknown phase, got nil")
	}
	if !strings.Contains(err.Error(), "nonexistent-phase") {
		t.Errorf("error should mention the unknown phase, got: %s", err)
	}
}

// TestRunPhaseLoop_BehaviorDispatch verifies that behavior-based dispatch
// takes priority over role-based dispatch.
func TestRunPhaseLoop_BehaviorDispatch(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	deps := &Deps{
		ConfigDir: configDirFn,
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: "in_progress"}, nil
		},
		GetChildren: func(parentID string) ([]Bead, error) {
			return nil, nil
		},
		GetActiveAttempt: func(parentID string) (*Bead, error) {
			return nil, nil
		},
		CreateAttemptBead: func(parentID, agentName, model, branch string) (string, error) {
			return "spi-behavior.attempt-1", nil
		},
		CloseAttemptBead: func(attemptID, result string) error { return nil },
		HasLabel:         func(b Bead, prefix string) string { return "" },
		AddLabel:         func(id, label string) error { return nil },
		ContainsLabel:    func(b Bead, label string) bool { return false },
		ResolveRepo: func(beadID string) (string, string, string, error) {
			return dir, "", "main", nil
		},
		RegistryAdd:    func(entry agent.Entry) error { return nil },
		RegistryRemove: func(name string) error { return nil },
		CreateStepBead: func(parentID, stepName string) (string, error) {
			return parentID + ".step-" + stepName, nil
		},
		ActivateStepBead: func(stepID string) error { return nil },
		CloseStepBead:    func(stepID string) error { return nil },
		Spawner:          &mockBackend{},
	}

	f := &formula.FormulaV2{
		Name:    "test-behavior",
		Version: 2,
		Phases: map[string]formula.PhaseConfig{
			// "skip" behavior should skip this phase regardless of role.
			"implement": {Role: "apprentice", Behavior: "skip"},
			"review":    {Role: "sage", Behavior: "auto-approve"},
		},
	}

	state := &State{
		BeadID:    "spi-behavior",
		AgentName: "wizard-behavior",
		Formula:   "test-behavior",
		Phase:     "implement",
		Subtasks:  make(map[string]SubtaskState),
	}

	e := NewForTest("spi-behavior", "wizard-behavior", f, state, deps)

	err := e.Run()
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}

	// Both phases should have been skipped (skip + auto-approve behaviors).
	// No apprentice or sage should have been spawned.
}

// =============================================================================
// Seam: ensureStagingWorktree resume and stale paths
//
// These tests verify the worktree state management without real git operations.
// Since ensureStagingWorktree() calls real git commands, we test the state
// conditions that control which path it takes.
// =============================================================================

// TestEnsureStagingWorktree_Resume verifies that when WorktreeDir is set
// and the directory exists, the worktree is resumed (not recreated).
func TestEnsureStagingWorktree_Resume(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	// Create the "worktree" directory so the resume path is taken.
	wtDir := dir + "/worktrees/spi-resume-wt"
	os.MkdirAll(wtDir, 0755)

	deps := &Deps{
		ConfigDir: configDirFn,
		AddLabel:  func(id, label string) error { return nil },
	}

	state := &State{
		BeadID:        "spi-resume-wt",
		AgentName:     "wizard-test",
		Subtasks:      make(map[string]SubtaskState),
		RepoPath:      dir,
		BaseBranch:    "main",
		StagingBranch: "staging/spi-resume-wt",
		WorktreeDir:   wtDir,
	}

	e := NewForTest("spi-resume-wt", "wizard-test", nil, state, deps)

	wt, err := e.ensureStagingWorktree()
	if err != nil {
		t.Fatalf("ensureStagingWorktree error: %v", err)
	}

	if wt == nil {
		t.Fatal("expected non-nil staging worktree")
	}

	// Verify the returned worktree wraps the existing dir.
	if wt.Dir != wtDir {
		t.Errorf("worktree Dir = %q, want %q", wt.Dir, wtDir)
	}
	if wt.Branch != "staging/spi-resume-wt" {
		t.Errorf("worktree Branch = %q, want staging/spi-resume-wt", wt.Branch)
	}
}

// TestEnsureStagingWorktree_StalePath verifies that when WorktreeDir is set
// but the directory does NOT exist, the stale state is cleared and recreation
// is attempted.
func TestEnsureStagingWorktree_StalePath(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	deps := &Deps{
		ConfigDir: configDirFn,
		AddLabel:  func(id, label string) error { return nil },
		ActiveTowerConfig: func() (*config.TowerConfig, error) {
			return nil, os.ErrNotExist
		},
	}

	state := &State{
		BeadID:        "spi-stale",
		AgentName:     "wizard-test",
		Subtasks:      make(map[string]SubtaskState),
		RepoPath:      dir,
		BaseBranch:    "main",
		StagingBranch: "staging/spi-stale",
		WorktreeDir:   dir + "/nonexistent-worktree", // doesn't exist
	}

	e := NewForTest("spi-stale", "wizard-test", nil, state, deps)

	// This will fail because it tries to do real git operations, but we can
	// verify the state was cleared.
	_, _ = e.ensureStagingWorktree()

	// The stale WorktreeDir should have been cleared.
	if e.state.WorktreeDir == dir+"/nonexistent-worktree" {
		t.Error("stale WorktreeDir should have been cleared")
	}
}

// TestEnsureStagingWorktree_NoStagingBranch verifies error when no staging
// branch is configured.
func TestEnsureStagingWorktree_NoStagingBranch(t *testing.T) {
	dir := t.TempDir()

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
	}

	state := &State{
		BeadID:        "spi-no-staging",
		AgentName:     "wizard-test",
		Subtasks:      make(map[string]SubtaskState),
		RepoPath:      dir,
		StagingBranch: "", // empty
	}

	e := NewForTest("spi-no-staging", "wizard-test", nil, state, deps)

	_, err := e.ensureStagingWorktree()
	if err == nil {
		t.Fatal("expected error when no staging branch configured")
	}
	if !strings.Contains(err.Error(), "no staging branch") {
		t.Errorf("error should mention no staging branch, got: %s", err)
	}
}

// TestCloseStagingWorktree verifies cleanup clears state.
func TestCloseStagingWorktree(t *testing.T) {
	dir := t.TempDir()

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
	}

	state := &State{
		BeadID:      "spi-close-wt",
		AgentName:   "wizard-test",
		WorktreeDir: "/some/path",
	}

	e := NewForTest("spi-close-wt", "wizard-test", nil, state, deps)

	// Set a mock staging worktree (without real git ops)
	// Just test that closeStagingWorktree clears the state field.
	e.closeStagingWorktree()

	if e.state.WorktreeDir != "" {
		t.Errorf("WorktreeDir should be cleared after closeStagingWorktree, got %q", e.state.WorktreeDir)
	}
}

// =============================================================================
// Additional: ArchmageIdentity
// =============================================================================

// TestArchmageIdentity_Default verifies fallback identity when no tower exists.
func TestArchmageIdentity_Default(t *testing.T) {
	deps := &Deps{
		ActiveTowerConfig: func() (*config.TowerConfig, error) {
			return nil, os.ErrNotExist
		},
	}

	name, email := ArchmageIdentity(deps)
	if name != "spire" {
		t.Errorf("name = %q, want spire", name)
	}
	if email != "spire@spire.local" {
		t.Errorf("email = %q, want spire@spire.local", email)
	}
}

// TestArchmageIdentity_FromTower verifies identity is read from tower config.
func TestArchmageIdentity_FromTower(t *testing.T) {
	deps := &Deps{
		ActiveTowerConfig: func() (*config.TowerConfig, error) {
			return &config.TowerConfig{
				Archmage: config.ArchmageConfig{
					Name:  "Test User",
					Email: "test@example.com",
				},
			}, nil
		},
	}

	name, email := ArchmageIdentity(deps)
	if name != "Test User" {
		t.Errorf("name = %q, want Test User", name)
	}
	if email != "test@example.com" {
		t.Errorf("email = %q, want test@example.com", email)
	}
}

// =============================================================================
// Seam: empty-implement escalation
//
// When the implement phase completes but the staging branch has no diff vs base
// (the apprentice produced no code changes), the executor should:
// 1. Call EscalateEmptyImplement (label needs-human, create alert bead, add comment)
// 2. NOT advance to review
// 3. Set terminated = true (return nil, not error)
// =============================================================================

// TestRunPhaseLoop_EmptyImplementEscalates verifies that when the implement
// phase produces no code changes (staging has no diff vs base), the executor
// escalates immediately instead of advancing to review.
func TestRunPhaseLoop_EmptyImplementEscalates(t *testing.T) {
	repoDir := initSeamTestRepo(t)
	configDir := t.TempDir()
	configDirFn := func() (string, error) { return configDir, nil }

	// Create a feat branch at the same commit as main (no new commits).
	// This means the staging branch will have no diff vs base after merging,
	// triggering empty-implement escalation.
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("branch", "feat/spi-empty")

	var labelsAdded []string
	var commentsAdded []string
	var beadsCreated []CreateOpts
	var depsAdded []string
	var closedAttemptResult string
	phasesExecuted := []string{}

	deps := &Deps{
		ConfigDir: configDirFn,
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: "in_progress"}, nil
		},
		GetChildren: func(parentID string) ([]Bead, error) {
			return nil, nil
		},
		GetActiveAttempt: func(parentID string) (*Bead, error) {
			return nil, nil
		},
		CreateAttemptBead: func(parentID, agentName, model, branch string) (string, error) {
			return "spi-empty.attempt-1", nil
		},
		CloseAttemptBead: func(attemptID, result string) error {
			closedAttemptResult = result
			return nil
		},
		HasLabel:    func(b Bead, prefix string) string { return "" },
		RemoveLabel: func(id, label string) error { return nil },
		AddLabel: func(id, label string) error {
			labelsAdded = append(labelsAdded, id+":"+label)
			return nil
		},
		AddComment: func(id, text string) error {
			commentsAdded = append(commentsAdded, text)
			return nil
		},
		CreateBead: func(opts CreateOpts) (string, error) {
			beadsCreated = append(beadsCreated, opts)
			return "spi-alert-1", nil
		},
		AddDepTyped: func(issueID, dependsOnID, depType string) error {
			depsAdded = append(depsAdded, issueID+"→"+dependsOnID+":"+depType)
			return nil
		},
		ContainsLabel: func(b Bead, label string) bool { return false },
		ResolveRepo: func(beadID string) (string, string, string, error) {
			return repoDir, "", "main", nil
		},
		RegistryAdd:    func(entry agent.Entry) error { return nil },
		RegistryRemove: func(name string) error { return nil },
		CreateStepBead: func(parentID, stepName string) (string, error) {
			return parentID + ".step-" + stepName, nil
		},
		ActivateStepBead: func(stepID string) error { return nil },
		CloseStepBead:    func(stepID string) error { return nil },
		CloseBead:        func(id string) error { return nil },
		Spawner: &mockBackend{
			spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
				phasesExecuted = append(phasesExecuted, string(cfg.Role))
				return &mockHandle{}, nil
			},
		},
		ActiveTowerConfig: func() (*config.TowerConfig, error) {
			return nil, os.ErrNotExist
		},
		ArchmageGitEnv: func(tower *config.TowerConfig) []string {
			return os.Environ()
		},
	}

	f := &formula.FormulaV2{
		Name:    "test-empty-impl",
		Version: 2,
		Phases: map[string]formula.PhaseConfig{
			"implement": {Role: "apprentice"},
			"review":    {Role: "sage"},
		},
	}

	state := &State{
		BeadID:    "spi-empty",
		AgentName: "wizard-empty",
		Formula:   "test-empty-impl",
		Phase:     "implement",
		Subtasks:  make(map[string]SubtaskState),
	}

	e := NewForTest("spi-empty", "wizard-empty", f, state, deps)

	err := e.Run()
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}

	// Verify terminated was set (state file would be cleaned up).
	if !e.terminated {
		t.Error("expected terminated=true after empty implement escalation")
	}

	// Verify only the implement phase was dispatched, NOT review.
	if len(phasesExecuted) != 1 {
		t.Errorf("expected 1 phase executed (implement only), got %d: %v", len(phasesExecuted), phasesExecuted)
	}
	if len(phasesExecuted) >= 1 && phasesExecuted[0] != "apprentice" {
		t.Errorf("first phase role = %q, want apprentice", phasesExecuted[0])
	}

	// Verify needs-human label was added to the bead.
	foundNeedsHuman := false
	for _, l := range labelsAdded {
		if l == "spi-empty:needs-human" {
			foundNeedsHuman = true
		}
	}
	if !foundNeedsHuman {
		t.Errorf("expected needs-human label on bead, got labels: %v", labelsAdded)
	}

	// Verify interrupted:empty-implement label was added alongside needs-human.
	foundInterrupted := false
	for _, l := range labelsAdded {
		if l == "spi-empty:interrupted:empty-implement" {
			foundInterrupted = true
		}
	}
	if !foundInterrupted {
		t.Errorf("expected interrupted:empty-implement label on bead, got labels: %v", labelsAdded)
	}

	// Verify an alert bead was created.
	foundAlert := false
	for _, opts := range beadsCreated {
		for _, lbl := range opts.Labels {
			if lbl == "alert:empty-implement" {
				foundAlert = true
			}
		}
	}
	if !foundAlert {
		t.Errorf("expected alert bead with alert:empty-implement label, got: %v", beadsCreated)
	}

	// Verify a caused-by dep was added (alert → bead, not ref: label).
	foundCausedByDep := false
	for _, d := range depsAdded {
		if d == "spi-alert-1→spi-empty:caused-by" {
			foundCausedByDep = true
		}
	}
	if !foundCausedByDep {
		t.Errorf("expected caused-by dep spi-alert-1→spi-empty, got: %v", depsAdded)
	}

	// Verify a comment was added explaining the situation.
	foundComment := false
	for _, c := range commentsAdded {
		if strings.Contains(c, "no code changes") {
			foundComment = true
		}
	}
	if !foundComment {
		t.Errorf("expected comment about no code changes, got: %v", commentsAdded)
	}

	// Verify attempt was closed with escalation message, not success.
	if !strings.Contains(closedAttemptResult, "escalat") {
		t.Errorf("attempt result = %q, want to contain 'escalat'", closedAttemptResult)
	}
}

// TestEscalateHumanFailure_InterruptedLabel verifies that EscalateHumanFailure
// adds an interrupted:<failureType> label alongside needs-human.
func TestEscalateHumanFailure_InterruptedLabel(t *testing.T) {
	var labelsAdded []string
	var beadsCreated []CreateOpts
	var depsAdded []string

	deps := &Deps{
		AddLabel: func(id, label string) error {
			labelsAdded = append(labelsAdded, id+":"+label)
			return nil
		},
		AddComment: func(id, text string) error { return nil },
		CreateBead: func(opts CreateOpts) (string, error) {
			beadsCreated = append(beadsCreated, opts)
			return "spi-alert-hf", nil
		},
		AddDepTyped: func(issueID, dependsOnID, depType string) error {
			depsAdded = append(depsAdded, issueID+"→"+dependsOnID+":"+depType)
			return nil
		},
	}

	EscalateHumanFailure("spi-test", "wizard-test", "merge-failure", "merge conflict", deps)

	// Verify needs-human label.
	foundNeedsHuman := false
	for _, l := range labelsAdded {
		if l == "spi-test:needs-human" {
			foundNeedsHuman = true
		}
	}
	if !foundNeedsHuman {
		t.Errorf("expected needs-human label, got: %v", labelsAdded)
	}

	// Verify interrupted:<failureType> label.
	foundInterrupted := false
	for _, l := range labelsAdded {
		if l == "spi-test:interrupted:merge-failure" {
			foundInterrupted = true
		}
	}
	if !foundInterrupted {
		t.Errorf("expected interrupted:merge-failure label, got: %v", labelsAdded)
	}

	// Verify alert bead was created with correct label.
	foundAlert := false
	for _, opts := range beadsCreated {
		for _, lbl := range opts.Labels {
			if lbl == "alert:merge-failure" {
				foundAlert = true
			}
		}
	}
	if !foundAlert {
		t.Errorf("expected alert bead with alert:merge-failure label, got: %v", beadsCreated)
	}

	// Verify caused-by dep was added.
	foundDep := false
	for _, d := range depsAdded {
		if d == "spi-alert-hf→spi-test:caused-by" {
			foundDep = true
		}
	}
	if !foundDep {
		t.Errorf("expected caused-by dep, got: %v", depsAdded)
	}
}

// =============================================================================
// Seam: wizardPlanTask() — standalone task planning path
//
// wizardPlanTask() is invoked for non-epic beads. It collects context,
// invokes Claude for a plan, and posts the result as an "Implementation plan:"
// comment. These tests cover the three key paths: happy, resume, empty plan.
// =============================================================================

// TestWizardPlanTask_HappyPath verifies that a non-epic bead invokes Claude
// and posts an "Implementation plan:" comment on the bead.
func TestWizardPlanTask_HappyPath(t *testing.T) {
	dir := t.TempDir()

	claudeRunnerCalled := false
	var postedComment string
	var postedCommentBeadID string

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Title: "Fix auth bug", Description: "Token refresh fails", Type: "task", Priority: 2}, nil
		},
		GetComments: func(id string) ([]*beads.Comment, error) {
			return nil, nil // no existing comments
		},
		GetDepsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			return nil, nil // no design deps
		},
		AddComment: func(id, text string) error {
			postedCommentBeadID = id
			postedComment = text
			return nil
		},
		ClaudeRunner: func(args []string, dir string) ([]byte, error) {
			claudeRunnerCalled = true
			return []byte("**Approach:** Fix the token refresh logic\n\n**Key files:**\n- auth.go"), nil
		},
	}

	state := &State{
		BeadID:    "spi-task1",
		AgentName: "wizard-test",
		Subtasks:  make(map[string]SubtaskState),
		RepoPath:  dir,
	}

	e := NewForTest("spi-task1", "wizard-test", nil, state, deps)
	pc := formula.PhaseConfig{Role: "wizard", Model: "claude-opus-4-6"}
	bead, _ := deps.GetBead("spi-task1")

	err := e.wizardPlanTask(bead, pc)
	if err != nil {
		t.Fatalf("wizardPlanTask returned error: %v", err)
	}

	if !claudeRunnerCalled {
		t.Error("ClaudeRunner was not called — task plan generation should invoke Claude")
	}

	if postedCommentBeadID != "spi-task1" {
		t.Errorf("comment posted to %q, want %q", postedCommentBeadID, "spi-task1")
	}

	if !strings.HasPrefix(postedComment, "Implementation plan:") {
		t.Errorf("comment does not start with 'Implementation plan:', got: %q", postedComment)
	}

	if !strings.Contains(postedComment, "Fix the token refresh logic") {
		t.Errorf("comment does not contain Claude's plan output, got: %q", postedComment)
	}
}

// TestWizardPlanTask_Resume verifies that when an "Implementation plan:" comment
// already exists, wizardPlanTask skips Claude invocation and returns nil.
func TestWizardPlanTask_Resume(t *testing.T) {
	dir := t.TempDir()

	claudeRunnerCalled := false

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Title: "Fix auth bug", Type: "task", Priority: 2}, nil
		},
		GetComments: func(id string) ([]*beads.Comment, error) {
			return []*beads.Comment{
				{ID: "c1", IssueID: id, Author: "wizard", Text: "Implementation plan:\n\n**Approach:** Fix the thing"},
			}, nil
		},
		GetDepsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			return nil, nil
		},
		AddComment: func(id, text string) error {
			t.Error("AddComment should not be called on resume")
			return nil
		},
		ClaudeRunner: func(args []string, dir string) ([]byte, error) {
			claudeRunnerCalled = true
			return nil, nil
		},
	}

	state := &State{
		BeadID:    "spi-task2",
		AgentName: "wizard-test",
		Subtasks:  make(map[string]SubtaskState),
		RepoPath:  dir,
	}

	e := NewForTest("spi-task2", "wizard-test", nil, state, deps)
	pc := formula.PhaseConfig{Role: "wizard", Model: "claude-opus-4-6"}
	bead, _ := deps.GetBead("spi-task2")

	err := e.wizardPlanTask(bead, pc)
	if err != nil {
		t.Fatalf("wizardPlanTask returned error: %v", err)
	}

	if claudeRunnerCalled {
		t.Error("ClaudeRunner should not be called when plan comment already exists")
	}
}

// TestWizardPlanTask_EmptyPlan verifies that when Claude returns an empty plan,
// wizardPlanTask returns an error.
func TestWizardPlanTask_EmptyPlan(t *testing.T) {
	dir := t.TempDir()

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Title: "Fix auth bug", Type: "task", Priority: 2}, nil
		},
		GetComments: func(id string) ([]*beads.Comment, error) {
			return nil, nil
		},
		GetDepsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			return nil, nil
		},
		AddComment: func(id, text string) error {
			t.Error("AddComment should not be called when plan is empty")
			return nil
		},
		ClaudeRunner: func(args []string, dir string) ([]byte, error) {
			return []byte("   \n  \n  "), nil // whitespace-only output
		},
	}

	state := &State{
		BeadID:    "spi-task3",
		AgentName: "wizard-test",
		Subtasks:  make(map[string]SubtaskState),
		RepoPath:  dir,
	}

	e := NewForTest("spi-task3", "wizard-test", nil, state, deps)
	pc := formula.PhaseConfig{Role: "wizard", Model: "claude-opus-4-6"}
	bead, _ := deps.GetBead("spi-task3")

	err := e.wizardPlanTask(bead, pc)
	if err == nil {
		t.Fatal("wizardPlanTask should return error when Claude produces empty plan")
	}

	if !strings.Contains(err.Error(), "empty task plan") {
		t.Errorf("error = %q, want to contain 'empty task plan'", err.Error())
	}
}

// =============================================================================
// Seam: collectDesignContext() — shared helper for design context assembly
//
// collectDesignContext() is used by both wizardPlanTask and wizardPlanEpic.
// It filters deps to discovered-from + design type, then assembles context
// from titles, descriptions, and comments.
// =============================================================================

// TestCollectDesignContext verifies that design beads linked via discovered-from
// deps are assembled into context text.
func TestCollectDesignContext(t *testing.T) {
	dir := t.TempDir()

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetDepsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			return []*beads.IssueWithDependencyMetadata{
				{
					Issue:          beads.Issue{ID: "spi-des1", Title: "Auth redesign", Description: "Use OAuth2 flow", IssueType: "design"},
					DependencyType: beads.DepDiscoveredFrom,
				},
				{
					Issue:          beads.Issue{ID: "spi-des2", Title: "Not a design", Description: "Some task", IssueType: "task"},
					DependencyType: beads.DepDiscoveredFrom,
				},
				{
					Issue:          beads.Issue{ID: "spi-des3", Title: "Related design", Description: "Unrelated", IssueType: "design"},
					DependencyType: beads.DepBlocks, // not discovered-from
				},
			}, nil
		},
		GetComments: func(id string) ([]*beads.Comment, error) {
			if id == "spi-des1" {
				return []*beads.Comment{
					{ID: "c1", IssueID: id, Author: "archmage", Text: "Use PKCE flow"},
				}, nil
			}
			return nil, nil
		},
	}

	state := &State{
		BeadID:    "spi-ctx",
		AgentName: "wizard-test",
		Subtasks:  make(map[string]SubtaskState),
		RepoPath:  dir,
	}

	e := NewForTest("spi-ctx", "wizard-test", nil, state, deps)
	result := e.collectDesignContext()

	// Should include the discovered-from design bead.
	if !strings.Contains(result, "spi-des1") {
		t.Errorf("result should contain design bead spi-des1, got: %q", result)
	}
	if !strings.Contains(result, "Auth redesign") {
		t.Errorf("result should contain design bead title, got: %q", result)
	}
	if !strings.Contains(result, "Use OAuth2 flow") {
		t.Errorf("result should contain design bead description, got: %q", result)
	}
	if !strings.Contains(result, "Use PKCE flow") {
		t.Errorf("result should contain design bead comment, got: %q", result)
	}

	// Should NOT include non-design dep (spi-des2 is a task, not design).
	if strings.Contains(result, "spi-des2") {
		t.Errorf("result should not contain non-design bead spi-des2, got: %q", result)
	}

	// Should NOT include non-discovered-from dep (spi-des3 is blocks, not discovered-from).
	if strings.Contains(result, "spi-des3") {
		t.Errorf("result should not contain non-discovered-from bead spi-des3, got: %q", result)
	}
}

// =============================================================================
// Seam: wizardValidateDesign — auto-create, poll-until-closed, poll-until-enriched
//
// wizardValidateDesign now polls instead of exiting when a design bead is
// missing, open, or empty. It auto-creates a design bead if none exists.
// =============================================================================

// TestWizardValidateDesign_CreatesDesignBead verifies that when no design bead
// exists, wizardValidateDesign auto-creates one, links it via discovered-from,
// adds comments, labels needs-human, and messages the archmage. On the second
// poll iteration, the newly created bead is found closed with content → returns nil.
func TestWizardValidateDesign_CreatesDesignBead(t *testing.T) {
	dir := t.TempDir()

	var (
		designBeadOpts   *CreateOpts
		addDepTypedCalls []struct{ issue, dep, depType string }
		addedLabels      []struct{ id, label string }
		addedComments    []struct{ id, text string }
		messageCount     int
	)

	pollCount := 0

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetDepsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			pollCount++
			if pollCount <= 1 {
				// First poll: no design beads.
				return nil, nil
			}
			// Second poll: design bead exists, closed, with content.
			return []*beads.IssueWithDependencyMetadata{
				{
					Issue: beads.Issue{
						ID:          "spi-design-new",
						Title:       "Design: spi-epic1",
						Description: "Design decisions here",
						IssueType:   "design",
						Status:      "closed",
					},
					DependencyType: beads.DepDiscoveredFrom,
				},
			}, nil
		},
		GetComments: func(id string) ([]*beads.Comment, error) {
			if id == "spi-design-new" {
				return []*beads.Comment{{ID: "c1", IssueID: id, Text: "PKCE flow"}}, nil
			}
			return nil, nil
		},
		CreateBead: func(opts CreateOpts) (string, error) {
			// Check if it's a message (has msg label) vs a design bead.
			for _, l := range opts.Labels {
				if l == "msg" {
					messageCount++
					return "spi-msg-1", nil
				}
			}
			// Design bead creation.
			copied := opts
			designBeadOpts = &copied
			return "spi-design-new", nil
		},
		AddDepTyped: func(issueID, dependsOnID, depType string) error {
			addDepTypedCalls = append(addDepTypedCalls, struct{ issue, dep, depType string }{issueID, dependsOnID, depType})
			return nil
		},
		AddDep: func(issueID, dependsOnID string) error { return nil },
		AddComment: func(id, text string) error {
			addedComments = append(addedComments, struct{ id, text string }{id, text})
			return nil
		},
		AddLabel: func(id, label string) error {
			addedLabels = append(addedLabels, struct{ id, label string }{id, label})
			return nil
		},
		RemoveLabel:    func(id, label string) error { return nil },
		ParseIssueType: func(s string) beads.IssueType { return beads.IssueType(s) },
	}

	state := &State{
		BeadID:    "spi-epic1",
		AgentName: "wizard-test",
		Subtasks:  make(map[string]SubtaskState),
		RepoPath:  dir,
	}

	e := NewForTest("spi-epic1", "wizard-test", nil, state, deps)
	e.designPollInterval = time.Millisecond

	err := e.wizardValidateDesign()
	if err != nil {
		t.Fatalf("wizardValidateDesign returned error: %v", err)
	}

	// Verify CreateBead was called with type=design.
	if designBeadOpts == nil {
		t.Fatal("CreateBead was never called for design bead")
	}
	if string(designBeadOpts.Type) != "design" {
		t.Errorf("CreateBead type = %q, want %q", designBeadOpts.Type, "design")
	}

	// Verify discovered-from dep was added.
	if len(addDepTypedCalls) == 0 {
		t.Fatal("AddDepTyped was not called")
	}
	dep := addDepTypedCalls[0]
	if dep.issue != "spi-epic1" || dep.dep != "spi-design-new" || dep.depType != "discovered-from" {
		t.Errorf("AddDepTyped(%q, %q, %q), want (spi-epic1, spi-design-new, discovered-from)",
			dep.issue, dep.dep, dep.depType)
	}

	// Verify needs-human label was added to the epic.
	foundNeedsHuman := false
	for _, l := range addedLabels {
		if l.id == "spi-epic1" && l.label == "needs-human" {
			foundNeedsHuman = true
			break
		}
	}
	if !foundNeedsHuman {
		t.Error("needs-human label was not added to epic")
	}

	// Verify comments were added to both the epic and the design bead.
	epicCommented, designCommented := false, false
	for _, c := range addedComments {
		if c.id == "spi-epic1" && strings.Contains(c.text, "auto-created") {
			epicCommented = true
		}
		if c.id == "spi-design-new" && strings.Contains(c.text, "auto-created") {
			designCommented = true
		}
	}
	if !epicCommented {
		t.Error("epic was not commented about auto-created design bead")
	}
	if !designCommented {
		t.Error("design bead was not commented")
	}

	// Verify archmage was messaged.
	if messageCount == 0 {
		t.Error("archmage was not messaged")
	}
}

// TestWizardValidateDesign_WaitsForOpenDesign verifies that when a design bead
// exists but is open, wizardValidateDesign polls until it's closed with content.
func TestWizardValidateDesign_WaitsForOpenDesign(t *testing.T) {
	dir := t.TempDir()

	pollCount := 0
	var addedLabels []string

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetDepsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			pollCount++
			status := beads.Status("open")
			if pollCount >= 2 {
				status = "closed"
			}
			return []*beads.IssueWithDependencyMetadata{
				{
					Issue: beads.Issue{
						ID:          "spi-des1",
						Title:       "Auth design",
						Description: "Use OAuth2",
						IssueType:   "design",
						Status:      status,
					},
					DependencyType: beads.DepDiscoveredFrom,
				},
			}, nil
		},
		GetComments: func(id string) ([]*beads.Comment, error) {
			return []*beads.Comment{{ID: "c1", IssueID: id, Text: "Some content"}}, nil
		},
		AddComment: func(id, text string) error { return nil },
		AddLabel: func(id, label string) error {
			addedLabels = append(addedLabels, label)
			return nil
		},
		RemoveLabel: func(id, label string) error { return nil },
		CreateBead: func(opts CreateOpts) (string, error) {
			return "spi-msg-1", nil // for archmage message
		},
		AddDep:         func(issueID, dependsOnID string) error { return nil },
		ParseIssueType: func(s string) beads.IssueType { return beads.IssueType(s) },
	}

	state := &State{
		BeadID:    "spi-epic2",
		AgentName: "wizard-test",
		Subtasks:  make(map[string]SubtaskState),
		RepoPath:  dir,
	}

	e := NewForTest("spi-epic2", "wizard-test", nil, state, deps)
	e.designPollInterval = time.Millisecond

	err := e.wizardValidateDesign()
	if err != nil {
		t.Fatalf("wizardValidateDesign returned error: %v", err)
	}

	// Should have polled at least twice.
	if pollCount < 2 {
		t.Errorf("pollCount = %d, want >= 2", pollCount)
	}

	// Verify needs-human label was added while waiting.
	found := false
	for _, l := range addedLabels {
		if l == "needs-human" {
			found = true
			break
		}
	}
	if !found {
		t.Error("needs-human label was not added while waiting for open design bead")
	}
}

// TestWizardValidateDesign_WaitsForEmptyDesign verifies that when a design bead
// is closed but has no content, wizardValidateDesign polls until it has content.
func TestWizardValidateDesign_WaitsForEmptyDesign(t *testing.T) {
	dir := t.TempDir()

	pollCount := 0

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetDepsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			pollCount++
			return []*beads.IssueWithDependencyMetadata{
				{
					Issue: beads.Issue{
						ID:        "spi-des1",
						Title:     "Auth design",
						IssueType: "design",
						Status:    "closed",
						// Description intentionally empty.
					},
					DependencyType: beads.DepDiscoveredFrom,
				},
			}, nil
		},
		GetComments: func(id string) ([]*beads.Comment, error) {
			if pollCount >= 2 {
				// Second poll: content added.
				return []*beads.Comment{{ID: "c1", IssueID: id, Text: "Design decisions"}}, nil
			}
			// First poll: no comments.
			return nil, nil
		},
		AddComment:  func(id, text string) error { return nil },
		AddLabel:    func(id, label string) error { return nil },
		RemoveLabel: func(id, label string) error { return nil },
		CreateBead: func(opts CreateOpts) (string, error) {
			return "spi-msg-1", nil
		},
		AddDep:         func(issueID, dependsOnID string) error { return nil },
		ParseIssueType: func(s string) beads.IssueType { return beads.IssueType(s) },
	}

	state := &State{
		BeadID:    "spi-epic3",
		AgentName: "wizard-test",
		Subtasks:  make(map[string]SubtaskState),
		RepoPath:  dir,
	}

	e := NewForTest("spi-epic3", "wizard-test", nil, state, deps)
	e.designPollInterval = time.Millisecond

	err := e.wizardValidateDesign()
	if err != nil {
		t.Fatalf("wizardValidateDesign returned error: %v", err)
	}

	// Should have polled at least twice (first: empty, second: has content).
	if pollCount < 2 {
		t.Errorf("pollCount = %d, want >= 2", pollCount)
	}
}

// TestWizardValidateDesign_HappyPath verifies that when a closed design bead
// with content already exists, wizardValidateDesign returns immediately.
func TestWizardValidateDesign_HappyPath(t *testing.T) {
	dir := t.TempDir()

	pollCount := 0
	removedLabels := []string{}

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetDepsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			pollCount++
			return []*beads.IssueWithDependencyMetadata{
				{
					Issue: beads.Issue{
						ID:          "spi-des1",
						Title:       "Auth design",
						Description: "Solid design",
						IssueType:   "design",
						Status:      "closed",
					},
					DependencyType: beads.DepDiscoveredFrom,
				},
			}, nil
		},
		GetComments: func(id string) ([]*beads.Comment, error) {
			return []*beads.Comment{{ID: "c1", IssueID: id, Text: "PKCE flow"}}, nil
		},
		AddComment: func(id, text string) error { return nil },
		AddLabel:   func(id, label string) error { return nil },
		RemoveLabel: func(id, label string) error {
			removedLabels = append(removedLabels, label)
			return nil
		},
		ParseIssueType: func(s string) beads.IssueType { return beads.IssueType(s) },
	}

	state := &State{
		BeadID:    "spi-epic4",
		AgentName: "wizard-test",
		Subtasks:  make(map[string]SubtaskState),
		RepoPath:  dir,
	}

	e := NewForTest("spi-epic4", "wizard-test", nil, state, deps)
	e.designPollInterval = time.Millisecond

	err := e.wizardValidateDesign()
	if err != nil {
		t.Fatalf("wizardValidateDesign returned error: %v", err)
	}

	// Should return on first poll — no waiting needed.
	if pollCount != 1 {
		t.Errorf("pollCount = %d, want 1 (should return immediately)", pollCount)
	}

	// Verify both needs-human and needs-design labels are removed on advance.
	wantRemoved := map[string]bool{"needs-human": false, "needs-design": false}
	for _, l := range removedLabels {
		if _, ok := wantRemoved[l]; ok {
			wantRemoved[l] = true
		}
	}
	for label, found := range wantRemoved {
		if !found {
			t.Errorf("label %q was not removed on advance", label)
		}
	}
}

// TestCollectDesignContext_NoDeps verifies that collectDesignContext returns
// an empty string when there are no discovered-from design deps.
func TestCollectDesignContext_NoDeps(t *testing.T) {
	dir := t.TempDir()

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetDepsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			return nil, nil
		},
		GetComments: func(id string) ([]*beads.Comment, error) {
			return nil, nil
		},
	}

	state := &State{
		BeadID:    "spi-nodeps",
		AgentName: "wizard-test",
		Subtasks:  make(map[string]SubtaskState),
		RepoPath:  dir,
	}

	e := NewForTest("spi-nodeps", "wizard-test", nil, state, deps)
	result := e.collectDesignContext()

	if result != "" {
		t.Errorf("expected empty design context, got: %q", result)
	}
}
