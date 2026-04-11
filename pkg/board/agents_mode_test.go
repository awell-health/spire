package board

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Compile-time check: AgentsMode must implement Mode.
var _ Mode = (*AgentsMode)(nil)

func TestAgentsModeViewWithAgents(t *testing.T) {
	m := NewAgentsMode("test-tower")
	m.SetSize(100, 40)
	m.snapshot = AgentSnapshot{
		Agents: []AgentInfo{
			{Name: "wizard-main", BeadID: "spi-a3f8", Phase: "implement", Status: "running", Duration: 12 * time.Minute},
			{Name: "wizard-web", BeadID: "web-b7d0", Phase: "review", Status: "idle", Duration: 0},
			{Name: "apprentice-1", BeadID: "spi-a3f8.2", Phase: "implement", Status: "running", Duration: 3 * time.Minute},
		},
		FetchedAt: time.Now(),
	}

	view := m.View()

	// Check header is present.
	if !strings.Contains(view, "Agent") || !strings.Contains(view, "Bead") || !strings.Contains(view, "Phase") {
		t.Errorf("View missing table headers, got:\n%s", view)
	}

	// Check agent names appear.
	if !strings.Contains(view, "wizard-main") {
		t.Errorf("View missing wizard-main, got:\n%s", view)
	}
	if !strings.Contains(view, "wizard-web") {
		t.Errorf("View missing wizard-web, got:\n%s", view)
	}
	if !strings.Contains(view, "apprentice-1") {
		t.Errorf("View missing apprentice-1, got:\n%s", view)
	}

	// Check bead IDs.
	if !strings.Contains(view, "spi-a3f8") {
		t.Errorf("View missing bead spi-a3f8, got:\n%s", view)
	}

	// Check statuses.
	if !strings.Contains(view, "running") {
		t.Errorf("View missing running status, got:\n%s", view)
	}
	if !strings.Contains(view, "idle") {
		t.Errorf("View missing idle status, got:\n%s", view)
	}

	// Check duration rendered.
	if !strings.Contains(view, "12m") {
		t.Errorf("View missing 12m duration, got:\n%s", view)
	}
}

func TestAgentsModeViewEmpty(t *testing.T) {
	m := NewAgentsMode("test-tower")
	m.SetSize(80, 40)
	m.snapshot = AgentSnapshot{
		Agents:    nil,
		FetchedAt: time.Now(),
	}

	view := m.View()

	if !strings.Contains(view, "No agents registered") {
		t.Errorf("View should show no-agents message, got:\n%s", view)
	}
	if !strings.Contains(view, "spire register") {
		t.Errorf("View should mention 'spire register', got:\n%s", view)
	}
}

func TestAgentsModeViewError(t *testing.T) {
	m := NewAgentsMode("test-tower")
	m.SetSize(80, 40)
	m.snapshot = AgentSnapshot{
		Error:     "registry directory not found",
		Agents:    nil,
		FetchedAt: time.Now(),
	}

	view := m.View()

	if !strings.Contains(view, "WARNING") {
		t.Errorf("View should show warning banner, got:\n%s", view)
	}
	if !strings.Contains(view, "registry directory not found") {
		t.Errorf("View should contain error message, got:\n%s", view)
	}
}

func TestAgentsModeID(t *testing.T) {
	m := NewAgentsMode("tower")
	if m.ID() != ModeAgents {
		t.Errorf("ID() = %v, want ModeAgents", m.ID())
	}
}

func TestAgentsModeHasOverlay(t *testing.T) {
	m := NewAgentsMode("tower")
	if m.HasOverlay() {
		t.Error("HasOverlay() should return false initially")
	}

	m.ConfirmOpen = true
	if !m.HasOverlay() {
		t.Error("HasOverlay() should return true when ConfirmOpen")
	}

	m.ConfirmOpen = false
	m.ActionMenuOpen = true
	if !m.HasOverlay() {
		t.Error("HasOverlay() should return true when ActionMenuOpen")
	}
}

// --- AgentsMode.FooterHints tests ---

