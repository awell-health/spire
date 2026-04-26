package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The four scenarios below pin the precedence contract that store
// dispatch (pkg/store/dispatch.go::isGatewayMode) and CLI helpers
// (config.ActiveTowerConfig) share. Before spi-43q7hp the resolver
// preferred a CWD-mapped instance over cfg.ActiveTower, so a desktop
// that ran `spire tower use <gateway>` from inside a same-prefix local
// repo silently routed mutations to the local direct tower. The fix
// puts ActiveTower above CWD; these tests assert the dispatch verdict
// (gateway vs direct) and the resolved URL, not just the tower name.

// makeGatewayTower writes a minimal gateway-mode tower config to disk.
func makeGatewayTower(t *testing.T, name, prefix, url string) *TowerConfig {
	t.Helper()
	tc := &TowerConfig{
		Name:      name,
		ProjectID: "11111111-2222-4333-8444-555555555555",
		HubPrefix: prefix,
		Database:  "beads_" + prefix,
		CreatedAt: "2026-04-26T12:00:00Z",
		Mode:      TowerModeGateway,
		URL:       url,
		TokenRef:  name,
	}
	if err := SaveTowerConfig(tc); err != nil {
		t.Fatalf("SaveTowerConfig(%q): %v", name, err)
	}
	return tc
}

// makeDirectTower writes a minimal direct-mode tower config to disk.
func makeDirectTower(t *testing.T, name, prefix string) *TowerConfig {
	t.Helper()
	tc := &TowerConfig{
		Name:      name,
		ProjectID: "22222222-3333-4444-8555-666666666666",
		HubPrefix: prefix,
		Database:  "beads_" + prefix,
		CreatedAt: "2026-04-26T12:00:00Z",
		Mode:      TowerModeDirect,
	}
	if err := SaveTowerConfig(tc); err != nil {
		t.Fatalf("SaveTowerConfig(%q): %v", name, err)
	}
	return tc
}

