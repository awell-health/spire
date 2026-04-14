package wizard

import (
	"testing"

	"github.com/awell-health/spire/pkg/executor"
)

// ---------------------------------------------------------------------------
// knownWizardSteps
// ---------------------------------------------------------------------------

func TestKnownWizardSteps_Contains(t *testing.T) {
	expected := []string{"design", "implement", "commit", "build-gate", "test", "review"}
	for _, step := range expected {
		if !knownWizardSteps[step] {
			t.Errorf("knownWizardSteps missing %q", step)
		}
	}
}

func TestKnownWizardSteps_RejectsUnknown(t *testing.T) {
	unknowns := []string{"deploy", "merge", "plan", "verify", ""}
	for _, step := range unknowns {
		if knownWizardSteps[step] {
			t.Errorf("knownWizardSteps should not contain %q", step)
		}
	}
}

// ---------------------------------------------------------------------------
// retryState.shouldSkipTo
// ---------------------------------------------------------------------------

func TestShouldSkipTo_NotRetrying(t *testing.T) {
	rs := &retryState{
		retrying: false,
		beadID:   "spi-test-1",
		log:      func(string, ...interface{}) {},
	}

	// When not retrying, shouldSkipTo should always return false.
	if rs.shouldSkipTo("design") {
		t.Error("shouldSkipTo('design') = true, want false when not retrying")
	}
	if rs.shouldSkipTo("implement") {
		t.Error("shouldSkipTo('implement') = true, want false when not retrying")
	}
}

func TestShouldSkipTo_RetryingSkipsEarlierSteps(t *testing.T) {
	rs := &retryState{
		retrying: true,
		request: &executor.RetryRequest{
			FromStep: "build-gate",
		},
		beadID: "spi-test-1",
		log:    func(string, ...interface{}) {},
	}

	// Steps before the target should be skipped.
	if !rs.shouldSkipTo("design") {
		t.Error("shouldSkipTo('design') = false, want true (before target)")
	}
	if !rs.shouldSkipTo("implement") {
		t.Error("shouldSkipTo('implement') = false, want true (before target)")
	}
	if !rs.shouldSkipTo("commit") {
		t.Error("shouldSkipTo('commit') = false, want true (before target)")
	}

	// The target step itself should NOT be skipped.
	if rs.shouldSkipTo("build-gate") {
		t.Error("shouldSkipTo('build-gate') = true, want false (target step)")
	}

	// Steps after the target: shouldSkipTo uses equality check, so steps
	// after the target are also != target and return true. This is correct
	// because handleStepSuccess clears the retrying flag after the target
	// step succeeds, so later steps won't see retrying=true.
	if !rs.shouldSkipTo("test") {
		t.Error("shouldSkipTo('test') = false, want true (after target, but retrying still true)")
	}
}

// ---------------------------------------------------------------------------
// retryState.enterStep
// ---------------------------------------------------------------------------

func TestEnterStep(t *testing.T) {
	rs := &retryState{
		beadID: "spi-test-1",
		log:    func(string, ...interface{}) {},
	}

	rs.enterStep("design")
	if rs.currentStep != "design" {
		t.Errorf("currentStep = %q, want 'design'", rs.currentStep)
	}

	rs.enterStep("implement")
	if rs.currentStep != "implement" {
		t.Errorf("currentStep = %q, want 'implement'", rs.currentStep)
	}
}

// ---------------------------------------------------------------------------
// retryState.handleStepSuccess
// ---------------------------------------------------------------------------

func TestHandleStepSuccess_NotRetrying(t *testing.T) {
	rs := &retryState{
		retrying: false,
		beadID:   "spi-test-1",
		log:      func(string, ...interface{}) {},
	}

	// When not retrying, handleStepSuccess should be a no-op.
	// (No store calls to fail on.)
	rs.handleStepSuccess()
	// Should not panic and retrying should still be false.
	if rs.retrying {
		t.Error("retrying = true after handleStepSuccess when not retrying")
	}
}

