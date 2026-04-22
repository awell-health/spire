// End-to-end transcript fixture tests covering the downstream half of the
// provider-neutral raw-transcript epic (spi-gbd0y): discovery under the new
// wizards/<wizard>/<provider>/ layout, inspector pretty/errors rendering,
// and Claude-legacy-extension compatibility.
//
// The writer path (pkg/wizard/wizard.go) is deliberately NOT exercised —
// subtask spi-7mgv9 owns its own capture tests. This file trusts the
// writer's on-disk layout and plants bytes directly under t.TempDir().
//
// Fixtures live under pkg/board/testdata/transcripts/ and are copied into
// the synthesized wizards tree per-test. No test touches a real
// ~/.spire or tower directory.
package board

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/board/logstream"
)

// testWizard is the wizard name used by every e2e test; the loadProviderLogViews
// input is <tmp>/wizards/<testWizard>/, which mirrors the runtime layout.
const testWizard = "wiz-a"

// ansiRE matches CSI (ANSI) color escape sequences. Used to strip lipgloss
// styling from rendered output before substring matching so the tests are
// robust against palette / terminal-profile changes.
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// stripStyle removes ANSI CSI escapes from s.
func stripStyle(s string) string { return ansiRE.ReplaceAllString(s, "") }

// newWizardTree returns a fresh temp root and the stable wizard name
// "wiz-a". The returned rootDir has <rootDir>/wizards/<testWizard>/ created
// so callers can write fixtures directly into provider subdirs.
func newWizardTree(t *testing.T) (rootDir, wizardName string) {
	t.Helper()
	rootDir = t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootDir, "wizards", testWizard), 0o755); err != nil {
		t.Fatalf("mkdir wizards/%s: %v", testWizard, err)
	}
	return rootDir, testWizard
}

// wizardDir returns <rootDir>/wizards/<wizardName>, the base path
// loadProviderLogViews walks.
func wizardDir(rootDir, wizardName string) string {
	return filepath.Join(rootDir, "wizards", wizardName)
}

// writeFixture copies a fixture from testdata/transcripts/<fixtureRel> into
// <rootDir>/wizards/<testWizard>/<destRel>, creating parent dirs as needed.
// Returns the destination absolute path.
func writeFixture(t *testing.T, rootDir, fixtureRel, destRel string) string {
	t.Helper()
	src := filepath.Join("testdata", "transcripts", fixtureRel)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read fixture %s: %v", src, err)
	}
	dst := filepath.Join(rootDir, "wizards", testWizard, destRel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(dst), err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
	return dst
}

// findLogView returns the first LogView with the given Provider and
// filename suffix match. Suffix matching avoids coupling tests to the
// exact directory prefix computed inside loadProviderLogViews.
func findLogView(t *testing.T, views []LogView, provider, baseName string) *LogView {
	t.Helper()
	for i := range views {
		if views[i].Provider == provider && strings.HasSuffix(views[i].Path, baseName) {
			return &views[i]
		}
	}
	t.Fatalf("no LogView with provider=%q and base=%q in %d views", provider, baseName, len(views))
	return nil
}

// eventKinds projects a slice of LogEvent to their Kind values for easier
// assertion / diagnostic output.
func eventKinds(events []logstream.LogEvent) []logstream.EventKind {
	out := make([]logstream.EventKind, len(events))
	for i, e := range events {
		out[i] = e.Kind
	}
	return out
}

