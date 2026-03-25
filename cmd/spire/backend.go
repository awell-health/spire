package main

import (
	"fmt"
	"io"
	"log"
	"time"
)

// AgentBackend is the unified adapter for agent execution environments.
// Every consumer (steward, workshop, status, logs) programs against this
// interface. Implementations exist for process, docker, and k8s.
//
// AgentBackend is a superset of AgentSpawner — any function accepting
// AgentSpawner also accepts AgentBackend.
type AgentBackend interface {
	// Spawn starts an agent. Returns a handle for per-agent lifecycle.
	Spawn(cfg SpawnConfig) (AgentHandle, error)

	// List returns all agents tracked by this backend.
	List() ([]AgentInfo, error)

	// Logs returns a log stream for the named agent.
	// Returns os.ErrNotExist if no logs are available.
	Logs(name string) (io.ReadCloser, error)

	// Kill force-stops an agent by name (when no handle is available).
	Kill(name string) error
}

// AgentInfo is the backend-agnostic view of a running or recently-run agent.
type AgentInfo struct {
	Name       string    // agent name (e.g. "wizard-spi-abc")
	BeadID     string    // bead being worked on
	Phase      string    // current phase (e.g. "implement", "review")
	Alive      bool      // true if the agent is still running
	Identifier string    // opaque: PID, container ID, pod name
	StartedAt  time.Time // when the agent was started
}

// errNotImplemented is returned by shim methods that are not yet filled in.
// Full implementations land in spi-1dl.5.3 (process) and spi-1dl.5.4 (docker).
var errNotImplemented = fmt.Errorf("not implemented")

// ---------------------------------------------------------------------------
// processBackendShim — wraps processSpawner to satisfy AgentBackend.
// Only Spawn is functional; List, Logs, Kill return errNotImplemented.
// The full processBackend implementation lands in spi-1dl.5.3.
// ---------------------------------------------------------------------------

type processBackendShim struct {
	spawner *processSpawner
}

func (b *processBackendShim) Spawn(cfg SpawnConfig) (AgentHandle, error) {
	return b.spawner.Spawn(cfg)
}

func (b *processBackendShim) List() ([]AgentInfo, error) {
	return nil, fmt.Errorf("processBackendShim.List: %w", errNotImplemented)
}

func (b *processBackendShim) Logs(name string) (io.ReadCloser, error) {
	return nil, fmt.Errorf("processBackendShim.Logs: %w", errNotImplemented)
}

func (b *processBackendShim) Kill(name string) error {
	return fmt.Errorf("processBackendShim.Kill: %w", errNotImplemented)
}

// ---------------------------------------------------------------------------
// Compile-time interface checks
// ---------------------------------------------------------------------------

var _ AgentBackend = (*processBackendShim)(nil)
var _ AgentBackend = (*dockerBackend)(nil)

// ---------------------------------------------------------------------------
// ResolveBackend returns an AgentBackend for the given backend name.
//
//   - "process" or "" → processBackendShim (wraps processSpawner)
//   - "docker"        → dockerBackend      (full implementation)
//   - unknown         → log warning, fall back to process
//
// ResolveBackend replaces NewSpawner as the preferred factory.
// ---------------------------------------------------------------------------

func ResolveBackend(name string) AgentBackend {
	switch name {
	case "process", "":
		return &processBackendShim{spawner: &processSpawner{}}
	case "docker":
		return newDockerBackend()
	default:
		log.Printf("[backend] unknown backend %q, falling back to process", name)
		return &processBackendShim{spawner: &processSpawner{}}
	}
}
