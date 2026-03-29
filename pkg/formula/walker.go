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
		ok, err := EvalCondition(step.Condition, ctx)
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
func EntryStep(graph *FormulaStepGraph) string {
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
//   - Exactly one entry point (step with no needs)
//   - All referenced needs exist
//   - No self-references in needs
//   - At least one terminal step
func ValidateGraph(graph *FormulaStepGraph) error {
	if len(graph.Steps) == 0 {
		return fmt.Errorf("step graph has no steps")
	}

	entryCount := 0
	terminalCount := 0
	for name, step := range graph.Steps {
		if len(step.Needs) == 0 {
			entryCount++
		}
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
	}

	if entryCount != 1 {
		return fmt.Errorf("step graph must have exactly one entry point, found %d", entryCount)
	}
	if terminalCount == 0 {
		return fmt.Errorf("step graph has no terminal steps")
	}

	return nil
}