// TestTranscriptLayout_CodexDiscovery verifies that a .jsonl + .stderr.log
// pair planted under wizards/<wizard>/codex/ surfaces as a single LogView
// with Provider="codex" and StderrPath pointing at the sidecar file.
func TestTranscriptLayout_CodexDiscovery(t *testing.T) {
	root, _ := newWizardTree(t)
	jsonlPath := writeFixture(t, root, "codex/tool_run_err.jsonl", "codex/tool_run_err.jsonl")
	stderrPath := writeFixture(t, root, "codex/tool_run_err.stderr.log", "codex/tool_run_err.stderr.log")

	views := loadProviderLogViews(wizardDir(root, testWizard), "")
	if len(views) != 1 {
		t.Fatalf("expected 1 LogView, got %d: %+v", len(views), views)
	}
	lv := views[0]
	if lv.Provider != "codex" {
		t.Errorf("Provider = %q, want %q", lv.Provider, "codex")
	}
	if lv.Path != jsonlPath {
		t.Errorf("Path = %q, want %q", lv.Path, jsonlPath)
	}
	if lv.StderrPath != stderrPath {
		t.Errorf("StderrPath = %q, want %q", lv.StderrPath, stderrPath)
	}
	if lv.StderrContent == "" {
		t.Errorf("StderrContent is empty; expected sidecar bytes to be loaded")
	}
	if len(lv.Events) == 0 {
		t.Errorf("Events is empty; codex adapter should have parsed the transcript")
	}
}

// TestInspector_CodexPrettyDefault asserts that a codex transcript renders
// in pretty mode by default (LogModePretty == zero value) and produces
// the expected kinds — including a KindToolCall for the failing command
// and a KindToolResult flagged as an error.
func TestInspector_CodexPrettyDefault(t *testing.T) {
	if LogModePretty != 0 {
		t.Fatalf("LogModePretty = %d, want 0 (zero-value default)", LogModePretty)
	}

	root, _ := newWizardTree(t)
	writeFixture(t, root, "codex/tool_run_err.jsonl", "codex/tool_run_err.jsonl")
	writeFixture(t, root, "codex/tool_run_err.stderr.log", "codex/tool_run_err.stderr.log")

	views := loadProviderLogViews(wizardDir(root, testWizard), "")
	lv := findLogView(t, views, "codex", "tool_run_err.jsonl")
	if len(lv.Events) == 0 {
		t.Fatal("LogView.Events is empty")
	}

	// The parsed event stream must contain the tool call for the failing
	// command and a tool result with Error=true.
	var sawToolCall, sawErrorResult bool
	for _, ev := range lv.Events {
		if ev.Kind == logstream.KindToolCall && strings.Contains(ev.Title, "ls /nope") {
			sawToolCall = true
		}
		if ev.Kind == logstream.KindToolResult && ev.Error {
			sawErrorResult = true
		}
	}
	if !sawToolCall {
		t.Errorf("no KindToolCall with 'ls /nope' in events: kinds=%v", eventKinds(lv.Events))
	}
	if !sawErrorResult {
		t.Errorf("no KindToolResult with Error=true in events: kinds=%v", eventKinds(lv.Events))
	}

	// Rendered pretty output must contain the tool-call marker and the
	// error-result marker.
	out := stripStyle(renderLogPane(lv, 100, LogModePretty, false, false))
	if !strings.Contains(out, "⚙ $ ") {
		t.Errorf("pretty output missing tool-call marker '⚙ $ ': %q", out)
	}
	if !strings.Contains(out, "exit 1") {
		t.Errorf("pretty output missing 'exit 1' result: %q", out)
	}
}

