package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseDoltRows(t *testing.T) {
	columns := []string{"prefix", "repo_url", "branch"}

	tests := []struct {
		name     string
		input    string
		wantLen  int
		wantRow0 map[string]string // nil means no rows expected
	}{
		{
			name:    "empty input",
			input:   "",
			wantLen: 0,
		},
		{
			name:    "header only, no data",
			input:   "+--------+----------+--------+\n| prefix | repo_url | branch |\n+--------+----------+--------+\n+--------+----------+--------+",
			wantLen: 0,
		},
		{
			name:    "one data row with separators",
			input:   "+--------+------------------+--------+\n| prefix | repo_url         | branch |\n+--------+------------------+--------+\n| spi    | https://gh/repo  | main   |\n+--------+------------------+--------+",
			wantLen: 1,
			wantRow0: map[string]string{
				"prefix":   "spi",
				"repo_url": "https://gh/repo",
				"branch":   "main",
			},
		},
		{
			name:    "multiple data rows",
			input:   "+-----+------+------+\n| prefix | repo_url | branch |\n+-----+------+------+\n| spi | url1 | main |\n| web | url2 | dev  |\n+-----+------+------+",
			wantLen: 2,
			wantRow0: map[string]string{
				"prefix":   "spi",
				"repo_url": "url1",
				"branch":   "main",
			},
		},
		{
			name:    "empty cell in middle column",
			input:   "+-----+------+------+\n| prefix | repo_url | branch |\n+-----+------+------+\n| spi |  | main |\n+-----+------+------+",
			wantLen: 1,
			wantRow0: map[string]string{
				"prefix":   "spi",
				"repo_url": "",
				"branch":   "main",
			},
		},
		{
			name:    "separator lines not treated as data",
			input:   "+---+---+---+\n| a | b | c |\n+---+---+---+\n| 1 | 2 | 3 |\n+---+---+---+",
			wantLen: 1,
			wantRow0: map[string]string{
				"prefix":   "1",
				"repo_url": "2",
				"branch":   "3",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows := parseDoltRows(tt.input, columns)
			if len(rows) != tt.wantLen {
				t.Fatalf("got %d rows, want %d", len(rows), tt.wantLen)
			}
			if tt.wantRow0 != nil && len(rows) > 0 {
				for k, want := range tt.wantRow0 {
					if got := rows[0][k]; got != want {
						t.Errorf("row[0][%q] = %q, want %q", k, got, want)
					}
				}
			}
		})
	}
}

// writeTowerConfig is a test helper that writes a tower config JSON file.
func writeTowerConfig(t *testing.T, dir, name, database string) {
	t.Helper()
	towersDir := filepath.Join(dir, ".config", "spire", "towers")
	if err := os.MkdirAll(towersDir, 0755); err != nil {
		t.Fatal(err)
	}
	tc := TowerConfig{Name: name, Database: database}
	data, _ := json.Marshal(tc)
	if err := os.WriteFile(filepath.Join(towersDir, name+".json"), data, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveDatabase_NoTowers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SPIRE_TOWER", "")
	cfg := &SpireConfig{Instances: map[string]*Instance{}}

	db, ambiguous := resolveDatabase(cfg)
	if ambiguous {
		t.Error("expected not ambiguous with no towers")
	}
	if db != "" {
		t.Errorf("got db=%q, want empty", db)
	}
}

func TestResolveDatabase_SingleTower(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SPIRE_TOWER", "")
	writeTowerConfig(t, home, "solo", "beads_solo")

	cfg := &SpireConfig{Instances: map[string]*Instance{}}
	db, ambiguous := resolveDatabase(cfg)
	if ambiguous {
		t.Error("expected not ambiguous with single tower")
	}
	if db != "beads_solo" {
		t.Errorf("got db=%q, want %q", db, "beads_solo")
	}
}

func TestResolveDatabase_MultipleTowersNoActive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SPIRE_TOWER", "")
	writeTowerConfig(t, home, "alpha", "beads_alpha")
	writeTowerConfig(t, home, "beta", "beads_beta")

	cfg := &SpireConfig{Instances: map[string]*Instance{}}
	db, ambiguous := resolveDatabase(cfg)
	if !ambiguous {
		t.Error("expected ambiguous with multiple towers and no active")
	}
	if db != "" {
		t.Errorf("got db=%q, want empty", db)
	}
}

