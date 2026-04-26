package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/config"
	towerpkg "github.com/awell-health/spire/pkg/tower"
)

func TestTowerConfigSaveLoadRoundtrip(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	tower := &TowerConfig{
		Name:          "test-team",
		ProjectID:     "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		HubPrefix:     "tes",
		DolthubRemote: "https://doltremoteapi.dolthub.com/org/repo",
		Database:      "beads_tes",
		CreatedAt:     "2026-03-21T10:00:00Z",
	}

	if err := saveTowerConfig(tower); err != nil {
		t.Fatalf("saveTowerConfig: %v", err)
	}

	loaded, err := loadTowerConfig("test-team")
	if err != nil {
		t.Fatalf("loadTowerConfig: %v", err)
	}

	if loaded.Name != tower.Name {
		t.Errorf("Name = %q, want %q", loaded.Name, tower.Name)
	}
	if loaded.ProjectID != tower.ProjectID {
		t.Errorf("ProjectID = %q, want %q", loaded.ProjectID, tower.ProjectID)
	}
	if loaded.HubPrefix != tower.HubPrefix {
		t.Errorf("HubPrefix = %q, want %q", loaded.HubPrefix, tower.HubPrefix)
	}
	if loaded.DolthubRemote != tower.DolthubRemote {
		t.Errorf("DolthubRemote = %q, want %q", loaded.DolthubRemote, tower.DolthubRemote)
	}
	if loaded.Database != tower.Database {
		t.Errorf("Database = %q, want %q", loaded.Database, tower.Database)
	}
	if loaded.CreatedAt != tower.CreatedAt {
		t.Errorf("CreatedAt = %q, want %q", loaded.CreatedAt, tower.CreatedAt)
	}
}

func TestTowerConfigJSON(t *testing.T) {
	tower := &TowerConfig{
		Name:      "my-team",
		ProjectID: "12345678-1234-4234-8234-123456789abc",
		HubPrefix: "myt",
		Database:  "beads_myt",
		CreatedAt: "2026-03-21T10:00:00Z",
	}

	data, err := json.Marshal(tower)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// DolthubRemote should be omitted when empty (omitempty tag)
	if strings.Contains(string(data), "dolthub_remote") {
		t.Error("expected dolthub_remote to be omitted when empty")
	}

	// Set it and verify it's included
	tower.DolthubRemote = "https://doltremoteapi.dolthub.com/org/repo"
	data, err = json.Marshal(tower)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), "dolthub_remote") {
		t.Error("expected dolthub_remote to be present when set")
	}
}

