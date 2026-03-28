// Package config manages Spire's global configuration: tower identity, repo
// instances, credentials, OS keychain, and identity detection. All paths are
// rooted in the user's ~/.config/spire/ directory (or SPIRE_CONFIG_DIR).
package config

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

// DoltDataDirFunc returns the dolt data directory. Set from cmd/spire so
// pkg/config can resolve tower database paths without importing dolt lifecycle.
var DoltDataDirFunc func() string

// StoreConfigGetterFunc retrieves a beads config value by key. Set from cmd/spire
// so pkg/config's identity detection can read the store without importing pkg/store.
var StoreConfigGetterFunc func(key string) (string, error)

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

// RealCwd returns the current working directory with symlinks resolved.
// This ensures paths stored in config are canonical regardless of how the
// user navigated to the directory (e.g. /tmp vs /private/tmp on macOS).
func RealCwd() (string, error) {
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

// Dir returns the Spire configuration directory, creating it if needed.
// If SPIRE_CONFIG_DIR is set, that path is used (useful for Docker containers).
// Otherwise falls back to ~/.config/spire/.
func Dir() (string, error) {
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

// Path returns ~/.config/spire/config.json.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Load reads and unmarshals the config. Returns an empty config if the file is missing.
func Load() (*SpireConfig, error) {
	p, err := Path()
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

// Save marshals and writes the config to disk.
func Save(cfg *SpireConfig) error {
	p, err := Path()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(data, '\n'), 0644)
}

// FindInstanceByPath looks up an instance by any of its registered paths (primary or worktree).
// Also matches if path is a subdirectory of a registered path.
func FindInstanceByPath(cfg *SpireConfig, path string) *Instance {
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

// AllPaths returns all registered paths for an instance (primary + worktrees).
func AllPaths(inst *Instance) []string {
	paths := []string{inst.Path}
	paths = append(paths, inst.Paths...)
	return paths
}

// RemoveFromPaths removes a path from inst.Paths (not the primary Path).
func RemoveFromPaths(inst *Instance, path string) {
	updated := inst.Paths[:0]
	for _, p := range inst.Paths {
		if p != path {
			updated = append(updated, p)
		}
	}
	inst.Paths = updated
}

// ResolveTowerConfig determines the active tower using explicit, deterministic
// resolution. Returns an error when the tower cannot be determined unambiguously.
// Loads config from disk. For callers that already have a SpireConfig, use
// ResolveTowerConfigWith.
//
// Resolution order:
//  1. SPIRE_TOWER env var (explicit tower name from --tower flag or env)
//  2. CWD → registered instance → instance's tower
//  3. Active tower (cfg.ActiveTower)
//  4. Sole tower on disk (exactly one tower config file)
//  5. Error: no tower found or ambiguous (multiple towers, none active)
func ResolveTowerConfig() (*TowerConfig, error) {
	// 1. SPIRE_TOWER env override (from --tower flag or explicit env).
	if towerName := os.Getenv("SPIRE_TOWER"); towerName != "" {
		tower, err := LoadTowerConfig(towerName)
		if err != nil {
			return nil, fmt.Errorf("SPIRE_TOWER=%q: %w", towerName, err)
		}
		return tower, nil
	}

	cfg, err := Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	return ResolveTowerConfigWith(cfg)
}

// ResolveTowerConfigWith is the inner resolution logic that uses a pre-loaded
// SpireConfig. This allows callers (like resolveDatabase) that already have a
// config to pass it in, so test-injected values (e.g. ActiveTower) are respected.
//
// Resolution order (after SPIRE_TOWER, handled by ResolveTowerConfig):
//  2. CWD → registered instance → instance's tower
//  3. Active tower (cfg.ActiveTower)
//  4. Sole tower on disk (exactly one tower config file)
//  5. Error: no tower found or ambiguous (multiple towers, none active)
func ResolveTowerConfigWith(cfg *SpireConfig) (*TowerConfig, error) {
	// 2. CWD → registered instance → instance's tower.
	if cwd, cwdErr := RealCwd(); cwdErr == nil {
		if inst := FindInstanceByPath(cfg, cwd); inst != nil && inst.Tower != "" {
			tower, tErr := LoadTowerConfig(inst.Tower)
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
		if tower, tErr := LoadTowerConfig(cfg.ActiveTower); tErr == nil {
			return tower, nil
		}
	}

	// 4. Sole tower on disk — refuse to guess when ambiguous.
	towers, tErr := ListTowerConfigs()
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

// doltDataDir returns the dolt data directory via the injected function,
// falling back to empty string if no function is set.
func doltDataDir() string {
	if DoltDataDirFunc != nil {
		return DoltDataDirFunc()
	}
	return ""
}

// ResolveBeadsDir returns a .beads/ directory path that can be used to open a store.
//
// Resolution order:
//  1. BEADS_DIR env var (explicit override)
//  2. ResolveTowerConfig() → tower database directory (deterministic, errors on ambiguity)
//  3. beads.FindBeadsDir() (walk up from CWD — may find repo stubs)
//
// Tower paths are checked before CWD because spire repo add creates .beads/ stubs
// in repo directories that lack the dolt/ subdirectory. The beads library needs
// the full database context to work in server mode.
func ResolveBeadsDir() string {
	if d := os.Getenv("BEADS_DIR"); d != "" {
		return d
	}

	// Unified tower resolution — no ambient "any tower" fallback.
	if tower, err := ResolveTowerConfig(); err == nil && tower.Database != "" {
		dd := doltDataDir()
		if dd != "" {
			d := filepath.Join(dd, tower.Database, ".beads")
			if info, sErr := os.Stat(d); sErr == nil && info.IsDir() {
				return d
			}
		}
	}

	// Fall back to CWD walk (finds repo .beads/ stubs — works when no tower configured)
	if d := beads.FindBeadsDir(); d != "" {
		return d
	}

	return ""
}
