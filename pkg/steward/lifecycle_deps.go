package steward

// lifecycle_deps.go wires the pkg/beadlifecycle.Deps interface to the
// store + recovery functions available inside pkg/steward. This lets the
// daemon tick call beadlifecycle.OrphanSweep without importing cmd/spire.

import (
	"github.com/awell-health/spire/pkg/beadlifecycle"
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// daemonLifecycleDeps implements beadlifecycle.Deps using the store package
// directly. The daemon always has a live store connection via the background
// dolt server, so these calls are safe to make from the daemon tick.
type daemonLifecycleDeps struct{}

func (daemonLifecycleDeps) GetBead(id string) (store.Bead, error) {
	return store.GetBead(id)
}

func (daemonLifecycleDeps) UpdateBead(id string, updates map[string]interface{}) error {
	return store.UpdateBead(id, updates)
}

func (daemonLifecycleDeps) CreateAttemptBead(parentID, agentName, model, branch string) (string, error) {
	return store.CreateAttemptBead(parentID, agentName, model, branch)
}

func (daemonLifecycleDeps) CloseAttemptBead(attemptID, resultLabel string) error {
	return store.CloseAttemptBead(attemptID, resultLabel)
}

func (daemonLifecycleDeps) ListAttemptsForBead(beadID string) ([]store.Bead, error) {
	children, err := store.GetChildren(beadID)
	if err != nil {
		return nil, err
	}
	var attempts []store.Bead
	for _, c := range children {
		if store.IsAttemptBead(c) {
			attempts = append(attempts, c)
		}
	}
	return attempts, nil
}

func (daemonLifecycleDeps) RemoveLabel(id, label string) error {
	return store.RemoveLabel(id, label)
}

func (daemonLifecycleDeps) AlertCascadeClose(sourceBeadID string) error {
	return recovery.CloseRelatedDependents(
		daemonRecoveryOps{},
		sourceBeadID,
		[]string{recovery.KindRecovery, recovery.KindAlert},
		[]string{"caused-by", "recovery-for", "related"},
		"work complete",
	)
}

func (daemonLifecycleDeps) AddLabel(id, label string) error {
	return store.AddLabel(id, label)
}

func (daemonLifecycleDeps) ListBeads(filter beads.IssueFilter) ([]store.Bead, error) {
	return store.ListBeads(filter)
}

// daemonRecoveryOps implements recovery.BeadOps for the daemon context.
type daemonRecoveryOps struct{}

func (daemonRecoveryOps) GetDependentsWithMeta(id string) ([]*beads.IssueWithDependencyMetadata, error) {
	return store.GetDependentsWithMeta(id)
}

func (daemonRecoveryOps) AddComment(id, text string) error {
	return store.AddComment(id, text)
}

func (daemonRecoveryOps) CloseBead(id string) error {
	return store.CloseBead(id)
}

// newDaemonLifecycleDeps returns a daemonLifecycleDeps wired to the store.
func newDaemonLifecycleDeps() beadlifecycle.Deps {
	return daemonLifecycleDeps{}
}
