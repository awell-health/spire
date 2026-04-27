package executor

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/formula"
)

func initWorkspaceTestRepo(t *testing.T) string {
	t.Helper()
	repoDir := t.TempDir()
	runWorkspaceGit(t, repoDir, "init", "-b", "main")
	runWorkspaceGit(t, repoDir, "config", "user.name", "Test")
	runWorkspaceGit(t, repoDir, "config", "user.email", "test@test.com")
	runWorkspaceGit(t, repoDir, "commit", "--allow-empty", "-m", "initial")
	return repoDir
}

func runWorkspaceGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@test",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@test",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// --- ValidateWorkspaces tests ---

func TestValidateWorkspaces_Valid(t *testing.T) {
	ws := map[string]formula.WorkspaceDecl{
		"main-repo": {Kind: formula.WorkspaceKindRepo, Scope: formula.WorkspaceScopeRun, Ownership: "owned", Cleanup: formula.WorkspaceCleanupTerminal},
		"staging":   {Kind: formula.WorkspaceKindStaging, Branch: "staging/{vars.bead_id}", Base: "{vars.base_branch}", Scope: formula.WorkspaceScopeRun, Ownership: "owned", Cleanup: formula.WorkspaceCleanupTerminal},
		"child":     {Kind: formula.WorkspaceKindOwnedWorktree, Branch: "feat/{vars.bead_id}", Scope: formula.WorkspaceScopeStep, Ownership: "owned", Cleanup: formula.WorkspaceCleanupAlways},
		"borrowed":  {Kind: formula.WorkspaceKindBorrowedWorktree, Branch: "feat/other", Scope: formula.WorkspaceScopeRun, Ownership: "borrowed", Cleanup: formula.WorkspaceCleanupNever},
	}
	if err := formula.ValidateWorkspaces(ws); err != nil {
		t.Fatalf("expected valid, got: %s", err)
	}
}

func TestValidateWorkspaces_InvalidKind(t *testing.T) {
	ws := map[string]formula.WorkspaceDecl{
		"bad": {Kind: "docker_volume", Scope: formula.WorkspaceScopeRun, Ownership: "owned", Cleanup: formula.WorkspaceCleanupTerminal},
	}
	if err := formula.ValidateWorkspaces(ws); err == nil {
		t.Fatal("expected error for invalid kind")
	}
}

func TestValidateWorkspaces_RepoWithBranch(t *testing.T) {
	ws := map[string]formula.WorkspaceDecl{
		"main": {Kind: formula.WorkspaceKindRepo, Branch: "main", Scope: formula.WorkspaceScopeRun, Ownership: "owned", Cleanup: formula.WorkspaceCleanupTerminal},
	}
	if err := formula.ValidateWorkspaces(ws); err == nil {
		t.Fatal("expected error: repo kind must not declare branch")
	}
}

func TestValidateWorkspaces_BorrowedWithOwnedOwnership(t *testing.T) {
	ws := map[string]formula.WorkspaceDecl{
		"borrowed": {Kind: formula.WorkspaceKindBorrowedWorktree, Scope: formula.WorkspaceScopeRun, Ownership: "owned", Cleanup: formula.WorkspaceCleanupTerminal},
	}
	if err := formula.ValidateWorkspaces(ws); err == nil {
		t.Fatal("expected error: borrowed_worktree must have ownership=borrowed")
	}
}

func TestValidateWorkspaces_InvalidScope(t *testing.T) {
	ws := map[string]formula.WorkspaceDecl{
		"bad": {Kind: formula.WorkspaceKindStaging, Scope: "forever", Ownership: "owned", Cleanup: formula.WorkspaceCleanupTerminal},
	}
	if err := formula.ValidateWorkspaces(ws); err == nil {
		t.Fatal("expected error for invalid scope")
	}
}

func TestValidateWorkspaces_InvalidCleanup(t *testing.T) {
	ws := map[string]formula.WorkspaceDecl{
		"bad": {Kind: formula.WorkspaceKindStaging, Scope: formula.WorkspaceScopeRun, Ownership: "owned", Cleanup: "nuke"},
	}
	if err := formula.ValidateWorkspaces(ws); err == nil {
		t.Fatal("expected error for invalid cleanup")
	}
}

// --- DefaultWorkspaceDecl tests ---

