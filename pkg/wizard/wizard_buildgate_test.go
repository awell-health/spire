package wizard

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/awell-health/spire/pkg/repoconfig"
)

// mockBuildRunner returns a BuildRunFunc that fails failCount times then succeeds.
// It records how many times it was called.
func mockBuildRunner(failCount int, errOutput string) (BuildRunFunc, *int) {
	calls := new(int)
	return func(dir, cmd string) (string, error) {
		*calls++
		if *calls <= failCount {
			return errOutput, fmt.Errorf("exit status 1")
		}
		return "", nil
	}, calls
}

// mockAgentRunner returns an AgentRunFunc that simulates Claude fixing the code
// by writing a file to the worktree. It records call count.
func mockAgentRunner() (AgentRunFunc, *int) {
	calls := new(int)
	return func(dir, promptPath, model, timeout string, maxTurns int, agentResultDir, label string) (ClaudeMetrics, error) {
		*calls++
		// Simulate Claude writing a fix file
		fixFile := filepath.Join(dir, fmt.Sprintf("fix-%d.go", *calls))
		os.WriteFile(fixFile, []byte(fmt.Sprintf("package main\n// fix round %d\n", *calls)), 0644)
		return ClaudeMetrics{InputTokens: 100, OutputTokens: 50, TotalTokens: 150, Turns: 1, CostUSD: 0.01}, nil
	}, calls
}

// mockAgentRunnerNoOp returns an AgentRunFunc that does nothing (no file changes).
func mockAgentRunnerNoOp() (AgentRunFunc, *int) {
	calls := new(int)
	return func(dir, promptPath, model, timeout string, maxTurns int, agentResultDir, label string) (ClaudeMetrics, error) {
		*calls++
		return ClaudeMetrics{}, nil
	}, calls
}

func TestWizardBuildGate_NoBuildCommand(t *testing.T) {
	wc := setupWorktree(t, "feat/gate-no-cmd")
	cfg := &repoconfig.RepoConfig{}

	passed := wizardBuildGateImpl(wc, "test-001", "no build cmd", wc.Dir, "", cfg, nil, "", noopLog, nil, nil)
	if !passed {
		t.Error("expected build gate to pass when no build command is configured")
	}
}

func TestWizardBuildGate_BuildPassesFirstTry(t *testing.T) {
	wc := setupWorktree(t, "feat/gate-pass")
	cfg := &repoconfig.RepoConfig{
		Runtime: repoconfig.RuntimeConfig{Build: "go build ./..."},
	}

	runBuild, buildCalls := mockBuildRunner(0, "") // always succeeds
	runAgent, agentCalls := mockAgentRunnerNoOp()

	var metrics ClaudeMetrics
	passed := wizardBuildGateImpl(wc, "test-002", "build passes", wc.Dir, "", cfg, &metrics, "", noopLog,
		runBuild, runAgent)

	if !passed {
		t.Error("expected build gate to pass when build succeeds")
	}
	if *buildCalls != 1 {
		t.Errorf("expected 1 build call, got %d", *buildCalls)
	}
	if *agentCalls != 0 {
		t.Errorf("expected 0 agent calls, got %d", *agentCalls)
	}
	if metrics.TotalTokens != 0 {
		t.Errorf("expected 0 tokens (no fix needed), got %d", metrics.TotalTokens)
	}
}

func TestWizardBuildGate_FixSucceedsRound1(t *testing.T) {
	wc := setupWorktree(t, "feat/gate-fix-r1")
	cfg := &repoconfig.RepoConfig{
		Runtime: repoconfig.RuntimeConfig{Build: "go build ./..."},
	}

	// Build fails once (initial), then succeeds after first fix round.
	// Call sequence: initial build (fail) -> fix round 1 build (pass)
	runBuild, buildCalls := mockBuildRunner(1, "main.go:5:1: undefined: foo")
	runAgent, agentCalls := mockAgentRunner()

	var metrics ClaudeMetrics
	passed := wizardBuildGateImpl(wc, "test-003", "fix round 1", wc.Dir, "", cfg, &metrics, "", noopLog,
		runBuild, runAgent)

	if !passed {
		t.Error("expected build gate to pass after fix round 1")
	}
	if *buildCalls != 2 {
		t.Errorf("expected 2 build calls (initial + re-check after fix), got %d", *buildCalls)
	}
	if *agentCalls != 1 {
		t.Errorf("expected 1 agent call, got %d", *agentCalls)
	}
	if metrics.TotalTokens != 150 {
		t.Errorf("expected 150 tokens from 1 fix round, got %d", metrics.TotalTokens)
	}
}

