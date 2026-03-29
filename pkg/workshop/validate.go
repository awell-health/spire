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

var validRoles = map[string]bool{
	"human": true, "apprentice": true, "sage": true, "wizard": true, "skip": true,
}

var validDispatch = map[string]bool{
	"direct": true, "wave": true, "sequential": true,
}

var validStrategy = map[string]bool{
	"squash": true, "merge": true, "rebase": true,
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

	// Level 2: version detection
	ver, _ := raw["version"].(int64)
	switch int(ver) {
	case 2:
		issues = append(issues, validateV2(data)...)
	case 3:
		issues = append(issues, validateV3(data)...)
	default:
		issues = append(issues, Issue{Level: "error", Message: fmt.Sprintf("unsupported or missing formula version: %v", raw["version"])})
	}

	return issues, nil
}

func validateV2(data []byte) []Issue {
	var issues []Issue

	f, err := formula.ParseFormulaV2(data)
	if err != nil {
		issues = append(issues, Issue{Level: "error", Message: fmt.Sprintf("v2 parse error: %v", err)})
		return issues
	}

	for phaseName, pc := range f.Phases {
		role := pc.GetRole()
		if !validRoles[role] {
			issues = append(issues, Issue{
				Level: "error", Phase: phaseName,
				Message: fmt.Sprintf("invalid role %q (must be human|apprentice|sage|wizard|skip)", role),
			})
		}

		dispatch := pc.GetDispatch()
		if !validDispatch[dispatch] {
			issues = append(issues, Issue{
				Level: "error", Phase: phaseName,
				Message: fmt.Sprintf("invalid dispatch %q (must be direct|wave|sequential)", dispatch),
			})
		}

		if pc.MergeStrategy != "" && !validStrategy[pc.MergeStrategy] {
			issues = append(issues, Issue{
				Level: "error", Phase: phaseName,
				Message: fmt.Sprintf("invalid merge strategy %q (must be squash|merge|rebase)", pc.MergeStrategy),
			})
		}

		// Logical checks
		if dispatch == "wave" && pc.StagingBranch == "" {
			issues = append(issues, Issue{
				Level: "warning", Phase: phaseName,
				Message: "wave dispatch without staging_branch",
			})
		}

		if role == "sage" && pc.RevisionPolicy == nil && phaseName == "review" {
			issues = append(issues, Issue{
				Level: "warning", Phase: phaseName,
				Message: "review phase with sage role has no revision_policy",
			})
		}

		if pc.Timeout != "" && !timeoutRe.MatchString(pc.Timeout) {
			issues = append(issues, Issue{
				Level: "warning", Phase: phaseName,
				Message: fmt.Sprintf("timeout %q may not be a valid duration (expected e.g. 10m, 5s, 1h)", pc.Timeout),
			})
		}
	}

	return issues
}

func validateV3(data []byte) []Issue {
	var issues []Issue

	f, err := formula.ParseFormulaStepGraph(data)
	if err != nil {
		issues = append(issues, Issue{Level: "error", Message: fmt.Sprintf("v3 parse error: %v", err)})
		return issues
	}

	// Structural validation
	if err := formula.ValidateGraph(f); err != nil {
		issues = append(issues, Issue{Level: "error", Message: fmt.Sprintf("graph validation: %v", err)})
	}

	for stepName, step := range f.Steps {
		// Role check
		if step.Role != "" && !validStepRoles[step.Role] {
			issues = append(issues, Issue{
				Level: "error", Phase: stepName,
				Message: fmt.Sprintf("invalid role %q", step.Role),
			})
		}

		// Condition syntax check
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
