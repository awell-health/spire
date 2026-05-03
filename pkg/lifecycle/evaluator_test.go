package lifecycle

import (
	"testing"

	"github.com/awell-health/spire/pkg/formula"
)

// evaluatorLegalStatuses enumerates every status the evaluator may legally
// emit today. Mirrors legalStatusSet in evaluator.go; kept in sync by
// TestLegalStatusSetMatchesTestSet so divergence is caught loudly.
var evaluatorLegalStatuses = []string{
	"open",
	"ready",
	"dispatched",
	"in_progress",
	"blocked",
	"deferred",
	"awaiting_review",
	"needs_changes",
	"awaiting_human",
	"merge_pending",
	"closed",
}

// TestLegalStatusSetMatchesTestSet pins the evaluatorLegalStatuses slice
// to the legalStatusSet map so the table-driven coverage below cannot
// silently fall out of sync with the production gate.
func TestLegalStatusSetMatchesTestSet(t *testing.T) {
	if got, want := len(legalStatusSet), len(evaluatorLegalStatuses); got != want {
		t.Fatalf("legalStatusSet size = %d, evaluatorLegalStatuses size = %d", got, want)
	}
	for _, s := range evaluatorLegalStatuses {
		if _, ok := legalStatusSet[s]; !ok {
			t.Errorf("status %q in test set but missing from legalStatusSet", s)
		}
	}
}

// TestApplyEvent_CoreEvents covers every core event from every legal
// status (acceptance criterion).
func TestApplyEvent_CoreEvents(t *testing.T) {
	uniform := func(next string) map[string]string {
		out := make(map[string]string, len(evaluatorLegalStatuses))
		for _, s := range evaluatorLegalStatuses {
			out[s] = next
		}
		return out
	}
	identity := func() map[string]string {
		out := make(map[string]string, len(evaluatorLegalStatuses))
		for _, s := range evaluatorLegalStatuses {
			out[s] = s
		}
		return out
	}

	// Per-event expected outcome keyed by current status. A missing
	// entry means we expect ApplyEvent to error from that source.
	cases := []struct {
		name  string
		event Event
		want  map[string]string
	}{
		{name: "Filed", event: Filed{}, want: uniform("open")},
		{name: "ReadyToWork", event: ReadyToWork{}, want: uniform("ready")},
		{
			name:  "WizardClaimed",
			event: WizardClaimed{},
			want: map[string]string{
				"open":            "in_progress",
				"ready":           "in_progress",
				"dispatched":      "dispatched",
				"in_progress":     "in_progress",
				"blocked":         "blocked",
				"deferred":        "deferred",
				"awaiting_review": "awaiting_review",
				"needs_changes":   "needs_changes",
				"awaiting_human":  "awaiting_human",
				"merge_pending":   "merge_pending",
				"closed":          "closed",
			},
		},
		{name: "Escalated", event: Escalated{}, want: identity()},
		{name: "Closed", event: Closed{}, want: uniform("closed")},
	}

	for _, tc := range cases {
		for _, current := range evaluatorLegalStatuses {
			t.Run(tc.name+"_from_"+current, func(t *testing.T) {
				got, err := ApplyEvent(current, tc.event, nil)
				if err != nil {
					t.Fatalf("ApplyEvent err = %v", err)
				}
				if got != tc.want[current] {
					t.Errorf("ApplyEvent = %q, want %q", got, tc.want[current])
				}
			})
		}
	}
}

// TestApplyEvent_LegacyFormulaStepIsNoOp verifies legacy formulas
// (Lifecycle nil) leave currentStatus unchanged — the zero-behavior-
// change contract.
func TestApplyEvent_LegacyFormulaStepIsNoOp(t *testing.T) {
	f := &formula.FormulaStepGraph{
		Steps: map[string]formula.StepConfig{
			"implement": {Lifecycle: nil},
		},
	}
	cases := []Event{
		FormulaStepStarted{Step: "implement"},
		FormulaStepCompleted{Step: "implement", Outputs: map[string]any{"verdict": "approve"}},
		FormulaStepFailed{Step: "implement"},
	}
	for _, ev := range cases {
		got, err := ApplyEvent("in_progress", ev, f)
		if err != nil {
			t.Fatalf("ApplyEvent(%T) err = %v", ev, err)
		}
		if got != "in_progress" {
			t.Errorf("ApplyEvent(%T) = %q, want unchanged in_progress", ev, got)
		}
	}
}

