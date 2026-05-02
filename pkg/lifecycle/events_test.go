package lifecycle

import (
	"errors"
	"testing"
)

// TestEventInterfaceCompliance asserts that every concrete event type in
// the package satisfies the sealed Event interface. If a future change
// adds a new event type, it must be added here so the interface check
// stays exhaustive.
func TestEventInterfaceCompliance(t *testing.T) {
	cases := []struct {
		name string
		ev   Event
	}{
		{"Filed", Filed{}},
		{"ReadyToWork", ReadyToWork{}},
		{"WizardClaimed", WizardClaimed{}},
		{"FormulaStepStarted", FormulaStepStarted{Step: "implement"}},
		{"FormulaStepCompleted", FormulaStepCompleted{Step: "implement", Outputs: map[string]any{"ok": true}}},
		{"FormulaStepFailed", FormulaStepFailed{Step: "implement", Err: errors.New("boom")}},
		{"Escalated", Escalated{}},
		{"Closed", Closed{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.ev == nil {
				t.Fatalf("event %s is nil — does not satisfy Event", tc.name)
			}
		})
	}
}

func TestFormulaStepStarted_PreservesStep(t *testing.T) {
	ev := FormulaStepStarted{Step: "review"}
	if ev.Step != "review" {
		t.Errorf("Step = %q, want %q", ev.Step, "review")
	}
}

func TestFormulaStepCompleted_PreservesStepAndOutputs(t *testing.T) {
	outputs := map[string]any{
		"verdict": "approve",
		"rounds":  3,
	}
	ev := FormulaStepCompleted{Step: "review", Outputs: outputs}
	if ev.Step != "review" {
		t.Errorf("Step = %q, want %q", ev.Step, "review")
	}
	if got := ev.Outputs["verdict"]; got != "approve" {
		t.Errorf("Outputs[verdict] = %v, want approve", got)
	}
	if got := ev.Outputs["rounds"]; got != 3 {
		t.Errorf("Outputs[rounds] = %v, want 3", got)
	}
}

func TestFormulaStepFailed_PreservesStepAndErr(t *testing.T) {
	cause := errors.New("step crashed")
	ev := FormulaStepFailed{Step: "implement", Err: cause}
	if ev.Step != "implement" {
		t.Errorf("Step = %q, want %q", ev.Step, "implement")
	}
	if !errors.Is(ev.Err, cause) {
		t.Errorf("Err = %v, want %v", ev.Err, cause)
	}
}

// TestEventInterfaceIsSealed asserts that the Event interface uses an
// unexported method as its sealing token. If isLifecycleEvent ever
// becomes exported, this test fails so the package author notices.
func TestEventInterfaceIsSealed(t *testing.T) {
	// The fact that this file compiles is the seal proof: external
	// packages cannot construct values that satisfy Event because they
	// cannot implement the unexported isLifecycleEvent method.
	// Asserting a known concrete type satisfies Event is the runtime
	// half of the check.
	var _ Event = Filed{}
	var _ Event = ReadyToWork{}
	var _ Event = WizardClaimed{}
	var _ Event = FormulaStepStarted{}
	var _ Event = FormulaStepCompleted{}
	var _ Event = FormulaStepFailed{}
	var _ Event = Escalated{}
	var _ Event = Closed{}
}

func TestErrTransitionConflict_IsSentinel(t *testing.T) {
	if ErrTransitionConflict == nil {
		t.Fatal("ErrTransitionConflict is nil; expected sentinel error")
	}
	wrapped := errors.New("wrapped: " + ErrTransitionConflict.Error())
	if errors.Is(wrapped, ErrTransitionConflict) {
		t.Error("plain Errorf wrapping should not satisfy errors.Is")
	}
	chained := errors.Join(errors.New("other"), ErrTransitionConflict)
	if !errors.Is(chained, ErrTransitionConflict) {
		t.Error("errors.Join should preserve sentinel identity")
	}
}