// TestInspector_ErrorsOnlyFilter asserts that toggling errorsOnly keeps only
// the error results and stderr sidecar lines; KindAssistantText /
// KindTurnStart / KindUsage must drop out.
func TestInspector_ErrorsOnlyFilter(t *testing.T) {
	root, _ := newWizardTree(t)
	writeFixture(t, root, "codex/tool_run_err.jsonl", "codex/tool_run_err.jsonl")
	writeFixture(t, root, "codex/tool_run_err.stderr.log", "codex/tool_run_err.stderr.log")

	views := loadProviderLogViews(wizardDir(root, testWizard), "")
	lv := findLogView(t, views, "codex", "tool_run_err.jsonl")

	// Manually apply the same filter predicate the renderer uses so we can
	// assert on structured data instead of rendered bytes.
	var kept []logstream.LogEvent
	for _, ev := range lv.Events {
		if shouldShowEvent(ev, true) {
			kept = append(kept, ev)
		}
	}
	if len(kept) == 0 {
		t.Fatalf("errorsOnly filter dropped every event; raw kinds=%v", eventKinds(lv.Events))
	}

	var (
		errorResults int
		stderrs      int
	)
	for _, ev := range kept {
		switch ev.Kind {
		case logstream.KindToolResult:
			if !ev.Error {
				t.Errorf("errorsOnly kept a non-error tool_result: %+v", ev)
			}
			errorResults++
		case logstream.KindStderr:
			stderrs++
		case logstream.KindAssistantText, logstream.KindTurnStart, logstream.KindUsage:
			t.Errorf("errorsOnly kept event of forbidden kind %v: %+v", ev.Kind, ev)
		}
	}
	if errorResults < 1 {
		t.Errorf("errorsOnly kept %d KindToolResult errors; want ≥1", errorResults)
	}
	// Sidecar has two non-empty lines → two KindStderr events.
	if stderrs != 2 {
		t.Errorf("errorsOnly kept %d KindStderr events; want 2", stderrs)
	}

	// Rendered output must contain the failed-command exit marker and the
	// stderr sidecar lines verbatim, and must NOT mention the assistant's
	// plain-text messages.
	out := stripStyle(renderLogPane(lv, 120, LogModePretty, true, false))
	if !strings.Contains(out, "exit 1") {
		t.Errorf("errorsOnly render missing 'exit 1': %q", out)
	}
	if !strings.Contains(out, "[mcp] server") {
		t.Errorf("errorsOnly render missing stderr sidecar content: %q", out)
	}
	if !strings.Contains(out, "[auth] token") {
		t.Errorf("errorsOnly render missing stderr sidecar content: %q", out)
	}
	if strings.Contains(out, "Directory does not exist") {
		t.Errorf("errorsOnly render leaked KindAssistantText body: %q", out)
	}
	if strings.Contains(out, "turn started") {
		t.Errorf("errorsOnly render leaked KindTurnStart: %q", out)
	}
	if strings.Contains(out, "input=18368") {
		t.Errorf("errorsOnly render leaked KindUsage body: %q", out)
	}
}

// TestInspector_ClaudeRegression asserts that the .log → .jsonl extension
// rename is a pure extension change: both files produce identical parsed
// events and identical pretty-rendered output. Regression guard for
// spi-7mgv9's writer change.
func TestInspector_ClaudeRegression(t *testing.T) {
	// Each file goes in its own wizard tree so loadProviderLogViews
	// returns them in isolation — otherwise the two files live in the
	// same claude/ dir and we'd have to disambiguate by path suffix on
	// every assertion.
	rootNew, _ := newWizardTree(t)
	writeFixture(t, rootNew, "claude/new_run.jsonl", "claude/new_run.jsonl")
	newViews := loadProviderLogViews(wizardDir(rootNew, testWizard), "")
	if len(newViews) != 1 {
		t.Fatalf("new-run: expected 1 LogView, got %d", len(newViews))
	}
	newView := newViews[0]

	rootLegacy, _ := newWizardTree(t)
	writeFixture(t, rootLegacy, "claude/legacy_run.log", "claude/legacy_run.log")
	legacyViews := loadProviderLogViews(wizardDir(rootLegacy, testWizard), "")
	if len(legacyViews) != 1 {
		t.Fatalf("legacy-run: expected 1 LogView, got %d", len(legacyViews))
	}
	legacyView := legacyViews[0]

	if newView.Provider != "claude" {
		t.Errorf("new-run Provider = %q, want claude", newView.Provider)
	}
	if legacyView.Provider != "claude" {
		t.Errorf("legacy-run Provider = %q, want claude", legacyView.Provider)
	}

	// Event vectors must be equal by Kind + Title + Body + Meta.
	if len(newView.Events) != len(legacyView.Events) {
		t.Fatalf("event count mismatch: new=%d legacy=%d; new_kinds=%v legacy_kinds=%v",
			len(newView.Events), len(legacyView.Events),
			eventKinds(newView.Events), eventKinds(legacyView.Events))
	}
	for i := range newView.Events {
		a, b := newView.Events[i], legacyView.Events[i]
		if a.Kind != b.Kind {
			t.Errorf("events[%d].Kind mismatch: new=%v legacy=%v", i, a.Kind, b.Kind)
		}
		if a.Title != b.Title {
			t.Errorf("events[%d].Title mismatch: new=%q legacy=%q", i, a.Title, b.Title)
		}
		if a.Body != b.Body {
			t.Errorf("events[%d].Body mismatch: new=%q legacy=%q", i, a.Body, b.Body)
		}
		if !metaEqual(a.Meta, b.Meta) {
			t.Errorf("events[%d].Meta mismatch: new=%v legacy=%v", i, a.Meta, b.Meta)
		}
	}

	// Pretty-mode rendering must be byte-identical. Strip styling first so
	// the comparison doesn't depend on lipgloss terminal probing.
	newRender := stripStyle(renderLogPane(&newView, 100, LogModePretty, false, false))
	legacyRender := stripStyle(renderLogPane(&legacyView, 100, LogModePretty, false, false))
	if newRender != legacyRender {
		t.Errorf("pretty render mismatch between .jsonl and .log extensions\nnew:\n%s\nlegacy:\n%s",
			newRender, legacyRender)
	}
}

