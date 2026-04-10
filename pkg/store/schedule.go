package store

import (
	"github.com/steveyegge/beads"
)

// GetActiveAttemptFunc is a test-replaceable function for GetActiveAttempt.
// Used by GetSchedulableWork to check for active attempt children.
var GetActiveAttemptFunc = GetActiveAttempt

// ScheduleResult holds the output of GetSchedulableWork.
type ScheduleResult struct {
	Schedulable []Bead
	Quarantined []QuarantinedBead
}

// QuarantinedBead represents a bead that could not be scheduled due to an
// invariant violation (e.g. multiple open attempts).
type QuarantinedBead struct {
	ID    string
	Error error
}

// GetSchedulableWork returns beads that are ready AND eligible for agent
// assignment. It calls GetReadyWork (which handles readiness: no open blockers,
// excludes deferred/design/attempt/review-round/step beads) and then applies
// scheduling policy filters:
//   - Skip message beads (exact "msg" label or "msg" prefix labels)
//   - Skip template beads
//   - Skip beads with an active attempt child (someone already working)
//   - Quarantine beads where GetActiveAttempt returns an error (invariant violation)
func GetSchedulableWork(filter beads.WorkFilter) (*ScheduleResult, error) {
	ready, err := GetReadyWork(filter)
	if err != nil {
		return nil, err
	}

	result := &ScheduleResult{}
	for _, b := range ready {
		// Skip message beads (both exact "msg" label and prefix like "msg:routing").
		if HasLabel(b, "msg") != "" || ContainsLabel(b, "msg") {
			continue
		}
		// Skip template beads.
		if ContainsLabel(b, "template") {
			continue
		}
		// Skip beads with an active attempt child (someone is already working).
		// Fail closed: if GetActiveAttemptFunc returns an error (e.g. multiple
		// open attempts), quarantine the bead rather than treating it as schedulable.
		attempt, aErr := GetActiveAttemptFunc(b.ID)
		if aErr != nil {
			result.Quarantined = append(result.Quarantined, QuarantinedBead{
				ID:    b.ID,
				Error: aErr,
			})
			continue
		}
		if attempt != nil {
			continue
		}
		result.Schedulable = append(result.Schedulable, b)
	}

	return result, nil
}
