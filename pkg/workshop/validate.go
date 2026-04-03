package workshop

import (
	"fmt"
	"regexp"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/awell-health/spire/pkg/formula"
)

// Issue represents a validation finding.
type Issue struct {
	Level   string `json:"level"`   // "error" or "warning"
	Phase   string `json:"phase"`   // phase/step name, or "" for formula-level
	Message string `json:"message"`
}

var validStepRoles = map[string]bool{
	"sage": true, "apprentice": true, "arbiter": true, "executor": true,
	"human": true, "wizard": true, "skip": true,
}

var timeoutRe = regexp.MustCompile(`^\d+[smh]$`)

// Validate loads a formula by name and runs multi-level validation.
// Returns accumulated issues. error is non-nil only if the formula cannot be found.
func Validate(name string) ([]Issue, error) {
	data, _, err := loadRawFormula(name)
	if err != nil {
		return nil, err
	}

	var issues []Issue

	// Level 1: TOML syntax
	var raw map[string]interface{}
	if err := toml.Unmarshal(data, &raw); err != nil {
		issues = append(issues, Issue{Level: "error", Message: fmt.Sprintf("TOML syntax error: %v", err)})
		return issues, nil
	}

	issues = append(issues, validateV3(data)...)
	return issues, nil
}

func validateV3(data []byte) []Issue {
	var issues []Issue

	f, err := formula.ParseFormulaStepGraph(data)
	if err != nil {
		issues = append(issues, Issue{Level: "error", Message: fmt.Sprintf("v3 parse error: %v", err)})
		return issues
	}

	// Per-step validators
	for stepName, step := range f.Steps {
		// Role check (backward compat field)
		if step.Role != "" && !validStepRoles[step.Role] {
			issues = append(issues, Issue{
				Level: "error", Phase: stepName,
				Message: fmt.Sprintf("invalid role %q", step.Role),
			})
		}

		// Condition syntax check (backward compat field)
		if step.Condition != "" {
			if err := tryParseCondition(step.Condition); err != nil {
				issues = append(issues, Issue{
					Level: "error", Phase: stepName,
					Message: fmt.Sprintf("condition syntax error: %v", err),
				})
			}
		}

		if step.Timeout != "" && !timeoutRe.MatchString(step.Timeout) {
			issues = append(issues, Issue{
				Level: "warning", Phase: stepName,
				Message: fmt.Sprintf("timeout %q may not be a valid duration", step.Timeout),
			})
		}

		issues = append(issues, validateV3StepKind(stepName, step)...)
		issues = append(issues, validateV3Action(stepName, step)...)
		issues = append(issues, validateV3When(stepName, step)...)
		issues = append(issues, validateV3Produces(stepName, step)...)
	}

	issues = append(issues, validateV3Workspaces(f)...)
	issues = append(issues, validateV3Vars(f)...)

	return issues
}

// validateV3StepKind checks step kind constraints.
// dispatch requires with.children and with.strategy; call requires graph field.
func validateV3StepKind(stepName string, step formula.StepConfig) []Issue {
	var issues []Issue
	if step.Kind == "" {
		return nil
	}
	if step.Kind == formula.StepKindDispatch {
		if step.With == nil || step.With["children"] == "" {
			issues = append(issues, Issue{
				Level: "warning", Phase: stepName,
				Message: "dispatch step missing with.children",
			})
		}
		if step.With == nil || step.With["strategy"] == "" {
			issues = append(issues, Issue{
				Level: "warning", Phase: stepName,
				Message: "dispatch step missing with.strategy",
			})
		}
	}
	if step.Kind == formula.StepKindCall && step.Graph == "" {
		issues = append(issues, Issue{
			Level: "error", Phase: stepName,
			Message: "call step requires graph field",
		})
	}
	return issues
}

// validateV3Action checks action (opcode) constraints.
func validateV3Action(stepName string, step formula.StepConfig) []Issue {
	var issues []Issue
	if step.Action == "" {
		return nil
	}
	if step.Action == formula.OpcodeWizardRun && step.Flow == "" {
		issues = append(issues, Issue{
			Level: "warning", Phase: stepName,
			Message: "wizard.run action without flow field",
		})
	}
	if step.Action == formula.OpcodeGraphRun && step.Graph == "" {
		issues = append(issues, Issue{
			Level: "error", Phase: stepName,
			Message: "graph.run action requires graph field",
		})
	}
	return issues
}

// validateV3Workspaces checks workspace declarations and step workspace references.
func validateV3Workspaces(f *formula.FormulaStepGraph) []Issue {
	var issues []Issue
	// Workspace declarations are already validated by formula.ValidateGraph
	// (called inside ParseFormulaStepGraph). Here we add workshop-level warnings.
	for name, ws := range f.Workspaces {
		if ws.Kind != formula.WorkspaceKindRepo && ws.Branch == "" {
			issues = append(issues, Issue{
				Level: "warning", Phase: "workspace:" + name,
				Message: fmt.Sprintf("non-repo workspace %q has no branch template", name),
			})
		}
	}
	return issues
}

// validateV3When checks structured when predicates for completeness.
func validateV3When(stepName string, step formula.StepConfig) []Issue {
	var issues []Issue
	if step.When == nil {
		return nil
	}
	for i, p := range step.When.All {
		if p.Left == "" {
			issues = append(issues, Issue{
				Level: "error", Phase: stepName,
				Message: fmt.Sprintf("when.all[%d] missing left operand", i),
			})
		}
		if p.Right == "" {
			issues = append(issues, Issue{
				Level: "error", Phase: stepName,
				Message: fmt.Sprintf("when.all[%d] missing right operand", i),
			})
		}
	}
	for i, p := range step.When.Any {
		if p.Left == "" {
			issues = append(issues, Issue{
				Level: "error", Phase: stepName,
				Message: fmt.Sprintf("when.any[%d] missing left operand", i),
			})
		}
		if p.Right == "" {
			issues = append(issues, Issue{
				Level: "error", Phase: stepName,
				Message: fmt.Sprintf("when.any[%d] missing right operand", i),
			})
		}
	}
	return issues
}

// validateV3Vars checks typed variable declarations.
func validateV3Vars(f *formula.FormulaStepGraph) []Issue {
	var issues []Issue
	for name, v := range f.Vars {
		if v.Required && v.Default == "" {
			issues = append(issues, Issue{
				Level: "warning", Phase: "var:" + name,
				Message: fmt.Sprintf("required variable %q has no default value", name),
			})
		}
	}
	return issues
}

// validateV3Produces checks that produces entries are non-empty.
func validateV3Produces(stepName string, step formula.StepConfig) []Issue {
	var issues []Issue
	for i, p := range step.Produces {
		if p == "" {
			issues = append(issues, Issue{
				Level: "error", Phase: stepName,
				Message: fmt.Sprintf("produces[%d] is empty", i),
			})
		}
	}
	return issues
}

// tryParseCondition validates condition syntax without evaluating it.
// Uses EvalCondition with an empty context — missing fields return false (not error),
// so only actual syntax errors (malformed operators, missing operands) are caught.
func tryParseCondition(expr string) error {
	_, err := formula.EvalCondition(expr, map[string]string{})
	return err
}
