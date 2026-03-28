// spawn_process.go provides backward-compatible type aliases for process spawner types.
// The real implementations live in pkg/agent.
package main

import (
	"github.com/awell-health/spire/pkg/agent"
)

// processSpawner is a type alias for test compatibility.
type processSpawner = agent.ProcessSpawner

// processHandle is a type alias for test compatibility.
type processHandle = agent.ProcessHandle
