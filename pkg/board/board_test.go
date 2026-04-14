package board

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// --- Test helpers ---

func keyMsg(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

func keyMsgType(t tea.KeyType) tea.KeyMsg {
	return tea.KeyMsg{Type: t}
}

func keyMsgStr(s string) tea.KeyMsg {
	// For special combos like "ctrl+u" that tea.KeyMsg.String() produces.
	switch s {
	case "ctrl+u":
		return tea.KeyMsg{Type: tea.KeyCtrlU}
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	default:
		// Fallback: treat as single rune.
		if len(s) == 1 {
			return keyMsg(rune(s[0]))
		}
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

func updateBoardMode(m *BoardMode, msg tea.Msg) *BoardMode {
	result, _ := m.Update(msg)
	return result.(*BoardMode)
}

// makeBoardMode creates a BoardMode with some columns populated for testing.
func makeBoardMode() *BoardMode {
	cols := Columns{
		Ready: []BoardBead{
			{ID: "spi-001", Title: "First task", Status: "open", Type: "task", Priority: 1, Labels: []string{"team:alpha"}},
			{ID: "spi-002", Title: "Second bug", Status: "open", Type: "bug", Priority: 2, Labels: []string{"team:beta"}},
		},
		Implement: []BoardBead{
			{ID: "spi-003", Title: "Implement feature", Status: "in_progress", Type: "feature", Priority: 1},
			{ID: "spi-004", Title: "Implement task", Status: "in_progress", Type: "task", Priority: 2},
			{ID: "spi-005", Title: "Implement epic", Status: "in_progress", Type: "epic", Priority: 0},
		},
		Done: []BoardBead{
			{ID: "spi-006", Title: "Done chore", Status: "closed", Type: "chore", Priority: 3},
		},
	}
	return &BoardMode{
		Cols:       cols,
		Width:      120,
		Height:     40,
		Identity:   "test@test.dev",
		SelSection: SectionColumns,
		Snapshot: &BoardSnapshot{
			Columns:     cols,
			DAGProgress: map[string]*DAGProgress{},
			PhaseMap:    map[string]string{},
		},
	}
}

// --- TestBuildActionMenu ---

func TestBuildActionMenu(t *testing.T) {
	t.Run("open bead no wizard", func(t *testing.T) {
		bead := &BoardBead{ID: "spi-001", Status: "open", Type: "task"}
		items := BuildActionMenu(bead, nil)

		expectActions(t, items, []PendingAction{ActionSummon, ActionDefer, ActionClose, ActionGrok, ActionTrace})

		// Verify danger levels.
		for _, item := range items {
			switch item.ActionType {
			case ActionSummon:
				if item.Danger != DangerNone {
					t.Errorf("Summon should be DangerNone, got %d", item.Danger)
				}
			case ActionClose:
				if item.Danger != DangerConfirm {
					t.Errorf("Close should be DangerConfirm, got %d", item.Danger)
				}
			}
		}
	})

	t.Run("in_progress with wizard", func(t *testing.T) {
		bead := &BoardBead{ID: "spi-002", Status: "in_progress", Type: "task"}
		agents := []LocalAgent{{Name: "wizard-spi-002", BeadID: "spi-002"}}
		items := BuildActionMenu(bead, agents)

		expectActions(t, items, []PendingAction{
			ActionLogs, ActionUnsummon, ActionResetSoft, ActionResetHard,
			ActionClose, ActionGrok, ActionTrace,
		})

		// Verify Reset --hard is DangerDestructive.
		for _, item := range items {
			if item.ActionType == ActionResetHard && item.Danger != DangerDestructive {
				t.Errorf("Reset --hard should be DangerDestructive, got %d", item.Danger)
			}
		}
	})

	t.Run("hooked bead shows Resume", func(t *testing.T) {
		bead := &BoardBead{ID: "spi-003", Status: "hooked", Type: "task"}
		items := BuildActionMenu(bead, nil)

		found := false
		for _, item := range items {
			if item.ActionType == ActionResummon {
				found = true
				break
			}
		}
		if !found {
			t.Error("expected Resume action for hooked bead")
		}
	})

	t.Run("in_progress without wizard without needs-human", func(t *testing.T) {
		bead := &BoardBead{ID: "spi-004", Status: "in_progress", Type: "task"}
		items := BuildActionMenu(bead, nil)

		hasSummon := false
		hasResummon := false
		for _, item := range items {
			if item.ActionType == ActionSummon {
				hasSummon = true
			}
			if item.ActionType == ActionResummon {
				hasResummon = true
			}
		}
		if !hasSummon {
			t.Error("expected Summon action")
		}
		if hasResummon {
			t.Error("should not have Resummon without needs-human")
		}
	})

	t.Run("shortcut keys are unique", func(t *testing.T) {
		bead := &BoardBead{ID: "spi-005", Status: "in_progress", Type: "task", Labels: []string{"needs-human"}}
		agents := []LocalAgent{{Name: "wizard-spi-005", BeadID: "spi-005"}}
		items := BuildActionMenu(bead, agents)

		seen := make(map[rune]string)
		for _, item := range items {
			if prev, ok := seen[item.Key]; ok {
				t.Errorf("duplicate shortcut key '%c': %s and %s", item.Key, prev, item.Label)
			}
			seen[item.Key] = item.Label
		}
	})

	t.Run("nil bead returns nil", func(t *testing.T) {
		items := BuildActionMenu(nil, nil)
		if items != nil {
			t.Errorf("expected nil for nil bead, got %v", items)
		}
	})

	t.Run("design bead with needs-human shows approve/reject", func(t *testing.T) {
		bead := &BoardBead{ID: "spi-010", Status: "in_progress", Type: "design", Labels: []string{"needs-human"}}
		items := BuildActionMenu(bead, nil)

		hasApproveDesign := false
		hasRejectDesign := false
		for _, item := range items {
			if item.ActionType == ActionApproveDesign {
				hasApproveDesign = true
				if item.Danger != DangerConfirm {
					t.Errorf("ApproveDesign should be DangerConfirm, got %d", item.Danger)
				}
			}
			if item.ActionType == ActionRejectDesign {
				hasRejectDesign = true
				if item.Danger != DangerNone {
					t.Errorf("RejectDesign should be DangerNone, got %d", item.Danger)
				}
			}
		}
		if !hasApproveDesign {
			t.Error("expected ApproveDesign action for design bead with needs-human")
		}
		if !hasRejectDesign {
			t.Error("expected RejectDesign action for design bead with needs-human")
		}
	})

	t.Run("open design bead with needs-human shows approve/reject", func(t *testing.T) {
		bead := &BoardBead{ID: "spi-011", Status: "open", Type: "design", Labels: []string{"needs-human"}}
		items := BuildActionMenu(bead, nil)

		hasApproveDesign := false
		hasRejectDesign := false
		for _, item := range items {
			if item.ActionType == ActionApproveDesign {
				hasApproveDesign = true
			}
			if item.ActionType == ActionRejectDesign {
				hasRejectDesign = true
			}
		}
		if !hasApproveDesign {
			t.Error("expected ApproveDesign for open design bead with needs-human")
		}
		if !hasRejectDesign {
			t.Error("expected RejectDesign for open design bead with needs-human")
		}
	})

	t.Run("non-design bead with needs-human does not show design actions", func(t *testing.T) {
		bead := &BoardBead{ID: "spi-012", Status: "in_progress", Type: "task", Labels: []string{"needs-human"}}
		items := BuildActionMenu(bead, nil)

		for _, item := range items {
			if item.ActionType == ActionApproveDesign {
				t.Error("non-design bead should not have ApproveDesign action")
			}
			if item.ActionType == ActionRejectDesign {
				t.Error("non-design bead should not have RejectDesign action")
			}
		}
	})

	t.Run("hooked non-design bead shows Resume and Reset", func(t *testing.T) {
		bead := &BoardBead{ID: "spi-014", Status: "hooked", Type: "task"}
		items := BuildActionMenu(bead, nil)

		hasResume := false
		hasReset := false
		for _, item := range items {
			if item.ActionType == ActionResummon {
				hasResume = true
			}
			if item.ActionType == ActionResetSoft {
				hasReset = true
			}
		}
		if !hasResume {
			t.Error("expected Resume action for hooked bead")
		}
		if !hasReset {
			t.Error("expected Reset action for hooked bead")
		}
	})

	t.Run("design bead without needs-human does not show design actions", func(t *testing.T) {
		bead := &BoardBead{ID: "spi-013", Status: "in_progress", Type: "design"}
		items := BuildActionMenu(bead, nil)

		for _, item := range items {
			if item.ActionType == ActionApproveDesign {
				t.Error("design bead without needs-human should not have ApproveDesign action")
			}
			if item.ActionType == ActionRejectDesign {
				t.Error("design bead without needs-human should not have RejectDesign action")
			}
		}
	})

	t.Run("hooked bead shows Resume not Resolve", func(t *testing.T) {
		bead := &BoardBead{ID: "spi-020", Status: "hooked", Type: "task"}
		items := BuildActionMenu(bead, nil)

		hasResume := false
		hasResolve := false
		for _, item := range items {
			if item.ActionType == ActionResummon {
				hasResume = true
			}
			if item.ActionType == ActionResolve {
				hasResolve = true
			}
		}
		if !hasResume {
			t.Error("expected Resume action for hooked bead")
		}
		if hasResolve {
			t.Error("should not have Resolve action for hooked bead")
		}
	})

	t.Run("hooked shows Resume Reset Close", func(t *testing.T) {
		bead := &BoardBead{ID: "spi-021", Status: "hooked", Type: "task"}
		items := BuildActionMenu(bead, nil)

		hasResummon := false
		hasReset := false
		hasClose := false
		for _, item := range items {
			if item.ActionType == ActionResummon {
				hasResummon = true
			}
			if item.ActionType == ActionResetSoft {
				hasReset = true
			}
			if item.ActionType == ActionClose {
				hasClose = true
			}
		}
		if !hasResummon {
			t.Error("expected Resume action for hooked bead")
		}
		if !hasReset {
			t.Error("expected Reset action for hooked bead")
		}
		if !hasClose {
			t.Error("expected Close action for hooked bead")
		}
	})
}

// --- TestBuildAgentActionMenu ---

func TestBuildAgentActionMenu(t *testing.T) {
	t.Run("running agent", func(t *testing.T) {
		agent := AgentInfo{Name: "wizard-main", BeadID: "spi-001", Status: "running"}
		items := BuildAgentActionMenu(agent)

		expectActions(t, items, []PendingAction{
			ActionUnsummon, ActionResetSoft, ActionResetHard, ActionClose,
			ActionGrok, ActionTrace,
		})

		// Verify danger levels.
		for _, item := range items {
			switch item.ActionType {
			case ActionUnsummon:
				if item.Danger != DangerConfirm {
					t.Errorf("Unsummon should be DangerConfirm, got %d", item.Danger)
				}
			case ActionResetHard:
				if item.Danger != DangerDestructive {
					t.Errorf("Reset --hard should be DangerDestructive, got %d", item.Danger)
				}
			}
		}
	})

	t.Run("errored agent", func(t *testing.T) {
		agent := AgentInfo{Name: "wizard-main", BeadID: "spi-002", Status: "errored"}
		items := BuildAgentActionMenu(agent)

		expectActions(t, items, []PendingAction{
			ActionResetSoft, ActionResetHard, ActionResummon, ActionClose,
			ActionGrok, ActionTrace,
		})

		// Errored agents get Resummon instead of Unsummon.
		hasResummon := false
		hasUnsummon := false
		for _, item := range items {
			if item.ActionType == ActionResummon {
				hasResummon = true
			}
			if item.ActionType == ActionUnsummon {
				hasUnsummon = true
			}
		}
		if !hasResummon {
			t.Error("expected Resummon for errored agent")
		}
		if hasUnsummon {
			t.Error("errored agent should not have Unsummon")
		}
	})

	t.Run("idle agent with bead", func(t *testing.T) {
		agent := AgentInfo{Name: "wizard-main", BeadID: "spi-003", Status: "idle"}
		items := BuildAgentActionMenu(agent)

		expectActions(t, items, []PendingAction{
			ActionSummon, ActionResetSoft, ActionClose,
			ActionGrok, ActionTrace,
		})

		// No ResetHard for idle agents.
		for _, item := range items {
			if item.ActionType == ActionResetHard {
				t.Error("idle agent should not have ResetHard")
			}
		}
	})

	t.Run("idle agent without bead", func(t *testing.T) {
		agent := AgentInfo{Name: "wizard-idle", BeadID: "", Status: "idle"}
		items := BuildAgentActionMenu(agent)

		if len(items) != 0 {
			t.Errorf("expected empty menu for idle agent without bead, got %d items", len(items))
		}
	})

	t.Run("shortcut keys are unique per status", func(t *testing.T) {
		statuses := []string{"running", "errored", "idle"}
		for _, status := range statuses {
			t.Run(status, func(t *testing.T) {
				agent := AgentInfo{Name: "wizard-test", BeadID: "spi-test", Status: status}
				items := BuildAgentActionMenu(agent)

				seen := make(map[rune]string)
				for _, item := range items {
					if prev, ok := seen[item.Key]; ok {
						t.Errorf("duplicate shortcut key '%c': %s and %s", item.Key, prev, item.Label)
					}
					seen[item.Key] = item.Label
				}
			})
		}
	})

	t.Run("all items have grok and trace at tail", func(t *testing.T) {
		for _, status := range []string{"running", "errored", "idle"} {
			t.Run(status, func(t *testing.T) {
				agent := AgentInfo{Name: "wizard-test", BeadID: "spi-test", Status: status}
				items := BuildAgentActionMenu(agent)
				n := len(items)
				if n < 2 {
					t.Fatalf("expected at least 2 items, got %d", n)
				}
				if items[n-2].ActionType != ActionGrok {
					t.Errorf("second-to-last item should be Grok, got %d", items[n-2].ActionType)
				}
				if items[n-1].ActionType != ActionTrace {
					t.Errorf("last item should be Trace, got %d", items[n-1].ActionType)
				}
			})
		}
	})
}

// --- TestBoardModeFooterHints ---

func TestBoardModeFooterHints(t *testing.T) {
	t.Run("ViewBoard default", func(t *testing.T) {
		m := makeBoardMode()
		m.ViewMode = ViewBoard
		hints := m.FooterHints()
		for _, want := range []string{"summon", "defer", "close", "reset", "actions", "search"} {
			if !strings.Contains(hints, want) {
				t.Errorf("ViewBoard hints missing %q, got %q", want, hints)
			}
		}
	})

	t.Run("ViewAlerts", func(t *testing.T) {
		m := makeBoardMode()
		m.ViewMode = ViewAlerts
		hints := m.FooterHints()
		for _, want := range []string{"summon", "close", "defer", "actions", "inspect"} {
			if !strings.Contains(hints, want) {
				t.Errorf("ViewAlerts hints missing %q, got %q", want, hints)
			}
		}
	})

	t.Run("ViewLower", func(t *testing.T) {
		m := makeBoardMode()
		m.ViewMode = ViewLower
		hints := m.FooterHints()
		for _, want := range []string{"v=view", "o resolve", "reset", "resummon", "close", "actions", "inspect"} {
			if !strings.Contains(hints, want) {
				t.Errorf("ViewLower hints missing %q, got %q", want, hints)
			}
		}
	})

	t.Run("ConfirmOpen overlay", func(t *testing.T) {
		m := makeBoardMode()
		m.ConfirmOpen = true
		hints := m.FooterHints()
		if !strings.Contains(hints, "confirm") || !strings.Contains(hints, "cancel") {
			t.Errorf("ConfirmOpen hints should show confirm/cancel, got %q", hints)
		}
	})

	t.Run("TermOpen overlay", func(t *testing.T) {
		m := makeBoardMode()
		m.TermOpen = true
		hints := m.FooterHints()
		if !strings.Contains(hints, "scroll") || !strings.Contains(hints, "close") {
			t.Errorf("TermOpen hints should show scroll/close, got %q", hints)
		}
	})

	t.Run("ActionMenuOpen overlay", func(t *testing.T) {
		m := makeBoardMode()
		m.ActionMenuOpen = true
		hints := m.FooterHints()
		if !strings.Contains(hints, "navigate") || !strings.Contains(hints, "select") {
			t.Errorf("ActionMenuOpen hints should show navigate/select, got %q", hints)
		}
	})

	t.Run("SearchActive overlay", func(t *testing.T) {
		m := makeBoardMode()
		m.SearchActive = true
		hints := m.FooterHints()
		if !strings.Contains(hints, "filter") || !strings.Contains(hints, "accept") {
			t.Errorf("SearchActive hints should show filter/accept, got %q", hints)
		}
	})

	t.Run("Cmdline active overlay", func(t *testing.T) {
		m := makeBoardMode()
		m.Cmdline.Active = true
		hints := m.FooterHints()
		if !strings.Contains(hints, "execute") || !strings.Contains(hints, "cancel") {
			t.Errorf("Cmdline hints should show execute/cancel, got %q", hints)
		}
	})

	t.Run("Inspecting overlay", func(t *testing.T) {
		m := makeBoardMode()
		m.Inspecting = true
		hints := m.FooterHints()
		if !strings.Contains(hints, "scroll") || !strings.Contains(hints, "close") {
			t.Errorf("Inspecting hints should show scroll/close, got %q", hints)
		}
	})

	t.Run("overlay takes priority over ViewMode", func(t *testing.T) {
		m := makeBoardMode()
		m.ViewMode = ViewLower
		m.ConfirmOpen = true
		hints := m.FooterHints()
		// Should show confirm overlay hints, not ViewLower hints.
		if strings.Contains(hints, "resolve") {
			t.Errorf("overlay should take priority, but got ViewLower hints: %q", hints)
		}
		if !strings.Contains(hints, "confirm") {
			t.Errorf("expected confirm overlay hints, got %q", hints)
		}
	})
}

func expectActions(t *testing.T, items []MenuAction, expected []PendingAction) {
	t.Helper()
	if len(items) != len(expected) {
		got := make([]PendingAction, len(items))
		for i, item := range items {
			got[i] = item.ActionType
		}
		t.Fatalf("expected %d actions %v, got %d: %v", len(expected), expected, len(items), got)
	}
	for i, want := range expected {
		if items[i].ActionType != want {
			t.Errorf("action[%d]: expected %d, got %d (%s)", i, want, items[i].ActionType, items[i].Label)
		}
	}
}

// --- TestSearchFiltering ---

func TestSearchFiltering(t *testing.T) {
	t.Run("matchesSearch by ID", func(t *testing.T) {
		b := BoardBead{ID: "spi-abc", Title: "Some title"}
		if !matchesSearch(b, "abc") {
			t.Error("expected match on ID substring")
		}
	})

	t.Run("matchesSearch by title case insensitive", func(t *testing.T) {
		b := BoardBead{ID: "spi-001", Title: "Fix Authentication Bug"}
		if !matchesSearch(b, "authentication") {
			t.Error("expected case-insensitive match on title")
		}
	})

	t.Run("matchesSearch by label", func(t *testing.T) {
		b := BoardBead{ID: "spi-001", Title: "A task", Labels: []string{"team:alpha", "urgent"}}
		if !matchesSearch(b, "alpha") {
			t.Error("expected match on label substring")
		}
	})

	t.Run("matchesSearch by type", func(t *testing.T) {
		b := BoardBead{ID: "spi-001", Title: "A task", Type: "epic"}
		if !matchesSearch(b, "epic") {
			t.Error("expected match on type")
		}
	})

	t.Run("matchesSearch no match", func(t *testing.T) {
		b := BoardBead{ID: "spi-001", Title: "A task", Type: "task", Labels: []string{"team:alpha"}}
		if matchesSearch(b, "zzzznotfound") {
			t.Error("expected no match")
		}
	})

	t.Run("FilterColumns end to end", func(t *testing.T) {
		cols := Columns{
			Ready: []BoardBead{
				{ID: "spi-001", Title: "Alpha task"},
				{ID: "spi-002", Title: "Beta task"},
			},
			Implement: []BoardBead{
				{ID: "spi-003", Title: "Alpha feature"},
			},
			Done: []BoardBead{
				{ID: "spi-004", Title: "Gamma chore"},
			},
		}
		filtered := FilterColumns(cols, "alpha")
		if len(filtered.Ready) != 1 || filtered.Ready[0].ID != "spi-001" {
			t.Errorf("expected 1 Ready bead matching alpha, got %d", len(filtered.Ready))
		}
		if len(filtered.Implement) != 1 || filtered.Implement[0].ID != "spi-003" {
			t.Errorf("expected 1 Implement bead matching alpha, got %d", len(filtered.Implement))
		}
		if len(filtered.Done) != 0 {
			t.Errorf("expected 0 Done beads matching alpha, got %d", len(filtered.Done))
		}
	})

	t.Run("FilterColumns empty query returns unchanged", func(t *testing.T) {
		cols := Columns{
			Ready: []BoardBead{{ID: "spi-001", Title: "A task"}},
		}
		filtered := FilterColumns(cols, "")
		if len(filtered.Ready) != 1 {
			t.Error("empty query should return unchanged columns")
		}
	})
}

// --- TestSearchKeyDispatch ---

func TestSearchKeyDispatch(t *testing.T) {
	t.Run("slash activates search", func(t *testing.T) {
		m := makeBoardMode()
		m = updateBoardMode(m, keyMsg('/'))
		if !m.SearchActive {
			t.Error("expected SearchActive after /")
		}
		if m.SearchQuery != "" {
			t.Error("expected empty SearchQuery on activation")
		}
	})

	t.Run("typing accumulates query", func(t *testing.T) {
		m := makeBoardMode()
		m = updateBoardMode(m, keyMsg('/'))
		m = updateBoardMode(m, keyMsg('a'))
		m = updateBoardMode(m, keyMsg('b'))
		m = updateBoardMode(m, keyMsg('c'))
		if m.SearchQuery != "abc" {
			t.Errorf("expected query 'abc', got %q", m.SearchQuery)
		}
	})

	t.Run("backspace removes last rune", func(t *testing.T) {
		m := makeBoardMode()
		m = updateBoardMode(m, keyMsg('/'))
		m = updateBoardMode(m, keyMsg('a'))
		m = updateBoardMode(m, keyMsg('b'))
		m = updateBoardMode(m, keyMsgType(tea.KeyBackspace))
		if m.SearchQuery != "a" {
			t.Errorf("expected query 'a' after backspace, got %q", m.SearchQuery)
		}
	})

	t.Run("ctrl+u clears query", func(t *testing.T) {
		m := makeBoardMode()
		m = updateBoardMode(m, keyMsg('/'))
		m = updateBoardMode(m, keyMsg('a'))
		m = updateBoardMode(m, keyMsg('b'))
		m = updateBoardMode(m, keyMsgStr("ctrl+u"))
		if m.SearchQuery != "" {
			t.Errorf("expected empty query after ctrl+u, got %q", m.SearchQuery)
		}
	})

	t.Run("esc exits search and clears query", func(t *testing.T) {
		m := makeBoardMode()
		m = updateBoardMode(m, keyMsg('/'))
		m = updateBoardMode(m, keyMsg('x'))
		m = updateBoardMode(m, keyMsgType(tea.KeyEsc))
		if m.SearchActive {
			t.Error("expected SearchActive=false after Esc")
		}
		if m.SearchQuery != "" {
			t.Errorf("expected empty query after Esc, got %q", m.SearchQuery)
		}
	})

	t.Run("enter exits search preserves query", func(t *testing.T) {
		m := makeBoardMode()
		m = updateBoardMode(m, keyMsg('/'))
		m = updateBoardMode(m, keyMsg('t'))
		m = updateBoardMode(m, keyMsg('e'))
		m = updateBoardMode(m, keyMsgType(tea.KeyEnter))
		if m.SearchActive {
			t.Error("expected SearchActive=false after Enter")
		}
		if m.SearchQuery != "te" {
			t.Errorf("expected query 'te' preserved after Enter, got %q", m.SearchQuery)
		}
	})

	t.Run("query change resets selection", func(t *testing.T) {
		m := makeBoardMode()
		m.SelCard = 2
		m.ColScroll = 1
		m = updateBoardMode(m, keyMsg('/'))
		m = updateBoardMode(m, keyMsg('x'))
		if m.SelCard != 0 {
			t.Errorf("expected SelCard=0 after query change, got %d", m.SelCard)
		}
		if m.ColScroll != 0 {
			t.Errorf("expected ColScroll=0 after query change, got %d", m.ColScroll)
		}
	})
}

// --- TestKeybindingDispatch ---

func TestKeybindingDispatch(t *testing.T) {
	t.Run("j moves SelCard down", func(t *testing.T) {
		m := makeBoardMode()
		// Start on first column (Ready) with 2 beads.
		m.SelCol = 0
		m.SelCard = 0
		m = updateBoardMode(m, keyMsg('j'))
		if m.SelCard != 1 {
			t.Errorf("expected SelCard=1 after j, got %d", m.SelCard)
		}
	})

	t.Run("k moves SelCard up", func(t *testing.T) {
		m := makeBoardMode()
		m.SelCol = 0
		m.SelCard = 1
		m = updateBoardMode(m, keyMsg('k'))
		if m.SelCard != 0 {
			t.Errorf("expected SelCard=0 after k, got %d", m.SelCard)
		}
	})

	t.Run("h moves SelCol left", func(t *testing.T) {
		m := makeBoardMode()
		m.SelCol = 1
		m = updateBoardMode(m, keyMsg('h'))
		if m.SelCol != 0 {
			t.Errorf("expected SelCol=0 after h, got %d", m.SelCol)
		}
	})

	t.Run("l moves SelCol right", func(t *testing.T) {
		m := makeBoardMode()
		m.SelCol = 0
		m = updateBoardMode(m, keyMsg('l'))
		if m.SelCol != 1 {
			t.Errorf("expected SelCol=1 after l, got %d", m.SelCol)
		}
	})

	t.Run("gg jumps to top", func(t *testing.T) {
		m := makeBoardMode()
		m.SelCol = 1 // Implement column with 3 beads
		m.SelCard = 2
		m.ColScroll = 1
		m = updateBoardMode(m, keyMsg('g'))
		if !m.PendingG {
			t.Error("expected PendingG after first g")
		}
		m = updateBoardMode(m, keyMsg('g'))
		if m.SelCard != 0 {
			t.Errorf("expected SelCard=0 after gg, got %d", m.SelCard)
		}
		if m.ColScroll != 0 {
			t.Errorf("expected ColScroll=0 after gg, got %d", m.ColScroll)
		}
		if m.PendingG {
			t.Error("PendingG should be cleared after gg")
		}
	})

	t.Run("g then other key clears PendingG", func(t *testing.T) {
		m := makeBoardMode()
		m.SelCol = 1
		m.SelCard = 2
		m = updateBoardMode(m, keyMsg('g'))
		if !m.PendingG {
			t.Error("expected PendingG after first g")
		}
		m = updateBoardMode(m, keyMsg('j'))
		if m.PendingG {
			t.Error("PendingG should be cleared after non-g key")
		}
	})

	t.Run("G jumps to bottom", func(t *testing.T) {
		m := makeBoardMode()
		m.SelCol = 1 // Implement column with 3 beads
		m.SelCard = 0
		m = updateBoardMode(m, keyMsg('G'))
		if m.SelCard != 2 {
			t.Errorf("expected SelCard=2 (last card) after G, got %d", m.SelCard)
		}
	})

	t.Run("t cycles TypeScope", func(t *testing.T) {
		m := makeBoardMode()
		if m.TypeScope != TypeAll {
			t.Fatalf("expected initial TypeScope=TypeAll, got %d", m.TypeScope)
		}
		m = updateBoardMode(m, keyMsg('t'))
		if m.TypeScope != TypeTask {
			t.Errorf("expected TypeScope=TypeTask after first t, got %d", m.TypeScope)
		}
		m = updateBoardMode(m, keyMsg('t'))
		if m.TypeScope != TypeBug {
			t.Errorf("expected TypeScope=TypeBug after second t, got %d", m.TypeScope)
		}
	})

	t.Run("H toggles ShowAllCols", func(t *testing.T) {
		m := makeBoardMode()
		if m.ShowAllCols {
			t.Fatal("expected ShowAllCols=false initially")
		}
		m = updateBoardMode(m, keyMsg('H'))
		if !m.ShowAllCols {
			t.Error("expected ShowAllCols=true after H")
		}
		m = updateBoardMode(m, keyMsg('H'))
		if m.ShowAllCols {
			t.Error("expected ShowAllCols=false after second H")
		}
	})

	t.Run("a opens action menu", func(t *testing.T) {
		m := makeBoardMode()
		m.SelCol = 0
		m.SelCard = 0
		m = updateBoardMode(m, keyMsg('a'))
		if !m.ActionMenuOpen {
			t.Error("expected ActionMenuOpen after 'a'")
		}
		if len(m.ActionMenuItems) == 0 {
			t.Error("expected non-empty ActionMenuItems")
		}
		if m.ActionMenuBeadID != "spi-001" {
			t.Errorf("expected ActionMenuBeadID=spi-001, got %s", m.ActionMenuBeadID)
		}
	})

	t.Run("q quits", func(t *testing.T) {
		m := makeBoardMode()
		m = updateBoardMode(m, keyMsg('q'))
		if !m.Quitting {
			t.Error("expected Quitting after q")
		}
	})

	t.Run("q clears search query first", func(t *testing.T) {
		m := makeBoardMode()
		m.SearchQuery = "test"
		m = updateBoardMode(m, keyMsg('q'))
		if m.Quitting {
			t.Error("q should clear search query before quitting")
		}
		if m.SearchQuery != "" {
			t.Errorf("expected empty SearchQuery after q, got %q", m.SearchQuery)
		}
	})
}

// --- TestActionMenuNavigation ---

func TestActionMenuNavigation(t *testing.T) {
	openMenu := func() *BoardMode {
		m := makeBoardMode()
		m.SelCol = 0
		m.SelCard = 0
		m = updateBoardMode(m, keyMsg('a'))
		return m
	}

	t.Run("j moves cursor down", func(t *testing.T) {
		m := openMenu()
		initial := m.ActionMenuCursor
		m = updateBoardMode(m, keyMsg('j'))
		if m.ActionMenuCursor != initial+1 {
			t.Errorf("expected cursor %d, got %d", initial+1, m.ActionMenuCursor)
		}
	})

	t.Run("k moves cursor up", func(t *testing.T) {
		m := openMenu()
		m.ActionMenuCursor = 1
		m = updateBoardMode(m, keyMsg('k'))
		if m.ActionMenuCursor != 0 {
			t.Errorf("expected cursor 0, got %d", m.ActionMenuCursor)
		}
	})

	t.Run("k at top stays at 0", func(t *testing.T) {
		m := openMenu()
		m.ActionMenuCursor = 0
		m = updateBoardMode(m, keyMsg('k'))
		if m.ActionMenuCursor != 0 {
			t.Errorf("expected cursor 0, got %d", m.ActionMenuCursor)
		}
	})

	t.Run("j at bottom stays at last", func(t *testing.T) {
		m := openMenu()
		last := len(m.ActionMenuItems) - 1
		m.ActionMenuCursor = last
		m = updateBoardMode(m, keyMsg('j'))
		if m.ActionMenuCursor != last {
			t.Errorf("expected cursor %d, got %d", last, m.ActionMenuCursor)
		}
	})

	t.Run("esc closes menu", func(t *testing.T) {
		m := openMenu()
		m = updateBoardMode(m, keyMsgType(tea.KeyEsc))
		if m.ActionMenuOpen {
			t.Error("expected ActionMenuOpen=false after Esc")
		}
	})

	t.Run("enter selects item", func(t *testing.T) {
		m := openMenu()
		m.ActionMenuCursor = 0
		expected := m.ActionMenuItems[0].ActionType
		m = updateBoardMode(m, keyMsgType(tea.KeyEnter))
		if m.ActionMenuOpen {
			t.Error("expected menu closed after Enter")
		}
		// The action should be dispatched (either as PendingAction or inline).
		if isInlineAction(expected) {
			// Inline actions set ActionRunning (if InlineActionFn is set) or fallback to PendingAction.
			if m.PendingAction != expected && !m.ActionRunning {
				// Without InlineActionFn, it falls back to PendingAction.
				if m.PendingAction != expected {
					t.Errorf("expected PendingAction=%d, got %d", expected, m.PendingAction)
				}
			}
		} else {
			if m.PendingAction != expected {
				t.Errorf("expected PendingAction=%d, got %d", expected, m.PendingAction)
			}
		}
	})

	t.Run("shortcut key selects item", func(t *testing.T) {
		m := openMenu()
		// Find an item with a known shortcut — the Grok action with key 'g'.
		var grokItem MenuAction
		found := false
		for _, item := range m.ActionMenuItems {
			if item.ActionType == ActionGrok {
				grokItem = item
				found = true
				break
			}
		}
		if !found {
			t.Fatal("expected Grok action in menu")
		}
		m = updateBoardMode(m, keyMsg(grokItem.Key))
		if m.ActionMenuOpen {
			t.Error("expected menu closed after shortcut key")
		}
	})

	t.Run("menu absorbs board keys", func(t *testing.T) {
		m := openMenu()
		origSelCard := m.SelCard
		m = updateBoardMode(m, keyMsg('j'))
		// j should move menu cursor, not SelCard.
		if m.SelCard != origSelCard {
			t.Error("board key leaked through action menu")
		}
	})
}

// --- TestInspectorNavigation ---

func TestInspectorNavigation(t *testing.T) {
	makeInspecting := func() *BoardMode {
		m := makeBoardMode()
		m.Inspecting = true
		m.InspectorTab = 0
		m.InspectorScroll = 0
		m.InspectorData = &InspectorData{
			Bead: BoardBead{
				ID:          "spi-001",
				Title:       "Test bead",
				Status:      "open",
				Type:        "task",
				Description: "Line1\nLine2\nLine3\nLine4\nLine5\nLine6\nLine7\nLine8\nLine9\nLine10\nLine11\nLine12\nLine13\nLine14\nLine15\nLine16\nLine17\nLine18\nLine19\nLine20\nLine21\nLine22\nLine23\nLine24\nLine25\nLine26\nLine27\nLine28\nLine29\nLine30\nLine31\nLine32\nLine33\nLine34\nLine35\nLine36\nLine37\nLine38\nLine39\nLine40",
			},
		}
		return m
	}

	t.Run("tab toggles InspectorTab", func(t *testing.T) {
		m := makeInspecting()
		m = updateBoardMode(m, keyMsgType(tea.KeyTab))
		if m.InspectorTab != 1 {
			t.Errorf("expected InspectorTab=1 after Tab, got %d", m.InspectorTab)
		}
		m = updateBoardMode(m, keyMsgType(tea.KeyTab))
		if m.InspectorTab != 0 {
			t.Errorf("expected InspectorTab=0 after second Tab, got %d", m.InspectorTab)
		}
	})

	t.Run("shift+tab toggles in reverse", func(t *testing.T) {
		m := makeInspecting()
		m.InspectorTab = 0
		m = updateBoardMode(m, keyMsgStr("shift+tab"))
		if m.InspectorTab != 1 {
			t.Errorf("expected InspectorTab=1 after Shift+Tab from 0, got %d", m.InspectorTab)
		}
	})

	t.Run("tab resets scroll", func(t *testing.T) {
		m := makeInspecting()
		m.InspectorScroll = 5
		m = updateBoardMode(m, keyMsgType(tea.KeyTab))
		if m.InspectorScroll != 0 {
			t.Errorf("expected InspectorScroll=0 after Tab, got %d", m.InspectorScroll)
		}
	})

	t.Run("j increments scroll", func(t *testing.T) {
		m := makeInspecting()
		m = updateBoardMode(m, keyMsg('j'))
		if m.InspectorScroll != 1 {
			t.Errorf("expected InspectorScroll=1 after j, got %d", m.InspectorScroll)
		}
	})

	t.Run("k decrements scroll", func(t *testing.T) {
		m := makeInspecting()
		m.InspectorScroll = 3
		m = updateBoardMode(m, keyMsg('k'))
		if m.InspectorScroll != 2 {
			t.Errorf("expected InspectorScroll=2 after k, got %d", m.InspectorScroll)
		}
	})

	t.Run("k at 0 stays at 0", func(t *testing.T) {
		m := makeInspecting()
		m.InspectorScroll = 0
		m = updateBoardMode(m, keyMsg('k'))
		if m.InspectorScroll != 0 {
			t.Errorf("expected InspectorScroll=0, got %d", m.InspectorScroll)
		}
	})

	t.Run("g jumps to top", func(t *testing.T) {
		m := makeInspecting()
		m.InspectorScroll = 10
		m = updateBoardMode(m, keyMsg('g'))
		if m.InspectorScroll != 0 {
			t.Errorf("expected InspectorScroll=0 after g, got %d", m.InspectorScroll)
		}
	})

	t.Run("G jumps to bottom", func(t *testing.T) {
		m := makeInspecting()
		m = updateBoardMode(m, keyMsg('G'))
		if m.InspectorScroll <= 0 {
			t.Error("expected InspectorScroll > 0 after G with content")
		}
	})

	t.Run("esc exits inspector", func(t *testing.T) {
		m := makeInspecting()
		m = updateBoardMode(m, keyMsgType(tea.KeyEsc))
		if m.Inspecting {
			t.Error("expected Inspecting=false after Esc")
		}
	})
}

// --- TestColumnScrolling ---

func TestColumnScrolling(t *testing.T) {
	// Create a model with a tall column but short terminal.
	makeScrollBoardMode := func() *BoardMode {
		beads := make([]BoardBead, 20)
		for i := range beads {
			beads[i] = BoardBead{
				ID:     "spi-" + string(rune('a'+i%26)),
				Title:  "Bead " + string(rune('A'+i%26)),
				Status: "open",
				Type:   "task",
			}
		}
		// Use unique IDs to avoid issues.
		for i := range beads {
			beads[i].ID = fmt.Sprintf("spi-%03d", i)
			beads[i].Title = fmt.Sprintf("Bead %03d", i)
		}
		cols := Columns{
			Ready: beads,
		}
		return &BoardMode{
			Cols:       cols,
			Width:      120,
			Height:     20, // Short enough that MaxCards < 20
			Identity:   "test@test.dev",
			SelSection: SectionColumns,
			SelCol:     0,
			SelCard:    0,
			Snapshot: &BoardSnapshot{
				Columns:     cols,
				DAGProgress: map[string]*DAGProgress{},
				PhaseMap:    map[string]string{},
			},
		}
	}

	t.Run("j past viewport advances ColScroll", func(t *testing.T) {
		m := makeScrollBoardMode()
		maxCards := m.colMaxCards()
		// Navigate down past the visible window.
		for i := 0; i < maxCards+2; i++ {
			m = updateBoardMode(m, keyMsg('j'))
		}
		if m.ColScroll == 0 {
			t.Error("expected ColScroll > 0 after navigating past viewport")
		}
	})

	t.Run("k back up retreats ColScroll", func(t *testing.T) {
		m := makeScrollBoardMode()
		maxCards := m.colMaxCards()
		// Navigate down past viewport.
		for i := 0; i < maxCards+2; i++ {
			m = updateBoardMode(m, keyMsg('j'))
		}
		scrollAfterDown := m.ColScroll
		// Navigate back up.
		for i := 0; i < 3; i++ {
			m = updateBoardMode(m, keyMsg('k'))
		}
		if m.ColScroll >= scrollAfterDown {
			t.Error("expected ColScroll to decrease after k")
		}
	})

	t.Run("ensureCardVisible adjusts scroll", func(t *testing.T) {
		m := makeScrollBoardMode()
		maxCards := m.colMaxCards()
		m.SelCard = maxCards + 3
		m.ColScroll = 0
		m.ensureCardVisible(maxCards)
		if m.ColScroll == 0 {
			t.Error("expected ColScroll > 0 after ensureCardVisible with card past viewport")
		}
		// SelCard should be visible: ColScroll <= SelCard < ColScroll+maxCards
		if m.SelCard < m.ColScroll || m.SelCard >= m.ColScroll+maxCards {
			t.Errorf("SelCard %d not visible in [%d, %d)", m.SelCard, m.ColScroll, m.ColScroll+maxCards)
		}
	})

	t.Run("ensureCardVisible scrolls up", func(t *testing.T) {
		m := makeScrollBoardMode()
		maxCards := m.colMaxCards()
		m.ColScroll = 10
		m.SelCard = 5
		m.ensureCardVisible(maxCards)
		if m.ColScroll != 5 {
			t.Errorf("expected ColScroll=5, got %d", m.ColScroll)
		}
	})

	t.Run("ClampSelection wraps negative", func(t *testing.T) {
		m := makeScrollBoardMode()
		m.SelCard = -1
		m.ClampSelection()
		active := m.DisplayColumns()
		if m.SelCol >= 0 && m.SelCol < len(active) {
			lastCard := len(active[m.SelCol].Beads) - 1
			if m.SelCard != lastCard {
				t.Errorf("expected SelCard=%d (wrapped from -1), got %d", lastCard, m.SelCard)
			}
		}
	})

	t.Run("ClampSelection caps overflow", func(t *testing.T) {
		m := makeScrollBoardMode()
		m.SelCard = 999
		m.ClampSelection()
		active := m.DisplayColumns()
		if m.SelCol >= 0 && m.SelCol < len(active) {
			n := len(active[m.SelCol].Beads)
			if m.SelCard >= n {
				t.Errorf("expected SelCard < %d after clamp, got %d", n, m.SelCard)
			}
		}
	})
}

// --- TestTypeScopeCycle ---

func TestTypeScopeCycle(t *testing.T) {
	scope := TypeAll
	expected := []TypeScope{TypeTask, TypeBug, TypeEpic, TypeDesign, TypeDecision, TypeOther, TypeAll}
	for i, want := range expected {
		scope = scope.Next()
		if scope != want {
			t.Errorf("step %d: expected %d, got %d", i, want, scope)
		}
	}
}

// --- TestSkipBead ---

func TestSkipBead(t *testing.T) {
	tests := []struct {
		name   string
		bead   BoardBead
		expect bool
	}{
		{
			name:   "review-substep label is skipped",
			bead:   BoardBead{ID: "spi-100", Title: "Discard branch", Labels: []string{"review-substep"}},
			expect: true,
		},
		{
			name:   "alert:merge-failure label is skipped",
			bead:   BoardBead{ID: "spi-101", Title: "Merge failure alert", Labels: []string{"alert:merge-failure"}},
			expect: true,
		},
		{
			name:   "bare alert label is skipped",
			bead:   BoardBead{ID: "spi-102", Title: "Some alert", Labels: []string{"alert"}},
			expect: true,
		},
		{
			name:   "msg label is skipped",
			bead:   BoardBead{ID: "spi-103", Title: "A message", Type: "message", Labels: []string{"msg"}},
			expect: true,
		},
		{
			name:   "workflow-step label is skipped",
			bead:   BoardBead{ID: "spi-104", Title: "step:implement", Type: "step", Labels: []string{"workflow-step", "step:implement"}},
			expect: true,
		},
		{
			name:   "attempt bead is skipped",
			bead:   BoardBead{ID: "spi-105", Title: "attempt: wizard-spi-xxx", Type: "attempt", Labels: []string{"attempt"}},
			expect: true,
		},
		{
			name:   "normal task bead is not skipped",
			bead:   BoardBead{ID: "spi-106", Title: "Fix auth bug", Type: "task", Labels: []string{"team:alpha"}},
			expect: false,
		},
		{
			name:   "bead with no labels is not skipped",
			bead:   BoardBead{ID: "spi-107", Title: "Simple task", Type: "task"},
			expect: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := skipBead(tc.bead)
			if got != tc.expect {
				t.Errorf("skipBead(%s) = %v, want %v", tc.bead.ID, got, tc.expect)
			}
		})
	}
}

// --- TestCategorizeFiltersAlerts ---

func TestCategorizeFiltersAlerts(t *testing.T) {
	t.Run("alert beads with non-open status do not leak into phase columns", func(t *testing.T) {
		openBeads := []BoardBead{
			{ID: "spi-200", Title: "Normal task", Status: "open", Type: "task"},
			{ID: "spi-201", Title: "In-progress alert", Status: "in_progress", Type: "task", Labels: []string{"alert:merge-failure"}},
		}
		cols := CategorizeColumnsFromStore(openBeads, nil, nil, "test@test.dev")
		if len(cols.Backlog) != 1 || cols.Backlog[0].ID != "spi-200" {
			t.Errorf("expected only spi-200 in Backlog, got %v", cols.Backlog)
		}
		if len(cols.Alerts) != 0 {
			t.Errorf("expected no alerts (status is in_progress, not open), got %v", cols.Alerts)
		}
	})

	t.Run("open alert beads route to Alerts column", func(t *testing.T) {
		openBeads := []BoardBead{
			{ID: "spi-300", Title: "Open alert", Status: "open", Type: "task", Labels: []string{"alert:build-failure"}},
		}
		cols := CategorizeColumnsFromStore(openBeads, nil, nil, "test@test.dev")
		if len(cols.Alerts) != 1 || cols.Alerts[0].ID != "spi-300" {
			t.Errorf("expected spi-300 in Alerts, got %v", cols.Alerts)
		}
		if len(cols.Ready) != 0 {
			t.Errorf("expected no Ready beads, got %v", cols.Ready)
		}
	})

	t.Run("review-substep beads are filtered from all columns", func(t *testing.T) {
		openBeads := []BoardBead{
			{ID: "spi-400", Title: "Arbiter", Status: "open", Type: "task", Labels: []string{"review-substep"}},
			{ID: "spi-401", Title: "Real task", Status: "open", Type: "task"},
		}
		cols := CategorizeColumnsFromStore(openBeads, nil, nil, "test@test.dev")
		if len(cols.Backlog) != 1 || cols.Backlog[0].ID != "spi-401" {
			t.Errorf("expected only spi-401 in Backlog, got %v", cols.Backlog)
		}
	})
}

// --- TestCalcHeightBudget ---

func TestCalcHeightBudget(t *testing.T) {
	t.Run("zero height returns permissive defaults", func(t *testing.T) {
		b := CalcHeightBudget(0, 0, 0, 0, 0, 3, 0)
		if b.MaxCards != 99 {
			t.Errorf("expected MaxCards=99 for zero height, got %d", b.MaxCards)
		}
	})

	t.Run("positive height computes budget", func(t *testing.T) {
		b := CalcHeightBudget(40, 0, 0, 0, 0, 3, 0)
		if b.MaxCards <= 0 {
			t.Errorf("expected MaxCards > 0, got %d", b.MaxCards)
		}
	})

	t.Run("very small height still works", func(t *testing.T) {
		b := CalcHeightBudget(10, 0, 0, 0, 0, 1, 0)
		if b.MaxCards < 1 {
			t.Errorf("expected MaxCards >= 1, got %d", b.MaxCards)
		}
	})
}

// --- TestTruncateTitle ---

func TestTruncateTitle(t *testing.T) {
	tests := []struct {
		name   string
		title  string
		maxLen int
		want   string
	}{
		{name: "shorter than max", title: "hello", maxLen: 10, want: "hello"},
		{name: "exactly at max", title: "hello", maxLen: 5, want: "hello"},
		{name: "longer than max", title: "hello world", maxLen: 8, want: "hello w…"},
		{name: "maxLen 0", title: "hello", maxLen: 0, want: "…"},
		{name: "maxLen 1", title: "hello", maxLen: 1, want: "…"},
		{name: "empty title", title: "", maxLen: 10, want: ""},
		{name: "multi-byte rune boundary", title: "café résumé", maxLen: 5, want: "café…"},
		{name: "emoji boundary", title: "🎉🎊🎈🎁", maxLen: 3, want: "🎉🎊…"},
		{name: "all emoji shorter", title: "🎉🎊", maxLen: 5, want: "🎉🎊"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateTitle(tc.title, tc.maxLen)
			if got != tc.want {
				t.Errorf("truncateTitle(%q, %d) = %q, want %q", tc.title, tc.maxLen, got, tc.want)
			}
		})
	}
}

// --- TestConfirmPromptForAction ---

func TestConfirmPromptForAction(t *testing.T) {
	tests := []struct {
		name   string
		action PendingAction
		beadID string
		title  string
		want   string
	}{
		{
			name:   "close with empty title shows ID only",
			action: ActionClose,
			beadID: "spi-001",
			title:  "",
			want:   "Close spi-001?",
		},
		{
			name:   "close with title shows ID and title",
			action: ActionClose,
			beadID: "spi-001",
			title:  "Fix auth bug",
			want:   "Close spi-001: Fix auth bug?",
		},
		{
			name:   "unsummon with title",
			action: ActionUnsummon,
			beadID: "spi-002",
			title:  "Some task",
			want:   "Dismiss wizard for spi-002: Some task?",
		},
		{
			name:   "reset soft with empty title",
			action: ActionResetSoft,
			beadID: "spi-003",
			title:  "",
			want:   "Reset spi-003?",
		},
		{
			name:   "reset hard with title",
			action: ActionResetHard,
			beadID: "spi-004",
			title:  "Important task",
			want:   "Hard reset spi-004: Important task? This is destructive.",
		},
		{
			name:   "long title is truncated",
			action: ActionClose,
			beadID: "spi-005",
			title:  "This is a very long title that should definitely be truncated to fit the dialog",
			want:   "Close spi-005: This is a very long title that should definitely …?",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := confirmPromptForAction(tc.action, tc.beadID, tc.title)
			if got != tc.want {
				t.Errorf("confirmPromptForAction(%d, %q, %q) = %q, want %q", tc.action, tc.beadID, tc.title, got, tc.want)
			}
		})
	}
}

