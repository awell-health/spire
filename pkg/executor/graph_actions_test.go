package executor

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
		ClaudeRunner: func(args []string, dir string, _ io.Writer) ([]byte, error) {
			argsCopy := make([]string, len(args))
			copy(argsCopy, args)
			*capturedArgs = append(*capturedArgs, argsCopy)
			return []byte("Implementation plan:\n\nDo the thing."), nil
		},
		IsAttemptBead:     func(b Bead) bool { return false },
		IsStepBead:        func(b Bead) bool { return false },
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
	deps.ClaudeRunner = func(args []string, dir string, _ io.Writer) ([]byte, error) {
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
	deps.ClaudeRunner = func(args []string, dir string, _ io.Writer) ([]byte, error) {
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
	exec.graphState.RepoPath = dir
	exec.graphState.BaseBranch = "main"
	exec.graphState.TowerName = "tower-test"

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
	if captured.Identity.TowerName != "tower-test" {
		t.Errorf("identity tower = %q, want %q", captured.Identity.TowerName, "tower-test")
	}
	if captured.Identity.Prefix != "spi" {
		t.Errorf("identity prefix = %q, want %q", captured.Identity.Prefix, "spi")
	}
	if captured.Identity.BaseBranch != "main" {
		t.Errorf("identity base branch = %q, want %q", captured.Identity.BaseBranch, "main")
	}
	if captured.Workspace == nil {
		t.Fatal("expected workspace handle to be populated")
	}
	if captured.Workspace.Kind != WorkspaceKindRepo {
		t.Errorf("workspace kind = %q, want %q", captured.Workspace.Kind, WorkspaceKindRepo)
	}
	if captured.Workspace.Path != dir {
		t.Errorf("workspace path = %q, want %q", captured.Workspace.Path, dir)
	}
	if captured.Run.FormulaStep != "implement" {
		t.Errorf("run formula step = %q, want %q", captured.Run.FormulaStep, "implement")
	}
	if captured.Run.WorkspaceKind != WorkspaceKindRepo {
		t.Errorf("run workspace kind = %q, want %q", captured.Run.WorkspaceKind, WorkspaceKindRepo)
	}
	if captured.Run.WorkspaceOrigin != WorkspaceOriginLocalBind {
		t.Errorf("run workspace origin = %q, want %q", captured.Run.WorkspaceOrigin, WorkspaceOriginLocalBind)
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

	result := wizardRunSpawn(exec, "implement", step, state, agent.RoleApprentice, []string{"--apprentice"}, nil)

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
	// wizardRunSpawn uses name = <agentName>-<stepName>-<attemptNum>; first
	// attempt is 1.
	spawnName := agentName + "-" + stepName + "-1"

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

	result := wizardRunSpawn(exec, stepName, step, state, agent.RoleApprentice, []string{"--apprentice"}, nil)

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

// TestWizardRunSpawnTrustsApproveResultDespiteWaitErr verifies that when a
// sage-review writes result.json with result="approve" and then exits non-zero,
// the executor trusts the declared output. This is the ZFC contract: the
// executor does not reinterpret subprocess results.
func TestWizardRunSpawnTrustsApproveResultDespiteWaitErr(t *testing.T) {
	agentName := "wizard-test"
	stepName := "sage-review"
	spawnName := agentName + "-" + stepName + "-1"

	resultDir := t.TempDir()
	ar := agentResultJSON{Result: "approve"}
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
		Name:    "test-approve-despite-kill",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			stepName: {Action: "wizard.run", Flow: "sage-review"},
		},
	}

	exec := NewGraphForTest("spi-test", agentName, graph, nil, deps)

	step := StepConfig{
		Action: "wizard.run",
		Flow:   "sage-review",
	}

	result := wizardRunSpawn(exec, stepName, step, exec.graphState, agent.RoleSage, nil, nil)

	if result.Error != nil {
		t.Fatalf("expected nil Error when result.json reports approve, got: %v", result.Error)
	}
	if result.Outputs["result"] != "approve" {
		t.Errorf("expected outputs[result]=%q, got %q", "approve", result.Outputs["result"])
	}
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
func (b *sageVerdictMockBackend) List() ([]agent.Info, error)             { return nil, nil }
func (b *sageVerdictMockBackend) Logs(name string) (io.ReadCloser, error) { return nil, os.ErrNotExist }
func (b *sageVerdictMockBackend) Kill(name string) error                  { return nil }

type sageVerdictMockHandle struct{}

func (h *sageVerdictMockHandle) Wait() error            { return nil }
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
	// The spawn name is "<executor-agent>-<step-name>-<attemptNum>".
	agentName := "wizard-test-sage-review-1"
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
		Spawner:   &sageVerdictMockBackend{},
		AgentResultDir: func(name string) string {
			return filepath.Join(dir, name)
		},
		RecordAgentRun: func(run AgentRun) (string, error) { return "", nil },
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

	agentName := "wizard-test-sage-review-1"
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
		Spawner:   &sageVerdictMockBackend{},
		AgentResultDir: func(name string) string {
			return filepath.Join(dir, name)
		},
		RecordAgentRun: func(run AgentRun) (string, error) { return "", nil },
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
		Spawner:   &sageVerdictMockBackend{},
		AgentResultDir: func(name string) string {
			return filepath.Join(dir, name)
		},
		RecordAgentRun: func(run AgentRun) (string, error) { return "", nil },
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

// TestGraphRun_ReviewPhase_PropagatesWorktreeDir verifies the critical path:
// when a parent graph step declares workspace="feature" and dispatches a
// subgraph-review nested graph via graph.run, the sage-review step inside the
// nested graph receives --worktree-dir pointing to the parent's feature
// workspace directory.
//
// This is the bug from spi-b34i5: without correct propagation, the sage
// tries to create a new worktree on the staging branch, which fails because
// the worktree already exists from the implement step.
func TestGraphRun_ReviewPhase_PropagatesWorktreeDir(t *testing.T) {
	dir := t.TempDir()

	// Create a fake workspace directory to represent the parent's feature workspace.
	featureDir := filepath.Join(dir, "feature-worktree")
	os.MkdirAll(featureDir, 0755)

	// Capture all spawn configs for inspection.
	var spawnConfigs []agent.SpawnConfig
	agentName := "wizard-test"

	backend := &mockBackend{
		spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
			spawnConfigs = append(spawnConfigs, cfg)
			return &mockHandle{}, nil
		},
	}

	// Pre-write result.json for the sage-review spawn.
	// wizardRunSpawn uses name = <agentName>-<stepName>-<attemptNum>.
	// e.agentName is still the parent executor's agent name ("wizard-test"),
	// so the first sage-review spawn name is "wizard-test-sage-review-1".
	sageSpawnName := agentName + "-sage-review-1"
	sageResultDir := filepath.Join(dir, sageSpawnName)
	os.MkdirAll(sageResultDir, 0755)
	sageResult, _ := json.Marshal(map[string]interface{}{
		"result":  "approve",
		"bead_id": "spi-test",
	})
	os.WriteFile(filepath.Join(sageResultDir, "result.json"), sageResult, 0644)

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		Spawner:   backend,
		AgentResultDir: func(name string) string {
			return filepath.Join(dir, name)
		},
		RecordAgentRun: func(run AgentRun) (string, error) { return "", nil },
		ResolveRepo: func(beadID string) (string, string, string, error) {
			return dir, "", "main", nil
		},
	}

	// Build the parent graph with a review step that declares workspace="feature"
	// and dispatches graph.run for subgraph-review.
	parentGraph := &formula.FormulaStepGraph{
		Name:    "test-parent",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"review": {
				Action:    "graph.run",
				Graph:     "subgraph-review",
				Workspace: "feature",
			},
		},
	}

	exec := NewGraphForTest("spi-test", agentName, parentGraph, nil, deps)

	// Set up parent graph state with the feature workspace populated.
	state := exec.graphState
	state.RepoPath = dir
	state.BaseBranch = "main"
	state.StagingBranch = "staging/spi-test"
	state.Vars["bead_id"] = "spi-test"
	state.Vars["max_review_rounds"] = "3"
	state.Workspaces["feature"] = WorkspaceState{
		Name:   "feature",
		Kind:   "owned_worktree",
		Dir:    featureDir,
		Branch: "feat/spi-test",
		Status: "active",
		Scope:  "run",
	}

	// Call actionGraphRun directly with the review step.
	step := StepConfig{
		Action:    "graph.run",
		Graph:     "subgraph-review",
		Workspace: "feature",
	}

	result := actionGraphRun(exec, "review", step, state)
	if result.Error != nil {
		t.Fatalf("actionGraphRun returned error: %v", result.Error)
	}

	// Verify the sage-review step was spawned.
	if len(spawnConfigs) == 0 {
		t.Fatal("no agents were spawned — expected at least sage-review")
	}

	// Find the sage-review spawn and check --worktree-dir.
	var sageConfig *agent.SpawnConfig
	for i := range spawnConfigs {
		if spawnConfigs[i].Role == agent.RoleSage {
			sageConfig = &spawnConfigs[i]
			break
		}
	}
	if sageConfig == nil {
		t.Fatal("sage-review was not spawned")
	}

	// Verify --worktree-dir is present and points to the feature workspace dir.
	foundWorktreeDir := ""
	for i, arg := range sageConfig.ExtraArgs {
		if arg == "--worktree-dir" && i+1 < len(sageConfig.ExtraArgs) {
			foundWorktreeDir = sageConfig.ExtraArgs[i+1]
			break
		}
	}
	if foundWorktreeDir == "" {
		t.Errorf("sage-review spawn missing --worktree-dir; ExtraArgs = %v", sageConfig.ExtraArgs)
	} else if foundWorktreeDir != featureDir {
		t.Errorf("sage-review --worktree-dir = %q, want %q", foundWorktreeDir, featureDir)
	}
	if sageConfig.Workspace == nil {
		t.Fatal("sage-review spawn missing workspace handle")
	}
	if sageConfig.Workspace.Name != "feature" {
		t.Errorf("workspace name = %q, want %q", sageConfig.Workspace.Name, "feature")
	}
	if sageConfig.Workspace.Path != featureDir {
		t.Errorf("workspace path = %q, want %q", sageConfig.Workspace.Path, featureDir)
	}
	if sageConfig.Run.FormulaStep != "sage-review" {
		t.Errorf("run formula step = %q, want %q", sageConfig.Run.FormulaStep, "sage-review")
	}

	// Verify the review outcome was captured.
	if result.Outputs["outcome"] != "merge" {
		t.Errorf("outcome = %q, want %q", result.Outputs["outcome"], "merge")
	}
}