func TestWizardBuildGate_ExhaustedAfterMaxRounds(t *testing.T) {
	wc := setupWorktree(t, "feat/gate-exhausted")
	cfg := &repoconfig.RepoConfig{
		Runtime: repoconfig.RuntimeConfig{Build: "go build ./..."},
	}

	// Build always fails — exhaust all fix rounds.
	// Call sequence: initial (fail) -> fix 1 build (fail) -> fix 2 build (fail)
	runBuild, buildCalls := mockBuildRunner(999, "main.go:1:1: syntax error")
	runAgent, agentCalls := mockAgentRunner()

	var metrics ClaudeMetrics
	passed := wizardBuildGateImpl(wc, "test-004", "exhausted rounds", wc.Dir, "", cfg, &metrics, "", noopLog,
		runBuild, runAgent)

	if passed {
		t.Error("expected build gate to fail after exhausting fix rounds")
	}
	// 1 initial + 2 re-checks (one per fix round)
	expectedBuildCalls := 1 + DefaultMaxBuildFixRounds
	if *buildCalls != expectedBuildCalls {
		t.Errorf("expected %d build calls, got %d", expectedBuildCalls, *buildCalls)
	}
	if *agentCalls != DefaultMaxBuildFixRounds {
		t.Errorf("expected %d agent calls, got %d", DefaultMaxBuildFixRounds, *agentCalls)
	}
	// Metrics should accumulate from all fix rounds
	expectedTokens := DefaultMaxBuildFixRounds * 150
	if metrics.TotalTokens != expectedTokens {
		t.Errorf("expected %d tokens from %d fix rounds, got %d",
			expectedTokens, DefaultMaxBuildFixRounds, metrics.TotalTokens)
	}
}

func TestWizardBuildGate_FixSucceedsRound2(t *testing.T) {
	wc := setupWorktree(t, "feat/gate-fix-r2")
	cfg := &repoconfig.RepoConfig{
		Runtime: repoconfig.RuntimeConfig{Build: "go build ./..."},
	}

	// Build fails twice (initial + after round 1), succeeds after round 2.
	runBuild, buildCalls := mockBuildRunner(2, "main.go:10:1: undefined: bar")
	runAgent, agentCalls := mockAgentRunner()

	var metrics ClaudeMetrics
	passed := wizardBuildGateImpl(wc, "test-005", "fix round 2", wc.Dir, "", cfg, &metrics, "", noopLog,
		runBuild, runAgent)

	if !passed {
		t.Error("expected build gate to pass after fix round 2")
	}
	if *buildCalls != 3 {
		t.Errorf("expected 3 build calls (initial + 2 re-checks), got %d", *buildCalls)
	}
	if *agentCalls != 2 {
		t.Errorf("expected 2 agent calls, got %d", *agentCalls)
	}
}

func TestWizardBuildGate_MetricsAccumulation(t *testing.T) {
	wc := setupWorktree(t, "feat/gate-metrics")
	cfg := &repoconfig.RepoConfig{
		Runtime: repoconfig.RuntimeConfig{Build: "go build ./..."},
	}

	// Build always fails to force max rounds.
	runBuild, _ := mockBuildRunner(999, "error")
	runAgent, _ := mockAgentRunner()

	// Start with pre-existing metrics.
	metrics := ClaudeMetrics{InputTokens: 1000, OutputTokens: 500, TotalTokens: 1500, Turns: 5, CostUSD: 1.0}
	wizardBuildGateImpl(wc, "test-006", "metrics test", wc.Dir, "", cfg, &metrics, "", noopLog,
		runBuild, runAgent)

	// Each fix round adds: 100 input, 50 output, 150 total, 1 turn, 0.01 cost.
	expectedTotal := 1500 + DefaultMaxBuildFixRounds*150
	if metrics.TotalTokens != expectedTotal {
		t.Errorf("expected accumulated total tokens %d, got %d", expectedTotal, metrics.TotalTokens)
	}
	expectedCost := 1.0 + float64(DefaultMaxBuildFixRounds)*0.01
	if metrics.CostUSD != expectedCost {
		t.Errorf("expected accumulated cost %.2f, got %.2f", expectedCost, metrics.CostUSD)
	}
}

func TestWizardBuildGate_NilMetrics(t *testing.T) {
	wc := setupWorktree(t, "feat/gate-nilmetrics")
	cfg := &repoconfig.RepoConfig{
		Runtime: repoconfig.RuntimeConfig{Build: "go build ./..."},
	}

	// Build fails then succeeds — nil metrics should not panic.
	runBuild, _ := mockBuildRunner(1, "error")
	runAgent, _ := mockAgentRunner()

	passed := wizardBuildGateImpl(wc, "test-007", "nil metrics", wc.Dir, "", cfg, nil, "", noopLog,
		runBuild, runAgent)

	if !passed {
		t.Error("expected build gate to pass (nil metrics should not cause issues)")
	}
}

func TestWizardBuildGate_PromptFileCleanup(t *testing.T) {
	wc := setupWorktree(t, "feat/gate-cleanup")
	cfg := &repoconfig.RepoConfig{
		Runtime: repoconfig.RuntimeConfig{Build: "go build ./..."},
	}

	// Build fails then succeeds — prompt file should be cleaned up.
	runBuild, _ := mockBuildRunner(1, "error")
	runAgent, _ := mockAgentRunnerNoOp()

	wizardBuildGateImpl(wc, "test-008", "cleanup test", wc.Dir, "", cfg, nil, "", noopLog,
		runBuild, runAgent)

	promptPath := filepath.Join(wc.Dir, ".spire-build-fix-prompt.txt")
	if _, err := os.Stat(promptPath); !os.IsNotExist(err) {
		t.Error("expected build-fix prompt file to be cleaned up")
	}
}
