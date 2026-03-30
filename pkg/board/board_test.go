package board

import (
	"fmt"
	"testing"

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

func updateModel(m Model, msg tea.Msg) Model {
	result, _ := m.Update(msg)
	return result.(Model)
}

// makeModel creates a Model with some columns populated for testing.
func makeModel() Model {
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
	return Model{
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

		expectActions(t, items, []PendingAction{ActionSummon, ActionClose, ActionGrok, ActionTrace})

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
			ActionAdvance, ActionClose, ActionGrok, ActionTrace,
		})

		// Verify Reset --hard is DangerDestructive.
		for _, item := range items {
			if item.ActionType == ActionResetHard && item.Danger != DangerDestructive {
				t.Errorf("Reset --hard should be DangerDestructive, got %d", item.Danger)
			}
		}
	})

	t.Run("in_progress with needs-human", func(t *testing.T) {
		bead := &BoardBead{ID: "spi-003", Status: "in_progress", Type: "task", Labels: []string{"needs-human"}}
		items := BuildActionMenu(bead, nil)

		found := false
		for _, item := range items {
			if item.ActionType == ActionResummon {
				found = true
				break
			}
		}
		if !found {
			t.Error("expected Resummon action for needs-human bead")
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
		m := makeModel()
		m = updateModel(m, keyMsg('/'))
		if !m.SearchActive {
			t.Error("expected SearchActive after /")
		}
		if m.SearchQuery != "" {
			t.Error("expected empty SearchQuery on activation")
		}
	})

	t.Run("typing accumulates query", func(t *testing.T) {
		m := makeModel()
		m = updateModel(m, keyMsg('/'))
		m = updateModel(m, keyMsg('a'))
		m = updateModel(m, keyMsg('b'))
		m = updateModel(m, keyMsg('c'))
		if m.SearchQuery != "abc" {
			t.Errorf("expected query 'abc', got %q", m.SearchQuery)
		}
	})

	t.Run("backspace removes last rune", func(t *testing.T) {
		m := makeModel()
		m = updateModel(m, keyMsg('/'))
		m = updateModel(m, keyMsg('a'))
		m = updateModel(m, keyMsg('b'))
		m = updateModel(m, keyMsgType(tea.KeyBackspace))
		if m.SearchQuery != "a" {
			t.Errorf("expected query 'a' after backspace, got %q", m.SearchQuery)
		}
	})

	t.Run("ctrl+u clears query", func(t *testing.T) {
		m := makeModel()
		m = updateModel(m, keyMsg('/'))
		m = updateModel(m, keyMsg('a'))
		m = updateModel(m, keyMsg('b'))
		m = updateModel(m, keyMsgStr("ctrl+u"))
		if m.SearchQuery != "" {
			t.Errorf("expected empty query after ctrl+u, got %q", m.SearchQuery)
		}
	})

	t.Run("esc exits search and clears query", func(t *testing.T) {
		m := makeModel()
		m = updateModel(m, keyMsg('/'))
		m = updateModel(m, keyMsg('x'))
		m = updateModel(m, keyMsgType(tea.KeyEsc))
		if m.SearchActive {
			t.Error("expected SearchActive=false after Esc")
		}
		if m.SearchQuery != "" {
			t.Errorf("expected empty query after Esc, got %q", m.SearchQuery)
		}
	})

	t.Run("enter exits search preserves query", func(t *testing.T) {
		m := makeModel()
		m = updateModel(m, keyMsg('/'))
		m = updateModel(m, keyMsg('t'))
		m = updateModel(m, keyMsg('e'))
		m = updateModel(m, keyMsgType(tea.KeyEnter))
		if m.SearchActive {
			t.Error("expected SearchActive=false after Enter")
		}
		if m.SearchQuery != "te" {
			t.Errorf("expected query 'te' preserved after Enter, got %q", m.SearchQuery)
		}
	})

	t.Run("query change resets selection", func(t *testing.T) {
		m := makeModel()
		m.SelCard = 2
		m.ColScroll = 1
		m = updateModel(m, keyMsg('/'))
		m = updateModel(m, keyMsg('x'))
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
		m := makeModel()
		// Start on first column (Ready) with 2 beads.
		m.SelCol = 0
		m.SelCard = 0
		m = updateModel(m, keyMsg('j'))
		if m.SelCard != 1 {
			t.Errorf("expected SelCard=1 after j, got %d", m.SelCard)
		}
	})

	t.Run("k moves SelCard up", func(t *testing.T) {
		m := makeModel()
		m.SelCol = 0
		m.SelCard = 1
		m = updateModel(m, keyMsg('k'))
		if m.SelCard != 0 {
			t.Errorf("expected SelCard=0 after k, got %d", m.SelCard)
		}
	})

	t.Run("h moves SelCol left", func(t *testing.T) {
		m := makeModel()
		m.SelCol = 1
		m = updateModel(m, keyMsg('h'))
		if m.SelCol != 0 {
			t.Errorf("expected SelCol=0 after h, got %d", m.SelCol)
		}
	})

	t.Run("l moves SelCol right", func(t *testing.T) {
		m := makeModel()
		m.SelCol = 0
		m = updateModel(m, keyMsg('l'))
		if m.SelCol != 1 {
			t.Errorf("expected SelCol=1 after l, got %d", m.SelCol)
		}
	})

	t.Run("gg jumps to top", func(t *testing.T) {
		m := makeModel()
		m.SelCol = 1 // Implement column with 3 beads
		m.SelCard = 2
		m.ColScroll = 1
		m = updateModel(m, keyMsg('g'))
		if !m.PendingG {
			t.Error("expected PendingG after first g")
		}
		m = updateModel(m, keyMsg('g'))
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
		m := makeModel()
		m.SelCol = 1
		m.SelCard = 2
		m = updateModel(m, keyMsg('g'))
		if !m.PendingG {
			t.Error("expected PendingG after first g")
		}
		m = updateModel(m, keyMsg('j'))
		if m.PendingG {
			t.Error("PendingG should be cleared after non-g key")
		}
	})

	t.Run("G jumps to bottom", func(t *testing.T) {
		m := makeModel()
		m.SelCol = 1 // Implement column with 3 beads
		m.SelCard = 0
		m = updateModel(m, keyMsg('G'))
		if m.SelCard != 2 {
			t.Errorf("expected SelCard=2 (last card) after G, got %d", m.SelCard)
		}
	})

	t.Run("t cycles TypeScope", func(t *testing.T) {
		m := makeModel()
		if m.TypeScope != TypeAll {
			t.Fatalf("expected initial TypeScope=TypeAll, got %d", m.TypeScope)
		}
		m = updateModel(m, keyMsg('t'))
		if m.TypeScope != TypeTask {
			t.Errorf("expected TypeScope=TypeTask after first t, got %d", m.TypeScope)
		}
		m = updateModel(m, keyMsg('t'))
		if m.TypeScope != TypeBug {
			t.Errorf("expected TypeScope=TypeBug after second t, got %d", m.TypeScope)
		}
	})

	t.Run("H toggles ShowAllCols", func(t *testing.T) {
		m := makeModel()
		if m.ShowAllCols {
			t.Fatal("expected ShowAllCols=false initially")
		}
		m = updateModel(m, keyMsg('H'))
		if !m.ShowAllCols {
			t.Error("expected ShowAllCols=true after H")
		}
		m = updateModel(m, keyMsg('H'))
		if m.ShowAllCols {
			t.Error("expected ShowAllCols=false after second H")
		}
	})

	t.Run("a opens action menu", func(t *testing.T) {
		m := makeModel()
		m.SelCol = 0
		m.SelCard = 0
		m = updateModel(m, keyMsg('a'))
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
		m := makeModel()
		m = updateModel(m, keyMsg('q'))
		if !m.Quitting {
			t.Error("expected Quitting after q")
		}
	})

	t.Run("q clears search query first", func(t *testing.T) {
		m := makeModel()
		m.SearchQuery = "test"
		m = updateModel(m, keyMsg('q'))
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
	openMenu := func() Model {
		m := makeModel()
		m.SelCol = 0
		m.SelCard = 0
		m = updateModel(m, keyMsg('a'))
		return m
	}

	t.Run("j moves cursor down", func(t *testing.T) {
		m := openMenu()
		initial := m.ActionMenuCursor
		m = updateModel(m, keyMsg('j'))
		if m.ActionMenuCursor != initial+1 {
			t.Errorf("expected cursor %d, got %d", initial+1, m.ActionMenuCursor)
		}
	})

	t.Run("k moves cursor up", func(t *testing.T) {
		m := openMenu()
		m.ActionMenuCursor = 1
		m = updateModel(m, keyMsg('k'))
		if m.ActionMenuCursor != 0 {
			t.Errorf("expected cursor 0, got %d", m.ActionMenuCursor)
		}
	})

	t.Run("k at top stays at 0", func(t *testing.T) {
		m := openMenu()
		m.ActionMenuCursor = 0
		m = updateModel(m, keyMsg('k'))
		if m.ActionMenuCursor != 0 {
			t.Errorf("expected cursor 0, got %d", m.ActionMenuCursor)
		}
	})

	t.Run("j at bottom stays at last", func(t *testing.T) {
		m := openMenu()
		last := len(m.ActionMenuItems) - 1
		m.ActionMenuCursor = last
		m = updateModel(m, keyMsg('j'))
		if m.ActionMenuCursor != last {
			t.Errorf("expected cursor %d, got %d", last, m.ActionMenuCursor)
		}
	})

	t.Run("esc closes menu", func(t *testing.T) {
		m := openMenu()
		m = updateModel(m, keyMsgType(tea.KeyEsc))
		if m.ActionMenuOpen {
			t.Error("expected ActionMenuOpen=false after Esc")
		}
	})

	t.Run("enter selects item", func(t *testing.T) {
		m := openMenu()
		m.ActionMenuCursor = 0
		expected := m.ActionMenuItems[0].ActionType
		m = updateModel(m, keyMsgType(tea.KeyEnter))
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
		m = updateModel(m, keyMsg(grokItem.Key))
		if m.ActionMenuOpen {
			t.Error("expected menu closed after shortcut key")
		}
	})

	t.Run("menu absorbs board keys", func(t *testing.T) {
		m := openMenu()
		origSelCard := m.SelCard
		m = updateModel(m, keyMsg('j'))
		// j should move menu cursor, not SelCard.
		if m.SelCard != origSelCard {
			t.Error("board key leaked through action menu")
		}
	})
}

// --- TestInspectorNavigation ---

func TestInspectorNavigation(t *testing.T) {
	makeInspecting := func() Model {
		m := makeModel()
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
		m = updateModel(m, keyMsgType(tea.KeyTab))
		if m.InspectorTab != 1 {
			t.Errorf("expected InspectorTab=1 after Tab, got %d", m.InspectorTab)
		}
		m = updateModel(m, keyMsgType(tea.KeyTab))
		if m.InspectorTab != 0 {
			t.Errorf("expected InspectorTab=0 after second Tab, got %d", m.InspectorTab)
		}
	})

	t.Run("shift+tab toggles in reverse", func(t *testing.T) {
		m := makeInspecting()
		m.InspectorTab = 0
		m = updateModel(m, keyMsgStr("shift+tab"))
		if m.InspectorTab != 1 {
			t.Errorf("expected InspectorTab=1 after Shift+Tab from 0, got %d", m.InspectorTab)
		}
	})

	t.Run("tab resets scroll", func(t *testing.T) {
		m := makeInspecting()
		m.InspectorScroll = 5
		m = updateModel(m, keyMsgType(tea.KeyTab))
		if m.InspectorScroll != 0 {
			t.Errorf("expected InspectorScroll=0 after Tab, got %d", m.InspectorScroll)
		}
	})

	t.Run("j increments scroll", func(t *testing.T) {
		m := makeInspecting()
		m = updateModel(m, keyMsg('j'))
		if m.InspectorScroll != 1 {
			t.Errorf("expected InspectorScroll=1 after j, got %d", m.InspectorScroll)
		}
	})

	t.Run("k decrements scroll", func(t *testing.T) {
		m := makeInspecting()
		m.InspectorScroll = 3
		m = updateModel(m, keyMsg('k'))
		if m.InspectorScroll != 2 {
			t.Errorf("expected InspectorScroll=2 after k, got %d", m.InspectorScroll)
		}
	})

	t.Run("k at 0 stays at 0", func(t *testing.T) {
		m := makeInspecting()
		m.InspectorScroll = 0
		m = updateModel(m, keyMsg('k'))
		if m.InspectorScroll != 0 {
			t.Errorf("expected InspectorScroll=0, got %d", m.InspectorScroll)
		}
	})

	t.Run("g jumps to top", func(t *testing.T) {
		m := makeInspecting()
		m.InspectorScroll = 10
		m = updateModel(m, keyMsg('g'))
		if m.InspectorScroll != 0 {
			t.Errorf("expected InspectorScroll=0 after g, got %d", m.InspectorScroll)
		}
	})

	t.Run("G jumps to bottom", func(t *testing.T) {
		m := makeInspecting()
		m = updateModel(m, keyMsg('G'))
		if m.InspectorScroll <= 0 {
			t.Error("expected InspectorScroll > 0 after G with content")
		}
	})

	t.Run("esc exits inspector", func(t *testing.T) {
		m := makeInspecting()
		m = updateModel(m, keyMsgType(tea.KeyEsc))
		if m.Inspecting {
			t.Error("expected Inspecting=false after Esc")
		}
	})
}

// --- TestColumnScrolling ---

func TestColumnScrolling(t *testing.T) {
	// Create a model with a tall column but short terminal.
	makeScrollModel := func() Model {
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
		return Model{
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
		m := makeScrollModel()
		maxCards := m.colMaxCards()
		// Navigate down past the visible window.
		for i := 0; i < maxCards+2; i++ {
			m = updateModel(m, keyMsg('j'))
		}
		if m.ColScroll == 0 {
			t.Error("expected ColScroll > 0 after navigating past viewport")
		}
	})

	t.Run("k back up retreats ColScroll", func(t *testing.T) {
		m := makeScrollModel()
		maxCards := m.colMaxCards()
		// Navigate down past viewport.
		for i := 0; i < maxCards+2; i++ {
			m = updateModel(m, keyMsg('j'))
		}
		scrollAfterDown := m.ColScroll
		// Navigate back up.
		for i := 0; i < 3; i++ {
			m = updateModel(m, keyMsg('k'))
		}
		if m.ColScroll >= scrollAfterDown {
			t.Error("expected ColScroll to decrease after k")
		}
	})

	t.Run("ensureCardVisible adjusts scroll", func(t *testing.T) {
		m := makeScrollModel()
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
		m := makeScrollModel()
		maxCards := m.colMaxCards()
		m.ColScroll = 10
		m.SelCard = 5
		m.ensureCardVisible(maxCards)
		if m.ColScroll != 5 {
			t.Errorf("expected ColScroll=5, got %d", m.ColScroll)
		}
	})

	t.Run("ClampSelection wraps negative", func(t *testing.T) {
		m := makeScrollModel()
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
		m := makeScrollModel()
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
			bead:   BoardBead{ID: "spi-103", Title: "A message", Labels: []string{"msg"}},
			expect: true,
		},
		{
			name:   "workflow-step label is skipped",
			bead:   BoardBead{ID: "spi-104", Title: "step:implement", Labels: []string{"workflow-step", "step:implement"}},
			expect: true,
		},
		{
			name:   "attempt bead is skipped",
			bead:   BoardBead{ID: "spi-105", Title: "attempt: wizard-spi-xxx", Labels: []string{"attempt"}},
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
		if len(cols.Ready) != 1 || cols.Ready[0].ID != "spi-200" {
			t.Errorf("expected only spi-200 in Ready, got %v", cols.Ready)
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
		if len(cols.Ready) != 1 || cols.Ready[0].ID != "spi-401" {
			t.Errorf("expected only spi-401 in Ready, got %v", cols.Ready)
		}
	})
}

// --- TestCalcHeightBudget ---

func TestCalcHeightBudget(t *testing.T) {
	t.Run("zero height returns permissive defaults", func(t *testing.T) {
		b := CalcHeightBudget(0, 0, 0, 3, 0)
		if b.MaxCards != 99 {
			t.Errorf("expected MaxCards=99 for zero height, got %d", b.MaxCards)
		}
	})

	t.Run("positive height computes budget", func(t *testing.T) {
		b := CalcHeightBudget(40, 0, 0, 3, 0)
		if b.MaxCards <= 0 {
			t.Errorf("expected MaxCards > 0, got %d", b.MaxCards)
		}
	})

	t.Run("very small height still works", func(t *testing.T) {
		b := CalcHeightBudget(10, 0, 0, 1, 0)
		if b.MaxCards < 1 {
			t.Errorf("expected MaxCards >= 1, got %d", b.MaxCards)
		}
	})
}
