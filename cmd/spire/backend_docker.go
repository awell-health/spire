// backend_docker.go provides backward-compatible wrappers delegating to pkg/agent.
// The real implementation lives in pkg/agent.
package main

import (
	"github.com/awell-health/spire/pkg/agent"
)

// dockerBackend is a type alias so existing test assertions compile unchanged.
type dockerBackend = agent.DockerBackend

// parseDockerInspect delegates to pkg/agent for backward compatibility with tests.
func parseDockerInspect(id, line string) (AgentInfo, error) {
	return agent.ParseDockerInspect(id, line)
}

// defaultDockerImage is re-exported for test compatibility.
const defaultDockerImage = agent.DefaultDockerImage