func TestDerivePrefixFromName(t *testing.T) {
	tests := []struct {
		name   string
		want   string
	}{
		{"my-team", "myt"},
		{"hello", "hel"},
		{"AB", "ab"},
		{"a", "a"},
		{"---", "hub"}, // no alphanumeric chars
		{"", "hub"},
		{"123", "123"},
		{"X-Y-Z", "xyz"},
		{"My Cool Team", "myc"},
		{"@#$abc", "abc"},
	}

	for _, tc := range tests {
		got := derivePrefixFromName(tc.name)
		if got != tc.want {
			t.Errorf("derivePrefixFromName(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestReadBeadsProjectID(t *testing.T) {
	tmpDir := t.TempDir()

	// Write a metadata.json with a project_id
	metaPath := filepath.Join(tmpDir, "metadata.json")
	content := `{"project_id": "abc-123-def", "database": "dolt"}`
	if err := os.WriteFile(metaPath, []byte(content), 0644); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}

	pid, err := readBeadsProjectID(tmpDir)
	if err != nil {
		t.Fatalf("readBeadsProjectID: %v", err)
	}
	if pid != "abc-123-def" {
		t.Errorf("project_id = %q, want %q", pid, "abc-123-def")
	}
}

func TestReadBeadsProjectID_Missing(t *testing.T) {
	tmpDir := t.TempDir()

	// No metadata.json at all
	_, err := readBeadsProjectID(tmpDir)
	if err == nil {
		t.Error("expected error for missing metadata.json, got nil")
	}

	// metadata.json without project_id
	metaPath := filepath.Join(tmpDir, "metadata.json")
	if err := os.WriteFile(metaPath, []byte(`{"database": "dolt"}`), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err = readBeadsProjectID(tmpDir)
	if err == nil {
		t.Error("expected error for missing project_id, got nil")
	}
}

func TestReposTableSQL(t *testing.T) {
	// Verify the SQL is non-empty and contains expected keywords. The
	// constant moved to pkg/tower in spi-2xf158 so both local-native and
	// cluster-native bootstrap paths share a single DDL source of truth.
	sql := towerpkg.ReposTableSQL
	if sql == "" {
		t.Fatal("ReposTableSQL is empty")
	}
	if !strings.Contains(sql, "CREATE TABLE") {
		t.Error("ReposTableSQL missing CREATE TABLE")
	}
	if !strings.Contains(sql, "repos") {
		t.Error("ReposTableSQL missing table name 'repos'")
	}
	if !strings.Contains(sql, "prefix") {
		t.Error("ReposTableSQL missing 'prefix' column")
	}
	if !strings.Contains(sql, "repo_url") {
		t.Error("ReposTableSQL missing 'repo_url' column")
	}
	if !strings.Contains(sql, "branch") {
		t.Error("ReposTableSQL missing 'branch' column")
	}
	if !strings.Contains(sql, "PRIMARY KEY") {
		t.Error("ReposTableSQL missing PRIMARY KEY")
	}
	if !strings.Contains(sql, "IF NOT EXISTS") {
		t.Error("ReposTableSQL missing IF NOT EXISTS")
	}
}

func TestTowerConfigDir(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	dir, err := towerConfigDir()
	if err != nil {
		t.Fatalf("towerConfigDir: %v", err)
	}

	expected := filepath.Join(tmpDir, ".config", "spire", "towers")
	if dir != expected {
		t.Errorf("towerConfigDir = %q, want %q", dir, expected)
	}

	// Should have created the directory
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat tower config dir: %v", err)
	}
	if !info.IsDir() {
		t.Error("tower config dir is not a directory")
	}
}

func TestTowerConfigPath(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	path, err := towerConfigPath("my-team")
	if err != nil {
		t.Fatalf("towerConfigPath: %v", err)
	}

	expected := filepath.Join(tmpDir, ".config", "spire", "towers", "my-team.json")
	if path != expected {
		t.Errorf("towerConfigPath = %q, want %q", path, expected)
	}
}

func TestListTowerConfigs_Empty(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	towers, err := listTowerConfigs()
	if err != nil {
		t.Fatalf("listTowerConfigs: %v", err)
	}
	if len(towers) != 0 {
		t.Errorf("expected 0 towers, got %d", len(towers))
	}
}

func TestListTowerConfigs_Multiple(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	// Save two towers
	t1 := &TowerConfig{
		Name:      "alpha",
		ProjectID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		HubPrefix: "alp",
		Database:  "beads_alp",
		CreatedAt: "2026-03-21T10:00:00Z",
	}
	t2 := &TowerConfig{
		Name:          "beta",
		ProjectID:     "11111111-2222-4333-8444-555555555555",
		HubPrefix:     "bet",
		DolthubRemote: "https://doltremoteapi.dolthub.com/org/beads_bet",
		Database:      "beads_bet",
		CreatedAt:     "2026-03-21T11:00:00Z",
	}

	if err := saveTowerConfig(t1); err != nil {
		t.Fatalf("save t1: %v", err)
	}
	if err := saveTowerConfig(t2); err != nil {
		t.Fatalf("save t2: %v", err)
	}

	towers, err := listTowerConfigs()
	if err != nil {
		t.Fatalf("listTowerConfigs: %v", err)
	}
	if len(towers) != 2 {
		t.Fatalf("expected 2 towers, got %d", len(towers))
	}

	// Check that both names are present (order may vary by filesystem)
	names := map[string]bool{}
	for _, tc := range towers {
		names[tc.Name] = true
	}
	if !names["alpha"] || !names["beta"] {
		t.Errorf("expected towers 'alpha' and 'beta', got %v", names)
	}
}

func TestNormalizeDolthubURL(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"org/repo", "https://doltremoteapi.dolthub.com/org/repo"},
		{"https://doltremoteapi.dolthub.com/org/repo", "https://doltremoteapi.dolthub.com/org/repo"},
		{"http://localhost:8080/test", "http://localhost:8080/test"},
		{"myorg/myrepo", "https://doltremoteapi.dolthub.com/myorg/myrepo"},
	}

	for _, tc := range tests {
		got := normalizeDolthubURL(tc.input)
		if got != tc.want {
			t.Errorf("normalizeDolthubURL(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestNameFromDolthubURL(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"org/repo", "repo"},
		{"https://doltremoteapi.dolthub.com/org/beads_hub", "beads_hub"},
		{"https://www.dolthub.com/repositories/org/beads_x", "beads_x"},
		{"simple", "simple"},
		{"", ""},
	}

	for _, tc := range tests {
		got := nameFromDolthubURL(tc.input)
		if got != tc.want {
			t.Errorf("nameFromDolthubURL(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestExtractSQLValue(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		want   string
	}{
		{
			"pipe-delimited",
			"+-------+\n| value |\n+-------+\n| abc   |\n+-------+",
			"abc",
		},
		{
			"count",
			"+----------+\n| COUNT(*) |\n+----------+\n| 42       |\n+----------+",
			"42",
		},
		{
			"plain value",
			"hello",
			"hello",
		},
		{
			"empty",
			"",
			"",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractSQLValue(tc.input)
			if got != tc.want {
				t.Errorf("extractSQLValue = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLoadTowerConfig_NotFound(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	_, err := loadTowerConfig("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent tower config, got nil")
	}
}

func TestCmdTower_UnknownSubcommand(t *testing.T) {
	err := cmdTower([]string{"bogus"})
	if err == nil {
		t.Fatal("expected error for unknown subcommand, got nil")
	}
	if !strings.Contains(err.Error(), "unknown tower subcommand") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCmdTowerCreate_MissingName(t *testing.T) {
	err := cmdTowerCreate([]string{})
	if err == nil {
		t.Fatal("expected error for missing name, got nil")
	}
	if !strings.Contains(err.Error(), "--name is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCmdTowerCreate_UnknownFlag(t *testing.T) {
	err := cmdTowerCreate([]string{"--bogus"})
	if err == nil {
		t.Fatal("expected error for unknown flag, got nil")
	}
	if !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestCmdTowerCreate_InvalidMode rejects --mode values that don't match the
// canonical constants. Silent acceptance of e.g. "cluster_native" would
// cause hard-to-debug drift between CLI, persisted config, and the chart.
func TestCmdTowerCreate_InvalidMode(t *testing.T) {
	cases := []struct {
		name string
		arg  string
	}{
		{name: "unknown word", arg: "--mode=garbage"},
		{name: "underscore form", arg: "--mode=cluster_native"},
		{name: "casing mismatch", arg: "--mode=Cluster-Native"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := cmdTowerCreate([]string{"--name=t1", tc.arg})
			if err == nil {
				t.Fatalf("expected error for %q, got nil", tc.arg)
			}
			if !strings.Contains(err.Error(), "invalid --mode value") {
				t.Errorf("unexpected error for %q: %v", tc.arg, err)
			}
		})
	}
}

func TestCmdTowerAttach_NoArgs(t *testing.T) {
	err := cmdTowerAttach([]string{})
	if err == nil {
		t.Fatal("expected error for no args, got nil")
	}
	if !strings.Contains(err.Error(), "usage:") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestMustHelper(t *testing.T) {
	got := must("hello", nil)
	if got != "hello" {
		t.Errorf("must with nil error = %q, want %q", got, "hello")
	}

	got = must("", os.ErrNotExist)
	if got != "" {
		t.Errorf("must with error = %q, want empty", got)
	}
}

func TestTowerConfigForDatabase(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SPIRE_CONFIG_DIR", tmpDir)

	// Save two towers
	t1 := &TowerConfig{
		Name:      "alpha",
		ProjectID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		HubPrefix: "alp",
		Database:  "beads_alp",
		CreatedAt: "2026-03-22T10:00:00Z",
	}
	t2 := &TowerConfig{
		Name:      "beta",
		ProjectID: "11111111-2222-4333-8444-555555555555",
		HubPrefix: "bet",
		Database:  "beads_bet",
		CreatedAt: "2026-03-22T11:00:00Z",
	}
	if err := saveTowerConfig(t1); err != nil {
		t.Fatalf("save t1: %v", err)
	}
	if err := saveTowerConfig(t2); err != nil {
		t.Fatalf("save t2: %v", err)
	}

	// Exact match
	tc, err := towerConfigForDatabase("beads_alp")
	if err != nil {
		t.Fatalf("exact match: %v", err)
	}
	if tc.Name != "alpha" {
		t.Errorf("exact match Name = %q, want %q", tc.Name, "alpha")
	}

	// beads_ prefix match (bare prefix → "beads_"+prefix)
	tc, err = towerConfigForDatabase("bet")
	if err != nil {
		t.Fatalf("prefix match: %v", err)
	}
	if tc.Name != "beta" {
		t.Errorf("prefix match Name = %q, want %q", tc.Name, "beta")
	}

	// Not found — must fail even when ActiveTower is set (no silent fallback)
	cfg := &SpireConfig{
		Instances:   map[string]*Instance{},
		ActiveTower: "alpha",
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	_, err = towerConfigForDatabase("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent database, got nil (ActiveTower should not rescue)")
	}
}

func TestInstanceTowerRoundtrip(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SPIRE_CONFIG_DIR", tmpDir)

	cfg := &SpireConfig{
		Instances: map[string]*Instance{
			"web": {
				Path:     "/tmp/web",
				Prefix:   "web",
				Database: "beads_hub",
				Tower:    "my-tower",
			},
		},
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := loadConfig()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	inst := loaded.Instances["web"]
	if inst == nil {
		t.Fatal("instance 'web' not found after reload")
	}
	if inst.Tower != "my-tower" {
		t.Errorf("Tower = %q, want %q", inst.Tower, "my-tower")
	}
}

// TestAttachBootstrapThenRegisterRepoClient verifies the full path:
// tower attach materializes .beads/ (metadata.json + config.yaml),
// then repo add's client construction resolves against that workspace.
func TestAttachBootstrapThenRegisterRepoClient(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	// Simulate dolt data dir with a cloned database
	dataDir := filepath.Join(tmpDir, "dolt-data")
	dbName := "beads_acme"
	dbDir := filepath.Join(dataDir, dbName)
	os.MkdirAll(dbDir, 0755)
	t.Setenv("DOLT_DATA_DIR", dataDir)

	// Save a tower config (as tower attach would after reading identity)
	tower := &TowerConfig{
		Name:          "acme-team",
		ProjectID:     "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		HubPrefix:     "acm",
		DolthubRemote: "https://doltremoteapi.dolthub.com/org/beads_acme",
		Database:      dbName,
		CreatedAt:     "2026-03-22T10:00:00Z",
	}
	if err := saveTowerConfig(tower); err != nil {
		t.Fatalf("save tower config: %v", err)
	}

	// --- Simulate tower attach bootstrap (the code under test) ---
	beadsDir := filepath.Join(dbDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	beadsMeta := map[string]any{
		"project_id":    tower.ProjectID,
		"database":      "dolt",
		"backend":       "dolt",
		"dolt_mode":     "server",
		"dolt_database": dbName,
	}
	metaBytes, _ := json.MarshalIndent(beadsMeta, "", "  ")
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), append(metaBytes, '\n'), 0644); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}
	configYAML := "dolt.host: \"127.0.0.1\"\ndolt.port: 3307\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(configYAML), 0644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

	// --- Verify: repo add's tower resolution works ---
	tc, err := towerConfigForDatabase(dbName)
	if err != nil {
		t.Fatalf("towerConfigForDatabase: %v", err)
	}
	if tc.Name != "acme-team" {
		t.Errorf("tower Name = %q, want %q", tc.Name, "acme-team")
	}

	// --- Verify: repo add's BeadsDir resolves to existing files ---
	clientBeadsDir := filepath.Join(doltDataDir(), tc.Database, ".beads")

	// metadata.json must exist and contain project_id
	metaPath := filepath.Join(clientBeadsDir, "metadata.json")
	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read metadata.json at client BeadsDir: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(metaData, &meta); err != nil {
		t.Fatalf("parse metadata.json: %v", err)
	}
	if pid, _ := meta["project_id"].(string); pid != tower.ProjectID {
		t.Errorf("metadata project_id = %q, want %q", pid, tower.ProjectID)
	}
	if db, _ := meta["dolt_database"].(string); db != dbName {
		t.Errorf("metadata dolt_database = %q, want %q", db, dbName)
	}

	// config.yaml must exist and contain host/port
	configPath := filepath.Join(clientBeadsDir, "config.yaml")
	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config.yaml at client BeadsDir: %v", err)
	}
	configStr := string(configData)
	if !strings.Contains(configStr, "dolt.host") {
		t.Error("config.yaml missing dolt.host")
	}
	if !strings.Contains(configStr, "dolt.port") {
		t.Error("config.yaml missing dolt.port")
	}
}

// --- cmdTowerRemove tests ---

// setupTowerRemoveEnv creates an isolated environment with tower config(s) and global config.
// Returns the temp dir path.
func setupTowerRemoveEnv(t *testing.T, towers []*TowerConfig, cfg *SpireConfig) string {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SPIRE_CONFIG_DIR", filepath.Join(tmpDir, ".config", "spire"))
	t.Setenv("DOLT_DATA_DIR", filepath.Join(tmpDir, "dolt-data"))
	for _, tower := range towers {
		if err := saveTowerConfig(tower); err != nil {
			t.Fatalf("save tower %q: %v", tower.Name, err)
		}
	}
	if cfg != nil {
		if err := saveConfig(cfg); err != nil {
			t.Fatalf("save config: %v", err)
		}
	}
	return tmpDir
}

func TestCmdTowerRemove_NotFound(t *testing.T) {
	setupTowerRemoveEnv(t, nil, nil)

	err := cmdTowerRemove("nonexistent", true)
	if err == nil {
		t.Fatal("expected error for nonexistent tower")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCmdTowerRemove_LastTowerWithoutForce(t *testing.T) {
	tower := &TowerConfig{
		Name:      "only-tower",
		ProjectID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		HubPrefix: "onl",
		Database:  "beads_onl",
		CreatedAt: "2026-03-21T10:00:00Z",
	}
	setupTowerRemoveEnv(t, []*TowerConfig{tower}, nil)

	err := cmdTowerRemove("only-tower", false)
	if err == nil {
		t.Fatal("expected error when removing last tower without --force")
	}
	if !strings.Contains(err.Error(), "last tower") {
		t.Errorf("unexpected error: %v", err)
	}

	// Tower config should still exist.
	if _, loadErr := loadTowerConfig("only-tower"); loadErr != nil {
		t.Errorf("tower config should still exist: %v", loadErr)
	}
}

func TestCmdTowerRemove_NonInteractiveWithoutForce(t *testing.T) {
	t1 := &TowerConfig{
		Name: "alpha", ProjectID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		HubPrefix: "alp", Database: "beads_alp", CreatedAt: "2026-03-21T10:00:00Z",
	}
	t2 := &TowerConfig{
		Name: "beta", ProjectID: "11111111-2222-4333-8444-555555555555",
		HubPrefix: "bet", Database: "beads_bet", CreatedAt: "2026-03-21T11:00:00Z",
	}
	setupTowerRemoveEnv(t, []*TowerConfig{t1, t2}, nil)

	// In test environment, stdin is not a terminal, so this should refuse.
	err := cmdTowerRemove("alpha", false)
	if err == nil {
		t.Fatal("expected error when stdin is not a terminal and --force not set")
	}
	if !strings.Contains(err.Error(), "not a terminal") {
		t.Errorf("unexpected error: %v", err)
	}

	// Tower config should still exist.
	if _, loadErr := loadTowerConfig("alpha"); loadErr != nil {
		t.Errorf("tower config should still exist: %v", loadErr)
	}
}

func TestCmdTowerRemove_ForceLastTower(t *testing.T) {
	tower := &TowerConfig{
		Name:      "solo",
		ProjectID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		HubPrefix: "sol",
		Database:  "beads_sol",
		CreatedAt: "2026-03-21T10:00:00Z",
	}
	// First create tower config only, then set up config with paths.
	tmpDir := setupTowerRemoveEnv(t, []*TowerConfig{tower}, nil)

	// Create the repo directory and .beads/ dir so cleanup can find it.
	repoBeads := filepath.Join(tmpDir, "web", ".beads")
	os.MkdirAll(repoBeads, 0755)
	os.WriteFile(filepath.Join(repoBeads, "metadata.json"), []byte(`{}`), 0644)

	// Save config with the correct tmpDir-based path.
	if err := saveConfig(&SpireConfig{
		ActiveTower: "solo",
		Instances: map[string]*Instance{
			"web": {
				Path:     filepath.Join(tmpDir, "web"),
				Prefix:   "web",
				Database: "beads_sol",
				Tower:    "solo",
			},
		},
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}

	err := cmdTowerRemove("solo", true)
	if err != nil {
		t.Fatalf("expected success with --force: %v", err)
	}

	// Tower config should be gone.
	if _, loadErr := loadTowerConfig("solo"); loadErr == nil {
		t.Error("tower config should have been removed")
	}

	// Active tower should be cleared.
	cfg, loadErr := loadConfig()
	if loadErr != nil {
		t.Fatalf("load config: %v", loadErr)
	}
	if cfg.ActiveTower != "" {
		t.Errorf("active tower should be empty, got %q", cfg.ActiveTower)
	}

	// Instance should be removed.
	if _, ok := cfg.Instances["web"]; ok {
		t.Error("instance 'web' should have been removed")
	}

	// .beads/ directory should be removed.
	if _, statErr := os.Stat(repoBeads); statErr == nil {
		t.Error(".beads/ directory should have been removed")
	}
}

func TestCmdTowerRemove_ForceMultipleTowers(t *testing.T) {
	t1 := &TowerConfig{
		Name: "alpha", ProjectID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		HubPrefix: "alp", Database: "beads_alp", CreatedAt: "2026-03-21T10:00:00Z",
	}
	t2 := &TowerConfig{
		Name: "beta", ProjectID: "11111111-2222-4333-8444-555555555555",
		HubPrefix: "bet", Database: "beads_bet", CreatedAt: "2026-03-21T11:00:00Z",
	}
	setupTowerRemoveEnv(t, []*TowerConfig{t1, t2}, &SpireConfig{
		ActiveTower: "beta",
		Instances: map[string]*Instance{
			"web": {Path: "/tmp/web", Prefix: "web", Database: "beads_alp", Tower: "alpha"},
			"api": {Path: "/tmp/api", Prefix: "api", Database: "beads_bet", Tower: "beta"},
		},
	})

	// Remove alpha — should only remove alpha's instances, leave beta's.
	err := cmdTowerRemove("alpha", true)
	if err != nil {
		t.Fatalf("remove alpha: %v", err)
	}

	// Alpha tower config gone.
	if _, loadErr := loadTowerConfig("alpha"); loadErr == nil {
		t.Error("alpha tower config should be removed")
	}

	// Beta still exists.
	if _, loadErr := loadTowerConfig("beta"); loadErr != nil {
		t.Errorf("beta tower config should still exist: %v", loadErr)
	}

	// Global config: only beta's instance should remain.
	cfg, loadErr := loadConfig()
	if loadErr != nil {
		t.Fatalf("load config: %v", loadErr)
	}
	if _, ok := cfg.Instances["web"]; ok {
		t.Error("instance 'web' (alpha) should have been removed")
	}
	if _, ok := cfg.Instances["api"]; !ok {
		t.Error("instance 'api' (beta) should still exist")
	}
	// Active tower should NOT be cleared (was beta, not alpha).
	if cfg.ActiveTower != "beta" {
		t.Errorf("active tower should still be %q, got %q", "beta", cfg.ActiveTower)
	}
}

func TestCmdTowerRemove_InvalidDatabaseName(t *testing.T) {
	tower := &TowerConfig{
		Name:      "bad-db",
		ProjectID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		HubPrefix: "bad",
		Database:  "beads`; DROP TABLE users; --",
		CreatedAt: "2026-03-21T10:00:00Z",
	}
	t2 := &TowerConfig{
		Name: "other", ProjectID: "11111111-2222-4333-8444-555555555555",
		HubPrefix: "oth", Database: "beads_oth", CreatedAt: "2026-03-21T11:00:00Z",
	}
	setupTowerRemoveEnv(t, []*TowerConfig{tower, t2}, nil)

	err := cmdTowerRemove("bad-db", true)
	if err == nil {
		t.Fatal("expected error for invalid database name")
	}
	if !strings.Contains(err.Error(), "invalid database name") {
		t.Errorf("unexpected error: %v", err)
	}
}

// Regression test for spi-yplcm3: gateway-mode towers have an empty
// Database field, but cleanup must still complete (remove tower config,
// instance entries, active-tower pointer) instead of bailing out at the
// isValidDatabaseName guard.
func TestCmdTowerRemove_GatewayTowerEmptyDatabase(t *testing.T) {
	gw := &TowerConfig{
		Name:      "gw",
		ProjectID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		HubPrefix: "gwh",
		Database:  "",
		Mode:      config.TowerModeGateway,
		URL:       "https://spire.example.com",
		TokenRef:  "gw",
		CreatedAt: "2026-04-26T10:00:00Z",
	}
	other := &TowerConfig{
		Name:      "other",
		ProjectID: "11111111-2222-4333-8444-555555555555",
		HubPrefix: "oth",
		Database:  "beads_oth",
		CreatedAt: "2026-04-26T11:00:00Z",
	}
	setupTowerRemoveEnv(t, []*TowerConfig{gw, other}, &SpireConfig{
		ActiveTower: "gw",
		Instances: map[string]*Instance{
			"web":   {Path: "/tmp/web", Prefix: "web", Database: "", Tower: "gw"},
			"other": {Path: "/tmp/other", Prefix: "oth", Database: "beads_oth", Tower: "other"},
		},
	})

	if err := cmdTowerRemove("gw", true); err != nil {
		t.Fatalf("remove gateway tower: %v", err)
	}

	// Tower config file should be gone.
	if _, loadErr := loadTowerConfig("gw"); loadErr == nil {
		t.Error("gateway tower config should have been deleted")
	}

	// Instance entry tied to gw should be gone; the unrelated one stays.
	cfg, loadErr := loadConfig()
	if loadErr != nil {
		t.Fatalf("load config: %v", loadErr)
	}
	if _, ok := cfg.Instances["web"]; ok {
		t.Error("instance 'web' (tower=gw) should have been removed")
	}
	if _, ok := cfg.Instances["other"]; !ok {
		t.Error("instance 'other' (tower=other) should still exist")
	}

	// Active-tower pointer should be cleared.
	if cfg.ActiveTower != "" {
		t.Errorf("active tower should be empty, got %q", cfg.ActiveTower)
	}
}

// Regression test for spi-yplcm3: a tower with an empty Database field
// (regardless of Mode) should also skip the drop step rather than tripping
// the validator. This covers the second signal the fix recognizes.
func TestCmdTowerRemove_EmptyDatabaseSkipsDrop(t *testing.T) {
	tower := &TowerConfig{
		Name:      "no-db",
		ProjectID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		HubPrefix: "ndb",
		Database:  "",
		CreatedAt: "2026-04-26T10:00:00Z",
	}
	other := &TowerConfig{
		Name:      "other",
		ProjectID: "11111111-2222-4333-8444-555555555555",
		HubPrefix: "oth",
		Database:  "beads_oth",
		CreatedAt: "2026-04-26T11:00:00Z",
	}
	setupTowerRemoveEnv(t, []*TowerConfig{tower, other}, nil)

	if err := cmdTowerRemove("no-db", true); err != nil {
		t.Fatalf("remove tower with empty database: %v", err)
	}

	if _, loadErr := loadTowerConfig("no-db"); loadErr == nil {
		t.Error("tower config should have been deleted")
	}
}

func TestIsValidDatabaseName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"beads_hub", true},
		{"beads-test", true},
		{"BeadsDB123", true},
		{"", false},
		{"beads`injection", false},
		{"db; DROP TABLE", false},
		{"name with spaces", false},
		{"name\ttab", false},
	}
	for _, tc := range tests {
		got := isValidDatabaseName(tc.name)
		if got != tc.want {
			t.Errorf("isValidDatabaseName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// --- mergeCustomStatuses tests ---

func TestMergeCustomStatuses_EmptyToRequired(t *testing.T) {
	merged, changed := mergeCustomStatuses("", requiredCustomStatuses)
	if !changed {
		t.Fatal("expected changed=true when adding to empty config")
	}
	if merged != "ready:active" {
		t.Errorf("merged = %q, want %q", merged, "ready:active")
	}
}

func TestMergeCustomStatuses_Idempotent(t *testing.T) {
	// Already has the required status — should not change.
	merged, changed := mergeCustomStatuses("ready:active", requiredCustomStatuses)
	if changed {
		t.Fatal("expected changed=false when status already present")
	}
	if merged != "ready:active" {
		t.Errorf("merged = %q, want %q", merged, "ready:active")
	}
}

func TestMergeCustomStatuses_PreservesUserStatuses(t *testing.T) {
	// User has a custom status — it should be preserved after merge.
	merged, changed := mergeCustomStatuses("custom:done", requiredCustomStatuses)
	if !changed {
		t.Fatal("expected changed=true when adding missing required status")
	}
	// Should contain both user status and required status, sorted.
	if merged != "custom:done,ready:active" {
		t.Errorf("merged = %q, want %q", merged, "custom:done,ready:active")
	}
}

func TestMergeCustomStatuses_PreservesExistingWithRequired(t *testing.T) {
	// User has both a custom status AND the required one — no change.
	merged, changed := mergeCustomStatuses("custom:done,ready:active", requiredCustomStatuses)
	if changed {
		t.Fatal("expected changed=false when all required statuses present")
	}
	if merged != "custom:done,ready:active" {
		t.Errorf("merged = %q, want %q", merged, "custom:done,ready:active")
	}
}

func TestMergeCustomStatuses_HandlesWhitespace(t *testing.T) {
	// Whitespace in existing config should be trimmed.
	merged, changed := mergeCustomStatuses(" custom:done , ready:active ", requiredCustomStatuses)
	if changed {
		t.Fatal("expected changed=false (trimmed values match)")
	}
	if merged != " custom:done , ready:active " {
		t.Errorf("merged = %q, want original (unchanged)", merged)
	}
}

func TestMergeCustomStatuses_MultipleRequired(t *testing.T) {
	// Test with multiple required statuses.
	required := []string{"ready:active", "paused:active"}
	merged, changed := mergeCustomStatuses("custom:done", required)
	if !changed {
		t.Fatal("expected changed=true")
	}
	if merged != "custom:done,paused:active,ready:active" {
		t.Errorf("merged = %q, want %q", merged, "custom:done,paused:active,ready:active")
	}
}

func TestInstanceTowerOmitEmpty(t *testing.T) {
	// Instance without Tower should omit the field in JSON (backward compat)
	cfg := &SpireConfig{
		Instances: map[string]*Instance{
			"old": {
				Path:     "/tmp/old",
				Prefix:   "old",
				Database: "beads_hub",
			},
		},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), `"tower"`) {
		t.Error("expected tower to be omitted when empty")
	}
}

func TestIsDoltAuthError(t *testing.T) {
	// Real stderr from the bug report — gRPC PermissionDenied plus the
	// "could not access dolt url" preamble.
	realBugReport := `cloning https://doltremoteapi.dolthub.com/awell/awell
error: failed to get remote db
cause: could not access dolt url 'https://doltremoteapi.dolthub.com/awell/awell': rpc error: code = PermissionDenied desc = permission denied`

	cases := []struct {
		name   string
		stderr string
		want   bool
	}{
		{
			name:   "real bug report stderr",
			stderr: realBugReport,
			want:   true,
		},
		{
			name:   "PermissionDenied token alone (gRPC shape)",
			stderr: "rpc error: code = PermissionDenied desc = permission denied",
			want:   true,
		},
		{
			name:   "mixed case PermissionDenied",
			stderr: "some prefix PERMISSIONDENIED suffix",
			want:   true,
		},
		{
			name:   "repository not found (404-like)",
			stderr: "error: repository not found",
			want:   false,
		},
		{
			name:   "network timeout",
			stderr: "dial tcp: i/o timeout",
			want:   false,
		},
		{
			name:   "empty stderr",
			stderr: "",
			want:   false,
		},
		{
			name:   "unrelated filesystem permission denied (no dolt url preamble)",
			stderr: "open /home/user/.dolt/creds/foo.jwk: permission denied",
			want:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isDoltAuthError(tc.stderr)
			if got != tc.want {
				t.Errorf("isDoltAuthError(%q) = %v, want %v", tc.stderr, got, tc.want)
			}
		})
	}
}