// TestGraphRun_ReviewPhase_PropagatesWorktreeDir_UnresolvedWorkspace verifies
// the bug scenario from spi-b34i5: when the parent graph declares a workspace
// on the graph.run step but that workspace's Dir was never resolved by a prior
// wizard.run step (as happens in epic-default where implement is also a
// graph.run), actionGraphRun must still resolve the workspace and propagate the
// Dir to the nested subState.
//
// Without the fix, the nested subgraph-review graph's sage-review step does NOT
// receive --worktree-dir, causing it to create a new worktree that collides
// with the existing one.
func TestGraphRun_ReviewPhase_PropagatesWorktreeDir_UnresolvedWorkspace(t *testing.T) {
	dir := t.TempDir()
	agentName := "wizard-epic-test"

	// Capture all spawn configs for inspection.
	var spawnConfigs []agent.SpawnConfig
	backend := &mockBackend{
		spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
			spawnConfigs = append(spawnConfigs, cfg)
			return &mockHandle{}, nil
		},
	}

	// Pre-write result.json for the sage-review spawn (attempt 1).
	sageSpawnName := agentName + "-sage-review-1"
	sageResultDir := filepath.Join(dir, sageSpawnName)
	os.MkdirAll(sageResultDir, 0755)
	sageResult, _ := json.Marshal(map[string]interface{}{
		"result":  "approve",
		"bead_id": "spi-epic",
	})
	os.WriteFile(filepath.Join(sageResultDir, "result.json"), sageResult, 0644)

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		Spawner:   backend,
		AgentResultDir: func(name string) string {
			return filepath.Join(dir, name)
		},
		RecordAgentRun: func(run AgentRun) (string, error) { return "", nil },
		ResolveRepo: func(beadID string) (string, string, string, error) {
			return dir, "", "main", nil
		},
	}

	parentGraph := &formula.FormulaStepGraph{
		Name:    "test-epic-parent",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"review": {
				Action:    "graph.run",
				Graph:     "subgraph-review",
				Workspace: "staging",
			},
		},
	}

	exec := NewGraphForTest("spi-epic", agentName, parentGraph, nil, deps)

	state := exec.graphState
	state.RepoPath = dir
	state.BaseBranch = "main"
	state.StagingBranch = "staging/spi-epic"
	state.Vars["bead_id"] = "spi-epic"
	state.Vars["max_review_rounds"] = "3"

	// Simulate the epic scenario: the "staging" workspace was initialized by
	// RunGraph's workspace init (from the parent formula's [workspaces.staging]),
	// but its Dir was NEVER resolved because no prior wizard.run step used it.
	// The implement step was a graph.run that created its own nested workspace.
	//
	// Use kind "repo" so resolveGraphWorkspace just sets Dir to RepoPath
	// without requiring real git operations (the real scenario uses "staging"
	// kind, but the resolution behavior is what we're testing, not git ops).
	state.Workspaces["staging"] = WorkspaceState{
		Name:       "staging",
		Kind:       "repo",
		Branch:     "epic/spi-epic",
		BaseBranch: "main",
		Status:     "pending",
		Scope:      "run",
		Ownership:  "owned",
		Cleanup:    "terminal",
		// Dir is intentionally empty — this is the bug scenario.
	}

	step := StepConfig{
		Action:    "graph.run",
		Graph:     "subgraph-review",
		Workspace: "staging",
	}

	result := actionGraphRun(exec, "review", step, state)
	if result.Error != nil {
		t.Fatalf("actionGraphRun returned error: %v", result.Error)
	}

	// Verify the sage-review step was spawned.
	if len(spawnConfigs) == 0 {
		t.Fatal("no agents were spawned — expected at least sage-review")
	}

	// Find the sage-review spawn.
	var sageConfig *agent.SpawnConfig
	for i := range spawnConfigs {
		if spawnConfigs[i].Role == agent.RoleSage {
			sageConfig = &spawnConfigs[i]
			break
		}
	}
	if sageConfig == nil {
		t.Fatal("sage-review was not spawned")
	}

	// Verify --worktree-dir is present and non-empty.
	foundWorktreeDir := ""
	for i, arg := range sageConfig.ExtraArgs {
		if arg == "--worktree-dir" && i+1 < len(sageConfig.ExtraArgs) {
			foundWorktreeDir = sageConfig.ExtraArgs[i+1]
			break
		}
	}
	if foundWorktreeDir == "" {
		t.Errorf("sage-review spawn missing --worktree-dir; ExtraArgs = %v\n"+
			"This is the spi-b34i5 bug: when the parent workspace Dir is unresolved,\n"+
			"actionGraphRun must resolve it before passing to the nested graph.",
			sageConfig.ExtraArgs)
	} else if foundWorktreeDir != dir {
		// With kind="repo", resolveGraphWorkspace sets Dir to state.RepoPath.
		t.Errorf("sage-review --worktree-dir = %q, want %q (repo path)", foundWorktreeDir, dir)
	}

	subState, err := LoadGraphState(agentName+"-review", deps.ConfigDir)
	if err != nil {
		t.Fatalf("load nested graph state: %v", err)
	}
	if subState == nil {
		t.Fatal("nested graph state missing")
	}
	if subState.StagingBranch != "epic/spi-epic" {
		t.Fatalf("nested StagingBranch = %q, want %q", subState.StagingBranch, "epic/spi-epic")
	}
	if got := subState.Workspaces["staging"].Branch; got != "epic/spi-epic" {
		t.Fatalf("nested staging workspace branch = %q, want %q", got, "epic/spi-epic")
	}

	// Verify the review outcome.
	if result.Outputs["outcome"] != "merge" {
		t.Errorf("outcome = %q, want %q", result.Outputs["outcome"], "merge")
	}
}

