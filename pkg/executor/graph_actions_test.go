package executor

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/formula"
	"github.com/steveyegge/beads"
)

// planTestDeps returns mock deps suitable for plan action tests.
// The returned capturedArgs slice captures the args passed to ClaudeRunner.
func planTestDeps(t *testing.T) (*Deps, *[][]string) {
	t.Helper()
	dir := t.TempDir()
	capturedArgs := &[][]string{}

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Status: "in_progress", Title: "test bead", Type: "task"}, nil
		},
		GetChildren: func(parentID string) ([]Bead, error) {
			return nil, nil
		},
		GetComments: func(id string) ([]*beads.Comment, error) {
			return nil, nil
		},
		AddComment: func(id, text string) error {
			return nil
		},
		GetDepsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			return nil, nil
		},
		ClaudeRunner: func(args []string, dir string) ([]byte, error) {
			argsCopy := make([]string, len(args))
			copy(argsCopy, args)
			*capturedArgs = append(*capturedArgs, argsCopy)
			return []byte("Implementation plan:\n\nDo the thing."), nil
		},
		IsAttemptBead:    func(b Bead) bool { return false },
		IsStepBead:       func(b Bead) bool { return false },
		IsReviewRoundBead: func(b Bead) bool { return false },
	}

	return deps, capturedArgs
}

// extractMaxTurns finds the --max-turns value from a ClaudeRunner args slice.
func extractMaxTurns(args []string) string {
	for i, a := range args {
		if a == "--max-turns" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// TestActionPlanTask_FormulaMaxTurns verifies that actionPlanTask passes the
// formula-declared MaxTurns through to ClaudeRunner, not a hardcoded value.
func TestActionPlanTask_FormulaMaxTurns(t *testing.T) {
	deps, capturedArgs := planTestDeps(t)

	graph := &formula.FormulaStepGraph{
		Name:    "test-plan-task",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"plan": {Action: "wizard.run", Flow: "task-plan", MaxTurns: 7},
		},
	}

	exec := NewGraphForTest("spi-test", "wizard-test", graph, nil, deps)

	step := StepConfig{
		Action:   "wizard.run",
		Flow:     "task-plan",
		Model:    "claude-sonnet-4-6",
		MaxTurns: 7,
	}

	result := actionPlanTask(exec, "plan", step, exec.graphState)
	if result.Error != nil {
		t.Fatalf("actionPlanTask returned error: %v", result.Error)
	}

	if len(*capturedArgs) != 1 {
		t.Fatalf("expected 1 ClaudeRunner call, got %d", len(*capturedArgs))
	}

	maxTurns := extractMaxTurns((*capturedArgs)[0])
	if maxTurns != "7" {
		t.Errorf("expected --max-turns 7 (formula-declared), got %q", maxTurns)
	}
}

// TestActionPlanTask_ZeroMaxTurns verifies that when the formula does not
// declare max_turns (Go zero value), the --max-turns flag is omitted entirely —
// the executor does NOT invent a hardcoded budget.
func TestActionPlanTask_ZeroMaxTurns(t *testing.T) {
	deps, capturedArgs := planTestDeps(t)

	graph := &formula.FormulaStepGraph{
		Name:    "test-plan-task-zero",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"plan": {Action: "wizard.run", Flow: "task-plan"},
		},
	}

	exec := NewGraphForTest("spi-test", "wizard-test", graph, nil, deps)

	step := StepConfig{
		Action: "wizard.run",
		Flow:   "task-plan",
		Model:  "claude-sonnet-4-6",
		// MaxTurns not set — Go zero value (0)
	}

	result := actionPlanTask(exec, "plan", step, exec.graphState)
	if result.Error != nil {
		t.Fatalf("actionPlanTask returned error: %v", result.Error)
	}

	if len(*capturedArgs) != 1 {
		t.Fatalf("expected 1 ClaudeRunner call, got %d", len(*capturedArgs))
	}

	maxTurns := extractMaxTurns((*capturedArgs)[0])
	if maxTurns != "" {
		t.Errorf("expected --max-turns flag absent (unset), but got --max-turns %q", maxTurns)
	}
}

