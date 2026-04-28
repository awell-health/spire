package config

import "time"

const (
	// DefaultAgentStaleText is the repo-config text form of the default stale threshold.
	DefaultAgentStaleText = "10m"

	// DefaultAgentShutdownText is the repo-config text form of the default shutdown threshold.
	DefaultAgentShutdownText = "60m"

	// DefaultAgentStaleThreshold is the warning threshold for in-progress work
	// when a repo config does not override agent.stale.
	DefaultAgentStaleThreshold = 10 * time.Minute

	// DefaultAgentShutdownThreshold is the kill/orphan threshold for
	// in-progress work when a repo config does not override agent.timeout.
	DefaultAgentShutdownThreshold = 60 * time.Minute
)
