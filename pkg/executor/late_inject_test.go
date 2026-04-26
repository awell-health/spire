package executor

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/formula"
	"github.com/steveyegge/beads"
)

// TestDispatchWaveCore_LateInjectTriggersRescan reproduces the spi-g4yi6j
// protocol gap: a child injected into the epic after wave-1 dispatches must
// be picked up by the re-scan loop and dispatched as a new wave.
//
// Before Fix A this test failed: the new child was silently dropped because
// nothing re-queried the children list after the pre-computed waves finished.
// The bug surfaced in the wild on epic spi-qxbont (see bead description).
func TestDispatchWaveCore_LateInjectTriggersRescan(t *testing.T) {
	const epicID = "spi-epic"

	backend := &concurrentBackend{sleepPerJob: 5 * time.Millisecond}

	// Initial children (wave-1). The 4th child is the "late-inject" that
	// gets added to the children list between wave-1 dispatch and close.
	initial := []Bead{
		{ID: epicID + ".1", Status: "in_progress"},
		{ID: epicID + ".2", Status: "in_progress"},
		{ID: epicID + ".3", Status: "in_progress"},
	}
	injected := Bead{ID: epicID + ".4", Status: "open"}

	// GetChildren is called by ComputeWaves during the re-scan loop. Both
	// calls return the same 4-child set (initial + injected). Fix A's
	// dispatched-set tracking filters the already-dispatched beads; only
	// the truly new one gets dispatched. The second call finds no new
	// candidates and exits the loop.
	var childrenMu sync.Mutex
	var getChildrenCalls int32
	getChildrenFn := func(parentID string) ([]Bead, error) {
		childrenMu.Lock()
		defer childrenMu.Unlock()
		atomic.AddInt32(&getChildrenCalls, 1)
		return append(append([]Bead{}, initial...), injected), nil
	}

	deps := &Deps{
		Spawner:           backend,
		MaxApprentices:    3,
		UpdateBead:        func(id string, updates map[string]interface{}) error { return nil },
		ResolveBranch:     func(beadID string) string { return "feat/" + beadID },
		GetChildren:       getChildrenFn,
		GetBlockedIssues:  func(filter beads.WorkFilter) ([]BoardBead, error) { return nil, nil },
		IsAttemptBead:     func(b Bead) bool { return false },
		IsStepBead:        func(b Bead) bool { return false },
		IsReviewRoundBead: func(b Bead) bool { return false },
	}

	e := NewForTest(epicID, "wizard-test", nil, deps)

	// Pass wave-1 explicitly. The re-scan loop is expected to pick up the
	// late-injected child and dispatch it as wave-2.
	wave1 := []string{initial[0].ID, initial[1].ID, initial[2].ID}
	resolver := func(string, string) error { return nil }

	results, err := e.dispatchWaveCore([][]string{wave1}, nil, "claude-sonnet-4-6", resolver, 3)
	if err != nil {
		t.Fatalf("dispatchWaveCore: %v", err)
	}

	// Expected: 3 (initial wave) + 1 (re-scan wave) = 4 apprentices spawned.
	if got := atomic.LoadInt32(&backend.spawnCount); got != 4 {
		t.Errorf("spawnCount = %d, want 4 (3 initial + 1 late-inject)", got)
	}
	if len(results) != 4 {
		t.Errorf("results count = %d, want 4", len(results))
	}

	// Verify the injected child was dispatched.
	found := false
	for _, r := range results {
		if r.BeadID == injected.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("late-injected child %s was not dispatched — re-scan loop failed", injected.ID)
	}

	// Re-scan loop should have exited after the second ComputeWaves call
	// returned no new candidates. At least two GetChildren calls are
	// expected (one to discover the injected child, one to confirm the set
	// is stable).
	if got := atomic.LoadInt32(&getChildrenCalls); got < 2 {
		t.Errorf("GetChildren calls = %d, want >= 2 (re-scan loop must converge)", got)
	}
}

