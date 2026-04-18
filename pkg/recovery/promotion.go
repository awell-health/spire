package recovery

import (
	"fmt"

	"github.com/awell-health/spire/pkg/store"
)

// PromotionState is the lookup result for a failure signature's promotion
// status. Count is the number of consecutive clean+recipe outcomes for the
// signature (newest-first, stopping at the first failure / demotion / row
// without a recipe). Recipe is the most recent codified replay — non-nil
// only when Count > 0 and the chain is unbroken. Threshold is the effective
// cutoff from spire.yaml. Promoted is true when Count >= Threshold and
// Recipe is non-nil.
//
// The "one regression demotes" semantic is enforced at two layers:
//  1. store.GetPromotionSnapshot walks rows newest-first and stops at
//     the first outcome != "clean", row with demoted_at set, or row with
//     an empty mechanical_recipe.
//  2. MarkDemoted stamps demoted_at on the current chain so subsequent
//     lookups see count=0 until a fresh agentic success rebuilds it.
//
// TODO(spi-ngi35): failure_signature is the lookup key today. The design
// bead notes this may conflate distinct failure modes (dirty tree vs.
// diverged origin vs. real content conflict under the same "step-failure:merge"
// signature). Extending the key to failure_signature + first_error_kind is
// tracked as v2 work.
type PromotionState struct {
	FailureSig string
	Count      int
	Threshold  int
	Recipe     *MechanicalRecipe
	Promoted   bool
}

// LookupPromotionState reads the promotion counter for failureSig and
// returns the current state. Threshold comes from the caller (resolved
// via repoconfig.ResolveClericPromotionThreshold at the decide call site)
// so this helper has no dependency on repo config parsing.
//
// Empty failureSig returns a zero-value state (never promoted). Store
// errors are returned so the decide step can fall through to the agentic
// default rather than fail the recovery on a transient SQL issue.
func LookupPromotionState(failureSig string, threshold int) (*PromotionState, error) {
	state := &PromotionState{FailureSig: failureSig, Threshold: threshold}
	if failureSig == "" {
		return state, nil
	}
	if threshold <= 0 {
		return nil, fmt.Errorf("promotion threshold must be positive, got %d", threshold)
	}
	snap, err := store.GetPromotionSnapshotAuto(failureSig)
	if err != nil {
		return nil, fmt.Errorf("lookup promotion snapshot: %w", err)
	}
	state.Count = snap.CleanCount
	if snap.LatestRecipe != "" {
		recipe, err := UnmarshalRecipe(snap.LatestRecipe)
		if err != nil {
			// Corrupt recipe — treat as no recipe (never promote). Log via
			// error so the caller can surface it, but don't block recovery.
			return state, fmt.Errorf("parse stored recipe for %s: %w", failureSig, err)
		}
		state.Recipe = recipe
	}
	if state.Count >= threshold && state.Recipe != nil {
		state.Promoted = true
	}
	return state, nil
}

// MarkDemoted stamps demoted_at on every row that currently contributes to
// the promotion count for failureSig. Call this from the failure path of a
// promoted mechanical recipe so the next recovery falls back to agentic.
//
// Empty signature is a no-op (nothing to demote).
func MarkDemoted(failureSig string) error {
	if failureSig == "" {
		return nil
	}
	if err := store.DemotePromotedRowsAuto(failureSig); err != nil {
		return fmt.Errorf("mark demoted %s: %w", failureSig, err)
	}
	return nil
}
