package main

// wizard_registry_adapter.go bridges pkg/registry (the legacy local-native
// wizard-tracking file at ~/.config/spire/wizards.json) to the
// wizardregistry.Registry interface. It exists for the migration window
// during which pkg/beadlifecycle and pkg/steward have moved to the new
// interface but pkg/summon still writes the actual PID into pkg/registry
// via registry.Update. Once pkg/summon migrates (sibling subtask), this
// adapter can be replaced by wizardregistry/local.New(...) directly.
//
// The adapter satisfies the Registry race-safety contract: every IsAlive
// call performs a fresh registry.List() read and a fresh
// process.ProcessAlive() probe. There is no per-sweep snapshot.

import (
	"context"
	"sync"
	"time"

	"github.com/awell-health/spire/pkg/process"
	"github.com/awell-health/spire/pkg/registry"
	"github.com/awell-health/spire/pkg/wizardregistry"
)

// localRegistryAdapter implements wizardregistry.Registry on top of the
// legacy pkg/registry file store.
type localRegistryAdapter struct {
	// mu guards the read-modify-write critical section in Upsert against
	// intra-process interleaving. The pkg/registry file lock provides
	// cross-process serialization.
	mu sync.Mutex
}

// newLocalRegistryAdapter returns the production adapter.
func newLocalRegistryAdapter() *localRegistryAdapter {
	return &localRegistryAdapter{}
}

func (a *localRegistryAdapter) List(_ context.Context) ([]wizardregistry.Wizard, error) {
	entries, err := registry.List()
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
	entries, err := registry.List()
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
	// Tower, Phase, InstanceID) when an entry already exists. pkg/summon
	// later reads Worktree off the entry it Updates with the real PID.
	existing, _ := registry.List()
	var prev *registry.Entry
	for i := range existing {
		if existing[i].Name == w.ID {
			prev = &existing[i]
			break
		}
	}
	entry := registry.Entry{
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
	return registry.Upsert(entry)
}

func (a *localRegistryAdapter) Remove(_ context.Context, id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	entries, err := registry.List()
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
	return registry.Remove(id)
}

func (a *localRegistryAdapter) IsAlive(_ context.Context, id string) (bool, error) {
	entries, err := registry.List()
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
	entries, err := registry.List()
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

// entryToWizard projects a pkg/registry.Entry into a wizardregistry.Wizard.
// The legacy on-disk entries are local-native only; Mode is hardcoded.
func entryToWizard(e registry.Entry) wizardregistry.Wizard {
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
