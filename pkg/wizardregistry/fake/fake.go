// Package fake provides a deterministic in-memory
// [wizardregistry.Registry] implementation for use in caller tests.
//
// The fake is intentionally minimal: a single mutex protects two maps
// (wizards by ID and liveness by ID). Every method holds the mutex for
// its full critical section, so the fake is correct-by-construction
// against the race-safety guarantee documented in
// [wizardregistry.Registry] — making it suitable as the reference
// backend that the conformance suite uses to validate itself.
package fake

import (
	"context"
	"sync"

	"github.com/awell-health/spire/pkg/wizardregistry"
	"github.com/awell-health/spire/pkg/wizardregistry/conformance"
)

// Registry is an in-memory [wizardregistry.Registry] plus
// [conformance.Control].
type Registry struct {
	mu      sync.Mutex
	wizards map[string]wizardregistry.Wizard
	alive   map[string]bool
}

// New returns a fresh, empty Registry.
func New() *Registry {
	return &Registry{
		wizards: make(map[string]wizardregistry.Wizard),
		alive:   make(map[string]bool),
	}
}

// List returns a copy of the registered wizards.
func (r *Registry) List(_ context.Context) ([]wizardregistry.Wizard, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]wizardregistry.Wizard, 0, len(r.wizards))
	for _, w := range r.wizards {
		out = append(out, w)
	}
	return out, nil
}

// Get returns the wizard with the given ID or
// [wizardregistry.ErrNotFound].
func (r *Registry) Get(_ context.Context, id string) (wizardregistry.Wizard, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	w, ok := r.wizards[id]
	if !ok {
		return wizardregistry.Wizard{}, wizardregistry.ErrNotFound
	}
	return w, nil
}

// Upsert stores w keyed by w.ID.
func (r *Registry) Upsert(_ context.Context, w wizardregistry.Wizard) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.wizards[w.ID] = w
	return nil
}

// Remove deletes the wizard with the given ID and its liveness flag.
// Returns [wizardregistry.ErrNotFound] if no entry exists.
func (r *Registry) Remove(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.wizards[id]; !ok {
		return wizardregistry.ErrNotFound
	}
	delete(r.wizards, id)
	delete(r.alive, id)
	return nil
}

// IsAlive reports the liveness flag for id. Returns
// [wizardregistry.ErrNotFound] if no entry exists. The lookup and the
// flag read happen in a single critical section — there is no separate
// snapshot phase.
func (r *Registry) IsAlive(_ context.Context, id string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.wizards[id]; !ok {
		return false, wizardregistry.ErrNotFound
	}
	return r.alive[id], nil
}

// Sweep returns the entries whose liveness flag is false. The whole
// scan runs inside a single critical section, so a concurrent Upsert
// either lands fully before the scan starts or after it ends — never
// during.
func (r *Registry) Sweep(_ context.Context) ([]wizardregistry.Wizard, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var dead []wizardregistry.Wizard
	for id, w := range r.wizards {
		if !r.alive[id] {
			dead = append(dead, w)
		}
	}
	return dead, nil
}

// SetAlive sets the liveness flag for id. The id need not be registered
// — this mirrors how a real authoritative source can have a process
// exist before it appears in the registry.
func (r *Registry) SetAlive(id string, alive bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.alive[id] = alive
}

var (
	_ wizardregistry.Registry = (*Registry)(nil)
	_ conformance.Control     = (*Registry)(nil)
)