func TestDefaultWorkspaceDecl(t *testing.T) {
	decl := formula.WorkspaceDecl{Kind: formula.WorkspaceKindStaging}
	formula.DefaultWorkspaceDecl(&decl)
	if decl.Scope != formula.WorkspaceScopeRun {
		t.Errorf("scope = %q, want %q", decl.Scope, formula.WorkspaceScopeRun)
	}
	if decl.Ownership != "owned" {
		t.Errorf("ownership = %q, want %q", decl.Ownership, "owned")
	}
	if decl.Cleanup != formula.WorkspaceCleanupTerminal {
		t.Errorf("cleanup = %q, want %q", decl.Cleanup, formula.WorkspaceCleanupTerminal)
	}
}

func TestDefaultWorkspaceDecl_NoOverrideExplicit(t *testing.T) {
	decl := formula.WorkspaceDecl{
		Kind:      formula.WorkspaceKindStaging,
		Scope:     formula.WorkspaceScopeStep,
		Ownership: "borrowed",
		Cleanup:   formula.WorkspaceCleanupAlways,
	}
	formula.DefaultWorkspaceDecl(&decl)
	if decl.Scope != formula.WorkspaceScopeStep {
		t.Errorf("scope changed to %q, expected step to be preserved", decl.Scope)
	}
	if decl.Ownership != "borrowed" {
		t.Errorf("ownership changed to %q, expected borrowed to be preserved", decl.Ownership)
	}
	if decl.Cleanup != formula.WorkspaceCleanupAlways {
		t.Errorf("cleanup changed to %q, expected always to be preserved", decl.Cleanup)
	}
}

// --- ValidateGraph workspace reference tests ---

func TestValidateGraph_WorkspaceRefValid(t *testing.T) {
	g := &formula.FormulaStepGraph{
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"entry": {Role: "executor", Workspace: "staging", Terminal: true},
		},
		Workspaces: map[string]formula.WorkspaceDecl{
			"staging": {Kind: formula.WorkspaceKindStaging, Scope: formula.WorkspaceScopeRun, Ownership: "owned", Cleanup: formula.WorkspaceCleanupTerminal},
		},
	}
	if err := formula.ValidateGraph(g); err != nil {
		t.Fatalf("expected valid, got: %s", err)
	}
}

func TestValidateGraph_WorkspaceRefMissing(t *testing.T) {
	g := &formula.FormulaStepGraph{
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"entry": {Role: "executor", Workspace: "nonexistent", Terminal: true},
		},
	}
	if err := formula.ValidateGraph(g); err == nil {
		t.Fatal("expected error: step references nonexistent workspace")
	}
}

func TestValidateGraph_EmptyWorkspaceAllowed(t *testing.T) {
	g := &formula.FormulaStepGraph{
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"entry": {Role: "executor", Terminal: true}, // no workspace — allowed
		},
	}
	if err := formula.ValidateGraph(g); err != nil {
		t.Fatalf("expected valid (empty workspace), got: %s", err)
	}
}

func TestValidateGraph_WorkspaceDeclInvalid(t *testing.T) {
	g := &formula.FormulaStepGraph{
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"entry": {Role: "executor", Workspace: "bad", Terminal: true},
		},
		Workspaces: map[string]formula.WorkspaceDecl{
			"bad": {Kind: "invalid_kind", Scope: formula.WorkspaceScopeRun, Ownership: "owned", Cleanup: formula.WorkspaceCleanupTerminal},
		},
	}
	if err := formula.ValidateGraph(g); err == nil {
		t.Fatal("expected error: workspace declaration with invalid kind")
	}
}

