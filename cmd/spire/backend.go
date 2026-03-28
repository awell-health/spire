// backend.go provides backward-compatible wrappers delegating to pkg/agent.
// cmd/spire callers continue to use unexported names; the real logic lives in
// the agent package.
package main

import (
	"github.com/awell-health/spire/pkg/agent"
)

// --- Type aliases so existing cmd/spire code compiles unchanged ---

type AgentBackend = agent.Backend
type AgentInfo = agent.Info

// ResolveBackend returns a Backend for the given backend name.
func ResolveBackend(name string) AgentBackend {
	return agent.ResolveBackend(name)
}