// --- TestHookedBeadCategorization ---

func TestHookedBeadCategorization(t *testing.T) {
	t.Run("hooked bead routes to Hooked not Ready", func(t *testing.T) {
		open := []BoardBead{
			{ID: "spi-hk", Title: "Waiting for approval", Status: "hooked", Type: "task", Priority: 1},
			{ID: "spi-ok", Title: "Normal task", Status: "open", Type: "task", Priority: 2},
		}
		cols := CategorizeColumnsFromStore(open, nil, nil, "test@test.dev")
		if len(cols.Hooked) != 1 || cols.Hooked[0].ID != "spi-hk" {
			t.Errorf("expected spi-hk in Hooked, got: %v", cols.Hooked)
		}
		if len(cols.Backlog) != 1 || cols.Backlog[0].ID != "spi-ok" {
			t.Errorf("expected spi-ok in Backlog, got: %v", cols.Backlog)
		}
	})

	t.Run("non-hooked bead is NOT routed to Hooked", func(t *testing.T) {
		// Design approval gate: needs-human alone should stay in normal phase routing.
		open := []BoardBead{
			{ID: "spi-design", Title: "Design review", Status: "in_progress", Type: "design", Priority: 1,
				Labels: []string{"needs-human", "phase:design"}},
		}
		phaseMap := map[string]string{"spi-design": "design"}
		cols := CategorizeWithPhases(open, nil, nil, phaseMap, "test@test.dev")
		if len(cols.Hooked) != 0 {
			t.Errorf("design bead with needs-human should NOT be in Hooked, got: %v", cols.Hooked)
		}
		if len(cols.Design) != 1 {
			t.Errorf("design bead should be in Design column, got: %v", cols.Design)
		}
	})

	t.Run("hooked takes priority over phase routing", func(t *testing.T) {
		open := []BoardBead{
			{ID: "spi-stuck", Title: "Stuck in implement", Status: "hooked", Type: "task", Priority: 1,
				Labels: []string{"phase:implement"}},
		}
		phaseMap := map[string]string{"spi-stuck": "implement"}
		cols := CategorizeWithPhases(open, nil, nil, phaseMap, "test@test.dev")
		if len(cols.Hooked) != 1 || cols.Hooked[0].ID != "spi-stuck" {
			t.Errorf("hooked bead should be in Hooked, got: %v", cols.Hooked)
		}
		if len(cols.Implement) != 0 {
			t.Errorf("hooked bead should NOT be in Implement, got: %v", cols.Implement)
		}
	})

	t.Run("CategorizeWithPhases also routes hooked", func(t *testing.T) {
		open := []BoardBead{
			{ID: "spi-fail", Title: "Repo failure", Status: "hooked", Type: "feature", Priority: 0},
		}
		phaseMap := map[string]string{"spi-fail": "review"}
		cols := CategorizeWithPhases(open, nil, nil, phaseMap, "test@test.dev")
		if len(cols.Hooked) != 1 || cols.Hooked[0].ID != "spi-fail" {
			t.Errorf("expected spi-fail in Hooked, got: %v", cols.Hooked)
		}
		if len(cols.Review) != 0 {
			t.Errorf("hooked bead should NOT be in Review, got: %v", cols.Review)
		}
	})
}