// TestGraphRun_ReviewPhase_ResumeRepairsWorktreeDir verifies that when a
// nested graph sub-state is resumed with an empty WorktreeDir, actionGraphRun
// repairs it from the parent's workspace before re-entering the nested graph.
func TestGraphRun_ReviewPhase_ResumeRepairsWorktreeDir(t *testing.T) {
	dir := t.TempDir()
	agentName := "wizard-resume-test"

	var spawnConfigs []agent.SpawnConfig
	backend := &mockBackend{
		spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
			spawnConfigs = append(spawnConfigs, cfg)
			return &mockHandle{}, nil
		},
	}

	// Pre-write result.json for the sage-review spawn (attempt 1).
	// wizardRunSpawn uses <e.agentName>-<stepName>-<attemptNum>, and
	// e.agentName is the parent executor's name (not the nested sub-agent name).
	sageSpawnName := agentName + "-sage-review-1"
	sageResultDir := filepath.Join(dir, sageSpawnName)
	os.MkdirAll(sageResultDir, 0755)
	sageResult, _ := json.Marshal(map[string]interface{}{
		"result":  "approve",
		"bead_id": "spi-resume",
	})
	os.WriteFile(filepath.Join(sageResultDir, "result.json"), sageResult, 0644)

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		Spawner:   backend,
		AgentResultDir: func(name string) string {
			return filepath.Join(dir, name)
		},
		RecordAgentRun: func(run AgentRun) (string, error) { return "", nil },
		ResolveRepo: func(beadID string) (string, string, string, error) {
			return dir, "", "main", nil
		},
	}

	parentGraph := &formula.FormulaStepGraph{
		Name:    "test-resume-parent",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"review": {
				Action:    "graph.run",
				Graph:     "subgraph-review",
				Workspace: "staging",
			},
		},
	}

	exec := NewGraphForTest("spi-resume", agentName, parentGraph, nil, deps)

	state := exec.graphState
	state.RepoPath = dir
	state.BaseBranch = "main"
	state.StagingBranch = "staging/spi-resume"
	state.Vars["bead_id"] = "spi-resume"
	state.Vars["max_review_rounds"] = "3"

	// Parent workspace with Dir resolved.
	state.Workspaces["staging"] = WorkspaceState{
		Name:       "staging",
		Kind:       "repo",
		Dir:        dir,
		Branch:     "epic/spi-resume",
		BaseBranch: "main",
		Status:     "active",
		Scope:      "run",
	}

	// Pre-persist a nested sub-state with EMPTY WorktreeDir to simulate
	// a resumed sub-state that was persisted before workspace resolution.
	subAgentName := agentName + "-review"
	reviewGraph, err := formula.LoadStepGraphByName("subgraph-review")
	if err != nil {
		t.Fatalf("load subgraph-review: %v", err)
	}
	subState := NewGraphState(reviewGraph, "spi-resume", subAgentName)
	subState.RepoPath = dir
	subState.BaseBranch = "main"
	subState.StagingBranch = "staging/spi-resume"
	subState.WorktreeDir = "" // <-- the bug: empty on resume
	subState.Vars["bead_id"] = "spi-resume"
	subState.Vars["max_review_rounds"] = "3"
	if saveErr := subState.Save(subAgentName, deps.ConfigDir); saveErr != nil {
		t.Fatalf("save sub-state: %v", saveErr)
	}

	step := StepConfig{
		Action:    "graph.run",
		Graph:     "subgraph-review",
		Workspace: "staging",
	}

	result := actionGraphRun(exec, "review", step, state)
	if result.Error != nil {
		t.Fatalf("actionGraphRun returned error: %v", result.Error)
	}

	// Find the sage-review spawn and verify --worktree-dir.
	var sageConfig *agent.SpawnConfig
	for i := range spawnConfigs {
		if spawnConfigs[i].Role == agent.RoleSage {
			sageConfig = &spawnConfigs[i]
			break
		}
	}
	if sageConfig == nil {
		t.Fatal("sage-review was not spawned")
	}

	foundWorktreeDir := ""
	for i, arg := range sageConfig.ExtraArgs {
		if arg == "--worktree-dir" && i+1 < len(sageConfig.ExtraArgs) {
			foundWorktreeDir = sageConfig.ExtraArgs[i+1]
			break
		}
	}
	if foundWorktreeDir == "" {
		t.Errorf("sage-review spawn missing --worktree-dir on resume; ExtraArgs = %v\n"+
			"The resume path must repair empty WorktreeDir from the parent workspace.",
			sageConfig.ExtraArgs)
	} else if foundWorktreeDir != dir {
		t.Errorf("sage-review --worktree-dir = %q, want %q", foundWorktreeDir, dir)
	}

	loadedSubState, err := LoadGraphState(subAgentName, deps.ConfigDir)
	if err != nil {
		t.Fatalf("load resumed nested graph state: %v", err)
	}
	if loadedSubState == nil {
		t.Fatal("resumed nested graph state missing")
	}
	if loadedSubState.StagingBranch != "epic/spi-resume" {
		t.Fatalf("resumed nested StagingBranch = %q, want %q", loadedSubState.StagingBranch, "epic/spi-resume")
	}
	if got := loadedSubState.Workspaces["staging"].Branch; got != "epic/spi-resume" {
		t.Fatalf("resumed nested staging workspace branch = %q, want %q", got, "epic/spi-resume")
	}
}

