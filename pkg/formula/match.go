package formula

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Eval evaluates a single on_complete_match `when` expression against
// step outputs. Returns true if the expression matches, false otherwise.
// A parse error returns (false, error); callers (the lifecycle
// evaluator) treat parse errors as no-match and log them.
//
// Grammar:
//
//	expr   := "outputs." FIELD WS OP WS LITERAL
//	OP     := "==" | "!=" | "contains"
//	LITERAL := QUOTED_STRING | NUMBER | BOOL
//	QUOTED_STRING := "'" ... "'"  |  "\"" ... "\""
//	NUMBER := /-?\d+(\.\d+)?/
//	BOOL   := "true" | "false"
//
// Field access is flat: outputs.foo is supported, outputs.foo.bar is not.
//
// Semantics:
//   - Field missing from outputs: returns (false, nil) for all operators.
//   - `==` / `!=`: typed comparison. The literal type and the field type
//     must be compatible (string vs string, numeric vs numeric, bool vs
//     bool). A type mismatch returns (false, *TypeMismatchError).
//   - `contains`: only valid with a string literal. Against a string field
//     it performs substring matching; against a slice ([]string or []any)
//     it performs element-membership matching. Other field types return
//     a *TypeMismatchError.
//
// Parse errors are returned as *ParseError. Type mismatches are returned
// as *TypeMismatchError. Both implement error.
func Eval(expr string, outputs map[string]any) (bool, error) {
	parsed, err := parseMatchExpr(expr)
	if err != nil {
		return false, err
	}
	return parsed.eval(outputs)
}

// EvalWhen is a backward-compatible alias for Eval. The lifecycle
// evaluator (pkg/lifecycle/evaluator.go) imports this name; new code
// should call Eval directly.
func EvalWhen(when string, outputs map[string]any) (bool, error) {
	return Eval(when, outputs)
}

// ParseError indicates the expression could not be tokenized or parsed.
type ParseError struct {
	Expr   string
	Reason string
}

func (e *ParseError) Error() string {
	if e.Expr == "" {
		return fmt.Sprintf("formula: parse error in when expression: %s", e.Reason)
	}
	return fmt.Sprintf("formula: parse error in when expression %q: %s", e.Expr, e.Reason)
}

// TypeMismatchError indicates an operator was applied to incompatible
// types: e.g. comparing a string field against a numeric literal, or
// using `contains` against a numeric field.
type TypeMismatchError struct {
	Field       string
	Op          string
	LiteralKind string
	ActualType  string
}

func (e *TypeMismatchError) Error() string {
	return fmt.Sprintf(
		"formula: type mismatch in when expression: field %q (type %s) is not comparable to %s literal via %q",
		e.Field, e.ActualType, e.LiteralKind, e.Op,
	)
}

// litKind tags the parsed literal's intrinsic type.
type litKind int

const (
	litString litKind = iota
	litInt
	litFloat
	litBool
)

func (k litKind) String() string {
	switch k {
	case litString:
		return "string"
	case litInt:
		return "int"
	case litFloat:
		return "float"
	case litBool:
		return "bool"
	}
	return "unknown"
}

// literal holds a parsed RHS value.
type literal struct {
	kind litKind
	str  string
	i    int64
	f    float64
	b    bool
}

// matchExpr is a parsed `when` expression.
type matchExpr struct {
	raw   string
	field string
	op    string
	lit   literal
}

// parseMatchExpr tokenises an expression of the form
// `outputs.<field> <op> <literal>` into its three parts.
func parseMatchExpr(expr string) (*matchExpr, error) {
	raw := strings.TrimSpace(expr)
	if raw == "" {
		return nil, &ParseError{Expr: expr, Reason: "empty expression"}
	}

	left, op, right, err := splitOnOperator(raw)
	if err != nil {
		return nil, &ParseError{Expr: expr, Reason: err.Error()}
	}

	if !strings.HasPrefix(left, "outputs.") {
		return nil, &ParseError{Expr: expr, Reason: "left side must reference outputs.<field>"}
	}
	field := strings.TrimPrefix(left, "outputs.")
	if field == "" {
		return nil, &ParseError{Expr: expr, Reason: "empty field name after outputs."}
	}
	if strings.Contains(field, ".") {
		return nil, &ParseError{Expr: expr, Reason: "nested field paths are not supported (use flat outputs.<field>)"}
	}
	if !validFieldName(field) {
		return nil, &ParseError{Expr: expr, Reason: fmt.Sprintf("invalid field name %q", field)}
	}

	lit, err := parseLiteral(right)
	if err != nil {
		return nil, &ParseError{Expr: expr, Reason: err.Error()}
	}

	return &matchExpr{raw: raw, field: field, op: op, lit: lit}, nil
}

