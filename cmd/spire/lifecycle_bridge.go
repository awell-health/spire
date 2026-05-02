package main

// lifecycle_bridge.go wires the pkg/lifecycle.Deps interface to the
// cmd/spire store bridge functions. It provides lifecycleDeps, a thin adapter
// that satisfies lifecycle.Deps using the existing store bridge wrappers.

import (
	"time"

	"github.com/awell-health/spire/pkg/lifecycle"
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// lifecycleDeps implements lifecycle.Deps by delegating to the store
// bridge functions that already exist in cmd/spire. It is not safe to share
// across goroutines — each call site creates its own instance.
type lifecycleDeps struct{}

func (lifecycleDeps) GetBead(id string) (store.Bead, error) {
	return storeGetBead(id)
}

func (lifecycleDeps) UpdateBead(id string, updates map[string]interface{}) error {
	return storeUpdateBead(id, updates)
}

func (lifecycleDeps) CreateAttemptBead(parentID, agentName, model, branch string) (string, error) {
	return storeCreateAttemptBeadAtomic(parentID, agentName, model, branch)
}

func (lifecycleDeps) CloseAttemptBead(attemptID, resultLabel string) error {
	return storeCloseAttemptBead(attemptID, resultLabel)
}

func (lifecycleDeps) ListAttemptsForBead(beadID string) ([]store.Bead, error) {
	children, err := storeGetChildren(beadID)
	if err != nil {
		return nil, err
	}
	var attempts []store.Bead
	for _, c := range children {
		if isAttemptBead(c) {
			attempts = append(attempts, c)
		}
	}
	return attempts, nil
}

func (lifecycleDeps) RemoveLabel(id, label string) error {
	return storeRemoveLabel(id, label)
}

func (lifecycleDeps) AlertCascadeClose(sourceBeadID string) error {
	return recovery.CloseRelatedDependents(
		storeBridgeOps{},
		sourceBeadID,
		[]string{recovery.KindRecovery, recovery.KindAlert},
		[]string{"caused-by", "recovery-for", "related"},
		"work complete",
	)
}

func (lifecycleDeps) AddLabel(id, label string) error {
	return storeAddLabel(id, label)
}

func (lifecycleDeps) ListBeads(filter beads.IssueFilter) ([]store.Bead, error) {
	return storeListBeads(filter)
}

// GetAttemptHeartbeat reads the active attempt's last_seen_at heartbeat
// from instance metadata. The stamp is written by the executor's
// heartbeat tick (~30s); OrphanSweep uses it as the execution-owner
// liveness clock so a fresh-heartbeat wizard is not orphaned by a
// registry blip or stale PID (spi-p2ou7v).
func (lifecycleDeps) GetAttemptHeartbeat(attemptID string) (time.Time, bool, error) {
	meta, err := store.GetAttemptInstance(attemptID)
	if err != nil {
		return time.Time{}, false, err
	}
	if meta == nil || meta.LastSeenAt == "" {
		return time.Time{}, false, nil
	}
	t, perr := time.Parse(time.RFC3339, meta.LastSeenAt)
	if perr != nil {
		return time.Time{}, false, perr
	}
	return t, true, nil
}

// newLifecycleDeps returns a lifecycleDeps wired to the store bridge.
func newLifecycleDeps() lifecycle.Deps {
	return lifecycleDeps{}
}