func TestAgentsModeFooterHints(t *testing.T) {
	t.Run("default with agents", func(t *testing.T) {
		m := NewAgentsMode("tower")
		m.snapshot = AgentSnapshot{
			Agents: []AgentInfo{
				{Name: "wizard-main", BeadID: "spi-001", Status: "running"},
			},
		}
		hints := m.FooterHints()
		for _, want := range []string{"unsummon", "reset", "close", "actions", "inspect", "copy"} {
			if !strings.Contains(hints, want) {
				t.Errorf("default hints missing %q, got %q", want, hints)
			}
		}
	})

	t.Run("empty agents", func(t *testing.T) {
		m := NewAgentsMode("tower")
		m.snapshot = AgentSnapshot{Agents: nil}
		hints := m.FooterHints()
		if hints != "" {
			t.Errorf("expected empty hints for no agents, got %q", hints)
		}
	})

	t.Run("ConfirmOpen overlay", func(t *testing.T) {
		m := NewAgentsMode("tower")
		m.snapshot = AgentSnapshot{
			Agents: []AgentInfo{{Name: "wizard-main", BeadID: "spi-001", Status: "running"}},
		}
		m.ConfirmOpen = true
		hints := m.FooterHints()
		if !strings.Contains(hints, "confirm") || !strings.Contains(hints, "cancel") {
			t.Errorf("ConfirmOpen hints should show confirm/cancel, got %q", hints)
		}
		// Should NOT show default hints.
		if strings.Contains(hints, "unsummon") {
			t.Errorf("ConfirmOpen should override default hints, got %q", hints)
		}
	})

	t.Run("ActionMenuOpen overlay", func(t *testing.T) {
		m := NewAgentsMode("tower")
		m.snapshot = AgentSnapshot{
			Agents: []AgentInfo{{Name: "wizard-main", BeadID: "spi-001", Status: "running"}},
		}
		m.ActionMenuOpen = true
		hints := m.FooterHints()
		if !strings.Contains(hints, "navigate") || !strings.Contains(hints, "select") {
			t.Errorf("ActionMenuOpen hints should show navigate/select, got %q", hints)
		}
		if strings.Contains(hints, "unsummon") {
			t.Errorf("ActionMenuOpen should override default hints, got %q", hints)
		}
	})
}

// --- AgentsMode key handler tests ---

// updateAgentsMode is a helper that runs Update and returns the concrete *AgentsMode.
func updateAgentsMode(m *AgentsMode, msg tea.Msg) (*AgentsMode, tea.Cmd) {
	result, cmd := m.Update(msg)
	return result.(*AgentsMode), cmd
}

func makeAgentsModeWithAgents() *AgentsMode {
	m := NewAgentsMode("test-tower")
	m.SetSize(100, 40)
	m.snapshot = AgentSnapshot{
		Agents: []AgentInfo{
			{Name: "wizard-main", BeadID: "spi-001", Phase: "implement", Status: "running"},
			{Name: "wizard-idle", BeadID: "", Phase: "", Status: "idle"},
			{Name: "wizard-err", BeadID: "spi-003", Phase: "review", Status: "errored"},
		},
	}
	return m
}

func TestAgentsModeKeyU_Unsummon(t *testing.T) {
	m := makeAgentsModeWithAgents()
	// Cursor is on agent[0] (running, has bead).
	m, _ = updateAgentsMode(m, keyMsg('u'))

	if !m.ConfirmOpen {
		t.Fatal("pressing 'u' on running agent should open confirm dialog")
	}
	if m.ConfirmAction != ActionUnsummon {
		t.Errorf("ConfirmAction = %d, want ActionUnsummon (%d)", m.ConfirmAction, ActionUnsummon)
	}
	if m.ConfirmBeadID != "spi-001" {
		t.Errorf("ConfirmBeadID = %q, want %q", m.ConfirmBeadID, "spi-001")
	}
}

func TestAgentsModeKeyU_NoOpOnIdle(t *testing.T) {
	m := makeAgentsModeWithAgents()
	// Move cursor to agent[1] (idle, no bead).
	m.cursor = 1
	m, _ = updateAgentsMode(m, keyMsg('u'))

	if m.ConfirmOpen {
		t.Error("pressing 'u' on idle agent without bead should not open confirm")
	}
}

func TestAgentsModeKeyR_Reset(t *testing.T) {
	m := makeAgentsModeWithAgents()
	// Cursor on agent[0] (running, has bead).
	m, _ = updateAgentsMode(m, keyMsg('r'))

	if !m.ConfirmOpen {
		t.Fatal("pressing 'r' on agent with bead should open confirm dialog")
	}
	if m.ConfirmAction != ActionResetSoft {
		t.Errorf("ConfirmAction = %d, want ActionResetSoft (%d)", m.ConfirmAction, ActionResetSoft)
	}
	if m.ConfirmBeadID != "spi-001" {
		t.Errorf("ConfirmBeadID = %q, want %q", m.ConfirmBeadID, "spi-001")
	}
}

func TestAgentsModeKeyX_Close(t *testing.T) {
	m := makeAgentsModeWithAgents()
	m, _ = updateAgentsMode(m, keyMsg('x'))

	if !m.ConfirmOpen {
		t.Fatal("pressing 'x' on agent with bead should open confirm dialog")
	}
	if m.ConfirmAction != ActionClose {
		t.Errorf("ConfirmAction = %d, want ActionClose (%d)", m.ConfirmAction, ActionClose)
	}
}

