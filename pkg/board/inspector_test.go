package board

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/awell-health/spire/pkg/executor"
)

// --- hookedWaitingFor tests (pure function) ---

func TestHookedWaitingFor_DesignRef(t *testing.T) {
	got := hookedWaitingFor("design-check", map[string]string{"design_ref": "spi-abc"})
	want := "design bead spi-abc"
	if got != want {
		t.Errorf("hookedWaitingFor() = %q, want %q", got, want)
	}
}

func TestHookedWaitingFor_ReviewStepName(t *testing.T) {
	got := hookedWaitingFor("review", nil)
	if got != "human approval" {
		t.Errorf("hookedWaitingFor(review, nil) = %q, want %q", got, "human approval")
	}
}

func TestHookedWaitingFor_ReviewSubstring(t *testing.T) {
	got := hookedWaitingFor("code-review", nil)
	if got != "human approval" {
		t.Errorf("hookedWaitingFor(code-review, nil) = %q, want %q", got, "human approval")
	}
}

func TestHookedWaitingFor_HookReason(t *testing.T) {
	got := hookedWaitingFor("implement", map[string]string{"hook_reason": "waiting for CI"})
	want := "waiting for CI"
	if got != want {
		t.Errorf("hookedWaitingFor() = %q, want %q", got, want)
	}
}

func TestHookedWaitingFor_RawOutputsFallback(t *testing.T) {
	got := hookedWaitingFor("implement", map[string]string{"foo": "bar", "baz": "qux"})
	want := "baz=qux, foo=bar" // sorted
	if got != want {
		t.Errorf("hookedWaitingFor() = %q, want %q", got, want)
	}
}

func TestHookedWaitingFor_NoOutputsFallback(t *testing.T) {
	got := hookedWaitingFor("implement", nil)
	if got != "external condition" {
		t.Errorf("hookedWaitingFor() = %q, want %q", got, "external condition")
	}
}

func TestHookedWaitingFor_EmptyOutputsFallback(t *testing.T) {
	got := hookedWaitingFor("implement", map[string]string{})
	if got != "external condition" {
		t.Errorf("hookedWaitingFor() = %q, want %q", got, "external condition")
	}
}

func TestHookedWaitingFor_DesignRefTakesPrecedenceOverReview(t *testing.T) {
	// design_ref should take precedence even if step name contains "review"
	got := hookedWaitingFor("review", map[string]string{"design_ref": "spi-xyz"})
	want := "design bead spi-xyz"
	if got != want {
		t.Errorf("hookedWaitingFor() = %q, want %q", got, want)
	}
}

// --- findHookedStepInfo tests ---

func TestFindHookedStepInfo_NilDAG(t *testing.T) {
	info := findHookedStepInfo("spi-test", nil)
	if info != nil {
		t.Errorf("expected nil for nil DAG, got %+v", info)
	}
}

func TestFindHookedStepInfo_NoHookedStep(t *testing.T) {
	dag := &DAGProgress{
		Steps: []DAGStep{
			{Name: "design-check", Status: "closed"},
			{Name: "plan", Status: "closed"},
			{Name: "implement", Status: "in_progress"},
			{Name: "review", Status: "open"},
		},
	}
	info := findHookedStepInfo("spi-test", dag)
	if info != nil {
		t.Errorf("expected nil when no hooked step, got %+v", info)
	}
}

func TestFindHookedStepInfo_HookedStepNoGraphState(t *testing.T) {
	// Set SPIRE_CONFIG_DIR to a temp dir with no graph state files.
	tmpDir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmpDir)

	dag := &DAGProgress{
		Steps: []DAGStep{
			{Name: "design-check", Status: "closed"},
			{Name: "implement", Status: "hooked"},
			{Name: "review", Status: "open"},
		},
	}
	info := findHookedStepInfo("spi-test", dag)
	if info == nil {
		t.Fatal("expected non-nil HookedStepInfo")
	}
	if info.StepName != "implement" {
		t.Errorf("StepName = %q, want %q", info.StepName, "implement")
	}
	// No graph state → falls back to hookedWaitingFor with nil outputs.
	if info.WaitingFor != "external condition" {
		t.Errorf("WaitingFor = %q, want %q", info.WaitingFor, "external condition")
	}
}

