package config

import (
	"fmt"
	"os"
	"strings"
)

// DetectIdentity returns the caller's agent prefix.
// Priority: --as flag > SPIRE_IDENTITY env > config issue-prefix.
func DetectIdentity(asFlag string) (string, error) {
	if asFlag != "" {
		return asFlag, nil
	}

	if env := os.Getenv("SPIRE_IDENTITY"); env != "" {
		return env, nil
	}

	// Try beads config issue-prefix via injected store config getter
	if StoreConfigGetterFunc != nil {
		out, err := StoreConfigGetterFunc("issue-prefix")
		if err == nil && out != "" {
			return out, nil
		}
	}

	return "", fmt.Errorf("cannot detect identity: set SPIRE_IDENTITY env var or use --as <name>")
}

// ParseAsFlag extracts --as <name> from args and returns (name, remaining args).
func ParseAsFlag(args []string) (string, []string) {
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

// DetectDBName returns the Dolt database name for the current context.
// Returns an error when the database cannot be determined unambiguously.
//
// Resolution order:
//  1. SPIRE_IDENTITY env → config instance lookup → identity as DB name
//  2. Store config issue-prefix
//  3. CWD → registered instance → instance's database
//  4. ResolveTowerConfig() → tower database (deterministic, errors on ambiguity)
func DetectDBName() (string, error) {
	if env := os.Getenv("SPIRE_IDENTITY"); env != "" {
		// Check config to see if this is a satellite (database != prefix)
		if cfg, err := Load(); err == nil {
			if inst, ok := cfg.Instances[env]; ok {
				return inst.Database, nil
			}
		}
		return env, nil
	}
	if StoreConfigGetterFunc != nil {
		out, err := StoreConfigGetterFunc("issue-prefix")
		if err == nil && out != "" {
			return strings.TrimSpace(out), nil
		}
	}
	// CWD instance lookup
	if cfg, err := Load(); err == nil {
		if cwd, cwdErr := RealCwd(); cwdErr == nil {
			if inst := FindInstanceByPath(cfg, cwd); inst != nil {
				return inst.Database, nil
			}
		}
	}
	// Unified tower resolution — no hardcoded fallback.
	tower, tErr := ResolveTowerConfig()
	if tErr != nil {
		return "", fmt.Errorf("cannot detect database: %w", tErr)
	}
	if tower.Database == "" {
		return "", fmt.Errorf("tower %q has no database configured", tower.Name)
	}
	return tower.Database, nil
}
