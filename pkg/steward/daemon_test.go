package steward

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/dolt"
)

func TestSyncTowerDerivedConfigs_UpdatesConfigYAML(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("DOLT_DATA_DIR", filepath.Join(tmpDir, "dolt-data"))
	// Use a predictable host/port so we can assert the output.
	t.Setenv("BEADS_DOLT_SERVER_HOST", "127.0.0.1")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "3307")

	tower := config.TowerConfig{
		Name:      "acme",
		HubPrefix: "acm",
		Database:  "beads_acm",
	}

	// Create the tower's .beads/ directory with a stale config.yaml.
	towerBeadsDir := filepath.Join(dolt.DataDir(), tower.Database, ".beads")
	if err := os.MkdirAll(towerBeadsDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	staleConfig := "dolt.host: \"old-host\"\ndolt.port: 9999\n"
	if err := os.WriteFile(filepath.Join(towerBeadsDir, "config.yaml"), []byte(staleConfig), 0644); err != nil {
		t.Fatalf("write stale config.yaml: %v", err)
	}

	SyncTowerDerivedConfigs(tower)

	data, err := os.ReadFile(filepath.Join(towerBeadsDir, "config.yaml"))
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "127.0.0.1") {
		t.Errorf("config.yaml missing updated host, got: %q", got)
	}
	if !strings.Contains(got, "3307") {
		t.Errorf("config.yaml missing updated port, got: %q", got)
	}
	if strings.Contains(got, "old-host") {
		t.Errorf("config.yaml still contains stale host: %q", got)
	}
}

func TestSyncTowerDerivedConfigs_UpdatesRepoPaths(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("DOLT_DATA_DIR", filepath.Join(tmpDir, "dolt-data"))
	t.Setenv("BEADS_DOLT_SERVER_HOST", "127.0.0.1")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "3307")

	tower := config.TowerConfig{
		Name:      "acme",
		HubPrefix: "acm",
		Database:  "beads_acm",
	}

	// Create tower .beads/ dir so sync doesn't skip.
	towerBeadsDir := filepath.Join(dolt.DataDir(), tower.Database, ".beads")
	if err := os.MkdirAll(towerBeadsDir, 0755); err != nil {
		t.Fatalf("mkdir tower beadsDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(towerBeadsDir, "config.yaml"), []byte(""), 0644); err != nil {
		t.Fatalf("write tower config.yaml: %v", err)
	}

	// Set up a repo with a local .beads/ containing stale config.yaml.
	repoDir := filepath.Join(tmpDir, "my-repo")
	repoBeadsDir := filepath.Join(repoDir, ".beads")
	if err := os.MkdirAll(repoBeadsDir, 0755); err != nil {
		t.Fatalf("mkdir repo beadsDir: %v", err)
	}
	staleConfig := "dolt.host: \"stale-host\"\ndolt.port: 1234\n"
	if err := os.WriteFile(filepath.Join(repoBeadsDir, "config.yaml"), []byte(staleConfig), 0644); err != nil {
		t.Fatalf("write repo config.yaml: %v", err)
	}

	// Register the instance in config.json.
	cfg := &config.SpireConfig{
		Instances: map[string]*config.Instance{
			"web": {
				Path:     repoDir,
				Prefix:   "web",
				Database: "beads_acm",
				Tower:    "acme",
			},
		},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	SyncTowerDerivedConfigs(tower)

	data, err := os.ReadFile(filepath.Join(repoBeadsDir, "config.yaml"))
	if err != nil {
		t.Fatalf("read repo config.yaml: %v", err)
	}
	got := string(data)
	if strings.Contains(got, "stale-host") {
		t.Errorf("repo config.yaml still has stale host: %q", got)
	}
	if !strings.Contains(got, "127.0.0.1") {
		t.Errorf("repo config.yaml missing updated host: %q", got)
	}
}

func TestSyncTowerDerivedConfigs_FixesDriftedDatabase(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("DOLT_DATA_DIR", filepath.Join(tmpDir, "dolt-data"))

	tower := config.TowerConfig{
		Name:      "acme",
		HubPrefix: "acm",
		Database:  "beads_acm",
	}

	// Create tower .beads/ dir.
	towerBeadsDir := filepath.Join(dolt.DataDir(), tower.Database, ".beads")
	if err := os.MkdirAll(towerBeadsDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(towerBeadsDir, "config.yaml"), []byte(""), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Instance has wrong database (drifted from tower).
	cfg := &config.SpireConfig{
		Instances: map[string]*config.Instance{
			"acm": {
				Path:     "/tmp/acm-repo",
				Prefix:   "acm",
				Database: "old_database", // drifted
				Tower:    "acme",
			},
		},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	SyncTowerDerivedConfigs(tower)

	loaded, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	inst := loaded.Instances["acm"]
	if inst == nil {
		t.Fatal("instance 'acm' not found after sync")
	}
	if inst.Database != tower.Database {
		t.Errorf("instance Database = %q, want %q (tower database)", inst.Database, tower.Database)
	}
}

func TestSyncTowerDerivedConfigs_SkipsOtherTowers(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("DOLT_DATA_DIR", filepath.Join(tmpDir, "dolt-data"))

	tower := config.TowerConfig{
		Name:     "acme",
		Database: "beads_acm",
	}

	// Create tower .beads/ dir.
	towerBeadsDir := filepath.Join(dolt.DataDir(), tower.Database, ".beads")
	if err := os.MkdirAll(towerBeadsDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(towerBeadsDir, "config.yaml"), []byte(""), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Instance belongs to a different tower with wrong database.
	cfg := &config.SpireConfig{
		Instances: map[string]*config.Instance{
			"web": {
				Path:     "/tmp/other",
				Prefix:   "web",
				Database: "old_database",
				Tower:    "other-tower", // different tower
			},
		},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	SyncTowerDerivedConfigs(tower)

	loaded, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	inst := loaded.Instances["web"]
	if inst == nil {
		t.Fatal("instance 'web' missing after sync")
	}
	// Should NOT have been updated — different tower.
	if inst.Database != "old_database" {
		t.Errorf("instance Database = %q, want %q (should be unchanged)", inst.Database, "old_database")
	}
}
