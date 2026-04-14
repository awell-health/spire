// backend_process.go provides backward-compatible wrappers delegating to pkg/agent.
// The real implementation lives in pkg/agent.
package main

import (
	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/config"
)

func init() {
	// Wire pkg/agent's ClearPIDFunc to cmd/spire's clearWizardPID.
	agent.ClearPIDFunc = clearWizardPID
	// Set the caller's instance ID so Kill() can distinguish same-instance
	// from cross-instance kills in log output.
	agent.CallerInstanceID = config.InstanceID()
}

// processBackend is a type alias so existing test assertions compile unchanged.
type processBackend = agent.ProcessBackend

// newProcessBackend creates a processBackend for backward compatibility.
func newProcessBackend() *processBackend {
	return agent.NewProcessBackend()
}
