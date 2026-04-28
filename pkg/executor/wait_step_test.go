package executor

import (
	"testing"
)

// TestWaitStepResult_HookedWhenProducedKeysMissing pins the externally-driven
// completion semantics added by the cleric foundation (spi-h2d7yn). A wait
// step with declared `produces` keys must park (Hooked=true) when any
// produced key is missing or empty in outputs — the gateway / steward sets
// the missing outputs to unhook.
func TestWaitStepResult_HookedWhenProducedKeysMissing(t *testing.T) {
	cases := []struct {
		name     string
		produces []string
		outputs  map[string]string
		hooked   bool
	}{
		{
			name:     "no outputs, single produced key",
			produces: []string{"gate"},
			outputs:  nil,
			hooked:   true,
		},
		{
			name:     "outputs present but missing key",
			produces: []string{"gate", "rejection_comment"},
			outputs:  map[string]string{"gate": "approve"},
			hooked:   true,
		},
		{
			name:     "outputs present but key is empty string",
			produces: []string{"gate"},
			outputs:  map[string]string{"gate": ""},
			hooked:   true,
		},
		{
			name:     "all produced keys present and non-empty",
			produces: []string{"gate"},
			outputs:  map[string]string{"gate": "approve"},
			hooked:   false,
		},
		{
			name:     "all multi-key produced keys present",
			produces: []string{"gate", "rejection_comment"},
			outputs:  map[string]string{"gate": "reject", "rejection_comment": "needs more thought"},
			hooked:   false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			step := StepConfig{Produces: c.produces}
			ss := StepState{Outputs: c.outputs}
			got := waitStepResult(step, ss)
			if got.Hooked != c.hooked {
				t.Fatalf("waitStepResult Hooked=%v, want %v (outputs=%v, produces=%v)",
					got.Hooked, c.hooked, c.outputs, c.produces)
			}
			if got.Error != nil {
				t.Fatalf("waitStepResult Error = %v, want nil", got.Error)
			}
		})
	}
}

// TestWaitStepResult_PreservesOutputsOnComplete asserts that when all produced
// keys are set, the dispatcher echoes the existing outputs back as the
// step's result. This mirrors the formula-engine assumption that downstream
// steps reading `steps.<wait_step>.outputs.<key>` see the same values that
// were set externally.
func TestWaitStepResult_PreservesOutputsOnComplete(t *testing.T) {
	step := StepConfig{Produces: []string{"gate", "rejection_comment"}}
	ss := StepState{Outputs: map[string]string{
		"gate":              "reject",
		"rejection_comment": "Try a different approach.",
	}}
	got := waitStepResult(step, ss)
	if got.Hooked {
		t.Fatalf("expected Hooked=false when all produces present")
	}
	if got.Outputs["gate"] != "reject" {
		t.Errorf("Outputs[gate] = %q, want reject", got.Outputs["gate"])
	}
	if got.Outputs["rejection_comment"] != "Try a different approach." {
		t.Errorf("Outputs[rejection_comment] = %q, want %q", got.Outputs["rejection_comment"], "Try a different approach.")
	}
}

// TestWaitStepResult_Idempotent verifies that repeated invocation while
// outputs are unset always yields the same Hooked=true result. Cleric
// foundation invariant: the dispatcher must be pure with respect to step
// state so a steward poll loop doesn't accidentally advance the step.
func TestWaitStepResult_Idempotent(t *testing.T) {
	step := StepConfig{Produces: []string{"gate"}}
	ss := StepState{Outputs: nil}
	for i := 0; i < 5; i++ {
		got := waitStepResult(step, ss)
		if !got.Hooked {
			t.Fatalf("iter %d: waitStepResult Hooked = false, want true (idempotent park)", i)
		}
	}
}
