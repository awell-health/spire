package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	// Verify the SQL is non-empty and contains expected keywords
	if reposTableSQL == "" {
		t.Fatal("reposTableSQL is empty")
	}
	if !strings.Contains(reposTableSQL, "CREATE TABLE") {
		t.Error("reposTableSQL missing CREATE TABLE")
	}
	if !strings.Contains(reposTableSQL, "repos") {
		t.Error("reposTableSQL missing table name 'repos'")
	}
	if !strings.Contains(reposTableSQL, "prefix") {
		t.Error("reposTableSQL missing 'prefix' column")
	}
	if !strings.Contains(reposTableSQL, "repo_url") {
		t.Error("reposTableSQL missing 'repo_url' column")
	}
	if !strings.Contains(reposTableSQL, "branch") {
		t.Error("reposTableSQL missing 'branch' column")
	}
	if !strings.Contains(reposTableSQL, "PRIMARY KEY") {
		t.Error("reposTableSQL missing PRIMARY KEY")
	}
	if !strings.Contains(reposTableSQL, "IF NOT EXISTS") {
		t.Error("reposTableSQL missing IF NOT EXISTS")
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

	cfg := &SpireConfig{
		Instances: map[string]*Instance{
			"web": {
				Path:     "/tmp/web",
				Prefix:   "web",
				Role:     "standalone",
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

func TestInstanceTowerOmitEmpty(t *testing.T) {
	// Instance without Tower should omit the field in JSON (backward compat)
	cfg := &SpireConfig{
		Instances: map[string]*Instance{
			"old": {
				Path:     "/tmp/old",
				Prefix:   "old",
				Role:     "standalone",
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
