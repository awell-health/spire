package formula

import (
	"errors"
	"strings"
	"testing"
)

// TestEval_Operators_StringLiteral covers ==, !=, contains with single
// and double-quoted string literals.
func TestEval_Operators_StringLiteral(t *testing.T) {
	tests := []struct {
		name    string
		expr    string
		outputs map[string]any
		want    bool
	}{
		{"eq single-quoted match", "outputs.verdict == 'approve'", map[string]any{"verdict": "approve"}, true},
		{"eq single-quoted no match", "outputs.verdict == 'approve'", map[string]any{"verdict": "deny"}, false},
		{"eq double-quoted match", `outputs.verdict == "approve"`, map[string]any{"verdict": "approve"}, true},
		{"eq empty string match", "outputs.note == ''", map[string]any{"note": ""}, true},
		{"eq empty string no match", "outputs.note == ''", map[string]any{"note": "hi"}, false},

		{"ne single-quoted match", "outputs.verdict != 'request_changes'", map[string]any{"verdict": "approve"}, true},
		{"ne single-quoted no match", "outputs.verdict != 'request_changes'", map[string]any{"verdict": "request_changes"}, false},
		{"ne double-quoted match", `outputs.verdict != "request_changes"`, map[string]any{"verdict": "approve"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Eval(tt.expr, tt.outputs)
			if err != nil {
				t.Fatalf("Eval(%q) returned unexpected error: %v", tt.expr, err)
			}
			if got != tt.want {
				t.Errorf("Eval(%q) = %v, want %v", tt.expr, got, tt.want)
			}
		})
	}
}

// TestEval_Operators_IntLiteral covers ==, != with bare integer literals.
// Numeric values may arrive as int or int64 (Go literals, TOML decode) or
// float64 (JSON decode); all should compare equal to the same numeric
// literal.
func TestEval_Operators_IntLiteral(t *testing.T) {
	tests := []struct {
		name    string
		expr    string
		outputs map[string]any
		want    bool
	}{
		{"eq int match (int)", "outputs.count == 42", map[string]any{"count": 42}, true},
		{"eq int match (int64)", "outputs.count == 42", map[string]any{"count": int64(42)}, true},
		{"eq int match (float64 from JSON)", "outputs.count == 42", map[string]any{"count": float64(42)}, true},
		{"eq int no match", "outputs.count == 42", map[string]any{"count": 7}, false},
		{"eq negative int match", "outputs.delta == -5", map[string]any{"delta": -5}, true},
		{"eq zero match", "outputs.count == 0", map[string]any{"count": 0}, true},

		{"ne int match", "outputs.count != 42", map[string]any{"count": 7}, true},
		{"ne int no match", "outputs.count != 42", map[string]any{"count": 42}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Eval(tt.expr, tt.outputs)
			if err != nil {
				t.Fatalf("Eval(%q) returned unexpected error: %v", tt.expr, err)
			}
			if got != tt.want {
				t.Errorf("Eval(%q) = %v, want %v", tt.expr, got, tt.want)
			}
		})
	}
}

// TestEval_Operators_FloatLiteral covers ==, != with bare float literals.
func TestEval_Operators_FloatLiteral(t *testing.T) {
	tests := []struct {
		name    string
		expr    string
		outputs map[string]any
		want    bool
	}{
		{"eq float match (float64)", "outputs.score == 3.14", map[string]any{"score": 3.14}, true},
		{"eq float match (float32)", "outputs.score == 3.5", map[string]any{"score": float32(3.5)}, true},
		{"eq float no match", "outputs.score == 3.14", map[string]any{"score": 2.71}, false},
		{"eq negative float match", "outputs.delta == -2.5", map[string]any{"delta": -2.5}, true},
		{"eq float vs int field (3.0 == 3)", "outputs.count == 3.0", map[string]any{"count": 3}, true},

		{"ne float match", "outputs.score != 3.14", map[string]any{"score": 2.71}, true},
		{"ne float no match", "outputs.score != 3.14", map[string]any{"score": 3.14}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Eval(tt.expr, tt.outputs)
			if err != nil {
				t.Fatalf("Eval(%q) returned unexpected error: %v", tt.expr, err)
			}
			if got != tt.want {
				t.Errorf("Eval(%q) = %v, want %v", tt.expr, got, tt.want)
			}
		})
	}
}

