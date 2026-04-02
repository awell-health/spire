package executor

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// TestExecuteWave_StartRefPropagation verifies that wave 0 spawns children with
// empty StartRef (base branch) and wave 1+ spawns children with StartRef set to
// the staging HEAD SHA after merging prior-wave branches.
func TestExecuteWave_StartRefPropagation(t *testing.T) {
	repoDir := initSeamTestRepo(t)
	configDir := t.TempDir()

	// Create two feature branches that simulate wave 0 and wave 1 child work.
	runGitIn(t, repoDir, "checkout", "-b", "feat/sub-a")
	os.WriteFile(filepath.Join(repoDir, "a.txt"), []byte("wave0"), 0644)
	runGitIn(t, repoDir, "add", "-A")
	runGitIn(t, repoDir, "commit", "-m", "wave 0 work")
	runGitIn(t, repoDir, "checkout", "main")

	runGitIn(t, repoDir, "checkout", "-b", "feat/sub-b")
	os.WriteFile(filepath.Join(repoDir, "b.txt"), []byte("wave1"), 0644)
	runGitIn(t, repoDir, "add", "-A")
	runGitIn(t, repoDir, "commit", "-m", "wave 1 work")
	runGitIn(t, repoDir, "checkout", "main")

	// Track spawn configs across waves.
	var mu sync.Mutex
	var spawnedConfigs []agent.SpawnConfig

	deps := &Deps{
		ConfigDir: func() (string, error) { return configDir, nil },
		Spawner: &mockBackend{
			spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
				mu.Lock()
				spawnedConfigs = append(spawnedConfigs, cfg)
				mu.Unlock()
				return &mockHandle{}, nil
			},
		},
		GetChildren: func(parentID string) ([]Bead, error) {
			return []Bead{
				{ID: "sub-a", Status: "open"},
				{ID: "sub-b", Status: "open"},
			}, nil
		},
		GetBlockedIssues: func(filter beads.WorkFilter) ([]BoardBead, error) {
			// sub-b depends on sub-a → wave 0 = [sub-a], wave 1 = [sub-b]
			return []BoardBead{
				{
					ID: "sub-b",
					Dependencies: []store.BoardDep{{DependsOnID: "sub-a"}},
				},
			}, nil
		},
		UpdateBead:     func(id string, u map[string]interface{}) error { return nil },
		CloseBead:      func(id string) error { return nil },
		RecordAgentRun: func(run AgentRun) error { return nil },
		IsAttemptBead:     func(b Bead) bool { return false },
		IsStepBead:        func(b Bead) bool { return false },
		IsReviewRoundBead: func(b Bead) bool { return false },
		AddLabel:          func(id, l string) error { return nil },
		ActiveTowerConfig: func() (*TowerConfig, error) { return nil, nil },
		ArchmageGitEnv:    func(tower *TowerConfig) []string { return os.Environ() },
	}

	state := &State{
		BeadID:        "spi-epic",
		AgentName:     "wizard-test",
		Subtasks:      make(map[string]SubtaskState),
		StagingBranch: "staging/spi-epic",
		BaseBranch:    "main",
		RepoPath:      repoDir,
	}

	f := &FormulaV2{
		Name:    "test",
		Version: 2,
		Phases:  map[string]formula.PhaseConfig{"implement": {Dispatch: "wave"}},
	}

	e := NewForTest("spi-epic", "wizard-test", f, state, deps)
	e.deps.ResolveBranch = func(beadID string) string { return "feat/" + beadID }

	pc := formula.PhaseConfig{Dispatch: "wave"}
	err := e.executeWave("implement", pc)
	if err != nil {
		t.Fatalf("executeWave: %v", err)
	}

	// Expect 2 spawn calls: one for wave 0 (sub-a), one for wave 1 (sub-b).
	if len(spawnedConfigs) != 2 {
		t.Fatalf("expected 2 spawn calls, got %d", len(spawnedConfigs))
	}

	// Wave 0 child should have empty StartRef (starts from base branch).
	if spawnedConfigs[0].StartRef != "" {
		t.Errorf("wave 0 StartRef = %q, want empty", spawnedConfigs[0].StartRef)
	}

	// Wave 1 child should have non-empty StartRef (staging HEAD after wave 0 merge).
	if spawnedConfigs[1].StartRef == "" {
		t.Error("wave 1 StartRef is empty, want staging HEAD SHA")
	}
}

