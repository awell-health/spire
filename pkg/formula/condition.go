package formula

import (
	"fmt"
	"strconv"
	"strings"
)

// Predicate is a single structured comparison clause.
type Predicate struct {
	Left  string `toml:"left"`  // dotted path: "steps.review.outputs.verdict", "vars.max_rounds"
	Op    string `toml:"op"`    // "eq", "ne", "lt", "gt", "le", "ge"
	Right string `toml:"right"` // literal value or dotted path
}

// StructuredCondition replaces string conditions with typed predicates.
// If both All and Any are set: all All predicates AND at least one Any predicate must hold.
type StructuredCondition struct {
	All []Predicate `toml:"all,omitempty"`
	Any []Predicate `toml:"any,omitempty"`
}

// validPredicateOps is the set of recognized predicate operators.
var validPredicateOps = map[string]bool{
	"eq": true, "ne": true,
	"lt": true, "gt": true,
	"le": true, "ge": true,
}

// ValidPredicateOp returns true if the operator is recognized.
func ValidPredicateOp(op string) bool {
	return validPredicateOps[op]
}

// resolveValue looks up a dotted path in the context map. If not found,
// returns the raw string as a literal.
func resolveValue(s string, ctx map[string]string) string {
	if v, ok := ctx[s]; ok {
		return v
	}
	return s
}

// EvalPredicate evaluates a single predicate against the context map.
// Left and Right are resolved via the context map (if the key exists);
// otherwise they are treated as literal values.
func EvalPredicate(p Predicate, ctx map[string]string) (bool, error) {
	left := resolveValue(p.Left, ctx)
	right := resolveValue(p.Right, ctx)

	// If Left was a context key and wasn't found, the predicate is false.
	if _, ok := ctx[p.Left]; !ok && left == p.Left && strings.Contains(p.Left, ".") {
		return false, nil
	}

	switch p.Op {
	case "eq":
		return left == right, nil
	case "ne":
		return left != right, nil
	case "lt", "gt", "le", "ge":
		a, err := strconv.Atoi(left)
		if err != nil {
			return false, fmt.Errorf("non-numeric left value for %q: %q", p.Left, left)
		}
		b, err := strconv.Atoi(right)
		if err != nil {
			return false, fmt.Errorf("non-numeric right value for %q: %q", p.Right, right)
		}
		switch p.Op {
		case "lt":
			return a < b, nil
		case "gt":
			return a > b, nil
		case "le":
			return a <= b, nil
		case "ge":
			return a >= b, nil
		}
	}
	return false, fmt.Errorf("unknown predicate operator %q", p.Op)
}

// EvalStructuredCondition evaluates a structured condition against a context map.
// The context map uses dotted keys (e.g. "steps.review.outputs.verdict" -> "approve").
// Nil condition is unconditionally true.
func EvalStructuredCondition(cond *StructuredCondition, ctx map[string]string) (bool, error) {
	if cond == nil {
		return true, nil
	}

	// Evaluate All predicates — every one must pass.
	for _, p := range cond.All {
		ok, err := EvalPredicate(p, ctx)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}

	// Evaluate Any predicates — at least one must pass (if any are declared).
	if len(cond.Any) > 0 {
		anyOK := false
		for _, p := range cond.Any {
			ok, err := EvalPredicate(p, ctx)
			if err != nil {
				return false, err
			}
			if ok {
				anyOK = true
				break
			}
		}
		if !anyOK {
			return false, nil
		}
	}

	return true, nil
}

// EvalStepCondition evaluates whichever condition a step declares (When or Condition).
// Returns error if both When and Condition are set on the same step.
func EvalStepCondition(step StepConfig, ctx map[string]string) (bool, error) {
	if step.When != nil && step.Condition != "" {
		return false, fmt.Errorf("step declares both when and condition; use only one")
	}
	if step.When != nil {
		return EvalStructuredCondition(step.When, ctx)
	}
	if step.Condition != "" {
		return EvalCondition(step.Condition, ctx)
	}
	return true, nil
}

// EvalCondition evaluates a compound condition expression against a context map.
// Empty expressions are unconditionally true. Expressions support:
//   - OR groups separated by " || "
//   - AND clauses within groups separated by " && "
//   - Comparison operators: ==, !=, <, >, <=, >=
//
// Missing context fields evaluate to false (not an error).
func EvalCondition(expr string, ctx map[string]string) (bool, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return true, nil
	}

	orGroups := strings.Split(expr, " || ")
	for _, group := range orGroups {
		ok, err := evalAndGroup(strings.TrimSpace(group), ctx)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

func evalAndGroup(group string, ctx map[string]string) (bool, error) {
	clauses := strings.Split(group, " && ")
	for _, clause := range clauses {
		field, op, value, err := parseClause(strings.TrimSpace(clause))
		if err != nil {
			return false, err
		}
		ok, err := evalClause(field, op, value, ctx)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

func parseClause(clause string) (field, op, value string, err error) {
	for _, twoChar := range []string{"==", "!=", ">=", "<="} {
		idx := strings.Index(clause, " "+twoChar+" ")
		if idx >= 0 {
			return strings.TrimSpace(clause[:idx]), twoChar,
				strings.TrimSpace(clause[idx+len(twoChar)+2:]), nil
		}
	}
	for _, oneChar := range []string{">", "<"} {
		idx := strings.Index(clause, " "+oneChar+" ")
		if idx >= 0 {
			return strings.TrimSpace(clause[:idx]), oneChar,
				strings.TrimSpace(clause[idx+len(oneChar)+2:]), nil
		}
	}
	return "", "", "", fmt.Errorf("unparseable condition clause: %q", clause)
}

func evalClause(field, op, value string, ctx map[string]string) (bool, error) {
	ctxVal, ok := ctx[field]
	if !ok {
		return false, nil
	}

	switch op {
	case "==":
		return ctxVal == value, nil
	case "!=":
		return ctxVal != value, nil
	case "<", ">", "<=", ">=":
		a, err := strconv.Atoi(ctxVal)
		if err != nil {
			return false, fmt.Errorf("non-numeric context value for %q: %q", field, ctxVal)
		}
		// Resolve the right-hand side: if it's not a literal number,
		// look it up in the context (e.g. "round < max_rounds").
		rhs := value
		if _, err := strconv.Atoi(rhs); err != nil {
			if resolved, ok := ctx[rhs]; ok {
				rhs = resolved
			}
		}
		b, err := strconv.Atoi(rhs)
		if err != nil {
			return false, fmt.Errorf("non-numeric comparison value for %q: %q", field, rhs)
		}
		switch op {
		case "<":
			return a < b, nil
		case ">":
			return a > b, nil
		case "<=":
			return a <= b, nil
		case ">=":
			return a >= b, nil
		}
	}
	return false, fmt.Errorf("unknown operator %q", op)
}
