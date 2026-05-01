package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/config"
)

// stubResummonGateway swaps the package-level seams cmdResummon uses
// for gateway-mode dispatch. Default is no tower so cmdResummon takes
// the local path (which fails on missing store — tests that need the
// gateway path swap activeTowerConfigFunc explicitly).
func stubResummonGateway(t *testing.T) func() {
	t.Helper()
	origTower := activeTowerConfigFunc
	origResummon := resummonViaGatewayFunc
	activeTowerConfigFunc = func() (*TowerConfig, error) { return nil, nil }
	resummonViaGatewayFunc = func(context.Context, string) error {
		t.Fatalf("resummonViaGatewayFunc must not be called when no tower is configured")
		return nil
	}
	return func() {
		activeTowerConfigFunc = origTower
		resummonViaGatewayFunc = origResummon
	}
}

// TestCmdResummon_GatewayMode_RoutesToGatewaySeam verifies that
// `spire resummon <bead>` against a gateway-mode tower dispatches to
// resummonViaGatewayFunc with the bead ID, never reaching the
// in-process resummon flow. Pins the gateway-mode CLI dispatch path
// established by spi-wrjiw6.
func TestCmdResummon_GatewayMode_RoutesToGatewaySeam(t *testing.T) {
	restore := stubResummonGateway(t)
	defer restore()

	activeTowerConfigFunc = func() (*TowerConfig, error) {
		return &TowerConfig{Name: "cluster", Mode: config.TowerModeGateway, URL: "https://example.com"}, nil
	}
	var gotID string
	resummonViaGatewayFunc = func(_ context.Context, id string) error {
		gotID = id
		return nil
	}

	if err := cmdResummon([]string{"spi-abc"}); err != nil {
		t.Fatalf("cmdResummon: %v", err)
	}
	if gotID != "spi-abc" {
		t.Errorf("resummonViaGatewayFunc id = %q, want spi-abc", gotID)
	}
}

// TestCmdResummon_GatewayMode_ErrorPropagates verifies that
// gateway-side errors surface verbatim — the user must see why the
// resummon failed (e.g. 404, 409 "not hooked"), not a wrapped message.
func TestCmdResummon_GatewayMode_ErrorPropagates(t *testing.T) {
	restore := stubResummonGateway(t)
	defer restore()

	activeTowerConfigFunc = func() (*TowerConfig, error) {
		return &TowerConfig{Name: "cluster", Mode: config.TowerModeGateway, URL: "https://example.com"}, nil
	}
	wantErr := errors.New("gatewayclient: HTTP 409: bead is not hooked")
	resummonViaGatewayFunc = func(context.Context, string) error {
		return wantErr
	}

	err := cmdResummon([]string{"spi-abc"})
	if err == nil {
		t.Fatal("cmdResummon: nil err, want HTTP 409 from gateway")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want gatewayclient error", err)
	}
}

// stubDismissGateway swaps the seams cmdDismiss uses for gateway-mode
// dispatch. Default activeTowerConfigFunc returns no tower so callers
// without tower config don't hit the gateway path.
func stubDismissGateway(t *testing.T) func() {
	t.Helper()
	origTower := activeTowerConfigFunc
	origK8s := isK8sAvailableFunc
	origDismiss := dismissBeadViaGatewayFunc
	activeTowerConfigFunc = func() (*TowerConfig, error) { return nil, nil }
	isK8sAvailableFunc = func() bool { return false }
	dismissBeadViaGatewayFunc = func(context.Context, string) error {
		t.Fatalf("dismissBeadViaGatewayFunc must not be called in non-gateway tests")
		return nil
	}
	return func() {
		activeTowerConfigFunc = origTower
		isK8sAvailableFunc = origK8s
		dismissBeadViaGatewayFunc = origDismiss
	}
}

// TestCmdDismiss_GatewayMode_TargetsRoutesPerID verifies that
// `spire dismiss --targets a,b` against a gateway-mode tower dispatches
// to dismissBeadViaGatewayFunc once per target ID, never reaching the
// wizard-pool path. Pins the bead-level gateway dispatch added in
// spi-wrjiw6.
func TestCmdDismiss_GatewayMode_TargetsRoutesPerID(t *testing.T) {
	restore := stubDismissGateway(t)
	defer restore()

	activeTowerConfigFunc = func() (*TowerConfig, error) {
		return &TowerConfig{Name: "cluster", Mode: config.TowerModeGateway, URL: "https://example.com"}, nil
	}
	var gotIDs []string
	dismissBeadViaGatewayFunc = func(_ context.Context, id string) error {
		gotIDs = append(gotIDs, id)
		return nil
	}

	if err := cmdDismiss([]string{"--targets", "spi-abc,spi-def"}); err != nil {
		t.Fatalf("cmdDismiss: %v", err)
	}
	if len(gotIDs) != 2 || gotIDs[0] != "spi-abc" || gotIDs[1] != "spi-def" {
		t.Errorf("dismissBeadViaGatewayFunc IDs = %v, want [spi-abc spi-def]", gotIDs)
	}
}

