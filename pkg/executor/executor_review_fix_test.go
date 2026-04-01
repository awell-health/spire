package executor

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/formula"
	spgit "github.com/awell-health/spire/pkg/git"
)

// TestDispatchFix_StagingDirect_SpawnsWithWorktreeDir verifies that when the
// executor has a persisted staging worktree, dispatchFix routes to fixInStaging
// which spawns wizard-run --review-fix --apprentice --worktree-dir <staging>.
// The post-fix merge is skipped because the wizard committed on staging.
func TestDispatchFix_StagingDirect_SpawnsWithWorktreeDir(t *testing.T) {
	repoDir := initSeamTestRepo(t)
	configDir := t.TempDir()

	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// Create staging branch and worktree on disk.
	runGit("branch", "staging/spi-test")
	wtDir := filepath.Join(t.TempDir(), "staging-wt")
	runGit("worktree", "add", wtDir, "staging/spi-test")

	// Pre-hydrate stagingWt so ensureStagingWorktree finds it.
	sw := spgit.ResumeStagingWorktree(repoDir, wtDir, "staging/spi-test", "main", nil)

	spawnCalled := false
	var capturedArgs []string
	fakeSpawner := &fakeTestBackend{
		spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
			spawnCalled = true
			capturedArgs = cfg.ExtraArgs
			return &fakeTestHandle{}, nil
		},
	}

	deps := &Deps{
		ConfigDir:         func() (string, error) { return configDir, nil },
		Spawner:           fakeSpawner,
		AddLabel:          func(id, label string) error { return nil },
		ActiveTowerConfig: func() (*TowerConfig, error) { return nil, fmt.Errorf("no tower") },
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: "in_progress"}, nil
		},
		HasLabel:      func(b Bead, prefix string) string { return "" },
		ContainsLabel: func(b Bead, label string) bool { return false },
	}

	f := &formula.FormulaV2{
		Name:    "test",
		Version: 2,
		Phases: map[string]formula.PhaseConfig{
			"implement": {Role: "apprentice", Model: "test-model"},
			"review":    {Role: "sage"},
		},
	}

	state := &State{
		BeadID:        "spi-test",
		AgentName:     "wizard-test",
		Phase:         "review",
		WorktreeDir:   wtDir,
		StagingBranch: "staging/spi-test",
		BaseBranch:    "main",
		RepoPath:      repoDir,
		Subtasks:      make(map[string]SubtaskState),
	}

	e := NewForTest("spi-test", "wizard-test", f, state, deps)
	e.stagingWt = sw

	stepCfg := formula.StepConfig{Role: "apprentice", Model: "test-model"}
	pc := formula.PhaseConfig{Role: "sage"}
	err := e.dispatchFix(stepCfg, pc)
	if err != nil {
		t.Fatalf("dispatchFix returned error: %v", err)
	}

	if !spawnCalled {
		t.Fatal("expected spawner to be called")
	}

	// Verify --worktree-dir is passed to the subprocess.
	hasWorktreeDir := false
	for i, arg := range capturedArgs {
		if arg == "--worktree-dir" && i+1 < len(capturedArgs) {
			hasWorktreeDir = true
			if capturedArgs[i+1] != wtDir {
				t.Errorf("expected worktree dir %s, got %s", wtDir, capturedArgs[i+1])
			}
		}
	}
	if !hasWorktreeDir {
		t.Errorf("expected --worktree-dir in spawn args, got %v", capturedArgs)
	}

	if e.state.Phase != "review" {
		t.Errorf("expected phase restored to review, got %s", e.state.Phase)
	}
}