// TestApplyEvent_FormulaStepStarted exercises the OnStart path.
func TestApplyEvent_FormulaStepStarted(t *testing.T) {
	f := &formula.FormulaStepGraph{
		Steps: map[string]formula.StepConfig{
			"implement": {Lifecycle: &formula.LifecycleConfig{OnStart: "in_progress"}},
			"noop":      {Lifecycle: &formula.LifecycleConfig{}}, // OnStart empty
		},
	}

	got, err := ApplyEvent("ready", FormulaStepStarted{Step: "implement"}, f)
	if err != nil {
		t.Fatalf("ApplyEvent err = %v", err)
	}
	if got != "in_progress" {
		t.Errorf("OnStart implement = %q, want in_progress", got)
	}

	got, err = ApplyEvent("ready", FormulaStepStarted{Step: "noop"}, f)
	if err != nil {
		t.Fatalf("ApplyEvent err = %v", err)
	}
	if got != "ready" {
		t.Errorf("OnStart noop should pass through current; got %q, want ready", got)
	}
}

// TestApplyEvent_FormulaStepCompleted_OnComplete covers the simple
// fall-through path (no OnCompleteMatch).
func TestApplyEvent_FormulaStepCompleted_OnComplete(t *testing.T) {
	f := &formula.FormulaStepGraph{
		Steps: map[string]formula.StepConfig{
			"implement": {Lifecycle: &formula.LifecycleConfig{OnComplete: "ready"}},
		},
	}
	got, err := ApplyEvent("in_progress", FormulaStepCompleted{Step: "implement", Outputs: nil}, f)
	if err != nil {
		t.Fatalf("ApplyEvent err = %v", err)
	}
	if got != "ready" {
		t.Errorf("OnComplete = %q, want ready", got)
	}
}

