package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/process"
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

// ErrNotFound is returned by RegistryUpdate when no entry with the given Name exists.
var ErrNotFound = errors.New("agent: registry entry not found")

// registryMu serializes intra-process read-modify-write critical sections.
// Cross-process serialization is provided by the file lock below.
var registryMu sync.Mutex

// RegistryPath returns the path to the wizard registry JSON file.
func RegistryPath() string {
	dir, err := config.Dir()
	if err != nil {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config", "spire")
	}
	return filepath.Join(dir, "wizards.json")
}

// loadRegistry reads the registry from disk. Returns an empty Registry on any error.
func loadRegistry() Registry {
	var reg Registry
	data, err := os.ReadFile(RegistryPath())
	if err != nil {
		return reg
	}
	_ = json.Unmarshal(data, &reg)
	return reg
}

// saveRegistry writes the registry to disk.
func saveRegistry(reg Registry) error {
	path := RegistryPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("agent registry: mkdir: %w", err)
	}
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return fmt.Errorf("agent registry: marshal: %w", err)
	}
	return os.WriteFile(path, data, 0o644)
}

// RegistryLock acquires a file lock for the wizard registry.
// Returns a cleanup function that releases the lock.
func RegistryLock() (func(), error) {
	lockPath := RegistryPath() + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, fmt.Errorf("agent registry: mkdir for lock: %w", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			f.Close()
			return func() { os.Remove(lockPath) }, nil
		}
		if time.Now().After(deadline) {
			os.Remove(lockPath)
			f, err = os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
			if err != nil {
				return nil, fmt.Errorf("agent registry: acquire lock: %w", err)
			}
			f.Close()
			return func() { os.Remove(lockPath) }, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// LoadRegistry reads the wizard registry from disk.
func LoadRegistry() Registry {
	return loadRegistry()
}

// SaveRegistry writes the wizard registry to disk.
// Non-atomic; prefer RegistryAdd/RegistryRemove/RegistryUpdate.
func SaveRegistry(reg Registry) {
	path := RegistryPath()
	os.MkdirAll(filepath.Dir(path), 0o755)
	data, _ := json.MarshalIndent(reg, "", "  ")
	os.WriteFile(path, data, 0o644)
}

// RegistryAdd adds or replaces an entry keyed by Name. File-locked.
func RegistryAdd(entry Entry) error {
	registryMu.Lock()
	defer registryMu.Unlock()
	unlock, err := RegistryLock()
	if err != nil {
		return err
	}
	defer unlock()

	reg := loadRegistry()
	found := false
	for i, w := range reg.Wizards {
		if w.Name == entry.Name {
			reg.Wizards[i] = entry
			found = true
			break
		}
	}
	if !found {
		reg.Wizards = append(reg.Wizards, entry)
	}
	return saveRegistry(reg)
}

// RegistryRemove deletes the entry with the given Name. Idempotent.
func RegistryRemove(name string) error {
	registryMu.Lock()
	defer registryMu.Unlock()
	unlock, err := RegistryLock()
	if err != nil {
		return err
	}
	defer unlock()

	reg := loadRegistry()
	var kept []Entry
	for _, w := range reg.Wizards {
		if w.Name != name {
			kept = append(kept, w)
		}
	}
	reg.Wizards = kept
	return saveRegistry(reg)
}

// RegistryUpdate runs fn against the entry with the given Name inside the
// file lock, persists the result, and returns ErrNotFound if no such entry
// exists.
func RegistryUpdate(name string, fn func(*Entry)) error {
	registryMu.Lock()
	defer registryMu.Unlock()
	unlock, err := RegistryLock()
	if err != nil {
		return err
	}
	defer unlock()

	reg := loadRegistry()
	for i := range reg.Wizards {
		if reg.Wizards[i].Name == name {
			fn(&reg.Wizards[i])
			return saveRegistry(reg)
		}
	}
	return fmt.Errorf("%w: %q", ErrNotFound, name)
}

// RegistryList returns a snapshot of all entries.
func RegistryList() ([]Entry, error) {
	reg := loadRegistry()
	entries := make([]Entry, len(reg.Wizards))
	copy(entries, reg.Wizards)
	return entries, nil
}

// pidProbe is the function used by RegistrySweep to test whether a PID is
// alive. Replaceable in tests. Defaults to process.ProcessAlive, which is
// zombie-safe (zombies report as dead).
var pidProbe = process.ProcessAlive

// RegistrySweep returns the subset of RegistryList() whose PID is no longer
// running per pidProbe. Sweep does NOT remove entries — caller decides what
// to do with them.
func RegistrySweep() ([]Entry, error) {
	entries, err := RegistryList()
	if err != nil {
		return nil, err
	}
	var dead []Entry
	for _, e := range entries {
		if !pidProbe(e.PID) {
			dead = append(dead, e)
		}
	}
	return dead, nil
}

// WithInstanceID returns an option function that sets the InstanceID field on an Entry.
// Used by callers that construct entries manually.
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
