package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/awell-health/spire/pkg/config"
)

// gatewayCanonicalErrFor builds the canonical error string the guard
// returns. Tests assert against this verbatim because the message is the
// public contract checked by both unit and integration layers.
func gatewayCanonicalErrFor(name, url string) string {
	return "tower " + name + " is gateway-mode; mutations route through " + url + "; direct Dolt sync is disabled"
}

// writeGatewayTower writes a minimal gateway-mode tower config. Mirrors
// pkg/config/resolve_tower_test.go::makeGatewayTower so the same fixtures
// drive both unit and integration coverage.
func writeGatewayTower(t *testing.T, name, prefix, url string) *config.TowerConfig {
	t.Helper()
	tc := &config.TowerConfig{
		Name:      name,
		ProjectID: "11111111-2222-4333-8444-555555555555",
		HubPrefix: prefix,
		Database:  "beads_" + prefix,
		CreatedAt: "2026-04-26T12:00:00Z",
		Mode:      config.TowerModeGateway,
		URL:       url,
		TokenRef:  name,
	}
	if err := config.SaveTowerConfig(tc); err != nil {
		t.Fatalf("SaveTowerConfig(%q): %v", name, err)
	}
	return tc
}

// writeDirectTower writes a minimal direct-mode tower config.
func writeDirectTower(t *testing.T, name, prefix string) *config.TowerConfig {
	t.Helper()
	tc := &config.TowerConfig{
		Name:      name,
		ProjectID: "22222222-3333-4444-8555-666666666666",
		HubPrefix: prefix,
		Database:  "beads_" + prefix,
		CreatedAt: "2026-04-26T12:00:00Z",
		Mode:      config.TowerModeDirect,
	}
	if err := config.SaveTowerConfig(tc); err != nil {
		t.Fatalf("SaveTowerConfig(%q): %v", name, err)
	}
	return tc
}

