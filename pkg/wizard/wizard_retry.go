package wizard

import (
	"fmt"
	"os"

	"github.com/awell-health/spire/pkg/executor"
)

// Known wizard steps that a retry request can target. These correspond to the
// internal phases of CmdWizardRun's execution flow.
var knownWizardSteps = map[string]bool{
	"design":    true,
	"implement": true,
	"commit":    true,
	"build-gate":     true,
	"test":      true,
	"review":    true,
}

// retryState tracks whether the current wizard run is executing a recovery
// retry request. When retrying is true, step completions and failures are
// reported back to the recovery agent via the handoff protocol.
type retryState struct {
	retrying       bool
	request        *executor.RetryRequest
	currentStep    string
	beadID         string
	log            func(string, ...interface{})
}

// checkRetryRequest checks for a pending retry request on the bead at wizard
// entry. If a request is present, it validates the target step and returns a
// retryState configured for the retry. If no request is pending, it returns
// a zero retryState with retrying=false.
//
// Multiple retry requests: only the latest (highest AttemptNumber) is honored.
// Stale requests (lower AttemptNumber) are cleared automatically by
// SetRetryRequest in recovery_protocol.go.
func checkRetryRequest(beadID string, log func(string, ...interface{})) (*retryState, error) {
	req, found, err := executor.GetRetryRequest(beadID)
	if err != nil {
		return nil, fmt.Errorf("check retry request: %w", err)
	}

	if !found {
		return &retryState{beadID: beadID, log: log}, nil
	}

	// Validate that FromStep matches a known wizard step.
	if !knownWizardSteps[req.FromStep] {
		// Unknown step — report failure and exit.
		result := executor.RetryResult{
			Success: false,
			Error:   fmt.Sprintf("unknown step: %s", req.FromStep),
		}
		if setErr := executor.SetRetryResult(beadID, result); setErr != nil {
			log("warning: failed to set retry result for unknown step: %s", setErr)
		}
		if clearErr := executor.ClearRetryRequest(beadID); clearErr != nil {
			log("warning: failed to clear retry request: %s", clearErr)
		}
		return nil, fmt.Errorf("recovery agent requested retry from unknown step: %s", req.FromStep)
	}

	log("Recovery agent requested retry from step: %s (attempt %d)", req.FromStep, req.AttemptNumber)
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

// shouldSkipTo returns true if the wizard should skip ahead to the given step
// during a retry. Steps before the retry target are skipped.
func (rs *retryState) shouldSkipTo(step string) bool {
	if !rs.retrying {
		return false
	}
	return step != rs.request.FromStep
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
		StepReached: rs.currentStep,
	}
	if err := executor.SetRetryResult(rs.beadID, result); err != nil {
		rs.log("warning: failed to set retry result: %s", err)
	}
	if err := executor.ClearRetryRequest(rs.beadID); err != nil {
		rs.log("warning: failed to clear retry request: %s", err)
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
		FailedStep: rs.currentStep,
		Error:      errMsg,
	}
	if err := executor.SetRetryResult(rs.beadID, result); err != nil {
		rs.log("warning: failed to set retry result: %s", err)
	}

	rs.log("Retry failed at step %s, deferring to recovery agent %s",
		rs.currentStep, rs.request.RecoveryBeadID)

	// Write a minimal result.json so the executor knows we exited intentionally.
	fmt.Fprintf(os.Stderr, "[%s] retry failure — exiting cleanly for recovery agent\n", rs.beadID)
	return true
}
