package executor

import (
	"encoding/json"
	"testing"
)

// ---------------------------------------------------------------------------
// RetryRequest type
// ---------------------------------------------------------------------------

func TestRetryRequest_Fields(t *testing.T) {
	req := RetryRequest{
		RecoveryBeadID: "spi-recovery-1",
		TargetBeadID:   "spi-target-1",
		FromStep:       "verify-build",
		AttemptNumber:  3,
		Guidance:       "try rebase onto main",
	}
	if req.RecoveryBeadID != "spi-recovery-1" {
		t.Errorf("RecoveryBeadID = %q", req.RecoveryBeadID)
	}
	if req.FromStep != "verify-build" {
		t.Errorf("FromStep = %q", req.FromStep)
	}
	if req.AttemptNumber != 3 {
		t.Errorf("AttemptNumber = %d", req.AttemptNumber)
	}
	if req.Guidance != "try rebase onto main" {
		t.Errorf("Guidance = %q", req.Guidance)
	}
}

// ---------------------------------------------------------------------------
// RetryResult JSON round-trip
// ---------------------------------------------------------------------------

func TestRetryResult_JSONRoundTrip_Success(t *testing.T) {
	result := RetryResult{
		Success:     true,
		StepReached: "verify-build",
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded RetryResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !decoded.Success {
		t.Error("decoded.Success = false, want true")
	}
	if decoded.StepReached != "verify-build" {
		t.Errorf("decoded.StepReached = %q, want 'verify-build'", decoded.StepReached)
	}
	if decoded.FailedStep != "" {
		t.Errorf("decoded.FailedStep = %q, want empty", decoded.FailedStep)
	}
	if decoded.Error != "" {
		t.Errorf("decoded.Error = %q, want empty", decoded.Error)
	}
}

func TestRetryResult_JSONRoundTrip_Failure(t *testing.T) {
	result := RetryResult{
		Success:     false,
		FailedStep:  "build-gate",
		Error:       "build failed: exit status 1",
		StepReached: "implement",
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded RetryResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Success {
		t.Error("decoded.Success = true, want false")
	}
	if decoded.FailedStep != "build-gate" {
		t.Errorf("decoded.FailedStep = %q, want 'build-gate'", decoded.FailedStep)
	}
	if decoded.Error != "build failed: exit status 1" {
		t.Errorf("decoded.Error = %q", decoded.Error)
	}
}

// ---------------------------------------------------------------------------
// Label prefix constants
// ---------------------------------------------------------------------------

func TestLabelPrefixConstants(t *testing.T) {
	// Verify label prefix constants are well-formed.
	prefixes := map[string]string{
		"recoveryLabelRetryFrom":    recoveryLabelRetryFrom,
		"recoveryLabelAttempt":      recoveryLabelAttempt,
		"recoveryLabelRecoveryBead": recoveryLabelRecoveryBead,
		"recoveryLabelStatus":       recoveryLabelStatus,
		"recoveryLabelResult":       recoveryLabelResult,
		"recoveryLabelGuidance":     recoveryLabelGuidance,
		"recoveryLabelPrefix":       recoveryLabelPrefix,
	}

	for name, prefix := range prefixes {
		if prefix == "" {
			t.Errorf("%s is empty", name)
		}
		// All should start with "recovery:"
		if prefix[:9] != "recovery:" {
			t.Errorf("%s = %q, should start with 'recovery:'", name, prefix)
		}
	}

	// recoveryLabelPrefix should be the common prefix for all others.
	for name, prefix := range prefixes {
		if name == "recoveryLabelPrefix" {
			continue
		}
		if len(prefix) < len(recoveryLabelPrefix) {
			t.Errorf("%s (%q) is shorter than recoveryLabelPrefix", name, prefix)
			continue
		}
		if prefix[:len(recoveryLabelPrefix)] != recoveryLabelPrefix {
			t.Errorf("%s (%q) does not start with recoveryLabelPrefix (%q)", name, prefix, recoveryLabelPrefix)
		}
	}
}

// ---------------------------------------------------------------------------
// Label construction
// ---------------------------------------------------------------------------

func TestRetryRequestLabelConstruction(t *testing.T) {
	// Verify that labels constructed from constants + values are well-formed.
	req := RetryRequest{
		RecoveryBeadID: "spi-recovery-42",
		FromStep:       "verify-build",
		AttemptNumber:  2,
		Guidance:       "try cherry-pick",
	}

	// These are the labels that SetRetryRequest would construct.
	labels := []string{
		recoveryLabelRetryFrom + req.FromStep,
		recoveryLabelAttempt + "2",
		recoveryLabelRecoveryBead + req.RecoveryBeadID,
		recoveryLabelStatus + "waiting",
		recoveryLabelGuidance + req.Guidance,
	}

	expected := []string{
		"recovery:retry-from=verify-build",
		"recovery:attempt=2",
		"recovery:recovery-bead=spi-recovery-42",
		"recovery:status=waiting",
		"recovery:guidance=try cherry-pick",
	}

	for i, got := range labels {
		if got != expected[i] {
			t.Errorf("label[%d] = %q, want %q", i, got, expected[i])
		}
	}
}

// ---------------------------------------------------------------------------
// MapToWizardPhase
// ---------------------------------------------------------------------------

func TestMapToWizardPhase_KnownWizardSteps(t *testing.T) {
	// Known wizard phases should pass through unchanged.
	for phase := range KnownWizardPhases {
		got := MapToWizardPhase(phase)
		if got != phase {
			t.Errorf("MapToWizardPhase(%q) = %q, want %q (passthrough)", phase, got, phase)
		}
	}
}

func TestMapToWizardPhase_GraphStepNames(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"verify-build", "build-gate"},
		{"build-failed", "build-gate"},
		{"dispatch-children", "implement"},
		{"implement-failed", "implement"},
		{"design-check", "design"},
		{"plan", "design"},
		{"materialize", "design"},
		{"merge", "review"},
		{"close", "review"},
		{"discard", "review"},
		{"sage-review", "review"},
		{"review-fix", "review"},
		{"arbiter", "review"},
		{"verified", "review"},
	}
	for _, tt := range tests {
		got := MapToWizardPhase(tt.input)
		if got != tt.want {
			t.Errorf("MapToWizardPhase(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestMapToWizardPhase_FlowValues(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"task-plan", "design"},
		{"epic-plan", "design"},
		// "implement" is already a known wizard phase, should passthrough.
		{"implement", "implement"},
	}
	for _, tt := range tests {
		got := MapToWizardPhase(tt.input)
		if got != tt.want {
			t.Errorf("MapToWizardPhase(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestMapToWizardPhase_UnknownFallback(t *testing.T) {
	unknowns := []string{"something-weird", "deploy", "rollback", "custom-step"}
	for _, step := range unknowns {
		got := MapToWizardPhase(step)
		if got != "implement" {
			t.Errorf("MapToWizardPhase(%q) = %q, want 'implement' (fallback)", step, got)
		}
	}
}

func TestMapToWizardPhase_Empty(t *testing.T) {
	got := MapToWizardPhase("")
	if got != "implement" {
		t.Errorf("MapToWizardPhase('') = %q, want 'implement'", got)
	}
}

func TestRetryResultStatusLabel(t *testing.T) {
	tests := []struct {
		success bool
		want    string
	}{
		{true, "recovery:status=succeeded"},
		{false, "recovery:status=failed"},
	}
	for _, tt := range tests {
		status := "succeeded"
		if !tt.success {
			status = "failed"
		}
		got := recoveryLabelStatus + status
		if got != tt.want {
			t.Errorf("status label = %q, want %q", got, tt.want)
		}
	}
}
