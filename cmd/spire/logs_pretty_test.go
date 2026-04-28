package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunLogsPretty_EmptyBeadPrintsFriendlyMessage covers the spec's
// "empty == not an error" requirement: a bead with no transcripts must
// not surface as an error to the user, otherwise a fresh bead looks
// broken in the CLI. The friendly message must include the bead ID and
// hint at where to look.
func TestRunLogsPretty_EmptyBeadPrintsFriendlyMessage(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_DOLT_DIR", tmp)
	t.Setenv("SPIRE_CONFIG_DIR", t.TempDir())
	// Ensure local-native: no active tower override.
	prev := activeTowerConfigFunc
	activeTowerConfigFunc = func() (*TowerConfig, error) { return nil, nil }
	defer func() { activeTowerConfigFunc = prev }()

	out, err := captureStdout(t, func() error {
		return runLogsPretty("spi-empty", "", false)
	})
	if err != nil {
		t.Fatalf("runLogsPretty returned error for empty bead: %v", err)
	}
	if !strings.Contains(out, "no transcripts found for spi-empty") {
		t.Fatalf("expected friendly empty message, got: %q", out)
	}
}

// TestRunLogsPretty_RendersClaudeTranscript walks a happy-path local
// transcript through the source → adapter → render pipeline. Any path
// that drops the adapter (or the local fallback) would surface as
// silent empty output here.
func TestRunLogsPretty_RendersClaudeTranscript(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_DOLT_DIR", tmp)
	t.Setenv("SPIRE_CONFIG_DIR", t.TempDir())
	prev := activeTowerConfigFunc
	activeTowerConfigFunc = func() (*TowerConfig, error) { return nil, nil }
	defer func() { activeTowerConfigFunc = prev }()

	beadID := "spi-pretty"
	wizardName := "wizard-" + beadID
	dir := filepath.Join(tmp, "wizards", wizardName, "claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	transcript := `{"type":"system","subtype":"init","session_id":"abc"}` + "\n" +
		`{"type":"user","message":{"role":"user","content":"hello"}}` + "\n"
	transcriptPath := filepath.Join(dir, "implement-20260417-173412.jsonl")
	if err := os.WriteFile(transcriptPath, []byte(transcript), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	out, err := captureStdout(t, func() error {
		return runLogsPretty(beadID, "", false)
	})
	if err != nil {
		t.Fatalf("runLogsPretty: %v", err)
	}
	if out == "" {
		t.Fatalf("expected non-empty rendered output for valid transcript")
	}
	// The "no transcripts" empty branch must NOT fire for a bead that
	// has a transcript on disk.
	if strings.Contains(out, "no transcripts found") {
		t.Errorf("rendered output incorrectly took empty path: %q", out)
	}
}

// TestRunLogsPretty_ProviderFilterReducesToZero verifies the empty
// branch fires when a provider filter excludes every available
// transcript — the user asked for codex but only claude exists.
func TestRunLogsPretty_ProviderFilterReducesToZero(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_DOLT_DIR", tmp)
	t.Setenv("SPIRE_CONFIG_DIR", t.TempDir())
	prev := activeTowerConfigFunc
	activeTowerConfigFunc = func() (*TowerConfig, error) { return nil, nil }
	defer func() { activeTowerConfigFunc = prev }()

	beadID := "spi-only-claude"
	wizardName := "wizard-" + beadID
	dir := filepath.Join(tmp, "wizards", wizardName, "claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "implement-20260417-173412.jsonl"),
		[]byte(`{"type":"system"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	out, err := captureStdout(t, func() error {
		return runLogsPretty(beadID, "codex", false)
	})
	if err != nil {
		t.Fatalf("runLogsPretty: %v", err)
	}
	if !strings.Contains(out, "no transcripts found for spi-only-claude") {
		t.Fatalf("expected empty message for codex filter, got: %q", out)
	}
}