// TestDispatchWaveCore_NoInjectIsUnchanged verifies Fix A's re-scan adds
// exactly one empty-result GetChildren call when no late-inject happens, and
// does not spawn any extra apprentices — regression guard for the common path.
func TestDispatchWaveCore_NoInjectIsUnchanged(t *testing.T) {
	const epicID = "spi-epic"

	backend := &concurrentBackend{sleepPerJob: 5 * time.Millisecond}

	// No injection: children is always exactly the initial set.
	children := []Bead{
		{ID: epicID + ".1", Status: "in_progress"},
		{ID: epicID + ".2", Status: "in_progress"},
	}

	var getChildrenCalls int32
	getChildrenFn := func(parentID string) ([]Bead, error) {
		atomic.AddInt32(&getChildrenCalls, 1)
		return append([]Bead{}, children...), nil
	}

	deps := &Deps{
		Spawner:           backend,
		MaxApprentices:    3,
		UpdateBead:        func(id string, updates map[string]interface{}) error { return nil },
		ResolveBranch:     func(beadID string) string { return "feat/" + beadID },
		GetChildren:       getChildrenFn,
		GetBlockedIssues:  func(filter beads.WorkFilter) ([]BoardBead, error) { return nil, nil },
		IsAttemptBead:     func(b Bead) bool { return false },
		IsStepBead:        func(b Bead) bool { return false },
		IsReviewRoundBead: func(b Bead) bool { return false },
	}

	e := NewForTest(epicID, "wizard-test", nil, deps)

	wave1 := []string{children[0].ID, children[1].ID}
	resolver := func(string, string) error { return nil }

	results, err := e.dispatchWaveCore([][]string{wave1}, nil, "claude-sonnet-4-6", resolver, 3)
	if err != nil {
		t.Fatalf("dispatchWaveCore: %v", err)
	}

	if got := atomic.LoadInt32(&backend.spawnCount); got != 2 {
		t.Errorf("spawnCount = %d, want 2 (no-inject path must be unchanged)", got)
	}
	if len(results) != 2 {
		t.Errorf("results count = %d, want 2", len(results))
	}
	// Re-scan fires at least once to confirm no new children. One call is
	// enough because the filtered batch comes up empty immediately.
	if got := atomic.LoadInt32(&getChildrenCalls); got < 1 {
		t.Errorf("GetChildren calls = %d, want >= 1 (re-scan must run at least once)", got)
	}
}

