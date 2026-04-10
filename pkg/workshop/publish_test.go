package workshop

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPublish(t *testing.T) {
	tempDir := t.TempDir()

	dest, err := Publish("task-default", tempDir)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	expected := filepath.Join(tempDir, "formulas", "task-default.formula.toml")
	if dest != expected {
		t.Errorf("dest: got %q, want %q", dest, expected)
	}

	// Verify file exists
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("published file not found: %v", err)
	}

	// Verify content is valid TOML by reading it
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read published file: %v", err)
	}
	if len(data) == 0 {
		t.Error("published file is empty")
	}
}

func TestPublishCreatesFormulasDir(t *testing.T) {
	tempDir := t.TempDir()

	// Ensure formulas/ subdirectory does NOT exist yet
	formulasDir := filepath.Join(tempDir, "formulas")
	if _, err := os.Stat(formulasDir); err == nil {
		t.Fatal("formulas/ should not exist before publish")
	}

	_, err := Publish("task-default", tempDir)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Verify formulas/ was created
	info, err := os.Stat(formulasDir)
	if err != nil {
		t.Fatalf("formulas dir not created: %v", err)
	}
	if !info.IsDir() {
		t.Error("formulas is not a directory")
	}
}

func TestPublishEmptyBeadsDir(t *testing.T) {
	_, err := Publish("task-default", "")
	if err == nil {
		t.Fatal("expected error for empty beadsDir")
	}
}

func TestPublishNonexistentFormula(t *testing.T) {
	tempDir := t.TempDir()
	_, err := Publish("does-not-exist", tempDir)
	if err == nil {
		t.Fatal("expected error for nonexistent formula")
	}
}

func TestUnpublish(t *testing.T) {
	tempDir := t.TempDir()

	// Publish first
	dest, err := Publish("task-default", tempDir)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Verify it exists
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("file should exist after publish: %v", err)
	}

	// Unpublish
	err = Unpublish("task-default", tempDir)
	if err != nil {
		t.Fatalf("Unpublish: %v", err)
	}

	// Verify removed
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("file should not exist after unpublish")
	}
}

func TestUnpublishNonexistent(t *testing.T) {
	tempDir := t.TempDir()

	err := Unpublish("does-not-exist", tempDir)
	if err == nil {
		t.Fatal("expected error for unpublishing nonexistent formula")
	}
}

func TestUnpublishEmptyBeadsDir(t *testing.T) {
	err := Unpublish("task-default", "")
	if err == nil {
		t.Fatal("expected error for empty beadsDir")
	}
}

func TestIsPublished(t *testing.T) {
	tempDir := t.TempDir()

	// Before publish
	if IsPublished("task-default", tempDir) {
		t.Error("should not be published before Publish()")
	}

	// After publish
	_, err := Publish("task-default", tempDir)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !IsPublished("task-default", tempDir) {
		t.Error("should be published after Publish()")
	}

	// After unpublish
	err = Unpublish("task-default", tempDir)
	if err != nil {
		t.Fatalf("Unpublish: %v", err)
	}
	if IsPublished("task-default", tempDir) {
		t.Error("should not be published after Unpublish()")
	}
}

func TestPublishedPath(t *testing.T) {
	got := PublishedPath("my-formula", "/tmp/beads")
	want := filepath.Join("/tmp/beads", "formulas", "my-formula.formula.toml")
	if got != want {
		t.Errorf("PublishedPath: got %q, want %q", got, want)
	}
}