// splitOnOperator finds the operator in expr (matched by required
// surrounding whitespace) and returns the trimmed left side, operator,
// and right side. Operators are tried longest first so that `contains`
// is preferred over a substring of `==` / `!=` (none collide today, but
// the discipline keeps future additions safe).
func splitOnOperator(expr string) (left, op, right string, err error) {
	candidates := []string{" contains ", " == ", " != "}
	for _, c := range candidates {
		idx := strings.Index(expr, c)
		if idx < 0 {
			continue
		}
		left = strings.TrimSpace(expr[:idx])
		op = strings.TrimSpace(c)
		right = strings.TrimSpace(expr[idx+len(c):])
		return left, op, right, nil
	}
	return "", "", "", errors.New("no operator (==, !=, or contains) surrounded by spaces")
}

// validFieldName accepts identifier-ish characters: letters, digits,
// underscore, hyphen.
func validFieldName(field string) bool {
	if field == "" {
		return false
	}
	for _, r := range field {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

// parseLiteral interprets the RHS token as one of:
//   - quoted string ('...' or "...")
//   - bare integer (42, -5)
//   - bare float (3.14, -2.5)
//   - bare boolean (true, false)
func parseLiteral(s string) (literal, error) {
	if s == "" {
		return literal{}, errors.New("missing literal on right side")
	}
	first := s[0]
	if first == '\'' || first == '"' {
		if len(s) < 2 || s[len(s)-1] != first {
			return literal{}, fmt.Errorf("unterminated string literal: %q", s)
		}
		return literal{kind: litString, str: s[1 : len(s)-1]}, nil
	}
	switch s {
	case "true":
		return literal{kind: litBool, b: true}, nil
	case "false":
		return literal{kind: litBool, b: false}, nil
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return literal{kind: litInt, i: i}, nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return literal{kind: litFloat, f: f}, nil
	}
	return literal{}, fmt.Errorf("unrecognized literal %q (expected quoted string, number, or boolean)", s)
}

// eval applies the parsed expression against an outputs map.
func (e *matchExpr) eval(outputs map[string]any) (bool, error) {
	val, present := outputs[e.field]
	if !present {
		return false, nil
	}

	switch e.op {
	case "==":
		return compareEqual(e.field, val, e.lit, "==")
	case "!=":
		eq, err := compareEqual(e.field, val, e.lit, "!=")
		if err != nil {
			return false, err
		}
		return !eq, nil
	case "contains":
		return evalContains(e.field, val, e.lit)
	}
	return false, &ParseError{Expr: e.raw, Reason: fmt.Sprintf("unsupported operator %q", e.op)}
}

// compareEqual implements the `==` half of `==` / `!=`. The caller
// inverts the result for `!=`. A type mismatch always yields a
// *TypeMismatchError; callers must not collapse that into "false" or
// they'll silently ignore misconfigured formulas.
func compareEqual(field string, val any, lit literal, op string) (bool, error) {
	switch lit.kind {
	case litString:
		s, ok := val.(string)
		if !ok {
			return false, mismatch(field, op, lit.kind, val)
		}
		return s == lit.str, nil

	case litInt:
		if f, ok := numericAsFloat(val); ok {
			return f == float64(lit.i), nil
		}
		return false, mismatch(field, op, lit.kind, val)

	case litFloat:
		if f, ok := numericAsFloat(val); ok {
			return f == lit.f, nil
		}
		return false, mismatch(field, op, lit.kind, val)

	case litBool:
		b, ok := val.(bool)
		if !ok {
			return false, mismatch(field, op, lit.kind, val)
		}
		return b == lit.b, nil
	}
	return false, &ParseError{Reason: fmt.Sprintf("internal: unknown literal kind %d", lit.kind)}
}

// evalContains implements `contains`. The literal must be a string;
// the field must be a string (substring match) or a list of strings
// (element membership). Anything else is a type mismatch.
func evalContains(field string, val any, lit literal) (bool, error) {
	if lit.kind != litString {
		return false, &TypeMismatchError{
			Field:       field,
			Op:          "contains",
			LiteralKind: lit.kind.String(),
			ActualType:  describeType(val),
		}
	}
	switch t := val.(type) {
	case string:
		return strings.Contains(t, lit.str), nil
	case []string:
		for _, s := range t {
			if s == lit.str {
				return true, nil
			}
		}
		return false, nil
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok && s == lit.str {
				return true, nil
			}
		}
		return false, nil
	}
	return false, &TypeMismatchError{
		Field:       field,
		Op:          "contains",
		LiteralKind: "string",
		ActualType:  describeType(val),
	}
}

// numericAsFloat coerces any of Go's standard numeric kinds to float64
// so we can compare TOML's int64-decoded values against JSON's
// float64-decoded values without a separate code path per encoder.
func numericAsFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

// describeType renders an outputs value's runtime type for diagnostics.
func describeType(v any) string {
	if v == nil {
		return "nil"
	}
	return fmt.Sprintf("%T", v)
}

// mismatch builds a *TypeMismatchError for == / != type clashes.
func mismatch(field, op string, expected litKind, val any) *TypeMismatchError {
	return &TypeMismatchError{
		Field:       field,
		Op:          op,
		LiteralKind: expected.String(),
		ActualType:  describeType(val),
	}
}