func TestAgentsModeKeyA_ActionMenu(t *testing.T) {
	m := makeAgentsModeWithAgents()
	m, _ = updateAgentsMode(m, keyMsg('a'))

	if !m.ActionMenuOpen {
		t.Fatal("pressing 'a' on agent with bead should open action menu")
	}
	if m.ActionMenuBeadID != "spi-001" {
		t.Errorf("ActionMenuBeadID = %q, want %q", m.ActionMenuBeadID, "spi-001")
	}
	if len(m.ActionMenuItems) == 0 {
		t.Error("ActionMenuItems should be populated")
	}
	if m.ActionMenuCursor != 0 {
		t.Errorf("ActionMenuCursor = %d, want 0", m.ActionMenuCursor)
	}
}

func TestAgentsModeKeyA_NoOpWithoutBead(t *testing.T) {
	m := makeAgentsModeWithAgents()
	m.cursor = 1 // idle, no bead
	m, _ = updateAgentsMode(m, keyMsg('a'))

	if m.ActionMenuOpen {
		t.Error("pressing 'a' on agent without bead should not open action menu")
	}
}

func TestAgentsModeKeyEnter_Inspect(t *testing.T) {
	m := makeAgentsModeWithAgents()
	_, cmd := updateAgentsMode(m, keyMsgType(tea.KeyEnter))

	if cmd == nil {
		t.Fatal("pressing Enter should produce a tea.Cmd")
	}
	msg := cmd()
	pending, ok := msg.(PendingActionMsg)
	if !ok {
		t.Fatalf("expected PendingActionMsg, got %T", msg)
	}
	if pending.Action != "focus" {
		t.Errorf("PendingActionMsg.Action = %q, want %q", pending.Action, "focus")
	}
	if len(pending.Args) == 0 || pending.Args[0] != "spi-001" {
		t.Errorf("PendingActionMsg.Args = %v, want [spi-001]", pending.Args)
	}
}

func TestAgentsModeKeyEnter_NoOpWithoutBead(t *testing.T) {
	m := makeAgentsModeWithAgents()
	m.cursor = 1 // idle, no bead
	_, cmd := updateAgentsMode(m, keyMsgType(tea.KeyEnter))

	if cmd != nil {
		t.Error("pressing Enter on agent without bead should not produce a command")
	}
}

func TestAgentsModeConfirmDialog(t *testing.T) {
	t.Run("confirm with y dispatches action", func(t *testing.T) {
		m := makeAgentsModeWithAgents()
		// Open confirm via 'r' key.
		m, _ = updateAgentsMode(m, keyMsg('r'))
		if !m.ConfirmOpen {
			t.Fatal("expected confirm to be open")
		}

		// Set up an InlineActionFn to capture the dispatch.
		var dispatchedAction PendingAction
		var dispatchedBead string
		m.InlineActionFn = func(a PendingAction, beadID string) error {
			dispatchedAction = a
			dispatchedBead = beadID
			return nil
		}

		// Press 'y' to confirm.
		m, cmd := updateAgentsMode(m, keyMsg('y'))
		if m.ConfirmOpen {
			t.Error("confirm should be closed after 'y'")
		}
		if !m.ActionRunning {
			t.Error("ActionRunning should be true after confirm")
		}
		// Execute the cmd to trigger the inline action.
		if cmd != nil {
			msg := cmd()
			result, ok := msg.(agentActionResultMsg)
			if !ok {
				t.Fatalf("expected agentActionResultMsg, got %T", msg)
			}
			if result.Err != nil {
				t.Errorf("unexpected error: %v", result.Err)
			}
		}
		if dispatchedAction != ActionResetSoft {
			t.Errorf("dispatched action = %d, want ActionResetSoft", dispatchedAction)
		}
		if dispatchedBead != "spi-001" {
			t.Errorf("dispatched bead = %q, want %q", dispatchedBead, "spi-001")
		}
	})

	t.Run("cancel with esc closes confirm", func(t *testing.T) {
		m := makeAgentsModeWithAgents()
		m, _ = updateAgentsMode(m, keyMsg('x'))
		if !m.ConfirmOpen {
			t.Fatal("expected confirm to be open")
		}

		m, _ = updateAgentsMode(m, keyMsgType(tea.KeyEsc))
		if m.ConfirmOpen {
			t.Error("confirm should be closed after Esc")
		}
	})

	t.Run("cancel with n closes confirm", func(t *testing.T) {
		m := makeAgentsModeWithAgents()
		m, _ = updateAgentsMode(m, keyMsg('u'))
		if !m.ConfirmOpen {
			t.Fatal("expected confirm to be open")
		}

		m, _ = updateAgentsMode(m, keyMsg('n'))
		if m.ConfirmOpen {
			t.Error("confirm should be closed after 'n'")
		}
	})
}

