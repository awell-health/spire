package board

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// mockMode implements Mode for testing. It records method calls and allows
// configuring return values.
type mockMode struct {
	id        ModeID
	overlay   bool
	viewText  string
	width     int
	height    int
	activated bool
	deactivated bool
	// Recorded calls
	initCalled          bool
	setSizeCalls        [][2]int // {w, h} pairs
	towerChangedCalls   []TowerChanged
	updateMsgs          []tea.Msg
	onActivateCalls     int
	onDeactivateCalls   int
}

func newMockMode(id ModeID) *mockMode {
	return &mockMode{id: id, viewText: id.String() + " view"}
}

func (m *mockMode) Init() tea.Cmd {
	m.initCalled = true
	return nil
}

func (m *mockMode) Update(msg tea.Msg) (Mode, tea.Cmd) {
	m.updateMsgs = append(m.updateMsgs, msg)
	return m, nil
}

func (m *mockMode) View() string {
	return m.viewText
}

func (m *mockMode) SetSize(w, h int) {
	m.width = w
	m.height = h
	m.setSizeCalls = append(m.setSizeCalls, [2]int{w, h})
}

func (m *mockMode) OnActivate() tea.Cmd {
	m.activated = true
	m.onActivateCalls++
	return nil
}

func (m *mockMode) OnDeactivate() {
	m.deactivated = true
	m.onDeactivateCalls++
}

func (m *mockMode) HandleTowerChanged(tc TowerChanged) tea.Cmd {
	m.towerChangedCalls = append(m.towerChangedCalls, tc)
	return nil
}

func (m *mockMode) HasOverlay() bool {
	return m.overlay
}

func (m *mockMode) ID() ModeID {
	return m.id
}

func TestTabCycling(t *testing.T) {
	m0 := newMockMode(ModeBoard)
	m1 := newMockMode(ModeAgents)
	m2 := newMockMode(ModeWorkshop)

	root := NewRootModel(RootOpts{
		TowerName: "test-tower",
		Identity:  "test-user",
		Modes:     []Mode{m0, m1, m2},
	})

	if root.activeModeIdx != 0 {
		t.Fatalf("expected initial activeModeIdx=0, got %d", root.activeModeIdx)
	}

	// Tab forward: 0 -> 1
	model, _ := root.Update(tea.KeyMsg{Type: tea.KeyTab})
	root = model.(RootModel)
	if root.activeModeIdx != 1 {
		t.Fatalf("expected activeModeIdx=1 after Tab, got %d", root.activeModeIdx)
	}
	if !m0.deactivated {
		t.Fatal("expected mode 0 OnDeactivate to be called")
	}
	if !m1.activated {
		t.Fatal("expected mode 1 OnActivate to be called")
	}

	// Tab forward: 1 -> 2
	model, _ = root.Update(tea.KeyMsg{Type: tea.KeyTab})
	root = model.(RootModel)
	if root.activeModeIdx != 2 {
		t.Fatalf("expected activeModeIdx=2 after Tab, got %d", root.activeModeIdx)
	}

	// Tab forward wraps: 2 -> 0
	model, _ = root.Update(tea.KeyMsg{Type: tea.KeyTab})
	root = model.(RootModel)
	if root.activeModeIdx != 0 {
		t.Fatalf("expected activeModeIdx=0 after wrap, got %d", root.activeModeIdx)
	}

	// Shift+Tab backward wraps: 0 -> 2
	model, _ = root.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	root = model.(RootModel)
	if root.activeModeIdx != 2 {
		t.Fatalf("expected activeModeIdx=2 after Shift+Tab, got %d", root.activeModeIdx)
	}
}

func TestTabPassthroughWhenOverlay(t *testing.T) {
	m0 := newMockMode(ModeBoard)
	m0.overlay = true // mode reports an overlay is open

	root := NewRootModel(RootOpts{
		TowerName: "test-tower",
		Identity:  "test-user",
		Modes:     []Mode{m0},
	})

	// Tab should be passed to the mode, not cycle.
	model, _ := root.Update(tea.KeyMsg{Type: tea.KeyTab})
	root = model.(RootModel)

	if root.activeModeIdx != 0 {
		t.Fatalf("expected activeModeIdx to stay 0 when overlay active, got %d", root.activeModeIdx)
	}
	if len(m0.updateMsgs) != 1 {
		t.Fatalf("expected 1 update message passed to mode, got %d", len(m0.updateMsgs))
	}
}

func TestWindowSizePropagation(t *testing.T) {
	m0 := newMockMode(ModeBoard)
	m1 := newMockMode(ModeAgents)
	m2 := newMockMode(ModeWorkshop)

	root := NewRootModel(RootOpts{
		TowerName: "test-tower",
		Identity:  "test-user",
		Modes:     []Mode{m0, m1, m2},
	})

	model, _ := root.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	root = model.(RootModel)

	expectedH := 40 - chromeHeight // 37
	for i, m := range []*mockMode{m0, m1, m2} {
		if len(m.setSizeCalls) != 1 {
			t.Fatalf("mode %d: expected 1 SetSize call, got %d", i, len(m.setSizeCalls))
		}
		if m.setSizeCalls[0][0] != 120 {
			t.Fatalf("mode %d: expected width=120, got %d", i, m.setSizeCalls[0][0])
		}
		if m.setSizeCalls[0][1] != expectedH {
			t.Fatalf("mode %d: expected height=%d, got %d", i, expectedH, m.setSizeCalls[0][1])
		}
	}
}

