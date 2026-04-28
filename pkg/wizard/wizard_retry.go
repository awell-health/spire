package wizard

// Cleric foundation (spi-h2d7yn): the wizard↔cleric retry protocol from the
// inline-recovery era is gone. The wizard no longer hands off to a cleric
// in-process; on step failure it hooks-and-exits. The retry struct is kept
// here as an inert shim so the existing wizard.go phase loop still
// compiles. Every method is a no-op that reports "not retrying", which
// makes shouldSkipTo always false (no skip), runVerifyPlanIfNonStep always
// false (no inline verify), and the step success/failure handlers no-op.
//
// The cleric runtime feature (spi-hhkozk) replaces this with a real
// retry surface — likely keyed off recovery beads in `awaiting_review`
// with the human-approved action carried via metadata.

type wizardRetryState struct {
	retrying    bool
	request     wizardRetryRequest
	currentStep string
	beadID      string
}

// wizardRetryRequest is a minimal placeholder so callers that read
// retry.request.FromStep still compile. It carries no state.
type wizardRetryRequest struct {
	FromStep string
}

// checkRetryRequest always returns an empty (non-retrying) retry state.
// The cleric foundation has no in-process retry handoff yet.
func checkRetryRequest(beadID string, _ func(format string, args ...any)) (*wizardRetryState, error) {
	return &wizardRetryState{beadID: beadID}, nil
}

// runVerifyPlanIfNonStep is a no-op in the foundation phase.
func (r *wizardRetryState) runVerifyPlanIfNonStep(_ string) bool { return false }

// shouldSkipTo never skips — every phase runs as authored.
func (r *wizardRetryState) shouldSkipTo(_ string) bool { return false }

// enterStep records the current phase name. Kept so existing call sites
// continue to compile; nothing reads it after the deletion of the retry
// protocol.
func (r *wizardRetryState) enterStep(name string) { r.currentStep = name }

// handleStepFailure always reports "not handled" so the caller's existing
// failure path runs.
func (r *wizardRetryState) handleStepFailure(_ string) bool { return false }

// handleStepSuccess is a no-op.
func (r *wizardRetryState) handleStepSuccess() {}
