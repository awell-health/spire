package board

import (
	"strings"
	"testing"
	"time"
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
		t.Error("HasOverlay() should return false")
	}
}