// TestActionBeadFinish_RefusesCloseWithStrandedChild verifies Fix B: if an
// epic has a non-closed child with no successful attempt, bead.finish must
// refuse to close, label the epic needs-human, and message the archmage.
func TestActionBeadFinish_RefusesCloseWithStrandedChild(t *testing.T) {
	dir := t.TempDir()

	var closeBeadCalls []string
	var addedLabels []string
	var createdMessages []CreateOpts
	var archmageMessage string

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetChildren: func(parentID string) ([]Bead, error) {
			switch parentID {
			case "spi-epic":
				return []Bead{
					// child-1: already closed — the guard treats this as
					// closed intentionally (e.g. duplicate/wontfix) and
					// allows the cascade through.
					{ID: "spi-epic.1", Status: "closed"},
					// child-2: open with no attempt — stranded.
					{ID: "spi-epic.2", Status: "open"},
				}, nil
			case "spi-epic.2":
				// No attempt bead — this child was never dispatched.
				return nil, nil
			}
			return nil, nil
		},
		CloseBead: func(id string) error {
			closeBeadCalls = append(closeBeadCalls, id)
			return nil
		},
		AddLabel: func(id, label string) error {
			addedLabels = append(addedLabels, id+":"+label)
			return nil
		},
		CreateBead: func(opts CreateOpts) (string, error) {
			createdMessages = append(createdMessages, opts)
			// Capture the archmage-message title specifically.
			for _, l := range opts.Labels {
				if l == "to:archmage" {
					archmageMessage = opts.Title
				}
			}
			return "spi-msg-1", nil
		},
		AddDepTyped:       func(issueID, dependsOnID, depType string) error { return nil },
		IsAttemptBead:     func(b Bead) bool { return false },
		IsStepBead:        func(b Bead) bool { return false },
		IsReviewRoundBead: func(b Bead) bool { return false },
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-epic-close",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"close": {Action: "bead.finish"},
		},
	}
	exec := NewGraphForTest("spi-epic", "wizard-epic", graph, nil, deps)

	step := StepConfig{
		Action: "bead.finish",
		With:   map[string]string{"status": "closed"},
	}

	result := actionBeadFinish(exec, "close", step, exec.graphState)

	// The guard must return an error and not close the epic.
	if result.Error == nil {
		t.Fatalf("actionBeadFinish should have returned an error for stranded child, got nil")
	}
	for _, id := range closeBeadCalls {
		if id == "spi-epic" {
			t.Errorf("CloseBead was called on the epic despite stranded child")
		}
	}

	// Must label the epic needs-human.
	foundNeedsHuman := false
	for _, l := range addedLabels {
		if l == "spi-epic:needs-human" {
			foundNeedsHuman = true
		}
	}
	if !foundNeedsHuman {
		t.Errorf("expected needs-human label on epic, got: %v", addedLabels)
	}

	// Must message archmage naming the stranded child.
	if archmageMessage == "" {
		t.Errorf("expected a message to archmage, got none; created messages: %v", createdMessages)
	} else if !strings.Contains(archmageMessage, "spi-epic.2") {
		t.Errorf("archmage message must name the stranded child; got: %q", archmageMessage)
	}
}

// TestActionBeadFinish_AllowsCloseWhenChildHasSuccessfulAttempt verifies Fix
// B's happy path: a child with a closed attempt labeled result:success does
// NOT trigger the guard, and the epic closes normally.
func TestActionBeadFinish_AllowsCloseWhenChildHasSuccessfulAttempt(t *testing.T) {
	dir := t.TempDir()

	var closedBeadIDs []string

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetChildren: func(parentID string) ([]Bead, error) {
			switch parentID {
			case "spi-epic":
				return []Bead{
					{ID: "spi-epic.1", Status: "in_progress"},
				}, nil
			case "spi-epic.1":
				// Child has a closed attempt bead with result:success — code
				// landed, epic is free to close.
				return []Bead{
					{
						ID:     "spi-epic.1.attempt-1",
						Status: "closed",
						Labels: []string{"attempt", "result:success"},
					},
				}, nil
			}
			return nil, nil
		},
		CloseBead: func(id string) error {
			closedBeadIDs = append(closedBeadIDs, id)
			return nil
		},
		AddLabel:         func(id, label string) error { return nil },
		CreateBead:       func(opts CreateOpts) (string, error) { return "spi-msg-1", nil },
		AddDepTyped:      func(issueID, dependsOnID, depType string) error { return nil },
		CloseAttemptBead: func(attemptID, result string) error { return nil },
		GetDependentsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			return nil, nil
		},
		AddComment: func(id, text string) error { return nil },
		IsAttemptBead: func(b Bead) bool {
			for _, l := range b.Labels {
				if l == "attempt" {
					return true
				}
			}
			return false
		},
		IsStepBead:        func(b Bead) bool { return false },
		IsReviewRoundBead: func(b Bead) bool { return false },
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-epic-close",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"close": {Action: "bead.finish"},
		},
	}
	exec := NewGraphForTest("spi-epic", "wizard-epic", graph, nil, deps)

	step := StepConfig{
		Action: "bead.finish",
		With:   map[string]string{"status": "closed"},
	}

	result := actionBeadFinish(exec, "close", step, exec.graphState)

	if result.Error != nil {
		t.Fatalf("actionBeadFinish returned error on happy path: %v", result.Error)
	}

	// Both the child and the epic must have been closed by the cascade.
	foundEpic := false
	foundChild := false
	for _, id := range closedBeadIDs {
		if id == "spi-epic" {
			foundEpic = true
		}
		if id == "spi-epic.1" {
			foundChild = true
		}
	}
	if !foundEpic {
		t.Errorf("epic was not closed; closed beads: %v", closedBeadIDs)
	}
	if !foundChild {
		t.Errorf("child was not closed; closed beads: %v", closedBeadIDs)
	}
}

