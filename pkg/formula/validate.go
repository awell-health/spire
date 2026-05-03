package formula

import (
	"fmt"
	"sort"
)

// terminalStatusAllowlist names bead statuses that are legitimately terminal-only:
// they appear as transition targets in [steps.X.lifecycle] blocks but no step is
// expected to resume from them via on_start. validateLifecycle skips orphan
// warnings for any status in this set.
var terminalStatusAllowlist = map[string]bool{
	"closed":         true,
	"awaiting_human": true,
}

// Warning is a non-fatal observation about a formula's structure produced at
// load time. Formula loading succeeds even when warnings are present; callers
// log warnings for operator visibility but do not reject the formula.
type Warning struct {
	Formula string
	Step    string
	Status  string
}

// String renders the warning in the standard form used for log output.
func (w Warning) String() string {
	return fmt.Sprintf(
		"formula %q: status %q declared by step %q lifecycle but never reached as on_start; legitimate if terminal",
		w.Formula, w.Status, w.Step,
	)
}

// validateLifecycle scans every [steps.X.lifecycle] block and returns one
// warning per orphaned status. A status is orphaned when it is named as a
// transition target (on_complete or on_complete_match[].status) but no step
// lists it as on_start, meaning execution can never resume from a bead in
// that status via the formula's declared steps.
//
// on_start values are never orphaned by definition — they are themselves the
// reachable resume points. on_fail is intentionally excluded from the scan;
// failure routing is handled separately by the lifecycle kernel.
//
// Statuses in terminalStatusAllowlist are skipped (e.g. "closed",
// "awaiting_human"): they are terminal by design.
//
// Warnings are advisory. Callers should log them but not reject the formula.
func validateLifecycle(formula *FormulaStepGraph) []Warning {
	if formula == nil || len(formula.Steps) == 0 {
		return nil
	}

	onStart := make(map[string]bool)
	for _, step := range formula.Steps {
		if step.Lifecycle == nil || step.Lifecycle.OnStart == "" {
			continue
		}
		onStart[step.Lifecycle.OnStart] = true
	}

	stepNames := make([]string, 0, len(formula.Steps))
	for name := range formula.Steps {
		stepNames = append(stepNames, name)
	}
	sort.Strings(stepNames)

	type orphan struct{ step, status string }
	seen := make(map[orphan]bool)
	var warnings []Warning
	for _, name := range stepNames {
		lc := formula.Steps[name].Lifecycle
		if lc == nil {
			continue
		}
		targets := make([]string, 0, 1+len(lc.OnCompleteMatch))
		if lc.OnComplete != "" {
			targets = append(targets, lc.OnComplete)
		}
		for _, m := range lc.OnCompleteMatch {
			if m.Status != "" {
				targets = append(targets, m.Status)
			}
		}
		for _, status := range targets {
			if onStart[status] || terminalStatusAllowlist[status] {
				continue
			}
			key := orphan{step: name, status: status}
			if seen[key] {
				continue
			}
			seen[key] = true
			warnings = append(warnings, Warning{
				Formula: formula.Name,
				Step:    name,
				Status:  status,
			})
		}
	}
	return warnings
}
