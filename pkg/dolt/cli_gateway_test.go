package dolt

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/awell-health/spire/pkg/config"
)

// CLIPush / CLIPull / CLIFetchMerge are guarded by
// config.EnsureNotGatewayResolved so a gateway-mode tower cannot reach
// the dolt subprocess even when a caller bypasses cmd/spire's own
// guards. These tests pin defense-in-depth — if cmd/spire ever loses
// its preflight guard, the dolt layer still fails closed.

// gatewayActiveTowerOnDisk primes a temp config home with a single
// gateway-mode tower selected as ActiveTower so ResolveTowerConfig
// returns it. The caller's CWD is moved to tmpDir so the resolver does
// not fall through to a CWD-mapped instance.
func gatewayActiveTowerOnDisk(t *testing.T) {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SPIRE_CONFIG_DIR", filepath.Join(tmpDir, "spire"))
	t.Setenv("SPIRE_TOWER", "")

	gw := &config.TowerConfig{
		Name:      "spi",
		ProjectID: "11111111-2222-4333-8444-555555555555",
		HubPrefix: "spi",
		Database:  "beads_spi",
		CreatedAt: "2026-04-26T12:00:00Z",
		Mode:      config.TowerModeGateway,
		URL:       "http://127.0.0.1:3030",
		TokenRef:  "spi",
	}
	if err := config.SaveTowerConfig(gw); err != nil {
		t.Fatalf("SaveTowerConfig: %v", err)
	}
	if err := config.Save(&config.SpireConfig{
		ActiveTower: gw.Name,
		Instances:   map[string]*config.Instance{},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	t.Chdir(tmpDir)
}

// directActiveTowerOnDisk primes a direct-mode tower so direct-mode
// passthrough tests can confirm the guard does not regress to "always
// reject". The data dir is left invalid because the test only asserts
// that the guard is bypassed — the subsequent dolt subprocess failure
// (no binary, no data dir) is acceptable evidence the guard returned.
func directActiveTowerOnDisk(t *testing.T) {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SPIRE_CONFIG_DIR", filepath.Join(tmpDir, "spire"))
	t.Setenv("SPIRE_TOWER", "")

	direct := &config.TowerConfig{
		Name:      "spi-local",
		ProjectID: "22222222-3333-4444-8555-666666666666",
		HubPrefix: "spi",
		Database:  "beads_spi",
		CreatedAt: "2026-04-26T12:00:00Z",
		Mode:      config.TowerModeDirect,
	}
	if err := config.SaveTowerConfig(direct); err != nil {
		t.Fatalf("SaveTowerConfig: %v", err)
	}
	if err := config.Save(&config.SpireConfig{
		ActiveTower: direct.Name,
		Instances:   map[string]*config.Instance{},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	t.Chdir(tmpDir)
}

func TestCLIPush_RejectsGatewayMode(t *testing.T) {
	gatewayActiveTowerOnDisk(t)

	err := CLIPush(context.Background(), "/tmp/does-not-matter", false)
	if err == nil {
		t.Fatal("CLIPush gateway-mode: got nil, want wrapped ErrGatewayDirectMutation")
	}
	if !errors.Is(err, config.ErrGatewayDirectMutation) {
		t.Fatalf("errors.Is(err, ErrGatewayDirectMutation) = false; got %v", err)
	}
}

func TestCLIPull_RejectsGatewayMode(t *testing.T) {
	gatewayActiveTowerOnDisk(t)

	err := CLIPull(context.Background(), "/tmp/does-not-matter", false)
	if err == nil {
		t.Fatal("CLIPull gateway-mode: got nil, want wrapped ErrGatewayDirectMutation")
	}
	if !errors.Is(err, config.ErrGatewayDirectMutation) {
		t.Fatalf("errors.Is(err, ErrGatewayDirectMutation) = false; got %v", err)
	}
}

func TestCLIFetchMerge_RejectsGatewayMode(t *testing.T) {
	gatewayActiveTowerOnDisk(t)

	out, err := CLIFetchMerge(context.Background(), "/tmp/does-not-matter")
	if err == nil {
		t.Fatal("CLIFetchMerge gateway-mode: got nil, want wrapped ErrGatewayDirectMutation")
	}
	if !errors.Is(err, config.ErrGatewayDirectMutation) {
		t.Fatalf("errors.Is(err, ErrGatewayDirectMutation) = false; got %v", err)
	}
	if out != "" {
		t.Errorf("CLIFetchMerge gateway-mode: out = %q, want empty (guard returned before fetch)", out)
	}
}

// TestCLIPush_DirectModeBypassesGuard confirms a direct-mode tower does
// not return the gateway sentinel — it falls through to the dolt
// subprocess. The subprocess will fail on the bogus dataDir, but the
// returned error must NOT match ErrGatewayDirectMutation; otherwise we
// would have regressed direct-mode flows.
func TestCLIPush_DirectModeBypassesGuard(t *testing.T) {
	directActiveTowerOnDisk(t)

	err := CLIPush(context.Background(), "/tmp/does-not-exist-xyz", false)
	if err == nil {
		// Either dolt isn't installed or the bogus path tripped before
		// dolt's own validation. Either way the guard didn't reject —
		// pass.
		return
	}
	if errors.Is(err, config.ErrGatewayDirectMutation) {
		t.Errorf("CLIPush direct-mode: returned ErrGatewayDirectMutation (guard misfired)")
	}
}

func TestCLIPull_DirectModeBypassesGuard(t *testing.T) {
	directActiveTowerOnDisk(t)

	err := CLIPull(context.Background(), "/tmp/does-not-exist-xyz", false)
	if err == nil {
		return
	}
	if errors.Is(err, config.ErrGatewayDirectMutation) {
		t.Errorf("CLIPull direct-mode: returned ErrGatewayDirectMutation (guard misfired)")
	}
}

func TestCLIFetchMerge_DirectModeBypassesGuard(t *testing.T) {
	directActiveTowerOnDisk(t)

	_, err := CLIFetchMerge(context.Background(), "/tmp/does-not-exist-xyz")
	if err == nil {
		return
	}
	if errors.Is(err, config.ErrGatewayDirectMutation) {
		t.Errorf("CLIFetchMerge direct-mode: returned ErrGatewayDirectMutation (guard misfired)")
	}
}

// TestSetCLIRemote_GatewayModeIsNoOp covers the only mutation helper in
// pkg/dolt that returns void: SetCLIRemote stages the remote in
// .dolt/config.json by exec'ing `dolt remote remove/add`. Under gateway
// mode the guard short-circuits before either subprocess runs, so no
// files appear in the data directory. The test passes a fresh temp dir
// and asserts nothing is written there — if the guard ever regressed
// and a real dolt binary were available, `remote add` would create
// `.dolt/repo_state.json` and the assertion would catch it.
func TestSetCLIRemote_GatewayModeIsNoOp(t *testing.T) {
	gatewayActiveTowerOnDisk(t)

	dataDir := t.TempDir()
	SetCLIRemote(dataDir, "origin", "http://example.com/repo")

	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatalf("read dataDir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("SetCLIRemote gateway-mode wrote files into dataDir: %v (guard should have returned before exec)", names)
	}
}

// TestEnsureDoltHubDB_RejectsGatewayMode pins the guard on the only
// direct-DoltHub-API call site in pkg/dolt. EnsureDoltHubDB POSTs to
// dolthub.com/api/v1alpha1/database to create a missing remote database;
// that REST call is a direct mutation against DoltHub, which is exactly
// what gateway-mode towers must not perform in cluster-as-truth
// deployments. The CLI push path already trips RejectIfGateway upstream,
// so this guard is defense-in-depth — any caller reaching the helper
// directly still fails closed before any HTTP I/O.
func TestEnsureDoltHubDB_RejectsGatewayMode(t *testing.T) {
	gatewayActiveTowerOnDisk(t)

	err := EnsureDoltHubDB("https://doltremoteapi.dolthub.com/awell/legacy")
	if err == nil {
		t.Fatal("EnsureDoltHubDB gateway-mode: got nil, want wrapped ErrGatewayDirectMutation")
	}
	if !errors.Is(err, config.ErrGatewayDirectMutation) {
		t.Fatalf("errors.Is(err, ErrGatewayDirectMutation) = false; got %v", err)
	}
}
