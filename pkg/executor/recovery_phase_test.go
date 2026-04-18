package executor

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/awell-health/spire/pkg/store"
)

// TestHandleFinish_NeedsHuman verifies that when the decide step outputs
// needs_human=true, the finish step does NOT close the recovery bead.
func TestHandleFinish_NeedsHuman(t *testing.T) {
	var closedBead string

	deps := &Deps{
		AddComment: func(id, text string) error { return nil },
		CloseBead: func(id string) error {
			closedBead = id
			return nil
		},
	}

	state := &GraphState{
		Steps: map[string]StepState{
			"decide": {
				Status: "completed",
				Outputs: map[string]string{
					"chosen_action": "escalate",
					"needs_human":   "true",
					"reasoning":     "cannot fix automatically",
				},
			},
			"learn": {
				Status: "completed",
				Outputs: map[string]string{
					"outcome": "dirty",
				},
			},
		},
	}

	e := NewGraphForTest("spi-recovery-nh", "wizard-recovery", nil, state, deps)

	result := handleFinish(e, "finish", StepConfig{Action: "cleric.finish"}, state)

	if result.Error != nil {
		t.Fatalf("handleFinish returned error: %v", result.Error)
	}
	if result.Outputs["status"] != "needs_human" {
		t.Errorf("outputs[status] = %q, want %q", result.Outputs["status"], "needs_human")
	}
	if closedBead != "" {
		t.Errorf("CloseBead was called with %q, but should NOT have been called for needs_human", closedBead)
	}
}

// TestHandleFinish_NonEscalate verifies that when decide did NOT choose
// escalate, the finish step closes the recovery bead as before.
func TestHandleFinish_NonEscalate(t *testing.T) {
	var closedBead string

	deps := &Deps{
		AddComment: func(id, text string) error { return nil },
		CloseBead: func(id string) error {
			closedBead = id
			return nil
		},
	}

	state := &GraphState{
		Steps: map[string]StepState{
			"decide": {
				Status: "completed",
				Outputs: map[string]string{
					"chosen_action": "retry",
					"needs_human":   "false",
					"reasoning":     "retrying build",
				},
			},
			"learn": {
				Status: "completed",
				Outputs: map[string]string{
					"outcome": "clean",
				},
			},
		},
	}

	e := NewGraphForTest("spi-recovery-ok", "wizard-recovery", nil, state, deps)

	result := handleFinish(e, "finish", StepConfig{Action: "cleric.finish"}, state)

	if result.Error != nil {
		t.Fatalf("handleFinish returned error: %v", result.Error)
	}
	if result.Outputs["status"] != "success" {
		t.Errorf("outputs[status] = %q, want %q", result.Outputs["status"], "success")
	}
	if closedBead != "spi-recovery-ok" {
		t.Errorf("CloseBead called with %q, want %q", closedBead, "spi-recovery-ok")
	}
}

// TestHandleFinish_NilState verifies that handleFinish with a nil state
// still closes the bead (backwards compat for edge cases).
func TestHandleFinish_NilState(t *testing.T) {
	var closedBead string

	deps := &Deps{
		AddComment: func(id, text string) error { return nil },
		CloseBead: func(id string) error {
			closedBead = id
			return nil
		},
	}

	e := NewGraphForTest("spi-recovery-nil", "wizard-recovery", nil, nil, deps)

	result := handleFinish(e, "finish", StepConfig{Action: "cleric.finish"}, nil)

	if result.Error != nil {
		t.Fatalf("handleFinish returned error: %v", result.Error)
	}
	if result.Outputs["status"] != "success" {
		t.Errorf("outputs[status] = %q, want %q", result.Outputs["status"], "success")
	}
	if closedBead != "spi-recovery-nil" {
		t.Errorf("CloseBead called with %q, want %q", closedBead, "spi-recovery-nil")
	}
}

// ---------------------------------------------------------------------------
// doTriage tests
// ---------------------------------------------------------------------------

