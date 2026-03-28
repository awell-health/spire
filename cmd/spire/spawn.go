// spawn.go provides backward-compatible wrappers delegating to pkg/agent.
// cmd/spire callers continue to use unexported names; the real logic lives in
// the agent package.
package main

import (
	"github.com/awell-health/spire/pkg/agent"
)

// --- Type aliases so existing cmd/spire code compiles unchanged ---

type AgentHandle = agent.Handle
type AgentSpawner = agent.Spawner
type SpawnRole = agent.SpawnRole
type SpawnConfig = agent.SpawnConfig

// Role constants — re-exported for cmd/spire callers.
const (
	RoleApprentice = agent.RoleApprentice
	RoleSage       = agent.RoleSage
	RoleWizard     = agent.RoleWizard
	RoleExecutor   = agent.RoleExecutor
)

// NewSpawner returns an AgentSpawner for the given backend.
//
// Deprecated: Use ResolveBackend instead.
func NewSpawner(backend string) AgentSpawner {
	return agent.NewSpawner(backend)
}