// --- TestConflictWarnings ---

func TestBoardResultWarnings(t *testing.T) {
	t.Run("empty warnings omitted from JSON", func(t *testing.T) {
		result := BoardResult{Columns: Columns{}}
		bj := BoardJSON{
			ColumnsJSON: result.Columns.ToJSON(nil),
			Warnings:    result.Warnings,
		}
		if bj.Warnings != nil {
			t.Errorf("expected nil warnings (omitempty), got %v", bj.Warnings)
		}
	})

	t.Run("warnings present in JSON envelope", func(t *testing.T) {
		result := BoardResult{
			Columns:  Columns{Ready: []BoardBead{{ID: "spi-1", Type: "task", Title: "test"}}},
			Warnings: []string{"dolt-conflict: 2 unresolved conflict(s) in issues table — run `spire pull` to resolve"},
		}
		bj := BoardJSON{
			ColumnsJSON: result.Columns.ToJSON(nil),
			Warnings:    result.Warnings,
		}
		if len(bj.Warnings) != 1 {
			t.Fatalf("expected 1 warning, got %d", len(bj.Warnings))
		}
		if len(bj.Ready) != 1 {
			t.Errorf("expected 1 ready bead alongside warning, got %d", len(bj.Ready))
		}
	})

	t.Run("conflict with empty columns", func(t *testing.T) {
		result := BoardResult{
			Warnings: []string{"dolt-conflict: 5 unresolved conflict(s) in issues table — run `spire pull` to resolve"},
		}
		bj := BoardJSON{
			ColumnsJSON: result.Columns.ToJSON(nil),
			Warnings:    result.Warnings,
		}
		if len(bj.Warnings) != 1 {
			t.Fatalf("expected 1 warning, got %d", len(bj.Warnings))
		}
		if len(bj.Ready) != 0 {
			t.Errorf("expected 0 ready beads, got %d", len(bj.Ready))
		}
	})
}

