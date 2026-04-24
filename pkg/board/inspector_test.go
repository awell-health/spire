package board

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
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

// --- buildLogCycleMap tests ---

// TestBuildLogCycleMap covers the bead-to-log-cycle mapping used by the
// inspector to group historical attempts under reset cycle headers. The
// fix-<N> suffix must pair with the review-round bead's cycle (not the
// attempt bead's cycle) because fix inherits the round's monotonic N.
func TestBuildLogCycleMap(t *testing.T) {
	// reviewBead builds a minimal review-round child bead with the two
	// labels buildLogCycleMap reads (round:N and reset-cycle:C).
	reviewBead := func(round, cycle int) Bead {
		labels := []string{"review-round"}
		if round > 0 {
			labels = append(labels, "round:"+strconv.Itoa(round))
		}
		if cycle > 0 {
			labels = append(labels, "reset-cycle:"+strconv.Itoa(cycle))
		}
		return Bead{ID: "rev", Title: "review-round-" + strconv.Itoa(round), Labels: labels}
	}
	// attemptBead builds a minimal attempt child bead.
	attemptBead := func(attempt, cycle int) Bead {
		labels := []string{"attempt"}
		if attempt > 0 {
			labels = append(labels, "attempt:"+strconv.Itoa(attempt))
		}
		if cycle > 0 {
			labels = append(labels, "reset-cycle:"+strconv.Itoa(cycle))
		}
		return Bead{ID: "att", Title: "attempt: wizard", Labels: labels}
	}

	tests := []struct {
		name     string
		children []Bead
		want     map[string]int
	}{
		{
			name:     "nil children yields nil",
			children: nil,
			want:     nil,
		},
		{
			name:     "empty children yields nil",
			children: []Bead{},
			want:     nil,
		},
		{
			name: "no attempt or review beads yields nil",
			children: []Bead{
				{ID: "step", Labels: []string{"workflow-step", "step:plan"}},
			},
			want: nil,
		},
		{
			name: "review beads map sage-review-N and fix-N",
			children: []Bead{
				reviewBead(1, 1),
				reviewBead(2, 1),
			},
			want: map[string]int{
				"sage-review-1": 1,
				"sage-review-2": 1,
				"fix-1":         1,
				"fix-2":         1,
			},
		},
		{
			name: "attempt beads map implement-N",
			children: []Bead{
				attemptBead(1, 1),
				attemptBead(2, 2),
			},
			want: map[string]int{
				"implement-1": 1,
				"implement-2": 2,
			},
		},
		{
			name: "mixed attempts and reviews across two reset cycles",
			children: []Bead{
				// Cycle 1: one attempt, three review rounds.
				attemptBead(1, 1),
				reviewBead(1, 1),
				reviewBead(2, 1),
				reviewBead(3, 1),
				// Cycle 2: another attempt, one round so far.
				attemptBead(2, 2),
				reviewBead(4, 2),
			},
			want: map[string]int{
				"implement-1":   1,
				"implement-2":   2,
				"sage-review-1": 1,
				"sage-review-2": 1,
				"sage-review-3": 1,
				"sage-review-4": 2,
				"fix-1":         1,
				"fix-2":         1,
				"fix-3":         1,
				"fix-4":         2,
			},
		},
		{
			name: "fix-N pairs with round:N not attempt:N",
			children: []Bead{
				// Cycle 1 has three rounds; cycle 2 has attempt:2.
				// fix-1/2/3 must land in cycle 1 (paired with rounds),
				// not in cycle 2 (which would happen if fix paired
				// with attempt:N).
				reviewBead(1, 1),
				reviewBead(2, 1),
				reviewBead(3, 1),
				attemptBead(2, 2),
			},
			want: map[string]int{
				"sage-review-1": 1,
				"sage-review-2": 1,
				"sage-review-3": 1,
				"fix-1":         1,
				"fix-2":         1,
				"fix-3":         1,
				"implement-2":   2,
			},
		},
		{
			name: "review bead with missing round label is skipped",
			children: []Bead{
				{ID: "rev-malformed", Title: "review-round-", Labels: []string{"review-round", "reset-cycle:1"}},
				reviewBead(2, 1),
			},
			want: map[string]int{
				"sage-review-2": 1,
				"fix-2":         1,
			},
		},
		{
			name: "attempt bead with missing attempt label is skipped",
			children: []Bead{
				{ID: "att-malformed", Title: "attempt: wizard", Labels: []string{"attempt", "reset-cycle:1"}},
				attemptBead(2, 2),
			},
			want: map[string]int{
				"implement-2": 2,
			},
		},
		{
			name: "missing reset-cycle label defaults to cycle 1",
			children: []Bead{
				// No reset-cycle label → ResetCycleNumber returns 1.
				reviewBead(1, 0),
				attemptBead(1, 0),
			},
			want: map[string]int{
				"sage-review-1": 1,
				"fix-1":         1,
				"implement-1":   1,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildLogCycleMap(tt.children)
			if len(got) != len(tt.want) {
				t.Errorf("buildLogCycleMap() returned %d entries, want %d\n got: %v\nwant: %v",
					len(got), len(tt.want), got, tt.want)
				return
			}
			for k, wantCycle := range tt.want {
				gotCycle, ok := got[k]
				if !ok {
					t.Errorf("buildLogCycleMap() missing key %q", k)
					continue
				}
				if gotCycle != wantCycle {
					t.Errorf("buildLogCycleMap()[%q] = %d, want %d", k, gotCycle, wantCycle)
				}
			}
			// Catch stray keys that shouldn't be present.
			for k := range got {
				if _, ok := tt.want[k]; !ok {
					t.Errorf("buildLogCycleMap() has unexpected key %q = %d", k, got[k])
				}
			}
		})
	}
}