// TestCmdDismiss_GatewayMode_NoTargetsBypassesGateway pins the
// design choice that bare-count dismiss (no --targets) does NOT route
// through the bead-level gateway endpoint — counts have no mapping to
// a per-bead URL, so they stay on the wizard-pool path.
func TestCmdDismiss_GatewayMode_NoTargetsBypassesGateway(t *testing.T) {
	// CRITICAL: this test reaches dismissLocal — TowerConfig.DeploymentMode
	// is unset on the tower below, and EffectiveDeploymentMode defaults
	// empty to LocalNative (pkg/config/tower.go), so bare-count dispatch
	// hits the local-native case, calls loadWizardRegistry, and signals
	// every PID it finds. Without SPIRE_CONFIG_DIR isolation the test
	// reads the operator's real ~/.config/spire/wizards.json and SIGINT-
	// kills every live wizard on the box (apprentices, sages, anything
	// registered). This was spi-od41sr — every `go test ./cmd/spire/`
	// run from an apprentice's worktree was murdering its own wizard.
	// Pin the registry to a temp dir so the loadWizardRegistry call sees
	// an empty registry and dismissLocal has nothing to signal.
	t.Setenv("SPIRE_CONFIG_DIR", t.TempDir())

	restore := stubDismissGateway(t)
	defer restore()

	activeTowerConfigFunc = func() (*TowerConfig, error) {
		return &TowerConfig{Name: "cluster", Mode: config.TowerModeGateway, URL: "https://example.com"}, nil
	}
	dismissBeadViaGatewayFunc = func(context.Context, string) error {
		t.Fatalf("dismissBeadViaGatewayFunc must NOT be called for bare-count dismiss")
		return nil
	}

	// Bare count without --targets falls through to the deployment-mode
	// switch. With no DeploymentMode set on the tower the switch lands
	// on the LocalNative branch (EffectiveDeploymentMode default), and
	// dismissLocal walks the (now-empty, see Setenv above) registry —
	// the assertion remains that the gateway seam is not invoked.
	_ = cmdDismiss([]string{"1"})
}

// TestCmdDismiss_GatewayMode_LastErrorPropagates verifies that when
// multiple targets are supplied and one fails, cmdDismiss surfaces the
// last error so the user knows at least one dispatch failed (per-target
// errors are also stderr-logged via fmt.Fprintf so callers see all of
// them).
func TestCmdDismiss_GatewayMode_LastErrorPropagates(t *testing.T) {
	restore := stubDismissGateway(t)
	defer restore()

	activeTowerConfigFunc = func() (*TowerConfig, error) {
		return &TowerConfig{Name: "cluster", Mode: config.TowerModeGateway, URL: "https://example.com"}, nil
	}
	wantErr := errors.New("gatewayclient: HTTP 404: bead spi-def not found")
	dismissBeadViaGatewayFunc = func(_ context.Context, id string) error {
		if id == "spi-def" {
			return wantErr
		}
		return nil
	}

	err := cmdDismiss([]string{"--targets", "spi-abc,spi-def"})
	if err == nil {
		t.Fatal("cmdDismiss: nil err, want propagated 404")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want gatewayclient 404", err)
	}
}

// stubUpdateGateway swaps the seams cmdUpdate uses for gateway-mode
// dispatch on --status-only updates.
func stubUpdateGateway(t *testing.T) func() {
	t.Helper()
	origTower := activeTowerConfigFunc
	origStatus := updateStatusViaGatewayFunc
	activeTowerConfigFunc = func() (*TowerConfig, error) { return nil, nil }
	updateStatusViaGatewayFunc = func(context.Context, string, string) error {
		t.Fatalf("updateStatusViaGatewayFunc must not be called when no tower is configured")
		return nil
	}
	return func() {
		activeTowerConfigFunc = origTower
		updateStatusViaGatewayFunc = origStatus
	}
}