func TestSnapshotWarningsRendered(t *testing.T) {
	t.Run("TUI renders warnings above alerts", func(t *testing.T) {
		m := makeBoardMode()
		m.Snapshot = &BoardSnapshot{
			Columns: m.Cols,
			Warnings: []string{
				"dolt-conflict: 3 unresolved conflict(s) in issues table — run `spire pull` to resolve",
			},
			DAGProgress: map[string]*DAGProgress{},
			PhaseMap:    map[string]string{},
		}
		view := m.View()
		if !strings.Contains(view, "SYSTEM WARNINGS") {
			t.Error("expected SYSTEM WARNINGS header in view output")
		}
		if !strings.Contains(view, "dolt-conflict") {
			t.Error("expected dolt-conflict warning text in view output")
		}
	})

	t.Run("TUI renders no warnings when empty", func(t *testing.T) {
		m := makeBoardMode()
		m.Snapshot = &BoardSnapshot{
			Columns:     m.Cols,
			DAGProgress: map[string]*DAGProgress{},
			PhaseMap:    map[string]string{},
		}
		view := m.View()
		if strings.Contains(view, "SYSTEM WARNINGS") {
			t.Error("unexpected SYSTEM WARNINGS header when no warnings")
		}
	})

	t.Run("TUI renders with warnings and no data", func(t *testing.T) {
		m := &BoardMode{Width: 120, Height: 40, Identity: "test@test.dev"}
		m.Snapshot = &BoardSnapshot{
			Warnings:    []string{"dolt-conflict: 1 unresolved conflict(s) in issues table — run `spire pull` to resolve"},
			DAGProgress: map[string]*DAGProgress{},
			PhaseMap:    map[string]string{},
		}
		view := m.View()
		if !strings.Contains(view, "SYSTEM WARNINGS") {
			t.Error("expected SYSTEM WARNINGS even with empty columns")
		}
	})
}

