package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestListTranscripts_MissingDirPrintsFriendlyMessage(t *testing.T) {
	tmp := t.TempDir()
	var buf bytes.Buffer
	if err := listTranscripts(&buf, tmp, "wizard-spi-missing", ""); err != nil {
		t.Fatalf("listTranscripts returned error: %v", err)
	}
	got := buf.String()
	want := "No transcripts recorded for wizard-spi-missing"
	if !strings.Contains(got, want) {
		t.Fatalf("listTranscripts output = %q, want substring %q", got, want)
	}
}

func TestListTranscripts_EmptyDirPrintsFriendlyMessage(t *testing.T) {
	tmp := t.TempDir()
	// Create the directory but no transcript files.
	if err := os.MkdirAll(filepath.Join(tmp, "wizards", "wizard-spi-empty", "claude"), 0o755); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	var buf bytes.Buffer
	if err := listTranscripts(&buf, tmp, "wizard-spi-empty", ""); err != nil {
		t.Fatalf("listTranscripts returned error: %v", err)
	}
	if !strings.Contains(buf.String(), "No transcripts recorded for wizard-spi-empty") {
		t.Fatalf("expected friendly message, got: %q", buf.String())
	}
}

func TestListTranscripts_RendersEntriesNewestFirst(t *testing.T) {
	tmp := t.TempDir()
	older := writeClaudeLog(t, tmp, "wizard-spi-w1", "epic-plan-20260417-120000.log")
	newer := writeClaudeLog(t, tmp, "wizard-spi-w1", "epic-plan-20260417-173412.log")
	// Force mtime ordering so it matches the filename timestamps.
	past := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 4, 17, 17, 34, 12, 0, time.UTC)
	if err := os.Chtimes(older, past, past); err != nil {
		t.Fatalf("chtimes older: %v", err)
	}
	if err := os.Chtimes(newer, recent, recent); err != nil {
		t.Fatalf("chtimes newer: %v", err)
	}

	var buf bytes.Buffer
	if err := listTranscripts(&buf, tmp, "wizard-spi-w1", ""); err != nil {
		t.Fatalf("listTranscripts returned error: %v", err)
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

	// Label should still be recoverable from the filename.
	if !strings.Contains(out, "epic-plan") {
		t.Fatalf("expected label 'epic-plan' in output, got: %q", out)
	}
	// Provider column should be emitted.
	if !strings.Contains(out, "PROVIDER") {
		t.Fatalf("expected PROVIDER column header in output, got: %q", out)
	}
	if !strings.Contains(out, "claude") {
		t.Fatalf("expected provider 'claude' in output, got: %q", out)
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

// writeProviderTranscript creates <doltGlobal>/wizards/<wizard>/<provider>/<name>.
func writeProviderTranscript(t *testing.T, doltGlobal, wizard, provider, name, body string) string {
	t.Helper()
	dir := filepath.Join(doltGlobal, "wizards", wizard, provider)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestDiscoverTranscripts_EnumeratesAllProviders(t *testing.T) {
	tmp := t.TempDir()
	wizardDir := filepath.Join(tmp, "wizards", "wizard-spi-abc")
	claudePath := writeProviderTranscript(t, tmp, "wizard-spi-abc", "claude", "implement-20260421-142001.jsonl", "{}\n")
	codexPath := writeProviderTranscript(t, tmp, "wizard-spi-abc", "codex", "implement-20260421-142120.jsonl", "{}\n")
	// Force ascending mtime to match filename timestamps so ts[len-1] == codex.
	early := time.Now().Add(-2 * time.Minute)
	later := time.Now().Add(-26 * time.Second)
	if err := os.Chtimes(claudePath, early, early); err != nil {
		t.Fatalf("chtimes claude: %v", err)
	}
	if err := os.Chtimes(codexPath, later, later); err != nil {
		t.Fatalf("chtimes codex: %v", err)
	}

	ts, err := discoverTranscripts(wizardDir, "")
	if err != nil {
		t.Fatalf("discoverTranscripts: %v", err)
	}
	if len(ts) != 2 {
		t.Fatalf("expected 2 transcripts, got %d: %+v", len(ts), ts)
	}
	// Ascending by mtime: claude first, codex last.
	if ts[0].Provider != "claude" {
		t.Errorf("ts[0].Provider = %q, want claude", ts[0].Provider)
	}
	if ts[len(ts)-1].Provider != "codex" {
		t.Errorf("ts[last].Provider = %q, want codex", ts[len(ts)-1].Provider)
	}
}

func TestDiscoverTranscripts_ProviderFilter(t *testing.T) {
	tmp := t.TempDir()
	wizardDir := filepath.Join(tmp, "wizards", "wizard-spi-xyz")
	writeProviderTranscript(t, tmp, "wizard-spi-xyz", "claude", "a.jsonl", "{}\n")
	writeProviderTranscript(t, tmp, "wizard-spi-xyz", "codex", "b.jsonl", "{}\n")

	ts, err := discoverTranscripts(wizardDir, "codex")
	if err != nil {
		t.Fatalf("discoverTranscripts: %v", err)
	}
	if len(ts) != 1 {
		t.Fatalf("expected 1 transcript with filter=codex, got %d", len(ts))
	}
	if ts[0].Provider != "codex" {
		t.Errorf("got provider %q, want codex", ts[0].Provider)
	}
}

func TestDiscoverTranscripts_ExcludesStderrSidecars(t *testing.T) {
	tmp := t.TempDir()
	wizardDir := filepath.Join(tmp, "wizards", "wizard-spi-sid")
	writeProviderTranscript(t, tmp, "wizard-spi-sid", "claude", "plan-20260421-100000.jsonl", "{}\n")
	writeProviderTranscript(t, tmp, "wizard-spi-sid", "claude", "plan-20260421-100000.stderr.log", "boom\n")

	ts, err := discoverTranscripts(wizardDir, "")
	if err != nil {
		t.Fatalf("discoverTranscripts: %v", err)
	}
	if len(ts) != 1 {
		t.Fatalf("expected 1 transcript (sidecar excluded), got %d: %+v", len(ts), ts)
	}
	if strings.HasSuffix(ts[0].Path, ".stderr.log") {
		t.Errorf("sidecar was not excluded: %s", ts[0].Path)
	}
}

func TestDiscoverTranscripts_ClaudeLegacyLogExtension(t *testing.T) {
	tmp := t.TempDir()
	wizardDir := filepath.Join(tmp, "wizards", "wizard-spi-legacy")
	// Legacy Claude transcripts have .log extension (pre-spi-7mgv9).
	writeClaudeLog(t, tmp, "wizard-spi-legacy", "epic-plan-20260417-173412.log")

	ts, err := discoverTranscripts(wizardDir, "")
	if err != nil {
		t.Fatalf("discoverTranscripts: %v", err)
	}
	if len(ts) != 1 {
		t.Fatalf("expected 1 legacy claude transcript, got %d", len(ts))
	}
	if ts[0].Provider != "claude" {
		t.Errorf("got provider %q, want claude", ts[0].Provider)
	}
}

func TestProviderExtensions(t *testing.T) {
	cases := map[string][]string{
		"claude":  {".jsonl", ".log"},
		"codex":   {".jsonl"},
		"unknown": {".jsonl"},
	}
	for name, want := range cases {
		got := providerExtensions(name)
		if len(got) != len(want) {
			t.Errorf("providerExtensions(%q) = %v, want %v", name, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("providerExtensions(%q)[%d] = %q, want %q", name, i, got[i], want[i])
			}
		}
	}
}

func TestWizardDirForBead_NormalizesPrefix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"spi-abc", "/gd/wizards/wizard-spi-abc"},
		{"wizard-spi-abc", "/gd/wizards/wizard-spi-abc"},
	}
	for _, c := range cases {
		got := wizardDirForBead("/gd", c.in)
		if got != c.want {
			t.Errorf("wizardDirForBead(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
