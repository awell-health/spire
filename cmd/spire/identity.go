package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// detectIdentity returns the caller's agent prefix.
// Priority: --as flag > SPIRE_IDENTITY env > config issue-prefix.
func detectIdentity(asFlag string) (string, error) {
	if asFlag != "" {
		return asFlag, nil
	}

	if env := os.Getenv("SPIRE_IDENTITY"); env != "" {
		return env, nil
	}

	// Try beads config issue-prefix
	out, err := storeGetConfig("issue-prefix")
	if err == nil && out != "" {
		return out, nil
	}

	return "", fmt.Errorf("cannot detect identity: set SPIRE_IDENTITY env var or use --as <name>")
}

// parseAsFlag extracts --as <name> from args and returns (name, remaining args).
func parseAsFlag(args []string) (string, []string) {
	for i, arg := range args {
		if arg == "--as" && i+1 < len(args) {
			remaining := make([]string, 0, len(args)-2)
			remaining = append(remaining, args[:i]...)
			remaining = append(remaining, args[i+2:]...)
			return args[i+1], remaining
		}
	}
	return "", args
}

// Bead represents a beads issue from bd JSON output.
// Note: bd show --json returns an array; use parseBead to handle this.
type Bead struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	Priority    int      `json:"priority"`
	Type        string   `json:"issue_type"`
	Labels      []string `json:"labels"`
	Parent      string   `json:"parent"`
}

// detectDBName returns the Dolt database name for the current context.
// Returns an error when the database cannot be determined unambiguously.
//
// Resolution order:
//  1. SPIRE_IDENTITY env → config instance lookup → identity as DB name
//  2. Store config issue-prefix
//  3. CWD → registered instance → instance's database
//  4. resolveTowerConfig() → tower database (deterministic, errors on ambiguity)
func detectDBName() (string, error) {
	if env := os.Getenv("SPIRE_IDENTITY"); env != "" {
		// Check config to see if this is a satellite (database != prefix)
		if cfg, err := loadConfig(); err == nil {
			if inst, ok := cfg.Instances[env]; ok {
				return inst.Database, nil
			}
		}
		return env, nil
	}
	out, err := storeGetConfig("issue-prefix")
	if err == nil && out != "" {
		return strings.TrimSpace(out), nil
	}
	// CWD instance lookup
	if cfg, err := loadConfig(); err == nil {
		if cwd, cwdErr := realCwd(); cwdErr == nil {
			if inst := findInstanceByPath(cfg, cwd); inst != nil {
				return inst.Database, nil
			}
		}
	}
	// Unified tower resolution — no hardcoded fallback.
	tower, tErr := resolveTowerConfig()
	if tErr != nil {
		return "", fmt.Errorf("cannot detect database: %w", tErr)
	}
	if tower.Database == "" {
		return "", fmt.Errorf("tower %q has no database configured", tower.Name)
	}
	return tower.Database, nil
}

// parseBead parses a bead from bd show --json output (which returns an array).
func parseBead(data []byte) (Bead, error) {
	var beads []Bead
	if err := json.Unmarshal(data, &beads); err != nil {
		return Bead{}, err
	}
	if len(beads) == 0 {
		return Bead{}, fmt.Errorf("no bead found")
	}
	return beads[0], nil
}
