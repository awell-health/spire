package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/registry"
)

// Registry tracks locally summoned wizards.
type Registry struct {
	Wizards []Entry `json:"wizards"`
}

// Entry is a single wizard in the registry.
type Entry struct {
	Name           string `json:"name"`
	PID            int    `json:"pid"`
	BeadID         string `json:"bead_id"`
	Worktree       string `json:"worktree"`
	StartedAt      string `json:"started_at"`
	Phase          string `json:"phase,omitempty"`
	PhaseStartedAt string `json:"phase_started_at,omitempty"`
	Tower          string `json:"tower,omitempty"`
	InstanceID     string `json:"instance_id,omitempty"`
}

// RegistryPath returns the path to the wizard registry JSON file.
func RegistryPath() string {
	dir, err := config.Dir()
	if err != nil {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config", "spire")
	}
	return filepath.Join(dir, "wizards.json")
}

// LoadRegistry reads the wizard registry from disk.
// DEPRECATED: migrated to pkg/registry. Use registry.List instead.
func LoadRegistry() Registry {
	entries, err := registry.List()
	if err != nil {
		return Registry{}
	}
	var reg Registry
	for _, e := range entries {
		reg.Wizards = append(reg.Wizards, fromRegistryEntry(e))
	}
	return reg
}

// SaveRegistry writes the wizard registry to disk.
// DEPRECATED: migrated to pkg/registry. Direct callers should migrate to the
// atomic Upsert/Remove/Update operations.
func SaveRegistry(reg Registry) {
	// Reconstruct from the provided registry by replacing all entries.
	// This mirrors the legacy behaviour: overwrite the file with whatever
	// the caller built in-memory. Not atomic — prefer Upsert/Remove.
	path := RegistryPath()
	os.MkdirAll(filepath.Dir(path), 0755)
	data, _ := json.MarshalIndent(reg, "", "  ")
	os.WriteFile(path, data, 0644)
}

// RegistryLock acquires a file lock for the wizard registry.
// Returns a cleanup function that releases the lock.
func RegistryLock() (func(), error) {
	lockPath := RegistryPath() + ".lock"
	os.MkdirAll(filepath.Dir(lockPath), 0755)

	deadline := time.Now().Add(5 * time.Second)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err == nil {
			f.Close()
			return func() { os.Remove(lockPath) }, nil
		}
		if time.Now().After(deadline) {
			// Force-remove stale lock and retry once
			os.Remove(lockPath)
			f, err = os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
			if err != nil {
				return nil, fmt.Errorf("acquire registry lock: %w", err)
			}
			f.Close()
			return func() { os.Remove(lockPath) }, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// toRegistryEntry converts an agent.Entry to a registry.Entry.
func toRegistryEntry(e Entry) registry.Entry {
	return registry.Entry{
		Name:           e.Name,
		PID:            e.PID,
		BeadID:         e.BeadID,
		Worktree:       e.Worktree,
		StartedAt:      e.StartedAt,
		Phase:          e.Phase,
		PhaseStartedAt: e.PhaseStartedAt,
		Tower:          e.Tower,
		InstanceID:     e.InstanceID,
	}
}

// fromRegistryEntry converts a registry.Entry to an agent.Entry.
func fromRegistryEntry(e registry.Entry) Entry {
	return Entry{
		Name:           e.Name,
		PID:            e.PID,
		BeadID:         e.BeadID,
		Worktree:       e.Worktree,
		StartedAt:      e.StartedAt,
		Phase:          e.Phase,
		PhaseStartedAt: e.PhaseStartedAt,
		Tower:          e.Tower,
		InstanceID:     e.InstanceID,
	}
}

// RegistryAdd adds or replaces an entry in the wizard registry (file-locked).
// DEPRECATED: migrated to pkg/registry. Use registry.Upsert instead.
func RegistryAdd(entry Entry) error {
	return registry.Upsert(toRegistryEntry(entry))
}

// RegistryRemove removes an entry by name from the wizard registry (file-locked).
// DEPRECATED: migrated to pkg/registry. Use registry.Remove instead.
func RegistryRemove(name string) error {
	return registry.Remove(name)
}

// RegistryUpdate updates an entry by name using the provided function (file-locked).
// DEPRECATED: migrated to pkg/registry. Use registry.Update instead.
func RegistryUpdate(name string, update func(*Entry)) error {
	return registry.Update(name, func(re *registry.Entry) {
		ae := fromRegistryEntry(*re)
		update(&ae)
		*re = toRegistryEntry(ae)
	})
}

// WithInstanceID returns an option function that sets the InstanceID field on an Entry.
// Used by callers that construct entries manually (e.g. tests, registry.Update closures).
func WithInstanceID(id string) func(*Entry) {
	return func(e *Entry) {
		e.InstanceID = id
	}
}

// FindLiveForBead returns the first registry entry for the given bead, or nil.
// The caller is expected to have already cleaned dead wizards from the registry.
func FindLiveForBead(reg Registry, beadID string) *Entry {
	for i := range reg.Wizards {
		if reg.Wizards[i].BeadID == beadID {
			return &reg.Wizards[i]
		}
	}
	return nil
}

// WizardsForTower returns wizards matching the given tower (or all if tower is "").
func WizardsForTower(reg Registry, tower string) []Entry {
	if tower == "" {
		return reg.Wizards
	}
	var result []Entry
	for _, w := range reg.Wizards {
		if w.Tower == tower {
			result = append(result, w)
		}
	}
	return result
}
