package recovery

import (
	"fmt"
	"time"

	"github.com/steveyegge/beads"
)

// BeadOps is the minimal store surface needed for recovery bead lifecycle
// operations. Satisfied by both executor deps (via an adapter) and a CLI-side
// adapter wrapping store bridge functions.
type BeadOps interface {
	GetDependentsWithMeta(id string) ([]*beads.IssueWithDependencyMetadata, error)
	AddComment(id, text string) error
	CloseBead(id string) error
}

// isRecoveryBead returns true for both new-style (type=recovery) and old-style
// (type=task + recovery-bead label) beads. Uses the "recovery-bead" label which
// both styles carry.
func isRecoveryBead(item *beads.IssueWithDependencyMetadata) bool {
	for _, l := range item.Labels {
		if l == "recovery-bead" {
			return true
		}
	}
	return false
}

// isRecoveryLink returns true for both caused-by (new) and recovery-for (old)
// dependency edges.
func isRecoveryLink(depType string) bool {
	return depType == "caused-by" || depType == "recovery-for"
}

// CloseRelatedRecoveryBeads closes all open recovery beads linked to beadID.
// Handles both new (caused-by) and legacy (recovery-for) dependency edges.
// reason is appended as a comment before closing.
func CloseRelatedRecoveryBeads(ops BeadOps, beadID, reason string) error {
	items, err := ops.GetDependentsWithMeta(beadID)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.Status != beads.StatusOpen && item.Status != beads.StatusInProgress {
			continue
		}
		if !isRecoveryBead(item) || !isRecoveryLink(string(item.DependencyType)) {
			continue
		}
		_ = ops.AddComment(item.ID, fmt.Sprintf("Resolved: %s", reason))
		if err := ops.CloseBead(item.ID); err != nil {
			return fmt.Errorf("close recovery bead %s: %w", item.ID, err)
		}
	}
	return nil
}

// DedupeRecoveryBead checks whether an open recovery bead already exists for
// parentID with failure class failureClass (matched via the
// "failure_class:<failureClass>" label). If found, appends an incident comment
// and returns the existing bead ID. If not found, returns ("", false, nil).
func DedupeRecoveryBead(ops BeadOps, parentID, failureClass string) (string, bool, error) {
	items, err := ops.GetDependentsWithMeta(parentID)
	if err != nil {
		return "", false, err
	}
	target := "failure_class:" + failureClass
	for _, item := range items {
		if item.Status != beads.StatusOpen && item.Status != beads.StatusInProgress {
			continue
		}
		if !isRecoveryBead(item) || !isRecoveryLink(string(item.DependencyType)) {
			continue
		}
		for _, l := range item.Labels {
			if l == target {
				_ = ops.AddComment(item.ID,
					fmt.Sprintf("New incident: %s at %s", failureClass,
						time.Now().UTC().Format(time.RFC3339)))
				return item.ID, true, nil
			}
		}
	}
	return "", false, nil
}