// TestCmdUpdate_GatewayMode_StatusOnlyRoutesToGatewaySeam verifies that
// `spire update <bead> --status <s>` against a gateway-mode tower
// dispatches to updateStatusViaGatewayFunc with (id, to) and never
// reaches the local store path. Pins the gateway-mode --status routing
// added in spi-wrjiw6.
func TestCmdUpdate_GatewayMode_StatusOnlyRoutesToGatewaySeam(t *testing.T) {
	restore := stubUpdateGateway(t)
	defer restore()

	activeTowerConfigFunc = func() (*TowerConfig, error) {
		return &TowerConfig{Name: "cluster", Mode: config.TowerModeGateway, URL: "https://example.com"}, nil
	}
	var gotID, gotTo string
	updateStatusViaGatewayFunc = func(_ context.Context, id, to string) error {
		gotID = id
		gotTo = to
		return nil
	}

	if err := executeUpdateCmd([]string{"spi-abc", "--status", "open"}); err != nil {
		t.Fatalf("cmdUpdate: %v", err)
	}
	if gotID != "spi-abc" || gotTo != "open" {
		t.Errorf("updateStatusViaGatewayFunc args = (%q, %q), want (spi-abc, open)", gotID, gotTo)
	}
}

// TestCmdUpdate_GatewayMode_StatusPlusOtherFieldsBypassesGateway pins
// the design rule that mixed-flag updates fall through to the local
// path — the gateway endpoint is single-field (`{to}`), so a request
// that also mutates title/priority/labels/etc. has to use direct Dolt
// access. Without this, a mixed update would silently lose the
// non-status changes in gateway mode.
func TestCmdUpdate_GatewayMode_StatusPlusOtherFieldsBypassesGateway(t *testing.T) {
	restore := stubUpdateGateway(t)
	defer restore()

	activeTowerConfigFunc = func() (*TowerConfig, error) {
		return &TowerConfig{Name: "cluster", Mode: config.TowerModeGateway, URL: "https://example.com"}, nil
	}
	updateStatusViaGatewayFunc = func(context.Context, string, string) error {
		t.Fatalf("updateStatusViaGatewayFunc must NOT be called for mixed-flag updates (status + title)")
		return nil
	}
	bead := Bead{ID: "spi-abc", Title: "old", Status: "open"}
	cleanup := stubUpdateDeps(t, bead)
	defer cleanup()

	if err := executeUpdateCmd([]string{"spi-abc", "--status", "open", "--title", "new"}); err != nil {
		t.Fatalf("cmdUpdate: %v", err)
	}
}

// TestCmdUpdate_GatewayMode_ErrorPropagates verifies that gateway-side
// 400 (invalid transition) or 404 surface verbatim — the desktop's
// useful error messages must reach the CLI user.
func TestCmdUpdate_GatewayMode_ErrorPropagates(t *testing.T) {
	restore := stubUpdateGateway(t)
	defer restore()

	activeTowerConfigFunc = func() (*TowerConfig, error) {
		return &TowerConfig{Name: "cluster", Mode: config.TowerModeGateway, URL: "https://example.com"}, nil
	}
	wantErr := errors.New("gatewayclient: HTTP 400: invalid status transition \"closed\" → \"open\"")
	updateStatusViaGatewayFunc = func(context.Context, string, string) error {
		return wantErr
	}

	err := executeUpdateCmd([]string{"spi-abc", "--status", "open"})
	if err == nil {
		t.Fatal("cmdUpdate: nil err, want propagated 400")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want gatewayclient 400", err)
	}
}

// TestCmdResummon_DirectMode_BypassesGateway pins the local-mode
// behaviour: when no tower is gateway-mode the gateway seam must NOT
// be called even if it's been swapped to a recorder.
func TestCmdResummon_DirectMode_BypassesGateway(t *testing.T) {
	restore := stubResummonGateway(t)
	defer restore()

	gatewayCalled := false
	resummonViaGatewayFunc = func(context.Context, string) error {
		gatewayCalled = true
		return nil
	}

	// Tower is nil → cmdResummon falls through to the local path which
	// will error from missing store; we don't care about that error
	// here, only the gateway seam being skipped.
	_ = cmdResummon([]string{"spi-abc"})
	if gatewayCalled {
		t.Errorf("gateway seam invoked in direct-mode tower")
	}
}

// TestCmdUpdate_DirectMode_BypassesGateway pins the local-mode
// behaviour for `spire update --status` against a direct tower.
func TestCmdUpdate_DirectMode_BypassesGateway(t *testing.T) {
	restore := stubUpdateGateway(t)
	defer restore()

	gatewayCalled := false
	updateStatusViaGatewayFunc = func(context.Context, string, string) error {
		gatewayCalled = true
		return nil
	}
	bead := Bead{ID: "spi-abc", Status: "open"}
	cleanup := stubUpdateDeps(t, bead)
	defer cleanup()

	if err := executeUpdateCmd([]string{"spi-abc", "--status", "ready"}); err != nil {
		// Ignored: stubUpdateDeps sets up the local-path stubs so the
		// command should succeed. If the local path errors we still
		// want the assertion to fire.
		_ = strings.Contains(err.Error(), "spi-abc")
	}
	if gatewayCalled {
		t.Errorf("gateway seam invoked in direct-mode tower")
	}
}
