// Package agent provides backend-agnostic agent invocation: spawning, listing,
// log streaming, and lifecycle management. Implementations exist for local OS
// processes and Docker containers.
//
// The key abstractions:
//   - Backend  — unified adapter for any execution environment (process, docker, k8s)
//   - Handle   — lifecycle handle for a single running agent
//   - Spawner  — subset of Backend that only creates agents
//   - Registry — file-locked wizard registry for tracking local agents
package agent

import (
	"io"
	"os"
	"time"
)

// Handle represents a running agent, regardless of execution backend.
// Implementations exist for process (local) and Docker.
type Handle interface {
	// Wait blocks until the agent exits. Returns nil on clean exit.
	Wait() error

	// Signal sends a signal to the agent.
	// Process: os signal. Docker: stop/kill.
	Signal(os.Signal) error

	// Alive returns true if the agent is still running.
	Alive() bool

	// Name returns the agent's configured name.
	Name() string

	// Identifier returns a backend-specific opaque identifier.
	// Process: PID as string. Docker: container ID.
	// Callers should not parse this — use Alive/Signal for lifecycle.
	Identifier() string
}

// Spawner creates agent processes on a specific backend.
type Spawner interface {
	Spawn(cfg SpawnConfig) (Handle, error)
}

// Backend is the unified adapter for agent execution environments.
// Every consumer (steward, wizard, status, logs) programs against this
// interface. Implementations exist for process and docker.
//
// Backend is a superset of Spawner — any function accepting
// Spawner also accepts Backend.
type Backend interface {
	// Spawn starts an agent. Returns a handle for per-agent lifecycle.
	Spawn(cfg SpawnConfig) (Handle, error)

	// List returns all agents tracked by this backend.
	List() ([]Info, error)

	// Logs returns a log stream for the named agent.
	// Returns os.ErrNotExist if no logs are available.
	Logs(name string) (io.ReadCloser, error)

	// Kill force-stops an agent by name (when no handle is available).
	Kill(name string) error
}

// Info is the backend-agnostic view of a running or recently-run agent.
type Info struct {
	Name       string    // agent name (e.g. "wizard-spi-abc")
	BeadID     string    // bead being worked on
	Phase      string    // current phase (e.g. "implement", "review")
	Alive      bool      // true if the agent is still running
	Identifier string    // opaque: PID, container ID, pod name
	StartedAt  time.Time // when the agent was started
}

// SpawnRole describes what kind of agent to run.
// Each backend maps this to its own execution mechanism.
type SpawnRole string

const (
	// RoleApprentice is a per-subtask implementer (wizard-run).
	RoleApprentice SpawnRole = "apprentice"

	// RoleSage is a per-review agent (wizard-review).
	RoleSage SpawnRole = "sage"

	// RoleWizard is the executor — handles full workflow for all workload types.
	RoleWizard SpawnRole = "wizard"

	// RoleExecutor is a formula-driven executor (execute).
	RoleExecutor SpawnRole = "executor"
)

// SpawnConfig describes the intent for spawning an agent.
// Backend-agnostic — each spawner translates this to its own mechanism.
type SpawnConfig struct {
	Name      string    // Agent name (e.g. "apprentice-spi-1dl-0")
	BeadID    string    // Bead to work on
	Role      SpawnRole // What kind of agent to run
	Tower     string    // Tower name — injected as SPIRE_TOWER into subprocess env
	ExtraArgs []string  // Additional args (e.g. "--review-fix")
	LogPath   string    // Output destination (empty = inherit stderr)
}

// NewSpawner returns a Spawner for the given backend.
// Supported: "process" (default), "docker".
//
// Deprecated: Use ResolveBackend instead, which returns the full Backend
// interface (superset of Spawner). NewSpawner now delegates to
// ResolveBackend internally.
func NewSpawner(backend string) Spawner {
	return ResolveBackend(backend)
}
