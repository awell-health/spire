package executor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/formula"
)

// TestWizardRunSpawn_LogAndClaudeDirShareStem verifies the single-source-of-truth
// naming contract: the orchestrator .log file and the spawned wizard's claude
// subdir share the same stem "<agentName>-<stepName>-<attemptNum>".
//
// Before spi-tayh7, cfg.Name was the bare stem (no -N) while LogPath carried
// the versioned stem, so the inspector could not pair a sibling wizard's log
// with its claude transcripts.
func TestWizardRunSpawn_LogAndClaudeDirShareStem(t *testing.T) {
	doltDir := t.TempDir()
	t.Setenv("SPIRE_DOLT_DIR", doltDir)

	agentName := "wizard-spi-test"
	stepName := "implement"
	attemptNum := 2
	expectedStem := agentName + "-" + stepName + "-" + strconv.Itoa(attemptNum)

	var captured agent.SpawnConfig
	backend := &mockBackend{
		spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
			captured = cfg
			return &mockHandle{}, nil
		},
	}

	dir := t.TempDir()
	deps := &Deps{
		Spawner:   backend,
		ConfigDir: func() (string, error) { return dir, nil },
		AgentResultDir: func(name string) string {
			return filepath.Join(doltDir, "wizards", name)
		},
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-naming",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			stepName: {Action: "wizard.run", Flow: "implement"},
		},
	}

	exec := NewGraphForTest("spi-test", agentName, graph, nil, deps)
	// Simulate a prior completed attempt so the next attempt is attemptNum.
	exec.graphState.Steps[stepName] = StepState{CompletedCount: attemptNum - 1}

	step := StepConfig{Action: "wizard.run", Flow: "implement"}
	result := wizardRunSpawn(exec, stepName, step, exec.graphState, agent.RoleApprentice, []string{"--apprentice"}, nil)
	if result.Error != nil {
		t.Fatalf("wizardRunSpawn: %v", result.Error)
	}

	// cfg.Name is what the spawned wizard receives via --name and what it uses
	// as wizardName when building its claude subdir.
	if captured.Name != expectedStem {
		t.Errorf("cfg.Name = %q, want %q", captured.Name, expectedStem)
	}

	// LogPath must be <doltDir>/wizards/<stem>.log.
	expectedLog := filepath.Join(doltDir, "wizards", expectedStem+".log")
	if captured.LogPath != expectedLog {
		t.Errorf("cfg.LogPath = %q, want %q", captured.LogPath, expectedLog)
	}

	// The claude subdir the spawned wizard would write to (via
	// WizardAgentResultDir) must share the same stem, not be the bare
	// "<agentName>-<stepName>" directory.
	claudeDir := filepath.Join(doltDir, "wizards", expectedStem, "claude")
	legacyDir := filepath.Join(doltDir, "wizards", agentName+"-"+stepName, "claude")
	if claudeDir == legacyDir {
		t.Fatalf("test bug: versioned and legacy claude dirs computed equal (%q)", claudeDir)
	}
	// Derived stem must match LogPath stem — the inspector pairs them by this.
	logStem := strings.TrimSuffix(filepath.Base(captured.LogPath), ".log")
	if logStem != expectedStem {
		t.Errorf("log stem = %q, want %q", logStem, expectedStem)
	}
}

