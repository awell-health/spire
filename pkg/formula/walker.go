package formula

import (
	"fmt"
	"sort"
)

// NextSteps returns the step names that are ready to execute given current
// completion state and condition context. A step is ready when:
//  1. It is not completed.
//  2. Its needs are met (at least one predecessor completed, OR no needs).
//  3. Its condition evaluates to true.
func NextSteps(graph *FormulaStepGraph, completed map[string]bool, ctx map[string]string) ([]string, error) {
	var ready []string
	for name, step := range graph.Steps {
		if completed[name] {
			continue
		}
		if len(step.Needs) > 0 {
			needsMet := false
			for _, need := range step.Needs {
				if completed[need] {
					needsMet = true
					break
				}
			}
			if !needsMet {
				continue
			}
		}
		ok, err := EvalStepCondition(step, ctx)
		if err != nil {
			return nil, fmt.Errorf("step %q condition: %w", name, err)
		}
		if !ok {
			continue
		}
		ready = append(ready, name)
	}
	sort.Strings(ready)
	return ready, nil
}

// EntryStep returns the step name with an empty needs list (the entry point).
// If graph.Entry is set, it takes precedence over scanning for needless steps.
func EntryStep(graph *FormulaStepGraph) string {
	if graph.Entry != "" {
		if _, ok := graph.Steps[graph.Entry]; ok {
			return graph.Entry
		}
		return ""
	}
	for name, step := range graph.Steps {
		if len(step.Needs) == 0 {
			return name
		}
	}
	return ""
}

// IsTerminal returns whether the named step is terminal.
func IsTerminal(graph *FormulaStepGraph, stepName string) bool {
	if step, ok := graph.Steps[stepName]; ok {
		return step.Terminal
	}
	return false
}

// ValidateGraph checks that a step graph is well-formed:
//   - At least one step exists
//   - Exactly one entry point (step with no needs, or explicit Entry)
//   - All referenced needs exist
//   - No self-references in needs
//   - At least one terminal step
//   - v3 fields: valid step kinds, opcodes, workspace refs, var types, condition exclusion
func ValidateGraph(graph *FormulaStepGraph) error {
	if len(graph.Steps) == 0 {
		return fmt.Errorf("step graph has no steps")
	}

	// Entry point validation: explicit Entry field takes precedence.
	if graph.Entry != "" {
		if _, ok := graph.Steps[graph.Entry]; !ok {
			return fmt.Errorf("explicit entry %q does not exist in steps", graph.Entry)
		}
	} else {
		entryCount := 0
		for _, step := range graph.Steps {
			if len(step.Needs) == 0 {
				entryCount++
			}
		}
		if entryCount != 1 {
			return fmt.Errorf("step graph must have exactly one entry point, found %d", entryCount)
		}
	}

	terminalCount := 0
	for name, step := range graph.Steps {
		if step.Terminal {
			terminalCount++
		}
		for _, need := range step.Needs {
			if need == name {
				return fmt.Errorf("step %q references itself in needs", name)
			}
			if _, ok := graph.Steps[need]; !ok {
				return fmt.Errorf("step %q needs %q which does not exist", name, need)
			}
		}

		// v3 field validation — only triggers when new fields are populated.
		if step.Kind != "" && !ValidStepKind(step.Kind) {
			return fmt.Errorf("step %q: invalid kind %q", name, step.Kind)
		}
		if step.Action != "" && !ValidOpcode(step.Action) {
			return fmt.Errorf("step %q: invalid action %q", name, step.Action)
		}
		if step.Workspace != "" && graph.Workspaces != nil {
			if _, ok := graph.Workspaces[step.Workspace]; !ok {
				return fmt.Errorf("step %q: workspace %q not declared", name, step.Workspace)
			}
		}
		if step.Workspace != "" && graph.Workspaces == nil {
			return fmt.Errorf("step %q: workspace %q referenced but no workspaces declared", name, step.Workspace)
		}
		if step.When != nil && step.Condition != "" {
			return fmt.Errorf("step %q: declares both when and condition; use only one", name)
		}
		if step.When != nil {
			for i, p := range step.When.All {
				if !ValidPredicateOp(p.Op) {
					return fmt.Errorf("step %q: when.all[%d] invalid op %q", name, i, p.Op)
				}
			}
			for i, p := range step.When.Any {
				if !ValidPredicateOp(p.Op) {
					return fmt.Errorf("step %q: when.any[%d] invalid op %q", name, i, p.Op)
				}
			}
		}
	}

	if terminalCount == 0 {
		return fmt.Errorf("step graph has no terminal steps")
	}

	// Validate workspace declarations.
	if graph.Workspaces != nil {
		if err := ValidateWorkspaces(graph.Workspaces); err != nil {
			return err
		}
	}

	// Validate var types.
	for name, v := range graph.Vars {
		if v.Type != "" && !ValidVarType(v.Type) {
			return fmt.Errorf("var %q: invalid type %q", name, v.Type)
		}
	}

	return nil
}
