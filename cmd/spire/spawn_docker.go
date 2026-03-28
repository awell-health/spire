// spawn_docker.go provides backward-compatible type aliases for docker spawner types.
// The real implementations live in pkg/agent.
package main

import (
	"github.com/awell-health/spire/pkg/agent"
)

// dockerSpawner is a type alias for test compatibility.
type dockerSpawner = agent.DockerSpawner

// dockerHandle is a type alias for test compatibility.
type dockerHandle = agent.DockerHandle

// sanitizeContainerName delegates to pkg/agent.
func sanitizeContainerName(s string) string {
	return agent.SanitizeContainerName(s)
}
