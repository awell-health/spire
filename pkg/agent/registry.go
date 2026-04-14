package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/awell-health/spire/pkg/config"
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
	Tower          string `json:"tower,omitempty"`
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
func LoadRegistry() Registry {
	var reg Registry
	data, err := os.ReadFile(RegistryPath())
	if err != nil {
		return reg
	}
	json.Unmarshal(data, &reg)
	return reg
}

// SaveRegistry writes the wizard registry to disk.
func SaveRegistry(reg Registry) {
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

// RegistryAdd adds or replaces an entry in the wizard registry (file-locked).
func RegistryAdd(entry Entry) error {
	unlock, err := RegistryLock()
	if err != nil {
		return err
	}
	defer unlock()

	reg := LoadRegistry()

	// Deduplicate by name — replace if exists, append if new.
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

	SaveRegistry(reg)
	return nil
}

// RegistryRemove removes an entry by name from the wizard registry (file-locked).
func RegistryRemove(name string) error {
	unlock, err := RegistryLock()
	if err != nil {
		return err
	}
	defer unlock()

	reg := LoadRegistry()

	var kept []Entry
	for _, w := range reg.Wizards {
		if w.Name != name {
			kept = append(kept, w)
		}
	}
	reg.Wizards = kept

	SaveRegistry(reg)
	return nil
}

// RegistryUpdate updates an entry by name using the provided function (file-locked).
func RegistryUpdate(name string, update func(*Entry)) error {
	unlock, err := RegistryLock()
	if err != nil {
		return err
	}
	defer unlock()

	reg := LoadRegistry()

	for i := range reg.Wizards {
		if reg.Wizards[i].Name == name {
			update(&reg.Wizards[i])
			SaveRegistry(reg)
			return nil
		}
	}
	return fmt.Errorf("wizard %q not found in registry", name)
}

// RegisterSelf registers the current process in the wizard registry and returns
// a cleanup function that removes it. Call cleanup via defer.
func RegisterSelf(name, beadID, phase string, opts ...func(*Entry)) func() {
	now := time.Now().UTC().Format(time.RFC3339)
	entry := Entry{
		Name:      name,
		PID:       os.Getpid(),
		BeadID:    beadID,
		StartedAt: now,
	}
	for _, opt := range opts {
		opt(&entry)
	}
	RegistryAdd(entry)
	return func() { RegistryRemove(name) }
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