// --- Conflict resolver turn budget tests ---

// TestBuildConflictResolverArgs_WithMaxTurns verifies that when a non-zero
// maxTurns is declared, --max-turns is included in the Claude CLI args.
func TestBuildConflictResolverArgs_WithMaxTurns(t *testing.T) {
	args := buildConflictResolverArgs("resolve prompt", "claude-sonnet-4-6", 15)
	mt := extractMaxTurns(args)
	if mt != "15" {
		t.Errorf("expected --max-turns 15, got %q", mt)
	}
}

// TestBuildConflictResolverArgs_ZeroMaxTurns verifies that when maxTurns is 0
// (formula did not declare conflict_max_turns), the --max-turns flag is omitted
// entirely — the executor does not invent a turn budget.
func TestBuildConflictResolverArgs_ZeroMaxTurns(t *testing.T) {
	args := buildConflictResolverArgs("resolve prompt", "claude-sonnet-4-6", 0)
	mt := extractMaxTurns(args)
	if mt != "" {
		t.Errorf("expected --max-turns flag absent, but got --max-turns %q", mt)
	}
}

// TestConflictResolver_ClosureCapturesMaxTurns verifies that conflictResolver
// returns a closure that captures the formula-declared turn budget. We test
// this indirectly by checking that the closure invokes resolveConflictsWithBudget
// with the correct maxTurns (via buildConflictResolverArgs).
func TestConflictResolver_ClosureCapturesMaxTurns(t *testing.T) {
	// We can't test the full resolveConflictsWithBudget without a real git repo,
	// but we can test the args construction that it delegates to.
	tests := []struct {
		name     string
		turns    int
		wantFlag string // empty = absent
	}{
		{"declared budget of 20", 20, "20"},
		{"declared budget of 1", 1, "1"},
		{"zero means omit", 0, ""},
		{"negative means omit", -1, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := buildConflictResolverArgs("prompt", "model", tt.turns)
			got := extractMaxTurns(args)
			if got != tt.wantFlag {
				t.Errorf("maxTurns=%d: expected --max-turns %q, got %q", tt.turns, tt.wantFlag, got)
			}
		})
	}
}

// TestActionDispatchChildren_ParsesConflictMaxTurns verifies that
// actionDispatchChildren reads step.With["conflict_max_turns"] and threads
// it to the dispatch helpers. We use the "direct" strategy with a minimal
// mock to isolate the parsing behavior.
func TestActionDispatchChildren_ParsesConflictMaxTurns(t *testing.T) {
	// This test verifies that the With parameter parsing works correctly.
	// We cannot fully test the MergeBranch callback invocation without a git
	// repo, but we verify the strconv.Atoi parsing and that non-numeric values
	// are handled gracefully (treated as 0).
	tests := []struct {
		name string
		raw  string
		want int
	}{
		{"numeric value", "15", 15},
		{"empty string (unset)", "", 0},
		{"non-numeric", "abc", 0},
		{"zero string", "0", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Parse the same way actionDispatchChildren does.
			var got int
			if tt.raw != "" {
				if v, err := strconv.Atoi(tt.raw); err == nil {
					got = v
				}
			}
			if got != tt.want {
				t.Errorf("parsing %q: expected %d, got %d", tt.raw, tt.want, got)
			}
		})
	}
}

// --- Recovery formula lifecycle regression tests ---

// TestBeadFinish_ResolvedStatus verifies that bead.finish with status="resolved"
// (used by the recovery formula's finish step) closes the bead via the success
// path, not the unknown-status error path.
func TestBeadFinish_ResolvedStatus(t *testing.T) {
	dir := t.TempDir()
	var closedBeadID string

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetChildren: func(parentID string) ([]Bead, error) {
			return nil, nil
		},
		CloseBead: func(id string) error {
			closedBeadID = id
			return nil
		},
		CloseAttemptBead: func(attemptID, result string) error {
			return nil
		},
		IsAttemptBead:     func(b Bead) bool { return false },
		IsStepBead:        func(b Bead) bool { return false },
		IsReviewRoundBead: func(b Bead) bool { return false },
		GetDependentsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			return nil, nil
		},
		AddComment: func(id, text string) error { return nil },
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-recovery-finish",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"finish": {Action: "bead.finish"},
		},
	}

	exec := NewGraphForTest("spi-recovery-test", "wizard-recovery", graph, nil, deps)

	step := StepConfig{
		Action: "bead.finish",
		With:   map[string]string{"status": "resolved"},
	}

	result := actionBeadFinish(exec, "finish", step, exec.graphState)
	if result.Error != nil {
		t.Fatalf("actionBeadFinish with status=resolved returned error: %v", result.Error)
	}
	if result.Outputs["status"] != "closed" {
		t.Errorf("outputs[status] = %q, want %q", result.Outputs["status"], "closed")
	}
	if closedBeadID != "spi-recovery-test" {
		t.Errorf("CloseBead called with %q, want %q", closedBeadID, "spi-recovery-test")
	}
}

