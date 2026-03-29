package formula

import (
	"fmt"
	"strconv"
	"strings"
)

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
