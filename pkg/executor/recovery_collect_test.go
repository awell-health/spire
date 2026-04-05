package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadWizardLogTailFrom_EmptyName(t *testing.T) {
	got := readWizardLogTailFrom("/tmp/fake", "")
	if got != "" {
		t.Errorf("empty name: got %q, want empty", got)
	}
}

func TestReadWizardLogTailFrom_MissingFile(t *testing.T) {
	dir := t.TempDir()
	got := readWizardLogTailFrom(dir, "nonexistent-wizard")
	if got != "" {
		t.Errorf("missing file: got %q, want empty", got)
	}
}

func TestReadWizardLogTailFrom_SmallFile(t *testing.T) {
	dir := t.TempDir()
	content := "line1\nline2\nline3"
	if err := os.WriteFile(filepath.Join(dir, "mywizard.log"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	got := readWizardLogTailFrom(dir, "mywizard")
	if got != content {
		t.Errorf("small file: got %q, want %q", got, content)
	}
}

func TestReadWizardLogTailFrom_ExactlyMaxLines(t *testing.T) {
	dir := t.TempDir()
	lines := make([]string, 100)
	for i := range lines {
		lines[i] = "line"
	}
	content := strings.Join(lines, "\n")
	if err := os.WriteFile(filepath.Join(dir, "mywizard.log"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	got := readWizardLogTailFrom(dir, "mywizard")
	if got != content {
		t.Errorf("exact 100 lines: got %d lines, want 100", len(strings.Split(got, "\n")))
	}
}

func TestReadWizardLogTailFrom_MoreThanMaxLines(t *testing.T) {
	dir := t.TempDir()
	lines := make([]string, 150)
	for i := range lines {
		lines[i] = "line" + string(rune('A'+i%26))
	}
	content := strings.Join(lines, "\n")
	if err := os.WriteFile(filepath.Join(dir, "mywizard.log"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	got := readWizardLogTailFrom(dir, "mywizard")
	gotLines := strings.Split(got, "\n")
	if len(gotLines) != 100 {
		t.Errorf("expected 100 lines, got %d", len(gotLines))
	}
	// Should be the LAST 100 lines (lines[50:150]).
	if gotLines[0] != lines[50] {
		t.Errorf("first returned line: got %q, want %q", gotLines[0], lines[50])
	}
	if gotLines[99] != lines[149] {
		t.Errorf("last returned line: got %q, want %q", gotLines[99], lines[149])
	}
}

func TestReadWizardLogTailFrom_WizardPrefixFallback(t *testing.T) {
	dir := t.TempDir()
	content := "wizard-prefix-log-output"
	// Only the wizard-<name>.log file exists, not <name>.log.
	if err := os.WriteFile(filepath.Join(dir, "wizard-mywiz.log"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	got := readWizardLogTailFrom(dir, "mywiz")
	if got != content {
		t.Errorf("wizard prefix fallback: got %q, want %q", got, content)
	}
}

func TestReadWizardLogTailFrom_PrefersDirectName(t *testing.T) {
	dir := t.TempDir()
	// Both files exist — <name>.log should be preferred.
	if err := os.WriteFile(filepath.Join(dir, "mywiz.log"), []byte("direct"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wizard-mywiz.log"), []byte("prefixed"), 0644); err != nil {
		t.Fatal(err)
	}
	got := readWizardLogTailFrom(dir, "mywiz")
	if got != "direct" {
		t.Errorf("prefer direct name: got %q, want %q", got, "direct")
	}
}
