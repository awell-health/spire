package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/executor"
	"github.com/steveyegge/beads"
)

// stubResolveDeps replaces test-replaceable vars used by resolve.go.
// Returns a cleanup func that restores originals.
func stubResolveDeps(t *testing.T) func() {
	t.Helper()
	origGetBead := resolveGetBeadFunc
	origGetDependents := resolveGetDependentsFunc
	origCloseBead := resolveCloseBeadFunc

	return func() {
		resolveGetBeadFunc = origGetBead
		resolveGetDependentsFunc = origGetDependents
		resolveCloseBeadFunc = origCloseBead
	}
}

// TestResolveSourceBead_NeedsHumanValidation verifies that resolveSourceBead
// rejects beads without the needs-human label.
func TestResolveSourceBead_NeedsHumanValidation(t *testing.T) {
	cleanup := stubResolveDeps(t)
	defer cleanup()

	resolveGetBeadFunc = func(id string) (Bead, error) {
		return Bead{ID: id, Status: "in_progress", Labels: []string{"some-label"}}, nil
	}
	resolveGetDependentsFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return nil, nil
	}

	err := resolveSourceBead("spi-test", "fixed it", false)
	if err == nil {
		t.Fatal("expected error for bead without needs-human label")
	}
	if !strings.Contains(err.Error(), "needs-human") {
		t.Errorf("expected 'needs-human' in error, got: %v", err)
	}
}

// TestResolveSourceBead_AcceptsNeedsHuman verifies that resolveSourceBead
// accepts a bead with the needs-human label and processes it.
func TestResolveSourceBead_AcceptsNeedsHuman(t *testing.T) {
	cleanup := stubResolveDeps(t)
	defer cleanup()

	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	resolveGetBeadFunc = func(id string) (Bead, error) {
		return Bead{
			ID:     id,
			Status: "in_progress",
			Labels: []string{"needs-human", "interrupted:step-failure"},
		}, nil
	}
	resolveGetDependentsFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return nil, nil
	}

	var closedBeads []string
	resolveCloseBeadFunc = func(id string) error {
		closedBeads = append(closedBeads, id)
		return nil
	}

	err := resolveSourceBead("spi-test", "fixed it", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No recovery beads, so nothing should be closed.
	if len(closedBeads) != 0 {
		t.Errorf("expected no beads closed, got: %v", closedBeads)
	}
}

// TestResolveSourceBead_RecoveryBeadDiscoveryAndClosing verifies that resolve
// finds open recovery beads with caused-by dep type and recovery-bead label,
// closes them, and skips already-closed ones.
func TestResolveSourceBead_RecoveryBeadDiscoveryAndClosing(t *testing.T) {
	cleanup := stubResolveDeps(t)
	defer cleanup()

	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	resolveGetBeadFunc = func(id string) (Bead, error) {
		switch id {
		case "spi-source":
			return Bead{
				ID:     "spi-source",
				Status: "in_progress",
				Labels: []string{"needs-human"},
			}, nil
		case "spi-recovery-open":
			return Bead{
				ID:     "spi-recovery-open",
				Status: "open",
				Labels: []string{"recovery-bead"},
				Metadata: map[string]string{
					"failure_class":    "build-error",
					"failure_signature": "exit code 1",
					"expected_outcome": "clean build",
					"source_bead":      "spi-source",
				},
			}, nil
		default:
			return Bead{}, nil
		}
	}

	resolveGetDependentsFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return []*beads.IssueWithDependencyMetadata{
			{
				Issue: beads.Issue{
					ID:     "spi-recovery-open",
					Status: "open",
					Labels: []string{"recovery-bead"},
				},
				DependencyType: "caused-by",
			},
			{
				// Already closed — should be skipped.
				Issue: beads.Issue{
					ID:     "spi-recovery-closed",
					Status: beads.StatusClosed,
					Labels: []string{"recovery-bead"},
				},
				DependencyType: "caused-by",
			},
			{
				// Different dep type — should be skipped.
				Issue: beads.Issue{
					ID:     "spi-child",
					Status: "open",
					Labels: []string{},
				},
				DependencyType: "parent-child",
			},
			{
				// No recovery-bead label — should be skipped.
				Issue: beads.Issue{
					ID:     "spi-other",
					Status: "open",
					Labels: []string{},
				},
				DependencyType: "caused-by",
			},
		}, nil
	}

	var closedBeads []string
	resolveCloseBeadFunc = func(id string) error {
		closedBeads = append(closedBeads, id)
		return nil
	}

	err := resolveSourceBead("spi-source", "fixed the build", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Only the open recovery bead should be closed.
	if len(closedBeads) != 1 {
		t.Fatalf("expected 1 bead closed, got %d: %v", len(closedBeads), closedBeads)
	}
	if closedBeads[0] != "spi-recovery-open" {
		t.Errorf("expected closed bead spi-recovery-open, got %s", closedBeads[0])
	}
}

// TestResolveSourceBead_CloseFlag verifies that --close closes the source bead.
func TestResolveSourceBead_CloseFlag(t *testing.T) {
	cleanup := stubResolveDeps(t)
	defer cleanup()

	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	resolveGetBeadFunc = func(id string) (Bead, error) {
		return Bead{
			ID:     id,
			Status: "in_progress",
			Labels: []string{"needs-human"},
		}, nil
	}
	resolveGetDependentsFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return nil, nil
	}

	var closedBeads []string
	resolveCloseBeadFunc = func(id string) error {
		closedBeads = append(closedBeads, id)
		return nil
	}

	err := resolveSourceBead("spi-test", "no longer needed", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Source bead should be closed when --close is set.
	found := false
	for _, id := range closedBeads {
		if id == "spi-test" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected source bead spi-test to be closed, closed beads: %v", closedBeads)
	}
}

