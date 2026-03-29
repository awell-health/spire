package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/formula"
)

// TestAttemptBuildFix_SucceedsFirstRound verifies that when the build-fix
// apprentice fixes the issue on the first attempt, attemptBuildFix returns nil.
func TestAttemptBuildFix_SucceedsFirstRound(t *testing.T) {
	wtDir := t.TempDir()
	configDir := t.TempDir()

	spawnCalls := 0
	commentTexts := []string{}

	deps := &Deps{
		ConfigDir: func() (string, error) { return configDir, nil },
		Spawner: &mockBackend{
			spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
				spawnCalls++
				return &mockHandle{}, nil
			},
		},
		AddComment: func(id, text string) error {
			commentTexts = append(commentTexts, text)
			return nil
		},
		RecordAgentRun: func(run AgentRun) error { return nil },
	}

	state := &State{
		BeadID:      "spi-test",
		AgentName:   "wizard-test",
		Subtasks:    make(map[string]SubtaskState),
		WorktreeDir: wtDir,
		RepoPath:    wtDir,
	}

	// Use a build command that always succeeds (after fix).
	pc := formula.PhaseConfig{
		Build: "true", // shell built-in that always exits 0
	}

	e := NewForTest("spi-test", "wizard-test", &formula.FormulaV2{
		Name:    "test",
		Version: 2,
		Phases:  map[string]formula.PhaseConfig{"implement": pc},
	}, state, deps)

	buildErr := fmt.Errorf("go build: duplicate method dagNextWave")
	err := e.attemptBuildFix(0, buildErr, pc)

	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if spawnCalls != 1 {
		t.Errorf("expected 1 spawn call, got %d", spawnCalls)
	}
	if state.BuildFixRounds != 1 {
		t.Errorf("expected BuildFixRounds=1, got %d", state.BuildFixRounds)
	}
	if len(commentTexts) != 1 {
		t.Errorf("expected 1 comment, got %d", len(commentTexts))
	}
	// Verify .build-error.log was cleaned up.
	if _, err := os.Stat(filepath.Join(wtDir, ".build-error.log")); !os.IsNotExist(err) {
		t.Error("expected .build-error.log to be cleaned up")
	}
}

// TestAttemptBuildFix_SucceedsSecondRound verifies that when the first fix
// attempt fails but the second succeeds, attemptBuildFix returns nil.
func TestAttemptBuildFix_SucceedsSecondRound(t *testing.T) {
	wtDir := t.TempDir()
	configDir := t.TempDir()

	spawnCalls := 0

	// Write a script that fails on first call, succeeds on second.
	scriptPath := filepath.Join(wtDir, "build.sh")
	counterPath := filepath.Join(wtDir, ".build-counter")
	os.WriteFile(scriptPath, []byte(fmt.Sprintf(`#!/bin/sh
if [ -f "%s" ]; then
    exit 0
else
    touch "%s"
    exit 1
fi
`, counterPath, counterPath)), 0755)

	deps := &Deps{
		ConfigDir: func() (string, error) { return configDir, nil },
		Spawner: &mockBackend{
			spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
				spawnCalls++
				return &mockHandle{}, nil
			},
		},
		AddComment:     func(id, text string) error { return nil },
		RecordAgentRun: func(run AgentRun) error { return nil },
	}

	state := &State{
		BeadID:      "spi-test2",
		AgentName:   "wizard-test",
		Subtasks:    make(map[string]SubtaskState),
		WorktreeDir: wtDir,
		RepoPath:    wtDir,
	}

	pc := formula.PhaseConfig{
		Build: scriptPath,
	}

	e := NewForTest("spi-test2", "wizard-test", &formula.FormulaV2{
		Name:    "test",
		Version: 2,
		Phases:  map[string]formula.PhaseConfig{"implement": pc},
	}, state, deps)

	buildErr := fmt.Errorf("initial build failure")
	err := e.attemptBuildFix(0, buildErr, pc)

	if err != nil {
		t.Fatalf("expected nil error on second round, got: %v", err)
	}
	if spawnCalls != 2 {
		t.Errorf("expected 2 spawn calls, got %d", spawnCalls)
	}
	if state.BuildFixRounds != 2 {
		t.Errorf("expected BuildFixRounds=2, got %d", state.BuildFixRounds)
	}
}