func TestWorkspaceStateHandle(t *testing.T) {
	tests := []struct {
		name    string
		state   WorkspaceState
		jsonDoc string
		want    WorkspaceHandle
	}{
		{
			name: "explicit origin and borrowed ownership",
			state: WorkspaceState{
				Name:       "feature",
				Kind:       "borrowed_worktree",
				Dir:        "/tmp/feature",
				Branch:     "feat/spi-test",
				BaseBranch: "main",
				Ownership:  "borrowed",
				Origin:     WorkspaceOriginOriginClone,
			},
			want: WorkspaceHandle{
				Name:       "feature",
				Kind:       WorkspaceKindBorrowedWorktree,
				Branch:     "feat/spi-test",
				BaseBranch: "main",
				Path:       "/tmp/feature",
				Origin:     WorkspaceOriginOriginClone,
				Borrowed:   true,
			},
		},
		{
			name: "legacy json defaults missing origin to local bind",
			jsonDoc: `{
				"name":"staging",
				"kind":"staging",
				"dir":"/tmp/staging",
				"branch":"staging/spi-test",
				"base_branch":"main",
				"ownership":"owned"
			}`,
			want: WorkspaceHandle{
				Name:       "staging",
				Kind:       WorkspaceKindStaging,
				Branch:     "staging/spi-test",
				BaseBranch: "main",
				Path:       "/tmp/staging",
				Origin:     WorkspaceOriginLocalBind,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ws := tt.state
			if tt.jsonDoc != "" {
				if err := json.Unmarshal([]byte(tt.jsonDoc), &ws); err != nil {
					t.Fatalf("json.Unmarshal: %v", err)
				}
			}
			got := ws.Handle()
			if got != tt.want {
				t.Fatalf("Handle() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// --- resolveWorkspaceBranch tests ---

func TestResolveWorkspaceBranch(t *testing.T) {
	e := NewForTest("spi-abc", "wizard-spi-abc", &State{
		BeadID:     "spi-abc",
		BaseBranch: "main",

		Workspaces: make(map[string]WorkspaceState),
		StepStates: make(map[string]StepState),
		Counters:   make(map[string]int),
	}, &Deps{})

	tests := []struct {
		pattern string
		want    string
	}{
		{"staging/{vars.bead_id}", "staging/spi-abc"},
		{"{vars.base_branch}", "main"},
		{"feat/{vars.bead_id}-impl", "feat/spi-abc-impl"},
		{"literal-branch", "literal-branch"},
		{"", ""},
	}

	for _, tt := range tests {
		got := e.resolveWorkspaceBranch(tt.pattern)
		if got != tt.want {
			t.Errorf("resolveWorkspaceBranch(%q) = %q, want %q", tt.pattern, got, tt.want)
		}
	}
}

// --- workspaceDir tests ---

func TestWorkspaceDir_Default(t *testing.T) {
	e := NewForTest("spi-xyz", "wizard-spi-xyz", &State{
		BeadID:   "spi-xyz",
		RepoPath: "/repo",

		Workspaces: make(map[string]WorkspaceState),
		StepStates: make(map[string]StepState),
		Counters:   make(map[string]int),
	}, &Deps{})

	got := e.workspaceDir("staging")
	want := filepath.Join("/repo", ".worktrees", "spi-xyz-staging")
	if got != want {
		t.Errorf("workspaceDir(staging) = %q, want %q", got, want)
	}
}

func TestWorkspaceDir_FromState(t *testing.T) {
	e := NewForTest("spi-xyz", "wizard-spi-xyz", &State{
		BeadID:   "spi-xyz",
		RepoPath: "/repo",

		Workspaces: map[string]WorkspaceState{
			"borrowed": {Dir: "/other/repo/.worktrees/child-123"},
		},
		StepStates: make(map[string]StepState),
		Counters:   make(map[string]int),
	}, &Deps{})

	got := e.workspaceDir("borrowed")
	want := "/other/repo/.worktrees/child-123"
	if got != want {
		t.Errorf("workspaceDir(borrowed) = %q, want %q", got, want)
	}
}

// --- InitWorkspaceStates tests ---

func TestInitWorkspaceStates(t *testing.T) {
	state := &State{

		Workspaces: make(map[string]WorkspaceState),
		StepStates: make(map[string]StepState),
		Counters:   make(map[string]int),
	}
	e := NewForTest("spi-abc", "wizard-spi-abc", state, &Deps{})

	decls := map[string]formula.WorkspaceDecl{
		"main-repo": {Kind: formula.WorkspaceKindRepo, Scope: formula.WorkspaceScopeRun, Ownership: "owned", Cleanup: formula.WorkspaceCleanupTerminal},
		"staging":   {Kind: formula.WorkspaceKindStaging, Branch: "staging/{vars.bead_id}", Base: "main", Scope: formula.WorkspaceScopeRun, Ownership: "owned", Cleanup: formula.WorkspaceCleanupTerminal},
	}
	e.InitWorkspaceStates(decls)

	if len(e.state.Workspaces) != 2 {
		t.Fatalf("expected 2 workspaces, got %d", len(e.state.Workspaces))
	}

	repo := e.state.Workspaces["main-repo"]
	if repo.Kind != formula.WorkspaceKindRepo || repo.Status != "pending" {
		t.Errorf("main-repo: kind=%q status=%q", repo.Kind, repo.Status)
	}

	staging := e.state.Workspaces["staging"]
	if staging.Kind != formula.WorkspaceKindStaging || staging.Status != "pending" {
		t.Errorf("staging: kind=%q status=%q", staging.Kind, staging.Status)
	}
	if staging.Branch != "staging/{vars.bead_id}" {
		t.Errorf("staging branch pattern not stored: %q", staging.Branch)
	}
}

func TestInitWorkspaceStates_SkipsExisting(t *testing.T) {
	state := &State{

		Workspaces: map[string]WorkspaceState{
			"staging": {Name: "staging", Kind: formula.WorkspaceKindStaging, Status: "active", Dir: "/some/dir"},
		},
		StepStates: make(map[string]StepState),
		Counters:   make(map[string]int),
	}
	e := NewForTest("spi-abc", "wizard-spi-abc", state, &Deps{})

	decls := map[string]formula.WorkspaceDecl{
		"staging": {Kind: formula.WorkspaceKindStaging, Branch: "staging/new", Scope: formula.WorkspaceScopeRun, Ownership: "owned", Cleanup: formula.WorkspaceCleanupTerminal},
	}
	e.InitWorkspaceStates(decls)

	// Should not overwrite the existing active workspace.
	ws := e.state.Workspaces["staging"]
	if ws.Status != "active" {
		t.Errorf("expected active (resume), got %q", ws.Status)
	}
	if ws.Dir != "/some/dir" {
		t.Errorf("expected preserved dir, got %q", ws.Dir)
	}
}

// --- resolveWorkspace repo kind test ---

func TestResolveWorkspace_RepoKind(t *testing.T) {
	state := &State{
		BeadID:   "spi-abc",
		RepoPath: "/repo",

		Workspaces: map[string]WorkspaceState{
			"main": {Name: "main", Kind: formula.WorkspaceKindRepo, Status: "pending", Scope: formula.WorkspaceScopeRun, Ownership: "owned", Cleanup: formula.WorkspaceCleanupTerminal},
		},
		StepStates: make(map[string]StepState),
		Counters:   make(map[string]int),
	}
	e := NewForTest("spi-abc", "wizard-spi-abc", state, &Deps{
		ActiveTowerConfig: func() (*TowerConfig, error) { return nil, nil },
	})

	wc, h, err := e.resolveWorkspace("main")
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if wc != nil {
		t.Fatal("repo kind should return nil WorktreeContext")
	}
	if h == nil {
		t.Fatal("repo kind should return a non-nil handle")
	}
	if h.Origin != WorkspaceOriginLocalBind {
		t.Errorf("repo kind handle origin = %q, want local-bind", h.Origin)
	}

	ws := e.state.Workspaces["main"]
	if ws.Status != "active" {
		t.Errorf("expected active, got %q", ws.Status)
	}
	if ws.Dir != "/repo" {
		t.Errorf("expected /repo, got %q", ws.Dir)
	}
}

func TestResolveWorkspace_NotFound(t *testing.T) {
	state := &State{

		Workspaces: make(map[string]WorkspaceState),
		StepStates: make(map[string]StepState),
		Counters:   make(map[string]int),
	}
	e := NewForTest("spi-abc", "wizard-spi-abc", state, &Deps{})

	_, _, err := e.resolveWorkspace("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent workspace")
	}
}

// --- releaseWorkspace tests ---

func TestReleaseWorkspace_BorrowedMarksClosedOnly(t *testing.T) {
	state := &State{

		Workspaces: map[string]WorkspaceState{
			"borrowed": {
				Name: "borrowed", Kind: formula.WorkspaceKindBorrowedWorktree,
				Status: "active", Scope: formula.WorkspaceScopeRun,
				Ownership: "borrowed", Cleanup: formula.WorkspaceCleanupAlways,
				Dir: "/some/dir",
			},
		},
		StepStates: make(map[string]StepState),
		Counters:   make(map[string]int),
	}
	e := NewForTest("spi-abc", "wizard-spi-abc", state, &Deps{})

	if err := e.releaseWorkspace("borrowed"); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	ws := e.state.Workspaces["borrowed"]
	if ws.Status != "closed" {
		t.Errorf("expected closed, got %q", ws.Status)
	}
}

func TestReleaseWorkspace_RepoMarksClosedOnly(t *testing.T) {
	state := &State{

		Workspaces: map[string]WorkspaceState{
			"main": {
				Name: "main", Kind: formula.WorkspaceKindRepo,
				Status: "active", Scope: formula.WorkspaceScopeRun,
				Ownership: "owned", Cleanup: formula.WorkspaceCleanupAlways,
			},
		},
		StepStates: make(map[string]StepState),
		Counters:   make(map[string]int),
	}
	e := NewForTest("spi-abc", "wizard-spi-abc", state, &Deps{})

	if err := e.releaseWorkspace("main"); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	ws := e.state.Workspaces["main"]
	if ws.Status != "closed" {
		t.Errorf("expected closed, got %q", ws.Status)
	}
}

func TestReleaseWorkspace_AlreadyClosed(t *testing.T) {
	state := &State{

		Workspaces: map[string]WorkspaceState{
			"ws": {Name: "ws", Kind: formula.WorkspaceKindRepo, Status: "closed", Ownership: "owned"},
		},
		StepStates: make(map[string]StepState),
		Counters:   make(map[string]int),
	}
	e := NewForTest("spi-abc", "wizard-spi-abc", state, &Deps{})

	if err := e.releaseWorkspace("ws"); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
}

func TestReleaseWorkspace_TerminalCleanup_NotTerminated(t *testing.T) {
	// With cleanup=terminal and executor not terminated, workspace stays active-state
	// but is marked closed (worktree is NOT removed).
	state := &State{
		RepoPath: "/repo",

		Workspaces: map[string]WorkspaceState{
			"staging": {
				Name: "staging", Kind: formula.WorkspaceKindStaging,
				Status: "active", Scope: formula.WorkspaceScopeRun,
				Ownership: "owned", Cleanup: formula.WorkspaceCleanupTerminal,
				Dir: "/repo/.worktrees/spi-abc-staging",
			},
		},
		StepStates: make(map[string]StepState),
		Counters:   make(map[string]int),
	}
	e := NewForTest("spi-abc", "wizard-spi-abc", state, &Deps{})
	// e.terminated is false by default

	if err := e.releaseWorkspace("staging"); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	ws := e.state.Workspaces["staging"]
	if ws.Status != "closed" {
		t.Errorf("expected closed, got %q", ws.Status)
	}
	// Dir is preserved because we didn't actually remove the worktree (no real git repo in test)
}

func TestReleaseWorkspace_NeverCleanup(t *testing.T) {
	state := &State{
		RepoPath: "/repo",

		Workspaces: map[string]WorkspaceState{
			"persist": {
				Name: "persist", Kind: formula.WorkspaceKindOwnedWorktree,
				Status: "active", Scope: formula.WorkspaceScopeRun,
				Ownership: "owned", Cleanup: formula.WorkspaceCleanupNever,
				Dir: "/repo/.worktrees/spi-abc-persist",
			},
		},
		StepStates: make(map[string]StepState),
		Counters:   make(map[string]int),
	}
	e := NewForTest("spi-abc", "wizard-spi-abc", state, &Deps{})
	e.terminated = true // even when terminated, "never" means no removal

	if err := e.releaseWorkspace("persist"); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	ws := e.state.Workspaces["persist"]
	if ws.Status != "closed" {
		t.Errorf("expected closed, got %q", ws.Status)
	}
}

// --- releaseStepWorkspaces tests ---

func TestReleaseStepWorkspaces(t *testing.T) {
	state := &State{

		Workspaces: map[string]WorkspaceState{
			"step-ws": {
				Name: "step-ws", Kind: formula.WorkspaceKindRepo,
				Status: "active", Scope: formula.WorkspaceScopeStep,
				Ownership: "owned", Cleanup: formula.WorkspaceCleanupAlways,
			},
			"run-ws": {
				Name: "run-ws", Kind: formula.WorkspaceKindRepo,
				Status: "active", Scope: formula.WorkspaceScopeRun,
				Ownership: "owned", Cleanup: formula.WorkspaceCleanupAlways,
			},
		},
		StepStates: make(map[string]StepState),
		Counters:   make(map[string]int),
	}
	e := NewForTest("spi-abc", "wizard-spi-abc", state, &Deps{})

	if err := e.releaseStepWorkspaces("impl"); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	// step-scoped workspace should be closed.
	if e.state.Workspaces["step-ws"].Status != "closed" {
		t.Errorf("step-ws: expected closed, got %q", e.state.Workspaces["step-ws"].Status)
	}
	// run-scoped workspace should still be active.
	if e.state.Workspaces["run-ws"].Status != "active" {
		t.Errorf("run-ws: expected active, got %q", e.state.Workspaces["run-ws"].Status)
	}
}

// --- releaseRunWorkspaces tests ---

func TestReleaseRunWorkspaces(t *testing.T) {
	state := &State{

		Workspaces: map[string]WorkspaceState{
			"run-ws": {
				Name: "run-ws", Kind: formula.WorkspaceKindRepo,
				Status: "active", Scope: formula.WorkspaceScopeRun,
				Ownership: "owned", Cleanup: formula.WorkspaceCleanupAlways,
			},
			"step-ws": {
				Name: "step-ws", Kind: formula.WorkspaceKindRepo,
				Status: "active", Scope: formula.WorkspaceScopeStep,
				Ownership: "owned", Cleanup: formula.WorkspaceCleanupAlways,
			},
		},
		StepStates: make(map[string]StepState),
		Counters:   make(map[string]int),
	}
	e := NewForTest("spi-abc", "wizard-spi-abc", state, &Deps{})

	if err := e.releaseRunWorkspaces(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	// run-scoped workspace should be closed.
	if e.state.Workspaces["run-ws"].Status != "closed" {
		t.Errorf("run-ws: expected closed, got %q", e.state.Workspaces["run-ws"].Status)
	}
	// step-scoped workspace should still be active.
	if e.state.Workspaces["step-ws"].Status != "active" {
		t.Errorf("step-ws: expected active, got %q", e.state.Workspaces["step-ws"].Status)
	}
}

// --- StepState serialization roundtrip ---

func TestStepState_JSONRoundtrip(t *testing.T) {
	original := StepState{
		Status: "done",
		Outputs: map[string]string{
			"verdict": "approve",
			"sha":     "abc123",
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %s", err)
	}

	var decoded StepState
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %s", err)
	}

	if decoded.Status != original.Status {
		t.Errorf("status = %q, want %q", decoded.Status, original.Status)
	}
	if len(decoded.Outputs) != len(original.Outputs) {
		t.Errorf("outputs length = %d, want %d", len(decoded.Outputs), len(original.Outputs))
	}
	for k, v := range original.Outputs {
		if decoded.Outputs[k] != v {
			t.Errorf("outputs[%q] = %q, want %q", k, decoded.Outputs[k], v)
		}
	}
}

// --- WorkspaceState serialization roundtrip ---

func TestWorkspaceState_JSONRoundtrip(t *testing.T) {
	original := WorkspaceState{
		Name:       "staging",
		Kind:       formula.WorkspaceKindStaging,
		Dir:        "/repo/.worktrees/spi-abc-staging",
		Branch:     "staging/spi-abc",
		BaseBranch: "main",
		StartSHA:   "deadbeef",
		Status:     "active",
		Scope:      formula.WorkspaceScopeRun,
		Ownership:  "owned",
		Cleanup:    formula.WorkspaceCleanupTerminal,
		Origin:     WorkspaceOriginLocalBind,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %s", err)
	}

	var decoded WorkspaceState
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %s", err)
	}

	if decoded != original {
		t.Errorf("roundtrip mismatch:\n  got:  %+v\n  want: %+v", decoded, original)
	}
}

// --- Integration test: resolveWorkspace with real git repo ---

func TestResolveWorkspace_OwnedWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	// Set up a temporary git repo.
	repoDir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("commit", "--allow-empty", "-m", "initial")

	state := &State{
		BeadID:     "spi-abc",
		RepoPath:   repoDir,
		BaseBranch: "main",

		Workspaces: map[string]WorkspaceState{
			"impl": {
				Name:       "impl",
				Kind:       formula.WorkspaceKindOwnedWorktree,
				Branch:     "feat/{vars.bead_id}",
				BaseBranch: "",
				Status:     "pending",
				Scope:      formula.WorkspaceScopeRun,
				Ownership:  "owned",
				Cleanup:    formula.WorkspaceCleanupTerminal,
			},
		},
		StepStates: make(map[string]StepState),
		Counters:   make(map[string]int),
	}
	e := NewForTest("spi-abc", "wizard-spi-abc", state, &Deps{
		ActiveTowerConfig: func() (*TowerConfig, error) { return nil, nil },
	})

	wc, h, err := e.resolveWorkspace("impl")
	if err != nil {
		t.Fatalf("resolveWorkspace: %s", err)
	}
	if wc == nil {
		t.Fatal("expected non-nil WorktreeContext for owned_worktree")
	}
	if h == nil || h.Origin != WorkspaceOriginLocalBind {
		t.Errorf("owned_worktree handle origin = %v, want local-bind", h)
	}

	ws := e.state.Workspaces["impl"]
	if ws.Status != "active" {
		t.Errorf("status = %q, want active", ws.Status)
	}
	if ws.Branch != "feat/spi-abc" {
		t.Errorf("branch = %q, want feat/spi-abc", ws.Branch)
	}
	if ws.Dir == "" {
		t.Error("dir should be set")
	}
	if ws.StartSHA == "" {
		t.Error("start_sha should be set")
	}

	// Verify the worktree directory exists on disk.
	expectedDir := filepath.Join(repoDir, ".worktrees", "spi-abc-impl")
	if ws.Dir != expectedDir {
		t.Errorf("dir = %q, want %q", ws.Dir, expectedDir)
	}
	if _, err := os.Stat(expectedDir); err != nil {
		t.Errorf("worktree dir does not exist: %s", err)
	}

	// Clean up the worktree.
	e.terminated = true
	if err := e.releaseWorkspace("impl"); err != nil {
		t.Fatalf("releaseWorkspace: %s", err)
	}
	if e.state.Workspaces["impl"].Status != "closed" {
		t.Errorf("expected closed after release")
	}
}

// --- Integration test: scope=run resume refreshes StartSHA ---

func TestResolveWorkspace_RunScopeResume(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repoDir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("commit", "--allow-empty", "-m", "initial")

	state := &State{
		BeadID:     "spi-def",
		RepoPath:   repoDir,
		BaseBranch: "main",

		Workspaces: map[string]WorkspaceState{
			"staging": {
				Name:       "staging",
				Kind:       formula.WorkspaceKindOwnedWorktree,
				Branch:     "feat/{vars.bead_id}",
				BaseBranch: "",
				Status:     "pending",
				Scope:      formula.WorkspaceScopeRun,
				Ownership:  "owned",
				Cleanup:    formula.WorkspaceCleanupTerminal,
			},
		},
		StepStates: make(map[string]StepState),
		Counters:   make(map[string]int),
	}
	e := NewForTest("spi-def", "wizard-spi-def", state, &Deps{
		ActiveTowerConfig: func() (*TowerConfig, error) { return nil, nil },
	})

	// First resolve: creates the workspace.
	wc1, _, err := e.resolveWorkspace("staging")
	if err != nil {
		t.Fatalf("first resolve: %s", err)
	}
	sha1 := e.state.Workspaces["staging"].StartSHA

	// Make a commit in the worktree so HEAD moves.
	wtCmd := exec.Command("git", "-C", wc1.Dir, "commit", "--allow-empty", "-m", "second commit")
	wtCmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test")
	if out, err := wtCmd.CombinedOutput(); err != nil {
		t.Fatalf("commit in worktree: %s\n%s", err, out)
	}

	// Second resolve: resumes and refreshes StartSHA.
	_, _, err = e.resolveWorkspace("staging")
	if err != nil {
		t.Fatalf("second resolve: %s", err)
	}
	sha2 := e.state.Workspaces["staging"].StartSHA

	if sha1 == sha2 {
		t.Error("expected StartSHA to be refreshed after commit, but it stayed the same")
	}

	// Clean up.
	e.terminated = true
	e.releaseWorkspace("staging")
}

func TestRebindRunScopedWorkspacesFromDisk_RebindsExistingOwnedWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repoDir := initWorkspaceTestRepo(t)
	beadID := "spi-rebind"
	branch := "feat/" + beadID
	wtDir := filepath.Join(repoDir, ".worktrees", beadID+"-feature")
	runWorkspaceGit(t, repoDir, "branch", branch, "main")
	runWorkspaceGit(t, repoDir, "worktree", "add", wtDir, branch)

	state := &GraphState{
		BeadID:     beadID,
		RepoPath:   repoDir,
		BaseBranch: "main",
		Workspaces: map[string]WorkspaceState{
			"feature": {
				Name:       "feature",
				Kind:       formula.WorkspaceKindOwnedWorktree,
				Branch:     "feat/{vars.bead_id}",
				BaseBranch: "{vars.base_branch}",
				Status:     "pending",
				Scope:      formula.WorkspaceScopeRun,
				Ownership:  "owned",
				Cleanup:    formula.WorkspaceCleanupTerminal,
			},
		},
	}
	graph := &formula.FormulaStepGraph{
		Workspaces: map[string]formula.WorkspaceDecl{
			"feature": {
				Kind:    formula.WorkspaceKindOwnedWorktree,
				Branch:  "feat/{vars.bead_id}",
				Base:    "{vars.base_branch}",
				Scope:   formula.WorkspaceScopeRun,
				Cleanup: formula.WorkspaceCleanupTerminal,
			},
		},
	}

	rebound, err := RebindRunScopedWorkspacesFromDisk(state, graph)
	if err != nil {
		t.Fatalf("RebindRunScopedWorkspacesFromDisk: %v", err)
	}
	if got, want := strings.Join(rebound, ","), "feature"; got != want {
		t.Fatalf("rebound = %q, want %q", got, want)
	}

	ws := state.Workspaces["feature"]
	if ws.Status != "active" {
		t.Errorf("status = %q, want active", ws.Status)
	}
	if ws.Dir != wtDir {
		t.Errorf("dir = %q, want %q", ws.Dir, wtDir)
	}
	if ws.Branch != branch {
		t.Errorf("branch = %q, want %q", ws.Branch, branch)
	}
	if ws.BaseBranch != "main" {
		t.Errorf("base_branch = %q, want main", ws.BaseBranch)
	}
	if ws.StartSHA == "" {
		t.Error("start_sha should be captured")
	}
	if ws.Origin != WorkspaceOriginLocalBind {
		t.Errorf("origin = %q, want %q", ws.Origin, WorkspaceOriginLocalBind)
	}
	if got := runWorkspaceGit(t, wtDir, "branch", "--show-current"); got != branch {
		t.Errorf("worktree branch = %q, want %q", got, branch)
	}
}

func TestRebindRunScopedWorkspacesFromDisk_CorruptWorktreeFailsWithoutMutation(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repoDir := initWorkspaceTestRepo(t)
	beadID := "spi-corrupt"
	branch := "feat/" + beadID
	wtDir := filepath.Join(repoDir, ".worktrees", beadID+"-feature")
	runWorkspaceGit(t, repoDir, "branch", branch, "main")
	runWorkspaceGit(t, repoDir, "worktree", "add", wtDir, branch)
	if err := os.Remove(filepath.Join(wtDir, ".git")); err != nil {
		t.Fatalf("remove worktree .git: %v", err)
	}

	original := WorkspaceState{
		Name:      "feature",
		Kind:      formula.WorkspaceKindOwnedWorktree,
		Branch:    "feat/{vars.bead_id}",
		Status:    "pending",
		Scope:     formula.WorkspaceScopeRun,
		Ownership: "owned",
		Cleanup:   formula.WorkspaceCleanupTerminal,
	}
	state := &GraphState{
		BeadID:     beadID,
		RepoPath:   repoDir,
		BaseBranch: "main",
		Workspaces: map[string]WorkspaceState{"feature": original},
	}
	graph := &formula.FormulaStepGraph{
		Workspaces: map[string]formula.WorkspaceDecl{
			"feature": {
				Kind:    formula.WorkspaceKindOwnedWorktree,
				Branch:  "feat/{vars.bead_id}",
				Base:    "{vars.base_branch}",
				Scope:   formula.WorkspaceScopeRun,
				Cleanup: formula.WorkspaceCleanupTerminal,
			},
		},
	}

	_, err := RebindRunScopedWorkspacesFromDisk(state, graph)
	if err == nil {
		t.Fatal("expected corrupt worktree error, got nil")
	}
	if !strings.Contains(err.Error(), "investigate the worktree") {
		t.Fatalf("error = %v, want actionable worktree message", err)
	}
	if got := state.Workspaces["feature"]; got != original {
		t.Fatalf("workspace mutated on failed rebind:\n got:  %+v\n want: %+v", got, original)
	}
}

// --- ParseFormulaStepGraph defaults test ---

func TestParseFormulaStepGraph_WorkspaceDefaults(t *testing.T) {
	toml := `
name = "test"
version = 3

[workspaces.staging]
kind = "staging"
branch = "staging/{vars.bead_id}"

[steps.entry]
role = "executor"
workspace = "staging"
terminal = true
`
	f, err := formula.ParseFormulaStepGraph([]byte(toml))
	if err != nil {
		t.Fatalf("parse: %s", err)
	}

	ws, ok := f.Workspaces["staging"]
	if !ok {
		t.Fatal("staging workspace not found")
	}
	if ws.Scope != formula.WorkspaceScopeRun {
		t.Errorf("scope = %q, want %q", ws.Scope, formula.WorkspaceScopeRun)
	}
	if ws.Ownership != "owned" {
		t.Errorf("ownership = %q, want %q", ws.Ownership, "owned")
	}
	if ws.Cleanup != formula.WorkspaceCleanupTerminal {
		t.Errorf("cleanup = %q, want %q", ws.Cleanup, formula.WorkspaceCleanupTerminal)
	}

	// Check step fields.
	step := f.Steps["entry"]
	if step.Workspace != "staging" {
		t.Errorf("step workspace = %q, want staging", step.Workspace)
	}
}
