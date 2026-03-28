package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Full test coverage lives in pkg/steward/daemon_test.go.
// These verify the cmd/spire wrappers compile and delegate correctly.

func TestSyncTowerDerivedConfigs_Wrapper(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("DOLT_DATA_DIR", filepath.Join(tmpDir, "dolt-data"))
	t.Setenv("BEADS_DOLT_SERVER_HOST", "127.0.0.1")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "3307")

	tower := TowerConfig{
		Name:      "acme",
		HubPrefix: "acm",
		Database:  "beads_acm",
	}

	towerBeadsDir := filepath.Join(doltDataDir(), tower.Database, ".beads")
	if err := os.MkdirAll(towerBeadsDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	staleConfig := "dolt.host: \"old-host\"\ndolt.port: 9999\n"
	if err := os.WriteFile(filepath.Join(towerBeadsDir, "config.yaml"), []byte(staleConfig), 0644); err != nil {
		t.Fatalf("write stale config.yaml: %v", err)
	}

	syncTowerDerivedConfigs(tower)

	data, err := os.ReadFile(filepath.Join(towerBeadsDir, "config.yaml"))
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "127.0.0.1") {
		t.Errorf("config.yaml missing updated host, got: %q", got)
	}
	if strings.Contains(got, "old-host") {
		t.Errorf("config.yaml still contains stale host: %q", got)
	}
}