// TestEval_Operators_BoolLiteral covers ==, != with bare boolean literals.
func TestEval_Operators_BoolLiteral(t *testing.T) {
	tests := []struct {
		name    string
		expr    string
		outputs map[string]any
		want    bool
	}{
		{"eq true match", "outputs.passed == true", map[string]any{"passed": true}, true},
		{"eq true no match", "outputs.passed == true", map[string]any{"passed": false}, false},
		{"eq false match", "outputs.passed == false", map[string]any{"passed": false}, true},
		{"eq false no match", "outputs.passed == false", map[string]any{"passed": true}, false},

		{"ne true match", "outputs.passed != true", map[string]any{"passed": false}, true},
		{"ne true no match", "outputs.passed != true", map[string]any{"passed": true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Eval(tt.expr, tt.outputs)
			if err != nil {
				t.Fatalf("Eval(%q) returned unexpected error: %v", tt.expr, err)
			}
			if got != tt.want {
				t.Errorf("Eval(%q) = %v, want %v", tt.expr, got, tt.want)
			}
		})
	}
}

// TestEval_Contains covers `contains` against strings (substring) and
// slices (membership). The literal must be a string.
func TestEval_Contains(t *testing.T) {
	tests := []struct {
		name    string
		expr    string
		outputs map[string]any
		want    bool
	}{
		// String substring.
		{"contains substring match", "outputs.summary contains 'flake'", map[string]any{"summary": "build,flake,race"}, true},
		{"contains substring no match", "outputs.summary contains 'flake'", map[string]any{"summary": "build,race"}, false},
		{"contains substring exact", "outputs.summary contains 'foo'", map[string]any{"summary": "foo"}, true},
		{"contains empty literal", "outputs.summary contains ''", map[string]any{"summary": "anything"}, true},

		// []string membership.
		{"contains []string match", "outputs.tags contains 'flake'", map[string]any{"tags": []string{"build", "flake"}}, true},
		{"contains []string no match", "outputs.tags contains 'flake'", map[string]any{"tags": []string{"build", "race"}}, false},

		// []any membership (TOML decodes string arrays this way).
		{"contains []any match", "outputs.tags contains 'flake'", map[string]any{"tags": []any{"build", "flake"}}, true},
		{"contains []any no match", "outputs.tags contains 'flake'", map[string]any{"tags": []any{"build", "race"}}, false},
		{"contains []any with non-string elements", "outputs.tags contains 'flake'", map[string]any{"tags": []any{42, "flake"}}, true},
		{"contains []any all non-string", "outputs.tags contains 'flake'", map[string]any{"tags": []any{42, true}}, false},

		// Empty slices.
		{"contains empty []string", "outputs.tags contains 'flake'", map[string]any{"tags": []string{}}, false},
		{"contains empty []any", "outputs.tags contains 'flake'", map[string]any{"tags": []any{}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Eval(tt.expr, tt.outputs)
			if err != nil {
				t.Fatalf("Eval(%q) returned unexpected error: %v", tt.expr, err)
			}
			if got != tt.want {
				t.Errorf("Eval(%q) = %v, want %v", tt.expr, got, tt.want)
			}
		})
	}
}

// TestEval_MissingField verifies that a field absent from outputs always
// returns (false, nil) regardless of operator.
func TestEval_MissingField(t *testing.T) {
	tests := []struct {
		name string
		expr string
	}{
		{"eq missing", "outputs.verdict == 'approve'"},
		{"ne missing", "outputs.verdict != 'approve'"},
		{"contains missing", "outputs.tags contains 'flake'"},
		{"eq int missing", "outputs.count == 42"},
		{"eq bool missing", "outputs.passed == true"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Eval(tt.expr, map[string]any{})
			if err != nil {
				t.Fatalf("Eval(%q) on empty outputs returned error: %v", tt.expr, err)
			}
			if got {
				t.Errorf("Eval(%q) on empty outputs = true, want false", tt.expr)
			}
		})
	}
}