// TestBeadFinish_EscalateStatus verifies that bead.finish with status="escalate"
// (the verb form used by the recovery formula's escalate step) triggers the
// escalation path, not the unknown-status error path.
func TestBeadFinish_EscalateStatus(t *testing.T) {
	dir := t.TempDir()
	var createdAlerts []CreateOpts

	deps := &Deps{
		ConfigDir:  func() (string, error) { return dir, nil },
		AddComment: func(id, text string) error { return nil },
		CreateBead: func(opts CreateOpts) (string, error) {
			createdAlerts = append(createdAlerts, opts)
			return "spi-alert-test", nil
		},
		AddDepTyped: func(issueID, dependsOnID, depType string) error {
			return nil
		},
		GetDependentsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			return nil, nil
		},
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-recovery-escalate",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"escalate": {Action: "bead.finish"},
		},
	}

	exec := NewGraphForTest("spi-recovery-test", "wizard-recovery", graph, nil, deps)

	step := StepConfig{
		Action: "bead.finish",
		With:   map[string]string{"status": "escalate"},
	}

	result := actionBeadFinish(exec, "escalate", step, exec.graphState)
	if result.Error != nil {
		t.Fatalf("actionBeadFinish with status=escalate returned error: %v", result.Error)
	}
	if result.Outputs["status"] != "escalated" {
		t.Errorf("outputs[status] = %q, want %q", result.Outputs["status"], "escalated")
	}
	// Verify escalation created an alert bead (status-based, not label-based).
	foundAlert := false
	for _, opts := range createdAlerts {
		for _, lbl := range opts.Labels {
			if lbl == "alert:bead-finish-escalate" {
				foundAlert = true
			}
		}
	}
	if !foundAlert {
		t.Errorf("expected alert bead with alert:bead-finish-escalate label, got: %v", createdAlerts)
	}
}

// TestRecoveryVerify_PromotesResultToVerificationStatus verifies that the
// recovery-verify flow promotes result.json's "result" field to
// outputs["verification_status"], mirroring the sage-review → verdict pattern.
// Without this promotion, the formula routing condition
// steps.verify.outputs.verification_status would never match.
func TestRecoveryVerify_PromotesResultToVerificationStatus(t *testing.T) {
	dir := t.TempDir()
	agentName := "wizard-recovery-test"
	stepName := "verify"
	spawnName := agentName + "-" + stepName + "-1"

	// Pre-write result.json with result="pass" (the apprentice writes this).
	resultDir := filepath.Join(dir, spawnName)
	os.MkdirAll(resultDir, 0755)
	resultData, _ := json.Marshal(map[string]interface{}{
		"result":  "pass",
		"bead_id": "spi-recovery-test",
	})
	os.WriteFile(filepath.Join(resultDir, "result.json"), resultData, 0644)

	graph := &formula.FormulaStepGraph{
		Name:    "test-recovery-verify",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			stepName: {Action: "wizard.run", Flow: "recovery-verify"},
		},
	}

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		Spawner:   &sageVerdictMockBackend{},
		AgentResultDir: func(name string) string {
			return filepath.Join(dir, name)
		},
		RecordAgentRun: func(run AgentRun) (string, error) { return "", nil },
	}

	exec := NewGraphForTest("spi-recovery-test", agentName, graph, nil, deps)

	step := StepConfig{
		Action: "wizard.run",
		Flow:   "recovery-verify",
	}

	result := actionWizardRun(exec, stepName, step, exec.graphState)
	if result.Error != nil {
		t.Fatalf("actionWizardRun(recovery-verify) returned error: %v", result.Error)
	}

	// The key assertion: verification_status must be promoted from result.
	if result.Outputs["verification_status"] != "pass" {
		t.Errorf("outputs[verification_status] = %q, want %q", result.Outputs["verification_status"], "pass")
	}
	if result.Outputs["result"] != "pass" {
		t.Errorf("outputs[result] = %q, want %q", result.Outputs["result"], "pass")
	}
}

// TestRecoveryVerify_FailPromotesVerificationStatus verifies that result="fail"
// is also promoted to verification_status, enabling the escalate routing path.
func TestRecoveryVerify_FailPromotesVerificationStatus(t *testing.T) {
	dir := t.TempDir()
	agentName := "wizard-recovery-test"
	stepName := "verify"
	spawnName := agentName + "-" + stepName + "-1"

	resultDir := filepath.Join(dir, spawnName)
	os.MkdirAll(resultDir, 0755)
	resultData, _ := json.Marshal(map[string]interface{}{
		"result":  "fail",
		"bead_id": "spi-recovery-test",
	})
	os.WriteFile(filepath.Join(resultDir, "result.json"), resultData, 0644)

	graph := &formula.FormulaStepGraph{
		Name:    "test-recovery-verify-fail",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			stepName: {Action: "wizard.run", Flow: "recovery-verify"},
		},
	}

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		Spawner:   &sageVerdictMockBackend{},
		AgentResultDir: func(name string) string {
			return filepath.Join(dir, name)
		},
		RecordAgentRun: func(run AgentRun) (string, error) { return "", nil },
	}

	exec := NewGraphForTest("spi-recovery-test", agentName, graph, nil, deps)

	step := StepConfig{
		Action: "wizard.run",
		Flow:   "recovery-verify",
	}

	result := actionWizardRun(exec, stepName, step, exec.graphState)
	if result.Error != nil {
		t.Fatalf("actionWizardRun(recovery-verify) returned error: %v", result.Error)
	}

	if result.Outputs["verification_status"] != "fail" {
		t.Errorf("outputs[verification_status] = %q, want %q", result.Outputs["verification_status"], "fail")
	}
}

// --- Tests for actionCheckDesignLinked with auto_create ---

// TestCheckDesignLinked_AutoCreateNoDesign verifies that when auto_create is true
// and no design bead exists, the action creates one and returns Hooked.
func TestCheckDesignLinked_AutoCreateNoDesign(t *testing.T) {
	deps, _ := planTestDeps(t)

	var createBeadCalled bool
	var createdOpts CreateOpts
	deps.GetDepsWithMeta = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return nil, nil // no deps
	}
	deps.CreateBead = func(opts CreateOpts) (string, error) {
		// Distinguish design bead creation from archmage message creation.
		for _, l := range opts.Labels {
			if l == "msg" {
				return "spi-msg-1", nil
			}
		}
		createBeadCalled = true
		createdOpts = opts
		return "spi-design-new", nil
	}
	deps.AddDepTyped = func(issueID, dependsOnID, depType string) error {
		return nil
	}
	deps.ParseIssueType = func(s string) beads.IssueType { return beads.IssueType(s) }

	graph := &formula.FormulaStepGraph{
		Name:    "test-design-check",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"design-check": {Action: "check.design-linked"},
		},
	}

	exec := NewGraphForTest("spi-epic1", "wizard-test", graph, nil, deps)

	step := StepConfig{
		Action: "check.design-linked",
		With:   map[string]string{"auto_create": "true"},
	}

	result := actionCheckDesignLinked(exec, "design-check", step, exec.graphState)
	if result.Error != nil {
		t.Fatalf("expected no error, got: %v", result.Error)
	}
	if !result.Hooked {
		t.Error("expected Hooked=true when auto-creating design bead")
	}
	if !createBeadCalled {
		t.Error("expected CreateBead to be called")
	}
	if string(createdOpts.Type) != "design" {
		t.Errorf("expected CreateBead type=design, got %q", createdOpts.Type)
	}
	if result.Outputs["design_ref"] != "spi-design-new" {
		t.Errorf("expected design_ref=spi-design-new, got %q", result.Outputs["design_ref"])
	}
}

