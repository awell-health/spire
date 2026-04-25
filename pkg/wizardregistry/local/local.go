// Package local is the local-native concretization of the
// [wizardregistry.Registry] contract. It persists wizard registrations
// in a JSON file guarded by an OS file lock and answers liveness via
// [process.ProcessAlive], the zombie-aware PID probe.
//
// # Race-safety
//
// Sweep holds both an in-process mutex and a cross-process file lock
// across the full iteration. A concurrent Upsert serializes against
// that critical section, so a fresh upsert observed before Sweep starts
// reflects its caller's full state and one observed after Sweep
// returns is invisible to it. Either way, the fresh entry can never
// be mis-classified as dead — closing the spi-5bzu9r OrphanSweep race
// at the registry layer.
//
// # Liveness
//
// The default probe is [process.ProcessAlive], which combines a kill -0
// existence check with a platform-specific zombie probe. Zombie PIDs
// are reported dead — same fix as spi-k2bz93 but in the new package.
package local

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/awell-health/spire/pkg/process"
	"github.com/awell-health/spire/pkg/wizardregistry"
)

// Local is a file-backed [wizardregistry.Registry].
//
// All mutating methods acquire l.mu and then an exclusive flock on
// path+".lock"; List/Get/IsAlive/Sweep follow the same lock pattern so
// every operation observes a consistent on-disk state.
type Local struct {
	path  string
	mu    sync.Mutex
	probe func(wizardregistry.Wizard) bool
}

// wrapper is the on-disk shape of the registry file. The wrapping
// object lets us add metadata fields later without breaking readers.
type wrapper struct {
	Wizards []wizardregistry.Wizard `json:"wizards"`
}

// New returns a Local persisting to path. The directory containing
// path is created on demand by the first lock acquisition.
func New(path string) (*Local, error) {
	if path == "" {
		return nil, fmt.Errorf("local: empty path")
	}
	return &Local{
		path: path,
		probe: func(w wizardregistry.Wizard) bool {
			return process.ProcessAlive(w.PID)
		},
	}, nil
}

// load reads and decodes the registry file. A missing or empty file
// yields an empty wrapper.
func (l *Local) load() (wrapper, error) {
	var w wrapper
	data, err := os.ReadFile(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return w, nil
		}
		return w, fmt.Errorf("local: read %s: %w", l.path, err)
	}
	if len(data) == 0 {
		return w, nil
	}
	if err := json.Unmarshal(data, &w); err != nil {
		return w, fmt.Errorf("local: parse %s: %w", l.path, err)
	}
	return w, nil
}

// save serializes and writes the registry file, creating the parent
// directory if needed.
func (l *Local) save(w wrapper) error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return fmt.Errorf("local: mkdir: %w", err)
	}
	data, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		return fmt.Errorf("local: marshal: %w", err)
	}
	return os.WriteFile(l.path, data, 0o644)
}

// flockGuard is an acquired exclusive flock plus the file holding it.
type flockGuard struct {
	file *os.File
}

// Release unlocks and closes the lock file.
func (g *flockGuard) Release() {
	if g.file == nil {
		return
	}
	_ = syscall.Flock(int(g.file.Fd()), syscall.LOCK_UN)
	_ = g.file.Close()
	g.file = nil
}

// acquireFileLock takes a blocking exclusive flock on path+".lock".
// Blocking semantics serialize concurrent Upsert/Sweep across processes
// without polling.
func (l *Local) acquireFileLock() (*flockGuard, error) {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return nil, fmt.Errorf("local: mkdir for lock: %w", err)
	}
	f, err := os.OpenFile(l.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("local: open lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("local: flock: %w", err)
	}
	return &flockGuard{file: f}, nil
}

// List returns a copy of the registered wizards.
func (l *Local) List(_ context.Context) ([]wizardregistry.Wizard, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	g, err := l.acquireFileLock()
	if err != nil {
		return nil, err
	}
	defer g.Release()
	w, err := l.load()
	if err != nil {
		return nil, err
	}
	out := make([]wizardregistry.Wizard, len(w.Wizards))
	copy(out, w.Wizards)
	return out, nil
}

// Get returns the wizard with the given ID.
func (l *Local) Get(_ context.Context, id string) (wizardregistry.Wizard, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	g, err := l.acquireFileLock()
	if err != nil {
		return wizardregistry.Wizard{}, err
	}
	defer g.Release()
	w, err := l.load()
	if err != nil {
		return wizardregistry.Wizard{}, err
	}
	for _, e := range w.Wizards {
		if e.ID == id {
			return e, nil
		}
	}
	return wizardregistry.Wizard{}, wizardregistry.ErrNotFound
}

// Upsert adds or replaces the entry keyed by w.ID.
func (l *Local) Upsert(_ context.Context, w wizardregistry.Wizard) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	g, err := l.acquireFileLock()
	if err != nil {
		return err
	}
	defer g.Release()
	reg, err := l.load()
	if err != nil {
		return err
	}
	found := false
	for i, e := range reg.Wizards {
		if e.ID == w.ID {
			reg.Wizards[i] = w
			found = true
			break
		}
	}
	if !found {
		reg.Wizards = append(reg.Wizards, w)
	}
	return l.save(reg)
}

// Remove deletes the entry with the given ID.
func (l *Local) Remove(_ context.Context, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	g, err := l.acquireFileLock()
	if err != nil {
		return err
	}
	defer g.Release()
	reg, err := l.load()
	if err != nil {
		return err
	}
	found := false
	kept := make([]wizardregistry.Wizard, 0, len(reg.Wizards))
	for _, e := range reg.Wizards {
		if e.ID == id {
			found = true
			continue
		}
		kept = append(kept, e)
	}
	if !found {
		return wizardregistry.ErrNotFound
	}
	reg.Wizards = kept
	return l.save(reg)
}

// IsAlive reports whether the wizard with the given ID is alive
// according to a fresh probe of its underlying PID.
func (l *Local) IsAlive(_ context.Context, id string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	g, err := l.acquireFileLock()
	if err != nil {
		return false, err
	}
	defer g.Release()
	reg, err := l.load()
	if err != nil {
		return false, err
	}
	for _, e := range reg.Wizards {
		if e.ID == id {
			return l.probe(e), nil
		}
	}
	return false, wizardregistry.ErrNotFound
}

// Sweep returns the entries whose underlying process is dead. The
// scan runs while holding both l.mu and the file lock, so a concurrent
// Upsert serializes against it: a fresh upsert is never mis-classified
// as dead. Sweep does not mutate the registry — caller decides the
// follow-up action.
func (l *Local) Sweep(_ context.Context) ([]wizardregistry.Wizard, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	g, err := l.acquireFileLock()
	if err != nil {
		return nil, err
	}
	defer g.Release()
	reg, err := l.load()
	if err != nil {
		return nil, err
	}
	var dead []wizardregistry.Wizard
	for _, e := range reg.Wizards {
		if !l.probe(e) {
			dead = append(dead, e)
		}
	}
	return dead, nil
}

var _ wizardregistry.Registry = (*Local)(nil)
