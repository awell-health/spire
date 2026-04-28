package cleric

import (
	"time"

	"github.com/awell-health/spire/pkg/store"
)

// ObserverDeps groups the seams the wizard observer reaches through to
// finalize pending cleric_outcomes rows. Tests pass an in-memory fake;
// production wiring routes to pkg/store's gateway-aware Auto helpers.
type ObserverDeps struct {
	// PendingForSourceBead returns every non-finalized outcome row whose
	// source_bead_id matches. Production: store.PendingClericOutcomesForSourceBeadAuto.
	PendingForSourceBead func(sourceBeadID string) ([]store.ClericOutcome, error)

	// Finalize stamps wizard_post_action_success + finalized=true on a
	// row by id. Production: store.FinalizeClericOutcomeAuto.
	Finalize func(id string, success bool, finalizedAt time.Time) error

	// Now is the clock seam; tests inject deterministic time, production
	// callers leave nil and we fall back to time.Now.
	Now func() time.Time
}

func (d ObserverDeps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now().UTC()
}

// FinalizePendingOutcomes is invoked by the wizard's step-completion
// observer whenever a step on a source bead transitions. For each
// pending row whose target_step matches (or is empty), the function
// stamps wizard_post_action_success based on the success arg.
//
// Match rules:
//   - row.TargetStep != "": match iff row.TargetStep == completedStep.
//   - row.TargetStep == "": match any completed step (treats "any
//     advance" as success for actions like reset --hard / resummon
//     where there is no explicit target step name).
//
// If the wizard's per-step timeout fires (or any other "step did not
// advance" signal), call this with success=false and the same step name
// to finalize stuck pending rows as failures.
//
// Errors are non-fatal — observer failures should not crash the wizard
// loop. Returns the number of rows finalized for caller logging.
func FinalizePendingOutcomes(sourceBeadID, completedStep string, success bool, deps ObserverDeps) int {
	if deps.PendingForSourceBead == nil || deps.Finalize == nil {
		return 0
	}
	if sourceBeadID == "" {
		return 0
	}
	rows, err := deps.PendingForSourceBead(sourceBeadID)
	if err != nil {
		return 0
	}
	now := deps.now().UTC()
	finalized := 0
	for _, r := range rows {
		if !targetStepMatches(r.TargetStep, completedStep) {
			continue
		}
		if err := deps.Finalize(r.ID, success, now); err != nil {
			continue
		}
		finalized++
	}
	return finalized
}

// targetStepMatches reports whether a pending outcome's target_step
// matches a just-completed step. An empty target_step is a wildcard.
func targetStepMatches(targetStep, completedStep string) bool {
	if targetStep == "" {
		return true
	}
	return targetStep == completedStep
}

// StoreObserver wraps the gateway-aware Auto helpers in pkg/store so
// callers outside the cleric package can wire the observer with a
// single line.
type StoreObserver struct{}

// PendingForSourceBead routes to store.PendingClericOutcomesForSourceBeadAuto.
func (StoreObserver) PendingForSourceBead(sourceBeadID string) ([]store.ClericOutcome, error) {
	return store.PendingClericOutcomesForSourceBeadAuto(sourceBeadID)
}

// Finalize routes to store.FinalizeClericOutcomeAuto.
func (StoreObserver) Finalize(id string, success bool, finalizedAt time.Time) error {
	return store.FinalizeClericOutcomeAuto(id, success, finalizedAt)
}

// DefaultObserver is the production wiring; tests overwrite via the
// ObserverDeps struct directly.
var DefaultObserver = ObserverDeps{
	PendingForSourceBead: StoreObserver{}.PendingForSourceBead,
	Finalize:             StoreObserver{}.Finalize,
}