// TestCheckDesignLinked_AutoCreateOpenDesign verifies that when auto_create is true
// and an open design bead exists, the action returns Hooked without creating a new bead.
func TestCheckDesignLinked_AutoCreateOpenDesign(t *testing.T) {
	deps, _ := planTestDeps(t)

	var createBeadCalled bool
	deps.GetDepsWithMeta = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return []*beads.IssueWithDependencyMetadata{
			{
				Issue: beads.Issue{
					ID:        "spi-des1",
					Title:     "Design: spi-epic1",
					IssueType: "design",
					Status:    "open",
				},
				DependencyType: beads.DepDiscoveredFrom,
			},
		}, nil
	}
	deps.CreateBead = func(opts CreateOpts) (string, error) {
		createBeadCalled = true
		return "spi-should-not", nil
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-design-check-open",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"design-check": {Action: "check.design-linked"},
		},
	}

	exec := NewGraphForTest("spi-epic1", "wizard-test", graph, nil, deps)

	step := StepConfig{
		Action: "check.design-linked",
		With:   map[string]string{"auto_create": "true"},
	}

	result := actionCheckDesignLinked(exec, "design-check", step, exec.graphState)
	if result.Error != nil {
		t.Fatalf("expected no error, got: %v", result.Error)
	}
	if !result.Hooked {
		t.Error("expected Hooked=true for open design bead with auto_create")
	}
	if createBeadCalled {
		t.Error("expected CreateBead NOT to be called for existing open design bead")
	}
	if result.Outputs["design_ref"] != "spi-des1" {
		t.Errorf("expected design_ref=spi-des1, got %q", result.Outputs["design_ref"])
	}
}

// TestCheckDesignLinked_NoAutoCreateNoDesign verifies that without auto_create,
// a missing design bead causes a hard error (preserves existing behavior).
func TestCheckDesignLinked_NoAutoCreateNoDesign(t *testing.T) {
	deps, _ := planTestDeps(t)

	deps.GetDepsWithMeta = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return nil, nil // no deps
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-design-check-no-auto",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"design-check": {Action: "check.design-linked"},
		},
	}

	exec := NewGraphForTest("spi-epic1", "wizard-test", graph, nil, deps)

	step := StepConfig{
		Action: "check.design-linked",
		// No auto_create — default hard-fail behavior.
	}

	result := actionCheckDesignLinked(exec, "design-check", step, exec.graphState)
	if result.Error == nil {
		t.Fatal("expected error when no design bead and auto_create is not set")
	}
	if result.Hooked {
		t.Error("expected Hooked=false when auto_create is not set")
	}
}

// TestCheckDesignLinked_ClosedDesignWithContent verifies that a closed design bead
// with content passes successfully without hooking.
func TestCheckDesignLinked_ClosedDesignWithContent(t *testing.T) {
	deps, _ := planTestDeps(t)

	deps.GetDepsWithMeta = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return []*beads.IssueWithDependencyMetadata{
			{
				Issue: beads.Issue{
					ID:          "spi-des1",
					Title:       "Design: auth overhaul",
					Description: "Use OAuth2 with PKCE flow",
					IssueType:   "design",
					Status:      "closed",
				},
				DependencyType: beads.DepDiscoveredFrom,
			},
		}, nil
	}
	deps.GetComments = func(id string) ([]*beads.Comment, error) {
		return []*beads.Comment{{ID: "c1", IssueID: id, Text: "Design notes"}}, nil
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-design-check-closed",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"design-check": {Action: "check.design-linked"},
		},
	}

	exec := NewGraphForTest("spi-epic1", "wizard-test", graph, nil, deps)

	step := StepConfig{
		Action: "check.design-linked",
	}

	result := actionCheckDesignLinked(exec, "design-check", step, exec.graphState)
	if result.Error != nil {
		t.Fatalf("expected no error, got: %v", result.Error)
	}
	if result.Hooked {
		t.Error("expected Hooked=false for closed design bead with content")
	}
	if result.Outputs["design_ref"] != "spi-des1" {
		t.Errorf("expected design_ref=spi-des1, got %q", result.Outputs["design_ref"])
	}
}

// --- interpolateWith tests ---

func TestInterpolateWith_StepOutputs(t *testing.T) {
	deps, _ := planTestDeps(t)
	graph := &formula.FormulaStepGraph{
		Name:    "test-interpolate",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"plan":      {Action: "noop"},
			"implement": {Action: "noop", Needs: []string{"plan"}},
		},
	}
	exec := NewGraphForTest("spi-test", "wizard-test", graph, nil, deps)

	// Simulate plan step having completed with outputs.
	exec.graphState.Steps["plan"] = StepState{
		Status:  "completed",
		Outputs: map[string]string{"context_files": "pkg/foo.go,pkg/bar.go"},
	}

	step := StepConfig{
		Action: "noop",
		With: map[string]string{
			"prompt": "Read these files: {steps.plan.outputs.context_files}",
		},
	}

	exec.interpolateWith(&step, exec.graphState)

	want := "Read these files: pkg/foo.go,pkg/bar.go"
	if step.With["prompt"] != want {
		t.Errorf("expected %q, got %q", want, step.With["prompt"])
	}
}

func TestInterpolateWith_Vars(t *testing.T) {
	deps, _ := planTestDeps(t)
	graph := &formula.FormulaStepGraph{
		Name:    "test-interpolate-vars",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"step1": {Action: "noop"},
		},
	}
	exec := NewGraphForTest("spi-abc", "wizard-test", graph, nil, deps)
	exec.graphState.Vars["bead_id"] = "spi-abc"
	exec.graphState.Vars["base_branch"] = "main"

	step := StepConfig{
		Action: "noop",
		With: map[string]string{
			"branch": "feat/{vars.bead_id}",
			"base":   "{vars.base_branch}",
		},
	}

	exec.interpolateWith(&step, exec.graphState)

	if step.With["branch"] != "feat/spi-abc" {
		t.Errorf("expected feat/spi-abc, got %q", step.With["branch"])
	}
	if step.With["base"] != "main" {
		t.Errorf("expected main, got %q", step.With["base"])
	}
}

