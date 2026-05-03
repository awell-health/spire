package formula

import (
	"strings"
	"testing"
)

// TestValidateLifecycle_CleanFormula covers case (a): every status named as a
// transition target is also declared as on_start of some step. No warnings.
func TestValidateLifecycle_CleanFormula(t *testing.T) {
	f := &FormulaStepGraph{
		Name: "clean",
		Steps: map[string]StepConfig{
			"implement": {
				Lifecycle: &LifecycleConfig{
					OnStart:    "in_progress",
					OnComplete: "awaiting_review",
				},
			},
			"review": {
				Lifecycle: &LifecycleConfig{
					OnStart: "awaiting_review",
					OnCompleteMatch: []MatchClause{
						{When: "outputs.verdict == 'approve'", Status: "merge_pending"},
						{When: "outputs.verdict == 'request_changes'", Status: "needs_changes"},
					},
				},
			},
			"fix": {
				Lifecycle: &LifecycleConfig{
					OnStart:    "needs_changes",
					OnComplete: "awaiting_review",
				},
			},
			"merge": {
				Lifecycle: &LifecycleConfig{
					OnStart: "merge_pending",
				},
			},
		},
	}
	warnings := validateLifecycle(f)
	if len(warnings) != 0 {
		t.Fatalf("expected 0 warnings, got %d: %+v", len(warnings), warnings)
	}
}

// TestValidateLifecycle_OrphanedStatusWarns covers case (b): a status named as
// a transition target but never as on_start of any step, and not in the
// terminal allowlist. validateLifecycle must surface a warning for it.
func TestValidateLifecycle_OrphanedStatusWarns(t *testing.T) {
	f := &FormulaStepGraph{
		Name: "orphan",
		Steps: map[string]StepConfig{
			"implement": {
				Lifecycle: &LifecycleConfig{
					OnStart:    "in_progress",
					OnComplete: "totally_made_up", // orphan: no step starts here, not allowlisted
				},
			},
			"close": {
				Lifecycle: &LifecycleConfig{
					OnStart: "in_progress",
				},
			},
		},
	}
	warnings := validateLifecycle(f)
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %+v", len(warnings), warnings)
	}
	w := warnings[0]
	if w.Formula != "orphan" {
		t.Errorf("Formula = %q, want %q", w.Formula, "orphan")
	}
	if w.Step != "implement" {
		t.Errorf("Step = %q, want %q", w.Step, "implement")
	}
	if w.Status != "totally_made_up" {
		t.Errorf("Status = %q, want %q", w.Status, "totally_made_up")
	}
	msg := w.String()
	for _, want := range []string{`"orphan"`, `"totally_made_up"`, `"implement"`, "never reached as on_start", "legitimate if terminal"} {
		if !strings.Contains(msg, want) {
			t.Errorf("warning string missing %q: %s", want, msg)
		}
	}
}

// TestValidateLifecycle_AllowlistedTerminalNoWarn covers case (c): when a
// terminal status (`closed`) is named as a target but never as on_start, the
// allowlist suppresses the warning.
func TestValidateLifecycle_AllowlistedTerminalNoWarn(t *testing.T) {
	f := &FormulaStepGraph{
		Name: "terminal-closed",
		Steps: map[string]StepConfig{
			"implement": {
				Lifecycle: &LifecycleConfig{
					OnStart:    "in_progress",
					OnComplete: "closed", // orphan but allowlisted
				},
			},
		},
	}
	warnings := validateLifecycle(f)
	if len(warnings) != 0 {
		t.Fatalf("expected 0 warnings (closed is allowlisted), got %d: %+v", len(warnings), warnings)
	}
}

// Sanity check: awaiting_human is also allowlisted, since the allowlist is
// the public contract of which terminals are known-legitimate.
func TestValidateLifecycle_AwaitingHumanAllowlisted(t *testing.T) {
	f := &FormulaStepGraph{
		Name: "terminal-human",
		Steps: map[string]StepConfig{
			"review": {
				Lifecycle: &LifecycleConfig{
					OnStart: "awaiting_review",
					OnCompleteMatch: []MatchClause{
						{When: "outputs.verdict == 'escalate'", Status: "awaiting_human"},
					},
				},
			},
		},
	}
	warnings := validateLifecycle(f)
	if len(warnings) != 0 {
		t.Fatalf("expected 0 warnings (awaiting_human is allowlisted), got %d: %+v", len(warnings), warnings)
	}
}

// Multiple orphan targets in the same step's match clauses should each surface
// a warning, sorted deterministically by step name.
func TestValidateLifecycle_MultipleOrphansAcrossMatchClauses(t *testing.T) {
	f := &FormulaStepGraph{
		Name: "multi",
		Steps: map[string]StepConfig{
			"review": {
				Lifecycle: &LifecycleConfig{
					OnStart: "awaiting_review",
					OnCompleteMatch: []MatchClause{
						{When: "outputs.verdict == 'approve'", Status: "ghost_one"},
						{When: "outputs.verdict == 'request_changes'", Status: "ghost_two"},
						{When: "outputs.verdict == 'escalate'", Status: "closed"}, // allowlisted, no warn
					},
				},
			},
		},
	}
	warnings := validateLifecycle(f)
	if len(warnings) != 2 {
		t.Fatalf("expected 2 warnings, got %d: %+v", len(warnings), warnings)
	}
	gotStatuses := map[string]bool{warnings[0].Status: true, warnings[1].Status: true}
	for _, want := range []string{"ghost_one", "ghost_two"} {
		if !gotStatuses[want] {
			t.Errorf("missing warning for status %q; got %+v", want, warnings)
		}
	}
}

// Steps with no lifecycle block are ignored entirely — they cannot contribute
// targets or on_start values.
func TestValidateLifecycle_NoLifecycleBlocks(t *testing.T) {
	f := &FormulaStepGraph{
		Name: "no-lifecycle",
		Steps: map[string]StepConfig{
			"implement": {Title: "Implement"},
			"close":     {Title: "Close"},
		},
	}
	warnings := validateLifecycle(f)
	if len(warnings) != 0 {
		t.Fatalf("expected 0 warnings, got %d: %+v", len(warnings), warnings)
	}
}

// nil and empty inputs must not panic.
func TestValidateLifecycle_NilAndEmpty(t *testing.T) {
	if w := validateLifecycle(nil); w != nil {
		t.Errorf("nil formula: expected nil warnings, got %+v", w)
	}
	if w := validateLifecycle(&FormulaStepGraph{Name: "empty"}); w != nil {
		t.Errorf("empty formula: expected nil warnings, got %+v", w)
	}
}

// Round-trip through the parser to verify the wiring in ParseFormulaStepGraph
// runs validateLifecycle without altering parse semantics. We can't easily
// capture log output here, so this just confirms parsing still succeeds with
// an orphaned target in a lifecycle block.
func TestParseFormulaStepGraph_OrphanLifecycleStillParses(t *testing.T) {
	raw := `
name = "orphan-parse"
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
on_complete = "phantom_status"

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
	if f == nil {
		t.Fatal("parsed formula is nil")
	}
	// Validate the orphan is still detectable post-parse.
	warnings := validateLifecycle(f)
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning after parse, got %d: %+v", len(warnings), warnings)
	}
	if warnings[0].Status != "phantom_status" {
		t.Errorf("Status = %q, want phantom_status", warnings[0].Status)
	}
}
