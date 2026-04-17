package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeClaudeLog creates <doltGlobal>/wizards/<wizard>/claude/<name>
// with a trivial body. Returns the file path.
func writeClaudeLog(t *testing.T, doltGlobal, wizard, name string) string {
	t.Helper()
	dir := filepath.Join(doltGlobal, "wizards", wizard, "claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestListClaudeLogs_MissingDirPrintsFriendlyMessage(t *testing.T) {
	tmp := t.TempDir()
	var buf bytes.Buffer
	if err := listClaudeLogs(&buf, tmp, "wizard-spi-missing"); err != nil {
		t.Fatalf("listClaudeLogs returned error: %v", err)
	}
	got := buf.String()
	want := "No claude invocations recorded for wizard-spi-missing"
	if !strings.Contains(got, want) {
		t.Fatalf("listClaudeLogs output = %q, want substring %q", got, want)
	}
}

func TestListClaudeLogs_EmptyDirPrintsFriendlyMessage(t *testing.T) {
	tmp := t.TempDir()
	// Create the directory but no *.log files.
	if err := os.MkdirAll(filepath.Join(tmp, "wizards", "wizard-spi-empty", "claude"), 0o755); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	var buf bytes.Buffer
	if err := listClaudeLogs(&buf, tmp, "wizard-spi-empty"); err != nil {
		t.Fatalf("listClaudeLogs returned error: %v", err)
	}
	if !strings.Contains(buf.String(), "No claude invocations recorded for wizard-spi-empty") {
		t.Fatalf("expected friendly message, got: %q", buf.String())
	}
}

func TestListClaudeLogs_RendersEntriesNewestFirst(t *testing.T) {
	tmp := t.TempDir()
	older := writeClaudeLog(t, tmp, "wizard-spi-w1", "epic-plan-20260417-120000.log")
	newer := writeClaudeLog(t, tmp, "wizard-spi-w1", "epic-plan-20260417-173412.log")
	_ = older
	_ = newer

	var buf bytes.Buffer
	if err := listClaudeLogs(&buf, tmp, "wizard-spi-w1"); err != nil {
		t.Fatalf("listClaudeLogs returned error: %v", err)
	}
	out := buf.String()

	// Both entries must appear...
	if !strings.Contains(out, "20260417-173412") || !strings.Contains(out, "20260417-120000") {
		t.Fatalf("expected both timestamps in output, got: %q", out)
	}
	// ...and the newer timestamp must appear before the older one.
	iNewer := strings.Index(out, "20260417-173412")
	iOlder := strings.Index(out, "20260417-120000")
	if iNewer < 0 || iOlder < 0 || iNewer > iOlder {
		t.Fatalf("expected newest-first ordering, got: %q", out)
	}

	// Label should be extracted from the filename.
	if !strings.Contains(out, "epic-plan") {
		t.Fatalf("expected label 'epic-plan' in output, got: %q", out)
	}
}

func TestResolveClaudeLog_PicksNewestMatchingLabel(t *testing.T) {
	tmp := t.TempDir()
	writeClaudeLog(t, tmp, "wizard-spi-w2", "epic-plan-20260417-120000.log")
	newest := writeClaudeLog(t, tmp, "wizard-spi-w2", "epic-plan-20260417-173412.log")
	// Another label should not interfere.
	writeClaudeLog(t, tmp, "wizard-spi-w2", "recovery-decide-20260418-000000.log")

	got, err := resolveClaudeLog(tmp, "wizard-spi-w2", "epic-plan")
	if err != nil {
		t.Fatalf("resolveClaudeLog returned error: %v", err)
	}
	if got != newest {
		t.Fatalf("resolveClaudeLog = %q, want newest = %q", got, newest)
	}
}

func TestResolveClaudeLog_NoMatchErrors(t *testing.T) {
	tmp := t.TempDir()
	writeClaudeLog(t, tmp, "wizard-spi-w3", "epic-plan-20260417-120000.log")

	if _, err := resolveClaudeLog(tmp, "wizard-spi-w3", "recovery-decide"); err == nil {
		t.Fatalf("expected error for unknown label, got nil")
	}
}

func TestResolveClaudeLog_LabelPrefixIsExact(t *testing.T) {
	tmp := t.TempDir()
	// Label "epic" must not accidentally match "epic-plan-..." because
	// resolution anchors on "<label>-" including the hyphen.
	writeClaudeLog(t, tmp, "wizard-spi-w4", "epic-plan-20260417-120000.log")
	writeClaudeLog(t, tmp, "wizard-spi-w4", "epic-20260417-173412.log")

	got, err := resolveClaudeLog(tmp, "wizard-spi-w4", "epic")
	if err != nil {
		t.Fatalf("resolveClaudeLog returned error: %v", err)
	}
	if filepath.Base(got) != "epic-20260417-173412.log" {
		t.Fatalf("resolveClaudeLog with label 'epic' = %q, want 'epic-20260417-173412.log'",
			filepath.Base(got))
	}
}

func TestTailClaudeFile_RejectsPathsOutsideWizards(t *testing.T) {
	tmp := t.TempDir()
	// Write a log *outside* the wizards/ subtree.
	outside := filepath.Join(tmp, "not-wizards", "rogue.log")
	if err := os.MkdirAll(filepath.Dir(outside), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(outside, []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	err := tailClaudeFile(tmp, outside, 10, false)
	if err == nil {
		t.Fatalf("expected error for path outside wizards/, got nil")
	}
	if !strings.Contains(err.Error(), "must live under") {
		t.Fatalf("expected 'must live under' in error, got: %v", err)
	}
}

func TestTailClaudeFile_RejectsNonAbsolutePaths(t *testing.T) {
	tmp := t.TempDir()
	err := tailClaudeFile(tmp, "relative/path.log", 10, false)
	if err == nil {
		t.Fatalf("expected error for relative path, got nil")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("expected 'absolute' in error, got: %v", err)
	}
}

func TestTailClaudeFile_RejectsTraversalOutsideWizards(t *testing.T) {
	tmp := t.TempDir()
	// Looks like it's under wizards/ but the ".." escapes it.
	sneaky := filepath.Join(tmp, "wizards", "..", "escape.log")
	if err := os.WriteFile(filepath.Join(tmp, "escape.log"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	err := tailClaudeFile(tmp, sneaky, 10, false)
	if err == nil {
		t.Fatalf("expected error for traversal path, got nil")
	}
}

func TestCmdLogs_ClaudeRequiresWizardName(t *testing.T) {
	err := cmdLogs([]string{"--claude"})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "--claude requires a wizard name") {
		t.Fatalf("expected helpful message, got: %v", err)
	}
}
