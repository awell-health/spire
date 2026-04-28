package executor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/formula"
)

// TestStepFailure_HooksSourceAndFilesRecoveryBead pins the cleric-foundation
// hook-and-exit invariant (spi-h2d7yn). When a graph step fails, the wizard
// MUST:
//
//   - transition the source bead to status="hooked"
//   - file a recovery bead (type=recovery)
//   - wire a `caused-by` dep from the new recovery bead → source bead
//   - exit cleanly without re-dispatching the failed step
//
// The `related` peer-link is not asserted here because the in-process test
// harness has no active store, so PeerRecoveries returns empty under the
// nil-store recover() guard. Peer-link sort behavior is pinned separately in
// pkg/recovery/metadata_test.go.
func TestStepFailure_HooksSourceAndFilesRecoveryBead(t *testing.T) {
	deps, _ := testGraphDeps(t)

	// Capture UpdateBead calls so we can assert the source bead → hooked
	// transition fires.
	type updateCall struct {
		id      string
		updates map[string]interface{}
	}
	var updates []updateCall
	deps.UpdateBead = func(id string, u map[string]interface{}) error {
		updates = append(updates, updateCall{id: id, updates: u})
		return nil
	}

	// Capture every CreateBead call so we can assert a type=recovery bead
	// landed alongside the alert.
	var creates []CreateOpts
	createdBeadCounter := 0
	deps.CreateBead = func(opts CreateOpts) (string, error) {
		creates = append(creates, opts)
		createdBeadCounter++
		// Return distinct IDs per call so dep-wiring assertions can identify
		// which bead carried which dep.
		return fmt.Sprintf("spi-create-%d", createdBeadCounter), nil
	}

	// Capture every typed dep so we can assert the caused-by edge from
	// recovery → source bead.
	type depCall struct{ from, to, typ string }
	var deps3 []depCall
	deps.AddDepTyped = func(from, to, typ string) error {
		deps3 = append(deps3, depCall{from: from, to: to, typ: typ})
		return nil
	}

	// Override the action registry so the implement step fails
	// deterministically.
	origRegistry := make(map[string]ActionHandler)
	for k, v := range actionRegistry {
		origRegistry[k] = v
	}
	defer func() {
		for k := range actionRegistry {
			delete(actionRegistry, k)
		}
		for k, v := range origRegistry {
			actionRegistry[k] = v
		}
	}()
	actionRegistry["test.fail"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		return ActionResult{Error: fmt.Errorf("test step failure")}
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-step-fail-hooks",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"implement": {
				Action: "test.fail",
				Flow:   "implement",
			},
			"done": {
				Action:   "noop",
				Needs:    []string{"implement"},
				Terminal: true,
			},
		},
	}

	exec := NewGraphForTest("spi-source-task", "wizard-source-task", graph, nil, deps)
	if err := exec.RunGraph(graph, exec.graphState); err != nil {
		t.Fatalf("RunGraph returned unexpected error: %v", err)
	}

	// (1) Source bead must be transitioned to "hooked".
	foundHooked := false
	for _, u := range updates {
		if u.id != "spi-source-task" {
			continue
		}
		if status, ok := u.updates["status"].(string); ok && status == "hooked" {
			foundHooked = true
			break
		}
	}
	if !foundHooked {
		t.Errorf("expected source bead spi-source-task to be transitioned to hooked, got updates: %+v", updates)
	}

	// (2) The implement step state must be parked as "hooked" (not failed).
	if ss, ok := exec.graphState.Steps["implement"]; !ok || ss.Status != "hooked" {
		got := "missing"
		if ok {
			got = ss.Status
		}
		t.Errorf("expected implement step status=hooked, got %s", got)
	}

	// (3) A recovery bead must have been filed.
	var recoveryOpts *CreateOpts
	var recoveryIdx int
	for i := range creates {
		if string(creates[i].Type) == "recovery" {
			recoveryOpts = &creates[i]
			recoveryIdx = i
			break
		}
	}
	if recoveryOpts == nil {
		t.Fatalf("expected at least one CreateBead with type=recovery, got: %+v", creates)
	}
	if !strings.Contains(recoveryOpts.Title, "spi-source-task") {
		t.Errorf("expected recovery title to reference source bead, got: %s", recoveryOpts.Title)
	}
	recoveryID := fmt.Sprintf("spi-create-%d", recoveryIdx+1)

	// (4) A `caused-by` dep must wire the recovery bead → source bead.
	foundCausedBy := false
	for _, d := range deps3 {
		if d.typ == "caused-by" && d.from == recoveryID && d.to == "spi-source-task" {
			foundCausedBy = true
			break
		}
	}
	if !foundCausedBy {
		t.Errorf("expected caused-by dep from %s → spi-source-task, got: %+v", recoveryID, deps3)
	}

	// (5) The graph must terminate cleanly (no infinite re-dispatch on the
	// failed step). We assert the implement step ran exactly once.
	if cc := exec.graphState.Steps["implement"].CompletedCount; cc > 1 {
		t.Errorf("expected implement step to be dispatched once, got CompletedCount=%d (re-dispatch leak)", cc)
	}
}

// TestMostRecentPeerRecovery_NoStore confirms the wrapper's no-peer
// behavior: when the active store is unreachable (the executor's
// in-process test harness), PeerRecoveries returns nil under its
// recover() guard, and mostRecentPeerRecovery returns the empty string.
// This pins the safe-degradation contract: peer-linkage is best-effort,
// never fatal. Cleric foundation (spi-h2d7yn).
func TestMostRecentPeerRecovery_NoStore(t *testing.T) {
	got := mostRecentPeerRecovery("spi-source", "spi-self")
	if got != "" {
		t.Errorf("mostRecentPeerRecovery with no active store = %q, want empty", got)
	}
}