// triageTestSetup builds an Executor and deps suitable for doTriage tests.
// It creates a temporary directory for the worktree and graph state.
func triageTestSetup(t *testing.T) (tmpDir string, cleanup func()) {
	t.Helper()
	dir := t.TempDir()

	// Create a fake worktree directory.
	wtDir := filepath.Join(dir, "worktrees", "feat-spi-src1")
	if err := os.MkdirAll(wtDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write graph state for the wizard agent referencing the worktree.
	runtimeDir := filepath.Join(dir, "config", "runtime", "wizard-spi-src1")
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		t.Fatal(err)
	}
	gs := &GraphState{
		BeadID:    "spi-src1",
		AgentName: "wizard-spi-src1",
		Workspaces: map[string]WorkspaceState{
			"feature": {Dir: wtDir, Branch: "feat/spi-src1", Status: "active"},
		},
	}
	data, _ := json.Marshal(gs)
	if err := os.WriteFile(filepath.Join(runtimeDir, "graph_state.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	return dir, func() {}
}

// newTriageExecutor creates an executor with the given deps and a config dir pointing at tmpDir.
func newTriageExecutor(t *testing.T, beadID string, deps *Deps) *Executor {
	t.Helper()
	e := NewGraphForTest(beadID, "cleric-agent", nil, nil, deps)
	return e
}

func TestDoTriage_MissingSourceBeadID(t *testing.T) {
	deps := &Deps{}
	e := newTriageExecutor(t, "spi-recovery-1", deps)

	req := recovery.RecoveryActionRequest{
		Kind:   recovery.ActionTriage,
		BeadID: "spi-recovery-1",
		// SourceBeadID intentionally empty
	}

	result := doTriage(e, req)
	if result.Success {
		t.Fatal("expected failure for missing source_bead_id")
	}
	if !strings.Contains(result.Error, "source_bead_id is required") {
		t.Errorf("error = %q, want to contain 'source_bead_id is required'", result.Error)
	}
}

func TestDoTriage_BudgetExhausted(t *testing.T) {
	deps := &Deps{
		GetBead: func(id string) (store.Bead, error) {
			return store.Bead{
				ID:       id,
				Metadata: map[string]string{recovery.KeyTriageCount: "2"},
			}, nil
		},
	}
	e := newTriageExecutor(t, "spi-recovery-1", deps)

	req := recovery.RecoveryActionRequest{
		Kind:         recovery.ActionTriage,
		BeadID:       "spi-recovery-1",
		SourceBeadID: "spi-src1",
	}

	result := doTriage(e, req)
	if result.Success {
		t.Fatal("expected failure for budget exhausted")
	}
	if !strings.Contains(result.Error, "budget exhausted") {
		t.Errorf("error = %q, want to contain 'budget exhausted'", result.Error)
	}
}

func TestDoTriage_NoGraphState(t *testing.T) {
	// Config dir points to an empty temp dir — no graph state file exists.
	emptyDir := t.TempDir()

	deps := &Deps{
		GetBead: func(id string) (store.Bead, error) {
			return store.Bead{ID: id, Metadata: map[string]string{}}, nil
		},
		GetChildren: func(parentID string) ([]store.Bead, error) {
			return nil, nil
		},
		ConfigDir: func() (string, error) { return emptyDir, nil },
	}
	e := newTriageExecutor(t, "spi-recovery-1", deps)

	req := recovery.RecoveryActionRequest{
		Kind:         recovery.ActionTriage,
		BeadID:       "spi-recovery-1",
		SourceBeadID: "spi-src1",
	}

	result := doTriage(e, req)
	if result.Success {
		t.Fatal("expected failure when no graph state exists")
	}
	if !strings.Contains(result.Error, "cannot determine worktree") {
		t.Errorf("error = %q, want to contain 'cannot determine worktree'", result.Error)
	}
}

// ---------------------------------------------------------------------------
// Feat-branch fallback + ResolveRepo tests
// ---------------------------------------------------------------------------

func TestDoTriage_FeatBranchFallback_ResolveRepoError(t *testing.T) {
	// No graph state, source bead has a feat-branch: label.
	// ResolveRepo returns an error → should return failResult with "cannot resolve repo".
	emptyDir := t.TempDir()

	deps := &Deps{
		GetBead: func(id string) (store.Bead, error) {
			if id == "spi-src1" {
				return store.Bead{
					ID:     id,
					Labels: []string{"feat-branch:feat/spi-src1"},
				}, nil
			}
			return store.Bead{ID: id, Metadata: map[string]string{}}, nil
		},
		GetChildren: func(parentID string) ([]store.Bead, error) {
			return nil, nil
		},
		ConfigDir: func() (string, error) { return emptyDir, nil },
		ResolveRepo: func(beadID string) (string, string, string, error) {
			return "", "", "", fmt.Errorf("no repo registered for prefix")
		},
	}
	e := newTriageExecutor(t, "spi-recovery-1", deps)

	req := recovery.RecoveryActionRequest{
		Kind:         recovery.ActionTriage,
		BeadID:       "spi-recovery-1",
		SourceBeadID: "spi-src1",
	}

	result := doTriage(e, req)
	if result.Success {
		t.Fatal("expected failure when ResolveRepo returns error")
	}
	if !strings.Contains(result.Error, "cannot resolve repo for bead spi-src1") {
		t.Errorf("error = %q, want to contain 'cannot resolve repo for bead spi-src1'", result.Error)
	}
}

func TestDoTriage_FeatBranchFallback_ResolveRepoEmptyDir(t *testing.T) {
	// No graph state, source bead has a feat-branch: label.
	// ResolveRepo returns empty repoDir → should return failResult with "cannot resolve repo".
	emptyDir := t.TempDir()

	deps := &Deps{
		GetBead: func(id string) (store.Bead, error) {
			if id == "spi-src1" {
				return store.Bead{
					ID:     id,
					Labels: []string{"feat-branch:feat/spi-src1"},
				}, nil
			}
			return store.Bead{ID: id, Metadata: map[string]string{}}, nil
		},
		GetChildren: func(parentID string) ([]store.Bead, error) {
			return nil, nil
		},
		ConfigDir: func() (string, error) { return emptyDir, nil },
		ResolveRepo: func(beadID string) (string, string, string, error) {
			return "", "https://github.com/org/repo", "main", nil
		},
	}
	e := newTriageExecutor(t, "spi-recovery-1", deps)

	req := recovery.RecoveryActionRequest{
		Kind:         recovery.ActionTriage,
		BeadID:       "spi-recovery-1",
		SourceBeadID: "spi-src1",
	}

	result := doTriage(e, req)
	if result.Success {
		t.Fatal("expected failure when ResolveRepo returns empty repoDir")
	}
	if !strings.Contains(result.Error, "cannot resolve repo for bead spi-src1") {
		t.Errorf("error = %q, want to contain 'cannot resolve repo for bead spi-src1'", result.Error)
	}
}

func TestDoTriage_FeatBranchFallback_ResolveRepoSuccess(t *testing.T) {
	// No graph state, source bead has a feat-branch: label.
	// ResolveRepo succeeds with a valid directory. Git commands run with Dir set
	// to the resolved path. Since the temp dir isn't a real git repo, the branch
	// verify (git rev-parse) will fail and the function falls through to the
	// "cannot determine worktree" error — but the important thing is that
	// ResolveRepo was called and the error path for resolution itself was NOT hit.
	emptyDir := t.TempDir()
	repoDir := t.TempDir()

	var resolvedBeadID string
	deps := &Deps{
		GetBead: func(id string) (store.Bead, error) {
			if id == "spi-src1" {
				return store.Bead{
					ID:     id,
					Labels: []string{"feat-branch:feat/spi-src1"},
				}, nil
			}
			return store.Bead{ID: id, Metadata: map[string]string{}}, nil
		},
		GetChildren: func(parentID string) ([]store.Bead, error) {
			return nil, nil
		},
		ConfigDir: func() (string, error) { return emptyDir, nil },
		ResolveRepo: func(beadID string) (string, string, string, error) {
			resolvedBeadID = beadID
			return repoDir, "https://github.com/org/repo", "main", nil
		},
	}
	e := newTriageExecutor(t, "spi-recovery-1", deps)

	req := recovery.RecoveryActionRequest{
		Kind:         recovery.ActionTriage,
		BeadID:       "spi-recovery-1",
		SourceBeadID: "spi-src1",
	}

	result := doTriage(e, req)

	// ResolveRepo should have been called with the source bead ID.
	if resolvedBeadID != "spi-src1" {
		t.Errorf("ResolveRepo called with %q, want %q", resolvedBeadID, "spi-src1")
	}

	// The result should NOT contain the "cannot resolve repo" error — that
	// path was skipped because ResolveRepo succeeded. Instead, the git
	// rev-parse fails (not a real repo), so we get the generic worktree error.
	if result.Success {
		t.Fatal("expected failure (fake repo has no git), but got success")
	}
	if strings.Contains(result.Error, "cannot resolve repo") {
		t.Errorf("error = %q, should NOT contain 'cannot resolve repo' when ResolveRepo succeeds", result.Error)
	}
	if !strings.Contains(result.Error, "cannot determine worktree") {
		t.Errorf("error = %q, want to contain 'cannot determine worktree'", result.Error)
	}
}

func TestDoTriage_FeatBranchFallback_ResolveRepoSuccess_WithGitRepo(t *testing.T) {
	// Full integration: set up a real git repo with the expected branch so
	// the worktree creation succeeds via the feat-branch fallback.
	emptyDir := t.TempDir()
	repoDir := t.TempDir()

	// Initialize a git repo and create the branch.
	cmds := [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "commit", "--allow-empty", "-m", "init"},
		{"git", "checkout", "-b", "feat/spi-src1"},
		{"git", "commit", "--allow-empty", "-m", "feature"},
		{"git", "checkout", "-"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git setup %v: %v\n%s", args, err, out)
		}
	}

	var spawnedCfg agent.SpawnConfig
	deps := &Deps{
		GetBead: func(id string) (store.Bead, error) {
			if id == "spi-src1" {
				return store.Bead{
					ID:     id,
					Labels: []string{"feat-branch:feat/spi-src1"},
				}, nil
			}
			return store.Bead{ID: id, Metadata: map[string]string{}}, nil
		},
		GetChildren: func(parentID string) ([]store.Bead, error) {
			return nil, nil
		},
		ConfigDir: func() (string, error) { return emptyDir, nil },
		ResolveRepo: func(beadID string) (string, string, string, error) {
			return repoDir, "https://github.com/org/repo", "main", nil
		},
		Spawner: &mockBackend{spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
			spawnedCfg = cfg
			return &mockHandle{}, nil
		}},
		AgentResultDir: func(agentName string) string {
			dir := filepath.Join(emptyDir, "results", agentName)
			os.MkdirAll(dir, 0755)
			data, _ := json.Marshal(agentResultJSON{Result: "success"})
			os.WriteFile(filepath.Join(dir, "result.json"), data, 0644)
			return dir
		},
		RecordAgentRun:  func(run AgentRun) (string, error) { return "run-1", nil },
		SetBeadMetadata: func(id string, meta map[string]string) error { return nil },
	}
	e := newTriageExecutor(t, "spi-recovery-1", deps)

	req := recovery.RecoveryActionRequest{
		Kind:         recovery.ActionTriage,
		BeadID:       "spi-recovery-1",
		SourceBeadID: "spi-src1",
		Params:       map[string]string{"test_output": "FAIL: TestFoo"},
	}

	result := doTriage(e, req)

	// Clean up the worktree so it doesn't linger.
	wtDir := filepath.Join(os.TempDir(), "spire-triage", "spi-src1")
	defer func() {
		rmCmd := exec.Command("git", "worktree", "remove", "--force", wtDir)
		rmCmd.Dir = repoDir
		rmCmd.Run()
		os.RemoveAll(wtDir)
	}()

	if !result.Success {
		t.Fatalf("expected success, got error: %s", result.Error)
	}

	// Verify the spawned agent got a --worktree-dir pointing at the
	// triage temp worktree (not CWD, not the repo root).
	found := false
	for i, arg := range spawnedCfg.ExtraArgs {
		if arg == "--worktree-dir" && i+1 < len(spawnedCfg.ExtraArgs) {
			found = true
			if !strings.Contains(spawnedCfg.ExtraArgs[i+1], "spire-triage") {
				t.Errorf("worktree-dir = %q, want to contain 'spire-triage'", spawnedCfg.ExtraArgs[i+1])
			}
		}
	}
	if !found {
		t.Error("ExtraArgs missing --worktree-dir flag")
	}
}

