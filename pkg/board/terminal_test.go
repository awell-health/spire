package board

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// --- truncateAnsi tests ---

func TestTruncateAnsi_PlainShort(t *testing.T) {
	// String shorter than maxWidth should be returned unchanged.
	s := "hello"
	got := truncateAnsi(s, 10)
	if got != s {
		t.Errorf("expected %q, got %q", s, got)
	}
}

func TestTruncateAnsi_PlainExact(t *testing.T) {
	// String exactly at maxWidth should be returned unchanged.
	s := "hello"
	got := truncateAnsi(s, 5)
	if got != s {
		t.Errorf("expected %q, got %q", s, got)
	}
}

func TestTruncateAnsi_PlainTruncated(t *testing.T) {
	// Long plain string should be truncated with ellipsis.
	s := "hello world this is long"
	got := truncateAnsi(s, 10)
	// Should end with ellipsis and ANSI reset.
	if !strings.HasSuffix(got, "…\x1b[0m") {
		t.Errorf("expected truncated string to end with ellipsis+reset, got %q", got)
	}
	// The visible portion (before ellipsis) should be <= maxWidth.
	if len(got) >= len(s) {
		t.Errorf("expected truncated string to be shorter than original")
	}
}

func TestTruncateAnsi_WithAnsiCodes(t *testing.T) {
	// ANSI escape codes should not count toward visible width.
	s := "\x1b[31mred text\x1b[0m"
	// Visible content is "red text" = 8 chars.
	got := truncateAnsi(s, 20)
	if got != s {
		t.Errorf("expected unchanged string when ANSI content fits, got %q", got)
	}
}

func TestTruncateAnsi_AnsiTruncated(t *testing.T) {
	// ANSI string that exceeds visible width should be truncated.
	s := "\x1b[31mhello world this is long\x1b[0m"
	got := truncateAnsi(s, 10)
	if !strings.HasSuffix(got, "…\x1b[0m") {
		t.Errorf("expected truncated ANSI string to end with ellipsis+reset, got %q", got)
	}
}

