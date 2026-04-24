package executor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/formula"
)

// TestRecoveryCycle_RunsEvenWithoutEnvVar is the acceptance proof for
// spi-gdzd7d: with SPIRE_INLINE_RECOVERY unset AND SPIRE_DISABLE_INLINE_RECOVERY
// unset, a step failure must enter runRecoveryCycle. We look for the
// `recovery[...] cycle start` log line before any hook transition as the
// signal that the cycle ran, per acceptance #1.
func TestRecoveryCycle_RunsEvenWithoutEnvVar(t *testing.T) {
	t.Setenv("SPIRE_INLINE_RECOVERY", "")
	t.Setenv("SPIRE_DISABLE_INLINE_RECOVERY", "")

	deps, _ := testGraphDeps(t)

	var logged []string
	origLog := func(format string, args ...interface{}) {}
	_ = origLog

	// Force-fail the implement step so we trip the recovery path.
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
		return ActionResult{Error: fmt.Errorf("synthetic implementation failure")}
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-recovery-default",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"implement": {Action: "test.fail"},
			"done":      {Action: "noop", Needs: []string{"implement"}, Terminal: true},
		},
	}

	exec := NewGraphForTest("spi-default", "wizard-default", graph, nil, deps)
	// Capture the interpreter's logs; the graph state's log is populated
	// via the log func set in NewGraphForTest.
	exec.log = func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	// Run to completion (the step will fail → enter recovery cycle →
	// eventually hook since Decide has no ClaudeRunner and dispatch fails
	// without a real workspace).
	_ = exec.RunGraph(graph, exec.graphState)

	// Find the cycle-start log line before any hook log.
	var cycleStartIdx, hookIdx int = -1, -1
	for i, line := range logged {
		if strings.Contains(line, "cycle start") && strings.Contains(line, "recovery[bead=spi-default") {
			cycleStartIdx = i
			break
		}
	}
	for i, line := range logged {
		if strings.Contains(line, "hooked for recovery") {
			hookIdx = i
			break
		}
	}

	if cycleStartIdx < 0 {
		t.Errorf("expected a 'cycle start' log line with the recovery prefix; captured log:\n%s",
			strings.Join(logged, "\n"))
	}
	if hookIdx >= 0 && cycleStartIdx >= 0 && cycleStartIdx >= hookIdx {
		t.Errorf("cycle start (%d) must appear BEFORE hook log (%d)", cycleStartIdx, hookIdx)
	}
}

// TestMergeFailure_EntersRecoveryCycle is a narrower variant that asserts on
// the diagnose phase specifically — so operators reading the acceptance #1
// requirement can see the exact `diagnose start` line, not just the cycle
// opener.
func TestMergeFailure_EntersRecoveryCycle(t *testing.T) {
	t.Setenv("SPIRE_INLINE_RECOVERY", "")
	t.Setenv("SPIRE_DISABLE_INLINE_RECOVERY", "")

	deps, _ := testGraphDeps(t)

	var logged []string

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

	actionRegistry["test.merge-race"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		return ActionResult{Error: fmt.Errorf("ff-only failed: merge race: main advanced during landing")}
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-merge-recovery",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"merge": {Action: "test.merge-race"},
			"done":  {Action: "noop", Needs: []string{"merge"}, Terminal: true},
		},
	}

	exec := NewGraphForTest("spi-merge", "wizard-merge", graph, nil, deps)
	exec.log = func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	_ = exec.RunGraph(graph, exec.graphState)

	var seen = map[string]bool{}
	for _, line := range logged {
		for _, frag := range []string{"cycle start", "diagnose start", "diagnose produced", "decide start", "dispatch start", "dispatch result", "cycle close"} {
			if strings.Contains(line, frag) && strings.Contains(line, "recovery[bead=spi-merge") {
				seen[frag] = true
			}
		}
	}
	for _, frag := range []string{"cycle start", "diagnose start", "decide start", "cycle close"} {
		if !seen[frag] {
			t.Errorf("missing log fragment %q in captured cycle logs:\n%s",
				frag, strings.Join(logged, "\n"))
		}
	}
}

// TestRecoveryCycle_KillSwitchSkipsCycle confirms the inverse: when the kill-
// switch is active, the cycle does NOT run and the legacy hook-and-escalate
// path takes over. No recovery-prefixed log lines should appear.
func TestRecoveryCycle_KillSwitchSkipsCycle(t *testing.T) {
	t.Setenv("SPIRE_DISABLE_INLINE_RECOVERY", "1")

	deps, _ := testGraphDeps(t)

	var logged []string

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
		return ActionResult{Error: fmt.Errorf("synthetic failure")}
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-kill-switch",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"implement": {Action: "test.fail"},
			"done":      {Action: "noop", Needs: []string{"implement"}, Terminal: true},
		},
	}

	exec := NewGraphForTest("spi-killsw", "wizard-killsw", graph, nil, deps)
	exec.log = func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	_ = exec.RunGraph(graph, exec.graphState)

	for _, line := range logged {
		if strings.Contains(line, "recovery[bead=spi-killsw") {
			t.Errorf("kill-switch should skip recovery cycle, but saw line: %q", line)
		}
		if strings.Contains(line, "entering recovery cycle") {
			t.Errorf("kill-switch should skip recovery cycle, but saw: %q", line)
		}
	}
}
