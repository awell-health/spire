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

	// Try bd config get issue-prefix
	out, err := bd("config", "get", "issue-prefix")
	if err == nil && out != "" && !strings.Contains(out, "(not set)") {
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

// detectDBName returns the Dolt database name.
// For hubs/standalones this is the prefix; for satellites it's the hub's prefix.
func detectDBName() string {
	if env := os.Getenv("SPIRE_IDENTITY"); env != "" {
		// Check config to see if this is a satellite (database != prefix)
		if cfg, err := loadConfig(); err == nil {
			if inst, ok := cfg.Instances[env]; ok {
				return inst.Database
			}
		}
		return env
	}
	out, err := bd("config", "get", "issue-prefix")
	if err == nil && out != "" && !strings.Contains(out, "(not set)") {
		return strings.TrimSpace(out)
	}
	// Fallback: look up cwd in config
	if cwd, err := os.Getwd(); err == nil {
		if cfg, err := loadConfig(); err == nil {
			if inst := findInstanceByPath(cfg, cwd); inst != nil {
				return inst.Database
			}
		}
	}
	return "spi" // fallback
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
