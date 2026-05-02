package formula

import (
	"fmt"
	"strings"
)

// LifecycleConfig declares per-step lifecycle hooks that map executor step
// transitions to bead status updates. The data lives in pkg/formula because
// formulas declare it; pkg/lifecycle consumes it via the evaluator.
type LifecycleConfig struct {
	OnStart         string        `toml:"on_start,omitempty"`
	OnComplete      string        `toml:"on_complete,omitempty"`
	OnFail          *FailAction   `toml:"on_fail,omitempty"`
	OnCompleteMatch []MatchClause `toml:"on_complete_match,omitempty"`
}

// FailAction declares how a step failure should be reflected in bead status.
// Status sets the new status directly; Event delegates to a core lifecycle
// event (e.g. Event="Escalated" routes through the escalation rule).
type FailAction struct {
	Status string `toml:"status,omitempty"`
	Event  string `toml:"event,omitempty"`
}

// MatchClause is a single conditional arm of OnCompleteMatch. The first clause
// whose When expression evaluates true wins; its Status is applied.
type MatchClause struct {
	When   string `toml:"when,omitempty"`
	Status string `toml:"status,omitempty"`
}

// EvalWhen evaluates a small DSL expression against an outputs map.
//
// Supported syntax:
//
//	outputs.<field> == '<literal>'
//	outputs.<field> != '<literal>'
//	outputs.<field> contains '<literal>'
//
// String literals may be wrapped in single or double quotes. For `contains`,
// the field may resolve to either a string (substring check) or a slice of
// strings (membership check).
//
// Errors: unknown operator, unparseable expression, missing `outputs.` prefix,
// unterminated quoted literal.
func EvalWhen(when string, outputs map[string]any) (bool, error) {
	expr := strings.TrimSpace(when)
	if expr == "" {
		return false, fmt.Errorf("empty when expression")
	}

	left, op, rightRaw, err := splitWhenExpr(expr)
	if err != nil {
		return false, err
	}

	if !strings.HasPrefix(left, "outputs.") {
		return false, fmt.Errorf("when expression must reference outputs.<field>: %q", expr)
	}
	field := strings.TrimPrefix(left, "outputs.")
	if field == "" {
		return false, fmt.Errorf("when expression has empty outputs field: %q", expr)
	}

	literal, err := parseStringLiteral(rightRaw)
	if err != nil {
		return false, fmt.Errorf("when expression %q: %w", expr, err)
	}

	val, present := outputs[field]

	switch op {
	case "==":
		if !present {
			return false, nil
		}
		s, ok := stringValue(val)
		if !ok {
			return false, nil
		}
		return s == literal, nil
	case "!=":
		if !present {
			return literal != "", nil
		}
		s, ok := stringValue(val)
		if !ok {
			return true, nil
		}
		return s != literal, nil
	case "contains":
		if !present {
			return false, nil
		}
		return containsValue(val, literal), nil
	}
	return false, fmt.Errorf("unsupported when operator %q", op)
}

// splitWhenExpr breaks an expression into (left, op, right) tokens.
// Operators are matched by their surrounding whitespace.
func splitWhenExpr(expr string) (left, op, right string, err error) {
	for _, candidate := range []string{" == ", " != ", " contains "} {
		idx := strings.Index(expr, candidate)
		if idx < 0 {
			continue
		}
		left = strings.TrimSpace(expr[:idx])
		op = strings.TrimSpace(candidate)
		right = strings.TrimSpace(expr[idx+len(candidate):])
		return left, op, right, nil
	}
	return "", "", "", fmt.Errorf("unparseable when expression: %q", expr)
}

// parseStringLiteral unwraps a single- or double-quoted string literal.
func parseStringLiteral(raw string) (string, error) {
	if len(raw) < 2 {
		return "", fmt.Errorf("expected quoted string literal, got %q", raw)
	}
	first := raw[0]
	last := raw[len(raw)-1]
	if (first != '\'' && first != '"') || first != last {
		return "", fmt.Errorf("expected quoted string literal, got %q", raw)
	}
	return raw[1 : len(raw)-1], nil
}

// stringValue extracts a string from an arbitrary outputs value when possible.
// Returns false for non-string values (callers decide whether that's an error).
func stringValue(v any) (string, bool) {
	if v == nil {
		return "", false
	}
	if s, ok := v.(string); ok {
		return s, true
	}
	return "", false
}

// containsValue implements the `contains` operator. If the value is a slice
// of strings (or []any of strings), it checks membership; if it's a plain
// string, it falls back to substring matching.
func containsValue(v any, literal string) bool {
	if v == nil {
		return false
	}
	switch t := v.(type) {
	case string:
		return strings.Contains(t, literal)
	case []string:
		for _, s := range t {
			if s == literal {
				return true
			}
		}
		return false
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok && s == literal {
				return true
			}
		}
		return false
	}
	return false
}
