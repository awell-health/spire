// Package registry is the exclusive owner of the wizard registry JSON file
// (~/.config/spire/wizards.json). Local-native only — cluster-native code
// never calls this package.
package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/awell-health/spire/pkg/config"
)

// Entry is a single wizard registration.
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

// ErrNotFound is returned by Update when no entry with the given Name exists.
var ErrNotFound = errors.New("registry: entry not found")

// registryFile returns the path to the wizard registry JSON file.
func registryFile() string {
	dir, err := config.Dir()
	if err != nil {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config", "spire")
	}
	return filepath.Join(dir, "wizards.json")
}

// registryWrapper is the on-disk shape of the file, matching the legacy format.
type registryWrapper struct {
	Wizards []Entry `json:"wizards"`
}

// load reads the registry from disk. Returns an empty registry on any error.
func load() registryWrapper {
	var reg registryWrapper
	data, err := os.ReadFile(registryFile())
	if err != nil {
		return reg
	}
	_ = json.Unmarshal(data, &reg)
	return reg
}

// save writes the registry to disk.
func save(reg registryWrapper) error {
	path := registryFile()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("registry: mkdir: %w", err)
	}
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return fmt.Errorf("registry: marshal: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}

// lock acquires the file lock for the registry.
// Returns a cleanup function that releases the lock.
func lock() (func(), error) {
	lockPath := registryFile() + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0755); err != nil {
		return nil, fmt.Errorf("registry: mkdir for lock: %w", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err == nil {
			f.Close()
			return func() { os.Remove(lockPath) }, nil
		}
		if time.Now().After(deadline) {
			// Force-remove stale lock and retry once.
			os.Remove(lockPath)
			f, err = os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
			if err != nil {
				return nil, fmt.Errorf("registry: acquire lock: %w", err)
			}
			f.Close()
			return func() { os.Remove(lockPath) }, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Upsert adds or replaces an entry keyed by Name. File-locked.
func Upsert(entry Entry) error {
	unlock, err := lock()
	if err != nil {
		return err
	}
	defer unlock()

	reg := load()

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

	return save(reg)
}

// Remove deletes the entry with the given Name. Idempotent —
// removing a nonexistent entry returns nil.
func Remove(name string) error {
	unlock, err := lock()
	if err != nil {
		return err
	}
	defer unlock()

	reg := load()

	var kept []Entry
	for _, w := range reg.Wizards {
		if w.Name != name {
			kept = append(kept, w)
		}
	}
	reg.Wizards = kept

	return save(reg)
}

// Update runs the provided function against the entry with the given Name
// inside the file lock, persists, and returns ErrNotFound if no such entry
// exists.
func Update(name string, fn func(*Entry)) error {
	unlock, err := lock()
	if err != nil {
		return err
	}
	defer unlock()

	reg := load()

	for i := range reg.Wizards {
		if reg.Wizards[i].Name == name {
			fn(&reg.Wizards[i])
			return save(reg)
		}
	}
	return fmt.Errorf("%w: %q", ErrNotFound, name)
}

// List returns a snapshot of all entries.
func List() ([]Entry, error) {
	reg := load()
	entries := make([]Entry, len(reg.Wizards))
	copy(entries, reg.Wizards)
	return entries, nil
}

// pidProbe is the function used by Sweep to test whether a PID is alive.
// It can be replaced in tests.
var pidProbe = func(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil
}

// Sweep returns the subset of List() whose PID is no longer running
// (per the pid probe). Sweep does NOT remove entries — caller decides
// what to do with them.
func Sweep() ([]Entry, error) {
	entries, err := List()
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
