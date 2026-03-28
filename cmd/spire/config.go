// config.go provides backward-compatible wrappers delegating to pkg/config.
// cmd/spire callers continue to use unexported names; the real logic lives in
// the config package.
package main

import (
	"github.com/awell-health/spire/pkg/config"
)

// --- Type aliases so existing cmd/spire code compiles unchanged ---

type SpireConfig = config.SpireConfig
type ShellConfig = config.ShellConfig
type Instance = config.Instance

// ErrAmbiguousTower is returned when multiple towers exist and none is active.
// This is the same sentinel from pkg/config — errors.Is works because Go
// interface equality compares the underlying pointer.
var ErrAmbiguousTower = config.ErrAmbiguousTower

// --- Wrappers delegating to pkg/config ---

func realCwd() (string, error) {
	return config.RealCwd()
}

func configDir() (string, error) {
	return config.Dir()
}

func configPath() (string, error) {
	return config.Path()
}

func loadConfig() (*SpireConfig, error) {
	return config.Load()
}

func saveConfig(cfg *SpireConfig) error {
	return config.Save(cfg)
}

func findInstanceByPath(cfg *SpireConfig, path string) *Instance {
	return config.FindInstanceByPath(cfg, path)
}

// allPaths, removeFromPaths, resolveTowerConfig removed — no callers in cmd/spire.

func resolveTowerConfigWith(cfg *SpireConfig) (*config.TowerConfig, error) {
	return config.ResolveTowerConfigWith(cfg)
}

func resolveBeadsDir() string {
	return config.ResolveBeadsDir()
}
