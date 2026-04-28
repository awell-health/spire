package repoconfig

import (
	"testing"
	"time"

	spireconfig "github.com/awell-health/spire/pkg/config"
)

func TestAgentTimeoutDurations_Defaults(t *testing.T) {
	stale, timeout, warnings := AgentTimeoutDurations(nil)

	if stale != spireconfig.DefaultAgentStaleThreshold {
		t.Fatalf("stale = %s, want %s", stale, spireconfig.DefaultAgentStaleThreshold)
	}
	if timeout != spireconfig.DefaultAgentShutdownThreshold {
		t.Fatalf("timeout = %s, want %s", timeout, spireconfig.DefaultAgentShutdownThreshold)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %d, want 0", len(warnings))
	}
}

func TestAgentTimeoutDurations_ConfigOverride(t *testing.T) {
	cfg := &RepoConfig{
		Agent: AgentConfig{
			Stale:   "12m",
			Timeout: "75m",
		},
	}

	stale, timeout, warnings := AgentTimeoutDurations(cfg)

	if stale != 12*time.Minute {
		t.Fatalf("stale = %s, want 12m", stale)
	}
	if timeout != 75*time.Minute {
		t.Fatalf("timeout = %s, want 75m", timeout)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %d, want 0", len(warnings))
	}
}

func TestAgentTimeoutDurations_InvalidValuesFallBack(t *testing.T) {
	cfg := &RepoConfig{
		Agent: AgentConfig{
			Stale:   "not-a-duration",
			Timeout: "still-bad",
		},
	}

	stale, timeout, warnings := AgentTimeoutDurations(cfg)

	if stale != spireconfig.DefaultAgentStaleThreshold {
		t.Fatalf("stale = %s, want %s", stale, spireconfig.DefaultAgentStaleThreshold)
	}
	if timeout != spireconfig.DefaultAgentShutdownThreshold {
		t.Fatalf("timeout = %s, want %s", timeout, spireconfig.DefaultAgentShutdownThreshold)
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings = %d, want 2", len(warnings))
	}
}
