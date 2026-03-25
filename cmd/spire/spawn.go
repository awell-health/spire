package main

import (
	"log"
	"os"
)

// AgentHandle represents a running agent, regardless of execution backend.
// Implementations exist for process (local), Docker, and Kubernetes.
type AgentHandle interface {
	// Wait blocks until the agent exits. Returns nil on clean exit.
	Wait() error

	// Signal sends a signal to the agent.
	// Process: os signal. Docker: stop/kill. K8s: pod delete.
	Signal(os.Signal) error

	// Alive returns true if the agent is still running.
	Alive() bool

	// Name returns the agent's configured name.
	Name() string

	// Identifier returns a backend-specific opaque identifier.
	// Process: PID as string. Docker: container ID. K8s: pod name.
	// Callers should not parse this — use Alive/Signal for lifecycle.
	Identifier() string
}

// AgentSpawner creates agent processes on a specific backend.
type AgentSpawner interface {
	Spawn(cfg SpawnConfig) (AgentHandle, error)
}

// SpawnRole describes what kind of agent to run.
// Each backend maps this to its own execution mechanism.
type SpawnRole string

const (
	// RoleApprentice is a per-subtask implementer (wizard-run).
	RoleApprentice SpawnRole = "apprentice"

	// RoleSage is a per-review agent (wizard-review).
	RoleSage SpawnRole = "sage"

	// RoleWizard is a per-epic orchestrator (workshop).
	RoleWizard SpawnRole = "wizard"
)

// SpawnConfig describes the intent for spawning an agent.
// Backend-agnostic — each spawner translates this to its own mechanism.
type SpawnConfig struct {
	Name      string    // Agent name (e.g. "apprentice-spi-1dl-0")
	BeadID    string    // Bead to work on
	Role      SpawnRole // What kind of agent to run
	ExtraArgs []string  // Additional args (e.g. "--review-fix")
	LogPath   string    // Output destination (empty = inherit stderr)
}

// NewSpawner returns an AgentSpawner for the given backend.
// Supported: "process" (default). Future: "docker", "k8s".
func NewSpawner(backend string) AgentSpawner {
	switch backend {
	case "process", "":
		return &processSpawner{}
	default:
		// TODO(spi-1dl.2): "docker" → &dockerSpawner{}
		// TODO(spi-1dl.5): "k8s" → &k8sSpawner{}
		log.Printf("[spawn] unknown backend %q, falling back to process", backend)
		return &processSpawner{}
	}
}