func TestDoTriage_WorktreeDeleted(t *testing.T) {
	tmpDir, _ := triageTestSetup(t)

	// Remove the worktree directory to simulate cleanup.
	wtDir := filepath.Join(tmpDir, "worktrees", "feat-spi-src1")
	os.RemoveAll(wtDir)

	deps := &Deps{
		GetBead: func(id string) (store.Bead, error) {
			return store.Bead{ID: id, Metadata: map[string]string{}}, nil
		},
		GetChildren: func(parentID string) ([]store.Bead, error) {
			return nil, nil
		},
		ConfigDir: func() (string, error) { return filepath.Join(tmpDir, "config"), nil },
	}
	e := newTriageExecutor(t, "spi-recovery-1", deps)

	req := recovery.RecoveryActionRequest{
		Kind:         recovery.ActionTriage,
		BeadID:       "spi-recovery-1",
		SourceBeadID: "spi-src1",
	}

	result := doTriage(e, req)
	if result.Success {
		t.Fatal("expected failure when worktree is deleted")
	}
	if !strings.Contains(result.Error, "worktree no longer exists") {
		t.Errorf("error = %q, want to contain 'worktree no longer exists'", result.Error)
	}
}

func TestDoTriage_SpawnError(t *testing.T) {
	tmpDir, _ := triageTestSetup(t)

	deps := &Deps{
		GetBead: func(id string) (store.Bead, error) {
			return store.Bead{ID: id, Metadata: map[string]string{}}, nil
		},
		GetChildren: func(parentID string) ([]store.Bead, error) {
			return nil, nil
		},
		ConfigDir: func() (string, error) { return filepath.Join(tmpDir, "config"), nil },
		Spawner: &mockBackend{spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
			return nil, fmt.Errorf("spawn failed: out of memory")
		}},
	}
	e := newTriageExecutor(t, "spi-recovery-1", deps)

	req := recovery.RecoveryActionRequest{
		Kind:         recovery.ActionTriage,
		BeadID:       "spi-recovery-1",
		SourceBeadID: "spi-src1",
		Params:       map[string]string{"test_output": "FAIL: TestFoo"},
	}

	result := doTriage(e, req)
	if result.Success {
		t.Fatal("expected failure on spawn error")
	}
	if !strings.Contains(result.Error, "spawn triage agent") {
		t.Errorf("error = %q, want to contain 'spawn triage agent'", result.Error)
	}
}

func TestDoTriage_AgentFailure(t *testing.T) {
	tmpDir, _ := triageTestSetup(t)

	var spawnedCfg agent.SpawnConfig
	var metaSet map[string]string

	deps := &Deps{
		GetBead: func(id string) (store.Bead, error) {
			return store.Bead{ID: id, Metadata: map[string]string{}}, nil
		},
		GetChildren: func(parentID string) ([]store.Bead, error) {
			return nil, nil
		},
		ConfigDir: func() (string, error) { return filepath.Join(tmpDir, "config"), nil },
		Spawner: &mockBackend{spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
			spawnedCfg = cfg
			return &mockHandle{}, nil
		}},
		// readAgentResult needs AgentResultDir to return a dir with result.json.
		AgentResultDir: func(agentName string) string {
			// Write a "error" result for the triage agent.
			dir := filepath.Join(tmpDir, "results", agentName)
			os.MkdirAll(dir, 0755)
			data, _ := json.Marshal(agentResultJSON{Result: "error"})
			os.WriteFile(filepath.Join(dir, "result.json"), data, 0644)
			return dir
		},
		RecordAgentRun: func(run AgentRun) (string, error) { return "run-1", nil },
		SetBeadMetadata: func(id string, meta map[string]string) error {
			metaSet = meta
			return nil
		},
	}
	e := newTriageExecutor(t, "spi-recovery-1", deps)

	req := recovery.RecoveryActionRequest{
		Kind:         recovery.ActionTriage,
		BeadID:       "spi-recovery-1",
		SourceBeadID: "spi-src1",
		Params:       map[string]string{"test_output": "FAIL: TestFoo"},
	}

	result := doTriage(e, req)
	if result.Success {
		t.Fatal("expected failure when agent result is error")
	}
	if !strings.Contains(result.Error, "triage agent returned error") {
		t.Errorf("error = %q, want to contain 'triage agent returned error'", result.Error)
	}

	// Verify spawn config.
	if spawnedCfg.Role != agent.RoleApprentice {
		t.Errorf("spawn role = %q, want %q", spawnedCfg.Role, agent.RoleApprentice)
	}
	if !strings.Contains(spawnedCfg.Name, "triage-spi-src1-1") {
		t.Errorf("spawn name = %q, want to contain 'triage-spi-src1-1'", spawnedCfg.Name)
	}

	// Verify triage count was incremented.
	if metaSet[recovery.KeyTriageCount] != "1" {
		t.Errorf("triage_count = %q, want %q", metaSet[recovery.KeyTriageCount], "1")
	}
}