// TestDispatchFix_ResumedExecutor_HydratesWorktree verifies the resumed-executor
// path: WorktreeDir is set in persisted state but e.stagingWt is nil. fixInStaging
// must hydrate via ensureStagingWorktree() before spawning.
func TestDispatchFix_ResumedExecutor_HydratesWorktree(t *testing.T) {
	repoDir := initSeamTestRepo(t)
	configDir := t.TempDir()

	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	runGit("branch", "staging/spi-resumed")
	wtDir := filepath.Join(t.TempDir(), "staging-wt")
	runGit("worktree", "add", wtDir, "staging/spi-resumed")

	spawnCalled := false
	fakeSpawner := &fakeTestBackend{
		spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
			spawnCalled = true
			return &fakeTestHandle{}, nil
		},
	}

	deps := &Deps{
		ConfigDir:         func() (string, error) { return configDir, nil },
		Spawner:           fakeSpawner,
		AddLabel:          func(id, label string) error { return nil },
		ActiveTowerConfig: func() (*TowerConfig, error) { return nil, fmt.Errorf("no tower") },
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: "in_progress"}, nil
		},
		HasLabel:      func(b Bead, prefix string) string { return "" },
		ContainsLabel: func(b Bead, label string) bool { return false },
	}

	f := &formula.FormulaV2{
		Name:    "test",
		Version: 2,
		Phases: map[string]formula.PhaseConfig{
			"implement": {Role: "apprentice", Model: "test-model"},
			"review":    {Role: "sage"},
		},
	}

	state := &State{
		BeadID:        "spi-resumed",
		AgentName:     "wizard-test",
		Phase:         "review",
		WorktreeDir:   wtDir, // persisted from prior session
		StagingBranch: "staging/spi-resumed",
		BaseBranch:    "main",
		RepoPath:      repoDir,
		Subtasks:      make(map[string]SubtaskState),
	}

	e := NewForTest("spi-resumed", "wizard-test", f, state, deps)
	// e.stagingWt is nil — simulates resumed executor.

	stepCfg := formula.StepConfig{Role: "apprentice"}
	pc := formula.PhaseConfig{Role: "sage"}
	err := e.dispatchFix(stepCfg, pc)
	if err != nil {
		t.Fatalf("dispatchFix returned error: %v", err)
	}

	if !spawnCalled {
		t.Error("expected spawner to be called after worktree hydration")
	}

	// stagingWt should now be hydrated.
	if e.stagingWt == nil {
		t.Error("expected stagingWt to be hydrated after fixInStaging")
	}
}

// TestDispatchFix_NoStagingWorktree_SpawnsSubprocess verifies that when no
// staging worktree exists, dispatchFix spawns a wizard-run subprocess on a
// feature branch (legacy path).
func TestDispatchFix_NoStagingWorktree_SpawnsSubprocess(t *testing.T) {
	dir := initSeamTestRepo(t)
	configDir := t.TempDir()

	spawnCalled := false
	fakeSpawner := &fakeTestBackend{
		spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
			spawnCalled = true
			for _, arg := range cfg.ExtraArgs {
				if arg == "--worktree-dir" {
					t.Error("did not expect --worktree-dir when no staging worktree")
				}
			}
			return &fakeTestHandle{}, nil
		},
	}

	deps := &Deps{
		ConfigDir: func() (string, error) { return configDir, nil },
		Spawner:   fakeSpawner,
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: "in_progress"}, nil
		},
		HasLabel:      func(b Bead, prefix string) string { return "" },
		ContainsLabel: func(b Bead, label string) bool { return false },
		ResolveBranch: func(beadID string) string { return "feat/" + beadID },
	}

	f := &formula.FormulaV2{
		Name:    "test",
		Version: 2,
		Phases: map[string]formula.PhaseConfig{
			"implement": {Role: "apprentice", Model: "test-model"},
			"review":    {Role: "sage"},
		},
	}

	state := &State{
		BeadID:    "spi-test",
		AgentName: "wizard-test",
		Phase:     "review",
		Subtasks:  make(map[string]SubtaskState),
		RepoPath:  dir,
	}

	e := NewForTest("spi-test", "wizard-test", f, state, deps)

	stepCfg := formula.StepConfig{Role: "apprentice"}
	pc := formula.PhaseConfig{Role: "sage"}
	err := e.dispatchFix(stepCfg, pc)
	if err != nil {
		t.Fatalf("dispatchFix returned error: %v", err)
	}

	if !spawnCalled {
		t.Error("expected spawner to be called for subprocess path")
	}
}

// --- Test fakes ---

type fakeTestBackend struct {
	spawnFn func(cfg agent.SpawnConfig) (agent.Handle, error)
}

func (f *fakeTestBackend) Spawn(cfg agent.SpawnConfig) (agent.Handle, error) {
	if f.spawnFn != nil {
		return f.spawnFn(cfg)
	}
	return &fakeTestHandle{}, nil
}
func (f *fakeTestBackend) List() ([]agent.Info, error)            { return nil, nil }
func (f *fakeTestBackend) Logs(name string) (io.ReadCloser, error) { return nil, os.ErrNotExist }
func (f *fakeTestBackend) Kill(name string) error                 { return nil }

type fakeTestHandle struct{}

func (h *fakeTestHandle) Wait() error                { return nil }
func (h *fakeTestHandle) Signal(sig os.Signal) error { return nil }
func (h *fakeTestHandle) Alive() bool                { return false }
func (h *fakeTestHandle) Name() string               { return "fake" }
func (h *fakeTestHandle) Identifier() string         { return "0" }
