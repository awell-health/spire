package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/beads"
)

// ErrAmbiguousTower is returned when multiple towers exist and none is active.
// Callers can use errors.Is(err, ErrAmbiguousTower) for reliable detection.
var ErrAmbiguousTower = errors.New("ambiguous tower")

// SpireConfig is the global Spire configuration stored at ~/.config/spire/config.json.
type SpireConfig struct {
	Shell        ShellConfig          `json:"shell"`
	Instances    map[string]*Instance `json:"instances"`
	ActiveTower  string               `json:"active_tower,omitempty"` // name of active tower
	MCPServer    string               `json:"mcp_server_path,omitempty"`
	EditorCursor *bool                `json:"editor_cursor,omitempty"` // default: true
	EditorClaude *bool                `json:"editor_claude,omitempty"` // default: true
}

// ShellConfig tracks whether shell env vars have been injected.
type ShellConfig struct {
	Configured bool   `json:"configured"`
	Profile    string `json:"profile,omitempty"`
}

// Instance represents one registered repo.
type Instance struct {
	Path           string   `json:"path"`
	Paths          []string `json:"paths,omitempty"`           // additional directories (e.g. git worktrees)
	Prefix         string   `json:"prefix"`
	Database       string   `json:"database"`
	DoltPort       int      `json:"dolt_port,omitempty"`       // default: 3307
	DaemonInterval string   `json:"daemon_interval,omitempty"` // default: "2m"
	DolthubRemote  string   `json:"dolthub_remote,omitempty"`
	Identity       string   `json:"identity,omitempty"`        // default: prefix
	Tower          string   `json:"tower,omitempty"`           // tower name this instance belongs to
}

// realCwd returns the current working directory with symlinks resolved.
// This ensures paths stored in config are canonical regardless of how the
// user navigated to the directory (e.g. /tmp vs /private/tmp on macOS).
func realCwd() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return cwd, nil // fallback to unresolved
	}
	return real, nil
}

// configDir returns the Spire configuration directory, creating it if needed.
// If SPIRE_CONFIG_DIR is set, that path is used (useful for Docker containers).
// Otherwise falls back to ~/.config/spire/.
func configDir() (string, error) {
	if d := os.Getenv("SPIRE_CONFIG_DIR"); d != "" {
		if err := os.MkdirAll(d, 0755); err != nil {
			return "", err
		}
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".config", "spire")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	return dir, nil
}

// configPath returns ~/.config/spire/config.json.
func configPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// loadConfig reads and unmarshals the config. Returns an empty config if the file is missing.
func loadConfig() (*SpireConfig, error) {
	p, err := configPath()
	if err != nil {
		return &SpireConfig{Instances: make(map[string]*Instance)}, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return &SpireConfig{Instances: make(map[string]*Instance)}, nil
		}
		return nil, err
	}
	var cfg SpireConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if cfg.Instances == nil {
		cfg.Instances = make(map[string]*Instance)
	}
	return &cfg, nil
}

// saveConfig marshals and writes the config to disk.
func saveConfig(cfg *SpireConfig) error {
	p, err := configPath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(data, '\n'), 0644)
}

// findInstanceByPath looks up an instance by any of its registered paths (primary or worktree).
// Also matches if path is a subdirectory of a registered path.
func findInstanceByPath(cfg *SpireConfig, path string) *Instance {
	// Exact match first
	for _, inst := range cfg.Instances {
		if inst.Path == path {
			return inst
		}
		for _, p := range inst.Paths {
			if p == path {
				return inst
			}
		}
	}
	// Subdirectory match — CWD may be inside a registered repo
	for _, inst := range cfg.Instances {
		if inst.Path != "" && strings.HasPrefix(path, inst.Path+"/") {
			return inst
		}
		for _, p := range inst.Paths {
			if p != "" && strings.HasPrefix(path, p+"/") {
				return inst
			}
		}
	}
	return nil
}

// allPaths returns all registered paths for an instance (primary + worktrees).
func allPaths(inst *Instance) []string {
	paths := []string{inst.Path}
	paths = append(paths, inst.Paths...)
	return paths
}

// removeFromPaths removes a path from inst.Paths (not the primary Path).
func removeFromPaths(inst *Instance, path string) {
	updated := inst.Paths[:0]
	for _, p := range inst.Paths {
		if p != path {
			updated = append(updated, p)
		}
	}
	inst.Paths = updated
}

