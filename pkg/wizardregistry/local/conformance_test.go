package local

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/awell-health/spire/pkg/wizardregistry"
	"github.com/awell-health/spire/pkg/wizardregistry/conformance"
)

// TestConformance runs the shared conformance suite against the Local
// backend, with a test-only probe that consults a Control's ID-keyed
// liveness map. The default process-based probe can't simulate the
// arbitrary PIDs the conformance suite uses (1234, 4242, etc.), so the
// factory wires the unexported l.probe field to the control directly.
func TestConformance(t *testing.T) {
	conformance.Run(t, func(t *testing.T) (wizardregistry.Registry, conformance.Control) {
		ctl := newTestControl()
		l, err := New(filepath.Join(t.TempDir(), "wizards.json"))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		l.probe = ctl.probe
		return l, ctl
	})
}

// testControl is a [conformance.Control] adapter that maps wizard ID
// to liveness for the conformance suite. Production callers don't use
// this — the production probe is process.ProcessAlive against PID.
type testControl struct {
	mu    sync.Mutex
	alive map[string]bool
}

func newTestControl() *testControl {
	return &testControl{alive: make(map[string]bool)}
}

// SetAlive flips the liveness flag for id.
func (c *testControl) SetAlive(id string, alive bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.alive[id] = alive
}

// probe is the bridge consulted by Local.IsAlive/Sweep when the
// control is wired in via the test factory.
func (c *testControl) probe(w wizardregistry.Wizard) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.alive[w.ID]
}
