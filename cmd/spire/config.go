package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

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

// Instance represents one init'd repo.
type Instance struct {
	Path           string   `json:"path"`
	Paths          []string `json:"paths,omitempty"`           // additional directories (e.g. git worktrees)
	Prefix         string   `json:"prefix"`
	Role           string   `json:"role"`                      // "hub", "satellite", "standalone"
	Database       string   `json:"database"`                  // for hub/standalone = prefix; for satellite = hub's prefix
	Hub            string   `json:"hub,omitempty"`             // only on satellites
	Satellites     []string `json:"satellites,omitempty"`      // only on hubs
	DoltPort       int      `json:"dolt_port,omitempty"`       // default: 3307
	DaemonInterval string   `json:"daemon_interval,omitempty"` // default: "2m"
	DolthubRemote  string   `json:"dolthub_remote,omitempty"`
	Identity       string   `json:"identity,omitempty"`        // default: prefix
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

// configDir returns ~/.config/spire/, creating it if needed.
func configDir() (string, error) {
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
func findInstanceByPath(cfg *SpireConfig, path string) *Instance {
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