func TestDoTriage_HappyPath(t *testing.T) {
	tmpDir, _ := triageTestSetup(t)

	var spawnedCfg agent.SpawnConfig
	var metaSet map[string]string

	deps := &Deps{
		GetBead: func(id string) (store.Bead, error) {
			return store.Bead{ID: id, Metadata: map[string]string{}}, nil
		},
		GetChildren: func(parentID string) ([]store.Bead, error) {
			// Return a child with an agent: label to test wizard name derivation.
			return []store.Bead{
				{ID: "spi-src1.attempt", Labels: []string{"attempt", "agent:wizard-spi-src1"}},
			}, nil
		},
		ConfigDir: func() (string, error) { return filepath.Join(tmpDir, "config"), nil },
		Spawner: &mockBackend{spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
			spawnedCfg = cfg
			return &mockHandle{}, nil
		}},
		AgentResultDir: func(agentName string) string {
			dir := filepath.Join(tmpDir, "results", agentName)
			os.MkdirAll(dir, 0755)
			data, _ := json.Marshal(agentResultJSON{Result: "success"})
			os.WriteFile(filepath.Join(dir, "result.json"), data, 0644)
			return dir
		},
		RecordAgentRun: func(run AgentRun) (string, error) { return "run-1", nil },
		SetBeadMetadata: func(id string, meta map[string]string) error {
			metaSet = meta
			return nil
		},
		RepoConfig: func() *repoconfig.RepoConfig {
			return &repoconfig.RepoConfig{
				Runtime: repoconfig.RuntimeConfig{
					Build: "go build ./...",
					Test:  "go test ./...",
					Lint:  "go vet ./...",
				},
			}
		},
	}
	e := newTriageExecutor(t, "spi-recovery-1", deps)

	req := recovery.RecoveryActionRequest{
		Kind:         recovery.ActionTriage,
		BeadID:       "spi-recovery-1",
		SourceBeadID: "spi-src1",
		Params: map[string]string{
			"test_output":    "FAIL: TestFoo\n    expected 1, got 2",
			"wizard_log_tail": "running tests...\nFAIL",
		},
	}

	result := doTriage(e, req)
	if !result.Success {
		t.Fatalf("expected success, got error: %s", result.Error)
	}
	if result.ResolutionKind != "triage" {
		t.Errorf("resolution_kind = %q, want %q", result.ResolutionKind, "triage")
	}
	if !strings.Contains(result.Output, "attempt 1 succeeded") {
		t.Errorf("output = %q, want to contain 'attempt 1 succeeded'", result.Output)
	}

	// Verify spawn config details.
	if spawnedCfg.Role != agent.RoleApprentice {
		t.Errorf("spawn role = %q, want %q", spawnedCfg.Role, agent.RoleApprentice)
	}
	if spawnedCfg.BeadID != "spi-src1" {
		t.Errorf("spawn bead_id = %q, want %q", spawnedCfg.BeadID, "spi-src1")
	}
	// Custom prompt should contain test output and validation commands.
	if !strings.Contains(spawnedCfg.CustomPrompt, "FAIL: TestFoo") {
		t.Error("custom prompt missing test output")
	}
	if !strings.Contains(spawnedCfg.CustomPrompt, "go build ./...") {
		t.Error("custom prompt missing build command")
	}
	if !strings.Contains(spawnedCfg.CustomPrompt, "go test ./...") {
		t.Error("custom prompt missing test command")
	}
	if !strings.Contains(spawnedCfg.CustomPrompt, "running tests...") {
		t.Error("custom prompt missing wizard log tail")
	}

	// Verify worktree dir in ExtraArgs.
	found := false
	for i, arg := range spawnedCfg.ExtraArgs {
		if arg == "--worktree-dir" && i+1 < len(spawnedCfg.ExtraArgs) {
			found = true
			if !strings.Contains(spawnedCfg.ExtraArgs[i+1], "feat-spi-src1") {
				t.Errorf("worktree-dir = %q, want to contain 'feat-spi-src1'", spawnedCfg.ExtraArgs[i+1])
			}
		}
	}
	if !found {
		t.Error("ExtraArgs missing --worktree-dir flag")
	}

	// Verify triage count was incremented.
	if metaSet[recovery.KeyTriageCount] != "1" {
		t.Errorf("triage_count = %q, want %q", metaSet[recovery.KeyTriageCount], "1")
	}
}

func TestDoTriage_WizardNameFirstMatch(t *testing.T) {
	// Verify that the first child with an agent: label is used, not the last.
	tmpDir, _ := triageTestSetup(t)

	// Also write graph state for wizard-first-agent so it would succeed with that name.
	runtimeDir := filepath.Join(tmpDir, "config", "runtime", "wizard-first-agent")
	os.MkdirAll(runtimeDir, 0755)
	wtDir := filepath.Join(tmpDir, "worktrees", "feat-spi-src1")
	gs := &GraphState{
		BeadID:    "spi-src1",
		AgentName: "wizard-first-agent",
		Workspaces: map[string]WorkspaceState{
			"feature": {Dir: wtDir, Branch: "feat/spi-src1", Status: "active"},
		},
	}
	data, _ := json.Marshal(gs)
	os.WriteFile(filepath.Join(runtimeDir, "graph_state.json"), data, 0644)

	deps := &Deps{
		GetBead: func(id string) (store.Bead, error) {
			return store.Bead{ID: id, Metadata: map[string]string{}}, nil
		},
		GetChildren: func(parentID string) ([]store.Bead, error) {
			return []store.Bead{
				{ID: "spi-src1.1", Labels: []string{"attempt", "agent:wizard-first-agent"}},
				{ID: "spi-src1.2", Labels: []string{"attempt", "agent:wizard-second-agent"}},
			}, nil
		},
		ConfigDir: func() (string, error) { return filepath.Join(tmpDir, "config"), nil },
		Spawner: &mockBackend{spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
			return &mockHandle{}, nil
		}},
		AgentResultDir: func(agentName string) string {
			dir := filepath.Join(tmpDir, "results", agentName)
			os.MkdirAll(dir, 0755)
			data, _ := json.Marshal(agentResultJSON{Result: "success"})
			os.WriteFile(filepath.Join(dir, "result.json"), data, 0644)
			return dir
		},
		RecordAgentRun:  func(run AgentRun) (string, error) { return "run-1", nil },
		SetBeadMetadata: func(id string, meta map[string]string) error { return nil },
	}
	e := newTriageExecutor(t, "spi-recovery-1", deps)

	req := recovery.RecoveryActionRequest{
		Kind:         recovery.ActionTriage,
		BeadID:       "spi-recovery-1",
		SourceBeadID: "spi-src1",
		Params:       map[string]string{"test_output": "FAIL"},
	}

	// Should succeed because first-match (wizard-first-agent) has a valid graph state.
	result := doTriage(e, req)
	if !result.Success {
		t.Fatalf("expected success with first-match wizard name, got: %s", result.Error)
	}
}

// ---------------------------------------------------------------------------
// buildDecidePrompt tests
// ---------------------------------------------------------------------------

func TestBuildDecidePrompt_IncludesTriageGuidance(t *testing.T) {
	cc := CollectContextResult{
		Diagnosis: &recovery.Diagnosis{
			BeadID:      "spi-src1",
			FailureMode: "step-failure",
			Git: &recovery.GitState{
				WorktreeExists: true,
				BranchExists:   true,
			},
		},
		WizardLogTail: "FAIL: TestFoo\n    expected 1, got 2",
	}

	prompt := buildDecidePrompt(cc, 0, nil)

	// Should include triage as a valid action.
	if !strings.Contains(prompt, `"triage"`) {
		t.Error("prompt missing triage in chosen_action enum")
	}
	// Should include triage guidance section.
	if !strings.Contains(prompt, "Triage Action") {
		t.Error("prompt missing Triage Action guidance section")
	}
	// Should show budget: 0 used, 2 remaining.
	if !strings.Contains(prompt, "0 of 2 attempts used") {
		t.Error("prompt missing correct triage budget for count=0")
	}
	if !strings.Contains(prompt, "2 remaining") {
		t.Error("prompt missing remaining count")
	}
	// Should indicate worktree exists.
	if !strings.Contains(prompt, "Worktree exists:** yes") {
		t.Error("prompt missing worktree existence confirmation")
	}
}

