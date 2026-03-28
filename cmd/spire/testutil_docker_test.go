package main

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// skipIfNoDocker skips the test if Docker is not installed or the daemon is
// not reachable. Two-stage check: LookPath for binary presence, then
// "docker info" with a 3-second timeout for daemon reachability.
func skipIfNoDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed, skipping")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		t.Skip("docker daemon not reachable, skipping")
	}
}