// TestActionPlanEpic_FormulaMaxTurns verifies that actionPlanEpic passes the
// formula-declared MaxTurns through to ClaudeRunner.
func TestActionPlanEpic_FormulaMaxTurns(t *testing.T) {
	deps, capturedArgs := planTestDeps(t)
	// ClaudeRunner returns JSON lines for epic planning.
	deps.ClaudeRunner = func(args []string, dir string) ([]byte, error) {
		argsCopy := make([]string, len(args))
		copy(argsCopy, args)
		*capturedArgs = append(*capturedArgs, argsCopy)
		return []byte(`{"title": "Task 1", "description": "Do task 1", "deps": [], "shared_files": [], "do_not_touch": []}`), nil
	}
	deps.CreateBead = func(opts CreateOpts) (string, error) {
		return "spi-test.1", nil
	}
	deps.ParseIssueType = func(s string) beads.IssueType { return beads.IssueType(s) }
	deps.AddDep = func(id, depID string) error { return nil }

	graph := &formula.FormulaStepGraph{
		Name:    "test-plan-epic",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"plan": {Action: "wizard.run", Flow: "epic-plan", MaxTurns: 10},
		},
	}

	exec := NewGraphForTest("spi-test", "wizard-test", graph, nil, deps)

	step := StepConfig{
		Action:   "wizard.run",
		Flow:     "epic-plan",
		Model:    "claude-opus-4-6",
		MaxTurns: 10,
	}

	result := actionPlanEpic(exec, "plan", step, exec.graphState)
	if result.Error != nil {
		t.Fatalf("actionPlanEpic returned error: %v", result.Error)
	}

	if len(*capturedArgs) < 1 {
		t.Fatal("expected at least 1 ClaudeRunner call")
	}

	maxTurns := extractMaxTurns((*capturedArgs)[0])
	if maxTurns != "10" {
		t.Errorf("expected --max-turns 10 (formula-declared), got %q", maxTurns)
	}
}

// TestActionPlanEpic_ZeroMaxTurns verifies that when the formula does not
// declare max_turns, the --max-turns flag is omitted entirely — the executor
// does NOT invent a hardcoded budget like 5.
func TestActionPlanEpic_ZeroMaxTurns(t *testing.T) {
	deps, capturedArgs := planTestDeps(t)
	deps.ClaudeRunner = func(args []string, dir string) ([]byte, error) {
		argsCopy := make([]string, len(args))
		copy(argsCopy, args)
		*capturedArgs = append(*capturedArgs, argsCopy)
		return []byte(`{"title": "Task 1", "description": "Do task 1", "deps": [], "shared_files": [], "do_not_touch": []}`), nil
	}
	deps.CreateBead = func(opts CreateOpts) (string, error) {
		return "spi-test.1", nil
	}
	deps.ParseIssueType = func(s string) beads.IssueType { return beads.IssueType(s) }
	deps.AddDep = func(id, depID string) error { return nil }

	graph := &formula.FormulaStepGraph{
		Name:    "test-plan-epic-zero",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"plan": {Action: "wizard.run", Flow: "epic-plan"},
		},
	}

	exec := NewGraphForTest("spi-test", "wizard-test", graph, nil, deps)

	step := StepConfig{
		Action: "wizard.run",
		Flow:   "epic-plan",
		Model:  "claude-opus-4-6",
		// MaxTurns not set — Go zero value (0)
	}

	result := actionPlanEpic(exec, "plan", step, exec.graphState)
	if result.Error != nil {
		t.Fatalf("actionPlanEpic returned error: %v", result.Error)
	}

	if len(*capturedArgs) < 1 {
		t.Fatal("expected at least 1 ClaudeRunner call")
	}

	maxTurns := extractMaxTurns((*capturedArgs)[0])
	if maxTurns != "" {
		t.Errorf("expected --max-turns flag absent (unset), but got --max-turns %q", maxTurns)
	}
}

// TestStepConfig_MaxTurns_ParseRoundTrip verifies that StepConfig.MaxTurns
// is parsed correctly from TOML and that the zero value means "not set".
func TestStepConfig_MaxTurns_ParseRoundTrip(t *testing.T) {
	tomlWithMaxTurns := `
name = "test-formula"
version = 3

[steps.plan]
action = "wizard.run"
flow = "task-plan"
max_turns = 5

[steps.finish]
action = "bead.finish"
needs = ["plan"]
terminal = true
`
	tomlWithoutMaxTurns := `
name = "test-formula"
version = 3

[steps.plan]
action = "wizard.run"
flow = "task-plan"

[steps.finish]
action = "bead.finish"
needs = ["plan"]
terminal = true
`

	t.Run("with max_turns declared", func(t *testing.T) {
		f, err := formula.ParseFormulaStepGraph([]byte(tomlWithMaxTurns))
		if err != nil {
			t.Fatalf("parse error: %v", err)
		}
		step := f.Steps["plan"]
		if step.MaxTurns != 5 {
			t.Errorf("expected MaxTurns=5, got %d", step.MaxTurns)
		}
	})

	t.Run("without max_turns (zero value)", func(t *testing.T) {
		f, err := formula.ParseFormulaStepGraph([]byte(tomlWithoutMaxTurns))
		if err != nil {
			t.Fatalf("parse error: %v", err)
		}
		step := f.Steps["plan"]
		if step.MaxTurns != 0 {
			t.Errorf("expected MaxTurns=0 (not set), got %d", step.MaxTurns)
		}
	})
}

