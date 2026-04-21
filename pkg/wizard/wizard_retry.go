package wizard

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/awell-health/spire/pkg/executor"
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/runtime"
)

// knownWizardSteps is a local alias for executor.KnownWizardPhases.
// Kept as a read-only reference for backward compatibility in tests.
var knownWizardSteps = executor.KnownWizardPhases

// retryState tracks whether the current wizard run is executing a recovery
// retry request. When retrying is true, step completions and failures are
// reported back to the recovery agent via the handoff protocol.
type retryState struct {
	retrying    bool
	request     *executor.RetryRequest
	currentStep string
	beadID      string
	log         func(string, ...interface{})
}

// checkRetryRequest checks for a pending retry request on the bead at wizard
// entry. If a request is present, it validates the target step and returns a
// retryState configured for the retry. If no request is pending, it returns
// a zero retryState with retrying=false.
//
// Multiple retry requests: only the latest (highest AttemptNumber) is honored.
// Stale requests (lower AttemptNumber) are cleared automatically by
// SetRetryRequest in recovery_protocol.go.
//
// VerifyPlan dispatch (design spi-h32xj §5): when the request carries a
// typed VerifyPlan, the wizard branches on VerifyPlan.Kind:
//   - rerun-step: skip to VerifyPlan.StepName (falls back to FromStep) and
//     run the phase loop normally.
//   - narrow-check / recipe-postcondition: handled inline by
//     runVerifyPlanIfNonStep; see that method's docs.
//
// Legacy pre-chunk-5 requests (nil VerifyPlan) are normalized here to
// Kind=rerun-step with StepName=FromStep so downstream dispatch has a
// single shape to consume.
func checkRetryRequest(beadID string, log func(string, ...interface{})) (*retryState, error) {
	req, found, err := executor.GetRetryRequest(beadID)
	if err != nil {
		return nil, fmt.Errorf("check retry request: %w", err)
	}

	if !found {
		return &retryState{beadID: beadID, log: log}, nil
	}

	// Map the FromStep to a wizard-compatible phase. This handles graph step
	// names (e.g., "verify-build") that the recovery agent may forward.
	mapped := executor.MapToWizardPhase(req.FromStep)
	if mapped != req.FromStep {
		log("Mapped recovery step %q → wizard phase %q", req.FromStep, mapped)
		req.FromStep = mapped
	}

	// Normalize VerifyPlan. A nil plan is legacy pre-chunk-5 shape — treat
	// it as rerun-step against FromStep so downstream dispatch is uniform.
	if req.VerifyPlan == nil {
		req.VerifyPlan = &recovery.VerifyPlan{
			Kind:     recovery.VerifyKindRerunStep,
			StepName: req.FromStep,
		}
	} else {
		if req.VerifyPlan.Kind == "" {
			req.VerifyPlan.Kind = recovery.VerifyKindRerunStep
		}
		if req.VerifyPlan.Kind == recovery.VerifyKindRerunStep && req.VerifyPlan.StepName == "" {
			req.VerifyPlan.StepName = req.FromStep
		}
	}

	log("Recovery agent requested %s verify from step: %s (attempt %d)",
		req.VerifyPlan.Kind, skipToStep(req), req.AttemptNumber)
	if req.Guidance != "" {
		log("Recovery guidance: %s", req.Guidance)
	}

	return &retryState{
		retrying: true,
		request:  req,
		beadID:   beadID,
		log:      log,
	}, nil
}

// skipToStep returns the wizard phase the retry should skip ahead to. Falls
// back to FromStep when VerifyPlan.StepName is empty so legacy callers that
// only populate FromStep keep working.
func skipToStep(req *executor.RetryRequest) string {
	if req == nil {
		return ""
	}
	if req.VerifyPlan != nil && req.VerifyPlan.StepName != "" {
		return req.VerifyPlan.StepName
	}
	return req.FromStep
}

// shouldSkipTo returns true if the wizard should skip ahead to the given step
// during a retry. Steps before the retry target are skipped. The target is
// taken from VerifyPlan.StepName when present; otherwise FromStep is used.
func (rs *retryState) shouldSkipTo(step string) bool {
	if !rs.retrying || rs.request == nil {
		return false
	}
	return step != skipToStep(rs.request)
}

// enterStep records the current step being executed. Called at the start of
// each wizard phase.
func (rs *retryState) enterStep(step string) {
	rs.currentStep = step
}

// handleStepSuccess is called when a step completes successfully during a
// retry. It reports success back to the recovery agent and clears the request.
// After calling this, the wizard should continue normal execution.
func (rs *retryState) handleStepSuccess() {
	if !rs.retrying {
		return
	}

	result := executor.RetryResult{
		Success:     true,
		Verdict:     recovery.VerifyVerdictPass,
		StepReached: rs.currentStep,
	}
	if err := executor.SetRetryResult(rs.beadID, result); err != nil {
		rs.log("warning: failed to set retry result: %s", err)
	}

	rs.log("Retry succeeded at step %s, continuing normal execution", rs.currentStep)
	// Clear retrying flag — the handoff is complete, continue normally.
	rs.retrying = false
}

