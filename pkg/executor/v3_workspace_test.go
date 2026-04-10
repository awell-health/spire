package executor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/awell-health/spire/pkg/formula"
)

// TestV3Workspace_InitFromFormula verifies that InitWorkspaceStates correctly
// populates GraphState.Workspaces from the formula's workspace declarations.
// This differs from TestInitWorkspaceStates in workspace_test.go, which tests
// the v2 State type. Here we test the v3 GraphState path used by the graph
// interpreter.
func TestV3Workspace_InitFromFormula(t *testing.T) {
	g, err := formula.LoadEmbeddedStepGraph("task-default")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	state := NewGraphState(g, "spi-ws", "wizard-ws")

	// The graph interpreter calls InitWorkspaceStates on the v2 State.
	// For v3, workspace state is initialized through NewGraphState + the
	// executor's workspace resolution. Here we verify the formula declares
	// the expected workspaces and that NewGraphState initializes correctly.
	if len(g.Workspaces) != 1 {
		t.Fatalf("expected 1 workspace declaration, got %d", len(g.Workspaces))
	}

	ws, ok := g.Workspaces["feature"]
	if !ok {
		t.Fatal("missing workspace 'feature' in formula")
	}
	if ws.Kind != formula.WorkspaceKindOwnedWorktree {
		t.Errorf("kind = %q, want %q", ws.Kind, formula.WorkspaceKindOwnedWorktree)
	}
	if ws.Branch != "feat/{vars.bead_id}" {
		t.Errorf("branch = %q, want %q", ws.Branch, "feat/{vars.bead_id}")
	}

	// NewGraphState initializes an empty workspace map for runtime use.
	if state.Workspaces == nil {
		t.Error("Workspaces should be initialized (not nil)")
	}

	// Verify formula defaults are applied via ParseFormulaStepGraph.
	if ws.Scope != formula.WorkspaceScopeRun {
		t.Errorf("scope = %q, want %q", ws.Scope, formula.WorkspaceScopeRun)
	}
	if ws.Ownership != "owned" {
		t.Errorf("ownership = %q, want %q", ws.Ownership, "owned")
	}
	if ws.Cleanup != formula.WorkspaceCleanupTerminal {
		t.Errorf("cleanup = %q, want %q", ws.Cleanup, formula.WorkspaceCleanupTerminal)
	}
}

// TestV3Workspace_ResumePreservesState verifies that workspace state survives
// a save/load cycle through GraphState.
func TestV3Workspace_ResumePreservesState(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	graph := &formula.FormulaStepGraph{
		Name:    "test-ws-resume",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"a": {Action: "test.noop", Terminal: true},
		},
	}

	state := NewGraphState(graph, "spi-ws-resume", "wizard-ws-resume")
	state.Workspaces["feature"] = WorkspaceState{
		Name:       "feature",
		Kind:       formula.WorkspaceKindOwnedWorktree,
		Dir:        "/repo/.worktrees/spi-ws-resume-feature",
		Branch:     "feat/spi-ws-resume",
		BaseBranch: "main",
		StartSHA:   "abc123",
		Status:     "active",
		Scope:      formula.WorkspaceScopeRun,
		Ownership:  "owned",
		Cleanup:    formula.WorkspaceCleanupTerminal,
	}
	state.Workspaces["staging"] = WorkspaceState{
		Name:       "staging",
		Kind:       formula.WorkspaceKindStaging,
		Dir:        "/repo/.worktrees/spi-ws-resume-staging",
		Branch:     "staging/spi-ws-resume",
		BaseBranch: "main",
		StartSHA:   "def456",
		Status:     "active",
		Scope:      formula.WorkspaceScopeRun,
		Ownership:  "owned",
		Cleanup:    formula.WorkspaceCleanupTerminal,
	}

	if err := state.Save("wizard-ws-resume", configDirFn); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := LoadGraphState("wizard-ws-resume", configDirFn)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if len(loaded.Workspaces) != 2 {
		t.Fatalf("workspace count = %d, want 2", len(loaded.Workspaces))
	}

	// Verify feature workspace.
	feature := loaded.Workspaces["feature"]
	if feature.Kind != formula.WorkspaceKindOwnedWorktree {
		t.Errorf("feature kind = %q, want %q", feature.Kind, formula.WorkspaceKindOwnedWorktree)
	}
	if feature.Dir != "/repo/.worktrees/spi-ws-resume-feature" {
		t.Errorf("feature dir = %q", feature.Dir)
	}
	if feature.Branch != "feat/spi-ws-resume" {
		t.Errorf("feature branch = %q", feature.Branch)
	}
	if feature.StartSHA != "abc123" {
		t.Errorf("feature start_sha = %q", feature.StartSHA)
	}
	if feature.Status != "active" {
		t.Errorf("feature status = %q", feature.Status)
	}
	if feature.Scope != formula.WorkspaceScopeRun {
		t.Errorf("feature scope = %q", feature.Scope)
	}
	if feature.Ownership != "owned" {
		t.Errorf("feature ownership = %q", feature.Ownership)
	}
	if feature.Cleanup != formula.WorkspaceCleanupTerminal {
		t.Errorf("feature cleanup = %q", feature.Cleanup)
	}

	// Verify staging workspace.
	staging := loaded.Workspaces["staging"]
	if staging.Kind != formula.WorkspaceKindStaging {
		t.Errorf("staging kind = %q, want %q", staging.Kind, formula.WorkspaceKindStaging)
	}
	if staging.StartSHA != "def456" {
		t.Errorf("staging start_sha = %q", staging.StartSHA)
	}
}