// setupGatewayActive primes a temp config dir with a single gateway-mode
// tower selected as ActiveTower. Returns the tower for assertion. The
// caller's CWD is left at tmpDir (no registered repo) so the resolver
// hits the ActiveTower branch, not CWD lookup.
func setupGatewayActive(t *testing.T, towerName, prefix, url string) *config.TowerConfig {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SPIRE_CONFIG_DIR", filepath.Join(tmpDir, "spire"))
	t.Setenv("SPIRE_TOWER", "")

	gw := writeGatewayTower(t, towerName, prefix, url)
	if err := config.Save(&config.SpireConfig{
		ActiveTower: gw.Name,
		Instances:   map[string]*config.Instance{},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	t.Chdir(tmpDir)
	return gw
}

// TestRunPush_GatewayActiveRejects covers the headline acceptance case for
// `spire push`. With a gateway-mode tower selected (ActiveTower) and no
// SPIRE_TOWER override, runPush must return *config.GatewayModeError
// before requireDolt, readBeadsDBName, bd dolt remote, dolt.SetCLIRemote,
// bd vc commit, or CLIPush can run. The guard returns first, so even
// without a live dolt server this test exercises the rejection path.
func TestRunPush_GatewayActiveRejects(t *testing.T) {
	gw := setupGatewayActive(t, "spi", "spi", "http://127.0.0.1:3030")

	err := runPush("")
	if err == nil {
		t.Fatal("runPush: got nil, want *config.GatewayModeError")
	}
	var gwErr *config.GatewayModeError
	if !errors.As(err, &gwErr) {
		t.Fatalf("runPush: err = %v (type %T), want *config.GatewayModeError", err, err)
	}
	if got := err.Error(); got != gatewayCanonicalErrFor(gw.Name, gw.URL) {
		t.Errorf("runPush error = %q, want %q", got, gatewayCanonicalErrFor(gw.Name, gw.URL))
	}
}

// TestRunPull_GatewayActiveRejects mirrors the push test for `spire pull`.
// Same observation: the guard fires before resolveDataDir, remote
// mutation, CLIPull, ownership repair, or conflict repair.
func TestRunPull_GatewayActiveRejects(t *testing.T) {
	gw := setupGatewayActive(t, "spi", "spi", "http://127.0.0.1:3030")

	err := runPull("", false)
	if err == nil {
		t.Fatal("runPull: got nil, want *config.GatewayModeError")
	}
	var gwErr *config.GatewayModeError
	if !errors.As(err, &gwErr) {
		t.Fatalf("runPull: err = %v (type %T), want *config.GatewayModeError", err, err)
	}
	if got := err.Error(); got != gatewayCanonicalErrFor(gw.Name, gw.URL) {
		t.Errorf("runPull error = %q, want %q", got, gatewayCanonicalErrFor(gw.Name, gw.URL))
	}
}

// TestRunSync_GatewayActiveRejects mirrors push/pull for `spire sync`.
// The runSync entry point is the merge variant; the guard fires before
// resolveDataDir, CLIFetchMerge, ownership repair, or conflict repair.
func TestRunSync_GatewayActiveRejects(t *testing.T) {
	gw := setupGatewayActive(t, "spi", "spi", "http://127.0.0.1:3030")

	err := runSync()
	if err == nil {
		t.Fatal("runSync: got nil, want *config.GatewayModeError")
	}
	var gwErr *config.GatewayModeError
	if !errors.As(err, &gwErr) {
		t.Fatalf("runSync: err = %v (type %T), want *config.GatewayModeError", err, err)
	}
	if got := err.Error(); got != gatewayCanonicalErrFor(gw.Name, gw.URL) {
		t.Errorf("runSync error = %q, want %q", got, gatewayCanonicalErrFor(gw.Name, gw.URL))
	}
}

// TestRunPushPullSync_SpireTowerEnvOverride covers the "uses canonical
// resolver" acceptance: active tower is direct-mode, but SPIRE_TOWER
// names a different gateway tower. The guard must trip on the
// env-selected tower, not the active one. This is the bug-catching test
// noted in the spec — `activeTowerConfig()` had to be made resolver-
// equivalent (or replaced) for this to work.
func TestRunPushPullSync_SpireTowerEnvOverride(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SPIRE_CONFIG_DIR", filepath.Join(tmpDir, "spire"))

	gw := writeGatewayTower(t, "spi-gw", "spi", "http://127.0.0.1:3030")
	direct := writeDirectTower(t, "spi-local", "spi")

	if err := config.Save(&config.SpireConfig{
		ActiveTower: direct.Name,
		Instances:   map[string]*config.Instance{},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	t.Chdir(tmpDir)
	t.Setenv("SPIRE_TOWER", gw.Name)

	cases := []struct {
		name string
		fn   func() error
	}{
		{"runPush", func() error { return runPush("") }},
		{"runPull", func() error { return runPull("", false) }},
		{"runSync", func() error { return runSync() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.fn()
			if err == nil {
				t.Fatalf("%s: got nil, want *config.GatewayModeError (env-named gateway)", tc.name)
			}
			var gwErr *config.GatewayModeError
			if !errors.As(err, &gwErr) {
				t.Fatalf("%s: err = %v (type %T), want *config.GatewayModeError", tc.name, err, err)
			}
			if gwErr.TowerName != gw.Name {
				t.Errorf("%s: TowerName = %q, want %q (env override should pick gateway)",
					tc.name, gwErr.TowerName, gw.Name)
			}
		})
	}
}

// TestRunPushPullSync_SamePrefixCwdCollision plants a registered repo at
// CWD whose Instance.Tower points at a same-prefix direct tower, while
// ActiveTower names the gateway. The guard must still fire because the
// canonical resolver puts ActiveTower above CWD (spi-43q7hp). If the
// guard ever bypassed the resolver and used a CWD-first lookup, the
// direct local tower would silently win and this test would catch it.
func TestRunPushPullSync_SamePrefixCwdCollision(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SPIRE_CONFIG_DIR", filepath.Join(tmpDir, "spire"))
	t.Setenv("SPIRE_TOWER", "")

	gw := writeGatewayTower(t, "spi", "spi", "http://127.0.0.1:3030")
	direct := writeDirectTower(t, "spi-local", "spi")

	repoDir := filepath.Join(tmpDir, "repo")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := config.Save(&config.SpireConfig{
		ActiveTower: gw.Name,
		Instances: map[string]*config.Instance{
			"repo": {
				Path:     repoDir,
				Prefix:   "spi",
				Database: direct.Database,
				Tower:    direct.Name,
			},
		},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	t.Chdir(repoDir)

	cases := []struct {
		name string
		fn   func() error
	}{
		{"runPush", func() error { return runPush("") }},
		{"runPull", func() error { return runPull("", false) }},
		{"runSync", func() error { return runSync() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.fn()
			if err == nil {
				t.Fatalf("%s: got nil, want *config.GatewayModeError (CWD must not mask active gateway)", tc.name)
			}
			var gwErr *config.GatewayModeError
			if !errors.As(err, &gwErr) {
				t.Fatalf("%s: err = %v (type %T), want *config.GatewayModeError", tc.name, err, err)
			}
			if gwErr.TowerName != gw.Name {
				t.Errorf("%s: TowerName = %q, want %q (CWD direct tower silently won)",
					tc.name, gwErr.TowerName, gw.Name)
			}
		})
	}
}

// TestRunPushPullSync_DirectModePassesGuard confirms direct-mode tower
// selection still proceeds past the guard. We don't drive the full
// command (no live dolt), so this test doesn't assert success — only
// that the failure (if any) is NOT *GatewayModeError. A regression that
// flipped the guard to "always reject" would be caught here.
func TestRunPushPullSync_DirectModePassesGuard(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SPIRE_CONFIG_DIR", filepath.Join(tmpDir, "spire"))
	t.Setenv("SPIRE_TOWER", "")

	direct := writeDirectTower(t, "spi-local", "spi")
	if err := config.Save(&config.SpireConfig{
		ActiveTower: direct.Name,
		Instances:   map[string]*config.Instance{},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	t.Chdir(tmpDir)

	cases := []struct {
		name string
		fn   func() error
	}{
		{"runPush", func() error { return runPush("") }},
		{"runPull", func() error { return runPull("", false) }},
		{"runSync", func() error { return runSync() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.fn()
			var gwErr *config.GatewayModeError
			if errors.As(err, &gwErr) {
				t.Errorf("%s direct-mode: returned *GatewayModeError (%v); guard should have passed",
					tc.name, err)
			}
		})
	}
}
