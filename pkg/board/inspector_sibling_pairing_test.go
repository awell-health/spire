package board

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/awell-health/spire/pkg/store"
)

// TestInspectorDiscovery_PairsSiblingLogWithClaudeTranscripts verifies the
// single-stem naming contract at the reader side: when a sibling wizard's
// orchestrator log and its claude transcript share a directory stem
// "<wizardName>-<step>-<attemptNum>", the inspector returns both a LogView for
// the orchestrator .log and a LogView for every transcript under
// <stem>/claude/*.jsonl.
//
// Before spi-tayh7, writers used two conventions — orchestrator log with a
// "-1" suffix but claude subdir without — so the inspector's glob+stem logic
// could not pair them and the claude transcripts were invisible on the board.
func TestInspectorDiscovery_PairsSiblingLogWithClaudeTranscripts(t *testing.T) {
	doltDir := t.TempDir()
	t.Setenv("SPIRE_DOLT_DIR", doltDir)
	// Inspector also reads config state under SPIRE_CONFIG_DIR for the hooked
	// step path; point it at a scratch dir so tests don't touch real state.
	t.Setenv("SPIRE_CONFIG_DIR", t.TempDir())

	beadID := "spi-test"
	wizardName := "wizard-" + beadID

	wizardsDir := filepath.Join(doltDir, "wizards")
	if err := os.MkdirAll(wizardsDir, 0o755); err != nil {
		t.Fatalf("mkdir wizards: %v", err)
	}

	// Parent wizard orchestrator log (the top-level "wizard" LogView).
	if err := os.WriteFile(filepath.Join(wizardsDir, wizardName+".log"),
		[]byte("parent wizard log\n"), 0o644); err != nil {
		t.Fatalf("write parent log: %v", err)
	}

	// Sibling spawn: orchestrator log + per-attempt claude transcript tree.
	siblingStem := wizardName + "-implement-1"
	if err := os.WriteFile(filepath.Join(wizardsDir, siblingStem+".log"),
		[]byte("sibling implement-1 log\n"), 0o644); err != nil {
		t.Fatalf("write sibling log: %v", err)
	}
	claudeDir := filepath.Join(wizardsDir, siblingStem, "claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatalf("mkdir claude: %v", err)
	}
	transcript := `{"type":"system","subtype":"init","session_id":"abc"}` + "\n"
	if err := os.WriteFile(filepath.Join(claudeDir, "implement-20260422-184843.jsonl"),
		[]byte(transcript), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	bead := store.BoardBead{ID: beadID, Status: "in_progress", Type: "task"}
	data := FetchInspectorData(bead)

	// Build an index of returned views by Name for readable assertions.
	byName := map[string]LogView{}
	for _, lv := range data.Logs {
		byName[lv.Name] = lv
	}

	// Orchestrator sibling log: the inspector strips the wizardName prefix, so
	// the LogView name is "implement-1".
	siblingLog, ok := byName["implement-1"]
	if !ok {
		var names []string
		for _, lv := range data.Logs {
			names = append(names, lv.Name)
		}
		t.Fatalf("expected sibling log view with Name=%q; got names=%v", "implement-1", names)
	}
	wantLogPath := filepath.Join(wizardsDir, siblingStem+".log")
	if siblingLog.Path != wantLogPath {
		t.Errorf("sibling log Path = %q, want %q", siblingLog.Path, wantLogPath)
	}

	// Claude transcript paired with the sibling: name is "<stem>/<label> (HH:MM)".
	wantTranscriptName := "implement-1/implement (18:48)"
	transcriptView, ok := byName[wantTranscriptName]
	if !ok {
		var names []string
		for _, lv := range data.Logs {
			names = append(names, lv.Name)
		}
		t.Fatalf("expected claude transcript view with Name=%q; got names=%v",
			wantTranscriptName, names)
	}
	if transcriptView.Provider != "claude" {
		t.Errorf("claude transcript Provider = %q, want %q", transcriptView.Provider, "claude")
	}
	wantTranscriptPath := filepath.Join(claudeDir, "implement-20260422-184843.jsonl")
	if transcriptView.Path != wantTranscriptPath {
		t.Errorf("claude transcript Path = %q, want %q", transcriptView.Path, wantTranscriptPath)
	}
}

// TestInspectorDiscovery_LegacySplitLayout_ClaudeTranscriptHidden documents the
// explicit non-migration choice made for spi-tayh7: on-disk trees written
// before this fix (orchestrator log suffixed with -1 but claude subdir
// unsuffixed) remain readable as raw sibling logs, but their claude
// transcripts are NOT surfaced by the inspector. Migrating legacy trees is
// out of scope; the intentional behavior is locked in by this test so a
// future "fallback glob for safety" does not accidentally reintroduce
// heuristic pairing.
func TestInspectorDiscovery_LegacySplitLayout_ClaudeTranscriptHidden(t *testing.T) {
	doltDir := t.TempDir()
	t.Setenv("SPIRE_DOLT_DIR", doltDir)
	t.Setenv("SPIRE_CONFIG_DIR", t.TempDir())

	beadID := "spi-legacy"
	wizardName := "wizard-" + beadID

	wizardsDir := filepath.Join(doltDir, "wizards")
	if err := os.MkdirAll(wizardsDir, 0o755); err != nil {
		t.Fatalf("mkdir wizards: %v", err)
	}

	// Orchestrator log uses the -1 suffix (the pre-fix versioned name).
	siblingLogStem := wizardName + "-implement-1"
	if err := os.WriteFile(filepath.Join(wizardsDir, siblingLogStem+".log"),
		[]byte("legacy sibling log\n"), 0o644); err != nil {
		t.Fatalf("write legacy log: %v", err)
	}
	// Claude subdir uses the bare (no-N) name — the legacy split layout.
	legacyClaudeDir := filepath.Join(wizardsDir, wizardName+"-implement", "claude")
	if err := os.MkdirAll(legacyClaudeDir, 0o755); err != nil {
		t.Fatalf("mkdir legacy claude: %v", err)
	}
	if err := os.WriteFile(filepath.Join(legacyClaudeDir, "implement-20260422-184843.jsonl"),
		[]byte(`{"type":"system"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write legacy transcript: %v", err)
	}

	bead := store.BoardBead{ID: beadID, Status: "in_progress", Type: "task"}
	data := FetchInspectorData(bead)

	// The orchestrator log is still discovered (it matches the glob).
	var sawSiblingLog bool
	for _, lv := range data.Logs {
		if lv.Name == "implement-1" {
			sawSiblingLog = true
		}
		// The legacy claude transcript must NOT appear under the versioned
		// stem — that's the pairing the fix intentionally does not perform.
		if lv.Provider == "claude" && lv.Name == "implement-1/implement (18:48)" {
			t.Errorf("legacy split-layout claude transcript was paired against the versioned stem; got LogView %q with Path=%q",
				lv.Name, lv.Path)
		}
	}
	if !sawSiblingLog {
		t.Fatalf("expected sibling orchestrator log to be discovered as raw; got none matching %q", "implement-1")
	}
}
