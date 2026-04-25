package steward

// lifecycle_deps.go wires the pkg/beadlifecycle.Deps interface to the
// store + recovery functions available inside pkg/steward. This lets the
// daemon tick call beadlifecycle.OrphanSweep without importing cmd/spire.
//
// It also exposes a wizardregistry.Registry adapter wired to the local
// wizard registry file owned by pkg/agent. The adapter projects the rich
// agent.Entry shape (PID, Phase, Worktree, Tower, etc.) onto the minimal
// wizardregistry.Wizard contract used by mode-portable callers. Two views
// over the same on-disk file agree on liveness via the shared file lock
// and the zombie-safe pkg/process.ProcessAlive probe.

import (
	"context"
	"sync"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/beadlifecycle"
	"github.com/awell-health/spire/pkg/process"
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
	"github.com/awell-health/spire/pkg/wizardregistry"
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

// localRegistryAdapter satisfies wizardregistry.Registry on top of the
// pkg/agent registry file. Race-safety: each IsAlive call performs a
// fresh agent.RegistryList() read and a fresh process.ProcessAlive() probe;
// no per-sweep snapshot is cached.
type localRegistryAdapter struct {
	mu sync.Mutex // serializes Upsert/Remove read-modify-write within this process
}

func newLocalRegistryAdapter() *localRegistryAdapter { return &localRegistryAdapter{} }

func (a *localRegistryAdapter) List(_ context.Context) ([]wizardregistry.Wizard, error) {
	entries, err := agent.RegistryList()
	if err != nil {
		return nil, err
	}
	out := make([]wizardregistry.Wizard, 0, len(entries))
	for _, e := range entries {
		out = append(out, agentEntryToWizard(e))
	}
	return out, nil
}

func (a *localRegistryAdapter) Get(_ context.Context, id string) (wizardregistry.Wizard, error) {
	entries, err := agent.RegistryList()
	if err != nil {
		return wizardregistry.Wizard{}, err
	}
	for _, e := range entries {
		if e.Name == id {
			return agentEntryToWizard(e), nil
		}
	}
	return wizardregistry.Wizard{}, wizardregistry.ErrNotFound
}

func (a *localRegistryAdapter) Upsert(_ context.Context, w wizardregistry.Wizard) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	existing, _ := agent.RegistryList()
	var prev *agent.Entry
	for i := range existing {
		if existing[i].Name == w.ID {
			prev = &existing[i]
			break
		}
	}
	entry := agent.Entry{
		Name:      w.ID,
		PID:       w.PID,
		BeadID:    w.BeadID,
		StartedAt: formatRegistryTime(w.StartedAt),
	}
	if prev != nil {
		if entry.PID == 0 {
			entry.PID = prev.PID
		}
		if entry.StartedAt == "" {
			entry.StartedAt = prev.StartedAt
		}
		entry.Worktree = prev.Worktree
		entry.Tower = prev.Tower
		entry.Phase = prev.Phase
		entry.PhaseStartedAt = prev.PhaseStartedAt
		entry.InstanceID = prev.InstanceID
	}
	return agent.RegistryAdd(entry)
}

func (a *localRegistryAdapter) Remove(_ context.Context, id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	entries, err := agent.RegistryList()
	if err != nil {
		return err
	}
	found := false
	for _, e := range entries {
		if e.Name == id {
			found = true
			break
		}
	}
	if !found {
		return wizardregistry.ErrNotFound
	}
	return agent.RegistryRemove(id)
}

func (a *localRegistryAdapter) IsAlive(_ context.Context, id string) (bool, error) {
	entries, err := agent.RegistryList()
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.Name == id {
			return process.ProcessAlive(e.PID), nil
		}
	}
	return false, wizardregistry.ErrNotFound
}

func (a *localRegistryAdapter) Sweep(_ context.Context) ([]wizardregistry.Wizard, error) {
	entries, err := agent.RegistryList()
	if err != nil {
		return nil, err
	}
	var dead []wizardregistry.Wizard
	for _, e := range entries {
		if !process.ProcessAlive(e.PID) {
			dead = append(dead, agentEntryToWizard(e))
		}
	}
	return dead, nil
}

func agentEntryToWizard(e agent.Entry) wizardregistry.Wizard {
	return wizardregistry.Wizard{
		ID:        e.Name,
		Mode:      wizardregistry.ModeLocal,
		PID:       e.PID,
		BeadID:    e.BeadID,
		StartedAt: parseRegistryTime(e.StartedAt),
	}
}

func parseRegistryTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func formatRegistryTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

var _ wizardregistry.Registry = (*localRegistryAdapter)(nil)
