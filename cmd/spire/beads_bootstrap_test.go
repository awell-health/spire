package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withStubbedBootstrapTypes(t *testing.T, fn func(string) error) {
	t.Helper()
	prev := ensureBootstrapCustomTypesFn
	ensureBootstrapCustomTypesFn = fn
	t.Cleanup(func() {
		ensureBootstrapCustomTypesFn = prev
	})
}

func TestBootstrapTowerBeadsDir_WritesWorkspaceAndEnsuresTypes(t *testing.T) {
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	tower := &TowerConfig{
		Name:      "acme",
		ProjectID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		Database:  "beads_acm",
	}

	var ensured string
	withStubbedBootstrapTypes(t, func(dir string) error {
		ensured = dir
		return nil
	})

	if err := bootstrapTowerBeadsDir(beadsDir, tower); err != nil {
		t.Fatalf("bootstrapTowerBeadsDir: %v", err)
	}

	if ensured != beadsDir {
		t.Fatalf("custom type registration ran for %q, want %q", ensured, beadsDir)
	}

	metaData, err := os.ReadFile(filepath.Join(beadsDir, "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata.json: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(metaData, &meta); err != nil {
		t.Fatalf("parse metadata.json: %v", err)
	}
	if got := meta["project_id"]; got != tower.ProjectID {
		t.Fatalf("metadata project_id = %v, want %q", got, tower.ProjectID)
	}
	if got := meta["dolt_database"]; got != tower.Database {
		t.Fatalf("metadata dolt_database = %v, want %q", got, tower.Database)
	}

	configData, err := os.ReadFile(filepath.Join(beadsDir, "config.yaml"))
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	config := string(configData)
	if !strings.Contains(config, "dolt.host") {
		t.Fatal("config.yaml missing dolt.host")
	}
	if !strings.Contains(config, "dolt.port") {
		t.Fatal("config.yaml missing dolt.port")
	}
}

func TestBootstrapRepoBeadsDir_WritesRepoFilesAndEnsuresTypes(t *testing.T) {
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	tower := &TowerConfig{
		Name:      "acme",
		ProjectID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		Database:  "beads_acm",
	}

	calls := 0
	withStubbedBootstrapTypes(t, func(dir string) error {
		calls++
		if dir != beadsDir {
			t.Fatalf("custom type registration ran for %q, want %q", dir, beadsDir)
		}
		return nil
	})

	if err := bootstrapRepoBeadsDir(beadsDir, tower, "web"); err != nil {
		t.Fatalf("bootstrapRepoBeadsDir: %v", err)
	}

	if calls != 1 {
		t.Fatalf("custom type registration called %d time(s), want 1", calls)
	}

	routesData, err := os.ReadFile(filepath.Join(beadsDir, "routes.jsonl"))
	if err != nil {
		t.Fatalf("read routes.jsonl: %v", err)
	}
	if string(routesData) != "{\"prefix\":\"web-\",\"path\":\".\"}\n" {
		t.Fatalf("routes.jsonl = %q", string(routesData))
	}

	gitignoreData, err := os.ReadFile(filepath.Join(beadsDir, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	gitignore := string(gitignoreData)
	for _, entry := range []string{"metadata.json", "config.yaml", "routes.jsonl"} {
		if !strings.Contains(gitignore, entry) {
			t.Fatalf(".gitignore missing %q", entry)
		}
	}
}
