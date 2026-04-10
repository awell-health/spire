package executor

import (
	"testing"
)

// TestHandleFinish_NeedsHuman verifies that when the decide step outputs
// needs_human=true, the finish step does NOT close the recovery bead.
func TestHandleFinish_NeedsHuman(t *testing.T) {
	var closedBead string

	deps := &Deps{
		AddComment: func(id, text string) error { return nil },
		CloseBead: func(id string) error {
			closedBead = id
			return nil
		},
	}

	state := &GraphState{
		Steps: map[string]StepState{
			"decide": {
				Status: "completed",
				Outputs: map[string]string{
					"chosen_action": "escalate",
					"needs_human":   "true",
					"reasoning":     "cannot fix automatically",
				},
			},
			"learn": {
				Status: "completed",
				Outputs: map[string]string{
					"outcome": "dirty",
				},
			},
		},
	}

	e := NewGraphForTest("spi-recovery-nh", "wizard-recovery", nil, state, deps)

	result := handleFinish(e, "finish", StepConfig{Action: "recovery.finish"}, state)

	if result.Error != nil {
		t.Fatalf("handleFinish returned error: %v", result.Error)
	}
	if result.Outputs["status"] != "needs_human" {
		t.Errorf("outputs[status] = %q, want %q", result.Outputs["status"], "needs_human")
	}
	if closedBead != "" {
		t.Errorf("CloseBead was called with %q, but should NOT have been called for needs_human", closedBead)
	}
}

// TestHandleFinish_NonEscalate verifies that when decide did NOT choose
// escalate, the finish step closes the recovery bead as before.
func TestHandleFinish_NonEscalate(t *testing.T) {
	var closedBead string

	deps := &Deps{
		AddComment: func(id, text string) error { return nil },
		CloseBead: func(id string) error {
			closedBead = id
			return nil
		},
	}

	state := &GraphState{
		Steps: map[string]StepState{
			"decide": {
				Status: "completed",
				Outputs: map[string]string{
					"chosen_action": "retry",
					"needs_human":   "false",
					"reasoning":     "retrying build",
				},
			},
			"learn": {
				Status: "completed",
				Outputs: map[string]string{
					"outcome": "clean",
				},
			},
		},
	}

	e := NewGraphForTest("spi-recovery-ok", "wizard-recovery", nil, state, deps)

	result := handleFinish(e, "finish", StepConfig{Action: "recovery.finish"}, state)

	if result.Error != nil {
		t.Fatalf("handleFinish returned error: %v", result.Error)
	}
	if result.Outputs["status"] != "success" {
		t.Errorf("outputs[status] = %q, want %q", result.Outputs["status"], "success")
	}
	if closedBead != "spi-recovery-ok" {
		t.Errorf("CloseBead called with %q, want %q", closedBead, "spi-recovery-ok")
	}
}

// TestHandleFinish_NilState verifies that handleFinish with a nil state
// still closes the bead (backwards compat for edge cases).
func TestHandleFinish_NilState(t *testing.T) {
	var closedBead string

	deps := &Deps{
		AddComment: func(id, text string) error { return nil },
		CloseBead: func(id string) error {
			closedBead = id
			return nil
		},
	}

	e := NewGraphForTest("spi-recovery-nil", "wizard-recovery", nil, nil, deps)

	result := handleFinish(e, "finish", StepConfig{Action: "recovery.finish"}, nil)

	if result.Error != nil {
		t.Fatalf("handleFinish returned error: %v", result.Error)
	}
	if result.Outputs["status"] != "success" {
		t.Errorf("outputs[status] = %q, want %q", result.Outputs["status"], "success")
	}
	if closedBead != "spi-recovery-nil" {
		t.Errorf("CloseBead called with %q, want %q", closedBead, "spi-recovery-nil")
	}
}