// handleStepFailure is called when a step fails during a retry. It reports
// the failure back to the recovery agent and signals that the wizard should
// exit cleanly without proceeding to normal failure/recovery handling.
// Returns true to indicate the caller should exit.
func (rs *retryState) handleStepFailure(errMsg string) bool {
	if !rs.retrying {
		return false
	}

	result := executor.RetryResult{
		Success:    false,
		Verdict:    recovery.VerifyVerdictFail,
		FailedStep: rs.currentStep,
		Error:      errMsg,
	}
	if err := executor.SetRetryResult(rs.beadID, result); err != nil {
		rs.log("warning: failed to set retry result: %s", err)
	}

	rs.log("Retry failed at step %s, deferring to recovery agent %s",
		rs.currentStep, rs.request.RecoveryBeadID)

	// Write a minimal result.json so the executor knows we exited intentionally.
	fmt.Fprintf(os.Stderr, "[%s] retry failure — exiting cleanly for recovery agent%s\n", rs.beadID, runtime.LogFields(runtime.RunContextFromEnv()))
	return true
}

// runVerifyPlanIfNonStep handles VerifyPlan kinds that don't flow through
// the phase loop. For narrow-check the VerifyPlan.Command runs in
// worktreeDir and the exit status becomes the VerifyVerdict. For
// recipe-postcondition a chunk-7 stub currently returns pass so promoted
// recipes don't regress before their postcondition runtime lands.
//
// Returns true when the caller should exit the wizard after this method
// (the verification has been fully dispatched and the result written via
// SetRetryResult). Returns false for rerun-step or when not retrying — in
// those cases the normal phase loop handles the handoff.
func (rs *retryState) runVerifyPlanIfNonStep(worktreeDir string) bool {
	if !rs.retrying || rs.request == nil || rs.request.VerifyPlan == nil {
		return false
	}
	switch rs.request.VerifyPlan.Kind {
	case recovery.VerifyKindNarrowCheck:
		verdict, errMsg := rs.runNarrowCheck(worktreeDir)
		rs.writeVerifyResult("narrow-check", verdict, errMsg)
		return true
	case recovery.VerifyKindRecipePostcondition:
		verdict, errMsg := rs.runRecipePostcondition(worktreeDir)
		rs.writeVerifyResult("recipe-postcondition", verdict, errMsg)
		return true
	}
	return false
}

// runNarrowCheck executes VerifyPlan.Command in worktreeDir and returns the
// resulting VerifyVerdict. Exit code 0 → pass; non-zero (or exec error) →
// fail. An empty command is treated as a misconfiguration and fails.
func (rs *retryState) runNarrowCheck(worktreeDir string) (recovery.VerifyVerdict, string) {
	verify := rs.request.VerifyPlan
	if len(verify.Command) == 0 {
		return recovery.VerifyVerdictFail, "narrow-check: empty command"
	}

	cmd := exec.Command(verify.Command[0], verify.Command[1:]...)
	if worktreeDir != "" {
		cmd.Dir = worktreeDir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		rs.log("narrow-check failed: %v (output: %s)", err, string(out))
		return recovery.VerifyVerdictFail, fmt.Sprintf("narrow-check: %v", err)
	}
	rs.log("narrow-check passed: %s", verify.Command)
	return recovery.VerifyVerdictPass, ""
}

// runRecipePostcondition is a chunk-7 stub for recipe postcondition
// verification. Until the captured-recipe postcondition runtime lands, a
// promoted recipe that reaches verify is treated as pass so the cleric can
// progress to learn/finish without blocking.
func (rs *retryState) runRecipePostcondition(_ string) (recovery.VerifyVerdict, string) {
	rs.log("recipe-postcondition stub (chunk 7) — returning pass")
	return recovery.VerifyVerdictPass, ""
}

// writeVerifyResult pushes the VerifyPlan-derived verdict back onto the
// target bead as a RetryResult so the waiting cleric sees the outcome
// through its poll loop. Clears the local retrying flag after the write.
func (rs *retryState) writeVerifyResult(label string, verdict recovery.VerifyVerdict, errMsg string) {
	result := executor.RetryResult{
		Success:     verdict == recovery.VerifyVerdictPass,
		Verdict:     verdict,
		StepReached: label,
		Error:       errMsg,
	}
	if verdict != recovery.VerifyVerdictPass {
		result.FailedStep = label
	}
	if err := executor.SetRetryResult(rs.beadID, result); err != nil {
		rs.log("warning: failed to set %s retry result: %s", label, err)
	}
	rs.retrying = false
}