// TestV3Workspace_AllKindsHaveDefaults verifies that each workspace kind gets
// correct default scope, ownership, and cleanup when DefaultWorkspaceDecl is
// applied.
func TestV3Workspace_AllKindsHaveDefaults(t *testing.T) {
	kinds := []struct {
		kind            string
		wantScope       string
		wantOwnership   string
		wantCleanup     string
	}{
		{formula.WorkspaceKindRepo, formula.WorkspaceScopeRun, "owned", formula.WorkspaceCleanupTerminal},
		{formula.WorkspaceKindOwnedWorktree, formula.WorkspaceScopeRun, "owned", formula.WorkspaceCleanupTerminal},
		{formula.WorkspaceKindStaging, formula.WorkspaceScopeRun, "owned", formula.WorkspaceCleanupTerminal},
		// borrowed_worktree is special — ownership is typically set explicitly to "borrowed",
		// but DefaultWorkspaceDecl defaults it to "owned" since it only fills zero values.
		{formula.WorkspaceKindBorrowedWorktree, formula.WorkspaceScopeRun, "owned", formula.WorkspaceCleanupTerminal},
	}

	for _, tt := range kinds {
		t.Run(tt.kind, func(t *testing.T) {
			decl := formula.WorkspaceDecl{Kind: tt.kind}
			formula.DefaultWorkspaceDecl(&decl)

			if decl.Scope != tt.wantScope {
				t.Errorf("scope = %q, want %q", decl.Scope, tt.wantScope)
			}
			if decl.Ownership != tt.wantOwnership {
				t.Errorf("ownership = %q, want %q", decl.Ownership, tt.wantOwnership)
			}
			if decl.Cleanup != tt.wantCleanup {
				t.Errorf("cleanup = %q, want %q", decl.Cleanup, tt.wantCleanup)
			}
		})
	}
}

// TestV3Workspace_EpicFormulaDeclaredWorkspaces validates the workspace
// declarations in the epic v3 formula.
func TestV3Workspace_EpicFormulaDeclaredWorkspaces(t *testing.T) {
	g, err := formula.LoadEmbeddedStepGraph("epic-default")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if len(g.Workspaces) != 1 {
		t.Fatalf("expected 1 workspace, got %d", len(g.Workspaces))
	}

	staging, ok := g.Workspaces["staging"]
	if !ok {
		t.Fatal("missing workspace 'staging'")
	}
	if staging.Kind != formula.WorkspaceKindStaging {
		t.Errorf("kind = %q, want %q", staging.Kind, formula.WorkspaceKindStaging)
	}
	if staging.Branch != "epic/{vars.bead_id}" {
		t.Errorf("branch = %q, want %q", staging.Branch, "epic/{vars.bead_id}")
	}
	if staging.Scope != formula.WorkspaceScopeRun {
		t.Errorf("scope = %q, want %q", staging.Scope, formula.WorkspaceScopeRun)
	}
	if staging.Cleanup != formula.WorkspaceCleanupTerminal {
		t.Errorf("cleanup = %q, want %q", staging.Cleanup, formula.WorkspaceCleanupTerminal)
	}
}

// TestV3Workspace_EpicImplementDeclaredWorkspaces validates workspace
// declarations in the subgraph-implement formula.
func TestV3Workspace_EpicImplementDeclaredWorkspaces(t *testing.T) {
	g, err := formula.LoadEmbeddedStepGraph("subgraph-implement")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if len(g.Workspaces) != 1 {
		t.Fatalf("expected 1 workspace, got %d", len(g.Workspaces))
	}

	staging, ok := g.Workspaces["staging"]
	if !ok {
		t.Fatal("missing workspace 'staging'")
	}
	if staging.Kind != formula.WorkspaceKindStaging {
		t.Errorf("kind = %q, want %q", staging.Kind, formula.WorkspaceKindStaging)
	}
	if staging.Branch != "staging/{vars.bead_id}" {
		t.Errorf("branch = %q, want %q", staging.Branch, "staging/{vars.bead_id}")
	}
}