// TestResolveTowerConfig_ActiveGatewayFromNonRepoCwd covers scenario (a):
// the operator selected a gateway tower with `spire tower use`, and the
// shell is sitting outside any registered repo directory. Resolution
// must return the gateway tower; before spi-43q7hp this worked, but the
// test is here to pin the regression-free path so a future precedence
// shuffle can't quietly demote it.
func TestResolveTowerConfig_ActiveGatewayFromNonRepoCwd(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SPIRE_CONFIG_DIR", filepath.Join(tmpDir, "spire"))
	t.Setenv("SPIRE_TOWER", "")

	gw := makeGatewayTower(t, "spi", "spi", "http://127.0.0.1:3030")

	// Operator picked the gateway with `spire tower use spi`.
	if err := Save(&SpireConfig{
		ActiveTower: gw.Name,
		Instances:   map[string]*Instance{},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// CWD is the temp dir (no instance registered for it).
	t.Chdir(tmpDir)

	got, err := ResolveTowerConfig()
	if err != nil {
		t.Fatalf("ResolveTowerConfig: %v", err)
	}
	if got.Name != gw.Name {
		t.Errorf("tower name = %q, want %q", got.Name, gw.Name)
	}
	if !got.IsGateway() {
		t.Errorf("IsGateway() = false, want true (dispatch would fall to direct local Dolt)")
	}
	if got.URL != gw.URL {
		t.Errorf("URL = %q, want %q", got.URL, gw.URL)
	}
}

// TestResolveTowerConfig_CwdDirectTowerLosesToActiveGateway covers
// scenario (b): the operator selected a gateway tower, but the shell
// is inside a registered repo directory whose Instance.Tower points at
// a same-prefix direct local tower. Before spi-43q7hp the CWD-mapped
// direct tower won and mutations silently routed to local Dolt; the
// fix puts cfg.ActiveTower above CWD so the gateway stays selected.
func TestResolveTowerConfig_CwdDirectTowerLosesToActiveGateway(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SPIRE_CONFIG_DIR", filepath.Join(tmpDir, "spire"))
	t.Setenv("SPIRE_TOWER", "")

	gw := makeGatewayTower(t, "spi", "spi", "http://127.0.0.1:3030")
	direct := makeDirectTower(t, "spi-local", "spi")

	repoDir := filepath.Join(tmpDir, "repo")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}

	// CWD-mapped instance binds to the direct local tower; ActiveTower
	// points at the gateway.
	if err := Save(&SpireConfig{
		ActiveTower: gw.Name,
		Instances: map[string]*Instance{
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

	got, err := ResolveTowerConfig()
	if err != nil {
		t.Fatalf("ResolveTowerConfig: %v", err)
	}
	if got.Name != gw.Name {
		t.Errorf("tower name = %q, want gateway %q (CWD direct tower silently won)", got.Name, gw.Name)
	}
	if !got.IsGateway() {
		t.Errorf("IsGateway() = false, want true (mutations would hit direct local Dolt)")
	}
	if got.URL != gw.URL {
		t.Errorf("URL = %q, want %q", got.URL, gw.URL)
	}
}

// TestResolveTowerConfig_PrefixCollisionResolvesToActive covers
// scenario (c): both towers share prefix "spi" — gateway and direct
// local. Whichever is named in cfg.ActiveTower is the one that wins,
// never silently the direct local. We assert both directions so the
// test isn't accidentally satisfied by gateway-always-wins.
func TestResolveTowerConfig_PrefixCollisionResolvesToActive(t *testing.T) {
	type want struct {
		name      string
		isGateway bool
		url       string
	}

	cases := []struct {
		name        string
		activeTower string
		want        want
	}{
		{
			name:        "active gateway resolves to gateway",
			activeTower: "spi",
			want:        want{name: "spi", isGateway: true, url: "http://127.0.0.1:3030"},
		},
		{
			name:        "active direct resolves to direct",
			activeTower: "spi-local",
			want:        want{name: "spi-local", isGateway: false, url: ""},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("HOME", tmpDir)
			t.Setenv("SPIRE_CONFIG_DIR", filepath.Join(tmpDir, "spire"))
			t.Setenv("SPIRE_TOWER", "")

			makeGatewayTower(t, "spi", "spi", "http://127.0.0.1:3030")
			makeDirectTower(t, "spi-local", "spi")

			if err := Save(&SpireConfig{
				ActiveTower: tc.activeTower,
				Instances:   map[string]*Instance{},
			}); err != nil {
				t.Fatalf("Save: %v", err)
			}

			t.Chdir(tmpDir)

			got, err := ResolveTowerConfig()
			if err != nil {
				t.Fatalf("ResolveTowerConfig: %v", err)
			}
			if got.Name != tc.want.name {
				t.Errorf("tower name = %q, want %q", got.Name, tc.want.name)
			}
			if got.IsGateway() != tc.want.isGateway {
				t.Errorf("IsGateway() = %v, want %v", got.IsGateway(), tc.want.isGateway)
			}
			if got.URL != tc.want.url {
				t.Errorf("URL = %q, want %q", got.URL, tc.want.url)
			}
		})
	}
}

// TestResolveTowerConfig_SpireTowerEnvOverridesActive covers scenario
// (d): SPIRE_TOWER=<gateway> wins over both cfg.ActiveTower and any
// CWD-mapped instance. Tests both that the env-named gateway wins over
// a different active tower, and the symmetric case (env-named direct
// tower wins over an active gateway) so the precedence is direction-
// independent.
func TestResolveTowerConfig_SpireTowerEnvOverridesActive(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SPIRE_CONFIG_DIR", filepath.Join(tmpDir, "spire"))

	gw := makeGatewayTower(t, "spi", "spi", "http://127.0.0.1:3030")
	otherGw := makeGatewayTower(t, "spi-other", "spi", "http://127.0.0.1:9999")
	direct := makeDirectTower(t, "spi-local", "spi")

	repoDir := filepath.Join(tmpDir, "repo")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}

	// Active = gw; CWD = direct local; env should still win over both.
	if err := Save(&SpireConfig{
		ActiveTower: gw.Name,
		Instances: map[string]*Instance{
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

	t.Run("env gateway overrides active gateway and CWD", func(t *testing.T) {
		t.Setenv("SPIRE_TOWER", otherGw.Name)
		got, err := ResolveTowerConfig()
		if err != nil {
			t.Fatalf("ResolveTowerConfig: %v", err)
		}
		if got.Name != otherGw.Name {
			t.Errorf("tower name = %q, want %q (SPIRE_TOWER env should win)", got.Name, otherGw.Name)
		}
		if !got.IsGateway() {
			t.Errorf("IsGateway() = false, want true")
		}
		if got.URL != otherGw.URL {
			t.Errorf("URL = %q, want %q", got.URL, otherGw.URL)
		}
	})

	t.Run("env direct overrides active gateway", func(t *testing.T) {
		t.Setenv("SPIRE_TOWER", direct.Name)
		got, err := ResolveTowerConfig()
		if err != nil {
			t.Fatalf("ResolveTowerConfig: %v", err)
		}
		if got.Name != direct.Name {
			t.Errorf("tower name = %q, want %q", got.Name, direct.Name)
		}
		if got.IsGateway() {
			t.Errorf("IsGateway() = true, want false (env-named direct tower)")
		}
	})
}

// TestResolveTowerConfig_StaleActiveTowerFallsThrough confirms a
// dangling cfg.ActiveTower (pointing at a tower whose config file has
// been deleted) does not stop resolution. The bead notes call this out
// explicitly: it must fall through cleanly to CWD/sole-tower rather
// than erroring out.
func TestResolveTowerConfig_StaleActiveTowerFallsThrough(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SPIRE_CONFIG_DIR", filepath.Join(tmpDir, "spire"))
	t.Setenv("SPIRE_TOWER", "")

	gw := makeGatewayTower(t, "spi", "spi", "http://127.0.0.1:3030")

	// ActiveTower points at a tower that doesn't exist on disk; only
	// the gateway is actually loadable, so sole-tower fallback should
	// pick it.
	if err := Save(&SpireConfig{
		ActiveTower: "nonexistent-tower",
		Instances:   map[string]*Instance{},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	t.Chdir(tmpDir)

	got, err := ResolveTowerConfig()
	if err != nil {
		t.Fatalf("ResolveTowerConfig: %v", err)
	}
	if got.Name != gw.Name {
		t.Errorf("tower name = %q, want %q (stale ActiveTower should not block fallback)", got.Name, gw.Name)
	}
}

// TestActiveTowerConfig_DelegatesToResolver pins the contract that
// ActiveTowerConfig now goes through the same resolver as ResolveTowerConfig
// — same precedence, same behavior. spi-43q7hp's regression came from
// these two helpers having different rules; this asserts they answer
// identically for the gateway-vs-CWD-direct case that surfaced the bug.
func TestActiveTowerConfig_DelegatesToResolver(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SPIRE_CONFIG_DIR", filepath.Join(tmpDir, "spire"))
	t.Setenv("SPIRE_TOWER", "")

	gw := makeGatewayTower(t, "spi", "spi", "http://127.0.0.1:3030")
	direct := makeDirectTower(t, "spi-local", "spi")

	repoDir := filepath.Join(tmpDir, "repo")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}

	if err := Save(&SpireConfig{
		ActiveTower: gw.Name,
		Instances: map[string]*Instance{
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

	resolved, err := ResolveTowerConfig()
	if err != nil {
		t.Fatalf("ResolveTowerConfig: %v", err)
	}
	active, err := ActiveTowerConfig()
	if err != nil {
		t.Fatalf("ActiveTowerConfig: %v", err)
	}

	if resolved.Name != active.Name {
		t.Errorf("ActiveTowerConfig name = %q, ResolveTowerConfig name = %q (must agree)", active.Name, resolved.Name)
	}
	if resolved.IsGateway() != active.IsGateway() {
		t.Errorf("IsGateway divergence: ActiveTowerConfig = %v, ResolveTowerConfig = %v", active.IsGateway(), resolved.IsGateway())
	}
	if active.Name != gw.Name {
		t.Errorf("ActiveTowerConfig name = %q, want gateway %q (CWD direct tower silently won)", active.Name, gw.Name)
	}
}

// TestResolveTowerConfig_NoTowerConfigured pins the empty-config error
// path. Before this test, a regression that flipped the no-tower branch
// to silently return nil could let dispatch code default to direct
// mode without surfacing a user-actionable error.
func TestResolveTowerConfig_NoTowerConfigured(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SPIRE_CONFIG_DIR", filepath.Join(tmpDir, "spire"))
	t.Setenv("SPIRE_TOWER", "")
	t.Chdir(tmpDir)

	_, err := ResolveTowerConfig()
	if err == nil {
		t.Fatal("ResolveTowerConfig with no towers should error, got nil")
	}
	if errors.Is(err, ErrAmbiguousTower) {
		t.Errorf("err = %v, want non-ambiguous (no towers configured)", err)
	}
}
