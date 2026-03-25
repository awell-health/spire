package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigDir_Default(t *testing.T) {
	// Clear any override so we get the default HOME-based path.
	t.Setenv("SPIRE_CONFIG_DIR", "")

	dir, err := configDir()
	if err != nil {
		t.Fatalf("configDir() error: %v", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir() error: %v", err)
	}

	want := filepath.Join(home, ".config", "spire")
	if dir != want {
		t.Errorf("configDir() = %q, want %q", dir, want)
	}
}

func TestConfigDir_EnvOverride(t *testing.T) {
	tmp := t.TempDir()
	override := filepath.Join(tmp, "custom-config")

	t.Setenv("SPIRE_CONFIG_DIR", override)

	dir, err := configDir()
	if err != nil {
		t.Fatalf("configDir() error: %v", err)
	}

	if dir != override {
		t.Errorf("configDir() = %q, want %q", dir, override)
	}

	// Verify the directory was created.
	info, err := os.Stat(override)
	if err != nil {
		t.Fatalf("Stat(%q) error: %v", override, err)
	}
	if !info.IsDir() {
		t.Errorf("%q is not a directory", override)
	}
}

func TestWizardRegistryPath_UsesConfigDir(t *testing.T) {
	tmp := t.TempDir()
	override := filepath.Join(tmp, "spire-cfg")

	t.Setenv("SPIRE_CONFIG_DIR", override)

	got := wizardRegistryPath()
	want := filepath.Join(override, "wizards.json")

	if got != want {
		t.Errorf("wizardRegistryPath() = %q, want %q", got, want)
	}
}