func TestResolveDatabase_MultipleTowersWithActive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SPIRE_TOWER", "")
	writeTowerConfig(t, home, "alpha", "beads_alpha")
	writeTowerConfig(t, home, "beta", "beads_beta")

	cfg := &SpireConfig{
		Instances:   map[string]*Instance{},
		ActiveTower: "beta",
	}
	db, ambiguous := resolveDatabase(cfg)
	if ambiguous {
		t.Error("expected not ambiguous when ActiveTower is set")
	}
	if db != "beads_beta" {
		t.Errorf("got db=%q, want %q", db, "beads_beta")
	}
}

func TestTowerUse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SPIRE_CONFIG_DIR", filepath.Join(home, ".config", "spire"))
	t.Setenv("SPIRE_TOWER", "")
	writeTowerConfig(t, home, "alpha", "beads_alpha")
	writeTowerConfig(t, home, "beta", "beads_beta")

	// Save initial config with no active tower
	cfg := &SpireConfig{Instances: map[string]*Instance{}}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	// Use a valid tower
	if err := cmdTowerUse("alpha"); err != nil {
		t.Fatalf("tower use alpha: %v", err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ActiveTower != "alpha" {
		t.Errorf("ActiveTower=%q, want %q", cfg.ActiveTower, "alpha")
	}

	// Switch to another
	if err := cmdTowerUse("beta"); err != nil {
		t.Fatalf("tower use beta: %v", err)
	}
	cfg, err = loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ActiveTower != "beta" {
		t.Errorf("ActiveTower=%q, want %q", cfg.ActiveTower, "beta")
	}

	// Use a nonexistent tower — should fail
	if err := cmdTowerUse("nonexistent"); err == nil {
		t.Error("expected error for nonexistent tower, got nil")
	}
}

func TestResolveRemoveDatabase_PrefersTowerConfig(t *testing.T) {
	// When inst.Tower points to a valid tower config, use its database
	// even if inst.Database is different (stale cache).
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SPIRE_TOWER", "")
	writeTowerConfig(t, home, "current", "beads_current")

	cfg := &SpireConfig{
		Instances: map[string]*Instance{
			"web": {
				Prefix:   "web",
				Tower:    "current",
				Database: "beads_stale", // stale cached value
			},
		},
	}
	db, err := resolveRemoveDatabase(cfg, "web")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if db != "beads_current" {
		t.Errorf("got db=%q, want %q (tower config should win over cached database)", db, "beads_current")
	}
}

func TestResolveRemoveDatabase_FallsToCachedDatabase(t *testing.T) {
	// When inst.Tower is empty, fall back to inst.Database.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SPIRE_TOWER", "")

	cfg := &SpireConfig{
		Instances: map[string]*Instance{
			"web": {
				Prefix:   "web",
				Database: "beads_cached",
			},
		},
	}
	db, err := resolveRemoveDatabase(cfg, "web")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if db != "beads_cached" {
		t.Errorf("got db=%q, want %q", db, "beads_cached")
	}
}

func TestResolveRemoveDatabase_FallsToGlobalResolution(t *testing.T) {
	// Unknown prefix (not in Instances) falls back to global resolution.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SPIRE_TOWER", "")
	writeTowerConfig(t, home, "solo", "beads_solo")

	cfg := &SpireConfig{Instances: map[string]*Instance{}}
	db, err := resolveRemoveDatabase(cfg, "unknown")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if db != "beads_solo" {
		t.Errorf("got db=%q, want %q", db, "beads_solo")
	}
}

func TestResolveRemoveDatabase_AmbiguousError(t *testing.T) {
	// Multiple towers, no active, unknown prefix → ambiguous error.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SPIRE_TOWER", "")
	writeTowerConfig(t, home, "alpha", "beads_alpha")
	writeTowerConfig(t, home, "beta", "beads_beta")

	cfg := &SpireConfig{Instances: map[string]*Instance{}}
	_, err := resolveRemoveDatabase(cfg, "unknown")
	if err == nil {
		t.Fatal("expected ambiguity error, got nil")
	}
}

func TestResolveRemoveDatabase_NoTowerNoInstance(t *testing.T) {
	// No towers, no matching instance → unresolvable error.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SPIRE_TOWER", "")

	cfg := &SpireConfig{Instances: map[string]*Instance{}}
	_, err := resolveRemoveDatabase(cfg, "unknown")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}