// --- Tests for implement flow --apprentice and wizardRunSpawn error propagation ---

// TestImplementFlowIncludesApprenticeFlag verifies that the "implement" flow
// passes --apprentice to the spawned wizard subprocess, preventing the child
// wizard from re-claiming the bead.
func TestImplementFlowIncludesApprenticeFlag(t *testing.T) {
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
		// No AgentResultDir — readAgentResult returns nil, which is fine.
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-implement",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"implement": {Action: "wizard.run", Flow: "implement"},
		},
	}

	exec := NewGraphForTest("spi-test", "wizard-test", graph, nil, deps)

	step := StepConfig{
		Action: "wizard.run",
		Flow:   "implement",
	}

	result := actionWizardRun(exec, "implement", step, exec.graphState)
	if result.Error != nil {
		t.Fatalf("actionWizardRun returned error: %v", result.Error)
	}

	// Check that --apprentice is present in ExtraArgs.
	found := false
	for _, arg := range captured.ExtraArgs {
		if arg == "--apprentice" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected --apprentice in ExtraArgs, got %v", captured.ExtraArgs)
	}

	// Also verify the role is apprentice.
	if captured.Role != agent.RoleApprentice {
		t.Errorf("expected role %q, got %q", agent.RoleApprentice, captured.Role)
	}
}

// TestWizardRunSpawnFailsOnChildExit verifies that wizardRunSpawn returns an
// ActionResult with a non-nil Error when the child process exits non-zero and
// there is no result.json.
func TestWizardRunSpawnFailsOnChildExit(t *testing.T) {
	backend := &mockBackend{
		spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
			return &mockHandle{waitErr: errors.New("exit status 1")}, nil
		},
	}

	dir := t.TempDir()
	deps := &Deps{
		Spawner:   backend,
		ConfigDir: func() (string, error) { return dir, nil },
		// No AgentResultDir — no result.json present.
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-fail",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"implement": {Action: "wizard.run", Flow: "implement"},
		},
	}

	exec := NewGraphForTest("spi-test", "wizard-test", graph, nil, deps)

	step := StepConfig{
		Action: "wizard.run",
		Flow:   "implement",
	}
	state := exec.graphState

	result := wizardRunSpawn(exec, "implement", step, state, agent.RoleApprentice, []string{"--apprentice"})

	if result.Error == nil {
		t.Fatal("expected non-nil Error when child process exits non-zero, got nil")
	}
	if result.Outputs["result"] != "error" {
		t.Errorf("expected outputs[result]=%q, got %q", "error", result.Outputs["result"])
	}
}

// TestWizardRunSpawnSucceedsWithResultJSONDespiteWaitErr verifies the edge
// case where the child process wrote result.json with result="success" but then
// got killed (waitErr != nil). The node should NOT fail because the work was done.
func TestWizardRunSpawnSucceedsWithResultJSONDespiteWaitErr(t *testing.T) {
	agentName := "wizard-test"
	stepName := "implement"
	spawnName := agentName + "-" + stepName

	// Set up a temp dir with result.json reporting success.
	resultDir := t.TempDir()
	ar := agentResultJSON{Result: "success", Branch: "feat/spi-test", Commit: "abc123"}
	data, _ := json.Marshal(ar)
	os.WriteFile(filepath.Join(resultDir, "result.json"), data, 0644)

	backend := &mockBackend{
		spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
			return &mockHandle{waitErr: errors.New("signal: killed")}, nil
		},
	}

	dir := t.TempDir()
	deps := &Deps{
		Spawner:   backend,
		ConfigDir: func() (string, error) { return dir, nil },
		AgentResultDir: func(name string) string {
			if name == spawnName {
				return resultDir
			}
			return ""
		},
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-success-despite-kill",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			stepName: {Action: "wizard.run", Flow: "implement"},
		},
	}

	exec := NewGraphForTest("spi-test", agentName, graph, nil, deps)

	step := StepConfig{
		Action: "wizard.run",
		Flow:   "implement",
	}
	state := exec.graphState

	result := wizardRunSpawn(exec, stepName, step, state, agent.RoleApprentice, []string{"--apprentice"})

	// The node should succeed because result.json says "success".
	if result.Error != nil {
		t.Fatalf("expected nil Error when result.json reports success, got: %v", result.Error)
	}
	if result.Outputs["result"] != "success" {
		t.Errorf("expected outputs[result]=%q, got %q", "success", result.Outputs["result"])
	}
	if result.Outputs["branch"] != "feat/spi-test" {
		t.Errorf("expected outputs[branch]=%q, got %q", "feat/spi-test", result.Outputs["branch"])
	}
}

