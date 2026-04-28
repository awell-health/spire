package cleric

import "github.com/awell-health/spire/pkg/store"

// ConsecutiveThreshold is the bound for both promotion and demotion.
// Three consecutive matching outcomes flip a pair into the auto-approved
// or demoted state; one non-matching outcome resets to zero. The value is
// hardcoded in v1 per the spi-kl8x5y feature spec.
const ConsecutiveThreshold = 3

// LabelAutoApproved is the label cleric.publish stamps onto a recovery
// bead when the (failure_class, action) pair is promoted and the round
// auto-approves without entering awaiting_review. Audit-trail-only —
// downstream surfaces honor it; promotion logic does not key off it.
const LabelAutoApproved = "auto-approved:promoted"

// LearningStore is the seam the learning policy reaches the cleric_outcomes
// table through. The pkg/store function set is the production wiring;
// tests pass an in-memory fake. Each method surfaces an error for
// transport / SQL failures — the policy returns false (do not promote
// or demote) on any error so a degraded store fails safe.
type LearningStore interface {
	LastNFinalizedOutcomes(failureClass, action string, n int) ([]store.ClericOutcome, error)
	ListDemotedPairs(threshold int) ([]store.DemotedClericPair, error)
}

// IsPromoted reports whether the most recent ConsecutiveThreshold
// finalized outcomes for (failure_class, action) are all
// gate=approve AND wizard_post_action_success=true. False on any error,
// any short fetch, or any non-matching row.
//
// Pending rows (finalized=false) are filtered server-side by
// LastNFinalizedOutcomes; this function does not need to reason about
// them. A streak therefore cannot be broken just because a pending
// approve outcome is in flight.
func IsPromoted(s LearningStore, failureClass, action string) bool {
	if s == nil {
		return false
	}
	if failureClass == "" || action == "" {
		return false
	}
	rows, err := s.LastNFinalizedOutcomes(failureClass, action, ConsecutiveThreshold)
	if err != nil {
		return false
	}
	if len(rows) < ConsecutiveThreshold {
		return false
	}
	for _, r := range rows {
		if r.Gate != "approve" {
			return false
		}
		if !r.WizardPostActionSuccess.Valid || !r.WizardPostActionSuccess.Bool {
			return false
		}
	}
	return true
}

// IsDemoted reports whether the most recent ConsecutiveThreshold
// finalized outcomes for (failure_class, action) are all gate=reject.
// Same fail-safe semantics as IsPromoted.
//
// Demotion is an advisory signal surfaced in the cleric prompt; it does
// not block the cleric from proposing the pair again, only down-weights
// it.
func IsDemoted(s LearningStore, failureClass, action string) bool {
	if s == nil {
		return false
	}
	if failureClass == "" || action == "" {
		return false
	}
	rows, err := s.LastNFinalizedOutcomes(failureClass, action, ConsecutiveThreshold)
	if err != nil {
		return false
	}
	if len(rows) < ConsecutiveThreshold {
		return false
	}
	for _, r := range rows {
		if r.Gate != "reject" {
			return false
		}
	}
	return true
}

// ListDemoted returns all (failure_class, action) pairs currently
// demoted under the same rule as IsDemoted. The cleric prompt-builder
// calls this on every round to surface "patterns the human keeps
// rejecting" guidance. Returns an empty slice (not nil error) on store
// failures; the prompt-builder degrades gracefully.
func ListDemoted(s LearningStore) []store.DemotedClericPair {
	if s == nil {
		return nil
	}
	out, err := s.ListDemotedPairs(ConsecutiveThreshold)
	if err != nil {
		return nil
	}
	return out
}

// StoreLearning is the production LearningStore implementation. Wraps
// the gateway-aware Auto helpers in pkg/store so cleric package callers
// don't need to know about gateway-mode dispatch.
type StoreLearning struct{}

// LastNFinalizedOutcomes routes to store.LastNFinalizedClericOutcomesAuto.
func (StoreLearning) LastNFinalizedOutcomes(failureClass, action string, n int) ([]store.ClericOutcome, error) {
	return store.LastNFinalizedClericOutcomesAuto(failureClass, action, n)
}

// ListDemotedPairs routes to store.ListDemotedClericPairsAuto.
func (StoreLearning) ListDemotedPairs(threshold int) ([]store.DemotedClericPair, error) {
	return store.ListDemotedClericPairsAuto(threshold)
}

// DefaultLearning is the package-level LearningStore production wiring.
// Tests overwrite this var; production leaves it set to StoreLearning{}.
var DefaultLearning LearningStore = StoreLearning{}
