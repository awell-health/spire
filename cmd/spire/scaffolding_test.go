package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallSpireSkills_InstallsBundledSkillEverywhere(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, "codex-home")
	repo := filepath.Join(home, "repo")
	claudeDir := filepath.Join(repo, ".claude")

	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codexHome)

	if err := os.MkdirAll(filepath.Join(home, ".claude", "skills", "spire-work"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "skills", "spire-work", "SKILL.md"), []byte("legacy"), 0644); err != nil {
		t.Fatal(err)
	}

	installSpireSkills(claudeDir)

	assertFileContains(t, filepath.Join(home, ".claude", "skills", "spire-conflicts", "SKILL.md"), "dolt sql")
	assertFileContains(t, filepath.Join(claudeDir, "skills", "spire-conflicts", "SKILL.md"), "dolt sql")
	assertFileContains(t, filepath.Join(claudeDir, "skills", "spire-conflicts", "agents", "openai.yaml"), "display_name: \"Spire Conflicts\"")
	assertFileContains(t, filepath.Join(codexHome, "skills", "spire-conflicts", "SKILL.md"), "dolt sql")

	assertFileContains(t, filepath.Join(home, ".claude", "skills", "spire-design", "SKILL.md"), "design bead")
	assertFileContains(t, filepath.Join(claudeDir, "skills", "spire-design", "SKILL.md"), "design bead")
	assertFileContains(t, filepath.Join(codexHome, "skills", "spire-design", "SKILL.md"), "design bead")
}

func TestCheckSpireSkills_FixInstallsBundledConflictSkill(t *testing.T) {
	home := t.TempDir()
	repo := filepath.Join(home, "repo")
	claudeSkills := filepath.Join(repo, ".claude", "skills")

	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex-home"))

	if err := os.MkdirAll(filepath.Join(claudeSkills, "spire-work"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeSkills, "spire-work", "SKILL.md"), []byte("legacy"), 0644); err != nil {
		t.Fatal(err)
	}

	r := checkSpireSkills(repo)
	if r.Status != statusMissing {
		t.Fatalf("expected missing status, got %s (%s)", r.Status, r.Detail)
	}
	if !strings.Contains(r.Detail, "spire-conflicts") {
		t.Fatalf("expected detail to mention spire-conflicts, got %q", r.Detail)
	}
	if r.FixFunc == nil {
		t.Fatal("expected fix function")
	}

	r.FixFunc()

	assertFileContains(t, filepath.Join(claudeSkills, "spire-conflicts", "SKILL.md"), "dolt sql")
	assertFileContains(t, filepath.Join(claudeSkills, "spire-design", "SKILL.md"), "design bead")
}

func assertFileContains(t *testing.T, path, needle string) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(data), needle) {
		t.Fatalf("%s does not contain %q", path, needle)
	}
}