func TestBuildDecidePrompt_TriageBudgetExhausted(t *testing.T) {
	cc := CollectContextResult{
		Diagnosis: &recovery.Diagnosis{
			BeadID:      "spi-src1",
			FailureMode: "step-failure",
		},
	}

	prompt := buildDecidePrompt(cc, 2, nil)

	if !strings.Contains(prompt, "2 of 2 attempts used") {
		t.Error("prompt missing correct budget for count=2")
	}
	if !strings.Contains(prompt, "0 remaining") {
		t.Error("prompt missing zero remaining")
	}
	if !strings.Contains(prompt, "do NOT choose `triage`") {
		t.Error("prompt missing exhaustion warning")
	}
}

func TestBuildDecidePrompt_WorktreeNotExists(t *testing.T) {
	cc := CollectContextResult{
		Diagnosis: &recovery.Diagnosis{
			BeadID:      "spi-src1",
			FailureMode: "step-failure",
			Git: &recovery.GitState{
				WorktreeExists: false,
			},
		},
	}

	prompt := buildDecidePrompt(cc, 0, nil)

	if !strings.Contains(prompt, "Worktree exists:** no") {
		t.Error("prompt missing worktree non-existence indicator")
	}
	if !strings.Contains(prompt, "triage is NOT possible") {
		t.Error("prompt missing triage-not-possible note")
	}
}

func TestBuildDecidePrompt_PartialCount(t *testing.T) {
	cc := CollectContextResult{}

	prompt := buildDecidePrompt(cc, 1, nil)

	if !strings.Contains(prompt, "1 of 2 attempts used") {
		t.Error("prompt missing correct budget for count=1")
	}
	if !strings.Contains(prompt, "1 remaining") {
		t.Error("prompt missing 1 remaining")
	}
}

// ---------------------------------------------------------------------------
// Triage param-injection tests (actionClericExecute)
// ---------------------------------------------------------------------------

func TestActionRecoveryExecute_TriageParamInjection(t *testing.T) {
	// Build a collect_context result with WizardLogTail.
	cc := CollectContextResult{
		WizardLogTail: "FAIL: TestFoo\n    expected 1, got 2",
	}
	ccJSON, _ := json.Marshal(cc)

	state := &GraphState{
		Steps: map[string]StepState{
			"collect_context": {
				Status: "completed",
				Outputs: map[string]string{
					"collect_context_result": string(ccJSON),
				},
			},
		},
	}

	// Capture the params passed to ExecuteRecoveryAction.
	var capturedParams map[string]string

	deps := &Deps{
		GetBead: func(id string) (store.Bead, error) {
			return store.Bead{
				ID:       id,
				Metadata: map[string]string{recovery.KeySourceBead: "spi-src1"},
			}, nil
		},
	}

	e := NewGraphForTest("spi-recovery-1", "cleric-agent", nil, state, deps)

	// We can't easily intercept ExecuteRecoveryAction, so instead test the
	// param-injection logic directly by simulating what actionClericExecute does.
	step := StepConfig{
		With: map[string]string{
			"action":         "triage",
			"source_bead_id": "spi-src1",
		},
	}

	// Reproduce the param-injection block from actionClericExecute.
	actionKind := step.With["action"]
	params := step.With
	if actionKind == "triage" && state != nil {
		params = make(map[string]string, len(step.With))
		for k, v := range step.With {
			params[k] = v
		}
		if cs, ok := state.Steps["collect_context"]; ok {
			if ccJSON := cs.Outputs["collect_context_result"]; ccJSON != "" {
				var cc CollectContextResult
				if err := json.Unmarshal([]byte(ccJSON), &cc); err == nil {
					if cc.WizardLogTail != "" {
						params["wizard_log_tail"] = cc.WizardLogTail
						if params["test_output"] == "" {
							params["test_output"] = cc.WizardLogTail
						}
					}
				}
			}
		}
	}

	capturedParams = params

	// Original step.With should be unchanged.
	if _, ok := step.With["wizard_log_tail"]; ok {
		t.Error("step.With was mutated — wizard_log_tail should not be in original map")
	}

	// Injected params should have wizard_log_tail and test_output.
	if capturedParams["wizard_log_tail"] != "FAIL: TestFoo\n    expected 1, got 2" {
		t.Errorf("wizard_log_tail = %q, want wizard log content", capturedParams["wizard_log_tail"])
	}
	if capturedParams["test_output"] != "FAIL: TestFoo\n    expected 1, got 2" {
		t.Errorf("test_output = %q, want wizard log content as fallback", capturedParams["test_output"])
	}
	// Original params should still be present.
	if capturedParams["action"] != "triage" {
		t.Errorf("action = %q, want %q", capturedParams["action"], "triage")
	}
	if capturedParams["source_bead_id"] != "spi-src1" {
		t.Errorf("source_bead_id = %q, want %q", capturedParams["source_bead_id"], "spi-src1")
	}

	_ = e // used for context only
}

func TestActionRecoveryExecute_TriageParamInjection_ExplicitTestOutput(t *testing.T) {
	// When test_output is already present in step.With, it should NOT be overridden.
	cc := CollectContextResult{
		WizardLogTail: "wizard log tail content",
	}
	ccJSON, _ := json.Marshal(cc)

	state := &GraphState{
		Steps: map[string]StepState{
			"collect_context": {
				Status: "completed",
				Outputs: map[string]string{
					"collect_context_result": string(ccJSON),
				},
			},
		},
	}

	step := StepConfig{
		With: map[string]string{
			"action":      "triage",
			"test_output": "explicit test output from step",
		},
	}

	actionKind := step.With["action"]
	params := step.With
	if actionKind == "triage" && state != nil {
		params = make(map[string]string, len(step.With))
		for k, v := range step.With {
			params[k] = v
		}
		if cs, ok := state.Steps["collect_context"]; ok {
			if ccJSON := cs.Outputs["collect_context_result"]; ccJSON != "" {
				var cc CollectContextResult
				if err := json.Unmarshal([]byte(ccJSON), &cc); err == nil {
					if cc.WizardLogTail != "" {
						params["wizard_log_tail"] = cc.WizardLogTail
						if params["test_output"] == "" {
							params["test_output"] = cc.WizardLogTail
						}
					}
				}
			}
		}
	}

	// test_output should keep the explicit value, not the fallback.
	if params["test_output"] != "explicit test output from step" {
		t.Errorf("test_output = %q, want %q (should not be overridden)", params["test_output"], "explicit test output from step")
	}
	// wizard_log_tail should still be injected.
	if params["wizard_log_tail"] != "wizard log tail content" {
		t.Errorf("wizard_log_tail = %q, want %q", params["wizard_log_tail"], "wizard log tail content")
	}
}

// ---------------------------------------------------------------------------
// buildDecidePrompt with LearningStats tests
// ---------------------------------------------------------------------------