// resolveTowerConfig determines the active tower using explicit, deterministic
// resolution. Returns an error when the tower cannot be determined unambiguously.
// Loads config from disk. For callers that already have a SpireConfig, use
// resolveTowerConfigWith.
//
// Resolution order:
//  1. SPIRE_TOWER env var (explicit tower name from --tower flag or env)
//  2. CWD → registered instance → instance's tower
//  3. Active tower (cfg.ActiveTower)
//  4. Sole tower on disk (exactly one tower config file)
//  5. Error: no tower found or ambiguous (multiple towers, none active)
func resolveTowerConfig() (*TowerConfig, error) {
	// 1. SPIRE_TOWER env override (from --tower flag or explicit env).
	if towerName := os.Getenv("SPIRE_TOWER"); towerName != "" {
		tower, err := loadTowerConfig(towerName)
		if err != nil {
			return nil, fmt.Errorf("SPIRE_TOWER=%q: %w", towerName, err)
		}
		return tower, nil
	}

	cfg, err := loadConfig()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	return resolveTowerConfigWith(cfg)
}

// resolveTowerConfigWith is the inner resolution logic that uses a pre-loaded
// SpireConfig. This allows callers (like resolveDatabase) that already have a
// config to pass it in, so test-injected values (e.g. ActiveTower) are respected.
//
// Resolution order (after SPIRE_TOWER, handled by resolveTowerConfig):
//  2. CWD → registered instance → instance's tower
//  3. Active tower (cfg.ActiveTower)
//  4. Sole tower on disk (exactly one tower config file)
//  5. Error: no tower found or ambiguous (multiple towers, none active)
func resolveTowerConfigWith(cfg *SpireConfig) (*TowerConfig, error) {
	// 2. CWD → registered instance → instance's tower.
	if cwd, cwdErr := realCwd(); cwdErr == nil {
		if inst := findInstanceByPath(cfg, cwd); inst != nil && inst.Tower != "" {
			tower, tErr := loadTowerConfig(inst.Tower)
			if tErr != nil {
				// Broken tower reference is likely a misconfiguration — log it.
				log.Printf("[tower] CWD instance references tower %q but config failed to load: %v", inst.Tower, tErr)
			} else {
				return tower, nil
			}
		}
	}

	// 3. Active tower from global config.
	if cfg.ActiveTower != "" {
		if tower, tErr := loadTowerConfig(cfg.ActiveTower); tErr == nil {
			return tower, nil
		}
	}

	// 4. Sole tower on disk — refuse to guess when ambiguous.
	towers, tErr := listTowerConfigs()
	if tErr != nil {
		return nil, fmt.Errorf("list towers: %w", tErr)
	}
	switch len(towers) {
	case 0:
		return nil, fmt.Errorf("no tower configured — run 'spire tower create' or 'spire tower attach'")
	case 1:
		return &towers[0], nil
	default:
		names := make([]string, len(towers))
		for i, t := range towers {
			names[i] = t.Name
		}
		return nil, fmt.Errorf("%w: multiple towers found (%s) — run 'spire tower use <name>' to set the active tower",
			ErrAmbiguousTower, strings.Join(names, ", "))
	}
}

// resolveBeadsDir returns a .beads/ directory path that can be used to open a store.
//
// Resolution order:
//  1. BEADS_DIR env var (explicit override)
//  2. resolveTowerConfig() → tower database directory (deterministic, errors on ambiguity)
//  3. beads.FindBeadsDir() (walk up from CWD — may find repo stubs)
//
// Tower paths are checked before CWD because spire repo add creates .beads/ stubs
// in repo directories that lack the dolt/ subdirectory. The beads library needs
// the full database context to work in server mode.
func resolveBeadsDir() string {
	if d := os.Getenv("BEADS_DIR"); d != "" {
		return d
	}

	// Unified tower resolution — no ambient "any tower" fallback.
	if tower, err := resolveTowerConfig(); err == nil && tower.Database != "" {
		d := filepath.Join(doltDataDir(), tower.Database, ".beads")
		if info, sErr := os.Stat(d); sErr == nil && info.IsDir() {
			return d
		}
	}

	// Fall back to CWD walk (finds repo .beads/ stubs — works when no tower configured)
	if d := beads.FindBeadsDir(); d != "" {
		return d
	}

	return ""
}