func TestHandleStepSuccess_ClearsRetryingFlag(t *testing.T) {
	// handleStepSuccess calls executor.SetRetryResult and
	// executor.ClearRetryRequest which hit the store. Those calls will fail
	// with "no store" errors, but the function logs warnings and continues.
	// The critical behavior to test is that it clears the retrying flag.

	// We can't call handleStepSuccess with a real store, but we can verify
	// the retrying flag is set to false. The store calls will produce
	// warnings but should not panic since errors are caught.
	//
	// NOTE: This test will produce warning logs to stderr from the store
	// calls failing — that's expected.
	rs := &retryState{
		retrying: true,
		request: &executor.RetryRequest{
			RecoveryBeadID: "spi-recovery-1",
			FromStep:       "build-gate",
		},
		currentStep: "build-gate",
		beadID:      "spi-test-1",
		log:         func(string, ...interface{}) {},
	}

	// handleStepSuccess will attempt store operations that fail without
	// a running store. The function catches errors via log warnings.
	// The critical invariant is: rs.retrying must be false after return.
	rs.handleStepSuccess()

	if rs.retrying {
		t.Error("retrying = true after handleStepSuccess, want false")
	}
}

// ---------------------------------------------------------------------------
// retryState.handleStepFailure
// ---------------------------------------------------------------------------

func TestHandleStepFailure_NotRetrying(t *testing.T) {
	rs := &retryState{
		retrying: false,
		beadID:   "spi-test-1",
		log:      func(string, ...interface{}) {},
	}

	shouldExit := rs.handleStepFailure("some error")
	if shouldExit {
		t.Error("handleStepFailure returned true when not retrying, want false")
	}
}

func TestHandleStepFailure_Retrying_ReturnsTrueToExit(t *testing.T) {
	rs := &retryState{
		retrying: true,
		request: &executor.RetryRequest{
			RecoveryBeadID: "spi-recovery-1",
			FromStep:       "build-gate",
		},
		currentStep: "build-gate",
		beadID:      "spi-test-1",
		log:         func(string, ...interface{}) {},
	}

	// handleStepFailure will attempt store operations that fail, but
	// should still return true to signal the caller to exit.
	shouldExit := rs.handleStepFailure("build failed: exit code 1")

	if !shouldExit {
		t.Error("handleStepFailure returned false when retrying, want true")
	}
}

// ---------------------------------------------------------------------------
// shouldSkipTo + handleStepSuccess coupling
// ---------------------------------------------------------------------------

// TestSkipToThenSuccess validates the coupling between shouldSkipTo and
// handleStepSuccess. After handleStepSuccess clears retrying, shouldSkipTo
// returns false for all subsequent steps — this is the reason equality-not-
// ordering in shouldSkipTo is correct.
func TestSkipToThenSuccess_CouplingBehavior(t *testing.T) {
	rs := &retryState{
		retrying: true,
		request: &executor.RetryRequest{
			RecoveryBeadID: "spi-recovery-1",
			FromStep:       "build-gate",
		},
		beadID: "spi-test-1",
		log:    func(string, ...interface{}) {},
	}

	// Before target step: should skip.
	if !rs.shouldSkipTo("design") {
		t.Error("before target: shouldSkipTo('design') = false, want true")
	}
	if !rs.shouldSkipTo("implement") {
		t.Error("before target: shouldSkipTo('implement') = false, want true")
	}

	// At target step: should NOT skip (step == FromStep).
	if rs.shouldSkipTo("build-gate") {
		t.Error("at target: shouldSkipTo('build-gate') = true, want false")
	}

	// Simulate entering and succeeding at the target step.
	rs.enterStep("build-gate")
	rs.handleStepSuccess() // This clears retrying flag.

	// After handleStepSuccess: retrying is false.
	if rs.retrying {
		t.Fatal("retrying should be false after handleStepSuccess")
	}

	// Now all subsequent steps should NOT be skipped (retrying=false).
	if rs.shouldSkipTo("test") {
		t.Error("after success: shouldSkipTo('test') = true, want false")
	}
	if rs.shouldSkipTo("review") {
		t.Error("after success: shouldSkipTo('review') = true, want false")
	}
}