// TestWizardRunSpawn_PerAttemptIsolation verifies that running the same step
// twice produces two distinct sibling trees on disk, so retry history is
// preserved without overwriting prior attempts.
func TestWizardRunSpawn_PerAttemptIsolation(t *testing.T) {
	doltDir := t.TempDir()
	t.Setenv("SPIRE_DOLT_DIR", doltDir)

	agentName := "wizard-spi-retry"
	stepName := "sage-review"

	var seenNames []string
	var seenLogPaths []string
	backend := &mockBackend{
		spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
			seenNames = append(seenNames, cfg.Name)
			seenLogPaths = append(seenLogPaths, cfg.LogPath)
			// Simulate the spawned wizard writing its orchestrator log and
			// a claude transcript under the per-attempt tree.
			if cfg.LogPath != "" {
				os.MkdirAll(filepath.Dir(cfg.LogPath), 0o755)
				os.WriteFile(cfg.LogPath, []byte("attempt log for "+cfg.Name+"\n"), 0o644)
			}
			claudeDir := filepath.Join(doltDir, "wizards", cfg.Name, "claude")
			os.MkdirAll(claudeDir, 0o755)
			os.WriteFile(filepath.Join(claudeDir, stepName+"-transcript.jsonl"),
				[]byte(`{"type":"system","subtype":"init","session_id":"`+cfg.Name+`"}`+"\n"), 0o644)
			return &mockHandle{}, nil
		},
	}

	dir := t.TempDir()
	deps := &Deps{
		Spawner:   backend,
		ConfigDir: func() (string, error) { return dir, nil },
		AgentResultDir: func(name string) string {
			return filepath.Join(doltDir, "wizards", name)
		},
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-retry",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			stepName: {Action: "wizard.run", Flow: "sage-review"},
		},
	}

	exec := NewGraphForTest("spi-retry", agentName, graph, nil, deps)

	// Attempt 1.
	step := StepConfig{Action: "wizard.run", Flow: "sage-review"}
	if r := wizardRunSpawn(exec, stepName, step, exec.graphState, agent.RoleSage, nil, nil); r.Error != nil {
		t.Fatalf("attempt 1: %v", r.Error)
	}

	// Advance CompletedCount to simulate a finished round before the next attempt.
	ss := exec.graphState.Steps[stepName]
	ss.CompletedCount = 1
	exec.graphState.Steps[stepName] = ss

	// Attempt 2.
	if r := wizardRunSpawn(exec, stepName, step, exec.graphState, agent.RoleSage, nil, nil); r.Error != nil {
		t.Fatalf("attempt 2: %v", r.Error)
	}

	// Two distinct per-attempt names.
	wantA := agentName + "-" + stepName + "-1"
	wantB := agentName + "-" + stepName + "-2"
	if len(seenNames) != 2 || seenNames[0] != wantA || seenNames[1] != wantB {
		t.Fatalf("spawn names = %v, want [%q %q]", seenNames, wantA, wantB)
	}

	// Two distinct log files exist on disk.
	for _, stem := range []string{wantA, wantB} {
		logPath := filepath.Join(doltDir, "wizards", stem+".log")
		if _, err := os.Stat(logPath); err != nil {
			t.Errorf("missing orchestrator log %s: %v", logPath, err)
		}
		claudePath := filepath.Join(doltDir, "wizards", stem, "claude", stepName+"-transcript.jsonl")
		if _, err := os.Stat(claudePath); err != nil {
			t.Errorf("missing claude transcript %s: %v", claudePath, err)
		}
	}

	// No cross-contamination: each attempt's transcript mentions its own
	// session_id, not the other's.
	for _, stem := range []string{wantA, wantB} {
		claudePath := filepath.Join(doltDir, "wizards", stem, "claude", stepName+"-transcript.jsonl")
		data, err := os.ReadFile(claudePath)
		if err != nil {
			continue
		}
		if !strings.Contains(string(data), `"session_id":"`+stem+`"`) {
			t.Errorf("transcript %s did not contain expected session_id; content=%q", claudePath, string(data))
		}
	}
}

// TestWizardRunSpawn_RecordAgentRunUsesVersionedName verifies that the agent
// run record and result.json read-back both key off the versioned name, so a
// result.json produced by the spawned wizard at <versioned>/result.json is
// found by the executor.
func TestWizardRunSpawn_RecordAgentRunUsesVersionedName(t *testing.T) {
	doltDir := t.TempDir()
	t.Setenv("SPIRE_DOLT_DIR", doltDir)

	agentName := "wizard-spi-record"
	stepName := "implement"
	expectedStem := agentName + "-" + stepName + "-1"

	// Pre-write a result.json at the versioned path (what the spawned wizard
	// would write). If the executor reads the bare stem instead, this is
	// invisible and the test fails.
	resultDir := filepath.Join(doltDir, "wizards", expectedStem)
	if err := os.MkdirAll(resultDir, 0o755); err != nil {
		t.Fatalf("mkdir result dir: %v", err)
	}
	resultBody, _ := json.Marshal(map[string]any{
		"result": "success",
		"branch": "feat/spi-record",
		"commit": "deadbeef",
	})
	if err := os.WriteFile(filepath.Join(resultDir, "result.json"), resultBody, 0o644); err != nil {
		t.Fatalf("write result.json: %v", err)
	}

	var recorded []AgentRun
	backend := &mockBackend{
		spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
			return &mockHandle{}, nil
		},
	}
	dir := t.TempDir()
	deps := &Deps{
		Spawner:   backend,
		ConfigDir: func() (string, error) { return dir, nil },
		AgentResultDir: func(name string) string {
			return filepath.Join(doltDir, "wizards", name)
		},
		RecordAgentRun: func(run AgentRun) (string, error) {
			recorded = append(recorded, run)
			return "", nil
		},
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-record",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			stepName: {Action: "wizard.run", Flow: "implement"},
		},
	}
	exec := NewGraphForTest("spi-record", agentName, graph, nil, deps)

	step := StepConfig{Action: "wizard.run", Flow: "implement"}
	result := wizardRunSpawn(exec, stepName, step, exec.graphState, agent.RoleApprentice, nil, nil)
	if result.Error != nil {
		t.Fatalf("wizardRunSpawn: %v", result.Error)
	}

	// result.json was found → its "success" result and branch/commit propagated.
	if got := result.Outputs["result"]; got != "success" {
		t.Errorf("outputs[result] = %q, want %q (executor did not find result.json at versioned path)", got, "success")
	}
	if got := result.Outputs["branch"]; got != "feat/spi-record" {
		t.Errorf("outputs[branch] = %q, want %q", got, "feat/spi-record")
	}

	// recordAgentRun was called with the versioned name.
	if len(recorded) != 1 {
		t.Fatalf("expected 1 recorded run, got %d", len(recorded))
	}
	if recorded[0].AgentName != expectedStem {
		t.Errorf("recorded agent name = %q, want %q", recorded[0].AgentName, expectedStem)
	}
}