// TestEval_TypeMismatch verifies that comparing incompatible types
// returns a *TypeMismatchError that callers can identify via errors.As.
func TestEval_TypeMismatch(t *testing.T) {
	tests := []struct {
		name        string
		expr        string
		outputs     map[string]any
		wantOp      string
		wantLitKind string
	}{
		{"string lit vs int field", "outputs.count == 'foo'", map[string]any{"count": 42}, "==", "string"},
		{"string lit vs bool field", "outputs.flag != 'foo'", map[string]any{"flag": true}, "!=", "string"},
		{"int lit vs string field", "outputs.name == 42", map[string]any{"name": "alice"}, "==", "int"},
		{"int lit vs bool field", "outputs.flag == 1", map[string]any{"flag": true}, "==", "int"},
		{"float lit vs string field", "outputs.name == 3.14", map[string]any{"name": "alice"}, "==", "float"},
		{"float lit vs bool field", "outputs.flag == 3.14", map[string]any{"flag": false}, "==", "float"},
		{"bool lit vs string field", "outputs.name == true", map[string]any{"name": "alice"}, "==", "bool"},
		{"bool lit vs int field", "outputs.count == false", map[string]any{"count": 0}, "==", "bool"},

		// contains: string literal required; field must be string or []string/[]any.
		{"contains against int field", "outputs.count contains 'foo'", map[string]any{"count": 42}, "contains", "string"},
		{"contains against bool field", "outputs.flag contains 'foo'", map[string]any{"flag": true}, "contains", "string"},
		{"contains against map field", "outputs.meta contains 'foo'", map[string]any{"meta": map[string]any{"a": 1}}, "contains", "string"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Eval(tt.expr, tt.outputs)
			if err == nil {
				t.Fatalf("Eval(%q) returned no error; want *TypeMismatchError", tt.expr)
			}
			if got {
				t.Errorf("Eval(%q) = true on type mismatch; want false", tt.expr)
			}
			var tme *TypeMismatchError
			if !errors.As(err, &tme) {
				t.Fatalf("Eval(%q) returned %T %v; want *TypeMismatchError", tt.expr, err, err)
			}
			if tme.Op != tt.wantOp {
				t.Errorf("TypeMismatchError.Op = %q, want %q", tme.Op, tt.wantOp)
			}
			if tme.LiteralKind != tt.wantLitKind {
				t.Errorf("TypeMismatchError.LiteralKind = %q, want %q", tme.LiteralKind, tt.wantLitKind)
			}
			// Smoke test the message format.
			if !strings.Contains(tme.Error(), "type mismatch") {
				t.Errorf("TypeMismatchError message should mention 'type mismatch': %q", tme.Error())
			}
		})
	}
}

// TestEval_ParseError covers malformed expressions. All return a
// *ParseError that callers can identify via errors.As.
func TestEval_ParseError(t *testing.T) {
	tests := []struct {
		name     string
		expr     string
		needSubstr string
	}{
		{"empty expression", "", "empty expression"},
		{"whitespace only", "   ", "empty expression"},
		{"missing outputs prefix", "verdict == 'approve'", "outputs."},
		{"empty field", "outputs. == 'approve'", "empty field"},
		{"nested field path", "outputs.foo.bar == 'baz'", "nested field"},
		{"invalid field char", "outputs.foo$bar == 'baz'", "invalid field name"},
		{"unsupported operator", "outputs.verdict ~ 'approve'", "no operator"},
		{"no operator at all", "outputs.verdict approve", "no operator"},
		{"unquoted string-ish literal", "outputs.verdict == approve", "unrecognized literal"},
		{"unterminated single quote", "outputs.verdict == 'approve", "unterminated string"},
		{"unterminated double quote", `outputs.verdict == "approve`, "unterminated string"},
		{"mismatched quotes", `outputs.verdict == 'approve"`, "unterminated string"},
		{"missing rhs", "outputs.verdict == ", "no operator"}, // " == " trailing is consumed by split, RHS empty.
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Eval(tt.expr, map[string]any{"verdict": "approve"})
			if err == nil {
				t.Fatalf("Eval(%q) returned no error; want *ParseError", tt.expr)
			}
			if got {
				t.Errorf("Eval(%q) = true on parse error; want false", tt.expr)
			}
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("Eval(%q) returned %T %v; want *ParseError", tt.expr, err, err)
			}
			if tt.needSubstr != "" && !strings.Contains(pe.Reason, tt.needSubstr) && !strings.Contains(pe.Error(), tt.needSubstr) {
				t.Errorf("ParseError message %q does not contain expected substring %q", pe.Error(), tt.needSubstr)
			}
		})
	}
}

