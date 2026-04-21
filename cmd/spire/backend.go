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

// ResolveBackend returns a Backend for the given backend name, reading
// agent.backend from spire.yaml via the process's current working
// directory when name is empty. Prefer resolveBackendForBead when a
// bead ID is in scope so backend selection does not depend on ambient
// CWD. See spi-vrzhf.
func ResolveBackend(name string) AgentBackend {
	return agent.ResolveBackend(name)
}

// resolveBackendForBead resolves the backend for the given bead, reading
// agent.backend from the bead's registered-repo spire.yaml rather than
// cwd. Falls back to cwd-based resolution when the repo path cannot be
// determined (e.g. the bead's prefix is not yet registered on this
// machine). Use this in executor/wizard dispatch sites where a bead ID
// is available.
func resolveBackendForBead(beadID string) AgentBackend {
	repoPath := ""
	if beadID != "" {
		if rp, _, _, err := wizardResolveRepo(beadID); err == nil {
			repoPath = rp
		}
	}
	return agent.ResolveBackendForRepo("", repoPath)
}