// TestApplyEvent_FormulaStepCompleted_OnCompleteMatch exercises every
// supported operator and falls through to OnComplete when no clause
// matches.
func TestApplyEvent_FormulaStepCompleted_OnCompleteMatch(t *testing.T) {
	f := &formula.FormulaStepGraph{
		Steps: map[string]formula.StepConfig{
			"review": {Lifecycle: &formula.LifecycleConfig{
				OnComplete: "in_progress",
				OnCompleteMatch: []formula.MatchClause{
					{When: "outputs.verdict == 'approve'", Status: "closed"},
					{When: "outputs.verdict != 'request_changes'", Status: "ready"},
					{When: "outputs.tags contains 'flake'", Status: "awaiting_human"},
				},
			}},
		},
	}

	tests := []struct {
		name    string
		outputs map[string]any
		want    string
	}{
		// First clause wins.
		{name: "eq_match_first_clause", outputs: map[string]any{"verdict": "approve"}, want: "closed"},
		// First clause fails (verdict != approve), second succeeds because
		// verdict != request_changes either.
		{name: "ne_match_second_clause", outputs: map[string]any{"verdict": "merged"}, want: "ready"},
		// First two fail; third (contains) wins on a slice.
		{name: "contains_slice_third_clause", outputs: map[string]any{"verdict": "request_changes", "tags": []string{"flake", "build"}}, want: "awaiting_human"},
		// No clause matches — fall back to OnComplete.
		{name: "fallback_on_complete", outputs: map[string]any{"verdict": "request_changes"}, want: "in_progress"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ApplyEvent("in_progress", FormulaStepCompleted{Step: "review", Outputs: tt.outputs}, f)
			if err != nil {
				t.Fatalf("ApplyEvent err = %v", err)
			}
			if got != tt.want {
				t.Errorf("ApplyEvent = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestApplyEvent_OnCompleteMatch_PropagatesEvalErr ensures a malformed
// When expression surfaces as an error rather than a silent skip.
func TestApplyEvent_OnCompleteMatch_PropagatesEvalErr(t *testing.T) {
	f := &formula.FormulaStepGraph{
		Steps: map[string]formula.StepConfig{
			"review": {Lifecycle: &formula.LifecycleConfig{
				OnCompleteMatch: []formula.MatchClause{
					{When: "broken expression", Status: "closed"},
				},
			}},
		},
	}
	_, err := ApplyEvent("in_progress", FormulaStepCompleted{Step: "review", Outputs: nil}, f)
	if err == nil {
		t.Fatal("expected error from malformed When expression")
	}
}

// TestApplyEvent_FormulaStepFailed covers the explicit-status path and
// the OnFail.Event="Escalated" delegation path.
func TestApplyEvent_FormulaStepFailed(t *testing.T) {
	t.Run("explicit_status", func(t *testing.T) {
		f := &formula.FormulaStepGraph{
			Steps: map[string]formula.StepConfig{
				"implement": {Lifecycle: &formula.LifecycleConfig{
					OnFail: &formula.FailAction{Status: "awaiting_human"},
				}},
			},
		}
		got, err := ApplyEvent("in_progress", FormulaStepFailed{Step: "implement"}, f)
		if err != nil {
			t.Fatalf("ApplyEvent err = %v", err)
		}
		if got != "awaiting_human" {
			t.Errorf("ApplyEvent = %q, want awaiting_human", got)
		}
	})

	t.Run("escalated_delegation", func(t *testing.T) {
		f := &formula.FormulaStepGraph{
			Steps: map[string]formula.StepConfig{
				"implement": {Lifecycle: &formula.LifecycleConfig{
					OnFail: &formula.FailAction{Event: "Escalated"},
				}},
			},
		}
		// Escalation today is no-op on status (zero-behavior-change).
		got, err := ApplyEvent("in_progress", FormulaStepFailed{Step: "implement"}, f)
		if err != nil {
			t.Fatalf("ApplyEvent err = %v", err)
		}
		if got != "in_progress" {
			t.Errorf("ApplyEvent = %q, want in_progress (Escalated is status no-op)", got)
		}
	})

	t.Run("status_takes_priority_over_event", func(t *testing.T) {
		f := &formula.FormulaStepGraph{
			Steps: map[string]formula.StepConfig{
				"implement": {Lifecycle: &formula.LifecycleConfig{
					OnFail: &formula.FailAction{Status: "awaiting_human", Event: "Escalated"},
				}},
			},
		}
		got, err := ApplyEvent("in_progress", FormulaStepFailed{Step: "implement"}, f)
		if err != nil {
			t.Fatalf("ApplyEvent err = %v", err)
		}
		if got != "awaiting_human" {
			t.Errorf("ApplyEvent = %q, want awaiting_human (Status beats Event delegation)", got)
		}
	})

	t.Run("nil_on_fail_is_noop", func(t *testing.T) {
		f := &formula.FormulaStepGraph{
			Steps: map[string]formula.StepConfig{
				"implement": {Lifecycle: &formula.LifecycleConfig{}},
			},
		}
		got, err := ApplyEvent("in_progress", FormulaStepFailed{Step: "implement"}, f)
		if err != nil {
			t.Fatalf("ApplyEvent err = %v", err)
		}
		if got != "in_progress" {
			t.Errorf("ApplyEvent = %q, want in_progress (nil OnFail)", got)
		}
	})
}

// TestApplyEvent_AcceptsLanding3Statuses covers the four statuses that
// spi-sqqero Landing 3 (spi-lkeuqy) introduces as valid transition
// targets. Each status must be reachable from in_progress via a formula
// declaration; before Landing 3 these returned a descriptive
// "not-in-legal-set" error, and that gate must now pass.
func TestApplyEvent_AcceptsLanding3Statuses(t *testing.T) {
	for _, s := range []string{"awaiting_review", "needs_changes", "awaiting_human", "merge_pending"} {
		t.Run("on_complete_"+s, func(t *testing.T) {
			f := &formula.FormulaStepGraph{
				Steps: map[string]formula.StepConfig{
					"implement": {Lifecycle: &formula.LifecycleConfig{OnComplete: s}},
				},
			}
			got, err := ApplyEvent("in_progress", FormulaStepCompleted{Step: "implement"}, f)
			if err != nil {
				t.Fatalf("ApplyEvent err = %v (status %q must be legal)", err, s)
			}
			if got != s {
				t.Errorf("ApplyEvent = %q, want %q", got, s)
			}
		})
		t.Run("on_start_"+s, func(t *testing.T) {
			f := &formula.FormulaStepGraph{
				Steps: map[string]formula.StepConfig{
					"review": {Lifecycle: &formula.LifecycleConfig{OnStart: s}},
				},
			}
			got, err := ApplyEvent("in_progress", FormulaStepStarted{Step: "review"}, f)
			if err != nil {
				t.Fatalf("ApplyEvent err = %v (status %q must be legal)", err, s)
			}
			if got != s {
				t.Errorf("ApplyEvent = %q, want %q", got, s)
			}
		})
		t.Run("on_complete_match_"+s, func(t *testing.T) {
			f := &formula.FormulaStepGraph{
				Steps: map[string]formula.StepConfig{
					"review": {Lifecycle: &formula.LifecycleConfig{
						OnCompleteMatch: []formula.MatchClause{
							{When: "outputs.verdict == 'go'", Status: s},
						},
					}},
				},
			}
			got, err := ApplyEvent("in_progress",
				FormulaStepCompleted{Step: "review", Outputs: map[string]any{"verdict": "go"}}, f)
			if err != nil {
				t.Fatalf("ApplyEvent err = %v (status %q must be legal)", err, s)
			}
			if got != s {
				t.Errorf("ApplyEvent = %q, want %q", got, s)
			}
		})
		t.Run("on_fail_"+s, func(t *testing.T) {
			f := &formula.FormulaStepGraph{
				Steps: map[string]formula.StepConfig{
					"implement": {Lifecycle: &formula.LifecycleConfig{
						OnFail: &formula.FailAction{Status: s},
					}},
				},
			}
			got, err := ApplyEvent("in_progress", FormulaStepFailed{Step: "implement"}, f)
			if err != nil {
				t.Fatalf("ApplyEvent err = %v (status %q must be legal)", err, s)
			}
			if got != s {
				t.Errorf("ApplyEvent = %q, want %q", got, s)
			}
		})
	}
}

// TestApplyEvent_RejectsUnknownStatus covers the generic
// "anything-not-in-the-legal-set" case beyond the four Landing 3 names.
func TestApplyEvent_RejectsUnknownStatus(t *testing.T) {
	f := &formula.FormulaStepGraph{
		Steps: map[string]formula.StepConfig{
			"implement": {Lifecycle: &formula.LifecycleConfig{OnStart: "totally_made_up"}},
		},
	}
	_, err := ApplyEvent("ready", FormulaStepStarted{Step: "implement"}, f)
	if err == nil {
		t.Fatal("expected error for unknown status")
	}
}

// TestApplyEvent_NilFormulaCoreEventsStillWork ensures core events do
// not require a formula. f may be nil for non-step events.
func TestApplyEvent_NilFormulaCoreEventsStillWork(t *testing.T) {
	if got, err := ApplyEvent("ready", WizardClaimed{}, nil); err != nil || got != "in_progress" {
		t.Errorf("WizardClaimed nil formula = (%q, %v)", got, err)
	}
}

// TestApplyEvent_UnknownEventType is a defensive check for the type
// switch's default arm — a future event type added to events.go but
// not to ApplyEvent must surface as an error.
func TestApplyEvent_UnknownEventType(t *testing.T) {
	_, err := ApplyEvent("open", unrecognizedEvent{}, nil)
	if err == nil {
		t.Fatal("expected error for unrecognized event type")
	}
}

// unrecognizedEvent satisfies Event via the unexported seal — defined
// in this _test.go file to exercise the default arm of the switch.
type unrecognizedEvent struct{}

func (unrecognizedEvent) isLifecycleEvent() {}

// TestApplyEvent_ApprenticeNoChanges covers the two HandoffDone modes
// of the migration event that replaces pkg/wizard/wizard.go:926.
//
// HandoffDone=false: in_progress → open (preserves the pre-migration
// reopen-as-open hack semantics); from any other status the event is a
// no-op so callers don't accidentally clobber a valid state.
//
// HandoffDone=true: deliberate no-op from every status — callers can
// fire the event unconditionally without branching on their side.
func TestApplyEvent_ApprenticeNoChanges(t *testing.T) {
	t.Run("handoff_false_from_in_progress_reopens", func(t *testing.T) {
		got, err := ApplyEvent("in_progress", ApprenticeNoChanges{HandoffDone: false}, nil)
		if err != nil {
			t.Fatalf("ApplyEvent err = %v", err)
		}
		if got != "open" {
			t.Errorf("ApplyEvent = %q, want open", got)
		}
	})

	t.Run("handoff_false_from_other_statuses_is_noop", func(t *testing.T) {
		for _, s := range evaluatorLegalStatuses {
			if s == "in_progress" {
				continue
			}
			got, err := ApplyEvent(s, ApprenticeNoChanges{HandoffDone: false}, nil)
			if err != nil {
				t.Fatalf("ApplyEvent from %q err = %v", s, err)
			}
			if got != s {
				t.Errorf("ApplyEvent from %q = %q, want unchanged", s, got)
			}
		}
	})

	t.Run("handoff_true_is_noop_from_every_status", func(t *testing.T) {
		for _, s := range evaluatorLegalStatuses {
			got, err := ApplyEvent(s, ApprenticeNoChanges{HandoffDone: true}, nil)
			if err != nil {
				t.Fatalf("ApplyEvent from %q err = %v", s, err)
			}
			if got != s {
				t.Errorf("ApplyEvent HandoffDone=true from %q = %q, want unchanged", s, got)
			}
		}
	})
}
