package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/awell-health/spire/pkg/config"
)

// requireDebugTower blocks spire debug commands from running against a
// non-debug tower. A tower is debug-approved if its name starts with
// "debug-", or is listed (whitespace-trimmed) in the comma-separated
// SPIRE_DEBUG_TOWER allowlist.
//
// Handlers MUST call this as their first statement — before any
// resolveBeadsDir() / os.Setenv("BEADS_DIR", ...) mutation or bead I/O —
// so rejection is side-effect-free. Current call sites: cmdDebugRecoveryNew,
// cmdDebugRecoveryDispatch, cmdDebugRecoveryTrace in debug.go.
func requireDebugTower() error {
	name, err := activeDebugTowerName()
	if err != nil {
		return fmt.Errorf("resolving active tower: %w", err)
	}
	if name == "" {
		return errors.New("refusing to run debug command: no active tower (use --tower debug-* or set SPIRE_TOWER)")
	}
	if isDebugTower(name, os.Getenv("SPIRE_DEBUG_TOWER")) {
		return nil
	}
	return fmt.Errorf(
		"refusing to file debug beads in tower %q — use --tower debug-* or set SPIRE_DEBUG_TOWER allowlist",
		name,
	)
}

// isDebugTower reports whether a tower name is debug-approved under the
// prefix-or-allowlist policy. Pure predicate — no env or config access —
// so it's cheap to unit-test.
func isDebugTower(name, allowlist string) bool {
	if name == "" {
		return false
	}
	if strings.HasPrefix(name, "debug-") {
		return true
	}
	if allowlist == "" {
		return false
	}
	for _, entry := range strings.Split(allowlist, ",") {
		if strings.TrimSpace(entry) == name {
			return true
		}
	}
	return false
}

// activeDebugTowerName resolves the active tower name. SPIRE_TOWER takes
// precedence as a literal name (same as the rest of the CLI) and short-
// circuits the disk load — this lets the guard run in test harnesses that
// set SPIRE_TOWER without a persisted tower config. When unset, falls
// back to config.ActiveTowerConfig. Returns "" when no tower is selected.
func activeDebugTowerName() (string, error) {
	if v := strings.TrimSpace(os.Getenv("SPIRE_TOWER")); v != "" {
		return v, nil
	}
	tc, err := config.ActiveTowerConfig()
	if err != nil {
		return "", err
	}
	if tc == nil {
		return "", nil
	}
	return tc.Name, nil
}
