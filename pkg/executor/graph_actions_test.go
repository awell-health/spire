package executor

import (
	"encoding/json"
	"io"
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

// --- Sage review verdict promotion tests ---

// sageVerdictMockBackend implements agent.Backend for sage-review verdict tests.
type sageVerdictMockBackend struct {
	// onSpawn is called when Spawn is invoked; lets tests write result.json
	// before Wait returns.
	onSpawn func(cfg agent.SpawnConfig)
}

func (b *sageVerdictMockBackend) Spawn(cfg agent.SpawnConfig) (agent.Handle, error) {
	if b.onSpawn != nil {
		b.onSpawn(cfg)
	}
	return &sageVerdictMockHandle{}, nil
}
func (b *sageVerdictMockBackend) List() ([]agent.Info, error)       { return nil, nil }
func (b *sageVerdictMockBackend) Logs(name string) (io.ReadCloser, error) { return nil, os.ErrNotExist }
func (b *sageVerdictMockBackend) Kill(name string) error            { return nil }

type sageVerdictMockHandle struct{}

func (h *sageVerdictMockHandle) Wait() error           { return nil }
func (h *sageVerdictMockHandle) Signal(os.Signal) error { return nil }
func (h *sageVerdictMockHandle) Alive() bool            { return false }
func (h *sageVerdictMockHandle) Name() string           { return "mock-sage" }
func (h *sageVerdictMockHandle) Identifier() string     { return "mock-id" }

// TestSageReview_ApproveVerdictPromotion verifies the full round-trip:
// when a sage writes result.json with "approve" (the no-diff path), the
// executor reads it back via readAgentResult and promotes it to
// outputs["verdict"] = "approve" so the review graph can route to merge.
func TestSageReview_ApproveVerdictPromotion(t *testing.T) {
	dir := t.TempDir()

	// Pre-write the result.json that the sage would produce.
	// The agent name is "<executor-agent>-<step-name>".
	agentName := "wizard-test-sage-review"
	resultDir := filepath.Join(dir, agentName)
	os.MkdirAll(resultDir, 0755)

	resultData, _ := json.Marshal(map[string]interface{}{
		"result":  "approve",
		"branch":  "",
		"commit":  "",
		"wizard":  agentName,
		"bead_id": "spi-test",
	})
	os.WriteFile(filepath.Join(resultDir, "result.json"), resultData, 0644)

	graph := &formula.FormulaStepGraph{
		Name:    "test-review-verdict",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"sage-review": {Action: "wizard.run", Flow: "sage-review"},
		},
	}

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		Spawner: &sageVerdictMockBackend{},
		AgentResultDir: func(name string) string {
			return filepath.Join(dir, name)
		},
		RecordAgentRun: func(run AgentRun) error { return nil },
	}

	exec := NewGraphForTest("spi-test", "wizard-test", graph, nil, deps)
	state := exec.graphState

	step := StepConfig{
		Action: "wizard.run",
		Flow:   "sage-review",
	}

	result := actionWizardRun(exec, "sage-review", step, state)
	if result.Error != nil {
		t.Fatalf("actionWizardRun returned error: %v", result.Error)
	}

	// The key assertion: verdict must be promoted from result to outputs.
	if result.Outputs["verdict"] != "approve" {
		t.Errorf("outputs[verdict] = %q, want %q", result.Outputs["verdict"], "approve")
	}
	if result.Outputs["result"] != "approve" {
		t.Errorf("outputs[result] = %q, want %q", result.Outputs["result"], "approve")
	}
}

// TestSageReview_RequestChangesVerdictPromotion verifies that request_changes
// is also promoted to outputs["verdict"].
func TestSageReview_RequestChangesVerdictPromotion(t *testing.T) {
	dir := t.TempDir()

	agentName := "wizard-test-sage-review"
	resultDir := filepath.Join(dir, agentName)
	os.MkdirAll(resultDir, 0755)

	resultData, _ := json.Marshal(map[string]interface{}{
		"result":  "request_changes",
		"bead_id": "spi-test",
	})
	os.WriteFile(filepath.Join(resultDir, "result.json"), resultData, 0644)

	graph := &formula.FormulaStepGraph{
		Name:    "test-review-verdict-rc",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"sage-review": {Action: "wizard.run", Flow: "sage-review"},
		},
	}

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		Spawner: &sageVerdictMockBackend{},
		AgentResultDir: func(name string) string {
			return filepath.Join(dir, name)
		},
		RecordAgentRun: func(run AgentRun) error { return nil },
	}

	exec := NewGraphForTest("spi-test", "wizard-test", graph, nil, deps)
	state := exec.graphState

	step := StepConfig{
		Action: "wizard.run",
		Flow:   "sage-review",
	}

	result := actionWizardRun(exec, "sage-review", step, state)
	if result.Error != nil {
		t.Fatalf("actionWizardRun returned error: %v", result.Error)
	}

	if result.Outputs["verdict"] != "request_changes" {
		t.Errorf("outputs[verdict] = %q, want %q", result.Outputs["verdict"], "request_changes")
	}
}

// TestSageReview_NoResultJSON_NoVerdict verifies that when no result.json
// exists (process succeeded but wrote nothing), verdict is NOT set. This
// was the original bug: the review graph gets stuck because no verdict
// output exists.
func TestSageReview_NoResultJSON_NoVerdict(t *testing.T) {
	dir := t.TempDir()

	// Do NOT write result.json — simulate the old no-diff bug.

	graph := &formula.FormulaStepGraph{
		Name:    "test-review-verdict-missing",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"sage-review": {Action: "wizard.run", Flow: "sage-review"},
		},
	}

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		Spawner: &sageVerdictMockBackend{},
		AgentResultDir: func(name string) string {
			return filepath.Join(dir, name)
		},
		RecordAgentRun: func(run AgentRun) error { return nil },
	}

	exec := NewGraphForTest("spi-test", "wizard-test", graph, nil, deps)
	state := exec.graphState

	step := StepConfig{
		Action: "wizard.run",
		Flow:   "sage-review",
	}

	result := actionWizardRun(exec, "sage-review", step, state)
	if result.Error != nil {
		t.Fatalf("actionWizardRun returned error: %v", result.Error)
	}

	// Without result.json, result is "success" and verdict is not promoted.
	if result.Outputs["result"] != "success" {
		t.Errorf("outputs[result] = %q, want %q", result.Outputs["result"], "success")
	}
	if v, ok := result.Outputs["verdict"]; ok {
		t.Errorf("outputs[verdict] should not be set, got %q", v)
	}
}