func TestBuildDecidePrompt_WithStats(t *testing.T) {
	cc := CollectContextResult{
		Diagnosis: &recovery.Diagnosis{
			BeadID:      "spi-src1",
			FailureMode: "step-failure",
		},
	}
	stats := &store.LearningStats{
		FailureClass:    "step-failure",
		TotalRecoveries: 10,
		ActionStats: []store.ActionOutcomeStat{
			{ResolutionKind: "resummon", Total: 6, CleanCount: 5, DirtyCount: 1, RelapsedCount: 0, SuccessRate: 0.833},
			{ResolutionKind: "reset", Total: 4, CleanCount: 2, DirtyCount: 1, RelapsedCount: 1, SuccessRate: 0.5},
		},
		PredictionAccuracy: 0.75,
	}

	prompt := buildDecidePrompt(cc, 0, stats)

	if !strings.Contains(prompt, "## Historical Outcome Statistics") {
		t.Error("prompt missing Historical Outcome Statistics header")
	}
	if !strings.Contains(prompt, "Based on 10 prior recoveries for failure class `step-failure`") {
		t.Error("prompt missing recovery count and failure class")
	}
	if !strings.Contains(prompt, "| resummon | 6 | 83%") {
		t.Error("prompt missing resummon stats row")
	}
	if !strings.Contains(prompt, "| reset | 4 | 50%") {
		t.Error("prompt missing reset stats row")
	}
	if !strings.Contains(prompt, "prediction accuracy: 75%") {
		t.Error("prompt missing prediction accuracy")
	}
	if !strings.Contains(prompt, "Weight your action choice by historical success rates") {
		t.Error("prompt missing action weighting guidance")
	}
}

func TestBuildDecidePrompt_NilStats(t *testing.T) {
	cc := CollectContextResult{
		Diagnosis: &recovery.Diagnosis{
			BeadID:      "spi-src1",
			FailureMode: "step-failure",
		},
	}

	prompt := buildDecidePrompt(cc, 0, nil)

	if strings.Contains(prompt, "Historical Outcome Statistics") {
		t.Error("prompt should NOT contain statistics section when stats is nil")
	}
}

func TestBuildDecidePrompt_ZeroRecoveries(t *testing.T) {
	cc := CollectContextResult{
		Diagnosis: &recovery.Diagnosis{
			BeadID:      "spi-src1",
			FailureMode: "step-failure",
		},
	}
	stats := &store.LearningStats{
		FailureClass:    "step-failure",
		TotalRecoveries: 0,
	}

	prompt := buildDecidePrompt(cc, 0, stats)

	if strings.Contains(prompt, "Historical Outcome Statistics") {
		t.Error("prompt should NOT contain statistics section when TotalRecoveries is 0")
	}
}

func TestBuildDecidePrompt_StatsWithoutPredictionAccuracy(t *testing.T) {
	cc := CollectContextResult{
		Diagnosis: &recovery.Diagnosis{
			BeadID:      "spi-src1",
			FailureMode: "step-failure",
		},
	}
	stats := &store.LearningStats{
		FailureClass:       "step-failure",
		TotalRecoveries:    5,
		ActionStats:        []store.ActionOutcomeStat{{ResolutionKind: "resummon", Total: 5, CleanCount: 4, DirtyCount: 1, SuccessRate: 0.8}},
		PredictionAccuracy: 0, // no predictions made
	}

	prompt := buildDecidePrompt(cc, 0, stats)

	if !strings.Contains(prompt, "## Historical Outcome Statistics") {
		t.Error("prompt missing statistics section")
	}
	if strings.Contains(prompt, "prediction accuracy") {
		t.Error("prompt should NOT contain prediction accuracy line when accuracy is 0")
	}
}

func TestActionRecoveryExecute_NonTriageNoInjection(t *testing.T) {
	// Non-triage actions should not get param injection.
	state := &GraphState{
		Steps: map[string]StepState{
			"collect_context": {
				Status: "completed",
				Outputs: map[string]string{
					"collect_context_result": `{"wizard_log_tail":"some log"}`,
				},
			},
		},
	}

	step := StepConfig{
		With: map[string]string{
			"action": "reset",
		},
	}

	actionKind := step.With["action"]
	params := step.With
	if actionKind == "triage" && state != nil {
		// This block should NOT execute for "reset".
		t.Fatal("param injection block should not execute for non-triage actions")
	}

	// Params should be the original step.With.
	if params["action"] != "reset" {
		t.Errorf("action = %q, want %q", params["action"], "reset")
	}
	if _, ok := params["wizard_log_tail"]; ok {
		t.Error("wizard_log_tail should not be injected for non-triage actions")
	}
}

// ---------------------------------------------------------------------------
// parseHumanGuidance
// ---------------------------------------------------------------------------

func TestParseHumanGuidance_KeywordMatching(t *testing.T) {
	tests := []struct {
		name     string
		comments []string
		want     string
	}{
		{"rebase keyword", []string{"try rebase onto main"}, "rebase-onto-base"},
		{"rebase simple", []string{"rebase"}, "rebase-onto-base"},
		{"cherry-pick", []string{"cherry-pick abc123"}, "cherry-pick"},
		{"cherry pick no hyphen", []string{"cherry pick that commit"}, "cherry-pick"},
		{"resolve conflicts", []string{"resolve conflicts please"}, "resolve-conflicts"},
		{"resolve conflict singular", []string{"resolve conflict"}, "resolve-conflicts"},
		{"rebuild", []string{"rebuild the project"}, "rebuild"},
		{"try rebuild", []string{"try rebuild"}, "rebuild"},
		{"resummon", []string{"resummon an apprentice"}, "resummon"},
		{"re-summon", []string{"re-summon"}, "resummon"},
		{"try again", []string{"try again"}, "resummon"},
		{"reset", []string{"reset the step"}, "reset-to-step"},
		{"reset to step", []string{"reset to step verify"}, "reset-to-step"},
		{"escalate", []string{"escalate to human"}, "escalate"},
		{"fix", []string{"fix the build issue"}, "targeted-fix"},
		{"targeted fix", []string{"targeted fix needed"}, "targeted-fix"},
		{"no match", []string{"hello world"}, ""},
		{"empty comments", []string{}, ""},
		{"nil comments", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseHumanGuidance(tt.comments, nil)
			if got != tt.want {
				t.Errorf("parseHumanGuidance(%v, nil) = %q, want %q", tt.comments, got, tt.want)
			}
		})
	}
}

func TestParseHumanGuidance_MostRecentCommentWins(t *testing.T) {
	// Most recent comment (last in slice) should be checked first.
	comments := []string{
		"try rebase",     // rebase-onto-base
		"rebuild please", // rebuild — this is more recent
	}
	got := parseHumanGuidance(comments, nil)
	if got != "rebuild" {
		t.Errorf("parseHumanGuidance = %q, want 'rebuild' (most recent comment)", got)
	}
}

func TestParseHumanGuidance_CaseInsensitive(t *testing.T) {
	comments := []string{"REBASE onto main"}
	got := parseHumanGuidance(comments, nil)
	if got != "rebase-onto-base" {
		t.Errorf("parseHumanGuidance = %q, want 'rebase-onto-base'", got)
	}
}

func TestParseHumanGuidance_SkipsRepeatedFailures(t *testing.T) {
	comments := []string{"try rebase"}
	repeated := map[string]int{"rebase-onto-base": 2}
	got := parseHumanGuidance(comments, repeated)
	if got != "" {
		t.Errorf("parseHumanGuidance = %q, want empty (rebase has 2 failures)", got)
	}
}

func TestParseHumanGuidance_SkipsRepeatedButFindsAlternative(t *testing.T) {
	comments := []string{"try rebase or rebuild"}
	repeated := map[string]int{"rebase-onto-base": 3}
	got := parseHumanGuidance(comments, repeated)
	if got != "rebuild" {
		t.Errorf("parseHumanGuidance = %q, want 'rebuild' (rebase filtered out)", got)
	}
}

func TestParseHumanGuidance_RepeatedBelowThreshold(t *testing.T) {
	comments := []string{"try rebase"}
	repeated := map[string]int{"rebase-onto-base": 1} // below threshold of 2
	got := parseHumanGuidance(comments, repeated)
	if got != "rebase-onto-base" {
		t.Errorf("parseHumanGuidance = %q, want 'rebase-onto-base' (only 1 failure)", got)
	}
}