// TestEval_FieldNameVariants covers the legal characters in field names.
func TestEval_FieldNameVariants(t *testing.T) {
	tests := []struct {
		name    string
		expr    string
		outputs map[string]any
		want    bool
	}{
		{"underscore", "outputs.has_review == true", map[string]any{"has_review": true}, true},
		{"hyphen", "outputs.review-state == 'ok'", map[string]any{"review-state": "ok"}, true},
		{"digits in field", "outputs.attempt2 == 1", map[string]any{"attempt2": 1}, true},
		{"all uppercase", "outputs.STATUS == 'OK'", map[string]any{"STATUS": "OK"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Eval(tt.expr, tt.outputs)
			if err != nil {
				t.Fatalf("Eval(%q) returned unexpected error: %v", tt.expr, err)
			}
			if got != tt.want {
				t.Errorf("Eval(%q) = %v, want %v", tt.expr, got, tt.want)
			}
		})
	}
}

// TestEval_NilFieldValue verifies that a field set to nil is treated as
// a type mismatch rather than crashing or silently matching.
func TestEval_NilFieldValue(t *testing.T) {
	_, err := Eval("outputs.verdict == 'approve'", map[string]any{"verdict": nil})
	if err == nil {
		t.Fatal("Eval against nil-valued field returned no error; want *TypeMismatchError")
	}
	var tme *TypeMismatchError
	if !errors.As(err, &tme) {
		t.Fatalf("Eval against nil-valued field returned %T; want *TypeMismatchError", err)
	}
	if tme.ActualType != "nil" {
		t.Errorf("TypeMismatchError.ActualType = %q, want %q", tme.ActualType, "nil")
	}
}

// TestEvalWhen_BackwardCompatAlias verifies that EvalWhen still routes
// through Eval, preserving the symbol the lifecycle evaluator imports.
func TestEvalWhen_BackwardCompatAlias(t *testing.T) {
	got, err := EvalWhen("outputs.verdict == 'approve'", map[string]any{"verdict": "approve"})
	if err != nil {
		t.Fatalf("EvalWhen returned error: %v", err)
	}
	if !got {
		t.Error("EvalWhen returned false for matching expression")
	}
}

// TestParseError_EmptyExpr verifies that the ParseError formatter
// handles the empty-expr case (where we'd otherwise print %q "").
func TestParseError_EmptyExpr(t *testing.T) {
	pe := &ParseError{Reason: "boom"}
	if msg := pe.Error(); !strings.Contains(msg, "boom") {
		t.Errorf("ParseError.Error() = %q, want substring %q", msg, "boom")
	}
}

// TestEval_FirstClauseWinsIsolation is a smoke test that demonstrates
// the documented "first matching clause wins" pattern lifecycle uses:
// each clause is evaluated in turn and the first true result is taken.
func TestEval_FirstClauseWinsIsolation(t *testing.T) {
	outputs := map[string]any{"verdict": "approve"}

	clauses := []struct{ when, status string }{
		{"outputs.verdict == 'request_changes'", "needs_changes"},
		{"outputs.verdict == 'approve'", "merge_pending"},
		{"outputs.verdict == 'approve'", "should_never_pick_this"},
	}

	var picked string
	for _, c := range clauses {
		ok, err := Eval(c.when, outputs)
		if err != nil {
			t.Fatalf("Eval(%q): %v", c.when, err)
		}
		if ok {
			picked = c.status
			break
		}
	}
	if picked != "merge_pending" {
		t.Errorf("first matching clause = %q, want merge_pending", picked)
	}
}
