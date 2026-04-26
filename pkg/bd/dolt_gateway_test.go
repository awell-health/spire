package bd

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/awell-health/spire/pkg/config"
)

// gatewayActiveTowerOnDisk primes a temp config home with a single
// gateway-mode tower selected as ActiveTower. Mirrors the helpers in
// pkg/dolt and cmd/spire so each layer's regression test reads as
// standalone.
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

// noBdBinary points the test client at a non-existent binary so any
// path that escapes the gateway guard would surface a different error
// from PATH lookup. The guard must reject with ErrGatewayDirectMutation
// before that point.
func newGuardTestClient() *Client {
	return &Client{BinPath: "/nonexistent/bd-binary-for-test"}
}

func TestDoltCommit_RejectsGatewayMode(t *testing.T) {
	gatewayActiveTowerOnDisk(t)

	c := newGuardTestClient()
	err := c.DoltCommit("hello")
	if err == nil {
		t.Fatal("DoltCommit gateway-mode: got nil, want wrapped ErrGatewayDirectMutation")
	}
	if !errors.Is(err, config.ErrGatewayDirectMutation) {
		t.Fatalf("errors.Is(err, ErrGatewayDirectMutation) = false; got %v", err)
	}
}

func TestDoltPush_RejectsGatewayMode(t *testing.T) {
	gatewayActiveTowerOnDisk(t)

	c := newGuardTestClient()
	err := c.DoltPush("origin", "main")
	if err == nil {
		t.Fatal("DoltPush gateway-mode: got nil, want wrapped ErrGatewayDirectMutation")
	}
	if !errors.Is(err, config.ErrGatewayDirectMutation) {
		t.Fatalf("errors.Is(err, ErrGatewayDirectMutation) = false; got %v", err)
	}
}

func TestDoltPull_RejectsGatewayMode(t *testing.T) {
	gatewayActiveTowerOnDisk(t)

	c := newGuardTestClient()
	err := c.DoltPull("origin", "main")
	if err == nil {
		t.Fatal("DoltPull gateway-mode: got nil, want wrapped ErrGatewayDirectMutation")
	}
	if !errors.Is(err, config.ErrGatewayDirectMutation) {
		t.Fatalf("errors.Is(err, ErrGatewayDirectMutation) = false; got %v", err)
	}
}

func TestDoltRemoteAdd_RejectsGatewayMode(t *testing.T) {
	gatewayActiveTowerOnDisk(t)

	c := newGuardTestClient()
	err := c.DoltRemoteAdd("origin", "https://example.com/repo")
	if err == nil {
		t.Fatal("DoltRemoteAdd gateway-mode: got nil, want wrapped ErrGatewayDirectMutation")
	}
	if !errors.Is(err, config.ErrGatewayDirectMutation) {
		t.Fatalf("errors.Is(err, ErrGatewayDirectMutation) = false; got %v", err)
	}
}

func TestDoltRemoteRemove_RejectsGatewayMode(t *testing.T) {
	gatewayActiveTowerOnDisk(t)

	c := newGuardTestClient()
	err := c.DoltRemoteRemove("origin")
	if err == nil {
		t.Fatal("DoltRemoteRemove gateway-mode: got nil, want wrapped ErrGatewayDirectMutation")
	}
	if !errors.Is(err, config.ErrGatewayDirectMutation) {
		t.Fatalf("errors.Is(err, ErrGatewayDirectMutation) = false; got %v", err)
	}
}

// TestReadOnlyDoltOps_NotGated confirms read-only wrappers (DoltSQL,
// DoltRemoteList) are not rejected by the guard. They exec through the
// missing binary, which produces a non-sentinel error — that is the
// expected shape: guard returns nil, subprocess fails with PATH error.
func TestReadOnlyDoltOps_NotGated(t *testing.T) {
	gatewayActiveTowerOnDisk(t)

	c := newGuardTestClient()
	if _, err := c.DoltSQL("SELECT 1"); errors.Is(err, config.ErrGatewayDirectMutation) {
		t.Errorf("DoltSQL gateway-mode: returned ErrGatewayDirectMutation; read-only ops must not be gated")
	}
	if _, err := c.DoltRemoteList(); errors.Is(err, config.ErrGatewayDirectMutation) {
		t.Errorf("DoltRemoteList gateway-mode: returned ErrGatewayDirectMutation; read-only ops must not be gated")
	}
}