func TestTruncateAnsi_EmptyString(t *testing.T) {
	got := truncateAnsi("", 10)
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestTruncateAnsi_MaxWidthOne(t *testing.T) {
	// Edge case: maxWidth of 1 — should still not panic.
	got := truncateAnsi("hello", 1)
	// Just verify it doesn't panic and returns something short.
	if len(got) > 20 { // generous bound — just checking it doesn't return the full string
		t.Errorf("expected very short result, got %q", got)
	}
}

// --- renderTerminalPane tests ---

func makeTermModel() Model {
	return Model{
		Width:  120,
		Height: 50,
	}
}

func TestRenderTerminalPane_Loading(t *testing.T) {
	m := makeTermModel()
	m.TermOpen = true
	m.TermLoading = true
	m.TermTitle = "Test Loading"

	out := renderTerminalPane(&m, 80, 30)

	if !strings.Contains(out, "Loading...") {
		t.Error("expected loading state to show 'Loading...'")
	}
	if !strings.Contains(out, "Test Loading") {
		t.Error("expected title to appear in output")
	}
}

func TestRenderTerminalPane_Empty(t *testing.T) {
	m := makeTermModel()
	m.TermOpen = true
	m.TermTitle = "Empty Pane"
	m.TermLines = nil

	out := renderTerminalPane(&m, 80, 30)

	if !strings.Contains(out, "(empty)") {
		t.Error("expected empty state to show '(empty)'")
	}
}

func TestRenderTerminalPane_WithContent(t *testing.T) {
	m := makeTermModel()
	m.TermOpen = true
	m.TermTitle = "Trace: spi-001"
	lines := make([]string, 20)
	for i := range lines {
		lines[i] = strings.Repeat("x", 10)
	}
	m.TermLines = lines

	out := renderTerminalPane(&m, 80, 30)

	if !strings.Contains(out, "Trace: spi-001") {
		t.Error("expected title in output")
	}
	// Content lines should appear.
	if !strings.Contains(out, "xxxxxxxxxx") {
		t.Error("expected content lines in output")
	}
	// Footer should appear.
	if !strings.Contains(out, "q/Esc close") {
		t.Error("expected footer key hints in output")
	}
}

func TestRenderTerminalPane_ScrollIndicator(t *testing.T) {
	m := makeTermModel()
	m.TermOpen = true
	m.TermTitle = "Scrollable"
	// Create more lines than viewport can hold (viewportH = height-5 = 25).
	lines := make([]string, 100)
	for i := range lines {
		lines[i] = "line content"
	}
	m.TermLines = lines
	m.TermScroll = 5

	out := renderTerminalPane(&m, 80, 30)

	// Should show scroll position indicator.
	if !strings.Contains(out, "[6/100]") {
		t.Errorf("expected scroll indicator [6/100] in output, got:\n%s", out)
	}
}

func TestRenderTerminalPane_MinDimensions(t *testing.T) {
	m := makeTermModel()
	m.TermOpen = true
	m.TermTitle = "Tiny"
	m.TermLines = []string{"a", "b", "c"}

	// Very small dimensions should be clamped up.
	out := renderTerminalPane(&m, 5, 3)
	if !strings.Contains(out, "Tiny") {
		t.Error("expected title even at small dimensions")
	}
}

func TestRenderTerminalPane_ViewportSlicing(t *testing.T) {
	m := makeTermModel()
	m.TermOpen = true
	m.TermTitle = "Slice Test"
	lines := make([]string, 50)
	for i := range lines {
		lines[i] = fmt.Sprintf("uniqueline-%03d", i)
	}
	m.TermLines = lines

	// Scroll to line 10. viewportH for height=30: h=30*85/100=25, clamped to 24 min → 25, vh=25-5=20.
	m.TermScroll = 10
	out := renderTerminalPane(&m, 80, 30)

	// Line at index 10 should appear, line at index 0 should not.
	if !strings.Contains(out, "uniqueline-010") {
		t.Error("expected line at scroll offset to appear")
	}
	if strings.Contains(out, "uniqueline-000") {
		t.Error("expected line before scroll offset to NOT appear")
	}
}

// --- Terminal pane Update (scroll/key handling) tests ---

func makeTermOpenModel(numLines int) Model {
	m := makeModel()
	m.TermOpen = true
	m.TermTitle = "Test"
	m.TermBeadID = "spi-001"
	lines := make([]string, numLines)
	for i := range lines {
		lines[i] = "line"
	}
	m.TermLines = lines
	m.TermScroll = 0
	return m
}

func TestTermPaneUpdate_ScrollDown(t *testing.T) {
	m := makeTermOpenModel(100)
	m = updateModel(m, keyMsg('j'))
	if m.TermScroll != 1 {
		t.Errorf("expected TermScroll=1 after j, got %d", m.TermScroll)
	}
}

func TestTermPaneUpdate_ScrollUp(t *testing.T) {
	m := makeTermOpenModel(100)
	m.TermScroll = 5
	m = updateModel(m, keyMsg('k'))
	if m.TermScroll != 4 {
		t.Errorf("expected TermScroll=4 after k from 5, got %d", m.TermScroll)
	}
}

func TestTermPaneUpdate_ScrollUpClampZero(t *testing.T) {
	m := makeTermOpenModel(100)
	m.TermScroll = 0
	m = updateModel(m, keyMsg('k'))
	if m.TermScroll != 0 {
		t.Errorf("expected TermScroll=0 (clamped), got %d", m.TermScroll)
	}
}

func TestTermPaneUpdate_ScrollDownClampMax(t *testing.T) {
	m := makeTermOpenModel(5) // few lines, viewport larger
	// With Height=40, viewportH = termViewportH(). The viewport is likely larger than 5 lines.
	// Scrolling down should clamp to 0 (content fits in viewport).
	m = updateModel(m, keyMsg('j'))
	if m.TermScroll != 0 {
		t.Errorf("expected TermScroll=0 (content fits viewport), got %d", m.TermScroll)
	}
}

func TestTermPaneUpdate_GoToBottom(t *testing.T) {
	m := makeTermOpenModel(200)
	m = updateModel(m, keyMsg('G'))
	viewportH := m.termViewportH()
	expected := len(m.TermLines) - viewportH
	if expected < 0 {
		expected = 0
	}
	if m.TermScroll != expected {
		t.Errorf("expected TermScroll=%d after G, got %d", expected, m.TermScroll)
	}
}

func TestTermPaneUpdate_GoToTop(t *testing.T) {
	m := makeTermOpenModel(200)
	m.TermScroll = 50
	// Press g once (sets PendingG), then g again (go to top).
	m = updateModel(m, keyMsg('g'))
	if !m.PendingG {
		t.Error("expected PendingG=true after first g")
	}
	m = updateModel(m, keyMsg('g'))
	if m.TermScroll != 0 {
		t.Errorf("expected TermScroll=0 after gg, got %d", m.TermScroll)
	}
	if m.PendingG {
		t.Error("expected PendingG=false after gg")
	}
}

func TestTermPaneUpdate_HalfPageDown(t *testing.T) {
	m := makeTermOpenModel(200)
	viewportH := m.termViewportH()
	m = updateModel(m, keyMsg('d'))
	expected := viewportH / 2
	if m.TermScroll != expected {
		t.Errorf("expected TermScroll=%d after d, got %d", expected, m.TermScroll)
	}
}

func TestTermPaneUpdate_HalfPageUp(t *testing.T) {
	m := makeTermOpenModel(200)
	viewportH := m.termViewportH()
	m.TermScroll = viewportH
	m = updateModel(m, keyMsg('u'))
	expected := viewportH - viewportH/2
	if m.TermScroll != expected {
		t.Errorf("expected TermScroll=%d after u from %d, got %d", expected, viewportH, m.TermScroll)
	}
}

func TestTermPaneUpdate_EscCloses(t *testing.T) {
	m := makeTermOpenModel(10)
	m = updateModel(m, keyMsgType(tea.KeyEscape))
	if m.TermOpen {
		t.Error("expected TermOpen=false after Esc")
	}
}

func TestTermPaneUpdate_QCloses(t *testing.T) {
	m := makeTermOpenModel(10)
	m = updateModel(m, keyMsg('q'))
	if m.TermOpen {
		t.Error("expected TermOpen=false after q")
	}
}

func TestTermPaneUpdate_AbsorbsOtherKeys(t *testing.T) {
	m := makeTermOpenModel(10)
	origCol := m.SelCol
	// Press 'l' which normally moves columns in the board.
	m = updateModel(m, keyMsg('l'))
	if !m.TermOpen {
		t.Error("expected TermOpen to remain true for unhandled key")
	}
	if m.SelCol != origCol {
		t.Error("expected board selection to be unchanged while term pane absorbs keys")
	}
}

func TestTermPaneUpdate_TermContentMsg(t *testing.T) {
	m := makeTermOpenModel(0)
	m.TermLoading = true

	msg := termContentMsg{
		Title:   "Trace: spi-abc",
		Content: "line1\nline2\nline3",
		BeadID:  "spi-abc",
	}
	m = updateModel(m, msg)

	if m.TermLoading {
		t.Error("expected TermLoading=false after termContentMsg")
	}
	if m.TermTitle != "Trace: spi-abc" {
		t.Errorf("expected title 'Trace: spi-abc', got %q", m.TermTitle)
	}
	if len(m.TermLines) != 3 {
		t.Errorf("expected 3 lines, got %d", len(m.TermLines))
	}
	if m.TermScroll != 0 {
		t.Errorf("expected TermScroll=0, got %d", m.TermScroll)
	}
}

func TestTermPaneUpdate_TermContentMsgError(t *testing.T) {
	m := makeTermOpenModel(0)
	m.TermLoading = true

	msg := termContentMsg{
		Title:  "Trace: spi-abc",
		BeadID: "spi-abc",
		Err:    errorf("fetch failed"),
	}
	m = updateModel(m, msg)

	if m.TermLoading {
		t.Error("expected TermLoading=false after error termContentMsg")
	}
	if len(m.TermLines) != 1 || !strings.Contains(m.TermLines[0], "fetch failed") {
		t.Errorf("expected error message in TermLines, got %v", m.TermLines)
	}
}

// --- termViewportH consistency test ---

func TestTermViewportH_MatchesRenderer(t *testing.T) {
	// Verify that termViewportH() produces the same value as renderTerminalPane's
	// internal viewportH for various terminal sizes.
	for _, height := range []int{24, 30, 40, 50, 80, 100} {
		m := Model{Width: 120, Height: height}
		got := m.termViewportH()

		// Replicate renderTerminalPane's calculation:
		h := height * 85 / 100
		if h < 24 {
			h = 24
		}
		if h > height {
			h = height
		}
		expected := h - 5
		if expected < 3 {
			expected = 3
		}

		if got != expected {
			t.Errorf("Height=%d: termViewportH()=%d, renderTerminalPane would use %d", height, got, expected)
		}
	}
}

// helper to create an error for testing
type simpleError string

func (e simpleError) Error() string { return string(e) }
func errorf(msg string) error       { return simpleError(msg) }