func TestCalcHeightBudgetWarnings(t *testing.T) {
	t.Run("warnings allocated in budget", func(t *testing.T) {
		b := CalcHeightBudget(50, 2, 0, 0, 0, 4, 0)
		if b.MaxWarnings != 2 {
			t.Errorf("expected MaxWarnings=2, got %d", b.MaxWarnings)
		}
	})

	t.Run("zero warnings no allocation", func(t *testing.T) {
		b := CalcHeightBudget(50, 0, 0, 0, 0, 4, 0)
		if b.MaxWarnings != 0 {
			t.Errorf("expected MaxWarnings=0, got %d", b.MaxWarnings)
		}
	})

	t.Run("warnings with non-TTY", func(t *testing.T) {
		b := CalcHeightBudget(0, 3, 0, 0, 0, 4, 0)
		if b.MaxWarnings != 3 {
			t.Errorf("expected MaxWarnings=3 for non-TTY, got %d", b.MaxWarnings)
		}
	})
}

// --- TestFetchRecoveryRef ---

func TestFetchRecoveryRef(t *testing.T) {
	t.Run("returns first open recovery-for dependent", func(t *testing.T) {
		getDeps := func(id string) ([]DepRecord, error) {
			return []DepRecord{
				{ID: "other-1", Title: "unrelated", Status: "open", DependencyType: "caused-by"},
				{ID: "rec-1", Title: "recovery: merge-failure", Status: "open", DependencyType: "recovery-for"},
				{ID: "rec-2", Title: "recovery: second", Status: "open", DependencyType: "recovery-for"},
			}, nil
		}
		ref := FetchRecoveryRef("spi-parent", getDeps)
		if ref == nil {
			t.Fatal("expected non-nil RecoveryRef")
		}
		if ref.ID != "rec-1" {
			t.Errorf("expected rec-1, got %s", ref.ID)
		}
		if ref.Title != "recovery: merge-failure" {
			t.Errorf("expected title 'recovery: merge-failure', got %s", ref.Title)
		}
	})

	t.Run("skips closed recovery beads", func(t *testing.T) {
		getDeps := func(id string) ([]DepRecord, error) {
			return []DepRecord{
				{ID: "rec-closed", Title: "closed recovery", Status: "closed", DependencyType: "recovery-for"},
				{ID: "rec-open", Title: "open recovery", Status: "open", DependencyType: "recovery-for"},
			}, nil
		}
		ref := FetchRecoveryRef("spi-parent", getDeps)
		if ref == nil {
			t.Fatal("expected non-nil RecoveryRef")
		}
		if ref.ID != "rec-open" {
			t.Errorf("expected rec-open, got %s", ref.ID)
		}
	})

	t.Run("returns nil when all recovery beads closed", func(t *testing.T) {
		getDeps := func(id string) ([]DepRecord, error) {
			return []DepRecord{
				{ID: "rec-1", Title: "closed", Status: "closed", DependencyType: "recovery-for"},
			}, nil
		}
		ref := FetchRecoveryRef("spi-parent", getDeps)
		if ref != nil {
			t.Errorf("expected nil, got %+v", ref)
		}
	})

	t.Run("returns nil when no recovery-for deps", func(t *testing.T) {
		getDeps := func(id string) ([]DepRecord, error) {
			return []DepRecord{
				{ID: "alert-1", Title: "alert", Status: "open", DependencyType: "caused-by"},
			}, nil
		}
		ref := FetchRecoveryRef("spi-parent", getDeps)
		if ref != nil {
			t.Errorf("expected nil, got %+v", ref)
		}
	})

	t.Run("returns nil when no dependents", func(t *testing.T) {
		getDeps := func(id string) ([]DepRecord, error) {
			return nil, nil
		}
		ref := FetchRecoveryRef("spi-parent", getDeps)
		if ref != nil {
			t.Errorf("expected nil, got %+v", ref)
		}
	})

	t.Run("returns nil on error", func(t *testing.T) {
		getDeps := func(id string) ([]DepRecord, error) {
			return nil, fmt.Errorf("store unavailable")
		}
		ref := FetchRecoveryRef("spi-parent", getDeps)
		if ref != nil {
			t.Errorf("expected nil on error, got %+v", ref)
		}
	})
}

