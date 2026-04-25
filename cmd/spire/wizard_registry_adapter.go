package main

// wizard_registry_adapter.go projects the rich pkg/agent registry shape
// (PID + Phase + Worktree + Tower + InstanceID) onto the minimal
// wizardregistry.Registry contract used by mode-portable callers
// (OrphanSweep, summon, board, trace).
//
// The adapter satisfies the Registry race-safety contract: every IsAlive
// call performs a fresh agent.RegistryList() read and a fresh
// process.ProcessAlive() probe. There is no per-sweep snapshot.
//
// In cluster mode the operator wires wizardregistry/cluster directly;
// this adapter is the local-mode counterpart and never runs inside an
// operator pod.

import (
	"context"
	"sync"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/process"
	"github.com/awell-health/spire/pkg/wizardregistry"
)

// localRegistryAdapter implements wizardregistry.Registry on top of the
// pkg/agent registry storage.
type localRegistryAdapter struct {
	// mu guards the read-modify-write critical section in Upsert against
	// intra-process interleaving. The pkg/agent file lock provides
	// cross-process serialization.
	mu sync.Mutex
}

// newLocalRegistryAdapter returns the production adapter.
func newLocalRegistryAdapter() *localRegistryAdapter {
	return &localRegistryAdapter{}
}

func (a *localRegistryAdapter) List(_ context.Context) ([]wizardregistry.Wizard, error) {
	entries, err := agent.RegistryList()
	if err != nil {
		return nil, err
	}
	out := make([]wizardregistry.Wizard, 0, len(entries))
	for _, e := range entries {
		out = append(out, entryToWizard(e))
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
			return entryToWizard(e), nil
		}
	}
	return wizardregistry.Wizard{}, wizardregistry.ErrNotFound
}

func (a *localRegistryAdapter) Upsert(_ context.Context, w wizardregistry.Wizard) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Preserve fields the new Wizard struct does not carry (Worktree,
	// Tower, Phase, InstanceID) when an entry already exists.
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
		StartedAt: formatWizardStartedAt(w.StartedAt),
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
			dead = append(dead, entryToWizard(e))
		}
	}
	return dead, nil
}

// entryToWizard projects an agent.Entry into a wizardregistry.Wizard.
// pkg/agent's on-disk entries are local-native only; Mode is hardcoded.
func entryToWizard(e agent.Entry) wizardregistry.Wizard {
	return wizardregistry.Wizard{
		ID:        e.Name,
		Mode:      wizardregistry.ModeLocal,
		PID:       e.PID,
		BeadID:    e.BeadID,
		StartedAt: parseWizardStartedAt(e.StartedAt),
	}
}

func parseWizardStartedAt(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func formatWizardStartedAt(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

var _ wizardregistry.Registry = (*localRegistryAdapter)(nil)
