package executor

import (
	"fmt"
	"os"
	"time"

	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
)

// relapseDeps abstracts store operations used by relapse detection, enabling
// unit tests without a live database.
type relapseDeps struct {
	listLearnings   func(filter store.RecoveryLookupFilter) ([]store.RecoveryLearning, error)
	setMetadata     func(beadID string, meta map[string]string) error
	updateOutcomeSQL func(beadID, outcome string) error
	addComment      func(beadID, text string) error
}

// defaultRelapseDeps returns production wiring that calls the store package.
func defaultRelapseDeps(deps *Deps) relapseDeps {
	var addComment func(string, string) error
	if deps != nil {
		addComment = deps.AddComment
	}
	return relapseDeps{
		listLearnings:    store.ListClosedRecoveryBeads,
		setMetadata:      store.SetBeadMetadataMap,
		updateOutcomeSQL: store.UpdateLearningOutcomeAuto,
		addComment:       addComment,
	}
}

// checkAndMarkRelapse detects when a prior "clean" recovery for the same source
// bead + failure class was actually a false positive. If the source bead is being
// escalated again within 24 hours of a "clean" resolution, the prior learning is
// updated to outcome=relapsed.
//
// This is called from the escalation path (createOrUpdateRecoveryBead) every time
// a new recovery bead is created. The check is idempotent — marking a learning as
// relapsed twice is harmless.
func checkAndMarkRelapse(sourceBeadID, failureClass string, deps *Deps) {
	rd := defaultRelapseDeps(deps)
	checkAndMarkRelapseWith(sourceBeadID, failureClass, rd, time.Now().UTC())
}

// checkAndMarkRelapseWith is the testable core of relapse detection. It accepts
// injected dependencies and an explicit "now" time for deterministic testing.
func checkAndMarkRelapseWith(sourceBeadID, failureClass string, rd relapseDeps, now time.Time) {
	if sourceBeadID == "" || failureClass == "" {
		return
	}

	// Find recent closed recovery beads for this source + failure class.
	reusable := true
	learnings, err := rd.listLearnings(store.RecoveryLookupFilter{
		SourceBead:   sourceBeadID,
		FailureClass: failureClass,
		Reusable:     &reusable,
		Limit:        5,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: relapse check query failed: %s\n", err)
		return
	}

	relapseWindow := 24 * time.Hour

	for _, l := range learnings {
		// Only mark clean outcomes as relapsed (dirty outcomes are already known-bad).
		if l.VerificationStatus != "clean" && l.Outcome != "clean" {
			continue
		}
		// Already marked as relapsed — skip.
		if l.Outcome == "relapsed" {
			continue
		}

		// Check if resolved within the relapse window.
		resolvedAt, parseErr := time.Parse(time.RFC3339, l.ResolvedAt)
		if parseErr != nil {
			continue
		}
		if now.Sub(resolvedAt) > relapseWindow {
			continue
		}

		// This learning claimed "clean" but the source bead failed again.
		// Update bead metadata to mark as relapsed.
		if rd.setMetadata != nil {
			_ = rd.setMetadata(l.BeadID, map[string]string{
				recovery.KeyOutcome: "relapsed",
			})
		}

		// Update the SQL table row as well.
		if rd.updateOutcomeSQL != nil {
			if sqlErr := rd.updateOutcomeSQL(l.BeadID, "relapsed"); sqlErr != nil {
				fmt.Fprintf(os.Stderr, "warning: relapse update SQL for %s: %s\n", l.BeadID, sqlErr)
			}
		}

		// Add comment to the old recovery bead.
		if rd.addComment != nil {
			_ = rd.addComment(l.BeadID, fmt.Sprintf(
				"Relapsed: source bead %s failed again with failure class %q within 24h of this recovery's clean outcome. "+
					"Learning outcome updated from clean to relapsed.",
				sourceBeadID, failureClass,
			))
		}

		fmt.Fprintf(os.Stderr, "recovery: marked %s as relapsed (source=%s, class=%s, resolved=%s)\n",
			l.BeadID, sourceBeadID, failureClass, l.ResolvedAt)
	}
}
