package board

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestCommentKeyOpensModal(t *testing.T) {
	m := makeBoardMode()

	m = updateBoardMode(m, keyMsg('c'))

	if !m.CommentActive {
		t.Fatal("expected CommentActive=true after pressing 'c'")
	}
	if m.CommentBeadID != "spi-001" {
		t.Errorf("expected CommentBeadID=spi-001, got %q", m.CommentBeadID)
	}
	if !m.CommentTextarea.Focused() {
		t.Error("expected textarea to be focused on open")
	}
	if m.CommentTextarea.Placeholder != "Comment…" {
		t.Errorf("expected placeholder %q, got %q", "Comment…", m.CommentTextarea.Placeholder)
	}
	if m.CommentTextarea.CharLimit != commentCharLimit {
		t.Errorf("expected CharLimit=%d, got %d", commentCharLimit, m.CommentTextarea.CharLimit)
	}
	if m.CommentTextarea.Value() != "" {
		t.Errorf("expected empty textarea on open, got %q", m.CommentTextarea.Value())
	}
}

func TestCommentModalFreshPerOpen(t *testing.T) {
	m := makeBoardMode()
	m = updateBoardMode(m, keyMsg('c'))
	// Type some text, then close.
	m = updateBoardMode(m, keyMsg('h'))
	m = updateBoardMode(m, keyMsg('i'))
	m = updateBoardMode(m, keyMsgType(tea.KeyEscape))
	if m.CommentActive {
		t.Fatal("expected CommentActive=false after esc")
	}

	// Re-open: buffer must be fresh.
	m = updateBoardMode(m, keyMsg('c'))
	if got := m.CommentTextarea.Value(); got != "" {
		t.Errorf("expected empty textarea on re-open, got %q", got)
	}
}

func TestCommentModalEscCancels(t *testing.T) {
	m := makeBoardMode()
	m = updateBoardMode(m, keyMsg('c'))
	m = updateBoardMode(m, keyMsg('x'))

	m = updateBoardMode(m, keyMsgType(tea.KeyEscape))

	if m.CommentActive {
		t.Error("expected CommentActive=false after esc")
	}
	if m.CommentBeadID != "" {
		t.Errorf("expected CommentBeadID cleared, got %q", m.CommentBeadID)
	}
	if m.ActionRunning {
		t.Error("esc should not flip ActionRunning")
	}
}

func TestCommentModalEnterInsertsNewline(t *testing.T) {
	m := makeBoardMode()
	m = updateBoardMode(m, keyMsg('c'))

	m = updateBoardMode(m, keyMsg('a'))
	m = updateBoardMode(m, keyMsgType(tea.KeyEnter))
	m = updateBoardMode(m, keyMsg('b'))

	if !m.CommentActive {
		t.Fatal("expected modal to remain active after enter (enter inserts newline, not submit)")
	}
	got := m.CommentTextarea.Value()
	if got != "a\nb" {
		t.Errorf("expected textarea value %q, got %q", "a\nb", got)
	}
}

func TestCommentModalCtrlDSubmits(t *testing.T) {
	m := makeBoardMode()
	m = updateBoardMode(m, keyMsg('c'))
	m = updateBoardMode(m, keyMsg('h'))
	m = updateBoardMode(m, keyMsg('i'))

	result, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	m = result.(*BoardMode)

	if m.CommentActive {
		t.Error("expected CommentActive=false after ctrl+d")
	}
	if m.CommentBeadID != "" {
		t.Errorf("expected CommentBeadID cleared, got %q", m.CommentBeadID)
	}
	if !m.ActionRunning {
		t.Error("expected ActionRunning=true after submit")
	}
	if m.ActionStatus != "Adding comment..." {
		t.Errorf("expected ActionStatus=\"Adding comment...\", got %q", m.ActionStatus)
	}
	if cmd == nil {
		t.Error("expected non-nil tea.Cmd from submit to carry the AddComment call")
	}
}

func TestCommentModalCtrlDEmptyNoOp(t *testing.T) {
	m := makeBoardMode()
	m = updateBoardMode(m, keyMsg('c'))

	result, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	m = result.(*BoardMode)

	if !m.CommentActive {
		t.Error("empty submit should leave the modal open so the user can keep typing")
	}
	if m.ActionRunning {
		t.Error("empty submit should not flip ActionRunning")
	}
	if !strings.Contains(m.ActionStatus, "required") {
		t.Errorf("expected a 'required' status message, got %q", m.ActionStatus)
	}
	if cmd != nil {
		t.Error("empty submit should not emit a tea.Cmd")
	}
}

func TestCommentModalFooterHints(t *testing.T) {
	m := makeBoardMode()
	m.CommentActive = true
	hints := m.FooterHints()
	for _, want := range []string{"ctrl+d", "submit", "esc", "cancel", "enter", "newline"} {
		if !strings.Contains(hints, want) {
			t.Errorf("CommentActive hints missing %q, got %q", want, hints)
		}
	}
}

func TestCommentActiveHasOverlay(t *testing.T) {
	m := makeBoardMode()
	if m.HasOverlay() {
		t.Fatal("HasOverlay should be false in default state")
	}
	m.CommentActive = true
	if !m.HasOverlay() {
		t.Error("HasOverlay should be true when CommentActive")
	}
}

func TestRenderCommentModalIncludesExpectedChrome(t *testing.T) {
	m := makeBoardMode()
	m = updateBoardMode(m, keyMsg('c')) // opens on spi-001

	out := renderCommentModal(m)

	for _, want := range []string{
		"Comment on spi-001",
		"ctrl+d submit",
		"esc cancel",
		"enter newline",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("modal output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

func TestRenderCommentModalShowsPlaceholder(t *testing.T) {
	m := makeBoardMode()
	m = updateBoardMode(m, keyMsg('c'))

	out := renderCommentModal(m)

	// bubbles textarea renders the Placeholder string when the buffer is
	// empty — assert it appears somewhere in the rendered modal.
	if !strings.Contains(out, "Comment…") {
		t.Errorf("expected placeholder %q in modal output\n--- output ---\n%s", "Comment…", out)
	}
}

func TestRenderCommentModalSizeSmallTerminal(t *testing.T) {
	// Degenerate terminal — function must still produce a usable popup.
	popW, popH := commentModalSize(20, 8)
	if popW < 20 {
		t.Errorf("popW=%d must accommodate tiny terminals", popW)
	}
	if popH < 8 {
		t.Errorf("popH=%d must accommodate tiny terminals", popH)
	}
}