func TestAgentsModeActionMenuNavigation(t *testing.T) {
	m := makeAgentsModeWithAgents()
	// Open action menu.
	m, _ = updateAgentsMode(m, keyMsg('a'))
	if !m.ActionMenuOpen {
		t.Fatal("expected action menu to be open")
	}

	initialItems := len(m.ActionMenuItems)
	if initialItems == 0 {
		t.Fatal("expected action menu items")
	}

	// Navigate down.
	m, _ = updateAgentsMode(m, keyMsg('j'))
	if m.ActionMenuCursor != 1 {
		t.Errorf("after j: cursor = %d, want 1", m.ActionMenuCursor)
	}

	// Navigate up.
	m, _ = updateAgentsMode(m, keyMsg('k'))
	if m.ActionMenuCursor != 0 {
		t.Errorf("after k: cursor = %d, want 0", m.ActionMenuCursor)
	}

	// Close with esc.
	m, _ = updateAgentsMode(m, keyMsgType(tea.KeyEsc))
	if m.ActionMenuOpen {
		t.Error("action menu should be closed after Esc")
	}
}

func TestAgentsModeActionMenuSelect(t *testing.T) {
	m := makeAgentsModeWithAgents()
	// Open action menu on running agent.
	m, _ = updateAgentsMode(m, keyMsg('a'))
	if !m.ActionMenuOpen {
		t.Fatal("expected action menu to be open")
	}

	// Select first item with Enter — should close menu and open confirm or dispatch.
	m, _ = updateAgentsMode(m, keyMsgType(tea.KeyEnter))
	if m.ActionMenuOpen {
		t.Error("action menu should close after Enter select")
	}
}

func TestAgentsModeNavigationJK(t *testing.T) {
	m := makeAgentsModeWithAgents()
	if m.cursor != 0 {
		t.Fatalf("initial cursor = %d, want 0", m.cursor)
	}

	// j moves down.
	m, _ = updateAgentsMode(m, keyMsg('j'))
	if m.cursor != 1 {
		t.Errorf("after j: cursor = %d, want 1", m.cursor)
	}

	// k moves up.
	m, _ = updateAgentsMode(m, keyMsg('k'))
	if m.cursor != 0 {
		t.Errorf("after k: cursor = %d, want 0", m.cursor)
	}

	// Wraps around.
	m, _ = updateAgentsMode(m, keyMsg('k'))
	if m.cursor != 2 {
		t.Errorf("after k from 0: cursor = %d, want 2 (wrap)", m.cursor)
	}
}

func TestAgentsModeNoOpOnEmptyAgents(t *testing.T) {
	m := NewAgentsMode("tower")
	m.snapshot = AgentSnapshot{Agents: nil}

	// All action keys should be no-ops.
	for _, key := range []rune{'u', 'r', 'x', 'a', 'y'} {
		m, _ = updateAgentsMode(m, keyMsg(key))
		if m.ConfirmOpen || m.ActionMenuOpen {
			t.Errorf("key '%c' should be no-op with empty agents", key)
		}
	}
}

func TestAgentsModeConfirmAbsorbsKeys(t *testing.T) {
	m := makeAgentsModeWithAgents()
	// Open confirm dialog.
	m, _ = updateAgentsMode(m, keyMsg('r'))
	if !m.ConfirmOpen {
		t.Fatal("expected confirm open")
	}

	// Other keys should not change cursor or open action menu.
	prevCursor := m.cursor
	m, _ = updateAgentsMode(m, keyMsg('j'))
	if m.cursor != prevCursor {
		t.Error("confirm dialog should absorb j key")
	}
	m, _ = updateAgentsMode(m, keyMsg('a'))
	if m.ActionMenuOpen {
		t.Error("confirm dialog should absorb 'a' key")
	}
}

func TestAgentsModeDispatchWithoutInlineActionFn(t *testing.T) {
	m := makeAgentsModeWithAgents()
	m.InlineActionFn = nil
	// Open confirm, then confirm.
	m, _ = updateAgentsMode(m, keyMsg('r'))
	m, cmd := updateAgentsMode(m, keyMsg('y'))

	if cmd == nil {
		t.Fatal("expected PendingActionMsg when InlineActionFn is nil")
	}
	msg := cmd()
	pending, ok := msg.(PendingActionMsg)
	if !ok {
		t.Fatalf("expected PendingActionMsg, got %T", msg)
	}
	if pending.Action == "" {
		t.Error("PendingActionMsg.Action should not be empty")
	}
	if len(pending.Args) == 0 || pending.Args[0] != "spi-001" {
		t.Errorf("PendingActionMsg.Args = %v, want [spi-001]", pending.Args)
	}
	_ = m
}
