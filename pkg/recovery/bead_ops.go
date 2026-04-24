package recovery

import (
	"fmt"
	"slices"
	"strings"
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

// Dependent-kind constants consumed by CloseRelatedDependents.
// Callers compose a []string with any combination; unknown values are ignored.
const (
	// KindRecovery matches beads carrying the "recovery-bead" label. This
	// covers both new-style (type=recovery) and legacy (type=task with
	// recovery-bead label) recovery beads.
	KindRecovery = "recovery"
	// KindAlert matches beads carrying any "alert:*" label (or the bare
	// "alert" label). These are the per-failure alert beads that the
	// executor escalation path files (merge-failure, dispatch-failure,
	// build-failure, ...).
	KindAlert = "alert"
)

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

// isAlertBead returns true for beads carrying any "alert:*" label or the bare
// "alert" label. Spire's escalation paths file these per-failure-class so the
// board can surface them; reset should close them in the cascade (spi-pwdhs5
// Bug B) so stale alerts don't linger past a reset.
func isAlertBead(item *beads.IssueWithDependencyMetadata) bool {
	for _, l := range item.Labels {
		if l == "alert" || strings.HasPrefix(l, "alert:") {
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

// matchesKind returns true if item satisfies any of the requested kinds.
// KindRecovery matches recovery beads (recovery-bead label); KindAlert
// matches alert beads (alert:* label). Unknown kinds are silently ignored —
// callers typed this list in source, not at runtime, so a bad kind is a
// programmer error surfaced by tests.
func matchesKind(item *beads.IssueWithDependencyMetadata, kinds []string) bool {
	for _, k := range kinds {
		switch k {
		case KindRecovery:
			if isRecoveryBead(item) {
				return true
			}
		case KindAlert:
			if isAlertBead(item) {
				return true
			}
		}
	}
	return false
}

// CloseRelatedDependents closes open dependent beads linked to beadID whose
// kind appears in the kinds slice and whose dependency edge type appears in
// the depTypes slice.
//
// kinds controls which bead kinds to close (KindRecovery, KindAlert).
// depTypes controls which dependency edge types to traverse. Pass
// []string{"caused-by", "recovery-for"} to preserve the previous behaviour
// (equivalent to the old hardcoded isRecoveryLink check). Pass
// []string{"caused-by", "related"} to also traverse "related" edges, which
// is needed when alert beads were linked via the resummon path.
//
// reason is appended as a comment before closing. When kinds contains
// KindAlert, callers typically pass a "reset-cycle:<N>" string so the board
// can group post-reset state by cycle (see cmd/spire/reset.go).
func CloseRelatedDependents(ops BeadOps, beadID string, kinds []string, depTypes []string, reason string) error {
	items, err := ops.GetDependentsWithMeta(beadID)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.Status != beads.StatusOpen && item.Status != beads.StatusInProgress {
			continue
		}
		if !slices.Contains(depTypes, string(item.DependencyType)) {
			continue
		}
		if !matchesKind(item, kinds) {
			continue
		}
		_ = ops.AddComment(item.ID, fmt.Sprintf("Resolved: %s", reason))
		if err := ops.CloseBead(item.ID); err != nil {
			return fmt.Errorf("close dependent bead %s: %w", item.ID, err)
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