// TestAttemptBuildFix_AllRoundsExhausted verifies that when all fix attempts
// fail, attemptBuildFix returns an error.
func TestAttemptBuildFix_AllRoundsExhausted(t *testing.T) {
	wtDir := t.TempDir()
	configDir := t.TempDir()

	spawnCalls := 0

	// Write a script that always fails.
	scriptPath := filepath.Join(wtDir, "build.sh")
	os.WriteFile(scriptPath, []byte("#!/bin/sh\nexit 1\n"), 0755)

	deps := &Deps{
		ConfigDir: func() (string, error) { return configDir, nil },
		Spawner: &mockBackend{
			spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
				spawnCalls++
				return &mockHandle{}, nil
			},
		},
		AddComment:     func(id, text string) error { return nil },
		RecordAgentRun: func(run AgentRun) error { return nil },
	}

	state := &State{
		BeadID:      "spi-test3",
		AgentName:   "wizard-test",
		Subtasks:    make(map[string]SubtaskState),
		WorktreeDir: wtDir,
		RepoPath:    wtDir,
	}

	pc := formula.PhaseConfig{
		Build: scriptPath,
	}

	e := NewForTest("spi-test3", "wizard-test", &formula.FormulaV2{
		Name:    "test",
		Version: 2,
		Phases:  map[string]formula.PhaseConfig{"implement": pc},
	}, state, deps)

	buildErr := fmt.Errorf("persistent build failure")
	err := e.attemptBuildFix(0, buildErr, pc)

	if err == nil {
		t.Fatal("expected error when all rounds exhausted, got nil")
	}
	if spawnCalls != 2 { // default MaxBuildFixRounds is 2
		t.Errorf("expected 2 spawn calls (default max), got %d", spawnCalls)
	}
	if state.BuildFixRounds != 2 {
		t.Errorf("expected BuildFixRounds=2, got %d", state.BuildFixRounds)
	}
}

// TestAttemptBuildFix_SpawnFailure verifies that when the spawner fails,
// attemptBuildFix returns immediately with an error.
func TestAttemptBuildFix_SpawnFailure(t *testing.T) {
	wtDir := t.TempDir()
	configDir := t.TempDir()

	deps := &Deps{
		ConfigDir: func() (string, error) { return configDir, nil },
		Spawner: &mockBackend{
			spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
				return nil, fmt.Errorf("container start failed")
			},
		},
		AddComment:     func(id, text string) error { return nil },
		RecordAgentRun: func(run AgentRun) error { return nil },
	}

	state := &State{
		BeadID:      "spi-test4",
		AgentName:   "wizard-test",
		Subtasks:    make(map[string]SubtaskState),
		WorktreeDir: wtDir,
		RepoPath:    wtDir,
	}

	pc := formula.PhaseConfig{
		Build: "go build ./...",
	}

	e := NewForTest("spi-test4", "wizard-test", &formula.FormulaV2{
		Name:    "test",
		Version: 2,
		Phases:  map[string]formula.PhaseConfig{"implement": pc},
	}, state, deps)

	buildErr := fmt.Errorf("build failure")
	err := e.attemptBuildFix(0, buildErr, pc)

	if err == nil {
		t.Fatal("expected error on spawn failure, got nil")
	}
	if state.BuildFixRounds != 1 {
		t.Errorf("expected BuildFixRounds=1 (incremented before spawn), got %d", state.BuildFixRounds)
	}
}

// TestAttemptBuildFix_EmptyWorktreeDir verifies that when WorktreeDir is empty,
// attemptBuildFix returns immediately with an error.
func TestAttemptBuildFix_EmptyWorktreeDir(t *testing.T) {
	configDir := t.TempDir()

	spawnCalls := 0

	deps := &Deps{
		ConfigDir: func() (string, error) { return configDir, nil },
		Spawner: &mockBackend{
			spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
				spawnCalls++
				return &mockHandle{}, nil
			},
		},
		AddComment:     func(id, text string) error { return nil },
		RecordAgentRun: func(run AgentRun) error { return nil },
	}

	state := &State{
		BeadID:      "spi-test5",
		AgentName:   "wizard-test",
		Subtasks:    make(map[string]SubtaskState),
		WorktreeDir: "", // empty — no staging worktree
		RepoPath:    "/tmp/does-not-matter",
	}

	pc := formula.PhaseConfig{
		Build: "go build ./...",
	}

	e := NewForTest("spi-test5", "wizard-test", &formula.FormulaV2{
		Name:    "test",
		Version: 2,
		Phases:  map[string]formula.PhaseConfig{"implement": pc},
	}, state, deps)

	buildErr := fmt.Errorf("build failure")
	err := e.attemptBuildFix(0, buildErr, pc)

	if err == nil {
		t.Fatal("expected error for empty WorktreeDir, got nil")
	}
	if spawnCalls != 0 {
		t.Errorf("expected 0 spawn calls (should fail before spawn), got %d", spawnCalls)
	}
}