// --- TestToJSON_RecoveryRefs ---

func TestToJSON_RecoveryRefs(t *testing.T) {
	t.Run("enriches interrupted beads with pre-fetched refs", func(t *testing.T) {
		cols := Columns{
			Hooked: []BoardBead{
				{ID: "spi-int1", Title: "interrupted1", Status: "in_progress", Type: "task", Labels: []string{"interrupted:merge-failure"}},
				{ID: "spi-int2", Title: "interrupted2", Status: "in_progress", Type: "task", Labels: []string{"interrupted:build-failure"}},
			},
		}
		refs := map[string]*RecoveryRef{
			"spi-int1": {ID: "spi-rec1", Title: "recovery for int1"},
		}
		cj := cols.ToJSON(refs)
		if cj.Hooked[0].RecoveryBead == nil {
			t.Fatal("expected RecoveryBead on spi-int1")
		}
		if cj.Hooked[0].RecoveryBead.ID != "spi-rec1" {
			t.Errorf("expected spi-rec1, got %s", cj.Hooked[0].RecoveryBead.ID)
		}
		if cj.Hooked[1].RecoveryBead != nil {
			t.Errorf("expected nil RecoveryBead on spi-int2, got %+v", cj.Hooked[1].RecoveryBead)
		}
	})

	t.Run("nil refs leaves RecoveryBead nil", func(t *testing.T) {
		cols := Columns{
			Hooked: []BoardBead{
				{ID: "spi-int1", Title: "interrupted1", Status: "in_progress", Type: "task", Labels: []string{"interrupted:merge-failure"}},
			},
		}
		cj := cols.ToJSON(nil)
		if cj.Hooked[0].RecoveryBead != nil {
			t.Errorf("expected nil RecoveryBead with nil refs, got %+v", cj.Hooked[0].RecoveryBead)
		}
	})
}

// --- TestCategorizeColumnsFromStore_ParentFiltering ---

func TestCategorizeColumnsFromStore_ParentFiltering(t *testing.T) {
	t.Run("parented bead excluded from all columns", func(t *testing.T) {
		openBeads := []BoardBead{
			{ID: "spi-epic", Title: "Epic bead", Status: "open", Type: "epic", Priority: 1},
			{ID: "spi-epic.1", Title: "Epic child task", Status: "open", Type: "task", Priority: 2, Parent: "spi-epic"},
			{ID: "spi-top", Title: "Top-level task", Status: "open", Type: "task", Priority: 2},
		}
		cols := CategorizeColumnsFromStore(openBeads, nil, nil, "test@test.dev")

		// The parented bead should not appear in any column.
		for _, col := range []struct {
			name  string
			beads []BoardBead
		}{
			{"Backlog", cols.Backlog},
			{"Ready", cols.Ready},
			{"Design", cols.Design},
			{"Plan", cols.Plan},
			{"Implement", cols.Implement},
			{"Review", cols.Review},
			{"Merge", cols.Merge},
			{"Done", cols.Done},
			{"Blocked", cols.Blocked},
			{"Alerts", cols.Alerts},
			{"Hooked", cols.Hooked},
		} {
			for _, b := range col.beads {
				if b.ID == "spi-epic.1" {
					t.Errorf("parented bead spi-epic.1 should not appear in %s column", col.name)
				}
			}
		}

		// The epic and top-level task should still appear (open beads go to Backlog).
		found := map[string]bool{}
		for _, b := range cols.Backlog {
			found[b.ID] = true
		}
		if !found["spi-epic"] {
			t.Error("epic bead spi-epic should appear in Backlog")
		}
		if !found["spi-top"] {
			t.Error("top-level bead spi-top should appear in Backlog")
		}
	})
}