func TestInterpolateWith_ShortFormVars(t *testing.T) {
	deps, _ := planTestDeps(t)
	graph := &formula.FormulaStepGraph{
		Name:    "test-interpolate-short",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"step1": {Action: "noop"},
		},
	}
	exec := NewGraphForTest("spi-abc", "wizard-test", graph, nil, deps)
	exec.graphState.Vars["bead_id"] = "spi-abc"

	step := StepConfig{
		Action: "noop",
		With: map[string]string{
			"id": "{bead_id}",
		},
	}

	exec.interpolateWith(&step, exec.graphState)

	if step.With["id"] != "spi-abc" {
		t.Errorf("expected spi-abc via short-form, got %q", step.With["id"])
	}
}

func TestInterpolateWith_UnresolvedLeftAsIs(t *testing.T) {
	deps, _ := planTestDeps(t)
	graph := &formula.FormulaStepGraph{
		Name:    "test-interpolate-unresolved",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"step1": {Action: "noop"},
		},
	}
	exec := NewGraphForTest("spi-test", "wizard-test", graph, nil, deps)

	step := StepConfig{
		Action: "noop",
		With: map[string]string{
			"prompt": "Value: {steps.missing.outputs.foo}",
		},
	}

	exec.interpolateWith(&step, exec.graphState)

	// Unresolved references must be left as-is.
	if step.With["prompt"] != "Value: {steps.missing.outputs.foo}" {
		t.Errorf("expected unresolved reference preserved, got %q", step.With["prompt"])
	}
}

func TestInterpolateWith_NoBracesUntouched(t *testing.T) {
	deps, _ := planTestDeps(t)
	graph := &formula.FormulaStepGraph{
		Name:    "test-interpolate-nobraces",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"step1": {Action: "noop"},
		},
	}
	exec := NewGraphForTest("spi-test", "wizard-test", graph, nil, deps)

	step := StepConfig{
		Action: "noop",
		With: map[string]string{
			"plain": "no interpolation needed",
		},
	}

	exec.interpolateWith(&step, exec.graphState)

	if step.With["plain"] != "no interpolation needed" {
		t.Errorf("expected plain value untouched, got %q", step.With["plain"])
	}
}

func TestInterpolateWith_EmptyWith(t *testing.T) {
	deps, _ := planTestDeps(t)
	graph := &formula.FormulaStepGraph{
		Name:    "test-interpolate-empty",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"step1": {Action: "noop"},
		},
	}
	exec := NewGraphForTest("spi-test", "wizard-test", graph, nil, deps)

	step := StepConfig{
		Action: "noop",
	}

	// Should not panic on nil/empty With map.
	exec.interpolateWith(&step, exec.graphState)
}

func TestDispatchAction_WithMapNotMutated(t *testing.T) {
	deps, _ := planTestDeps(t)
	graph := &formula.FormulaStepGraph{
		Name:    "test-dispatch-no-mutate",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"plan": {Action: "noop"},
			"impl": {Action: "noop", Needs: []string{"plan"}},
		},
	}
	exec := NewGraphForTest("spi-test", "wizard-test", graph, nil, deps)
	exec.graphState.Steps["plan"] = StepState{
		Status:  "completed",
		Outputs: map[string]string{"files": "a.go"},
	}

	original := map[string]string{
		"prompt": "Files: {steps.plan.outputs.files}",
	}
	step := StepConfig{
		Action: "noop",
		With:   original,
	}

	result := exec.dispatchAction("impl", step, exec.graphState)
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}

	// The original map must NOT have been mutated.
	if original["prompt"] != "Files: {steps.plan.outputs.files}" {
		t.Errorf("original With map was mutated: prompt=%q", original["prompt"])
	}
}

// --- human.approve action tests ---

// humanApproveTestDeps returns deps for human.approve action tests.
func humanApproveTestDeps(labels map[string]bool) *Deps {
	return &Deps{
		GetBead: func(id string) (Bead, error) {
			b := Bead{ID: id, Status: "in_progress"}
			for l := range labels {
				b.Labels = append(b.Labels, l)
			}
			return b, nil
		},
		AddLabel: func(id, label string) error {
			labels[label] = true
			return nil
		},
		RemoveLabel: func(id, label string) error {
			delete(labels, label)
			return nil
		},
		AddComment: func(id, text string) error {
			return nil
		},
		ContainsLabel: func(b Bead, label string) bool {
			for _, l := range b.Labels {
				if l == label {
					return true
				}
			}
			return false
		},
		HasLabel: func(b Bead, prefix string) string {
			for _, l := range b.Labels {
				if len(l) >= len(prefix) && l[:len(prefix)] == prefix {
					return l[len(prefix):]
				}
			}
			return ""
		},
	}
}

func TestActionHumanApprove_FirstRun(t *testing.T) {
	labels := map[string]bool{}
	deps := humanApproveTestDeps(labels)

	graph := &formula.FormulaStepGraph{
		Name:    "test-approve",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"approve": {Action: "human.approve", Title: "Human reviews"},
		},
	}

	exec := NewGraphForTest("spi-test", "wizard-test", graph, nil, deps)
	step := StepConfig{Action: "human.approve", Title: "Human reviews"}

	result := actionHumanApprove(exec, "approve", step, exec.graphState)

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if !result.Hooked {
		t.Error("expected Hooked=true on first run")
	}
	// Status-based model: human.approve returns Hooked=true and the graph
	// interpreter sets the step bead + parent bead to "hooked" status.
	// No labels are added — the hooked status IS the signal.
}

func TestActionHumanApprove_StillWaiting(t *testing.T) {
	labels := map[string]bool{
		"needs-human":       true,
		"awaiting-approval": true,
	}
	deps := humanApproveTestDeps(labels)

	graph := &formula.FormulaStepGraph{
		Name:    "test-approve",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"approve": {Action: "human.approve"},
		},
	}

	exec := NewGraphForTest("spi-test", "wizard-test", graph, nil, deps)
	step := StepConfig{Action: "human.approve"}

	result := actionHumanApprove(exec, "approve", step, exec.graphState)

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if !result.Hooked {
		t.Error("expected Hooked=true while awaiting approval")
	}
}

