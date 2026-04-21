package wizard

import (
	"testing"

	"github.com/awell-health/spire/pkg/executor"
	"github.com/awell-health/spire/pkg/recovery"
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
	unknowns := []string{"deploy", "verify", ""}
	for _, step := range unknowns {
		if knownWizardSteps[step] {
			t.Errorf("knownWizardSteps should not contain %q", step)
		}
	}
}

// TestKnownWizardSteps_MatchesExecutor verifies the local alias points to
// the canonical set in executor.KnownWizardPhases.
func TestKnownWizardSteps_MatchesExecutor(t *testing.T) {
	for phase := range executor.KnownWizardPhases {
		if !knownWizardSteps[phase] {
			t.Errorf("knownWizardSteps missing executor phase %q", phase)
		}
	}
	for phase := range knownWizardSteps {
		if !executor.KnownWizardPhases[phase] {
			t.Errorf("knownWizardSteps has extra phase %q not in executor", phase)
		}
	}
}

// TestMapToWizardPhase_VerifyBuildAccepted tests the exact bug scenario:
// recovery agent sends FromStep="verify-build" which should translate to
// "build-gate" and be accepted, not rejected as unknown.
func TestMapToWizardPhase_VerifyBuildAccepted(t *testing.T) {
	mapped := executor.MapToWizardPhase("verify-build")
	if mapped != "build-gate" {
		t.Fatalf("MapToWizardPhase('verify-build') = %q, want 'build-gate'", mapped)
	}
	if !knownWizardSteps[mapped] {
		t.Errorf("mapped phase %q not in knownWizardSteps", mapped)
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

// ---------------------------------------------------------------------------
// VerifyPlan dispatch (design spi-h32xj §5)
// ---------------------------------------------------------------------------

// TestShouldSkipTo_LegacyFromStepOnly covers the pre-chunk-5 wire shape:
// the retry request carries FromStep but no VerifyPlan. shouldSkipTo must
// still target FromStep so legacy cleric callers keep working.
func TestShouldSkipTo_LegacyFromStepOnly(t *testing.T) {
	rs := &retryState{
		retrying: true,
		request: &executor.RetryRequest{
			FromStep: "implement",
			// VerifyPlan intentionally left nil — legacy shape.
		},
		beadID: "spi-test-legacy",
		log:    func(string, ...interface{}) {},
	}

	if !rs.shouldSkipTo("design") {
		t.Error("legacy: shouldSkipTo('design') = false, want true (before FromStep)")
	}
	if rs.shouldSkipTo("implement") {
		t.Error("legacy: shouldSkipTo('implement') = true, want false (at FromStep)")
	}
}

// TestShouldSkipTo_VerifyPlanRerunStepPrefersStepName verifies that when a
// VerifyPlan with Kind=rerun-step is attached, its StepName overrides
// FromStep as the skip-to target. This is the chunk-5 typed-plan path.
func TestShouldSkipTo_VerifyPlanRerunStepPrefersStepName(t *testing.T) {
	rs := &retryState{
		retrying: true,
		request: &executor.RetryRequest{
			FromStep: "implement", // legacy fallback
			VerifyPlan: &recovery.VerifyPlan{
				Kind:     recovery.VerifyKindRerunStep,
				StepName: "build-gate",
			},
		},
		beadID: "spi-test-rerun",
		log:    func(string, ...interface{}) {},
	}

	// StepName=build-gate is the target — implement should be skipped.
	if !rs.shouldSkipTo("implement") {
		t.Error("rerun-step: shouldSkipTo('implement') = false, want true (before StepName)")
	}
	if rs.shouldSkipTo("build-gate") {
		t.Error("rerun-step: shouldSkipTo('build-gate') = true, want false (at StepName)")
	}
}

// TestRunVerifyPlanIfNonStep_NarrowCheckPass runs `true` (always-succeeds
// POSIX utility) through the narrow-check branch and asserts the wizard
// reports VerifyVerdictPass. The SetRetryResult call hits the store and
// fails without one — the critical assertion is the verdict and the
// retrying flag being cleared, which we can observe on the retryState.
func TestRunVerifyPlanIfNonStep_NarrowCheckPass(t *testing.T) {
	rs := &retryState{
		retrying: true,
		request: &executor.RetryRequest{
			RecoveryBeadID: "spi-recovery-nc-pass",
			VerifyPlan: &recovery.VerifyPlan{
				Kind:    recovery.VerifyKindNarrowCheck,
				Command: []string{"true"},
			},
		},
		beadID: "spi-test-nc-pass",
		log:    func(string, ...interface{}) {},
	}

	verdict, errMsg := rs.runNarrowCheck("")
	if verdict != recovery.VerifyVerdictPass {
		t.Errorf("narrow-check(true): verdict = %q, want %q", verdict, recovery.VerifyVerdictPass)
	}
	if errMsg != "" {
		t.Errorf("narrow-check(true): errMsg = %q, want empty", errMsg)
	}

	// runVerifyPlanIfNonStep should also report handled=true for narrow-check.
	handled := rs.runVerifyPlanIfNonStep("")
	if !handled {
		t.Error("runVerifyPlanIfNonStep(narrow-check) = false, want true")
	}
	if rs.retrying {
		t.Error("retrying flag should be cleared after narrow-check dispatch")
	}
}

// TestRunNarrowCheck_Fail runs `false` (always-fails POSIX utility) through
// runNarrowCheck and asserts the verdict is fail and an error message is
// populated.
func TestRunNarrowCheck_Fail(t *testing.T) {
	rs := &retryState{
		retrying: true,
		request: &executor.RetryRequest{
			RecoveryBeadID: "spi-recovery-nc-fail",
			VerifyPlan: &recovery.VerifyPlan{
				Kind:    recovery.VerifyKindNarrowCheck,
				Command: []string{"false"},
			},
		},
		beadID: "spi-test-nc-fail",
		log:    func(string, ...interface{}) {},
	}

	verdict, errMsg := rs.runNarrowCheck("")
	if verdict != recovery.VerifyVerdictFail {
		t.Errorf("narrow-check(false): verdict = %q, want %q", verdict, recovery.VerifyVerdictFail)
	}
	if errMsg == "" {
		t.Error("narrow-check(false): errMsg should be non-empty")
	}
}

// TestRunNarrowCheck_EmptyCommand verifies that a narrow-check VerifyPlan
// with no Command is treated as a misconfiguration and fails.
func TestRunNarrowCheck_EmptyCommand(t *testing.T) {
	rs := &retryState{
		retrying: true,
		request: &executor.RetryRequest{
			VerifyPlan: &recovery.VerifyPlan{Kind: recovery.VerifyKindNarrowCheck},
		},
		beadID: "spi-test-nc-empty",
		log:    func(string, ...interface{}) {},
	}

	verdict, errMsg := rs.runNarrowCheck("")
	if verdict != recovery.VerifyVerdictFail {
		t.Errorf("narrow-check(empty): verdict = %q, want %q", verdict, recovery.VerifyVerdictFail)
	}
	if errMsg == "" {
		t.Error("narrow-check(empty): errMsg should be non-empty")
	}
}

// TestRunVerifyPlanIfNonStep_RerunStepNotHandled verifies that rerun-step
// plans (or nil plans) return false — the caller must fall through to the
// normal phase loop for those.
func TestRunVerifyPlanIfNonStep_RerunStepNotHandled(t *testing.T) {
	cases := []struct {
		name string
		plan *recovery.VerifyPlan
	}{
		{name: "nil-plan", plan: nil},
		{name: "rerun-step", plan: &recovery.VerifyPlan{Kind: recovery.VerifyKindRerunStep, StepName: "build-gate"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs := &retryState{
				retrying: true,
				request: &executor.RetryRequest{
					FromStep:   "build-gate",
					VerifyPlan: tc.plan,
				},
				beadID: "spi-test-rerun-passthrough",
				log:    func(string, ...interface{}) {},
			}

			if rs.runVerifyPlanIfNonStep("") {
				t.Errorf("runVerifyPlanIfNonStep(%s) = true, want false (phase loop handles it)", tc.name)
			}
			if !rs.retrying {
				t.Errorf("retrying flag should remain set for %s — the phase loop hasn't run yet", tc.name)
			}
		})
	}
}

// TestRunVerifyPlanIfNonStep_RecipePostcondition asserts the chunk-7 stub
// currently returns pass so promoted recipes don't regress before the real
// postcondition runtime lands.
func TestRunVerifyPlanIfNonStep_RecipePostcondition(t *testing.T) {
	rs := &retryState{
		retrying: true,
		request: &executor.RetryRequest{
			RecoveryBeadID: "spi-recovery-rp",
			VerifyPlan: &recovery.VerifyPlan{
				Kind: recovery.VerifyKindRecipePostcondition,
			},
		},
		beadID: "spi-test-recipe-pc",
		log:    func(string, ...interface{}) {},
	}

	verdict, errMsg := rs.runRecipePostcondition("")
	if verdict != recovery.VerifyVerdictPass {
		t.Errorf("recipe-postcondition stub: verdict = %q, want %q", verdict, recovery.VerifyVerdictPass)
	}
	if errMsg != "" {
		t.Errorf("recipe-postcondition stub: errMsg = %q, want empty", errMsg)
	}

	if !rs.runVerifyPlanIfNonStep("") {
		t.Error("runVerifyPlanIfNonStep(recipe-postcondition) = false, want true")
	}
	if rs.retrying {
		t.Error("retrying flag should be cleared after recipe-postcondition dispatch")
	}
}
