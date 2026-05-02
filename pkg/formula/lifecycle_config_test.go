package formula

import (
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml/v2"
)

// --- Step lifecycle TOML parsing ---

func TestParseStep_LifecycleOnStartCompleteFail(t *testing.T) {
	raw := `
name = "lifecycle-test"
version = 3
entry = "implement"

[vars.bead_id]
type = "bead_id"
required = true

[steps.implement]
kind = "op"
action = "wizard.run"
flow = "implement"
title = "Implement"

[steps.implement.lifecycle]
on_start = "in_progress"
on_complete = "awaiting_review"

[steps.implement.lifecycle.on_fail]
status = "needs_changes"
event = "Escalated"

[steps.close]
kind = "op"
action = "bead.finish"
needs = ["implement"]
terminal = true
title = "Close"
`
	f, err := ParseFormulaStepGraph([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	step, ok := f.Steps["implement"]
	if !ok {
		t.Fatal("missing step implement")
	}
	if step.Lifecycle == nil {
		t.Fatal("expected Lifecycle to be set")
	}
	if step.Lifecycle.OnStart != "in_progress" {
		t.Errorf("OnStart = %q, want in_progress", step.Lifecycle.OnStart)
	}
	if step.Lifecycle.OnComplete != "awaiting_review" {
		t.Errorf("OnComplete = %q, want awaiting_review", step.Lifecycle.OnComplete)
	}
	if step.Lifecycle.OnFail == nil {
		t.Fatal("expected OnFail to be set")
	}
	if step.Lifecycle.OnFail.Status != "needs_changes" {
		t.Errorf("OnFail.Status = %q, want needs_changes", step.Lifecycle.OnFail.Status)
	}
	if step.Lifecycle.OnFail.Event != "Escalated" {
		t.Errorf("OnFail.Event = %q, want Escalated", step.Lifecycle.OnFail.Event)
	}
	if len(step.Lifecycle.OnCompleteMatch) != 0 {
		t.Errorf("expected no OnCompleteMatch, got %d", len(step.Lifecycle.OnCompleteMatch))
	}
}

func TestParseStep_LifecycleOnCompleteMatch(t *testing.T) {
	raw := `
name = "lifecycle-match-test"
version = 3
entry = "review"

[vars.bead_id]
type = "bead_id"
required = true

[steps.review]
kind = "op"
action = "wizard.run"
flow = "review"
title = "Review"

[[steps.review.lifecycle.on_complete_match]]
when = "outputs.verdict == 'approve'"
status = "merge_pending"

[[steps.review.lifecycle.on_complete_match]]
when = "outputs.verdict == 'request_changes'"
status = "needs_changes"

[steps.close]
kind = "op"
action = "bead.finish"
needs = ["review"]
terminal = true
title = "Close"
`
	f, err := ParseFormulaStepGraph([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	step := f.Steps["review"]
	if step.Lifecycle == nil {
		t.Fatal("expected Lifecycle to be set")
	}
	if len(step.Lifecycle.OnCompleteMatch) != 2 {
		t.Fatalf("OnCompleteMatch len = %d, want 2", len(step.Lifecycle.OnCompleteMatch))
	}
	first := step.Lifecycle.OnCompleteMatch[0]
	if first.When != "outputs.verdict == 'approve'" {
		t.Errorf("first.When = %q", first.When)
	}
	if first.Status != "merge_pending" {
		t.Errorf("first.Status = %q, want merge_pending", first.Status)
	}
	second := step.Lifecycle.OnCompleteMatch[1]
	if second.When != "outputs.verdict == 'request_changes'" {
		t.Errorf("second.When = %q", second.When)
	}
	if second.Status != "needs_changes" {
		t.Errorf("second.Status = %q, want needs_changes", second.Status)
	}
}

func TestParseStep_LegacyNoLifecycle(t *testing.T) {
	// A formula step with no lifecycle block should still parse and
	// leave Lifecycle nil (backwards compat).
	raw := `
name = "legacy-test"
version = 3
entry = "implement"

[vars.bead_id]
type = "bead_id"
required = true

[steps.implement]
kind = "op"
action = "wizard.run"
flow = "implement"
title = "Implement"

[steps.close]
kind = "op"
action = "bead.finish"
needs = ["implement"]
terminal = true
title = "Close"
`
	f, err := ParseFormulaStepGraph([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	step := f.Steps["implement"]
	if step.Lifecycle != nil {
		t.Errorf("expected Lifecycle to be nil for legacy step, got %+v", step.Lifecycle)
	}
}

// Direct TOML round-trip on StepConfig (independent of graph validation)
// to confirm the toml tag is wired correctly.
func TestStepConfig_LifecycleTomlRoundtrip(t *testing.T) {
	raw := `
kind = "op"
action = "wizard.run"

[lifecycle]
on_start = "in_progress"
on_complete = "awaiting_review"

[lifecycle.on_fail]
status = "needs_changes"
`
	var step StepConfig
	if err := toml.Unmarshal([]byte(raw), &step); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if step.Lifecycle == nil {
		t.Fatal("Lifecycle nil")
	}
	if step.Lifecycle.OnStart != "in_progress" {
		t.Errorf("OnStart = %q", step.Lifecycle.OnStart)
	}
	if step.Lifecycle.OnFail == nil || step.Lifecycle.OnFail.Status != "needs_changes" {
		t.Errorf("OnFail.Status mismatch: %+v", step.Lifecycle.OnFail)
	}
}

// --- EvalWhen ---

func TestEvalWhen(t *testing.T) {
	tests := []struct {
		name    string
		expr    string
		outputs map[string]any
		want    bool
		wantErr bool
	}{
		// Equality.
		{
			name:    "eq match",
			expr:    "outputs.verdict == 'approve'",
			outputs: map[string]any{"verdict": "approve"},
			want:    true,
		},
		{
			name:    "eq no match",
			expr:    "outputs.verdict == 'approve'",
			outputs: map[string]any{"verdict": "request_changes"},
			want:    false,
		},
		{
			name:    "eq missing field",
			expr:    "outputs.verdict == 'approve'",
			outputs: map[string]any{},
			want:    false,
		},
		{
			name:    "eq double-quoted literal",
			expr:    `outputs.verdict == "approve"`,
			outputs: map[string]any{"verdict": "approve"},
			want:    true,
		},

		// Inequality.
		{
			name:    "ne match",
			expr:    "outputs.verdict != 'request_changes'",
			outputs: map[string]any{"verdict": "approve"},
			want:    true,
		},
		{
			name:    "ne no match",
			expr:    "outputs.verdict != 'request_changes'",
			outputs: map[string]any{"verdict": "request_changes"},
			want:    false,
		},

		// Contains over a string slice.
		{
			name:    "contains slice match",
			expr:    "outputs.tags contains 'flake'",
			outputs: map[string]any{"tags": []string{"build", "flake"}},
			want:    true,
		},
		{
			name:    "contains slice no match",
			expr:    "outputs.tags contains 'flake'",
			outputs: map[string]any{"tags": []string{"build", "race"}},
			want:    false,
		},
		// Contains over a []any (toml decodes string arrays as []any).
		{
			name:    "contains []any match",
			expr:    "outputs.tags contains 'flake'",
			outputs: map[string]any{"tags": []any{"build", "flake"}},
			want:    true,
		},
		// Contains over a plain string (substring fallback).
		{
			name:    "contains string substring match",
			expr:    "outputs.tags contains 'flake'",
			outputs: map[string]any{"tags": "build,flake,race"},
			want:    true,
		},
		{
			name:    "contains missing field",
			expr:    "outputs.tags contains 'flake'",
			outputs: map[string]any{},
			want:    false,
		},

		// Errors.
		{
			name:    "missing outputs prefix",
			expr:    "verdict == 'approve'",
			outputs: map[string]any{"verdict": "approve"},
			wantErr: true,
		},
		{
			name:    "unsupported operator",
			expr:    "outputs.verdict ~ 'approve'",
			outputs: map[string]any{"verdict": "approve"},
			wantErr: true,
		},
		{
			name:    "unparseable expression",
			expr:    "outputs.verdict approve",
			outputs: map[string]any{"verdict": "approve"},
			wantErr: true,
		},
		{
			name:    "empty expression",
			expr:    "",
			outputs: map[string]any{},
			wantErr: true,
		},
		{
			name:    "unquoted literal",
			expr:    "outputs.verdict == approve",
			outputs: map[string]any{"verdict": "approve"},
			wantErr: true,
		},
		{
			name:    "mismatched quotes",
			expr:    `outputs.verdict == 'approve"`,
			outputs: map[string]any{"verdict": "approve"},
			wantErr: true,
		},
		{
			name:    "empty outputs field",
			expr:    "outputs. == 'approve'",
			outputs: map[string]any{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := EvalWhen(tt.expr, tt.outputs)
			if (err != nil) != tt.wantErr {
				t.Fatalf("EvalWhen() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Fatalf("EvalWhen() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEvalWhen_ErrorMessage_MissingPrefix(t *testing.T) {
	_, err := EvalWhen("foo == 'bar'", map[string]any{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "outputs.") {
		t.Errorf("error should mention outputs prefix: %v", err)
	}
}

// --- Embedded formula smoke test ---

// TestEmbeddedFormulas_LoadWithLifecycleParser verifies the existing
// embedded formulas (which today have no lifecycle blocks) still parse
// cleanly after the schema is extended. Landing 3 will start populating
// these blocks; until then, every Lifecycle pointer should be nil.
func TestEmbeddedFormulas_LoadWithLifecycleParser(t *testing.T) {
	names := []string{
		"task-default",
		"bug-default",
		"epic-default",
		"chore-default",
		"cleric-default",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			f, err := LoadEmbeddedStepGraph(name)
			if err != nil {
				t.Fatalf("load %s: %v", name, err)
			}
			for stepName, step := range f.Steps {
				if step.Lifecycle != nil {
					// Not an error — Landing 3 is allowed to add these later.
					// We only require that parse succeeds. Log for visibility.
					t.Logf("%s.%s has Lifecycle populated (Landing 3 progress)", name, stepName)
				}
			}
		})
	}
}
