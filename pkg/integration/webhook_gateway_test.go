package integration

import (
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/awell-health/spire/pkg/config"
)

// gatewayActiveTowerOnDisk primes a temp config home with a single
// gateway-mode tower selected as ActiveTower so
// config.EnsureNotGatewayResolved trips. Mirrors the fixture used in
// pkg/dolt and pkg/bd so each layer's regression test stands alone.
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

// TestProcessWebhookQueue_GatewayModeIsNoOp confirms the queue drainer
// short-circuits before any DoltSQL invocation on a gateway-mode tower.
// In cluster-as-truth deployments the cluster's daemon owns the
// webhook_queue table; the laptop must not touch local Dolt to mark
// rows processed. The test installs a DoltSQL callback that fails the
// test if it ever gets called — the guard must return before it reaches
// the SELECT.
func TestProcessWebhookQueue_GatewayModeIsNoOp(t *testing.T) {
	gatewayActiveTowerOnDisk(t)

	prevDoltSQL := DoltSQL
	t.Cleanup(func() { DoltSQL = prevDoltSQL })

	var called int32
	DoltSQL = func(query string, jsonOutput bool) (string, error) {
		atomic.AddInt32(&called, 1)
		return "", nil
	}

	processed, errs := ProcessWebhookQueue()
	if processed != 0 || errs != 0 {
		t.Errorf("ProcessWebhookQueue gateway-mode: (processed,errs) = (%d,%d), want (0,0)", processed, errs)
	}
	if got := atomic.LoadInt32(&called); got != 0 {
		t.Errorf("DoltSQL was called %d time(s) under gateway-mode; want 0 (queue is server-owned)", got)
	}
}

// TestProcessWebhookQueue_DirectModeReachesDoltSQL confirms direct-mode
// passthrough: the guard returns nil, so the function attempts the
// SELECT against DoltSQL. We supply an empty-result callback so the
// drain runs cleanly without any real database. The callback must be
// invoked at least once — that is the evidence the guard did not
// regress to "always reject".
func TestProcessWebhookQueue_DirectModeReachesDoltSQL(t *testing.T) {
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

	prevDoltSQL := DoltSQL
	t.Cleanup(func() { DoltSQL = prevDoltSQL })

	var called int32
	DoltSQL = func(query string, jsonOutput bool) (string, error) {
		atomic.AddInt32(&called, 1)
		return "", nil // empty queue
	}

	if processed, errs := ProcessWebhookQueue(); processed != 0 || errs != 0 {
		t.Errorf("ProcessWebhookQueue direct-mode (empty queue): (%d,%d), want (0,0)", processed, errs)
	}
	if got := atomic.LoadInt32(&called); got == 0 {
		t.Errorf("DoltSQL was not called under direct-mode; guard regressed to always-reject")
	}
}