// TestActionBeadFinish_AllowsCloseWithOpenRecoveryChild verifies that recovery
// children created by recovery bookkeeping do not trip the stranded-work guard.
// They have no landed code of their own; successful parent close means the
// failure they represent was resolved by the eventual successful run.
func TestActionBeadFinish_AllowsCloseWithOpenRecoveryChild(t *testing.T) {
	tests := []struct {
		name  string
		child Bead
	}{
		{
			name: "inline type recovery child",
			child: Bead{
				ID:     "spi-rec-type",
				Status: "open",
				Type:   "recovery",
				Labels: []string{
					"recovery",
					"recovery:step:implement",
					"recovery:round:1",
					"recovery:source:spi-parent",
				},
			},
		},
		{
			name: "legacy recovery-bead label child",
			child: Bead{
				ID:     "spi-rec-label",
				Status: "open",
				Type:   "task",
				Labels: []string{"recovery-bead", "failure_class:step-failure"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()

			var closedBeadIDs []string
			var addedLabels []string

			deps := &Deps{
				ConfigDir: func() (string, error) { return dir, nil },
				GetChildren: func(parentID string) ([]Bead, error) {
					if parentID == "spi-parent" {
						return []Bead{tt.child}, nil
					}
					return nil, nil
				},
				CloseBead: func(id string) error {
					closedBeadIDs = append(closedBeadIDs, id)
					return nil
				},
				AddLabel: func(id, label string) error {
					addedLabels = append(addedLabels, id+":"+label)
					return nil
				},
				CreateBead:       func(opts CreateOpts) (string, error) { return "spi-msg-1", nil },
				AddDepTyped:      func(issueID, dependsOnID, depType string) error { return nil },
				CloseAttemptBead: func(attemptID, result string) error { return nil },
				GetDependentsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
					return nil, nil
				},
				AddComment:        func(id, text string) error { return nil },
				IsAttemptBead:     func(b Bead) bool { return false },
				IsStepBead:        func(b Bead) bool { return false },
				IsReviewRoundBead: func(b Bead) bool { return false },
			}

			graph := &formula.FormulaStepGraph{
				Name:    "test-recovery-close",
				Version: 3,
				Steps: map[string]formula.StepConfig{
					"close": {Action: "bead.finish"},
				},
			}
			exec := NewGraphForTest("spi-parent", "wizard-parent", graph, nil, deps)

			step := StepConfig{
				Action: "bead.finish",
				With:   map[string]string{"status": "closed"},
			}

			result := actionBeadFinish(exec, "close", step, exec.graphState)
			if result.Error != nil {
				t.Fatalf("actionBeadFinish must not refuse close on recovery child %s: %v", tt.child.ID, result.Error)
			}

			foundParent := false
			foundRecovery := false
			for _, id := range closedBeadIDs {
				if id == "spi-parent" {
					foundParent = true
				}
				if id == tt.child.ID {
					foundRecovery = true
				}
			}
			if !foundParent {
				t.Errorf("parent was not closed; closed beads: %v", closedBeadIDs)
			}
			if !foundRecovery {
				t.Errorf("recovery child was not closed by cascade; closed beads: %v", closedBeadIDs)
			}

			for _, label := range addedLabels {
				if label == "spi-parent:needs-human" {
					t.Errorf("guard wrongly added needs-human for recovery child: %v", addedLabels)
				}
			}
		})
	}
}