// --- TestCategorizeWithPhases_ParentFiltering ---

func TestCategorizeWithPhases_ParentFiltering(t *testing.T) {
	t.Run("parented bead excluded from all columns including phases", func(t *testing.T) {
		openBeads := []BoardBead{
			{ID: "spi-epic", Title: "Epic bead", Status: "in_progress", Type: "epic", Priority: 1},
			{ID: "spi-epic.1", Title: "Child in implement", Status: "in_progress", Type: "task", Priority: 2, Parent: "spi-epic"},
			{ID: "spi-epic.2", Title: "Child in review", Status: "in_progress", Type: "task", Priority: 2, Parent: "spi-epic"},
			{ID: "spi-epic.3", Title: "Child in design", Status: "in_progress", Type: "task", Priority: 2, Parent: "spi-epic"},
			{ID: "spi-epic.4", Title: "Child open no phase", Status: "open", Type: "task", Priority: 2, Parent: "spi-epic"},
			{ID: "spi-top", Title: "Top-level in implement", Status: "in_progress", Type: "task", Priority: 2},
		}
		phaseMap := map[string]string{
			"spi-epic":   "implement",
			"spi-epic.1": "implement",
			"spi-epic.2": "review",
			"spi-epic.3": "design",
			"spi-top":    "implement",
		}

		cols := CategorizeWithPhases(openBeads, nil, nil, phaseMap, "test@test.dev")

		// No parented bead should appear in any column.
		childIDs := map[string]bool{
			"spi-epic.1": true, "spi-epic.2": true,
			"spi-epic.3": true, "spi-epic.4": true,
		}
		for _, col := range []struct {
			name  string
			beads []BoardBead
		}{
			{"Backlog", cols.Backlog},
			{"Ready", cols.Ready},
			{"Design", cols.Design},
			{"Plan", cols.Plan},
			{"Implement", cols.Implement},
			{"Review", cols.Review},
			{"Merge", cols.Merge},
			{"Done", cols.Done},
			{"Blocked", cols.Blocked},
			{"Alerts", cols.Alerts},
			{"Hooked", cols.Hooked},
		} {
			for _, b := range col.beads {
				if childIDs[b.ID] {
					t.Errorf("parented bead %s should not appear in %s column", b.ID, col.name)
				}
			}
		}

		// The epic itself and top-level task should still appear in Implement.
		if len(cols.Implement) != 2 {
			t.Fatalf("expected 2 beads in Implement, got %d", len(cols.Implement))
		}
		implIDs := map[string]bool{}
		for _, b := range cols.Implement {
			implIDs[b.ID] = true
		}
		if !implIDs["spi-epic"] {
			t.Error("epic bead spi-epic should appear in Implement")
		}
		if !implIDs["spi-top"] {
			t.Error("top-level bead spi-top should appear in Implement")
		}
	})
}

// --- TestCategorizeWithPhases_DeferredBeads ---

func TestCategorizeWithPhases_DeferredBeads(t *testing.T) {
	t.Run("deferred top-level bead lands in Backlog", func(t *testing.T) {
		openBeads := []BoardBead{
			{ID: "spi-d1", Title: "deferred task", Status: "deferred", Type: "task", Priority: 2},
			{ID: "spi-r1", Title: "regular task", Status: "open", Type: "task", Priority: 1},
		}
		phaseMap := map[string]string{}
		blockedMap := map[string][]string{}

		cols := CategorizeWithPhases(openBeads, nil, blockedMap, phaseMap, "test")

		if len(cols.Backlog) != 2 {
			t.Fatalf("expected 2 beads in Backlog, got %d", len(cols.Backlog))
		}
		foundDeferred := false
		for _, b := range cols.Backlog {
			if b.ID == "spi-d1" {
				foundDeferred = true
			}
		}
		if !foundDeferred {
			t.Error("expected deferred bead spi-d1 in Backlog column")
		}
	})

	t.Run("deferred child bead excluded from Backlog", func(t *testing.T) {
		openBeads := []BoardBead{
			{ID: "spi-d2", Title: "deferred child", Status: "deferred", Type: "task", Parent: "spi-epic"},
		}
		phaseMap := map[string]string{}
		blockedMap := map[string][]string{}

		cols := CategorizeWithPhases(openBeads, nil, blockedMap, phaseMap, "test")

		if len(cols.Backlog) != 0 {
			t.Errorf("expected 0 beads in Backlog (child beads excluded), got %d", len(cols.Backlog))
		}
	})
}

// --- TestSortBeads_DeferredToBottom ---

func TestSortBeads_DeferredToBottom(t *testing.T) {
	t.Run("deferred beads sort after non-deferred", func(t *testing.T) {
		beads := []BoardBead{
			{ID: "spi-d1", Title: "deferred low prio", Status: "deferred", Priority: 0, UpdatedAt: "2026-04-01T00:00:00Z"},
			{ID: "spi-r1", Title: "regular high prio", Status: "open", Priority: 3, UpdatedAt: "2026-01-01T00:00:00Z"},
			{ID: "spi-r2", Title: "regular low prio", Status: "open", Priority: 1, UpdatedAt: "2026-03-01T00:00:00Z"},
			{ID: "spi-d2", Title: "deferred task", Status: "deferred", Priority: 1, UpdatedAt: "2026-04-01T00:00:00Z"},
		}

		SortBeads(beads)

		// Non-deferred should come first (sorted by priority then time).
		if beads[0].ID != "spi-r2" {
			t.Errorf("position 0: expected spi-r2 (prio 1), got %s (prio %d)", beads[0].ID, beads[0].Priority)
		}
		if beads[1].ID != "spi-r1" {
			t.Errorf("position 1: expected spi-r1 (prio 3), got %s (prio %d)", beads[1].ID, beads[1].Priority)
		}
		// Deferred should come last (sorted by priority then time among themselves).
		if beads[2].ID != "spi-d1" {
			t.Errorf("position 2: expected spi-d1 (deferred prio 0), got %s", beads[2].ID)
		}
		if beads[3].ID != "spi-d2" {
			t.Errorf("position 3: expected spi-d2 (deferred prio 1), got %s", beads[3].ID)
		}
	})

	t.Run("all deferred preserves priority order", func(t *testing.T) {
		beads := []BoardBead{
			{ID: "spi-d3", Title: "prio 2", Status: "deferred", Priority: 2, UpdatedAt: "2026-01-01T00:00:00Z"},
			{ID: "spi-d1", Title: "prio 0", Status: "deferred", Priority: 0, UpdatedAt: "2026-01-01T00:00:00Z"},
			{ID: "spi-d2", Title: "prio 1", Status: "deferred", Priority: 1, UpdatedAt: "2026-01-01T00:00:00Z"},
		}

		SortBeads(beads)

		if beads[0].Priority != 0 || beads[1].Priority != 1 || beads[2].Priority != 2 {
			t.Errorf("expected priority order [0,1,2], got [%d,%d,%d]",
				beads[0].Priority, beads[1].Priority, beads[2].Priority)
		}
	})

	t.Run("no deferred preserves normal sort", func(t *testing.T) {
		beads := []BoardBead{
			{ID: "spi-r2", Title: "prio 2", Status: "open", Priority: 2, UpdatedAt: "2026-01-01T00:00:00Z"},
			{ID: "spi-r1", Title: "prio 1", Status: "open", Priority: 1, UpdatedAt: "2026-01-01T00:00:00Z"},
		}

		SortBeads(beads)

		if beads[0].ID != "spi-r1" || beads[1].ID != "spi-r2" {
			t.Errorf("expected [spi-r1, spi-r2], got [%s, %s]", beads[0].ID, beads[1].ID)
		}
	})
}

// --- ViewMode cycling tests ---

func TestVCyclesViewMode(t *testing.T) {
	m := makeBoardMode()

	// Default is ViewBoard.
	if m.ViewMode != ViewBoard {
		t.Fatalf("expected ViewBoard default, got %d", m.ViewMode)
	}

	// v: Board -> Alerts.
	m = updateBoardMode(m, keyMsgStr("v"))
	if m.ViewMode != ViewAlerts {
		t.Errorf("after 1st v: expected ViewAlerts, got %d", m.ViewMode)
	}

	// v: Alerts -> Lower.
	m = updateBoardMode(m, keyMsgStr("v"))
	if m.ViewMode != ViewLower {
		t.Errorf("after 2nd v: expected ViewLower, got %d", m.ViewMode)
	}

	// v: Lower -> Board (wrap).
	m = updateBoardMode(m, keyMsgStr("v"))
	if m.ViewMode != ViewBoard {
		t.Errorf("after 3rd v: expected ViewBoard (wrap), got %d", m.ViewMode)
	}
}

func TestShiftVCyclesViewModeBackward(t *testing.T) {
	m := makeBoardMode()

	// V: Board -> Lower (backward wrap).
	m = updateBoardMode(m, keyMsgStr("V"))
	if m.ViewMode != ViewLower {
		t.Errorf("after V from Board: expected ViewLower, got %d", m.ViewMode)
	}

	// V: Lower -> Alerts.
	m = updateBoardMode(m, keyMsgStr("V"))
	if m.ViewMode != ViewAlerts {
		t.Errorf("after V from Lower: expected ViewAlerts, got %d", m.ViewMode)
	}

	// V: Alerts -> Board.
	m = updateBoardMode(m, keyMsgStr("V"))
	if m.ViewMode != ViewBoard {
		t.Errorf("after V from Alerts: expected ViewBoard, got %d", m.ViewMode)
	}
}

func TestViewCycleResetsSelCardAndColScroll(t *testing.T) {
	// Use a model with alerts so ViewAlerts has cards to select.
	m := makeBoardMode()
	m.Cols.Alerts = []BoardBead{
		{ID: "spi-a1", Title: "Alert", Status: "open", Type: "task", Priority: 0, Labels: []string{"alert:test"}},
	}
	m.Snapshot.Columns.Alerts = m.Cols.Alerts
	m.SelCard = 2
	m.ColScroll = 1

	m = updateBoardMode(m, keyMsgStr("v"))
	if m.SelCard != 0 {
		t.Errorf("expected SelCard=0 after v, got %d", m.SelCard)
	}
	if m.ColScroll != 0 {
		t.Errorf("expected ColScroll=0 after v, got %d", m.ColScroll)
	}
}