func TestV3Workspace_ResolveGraphBranchState_UsesDeclaredStagingBranch(t *testing.T) {
	deps, _ := testGraphDeps(t)

	graph := &formula.FormulaStepGraph{
		Name:    "test-epic-workspace-branch",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"entry": {Action: "noop", Terminal: true},
		},
		Workspaces: map[string]formula.WorkspaceDecl{
			"staging": {
				Kind:   formula.WorkspaceKindStaging,
				Branch: "epic/{vars.bead_id}",
				Base:   "{vars.base_branch}",
			},
		},
	}

	exec := NewGraphForTest("spi-epic", "wizard-epic", graph, nil, deps)
	if err := exec.resolveGraphBranchState(graph, exec.graphState); err != nil {
		t.Fatalf("resolveGraphBranchState: %v", err)
	}

	if exec.graphState.StagingBranch != "epic/spi-epic" {
		t.Fatalf("StagingBranch = %q, want %q", exec.graphState.StagingBranch, "epic/spi-epic")
	}
}

func TestV3Workspace_ResolveGraphWorkspace_StagingPersistsAndResumes(t *testing.T) {
	repoDir := initSeamTestRepo(t)
	configDir := t.TempDir()
	configDirFn := func() (string, error) { return configDir, nil }

	graph := &formula.FormulaStepGraph{
		Name:    "test-staging-persist",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"entry": {Action: "noop", Terminal: true},
		},
		Workspaces: map[string]formula.WorkspaceDecl{
			"staging": {
				Kind:   formula.WorkspaceKindStaging,
				Branch: "epic/{vars.bead_id}",
				Base:   "{vars.base_branch}",
			},
		},
	}

	deps := &Deps{
		ConfigDir: configDirFn,
		ResolveRepo: func(beadID string) (string, string, string, error) {
			return repoDir, "", "main", nil
		},
		AddLabel: func(id, label string) error { return nil },
		ActiveTowerConfig: func() (*TowerConfig, error) {
			return nil, os.ErrNotExist
		},
	}

	exec := NewGraphForTest("spi-resume", "wizard-resume", graph, nil, deps)
	state := exec.graphState
	state.RepoPath = repoDir
	state.BaseBranch = "main"
	state.Vars["bead_id"] = "spi-resume"
	exec.initMissingGraphWorkspaces(graph, state)

	if err := exec.resolveGraphBranchState(graph, state); err != nil {
		t.Fatalf("resolveGraphBranchState: %v", err)
	}

	dir, err := exec.resolveGraphWorkspace("staging", state)
	if err != nil {
		t.Fatalf("resolveGraphWorkspace create: %v", err)
	}
	wantDir := filepath.Join(repoDir, ".worktrees", "spi-resume-staging")
	if dir != wantDir {
		t.Fatalf("staging dir = %q, want %q", dir, wantDir)
	}

	loaded, err := LoadGraphState("wizard-resume", configDirFn)
	if err != nil {
		t.Fatalf("load graph state: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected persisted graph state after staging workspace creation")
	}
	if loaded.WorktreeDir != wantDir {
		t.Fatalf("persisted WorktreeDir = %q, want %q", loaded.WorktreeDir, wantDir)
	}
	if got := loaded.Workspaces["staging"].Branch; got != "epic/spi-resume" {
		t.Fatalf("persisted staging branch = %q, want %q", got, "epic/spi-resume")
	}

	testFile := filepath.Join(dir, "wave.txt")
	if err := os.WriteFile(testFile, []byte("wave\n"), 0644); err != nil {
		t.Fatalf("write wave file: %v", err)
	}
	runGitIn(t, dir, "add", "wave.txt")
	runGitIn(t, dir, "commit", "-m", "wave progress")

	exec2 := NewGraphForTest("spi-resume", "wizard-resume", graph, loaded, deps)
	dir2, err := exec2.resolveGraphWorkspace("staging", loaded)
	if err != nil {
		t.Fatalf("resolveGraphWorkspace resume: %v", err)
	}
	if dir2 != wantDir {
		t.Fatalf("resumed staging dir = %q, want %q", dir2, wantDir)
	}
	runGitIn(t, dir2, "cat-file", "-e", "HEAD:wave.txt")
}
