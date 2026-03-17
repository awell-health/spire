package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// SpireConfig is the global Spire configuration stored at ~/.config/spire/config.json.
type SpireConfig struct {
	Shell     ShellConfig          `json:"shell"`
	Instances map[string]*Instance `json:"instances"`
}

// ShellConfig tracks whether shell env vars have been injected.
type ShellConfig struct {
	Configured bool   `json:"configured"`
	Profile    string `json:"profile,omitempty"`
}

// Instance represents one init'd repo.
type Instance struct {
	Path       string   `json:"path"`
	Prefix     string   `json:"prefix"`
	Role       string   `json:"role"`       // "hub", "satellite", "standalone"
	Database   string   `json:"database"`   // for hub/standalone = prefix; for satellite = hub's prefix
	Hub        string   `json:"hub,omitempty"`        // only on satellites
	Satellites []string `json:"satellites,omitempty"`  // only on hubs
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

// findInstanceByPath looks up the current working directory in the config.
func findInstanceByPath(cfg *SpireConfig, path string) *Instance {
	for _, inst := range cfg.Instances {
		if inst.Path == path {
			return inst
		}
	}
	return nil
}
