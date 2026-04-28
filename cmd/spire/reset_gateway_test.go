package main

import (
	"context"
	"errors"
	"testing"

	"github.com/awell-health/spire/pkg/config"
	resetpkg "github.com/awell-health/spire/pkg/reset"
)

// stubResetGateway swaps the package-level seams cmdReset uses for
// gateway-mode dispatch (activeTowerConfigFunc + gatewayResetBeadFunc +
// resetHardBeadViaGatewayFunc) and the in-process runResetCore path.
// The returned restore function must be called via defer to put the
// production seams back.
func stubResetGateway(t *testing.T) func() {
	t.Helper()

	origTower := activeTowerConfigFunc
	origGateway := gatewayResetBeadFunc
	origHard := resetHardBeadViaGatewayFunc

	// Default to a non-gateway tower so cmdReset takes the in-process
	// runResetCore branch. Tests that need the gateway path swap this
	// seam explicitly.
	activeTowerConfigFunc = func() (*TowerConfig, error) { return nil, nil }
	gatewayResetBeadFunc = func(context.Context, string, resetpkg.Opts) error {
		t.Fatalf("gatewayResetBeadFunc must not be called in direct-mode tests")
		return nil
	}
	resetHardBeadViaGatewayFunc = func(context.Context, string) error {
		t.Fatalf("resetHardBeadViaGatewayFunc must not be called in direct-mode tests")
		return nil
	}

	return func() {
		activeTowerConfigFunc = origTower
		gatewayResetBeadFunc = origGateway
		resetHardBeadViaGatewayFunc = origHard
	}
}

// TestCmdResetGatewayModeRoutesToGatewayClient verifies that cmdReset, on
// a gateway-mode tower, dispatches to the gateway-mode seam with the
// parsed flag values copied verbatim. The local reset core is short-
// circuited so the gateway endpoint is the single source of truth.
func TestCmdResetGatewayModeRoutesToGatewayClient(t *testing.T) {
	restore := stubResetGateway(t)
	defer restore()

	activeTowerConfigFunc = func() (*TowerConfig, error) {
		return &TowerConfig{Name: "cluster", Mode: config.TowerModeGateway, URL: "https://example.com"}, nil
	}

	var gotID string
	var gotOpts resetpkg.Opts
	gatewayResetBeadFunc = func(_ context.Context, id string, opts resetpkg.Opts) error {
		gotID = id
		gotOpts = opts
		return nil
	}

	if err := cmdReset([]string{"spi-abc", "--to", "review", "--force", "--set", "implement.outputs.outcome=verified"}); err != nil {
		t.Fatalf("cmdReset: %v", err)
	}
	if gotID != "spi-abc" {
		t.Errorf("gateway id = %q, want spi-abc", gotID)
	}
	if gotOpts.To != "review" {
		t.Errorf("opts.To = %q, want review", gotOpts.To)
	}
	if !gotOpts.Force {
		t.Errorf("opts.Force = false, want true")
	}
	if got := gotOpts.Set["implement.outputs.outcome"]; got != "verified" {
		t.Errorf("opts.Set[implement.outputs.outcome] = %q, want verified", got)
	}
}

// TestCmdResetGatewayModePropagatesError verifies that gateway-side
// reset errors surface verbatim — the user must see why the reset
// failed, not a generic "reset failed" message.
func TestCmdResetGatewayModePropagatesError(t *testing.T) {
	restore := stubResetGateway(t)
	defer restore()

	activeTowerConfigFunc = func() (*TowerConfig, error) {
		return &TowerConfig{Name: "cluster", Mode: config.TowerModeGateway, URL: "https://example.com"}, nil
	}
	wantErr := errors.New("gatewayclient: HTTP 409: step has not been reached")
	gatewayResetBeadFunc = func(context.Context, string, resetpkg.Opts) error {
		return wantErr
	}

	err := cmdReset([]string{"spi-abc", "--to", "review"})
	if err == nil {
		t.Fatal("cmdReset: nil err, want HTTP 409 from gateway")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want gatewayclient error", err)
	}
}

// TestCmdResetDirectModeBypassesGateway pins the local-mode behaviour:
// when no tower is gateway-mode the gateway seam must NOT be called.
func TestCmdResetDirectModeBypassesGateway(t *testing.T) {
	restore := stubResetGateway(t)
	defer restore()

	// No tower → activeTowerConfigFunc returns nil. cmdReset still hits the
	// in-process runResetCore which would attempt store reads we don't
	// want here, so route through a stand-in by stubbing only what's
	// needed: the test verifies the gateway path is *not* taken.
	gatewayCalled := false
	gatewayResetBeadFunc = func(context.Context, string, resetpkg.Opts) error {
		gatewayCalled = true
		return nil
	}

	// We expect cmdReset to fall through to runResetCore which will fail
	// (no store, no bead), but the assertion is on the gateway seam.
	_ = cmdReset([]string{"spi-abc"})
	if gatewayCalled {
		t.Errorf("gateway seam invoked in direct-mode tower")
	}
}

// TestCmdResetGatewayModeHardRoutesToHardEndpoint verifies that `--hard`
// dispatches to the dedicated /api/v1/beads/{id}/reset_hard seam (not
// the multipurpose /reset endpoint). This pins the spi-wrjiw6 catalog's
// one-verb-per-endpoint shape: the destructive variant has its own
// route so the manifest can list it alongside dismiss / resummon /
// update_status without overloading reset's body fields.
func TestCmdResetGatewayModeHardRoutesToHardEndpoint(t *testing.T) {
	restore := stubResetGateway(t)
	defer restore()

	activeTowerConfigFunc = func() (*TowerConfig, error) {
		return &TowerConfig{Name: "cluster", Mode: config.TowerModeGateway, URL: "https://example.com"}, nil
	}

	var hardCalledID string
	resetHardBeadViaGatewayFunc = func(_ context.Context, id string) error {
		hardCalledID = id
		return nil
	}
	gatewayResetBeadFunc = func(context.Context, string, resetpkg.Opts) error {
		t.Fatalf("--hard must NOT route through gatewayResetBeadFunc; expected resetHardBeadViaGatewayFunc")
		return nil
	}

	if err := cmdReset([]string{"spi-abc", "--hard"}); err != nil {
		t.Fatalf("cmdReset: %v", err)
	}
	if hardCalledID != "spi-abc" {
		t.Errorf("resetHardBeadViaGatewayFunc id = %q, want spi-abc", hardCalledID)
	}
}

// TestCmdResetGatewayModeHardErrorPropagates verifies that the
// reset_hard gateway seam's errors surface verbatim — the user must
// see why the reset failed (e.g. 501 cluster-mode), not a generic
// wrapped message.
func TestCmdResetGatewayModeHardErrorPropagates(t *testing.T) {
	restore := stubResetGateway(t)
	defer restore()

	activeTowerConfigFunc = func() (*TowerConfig, error) {
		return &TowerConfig{Name: "cluster", Mode: config.TowerModeGateway, URL: "https://example.com"}, nil
	}
	wantErr := errors.New("gatewayclient: HTTP 501: reset not supported in cluster mode yet")
	resetHardBeadViaGatewayFunc = func(context.Context, string) error {
		return wantErr
	}

	err := cmdReset([]string{"spi-abc", "--hard"})
	if err == nil {
		t.Fatal("cmdReset: nil err, want HTTP 501 from reset_hard gateway seam")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want gatewayclient error", err)
	}
}