// TestInspector_UnknownEventPreserved asserts that an unrecognized codex
// item type surfaces as KindUnknown with the original line preserved in
// Raw, and that LogModeRaw prints that line verbatim.
func TestInspector_UnknownEventPreserved(t *testing.T) {
	root, _ := newWizardTree(t)

	// Read the simple_run fixture, append a garbage line, and plant it.
	src, err := os.ReadFile(filepath.Join("testdata", "transcripts", "codex", "simple_run.jsonl"))
	if err != nil {
		t.Fatalf("read simple_run.jsonl: %v", err)
	}
	garbageLine := `{"type":"item.completed","item":{"type":"unheard_of_thing"}}`
	combined := append([]byte{}, src...)
	if !strings.HasSuffix(string(combined), "\n") {
		combined = append(combined, '\n')
	}
	combined = append(combined, []byte(garbageLine+"\n")...)
	dst := filepath.Join(root, "wizards", testWizard, "codex", "simple_run.jsonl")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(dst, combined, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	views := loadProviderLogViews(wizardDir(root, testWizard), "")
	lv := findLogView(t, views, "codex", "simple_run.jsonl")

	// Exactly one KindUnknown, and its Raw must equal the garbage line.
	var unknowns []logstream.LogEvent
	for _, ev := range lv.Events {
		if ev.Kind == logstream.KindUnknown {
			unknowns = append(unknowns, ev)
		}
	}
	if len(unknowns) != 1 {
		t.Fatalf("got %d KindUnknown events, want 1; kinds=%v", len(unknowns), eventKinds(lv.Events))
	}
	if unknowns[0].Raw != garbageLine {
		t.Errorf("unknown Raw = %q, want %q", unknowns[0].Raw, garbageLine)
	}

	// Raw-mode render must contain the garbage line verbatim (the raw
	// renderer prints Content unchanged).
	raw := stripStyle(renderLogPane(lv, 200, LogModeRaw, false, false))
	if !strings.Contains(raw, garbageLine) {
		t.Errorf("LogModeRaw output missing garbage line verbatim.\ngot:\n%s", raw)
	}
}

// metaEqual returns true when a and b contain the same key/value pairs.
// nil and empty maps compare equal, matching the behavior of reflect.DeepEqual
// for our use case without the reflection overhead.
func metaEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