// TestResolveSourceBead_ReSummon verifies that without --close, hooked steps
// are reset to pending in the graph state file.
func TestResolveSourceBead_ReSummon(t *testing.T) {
	cleanup := stubResolveDeps(t)
	defer cleanup()

	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	resolveGetBeadFunc = func(id string) (Bead, error) {
		return Bead{
			ID:     id,
			Status: "in_progress",
			Labels: []string{"needs-human"},
		}, nil
	}
	resolveGetDependentsFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return nil, nil
	}
	resolveCloseBeadFunc = func(id string) error { return nil }

	// Create a graph state file with a hooked step.
	agentName := "wizard-spi-test"
	runtimeDir := filepath.Join(tmp, "runtime", agentName)
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		t.Fatal(err)
	}

	gs := executor.GraphState{
		BeadID:    "spi-test",
		AgentName: agentName,
		Steps: map[string]executor.StepState{
			"implement": {Status: "completed"},
			"review":    {Status: "hooked", StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:01:00Z"},
			"merge":     {Status: "pending"},
		},
	}
	data, err := json.MarshalIndent(gs, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "graph_state.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	err = resolveSourceBead("spi-test", "fixed the issue", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Re-read graph state and verify hooked step was reset.
	data, err = os.ReadFile(filepath.Join(runtimeDir, "graph_state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var updated executor.GraphState
	if err := json.Unmarshal(data, &updated); err != nil {
		t.Fatal(err)
	}

	reviewStep := updated.Steps["review"]
	if reviewStep.Status != "pending" {
		t.Errorf("expected review step reset to pending, got %s", reviewStep.Status)
	}
	if reviewStep.StartedAt != "" {
		t.Errorf("expected StartedAt cleared, got %s", reviewStep.StartedAt)
	}
	if reviewStep.CompletedAt != "" {
		t.Errorf("expected CompletedAt cleared, got %s", reviewStep.CompletedAt)
	}
	// Other steps should be unchanged.
	if updated.Steps["implement"].Status != "completed" {
		t.Errorf("expected implement step unchanged, got %s", updated.Steps["implement"].Status)
	}
	if updated.Steps["merge"].Status != "pending" {
		t.Errorf("expected merge step unchanged, got %s", updated.Steps["merge"].Status)
	}
}

// TestResolveCleanWorktrees verifies that worktree cleanup removes matching directories.
func TestResolveCleanWorktrees(t *testing.T) {
	tmp := t.TempDir()

	// Create fake in-repo worktree dir.
	oldWd, _ := os.Getwd()
	os.Chdir(tmp)
	defer os.Chdir(oldWd)

	inRepoDir := filepath.Join(tmp, ".worktrees", "spi-test")
	if err := os.MkdirAll(inRepoDir, 0755); err != nil {
		t.Fatal(err)
	}
	subtaskDir := filepath.Join(tmp, ".worktrees", "spi-test-staging")
	if err := os.MkdirAll(subtaskDir, 0755); err != nil {
		t.Fatal(err)
	}

	resolveCleanWorktrees("spi-test")

	// Both should be removed.
	if _, err := os.Stat(inRepoDir); !os.IsNotExist(err) {
		t.Error("expected in-repo worktree to be removed")
	}
	if _, err := os.Stat(subtaskDir); !os.IsNotExist(err) {
		t.Error("expected subtask worktree to be removed")
	}
}

// TestResolveResetHookedSteps_NoGraphState verifies graceful handling when
// no graph state exists.
func TestResolveResetHookedSteps_NoGraphState(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	// Should not panic or error — just print a warning.
	resolveResetHookedSteps("spi-nonexistent")
}

// TestResolveResetHookedSteps_MultipleHooked verifies that all hooked steps
// are reset, not just the first one.
func TestResolveResetHookedSteps_MultipleHooked(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	agentName := "wizard-spi-multi"
	runtimeDir := filepath.Join(tmp, "runtime", agentName)
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		t.Fatal(err)
	}

	gs := executor.GraphState{
		BeadID:    "spi-multi",
		AgentName: agentName,
		Steps: map[string]executor.StepState{
			"step-a": {Status: "hooked", StartedAt: "t1", CompletedAt: "t2", Outputs: map[string]string{"x": "y"}},
			"step-b": {Status: "hooked", StartedAt: "t3"},
			"step-c": {Status: "completed"},
		},
	}
	data, _ := json.MarshalIndent(gs, "", "  ")
	os.WriteFile(filepath.Join(runtimeDir, "graph_state.json"), data, 0644)

	resolveResetHookedSteps("spi-multi")

	data, _ = os.ReadFile(filepath.Join(runtimeDir, "graph_state.json"))
	var updated executor.GraphState
	json.Unmarshal(data, &updated)

	for _, name := range []string{"step-a", "step-b"} {
		s := updated.Steps[name]
		if s.Status != "pending" {
			t.Errorf("step %s: expected pending, got %s", name, s.Status)
		}
		if s.StartedAt != "" {
			t.Errorf("step %s: expected StartedAt cleared", name)
		}
		if s.CompletedAt != "" {
			t.Errorf("step %s: expected CompletedAt cleared", name)
		}
		if len(s.Outputs) != 0 {
			t.Errorf("step %s: expected Outputs cleared, got %v", name, s.Outputs)
		}
	}
	if updated.Steps["step-c"].Status != "completed" {
		t.Error("step-c should remain completed")
	}
}

// Ensure config.Dir is used (avoid unused import error in case of direct use).
var _ = config.Dir