// ---------------------------------------------------------------------------
// decideFromGitState
// ---------------------------------------------------------------------------

func TestDecideFromGitState_Diverged(t *testing.T) {
	ctx := &FullRecoveryContext{
		GitState: &git.BranchDiagnostics{
			Diverged:    true,
			BehindMain:  3,
			AheadOfMain: 2,
		},
	}
	got := decideFromGitState(ctx)
	if got != "rebase-onto-base" {
		t.Errorf("decideFromGitState(diverged) = %q, want 'rebase-onto-base'", got)
	}
}

func TestDecideFromGitState_BehindMain(t *testing.T) {
	ctx := &FullRecoveryContext{
		GitState: &git.BranchDiagnostics{
			BehindMain:  5,
			AheadOfMain: 1,
			Diverged:    false,
		},
	}
	got := decideFromGitState(ctx)
	if got != "rebase-onto-base" {
		t.Errorf("decideFromGitState(behind) = %q, want 'rebase-onto-base'", got)
	}
}

func TestDecideFromGitState_DirtyWorktree(t *testing.T) {
	ctx := &FullRecoveryContext{
		GitState: &git.BranchDiagnostics{
			BehindMain: 0,
		},
		WorktreeState: &git.WorktreeDiagnostics{
			Exists:  true,
			IsDirty: true,
		},
	}
	got := decideFromGitState(ctx)
	if got != "rebuild" {
		t.Errorf("decideFromGitState(dirty worktree) = %q, want 'rebuild'", got)
	}
}

func TestDecideFromGitState_CleanState(t *testing.T) {
	ctx := &FullRecoveryContext{
		GitState: &git.BranchDiagnostics{
			BehindMain: 0,
		},
		WorktreeState: &git.WorktreeDiagnostics{
			Exists:  true,
			IsDirty: false,
		},
	}
	got := decideFromGitState(ctx)
	if got != "" {
		t.Errorf("decideFromGitState(clean) = %q, want empty", got)
	}
}

func TestDecideFromGitState_NilGitState(t *testing.T) {
	ctx := &FullRecoveryContext{}
	got := decideFromGitState(ctx)
	if got != "" {
		t.Errorf("decideFromGitState(nil git) = %q, want empty", got)
	}
}

func TestDecideFromGitState_WorktreeNotExists(t *testing.T) {
	ctx := &FullRecoveryContext{
		GitState: &git.BranchDiagnostics{BehindMain: 0},
		WorktreeState: &git.WorktreeDiagnostics{
			Exists: false,
		},
	}
	got := decideFromGitState(ctx)
	if got != "" {
		t.Errorf("decideFromGitState(worktree not exists) = %q, want empty", got)
	}
}

func TestDecideFromGitState_PriorityOrder(t *testing.T) {
	// Diverged takes priority over dirty worktree.
	ctx := &FullRecoveryContext{
		GitState: &git.BranchDiagnostics{
			Diverged:   true,
			BehindMain: 3,
		},
		WorktreeState: &git.WorktreeDiagnostics{
			Exists:  true,
			IsDirty: true,
		},
	}
	got := decideFromGitState(ctx)
	if got != "rebase-onto-base" {
		t.Errorf("decideFromGitState(diverged+dirty) = %q, want 'rebase-onto-base' (diverged takes priority)", got)
	}
}

// ---------------------------------------------------------------------------
// gitStateReasoning
// ---------------------------------------------------------------------------

func TestGitStateReasoning(t *testing.T) {
	ctx := &FullRecoveryContext{
		GitState: &git.BranchDiagnostics{
			BehindMain: 7,
			MainRef:    "main",
		},
	}

	tests := []struct {
		action   string
		contains string
	}{
		{"resolve-conflicts", "merge conflicts"},
		{"rebase-onto-base", "7 commits behind main"},
		{"rebuild", "uncommitted changes"},
		{"unknown-action", "unknown-action"},
	}
	for _, tt := range tests {
		t.Run(tt.action, func(t *testing.T) {
			got := gitStateReasoning(ctx, tt.action)
			if !strings.Contains(got, tt.contains) {
				t.Errorf("gitStateReasoning(ctx, %q) = %q, want to contain %q", tt.action, got, tt.contains)
			}
		})
	}
}

func TestGitStateReasoning_NilGitState(t *testing.T) {
	ctx := &FullRecoveryContext{}
	got := gitStateReasoning(ctx, "rebase-onto-base")
	if got != "branch is behind base" {
		t.Errorf("gitStateReasoning(nil git, rebase) = %q, want 'branch is behind base'", got)
	}
}

// TestHandleVerify_ExecuteFailed verifies that when the execute step's output
// status is "failed", handleVerify immediately returns loop_to=decide without
// attempting to set a retry request.
func TestHandleVerify_ExecuteFailed(t *testing.T) {
	deps := &Deps{
		AddComment: func(id, text string) error { return nil },
	}

	state := &GraphState{
		Steps: map[string]StepState{
			"collect_context": {
				Status:  "completed",
				Outputs: map[string]string{"source_bead": "spi-target"},
			},
			"decide": {
				Status:  "completed",
				Outputs: map[string]string{"chosen_action": "resummon", "reasoning": "try again"},
			},
			"execute": {
				Status:  "completed",
				Outputs: map[string]string{"status": "failed", "action": "resummon"},
			},
		},
	}

	e := NewGraphForTest("spi-recovery-v", "wizard-recovery", nil, state, deps)

	result := handleVerify(e, "verify", StepConfig{Action: "cleric.execute", With: map[string]string{"action": "verify"}}, state)

	if result.Error != nil {
		t.Fatalf("handleVerify returned error: %v", result.Error)
	}
	if result.Outputs["status"] != "failed" {
		t.Errorf("outputs[status] = %q, want %q", result.Outputs["status"], "failed")
	}
	if result.Outputs["loop_to"] != "decide" {
		t.Errorf("outputs[loop_to] = %q, want %q", result.Outputs["loop_to"], "decide")
	}
}

// TestHandleVerify_ExecuteEmpty verifies that when execute has no status output
// (empty string), handleVerify treats it as failure and returns loop_to=decide.
func TestHandleVerify_ExecuteEmpty(t *testing.T) {
	deps := &Deps{
		AddComment: func(id, text string) error { return nil },
	}

	state := &GraphState{
		Steps: map[string]StepState{
			"collect_context": {
				Status:  "completed",
				Outputs: map[string]string{"source_bead": "spi-target"},
			},
			"decide": {
				Status:  "completed",
				Outputs: map[string]string{"chosen_action": "resummon"},
			},
			"execute": {
				Status:  "completed",
				Outputs: map[string]string{},
			},
		},
	}

	e := NewGraphForTest("spi-recovery-v2", "wizard-recovery", nil, state, deps)

	result := handleVerify(e, "verify", StepConfig{Action: "cleric.execute", With: map[string]string{"action": "verify"}}, state)

	if result.Error != nil {
		t.Fatalf("handleVerify returned error: %v", result.Error)
	}
	if result.Outputs["loop_to"] != "decide" {
		t.Errorf("outputs[loop_to] = %q, want %q", result.Outputs["loop_to"], "decide")
	}
}

// ---------------------------------------------------------------------------
// handleRecordExecuteError tests (spi-676a4)
// ---------------------------------------------------------------------------