func TestTowerChangedPropagation(t *testing.T) {
	m0 := newMockMode(ModeBoard)
	m1 := newMockMode(ModeAgents)

	root := NewRootModel(RootOpts{
		TowerName:  "tower-a",
		Identity:   "test-user",
		BeadsDir:   "/tmp/beads",
		Modes:      []Mode{m0, m1},
		TowerNames: []string{"tower-a", "tower-b"},
	})

	// Open tower switcher.
	model, _ := root.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'T'}})
	root = model.(RootModel)
	if !root.showTowerSwitcher {
		t.Fatal("expected tower switcher to be open")
	}

	// Move to tower-b (cursor starts at 0 = tower-a).
	model, _ = root.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	root = model.(RootModel)
	if root.towerCursor != 1 {
		t.Fatalf("expected towerCursor=1, got %d", root.towerCursor)
	}

	// Select tower-b.
	model, _ = root.Update(tea.KeyMsg{Type: tea.KeyEnter})
	root = model.(RootModel)

	if root.showTowerSwitcher {
		t.Fatal("expected tower switcher to be closed after selection")
	}
	if root.towerName != "tower-b" {
		t.Fatalf("expected towerName='tower-b', got '%s'", root.towerName)
	}

	// Both modes should have received TowerChanged.
	for i, m := range []*mockMode{m0, m1} {
		if len(m.towerChangedCalls) != 1 {
			t.Fatalf("mode %d: expected 1 TowerChanged call, got %d", i, len(m.towerChangedCalls))
		}
		tc := m.towerChangedCalls[0]
		if tc.Name != "tower-b" {
			t.Fatalf("mode %d: expected TowerChanged.Name='tower-b', got '%s'", i, tc.Name)
		}
	}
}

func TestPendingActionMsgQuitsProgram(t *testing.T) {
	m0 := newMockMode(ModeBoard)

	root := NewRootModel(RootOpts{
		TowerName: "test-tower",
		Identity:  "test-user",
		Modes:     []Mode{m0},
	})

	action := PendingActionMsg{Action: "focus", Args: []string{"spi-abc"}}
	model, cmd := root.Update(action)
	root = model.(RootModel)

	if root.pendingAction == nil {
		t.Fatal("expected pendingAction to be set")
	}
	if root.pendingAction.Action != "focus" {
		t.Fatalf("expected action='focus', got '%s'", root.pendingAction.Action)
	}

	// Verify tea.Quit was returned.
	if cmd == nil {
		t.Fatal("expected a quit command")
	}
	// tea.Quit returns a QuitMsg when called.
	quitMsg := cmd()
	if _, ok := quitMsg.(tea.QuitMsg); !ok {
		t.Fatalf("expected tea.QuitMsg, got %T", quitMsg)
	}

	// Verify PendingAction() accessor.
	pa := root.PendingAction()
	if pa == nil || pa.Action != "focus" {
		t.Fatal("PendingAction() did not return the expected value")
	}
}

func TestQKeyQuitsWhenNoOverlay(t *testing.T) {
	m0 := newMockMode(ModeBoard)
	m0.overlay = false

	root := NewRootModel(RootOpts{
		TowerName: "test-tower",
		Identity:  "test-user",
		Modes:     []Mode{m0},
	})

	_, cmd := root.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})

	if cmd == nil {
		t.Fatal("expected a quit command for 'q' key")
	}
	quitMsg := cmd()
	if _, ok := quitMsg.(tea.QuitMsg); !ok {
		t.Fatalf("expected tea.QuitMsg, got %T", quitMsg)
	}
}

func TestQKeyPassedToModeWhenOverlay(t *testing.T) {
	m0 := newMockMode(ModeBoard)
	m0.overlay = true

	root := NewRootModel(RootOpts{
		TowerName: "test-tower",
		Identity:  "test-user",
		Modes:     []Mode{m0},
	})

	model, _ := root.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	root = model.(RootModel)

	// Should not quit; should pass to mode.
	if len(m0.updateMsgs) != 1 {
		t.Fatalf("expected q key passed to mode with overlay, got %d msgs", len(m0.updateMsgs))
	}
}

func TestInitCallsAllModes(t *testing.T) {
	m0 := newMockMode(ModeBoard)
	m1 := newMockMode(ModeAgents)

	root := NewRootModel(RootOpts{
		TowerName: "test-tower",
		Identity:  "test-user",
		Modes:     []Mode{m0, m1},
	})

	root.Init()

	if !m0.initCalled {
		t.Fatal("expected mode 0 Init to be called")
	}
	if !m1.initCalled {
		t.Fatal("expected mode 1 Init to be called")
	}
}

func TestTowerSwitcherEsc(t *testing.T) {
	root := NewRootModel(RootOpts{
		TowerName:  "tower-a",
		Identity:   "test-user",
		Modes:      []Mode{newMockMode(ModeBoard)},
		TowerNames: []string{"tower-a", "tower-b"},
	})

	// Open tower switcher.
	model, _ := root.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'T'}})
	root = model.(RootModel)
	if !root.showTowerSwitcher {
		t.Fatal("expected tower switcher open")
	}

	// Esc should close it.
	model, _ = root.Update(tea.KeyMsg{Type: tea.KeyEsc})
	root = model.(RootModel)
	if root.showTowerSwitcher {
		t.Fatal("expected tower switcher closed after Esc")
	}
}