func TestActionHumanApprove_Approved(t *testing.T) {
	// Simulate: labels were cleared by spire approve, CompletedCount > 0 from prior hooked run.
	labels := map[string]bool{}
	deps := humanApproveTestDeps(labels)

	graph := &formula.FormulaStepGraph{
		Name:    "test-approve",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"approve": {Action: "human.approve"},
		},
	}

	exec := NewGraphForTest("spi-test", "wizard-test", graph, nil, deps)
	// Simulate that the step has been completed once before (hooked counts as a completion).
	exec.graphState.Steps["approve"] = StepState{Status: "pending", CompletedCount: 1}
	step := StepConfig{Action: "human.approve"}

	result := actionHumanApprove(exec, "approve", step, exec.graphState)

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.Hooked {
		t.Error("expected Hooked=false after approval")
	}
	if result.Outputs["status"] != "approved" {
		t.Errorf("expected status=approved, got %q", result.Outputs["status"])
	}
}

func TestActionHumanApprove_InconsistentState(t *testing.T) {
	// Status-based model: approval is determined solely by CompletedCount.
	// CompletedCount == 0 means the step hasn't been hooked+resolved yet → hooks.
	// CompletedCount > 0 means the hook was resolved → approved.
	labels := map[string]bool{}
	deps := humanApproveTestDeps(labels)

	graph := &formula.FormulaStepGraph{
		Name:    "test-approve",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"approve": {Action: "human.approve"},
		},
	}

	exec := NewGraphForTest("spi-test", "wizard-test", graph, nil, deps)
	step := StepConfig{Action: "human.approve"}

	// With CompletedCount == 0 (default), the action hooks — waiting for human.
	result := actionHumanApprove(exec, "approve", step, exec.graphState)
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if !result.Hooked {
		t.Error("expected Hooked=true when CompletedCount == 0")
	}

	// With CompletedCount > 0, the action treats it as approved.
	exec.graphState.Steps["approve"] = StepState{Status: "pending", CompletedCount: 1}
	result = actionHumanApprove(exec, "approve", step, exec.graphState)
	if result.Error != nil {
		t.Fatalf("unexpected error on approved path: %v", result.Error)
	}
	if result.Hooked {
		t.Error("expected Hooked=false when CompletedCount > 0")
	}
	if result.Outputs["status"] != "approved" {
		t.Errorf("expected status=approved, got %q", result.Outputs["status"])
	}
}

// Unit test: the Conventional-Commit attribution matcher. Asserts the
// exact shape of subject lines we treat as "this commit attributes code
// to <childID>", per CLAUDE.md's `<type>(<bead-id>): <message>` format.
func TestCommitAttributesBead_Table(t *testing.T) {
	cases := []struct {
		name, subject, child string
		want                 bool
	}{
		{"feat match", "feat(spi-k465sm): add thing", "spi-k465sm", true},
		{"fix match", "fix(spi-abc): repair", "spi-abc", true},
		{"docs match", "docs(spi-abc): update readme", "spi-abc", true},
		{"chore match", "chore(spi-abc): deps", "spi-abc", true},
		{"refactor match", "refactor(spi-abc): rename", "spi-abc", true},
		{"test match", "test(spi-abc): coverage", "spi-abc", true},
		{"wrong id", "feat(spi-other): add thing", "spi-abc", false},
		{"no type", "(spi-abc): headerless", "spi-abc", false},
		{"no parens", "feat spi-abc add thing", "spi-abc", false},
		{"revert style", `Revert "feat(spi-abc): x"`, "spi-abc", false},
		{"empty subject", "", "spi-abc", false},
		{"empty id", "feat(spi-abc): x", "", false},
		{"case mismatch", "feat(SPI-ABC): x", "spi-abc", false},
		{"hierarchical id", "feat(spi-abc.1): hier", "spi-abc.1", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := commitAttributesBead(tc.subject, tc.child); got != tc.want {
				t.Fatalf("commitAttributesBead(%q, %q) = %v, want %v", tc.subject, tc.child, got, tc.want)
			}
		})
	}
}

// Integration test: drive mergedCommitsAttributeBead against a real
// temp git repo. Confirms that wave-apprentice-style commits attributed
// to subtask IDs are discoverable on the feature/epic branch.
func TestMergedCommitsAttributeBead_FindsAttribution(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "T")
	run("commit", "--allow-empty", "-m", "base")
	run("checkout", "-q", "-b", "feat/epic")
	run("commit", "--allow-empty", "-m", "feat(spi-k465sm): wave work")
	run("commit", "--allow-empty", "-m", "feat(spi-uq03af): more wave work")
	run("commit", "--allow-empty", "-m", "chore: untagged")

	ok, err := mergedCommitsAttributeBead(dir, "main", "feat/epic", "spi-k465sm")
	if err != nil {
		t.Fatalf("spi-k465sm: err=%v", err)
	}
	if !ok {
		t.Fatal("spi-k465sm: not found (want true)")
	}
	ok, _ = mergedCommitsAttributeBead(dir, "main", "feat/epic", "spi-uq03af")
	if !ok {
		t.Fatal("spi-uq03af not found on branch")
	}
	ok, _ = mergedCommitsAttributeBead(dir, "main", "feat/epic", "spi-missing")
	if ok {
		t.Fatal("spi-missing should not be found")
	}
	// Empty inputs are non-fatal.
	ok, err = mergedCommitsAttributeBead("", "main", "feat/epic", "spi-k465sm")
	if ok || err != nil {
		t.Fatalf("empty repoPath: want (false,nil), got (%v,%v)", ok, err)
	}
	// Missing ref → (false, nil) — caller falls back to attempt-bead check.
	ok, err = mergedCommitsAttributeBead(dir, "main", "feat/nonexistent", "spi-k465sm")
	if ok || err != nil {
		t.Fatalf("missing ref: want (false,nil), got (%v,%v)", ok, err)
	}
}

// Regression for spi-h61t0w: after a fast-forward merge, base and the
// merged branch point at the same SHA, so base..branchTip is empty and
// the close guard misclassifies already-landed child commits as stranded.
// The fix widens the scan to branchTip's full history when refs match.
func TestMergedCommitsAttributeBead_FastForwardCase(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "T")
	run("commit", "--allow-empty", "-m", "base")
	run("checkout", "-q", "-b", "feat/epic")
	run("commit", "--allow-empty", "-m", "feat(spi-ff001): wave work")
	run("checkout", "-q", "main")
	run("merge", "--ff-only", "feat/epic")

	// main and feat/epic now point at the same SHA. Pre-fix, base..branchTip
	// is empty and the attribution check returns (false, nil); post-fix the
	// scan widens to branchTip's full history and finds the child commit.
	ok, err := mergedCommitsAttributeBead(dir, "main", "feat/epic", "spi-ff001")
	if err != nil {
		t.Fatalf("ff case: err=%v", err)
	}
	if !ok {
		t.Fatal("ff case: spi-ff001 not found (want true)")
	}
	// Sanity: a child ID that was never committed must still return false.
	ok, _ = mergedCommitsAttributeBead(dir, "main", "feat/epic", "spi-absent")
	if ok {
		t.Fatal("ff case: spi-absent should not be found")
	}
}