func TestResolveKeyOpensResolveInput(t *testing.T) {
	m := makeBoardMode()
	m.ViewMode = ViewLower
	m.SelSection = SectionLower
	m.SelLowerCol = 1 // Hooked column
	m.Cols.Hooked = []BoardBead{
		{ID: "spi-nh1", Title: "Needs human", Status: "in_progress", Type: "task", Labels: []string{"needs-human"}},
	}
	m.SelCard = 0

	m = updateBoardMode(m, keyMsgStr("o"))

	if !m.ResolveActive {
		t.Error("expected ResolveActive=true after pressing 'o' on needs-human bead")
	}
	if !m.Inspecting {
		t.Error("expected Inspecting=true after pressing 'o' on needs-human bead")
	}
	if m.ResolveBeadID != "spi-nh1" {
		t.Errorf("expected ResolveBeadID=spi-nh1, got %q", m.ResolveBeadID)
	}
}

func TestResolveKeyNoOpWithoutNeedsHuman(t *testing.T) {
	m := makeBoardMode()
	m.ViewMode = ViewLower
	m.SelSection = SectionLower
	m.SelLowerCol = 0 // Blocked column
	m.Cols.Blocked = []BoardBead{
		{ID: "spi-blk1", Title: "Blocked task", Status: "open", Type: "task"},
	}
	m.SelCard = 0

	m = updateBoardMode(m, keyMsgStr("o"))

	if m.ResolveActive {
		t.Error("expected ResolveActive=false after pressing 'o' on non-needs-human bead")
	}
	if m.Inspecting {
		t.Error("expected Inspecting=false after pressing 'o' on non-needs-human bead")
	}
}

func TestClampSelectionForcesSection(t *testing.T) {
	m := makeBoardMode()

	// ViewAlerts -> SelSection must be SectionAlerts.
	m.ViewMode = ViewAlerts
	m.SelSection = SectionColumns // intentionally wrong
	m.ClampSelection()
	if m.SelSection != SectionAlerts {
		t.Errorf("ClampSelection with ViewAlerts: expected SectionAlerts, got %d", m.SelSection)
	}

	// ViewBoard -> SelSection must be SectionColumns.
	m.ViewMode = ViewBoard
	m.SelSection = SectionAlerts // intentionally wrong
	m.ClampSelection()
	if m.SelSection != SectionColumns {
		t.Errorf("ClampSelection with ViewBoard: expected SectionColumns, got %d", m.SelSection)
	}

	// ViewLower -> SelSection must be SectionLower.
	m.ViewMode = ViewLower
	m.SelSection = SectionColumns // intentionally wrong
	m.ClampSelection()
	if m.SelSection != SectionLower {
		t.Errorf("ClampSelection with ViewLower: expected SectionLower, got %d", m.SelSection)
	}
}

func TestJKStaysWithinViewMode(t *testing.T) {
	// In ViewAlerts, j/k should not cross into SectionColumns.
	cols := Columns{
		Alerts: []BoardBead{
			{ID: "spi-a1", Title: "Alert 1", Status: "open", Type: "task", Priority: 0, Labels: []string{"alert:test"}},
			{ID: "spi-a2", Title: "Alert 2", Status: "open", Type: "task", Priority: 1, Labels: []string{"alert:test"}},
		},
		Ready: []BoardBead{
			{ID: "spi-r1", Title: "Ready 1", Status: "open", Type: "task", Priority: 1},
		},
	}
	m := &BoardMode{
		Cols:       cols,
		Width:      120,
		Height:     40,
		SelSection: SectionAlerts,
		ViewMode:   ViewAlerts,
		Snapshot: &BoardSnapshot{
			Columns:     cols,
			DAGProgress: map[string]*DAGProgress{},
			PhaseMap:    map[string]string{},
		},
	}

	// Move down to second alert.
	m = updateBoardMode(m, keyMsg('j'))
	if m.SelCard != 1 {
		t.Errorf("expected SelCard=1, got %d", m.SelCard)
	}
	if m.SelSection != SectionAlerts {
		t.Errorf("expected SectionAlerts after j, got %d", m.SelSection)
	}

	// Move down again — should NOT leave SectionAlerts.
	m = updateBoardMode(m, keyMsg('j'))
	if m.SelSection != SectionAlerts {
		t.Errorf("j at bottom of alerts should stay in SectionAlerts, got %d", m.SelSection)
	}
	if m.SelCard != 1 {
		t.Errorf("j at bottom of alerts should keep SelCard=1, got %d", m.SelCard)
	}
}

func TestRenderTabSidebar(t *testing.T) {
	t.Run("active tab is highlighted", func(t *testing.T) {
		sidebar := renderTabSidebar(ViewBoard, 3, 2, 0)
		if !strings.Contains(sidebar, "BOARD") {
			t.Error("sidebar should contain BOARD label")
		}
		if !strings.Contains(sidebar, "ALERTS") {
			t.Error("sidebar should contain ALERTS label")
		}
		if !strings.Contains(sidebar, "BLOCKED") {
			t.Error("sidebar should contain BLOCKED label")
		}
	})

	t.Run("counts shown when non-zero", func(t *testing.T) {
		sidebar := renderTabSidebar(ViewAlerts, 5, 0, 0)
		if !strings.Contains(sidebar, "ALERTS (5)") {
			t.Error("sidebar should show ALERTS (5) when alertCount=5")
		}
		if strings.Contains(sidebar, "BLOCKED (") {
			t.Error("sidebar should not show count for BLOCKED when lowerCount=0")
		}
	})

	t.Run("separate blocked and hooked counts", func(t *testing.T) {
		sidebar := renderTabSidebar(ViewBoard, 0, 2, 1)
		if !strings.Contains(sidebar, "BLK(2) HKD(1)") {
			t.Errorf("sidebar should show 'BLK(2) HKD(1)' when both present, got: %s", sidebar)
		}
	})

	t.Run("only hooked count", func(t *testing.T) {
		sidebar := renderTabSidebar(ViewBoard, 0, 0, 3)
		if !strings.Contains(sidebar, "HOOKED (3)") {
			t.Errorf("sidebar should show 'HOOKED (3)' when only hooked, got: %s", sidebar)
		}
	})
}

func TestViewRendersActiveMode(t *testing.T) {
	cols := Columns{
		Alerts: []BoardBead{
			{ID: "spi-a1", Title: "Test Alert", Status: "open", Type: "task", Priority: 0, Labels: []string{"alert:test"}},
		},
		Ready: []BoardBead{
			{ID: "spi-r1", Title: "Ready Task", Status: "open", Type: "task", Priority: 1},
		},
		Blocked: []BoardBead{
			{ID: "spi-b1", Title: "Blocked Task", Status: "open", Type: "task", Priority: 2, Labels: []string{}},
		},
	}
	m := &BoardMode{
		Cols:       cols,
		Width:      120,
		Height:     40,
		SelSection: SectionColumns,
		ViewMode:   ViewBoard,
		Opts:       Opts{Interval: 5 * time.Second},
		Snapshot: &BoardSnapshot{
			Columns:     cols,
			DAGProgress: map[string]*DAGProgress{},
			PhaseMap:    map[string]string{},
		},
	}

	t.Run("ViewBoard shows columns not alerts", func(t *testing.T) {
		output := m.View()
		if !strings.Contains(output, "Ready Task") {
			t.Error("ViewBoard should show column content like 'Ready Task'")
		}
		// Chrome (sidebar) is rendered by RootModel, not BoardMode.
		// BoardMode.View() only renders board content.
	})

	t.Run("ViewAlerts shows alerts", func(t *testing.T) {
		m2 := *m
		m2.ViewMode = ViewAlerts
		m2.SelSection = SectionAlerts
		output := m2.View()
		if !strings.Contains(output, "Test Alert") {
			t.Error("ViewAlerts should show alert content")
		}
	})

	t.Run("ViewLower shows blocked", func(t *testing.T) {
		m3 := *m
		m3.ViewMode = ViewLower
		m3.SelSection = SectionLower
		output := m3.View()
		if !strings.Contains(output, "Blocked Task") {
			t.Error("ViewLower should show blocked content")
		}
	})

	t.Run("ViewAlerts empty shows placeholder", func(t *testing.T) {
		m4 := *m
		m4.ViewMode = ViewAlerts
		m4.SelSection = SectionAlerts
		emptyCols := Columns{}
		m4.Cols = emptyCols
		m4.Snapshot = &BoardSnapshot{
			Columns:     emptyCols,
			DAGProgress: map[string]*DAGProgress{},
			PhaseMap:    map[string]string{},
		}
		output := m4.View()
		if !strings.Contains(output, "No alerts") {
			t.Error("ViewAlerts with no alerts should show 'No alerts' placeholder")
		}
	})

	t.Run("ViewLower empty shows placeholder", func(t *testing.T) {
		m5 := *m
		m5.ViewMode = ViewLower
		m5.SelSection = SectionLower
		emptyCols := Columns{}
		m5.Cols = emptyCols
		m5.Snapshot = &BoardSnapshot{
			Columns:     emptyCols,
			DAGProgress: map[string]*DAGProgress{},
			PhaseMap:    map[string]string{},
		}
		output := m5.View()
		if !strings.Contains(output, "No blocked or hooked") {
			t.Error("ViewLower with empty data should show placeholder")
		}
	})
}

func TestInspectorTabNotAffectedByViewMode(t *testing.T) {
	m := makeBoardMode()
	m.Inspecting = true
	m.InspectorTab = 0

	// Tab in inspector should toggle InspectorTab, not ViewMode.
	m = updateBoardMode(m, keyMsgStr("tab"))
	if m.InspectorTab != 1 {
		t.Errorf("tab in inspector should toggle InspectorTab, got %d", m.InspectorTab)
	}
	if m.ViewMode != ViewBoard {
		t.Errorf("tab in inspector should not change ViewMode, got %d", m.ViewMode)
	}
}

func TestParsePendingAction(t *testing.T) {
	tests := []struct {
		input string
		want  PendingAction
	}{
		// Lowercase (hardcoded paths).
		{"focus", ActionFocus},
		{"logs", ActionLogs},
		{"summon", ActionSummon},
		{"claim", ActionClaim},
		{"resummon", ActionResummon},
		{"close", ActionClose},
		{"grok", ActionGrok},
		{"trace", ActionTrace},
		// Title-case (from actionLabel()).
		{"Grok", ActionGrok},
		{"Trace", ActionTrace},
		{"Summon", ActionSummon},
		{"Close", ActionClose},
		// Unknown.
		{"", ActionNone},
		{"unknown", ActionNone},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parsePendingAction(tt.input)
			if got != tt.want {
				t.Errorf("parsePendingAction(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