// TestExecuteSequential_StartRefPropagation verifies that sequential step 0
// spawns with empty StartRef and step 1+ spawns with StartRef set to the base
// branch HEAD after the prior step's merge-to-main.
func TestExecuteSequential_StartRefPropagation(t *testing.T) {
	// initSeqTestRepo creates a repo with a bare remote so Push works.
	repoDir := initSeqTestRepo(t)
	configDir := t.TempDir()

	// Create two feature branches that simulate sequential steps.
	runGitIn(t, repoDir, "checkout", "-b", "feat/sub-a")
	os.WriteFile(filepath.Join(repoDir, "a.txt"), []byte("step0"), 0644)
	runGitIn(t, repoDir, "add", "-A")
	runGitIn(t, repoDir, "commit", "-m", "step 0 work")
	runGitIn(t, repoDir, "checkout", "main")

	runGitIn(t, repoDir, "checkout", "-b", "feat/sub-b")
	os.WriteFile(filepath.Join(repoDir, "b.txt"), []byte("step1"), 0644)
	runGitIn(t, repoDir, "add", "-A")
	runGitIn(t, repoDir, "commit", "-m", "step 1 work")
	runGitIn(t, repoDir, "checkout", "main")

	var spawnedConfigs []agent.SpawnConfig

	deps := &Deps{
		ConfigDir: func() (string, error) { return configDir, nil },
		Spawner: &mockBackend{
			spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
				spawnedConfigs = append(spawnedConfigs, cfg)
				return &mockHandle{}, nil
			},
		},
		GetChildren: func(parentID string) ([]Bead, error) {
			return []Bead{
				{ID: "sub-a", Status: "open"},
				{ID: "sub-b", Status: "open"},
			}, nil
		},
		GetBlockedIssues: func(filter beads.WorkFilter) ([]BoardBead, error) {
			// sub-b depends on sub-a → wave 0 = [sub-a], wave 1 = [sub-b]
			return []BoardBead{
				{
					ID: "sub-b",
					Dependencies: []store.BoardDep{{DependsOnID: "sub-a"}},
				},
			}, nil
		},
		UpdateBead:     func(id string, u map[string]interface{}) error { return nil },
		CloseBead:      func(id string) error { return nil },
		RecordAgentRun: func(run AgentRun) error { return nil },
		AddLabel:       func(id, l string) error { return nil },
		RemoveLabel:    func(id, l string) error { return nil },
		ActiveTowerConfig: func() (*TowerConfig, error) { return nil, nil },
		ArchmageGitEnv:    func(tower *TowerConfig) []string { return os.Environ() },
		IsAttemptBead:     func(b Bead) bool { return false },
		IsStepBead:        func(b Bead) bool { return false },
		IsReviewRoundBead: func(b Bead) bool { return false },
	}

	state := &State{
		BeadID:        "spi-epic",
		AgentName:     "wizard-test",
		Subtasks:      make(map[string]SubtaskState),
		StagingBranch: "staging/spi-epic",
		BaseBranch:    "main",
		RepoPath:      repoDir,
	}

	f := &FormulaV2{
		Name:    "test",
		Version: 2,
		Phases: map[string]formula.PhaseConfig{
			"implement": {Dispatch: "sequential"},
		},
	}

	e := NewForTest("spi-epic", "wizard-test", f, state, deps)
	e.deps.ResolveBranch = func(beadID string) string { return "feat/" + beadID }

	pc := formula.PhaseConfig{Dispatch: "sequential"}
	err := e.executeSequential("implement", pc)
	if err != nil {
		t.Fatalf("executeSequential: %v", err)
	}

	// Expect 2 spawn calls: one for step 0 (sub-a), one for step 1 (sub-b).
	if len(spawnedConfigs) != 2 {
		t.Fatalf("expected 2 spawn calls, got %d", len(spawnedConfigs))
	}

	// Step 0 should have empty StartRef (starts from base branch).
	if spawnedConfigs[0].StartRef != "" {
		t.Errorf("step 0 StartRef = %q, want empty", spawnedConfigs[0].StartRef)
	}

	// Step 1 should have non-empty StartRef (base branch HEAD after step 0 merge).
	if spawnedConfigs[1].StartRef == "" {
		t.Error("step 1 StartRef is empty, want base branch HEAD SHA")
	}
}

// TestExecuteDirect_NoStartRef verifies that the direct single-bead path does
// NOT set StartRef — it should remain unchanged by this feature.
func TestExecuteDirect_NoStartRef(t *testing.T) {
	configDir := t.TempDir()

	var spawnedConfig agent.SpawnConfig
	deps := &Deps{
		ConfigDir: func() (string, error) { return configDir, nil },
		Spawner: &mockBackend{
			spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
				spawnedConfig = cfg
				return &mockHandle{}, nil
			},
		},
		RecordAgentRun: func(run AgentRun) error { return nil },
	}

	state := &State{
		BeadID:    "spi-test",
		AgentName: "wizard-test",
		Subtasks:  make(map[string]SubtaskState),
	}

	e := NewForTest("spi-test", "wizard-test", &FormulaV2{
		Name:    "test",
		Version: 2,
		Phases:  map[string]formula.PhaseConfig{"implement": {Dispatch: "direct"}},
	}, state, deps)

	pc := formula.PhaseConfig{Dispatch: "direct"}
	err := e.executeDirect("implement", pc)
	if err != nil {
		t.Fatalf("executeDirect: %v", err)
	}

	if spawnedConfig.StartRef != "" {
		t.Errorf("direct mode StartRef = %q, want empty", spawnedConfig.StartRef)
	}
}