func TestFindHookedStepInfo_HookedReviewStep(t *testing.T) {
	// Set SPIRE_CONFIG_DIR to a temp dir with no graph state files.
	tmpDir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmpDir)

	dag := &DAGProgress{
		Steps: []DAGStep{
			{Name: "implement", Status: "closed"},
			{Name: "review", Status: "hooked"},
		},
	}
	info := findHookedStepInfo("spi-test", dag)
	if info == nil {
		t.Fatal("expected non-nil HookedStepInfo")
	}
	if info.StepName != "review" {
		t.Errorf("StepName = %q, want %q", info.StepName, "review")
	}
	if info.WaitingFor != "human approval" {
		t.Errorf("WaitingFor = %q, want %q", info.WaitingFor, "human approval")
	}
}

func TestFindHookedStepInfo_WithGraphState_DesignRef(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmpDir)

	// Write a graph state file for wizard-spi-test.
	gs := executor.GraphState{
		BeadID:    "spi-test",
		AgentName: "wizard-spi-test",
		Steps: map[string]executor.StepState{
			"design-check": {
				Status:  "hooked",
				Outputs: map[string]string{"design_ref": "spi-design1"},
			},
		},
	}
	writeGraphState(t, tmpDir, "wizard-spi-test", gs)

	dag := &DAGProgress{
		Steps: []DAGStep{
			{Name: "design-check", Status: "hooked"},
			{Name: "plan", Status: "open"},
		},
	}
	info := findHookedStepInfo("spi-test", dag)
	if info == nil {
		t.Fatal("expected non-nil HookedStepInfo")
	}
	if info.StepName != "design-check" {
		t.Errorf("StepName = %q, want %q", info.StepName, "design-check")
	}
	if info.WaitingFor != "design bead spi-design1" {
		t.Errorf("WaitingFor = %q, want %q", info.WaitingFor, "design bead spi-design1")
	}
}

func TestFindHookedStepInfo_WithGraphState_HookReason(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmpDir)

	gs := executor.GraphState{
		BeadID:    "spi-test",
		AgentName: "wizard-spi-test",
		Steps: map[string]executor.StepState{
			"implement": {
				Status:  "hooked",
				Outputs: map[string]string{"hook_reason": "needs external API key"},
			},
		},
	}
	writeGraphState(t, tmpDir, "wizard-spi-test", gs)

	dag := &DAGProgress{
		Steps: []DAGStep{
			{Name: "implement", Status: "hooked"},
		},
	}
	info := findHookedStepInfo("spi-test", dag)
	if info == nil {
		t.Fatal("expected non-nil HookedStepInfo")
	}
	if info.WaitingFor != "needs external API key" {
		t.Errorf("WaitingFor = %q, want %q", info.WaitingFor, "needs external API key")
	}
}

func TestFindHookedStepInfo_WithGraphState_RawOutputs(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmpDir)

	gs := executor.GraphState{
		BeadID:    "spi-test",
		AgentName: "wizard-spi-test",
		Steps: map[string]executor.StepState{
			"implement": {
				Status:  "hooked",
				Outputs: map[string]string{"alpha": "1", "beta": "2"},
			},
		},
	}
	writeGraphState(t, tmpDir, "wizard-spi-test", gs)

	dag := &DAGProgress{
		Steps: []DAGStep{
			{Name: "implement", Status: "hooked"},
		},
	}
	info := findHookedStepInfo("spi-test", dag)
	if info == nil {
		t.Fatal("expected non-nil HookedStepInfo")
	}
	if info.WaitingFor != "alpha=1, beta=2" {
		t.Errorf("WaitingFor = %q, want %q", info.WaitingFor, "alpha=1, beta=2")
	}
}

// writeGraphState writes a graph state JSON file to the expected location.
func writeGraphState(t *testing.T, configDir, agentName string, gs executor.GraphState) {
	t.Helper()
	dir := filepath.Join(configDir, "runtime", agentName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	data, err := json.Marshal(gs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "graph_state.json"), data, 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
}
