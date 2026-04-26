package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIsGatewayMode covers the cheap predicate used by daemon/steward
// iteration paths. It must answer purely on the loaded TowerConfig — no
// resolver call, no env var read.
func TestIsGatewayMode(t *testing.T) {
	cases := []struct {
		name string
		tc   *TowerConfig
		want bool
	}{
		{name: "nil tower is not gateway", tc: nil, want: false},
		{name: "explicit gateway is gateway", tc: &TowerConfig{Mode: TowerModeGateway}, want: true},
		{name: "explicit direct is not gateway", tc: &TowerConfig{Mode: TowerModeDirect}, want: false},
		{name: "empty mode defaults to direct", tc: &TowerConfig{Mode: ""}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsGatewayMode(tc.tc); got != tc.want {
				t.Errorf("IsGatewayMode = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestGatewayModeError_Message pins the canonical error string. The wording
// is asserted by integration tests in cmd/spire as well, so any drift here
// will be caught both at the unit and integration layers.
func TestGatewayModeError_Message(t *testing.T) {
	err := &GatewayModeError{TowerName: "spi", GatewayURL: "http://127.0.0.1:3030"}
	want := "tower spi is gateway-mode; mutations route through http://127.0.0.1:3030; direct Dolt sync is disabled"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// TestGatewayModeError_EmptyURL confirms that a malformed gateway tower with
// an empty URL still trips the guard. Mode is the source of truth; URL is
// decoration. The message just shows an empty URL — better than silently
// letting a misconfigured gateway tower fall through to direct Dolt sync.
func TestGatewayModeError_EmptyURL(t *testing.T) {
	err := &GatewayModeError{TowerName: "spi", GatewayURL: ""}
	want := "tower spi is gateway-mode; mutations route through ; direct Dolt sync is disabled"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// TestRejectIfGateway_GatewayActive covers the headline acceptance path:
// active tower is gateway-mode, resolver picks it, guard returns
// *GatewayModeError with the canonical message.
func TestRejectIfGateway_GatewayActive(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SPIRE_CONFIG_DIR", filepath.Join(tmpDir, "spire"))
	t.Setenv("SPIRE_TOWER", "")

	gw := makeGatewayTower(t, "spi", "spi", "http://127.0.0.1:3030")
	if err := Save(&SpireConfig{
		ActiveTower: gw.Name,
		Instances:   map[string]*Instance{},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	t.Chdir(tmpDir)

	err := RejectIfGateway()
	if err == nil {
		t.Fatal("RejectIfGateway: got nil, want *GatewayModeError")
	}
	var gwErr *GatewayModeError
	if !errors.As(err, &gwErr) {
		t.Fatalf("RejectIfGateway: err = %v (type %T), want *GatewayModeError", err, err)
	}
	if gwErr.TowerName != "spi" {
		t.Errorf("TowerName = %q, want %q", gwErr.TowerName, "spi")
	}
	if gwErr.GatewayURL != "http://127.0.0.1:3030" {
		t.Errorf("GatewayURL = %q, want %q", gwErr.GatewayURL, "http://127.0.0.1:3030")
	}
	want := "tower spi is gateway-mode; mutations route through http://127.0.0.1:3030; direct Dolt sync is disabled"
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
}

// TestRejectIfGateway_DirectActive covers the passthrough: direct-mode
// active tower returns nil, so command handlers proceed normally. Without
// this case the guard could regress to "always error" and silently break
// direct-mode flows.
func TestRejectIfGateway_DirectActive(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SPIRE_CONFIG_DIR", filepath.Join(tmpDir, "spire"))
	t.Setenv("SPIRE_TOWER", "")

	direct := makeDirectTower(t, "spi-local", "spi")
	if err := Save(&SpireConfig{
		ActiveTower: direct.Name,
		Instances:   map[string]*Instance{},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	t.Chdir(tmpDir)

	if err := RejectIfGateway(); err != nil {
		t.Errorf("RejectIfGateway = %v, want nil for direct-mode tower", err)
	}
}

// TestRejectIfGateway_NoTowerPropagates confirms the "no tower" resolver
// error survives the guard — command handlers (which already produced a
// "no tower configured" message before this guard existed) keep their
// existing UX.
func TestRejectIfGateway_NoTowerPropagates(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SPIRE_CONFIG_DIR", filepath.Join(tmpDir, "spire"))
	t.Setenv("SPIRE_TOWER", "")
	t.Chdir(tmpDir)

	err := RejectIfGateway()
	if err == nil {
		t.Fatal("RejectIfGateway with no towers: got nil, want resolver error")
	}
	var gwErr *GatewayModeError
	if errors.As(err, &gwErr) {
		t.Errorf("RejectIfGateway no-tower path: got *GatewayModeError, want resolver error")
	}
}

// TestRejectIfGateway_SpireTowerEnvOverride exercises the resolver
// precedence: SPIRE_TOWER pointing at a gateway tower trips the guard
// even when cfg.ActiveTower names a direct tower. This is the
// regression-catching test the spec calls out — activeTowerConfig() (the
// CLI helper) now goes through the same resolver, so this confirms env
// override flows through the guard correctly.
func TestRejectIfGateway_SpireTowerEnvOverride(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SPIRE_CONFIG_DIR", filepath.Join(tmpDir, "spire"))

	gw := makeGatewayTower(t, "spi-gw", "spi", "http://127.0.0.1:3030")
	direct := makeDirectTower(t, "spi-local", "spi")

	if err := Save(&SpireConfig{
		ActiveTower: direct.Name,
		Instances:   map[string]*Instance{},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	t.Chdir(tmpDir)

	t.Setenv("SPIRE_TOWER", gw.Name)

	err := RejectIfGateway()
	if err == nil {
		t.Fatal("RejectIfGateway with SPIRE_TOWER=gateway: got nil, want *GatewayModeError")
	}
	var gwErr *GatewayModeError
	if !errors.As(err, &gwErr) {
		t.Fatalf("RejectIfGateway SPIRE_TOWER override: err = %v (type %T), want *GatewayModeError", err, err)
	}
	if gwErr.TowerName != gw.Name {
		t.Errorf("TowerName = %q, want %q (env-named gateway should win over active direct)", gwErr.TowerName, gw.Name)
	}
}

// TestEnsureNotGateway_NilCfg pins the explicit-cfg variant's nil contract:
// callers that haven't loaded a tower yet (library helpers running before
// config is wired) get nil so they don't accidentally fail on a missing
// resolver. This matches the IsGatewayMode(nil) contract.
func TestEnsureNotGateway_NilCfg(t *testing.T) {
	if err := EnsureNotGateway(nil, "any.op"); err != nil {
		t.Errorf("EnsureNotGateway(nil) = %v, want nil", err)
	}
}

// TestEnsureNotGateway_DirectMode confirms the passthrough for direct-mode
// towers — the helper is a no-op so existing direct-mode flows can't
// regress.
func TestEnsureNotGateway_DirectMode(t *testing.T) {
	cases := []struct {
		name string
		mode string
	}{
		{name: "explicit direct", mode: TowerModeDirect},
		{name: "empty mode (legacy default)", mode: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &TowerConfig{Name: "spi", Mode: tc.mode}
			if err := EnsureNotGateway(cfg, "dolt.CLIPush"); err != nil {
				t.Errorf("EnsureNotGateway(%s) = %v, want nil", tc.mode, err)
			}
		})
	}
}

// TestEnsureNotGateway_GatewayWrapsSentinel covers the sentinel contract:
// gateway-mode rejection must wrap ErrGatewayDirectMutation so callers and
// tests can errors.Is against the sentinel uniformly across the audited
// helpers (CLI, dolt CLI helpers, bd wrappers, integration writers).
func TestEnsureNotGateway_GatewayWrapsSentinel(t *testing.T) {
	cfg := &TowerConfig{Name: "spi", Mode: TowerModeGateway, URL: "http://127.0.0.1:3030"}

	err := EnsureNotGateway(cfg, "dolt.CLIPush")
	if err == nil {
		t.Fatal("EnsureNotGateway(gateway) = nil, want wrapped ErrGatewayDirectMutation")
	}
	if !errors.Is(err, ErrGatewayDirectMutation) {
		t.Fatalf("errors.Is(err, ErrGatewayDirectMutation) = false; got %v", err)
	}
	if !strings.Contains(err.Error(), "dolt.CLIPush") {
		t.Errorf("Error() = %q, want op name embedded", err.Error())
	}
}

// TestGatewayModeError_IsSentinel pins the cross-shape sentinel match:
// CLI top-level guards return *GatewayModeError; library helpers return
// the wrapped sentinel form. Both must satisfy
// errors.Is(err, ErrGatewayDirectMutation) so a single test idiom covers
// every guarded path.
func TestGatewayModeError_IsSentinel(t *testing.T) {
	err := &GatewayModeError{TowerName: "spi", GatewayURL: "http://127.0.0.1:3030"}
	if !errors.Is(err, ErrGatewayDirectMutation) {
		t.Errorf("errors.Is(*GatewayModeError, ErrGatewayDirectMutation) = false, want true")
	}
}

// TestEnsureNotGatewayResolved_GatewayActive exercises the resolver-driven
// variant: a gateway tower selected via cfg.ActiveTower must trip the
// helper exactly the same way EnsureNotGateway(cfg, op) would. Library
// helpers reach this entry point when they have no TowerConfig in scope.
func TestEnsureNotGatewayResolved_GatewayActive(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SPIRE_CONFIG_DIR", filepath.Join(tmpDir, "spire"))
	t.Setenv("SPIRE_TOWER", "")

	gw := makeGatewayTower(t, "spi", "spi", "http://127.0.0.1:3030")
	if err := Save(&SpireConfig{
		ActiveTower: gw.Name,
		Instances:   map[string]*Instance{},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	t.Chdir(tmpDir)

	err := EnsureNotGatewayResolved("dolt.CLIPush")
	if err == nil {
		t.Fatal("EnsureNotGatewayResolved: got nil, want wrapped ErrGatewayDirectMutation")
	}
	if !errors.Is(err, ErrGatewayDirectMutation) {
		t.Errorf("errors.Is(err, ErrGatewayDirectMutation) = false; got %v", err)
	}
}

// TestEnsureNotGatewayResolved_DirectActive confirms direct-mode passthrough.
func TestEnsureNotGatewayResolved_DirectActive(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SPIRE_CONFIG_DIR", filepath.Join(tmpDir, "spire"))
	t.Setenv("SPIRE_TOWER", "")

	direct := makeDirectTower(t, "spi-local", "spi")
	if err := Save(&SpireConfig{
		ActiveTower: direct.Name,
		Instances:   map[string]*Instance{},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	t.Chdir(tmpDir)

	if err := EnsureNotGatewayResolved("dolt.CLIPush"); err != nil {
		t.Errorf("EnsureNotGatewayResolved direct mode = %v, want nil", err)
	}
}

// TestEnsureNotGatewayResolved_NoTowerTreatsAsDirect: library helpers reach
// this entry point in early-boot or test contexts where the resolver
// produces "no tower configured". Returning nil keeps such callers
// unblocked — the top-level CLI guard (RejectIfGateway) is the right
// place to surface resolver errors to the operator.
func TestEnsureNotGatewayResolved_NoTowerTreatsAsDirect(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SPIRE_CONFIG_DIR", filepath.Join(tmpDir, "spire"))
	t.Setenv("SPIRE_TOWER", "")
	t.Chdir(tmpDir)

	if err := EnsureNotGatewayResolved("dolt.CLIPush"); err != nil {
		t.Errorf("EnsureNotGatewayResolved no-tower = %v, want nil (treated as direct-mode)", err)
	}
}

// TestRejectIfGateway_CwdDirectLosesToActiveGateway is the prefix-collision
// scenario the parent epic was designed around: CWD-mapped instance points
// at a same-prefix direct local tower, but `spire tower use <gateway>` is
// active. The guard fires for the gateway because the canonical resolver
// puts ActiveTower above CWD (spi-43q7hp). Pinning this here keeps the
// guard wired to the canonical resolver — a future shuffle that bypassed
// the resolver would silently let direct Dolt sync proceed.
func TestRejectIfGateway_CwdDirectLosesToActiveGateway(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SPIRE_CONFIG_DIR", filepath.Join(tmpDir, "spire"))
	t.Setenv("SPIRE_TOWER", "")

	gw := makeGatewayTower(t, "spi", "spi", "http://127.0.0.1:3030")
	direct := makeDirectTower(t, "spi-local", "spi")

	repoDir := filepath.Join(tmpDir, "repo")
	if err := os.MkdirAll(repoDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
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

	err := RejectIfGateway()
	if err == nil {
		t.Fatal("RejectIfGateway: got nil, want *GatewayModeError (CWD direct must not mask active gateway)")
	}
	var gwErr *GatewayModeError
	if !errors.As(err, &gwErr) {
		t.Fatalf("err = %v (type %T), want *GatewayModeError", err, err)
	}
	if gwErr.TowerName != gw.Name {
		t.Errorf("TowerName = %q, want %q (CWD direct silently won)", gwErr.TowerName, gw.Name)
	}
}