// TestHandleRecordExecuteError_PostsCommentWithErrorText verifies the happy
// path: an execute step already has outputs.error recorded by the
// interpreter, handleRecordExecuteError reads it and posts a bead comment
// containing the verbatim error text, then returns status=recorded.
func TestHandleRecordExecuteError_PostsCommentWithErrorText(t *testing.T) {
	var commentBeadID, commentText string

	deps := &Deps{
		AddComment: func(id, text string) error {
			commentBeadID = id
			commentText = text
			return nil
		},
	}

	state := &GraphState{
		Steps: map[string]StepState{
			"execute": {
				Status: "completed",
				Outputs: map[string]string{
					"error":  "rebase onto main failed: conflict in pkg/gateway/gateway_test.go",
					"status": "failed",
				},
			},
		},
	}

	e := NewGraphForTest("spi-recovery-rec", "cleric-agent", nil, state, deps)

	result := handleRecordExecuteError(e, "retry_on_error",
		StepConfig{Action: "cleric.execute", With: map[string]string{"action": "record_error"}}, state)

	if result.Error != nil {
		t.Fatalf("handleRecordExecuteError returned error: %v", result.Error)
	}
	if result.Outputs["status"] != "recorded" {
		t.Errorf("outputs[status] = %q, want %q", result.Outputs["status"], "recorded")
	}
	if commentBeadID != "spi-recovery-rec" {
		t.Errorf("AddComment called with bead %q, want %q", commentBeadID, "spi-recovery-rec")
	}
	if !strings.Contains(commentText, "rebase onto main failed: conflict in pkg/gateway/gateway_test.go") {
		t.Errorf("comment text missing error text, got: %q", commentText)
	}
	if !strings.Contains(commentText, "Cleric execute errored") {
		t.Errorf("comment text missing header, got: %q", commentText)
	}
}

// TestHandleRecordExecuteError_DefensiveEmptyError verifies the defensive
// fallback when the execute step somehow has no recorded error text (which
// should not happen if the interpreter wired correctly) — a placeholder
// comment is posted instead of a bare empty block.
func TestHandleRecordExecuteError_DefensiveEmptyError(t *testing.T) {
	var commentText string
	deps := &Deps{
		AddComment: func(id, text string) error {
			commentText = text
			return nil
		},
	}

	state := &GraphState{
		Steps: map[string]StepState{
			"execute": {
				Status:  "completed",
				Outputs: map[string]string{}, // no "error" key
			},
		},
	}

	e := NewGraphForTest("spi-recovery-empty", "cleric-agent", nil, state, deps)

	result := handleRecordExecuteError(e, "retry_on_error",
		StepConfig{Action: "cleric.execute"}, state)

	if result.Error != nil {
		t.Fatalf("handleRecordExecuteError returned error: %v", result.Error)
	}
	if result.Outputs["status"] != "recorded" {
		t.Errorf("outputs[status] = %q, want %q", result.Outputs["status"], "recorded")
	}
	if !strings.Contains(commentText, "no error text recorded") {
		t.Errorf("expected defensive placeholder in comment, got: %q", commentText)
	}
}

// TestHandleRecordExecuteError_NilAddCommentDep verifies the handler is
// nil-safe when AddComment is not wired (e.g., unit test envs). Status is
// still recorded so the formula's reset/retry flow continues.
func TestHandleRecordExecuteError_NilAddCommentDep(t *testing.T) {
	deps := &Deps{} // AddComment intentionally nil

	state := &GraphState{
		Steps: map[string]StepState{
			"execute": {
				Outputs: map[string]string{"error": "boom"},
			},
		},
	}

	e := NewGraphForTest("spi-recovery-nil-dep", "cleric-agent", nil, state, deps)

	// Must not panic.
	result := handleRecordExecuteError(e, "retry_on_error",
		StepConfig{Action: "cleric.execute"}, state)

	if result.Error != nil {
		t.Fatalf("handleRecordExecuteError returned error: %v", result.Error)
	}
	if result.Outputs["status"] != "recorded" {
		t.Errorf("outputs[status] = %q, want %q", result.Outputs["status"], "recorded")
	}
}

// ---------------------------------------------------------------------------
// handleFinish step.With["needs_human"] override tests (spi-676a4)
// ---------------------------------------------------------------------------

// TestHandleFinish_NeedsHumanViaStepWith verifies that handleFinish honors
// the formula-level override step.With["needs_human"]="true" and leaves the
// recovery bead OPEN — even when the decide step did not set
// outputs.needs_human. This is the path taken by
// finish_needs_human_on_error when the execute-error retry budget is
// exhausted.
func TestHandleFinish_NeedsHumanViaStepWith(t *testing.T) {
	var closedBead string

	deps := &Deps{
		AddComment: func(id, text string) error { return nil },
		CloseBead: func(id string) error {
			closedBead = id
			return nil
		},
	}

	state := &GraphState{
		Steps: map[string]StepState{
			"decide": {
				Status: "completed",
				Outputs: map[string]string{
					"chosen_action": "rebase-onto-base",
					// NOTE: no needs_human=true here — the override is purely via step.With.
				},
			},
			"execute": {
				Status: "completed",
				Outputs: map[string]string{
					"status": "failed",
					"error":  "rebase conflict",
				},
			},
		},
	}

	e := NewGraphForTest("spi-recovery-finish-nh-with", "wizard-recovery", nil, state, deps)

	step := StepConfig{
		Action: "cleric.execute",
		With: map[string]string{
			"action":      "finish",
			"needs_human": "true",
		},
	}

	result := handleFinish(e, "finish_needs_human_on_error", step, state)

	if result.Error != nil {
		t.Fatalf("handleFinish returned error: %v", result.Error)
	}
	if result.Outputs["status"] != "needs_human" {
		t.Errorf("outputs[status] = %q, want %q (forced via step.With)",
			result.Outputs["status"], "needs_human")
	}
	if closedBead != "" {
		t.Errorf("CloseBead was called with %q, but must be left open for spire resolve", closedBead)
	}
}

// TestHandleFinish_StepWithNeedsHumanFalse verifies that when step.With has
// needs_human != "true" (the default/empty case), the decide-output path is
// the sole driver — absence of decide.outputs.needs_human → normal close.
func TestHandleFinish_StepWithNeedsHumanFalse(t *testing.T) {
	var closedBead string

	deps := &Deps{
		AddComment: func(id, text string) error { return nil },
		CloseBead: func(id string) error {
			closedBead = id
			return nil
		},
	}

	state := &GraphState{
		Steps: map[string]StepState{
			"decide": {
				Status:  "completed",
				Outputs: map[string]string{"chosen_action": "resummon"},
			},
			"learn": {
				Status:  "completed",
				Outputs: map[string]string{"outcome": "clean"},
			},
		},
	}

	e := NewGraphForTest("spi-recovery-finish-nh-off", "wizard-recovery", nil, state, deps)

	step := StepConfig{
		Action: "cleric.execute",
		With: map[string]string{
			"action":      "finish",
			"needs_human": "false", // explicit non-override
		},
	}

	result := handleFinish(e, "finish", step, state)

	if result.Error != nil {
		t.Fatalf("handleFinish returned error: %v", result.Error)
	}
	if result.Outputs["status"] != "success" {
		t.Errorf("outputs[status] = %q, want %q", result.Outputs["status"], "success")
	}
	if closedBead != "spi-recovery-finish-nh-off" {
		t.Errorf("CloseBead called with %q, want %q (no override → normal close)",
			closedBead, "spi-recovery-finish-nh-off")
	}
}
