// End-to-end fixtures for the provider-agnostic `spire logs` listing path.
// Complements pkg/board/transcript_e2e_test.go which covers the inspector
// half of the same epic (spi-gbd0y). Fixtures live under
// pkg/board/testdata/transcripts/ as the single source of truth and are
// read here via a relative path.
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixturesRoot is where the shared fixture tree lives, relative to this
// test file's directory (cmd/spire/). Kept in one place so a future layout
// change only touches this constant.
const fixturesRoot = "../../pkg/board/testdata/transcripts"

// readFixture returns the bytes of testdata/transcripts/<rel> as stored
// under pkg/board.
func readFixture(t *testing.T, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixturesRoot, rel))
	if err != nil {
		t.Fatalf("read fixture %s: %v", rel, err)
	}
	return data
}

// plantFixture copies fixture <fixtureRel> into
// <doltGlobal>/wizards/<wizard>/<destRel>, creating parents as needed.
func plantFixture(t *testing.T, doltGlobal, wizard, fixtureRel, destRel string) string {
	t.Helper()
	data := readFixture(t, fixtureRel)
	dst := filepath.Join(doltGlobal, "wizards", wizard, destRel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(dst), err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
	return dst
}

// TestSpireLogs_ListsAllProviders populates a wizard dir with transcripts
// under both claude/ and codex/ and verifies:
//  1. the default listing (no --provider) shows a row for each transcript
//     with the provider column populated for both, and
//  2. --provider=codex filters Claude rows out.
func TestSpireLogs_ListsAllProviders(t *testing.T) {
	tmp := t.TempDir()
	wizard := "wizard-wiz-a"
	plantFixture(t, tmp, wizard, "claude/new_run.jsonl", "claude/new_run.jsonl")
	plantFixture(t, tmp, wizard, "codex/tool_run_err.jsonl", "codex/tool_run_err.jsonl")
	plantFixture(t, tmp, wizard, "codex/tool_run_err.stderr.log", "codex/tool_run_err.stderr.log")

	// All providers.
	var all bytes.Buffer
	if err := listTranscripts(&all, tmp, wizard, ""); err != nil {
		t.Fatalf("listTranscripts all: %v", err)
	}
	out := all.String()
	if !strings.Contains(out, "PROVIDER") {
		t.Errorf("missing PROVIDER column in output: %q", out)
	}
	if !strings.Contains(out, "claude") {
		t.Errorf("missing claude row: %q", out)
	}
	if !strings.Contains(out, "codex") {
		t.Errorf("missing codex row: %q", out)
	}
	if !strings.Contains(out, "new_run.jsonl") {
		t.Errorf("missing claude transcript filename: %q", out)
	}
	if !strings.Contains(out, "tool_run_err.jsonl") {
		t.Errorf("missing codex transcript filename: %q", out)
	}
	// Sidecar must not appear as a transcript row — only as a non-listed
	// companion file (discoverTranscripts excludes *.stderr.log).
	if strings.Contains(out, "tool_run_err.stderr.log") {
		t.Errorf("stderr sidecar leaked into transcript listing: %q", out)
	}

	// --provider=codex filters claude rows out.
	var codexOnly bytes.Buffer
	if err := listTranscripts(&codexOnly, tmp, wizard, "codex"); err != nil {
		t.Fatalf("listTranscripts codex-only: %v", err)
	}
	outC := codexOnly.String()
	if !strings.Contains(outC, "tool_run_err.jsonl") {
		t.Errorf("codex-only listing missing codex transcript: %q", outC)
	}
	if strings.Contains(outC, "new_run.jsonl") {
		t.Errorf("codex-only listing leaked claude transcript: %q", outC)
	}
}

// TestSpireLogs_ClaudeLegacyGlobbed confirms the {*.jsonl,*.log} glob
// still discovers legacy .log-extension claude transcripts and lists them
// with provider=claude. Regression guard against the spi-7mgv9 rename.
func TestSpireLogs_ClaudeLegacyGlobbed(t *testing.T) {
	tmp := t.TempDir()
	wizard := "wizard-wiz-legacy"
	plantFixture(t, tmp, wizard, "claude/legacy_run.log", "claude/legacy_run.log")

	var buf bytes.Buffer
	if err := listTranscripts(&buf, tmp, wizard, ""); err != nil {
		t.Fatalf("listTranscripts: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "legacy_run.log") {
		t.Errorf("legacy .log transcript not listed: %q", out)
	}
	if !strings.Contains(out, "claude") {
		t.Errorf("legacy .log transcript not labelled provider=claude: %q", out)
	}
}
