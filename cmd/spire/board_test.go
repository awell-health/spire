package main

import (
	"testing"

	"github.com/awell-health/spire/pkg/board"
)

func TestCalcHeightBudget_NoTerminal(t *testing.T) {
	b := board.CalcHeightBudget(0, 0, 3, 0, 5, 4, 0)
	if b.MaxCards < 10 {
		t.Errorf("expected permissive maxCards for non-TTY, got %d", b.MaxCards)
	}
	if b.Compact {
		t.Error("expected compact=false for non-TTY")
	}
	if b.MaxAlerts != 3 {
		t.Errorf("expected maxAlerts=3, got %d", b.MaxAlerts)
	}
	if b.MaxBlocked < 5 {
		t.Errorf("expected maxBlocked >= 5 for non-TTY with 5 blocked, got %d", b.MaxBlocked)
	}
}

func TestCalcHeightBudget_TallTerminal(t *testing.T) {
	b := board.CalcHeightBudget(50, 0, 0, 0, 0, 4, 0)
	if b.Compact {
		t.Errorf("expected compact=false for tall terminal, got compact=true (maxCards=%d)", b.MaxCards)
	}
	if b.MaxCards < 5 {
		t.Errorf("expected maxCards >= 5 for 50-row terminal, got %d", b.MaxCards)
	}
}

func TestCalcHeightBudget_ShortTerminal(t *testing.T) {
	b := board.CalcHeightBudget(12, 0, 0, 0, 0, 4, 0)
	if !b.Compact {
		t.Errorf("expected compact=true for 12-row terminal, got compact=false (maxCards=%d)", b.MaxCards)
	}
	if b.MaxCards < 1 {
		t.Error("maxCards must be at least 1")
	}
}

func TestCalcHeightBudget_AlertsCapped(t *testing.T) {
	b := board.CalcHeightBudget(30, 0, 10, 0, 0, 4, 0)
	if b.MaxAlerts >= 10 {
		t.Errorf("expected maxAlerts < 10 for 30-row terminal with 10 alerts, got %d", b.MaxAlerts)
	}
	if b.MaxAlerts < 1 {
		t.Error("maxAlerts must be at least 1")
	}
}

func TestCalcHeightBudget_BlockedCapped(t *testing.T) {
	b := board.CalcHeightBudget(30, 0, 0, 0, 10, 4, 0)
	if b.MaxBlocked >= 10 {
		t.Errorf("expected maxBlocked < 10 for 30-row terminal with 10 blocked, got %d", b.MaxBlocked)
	}
	if b.MaxBlocked < 1 {
		t.Error("maxBlocked must be at least 1")
	}
}

func TestCalcHeightBudget_AlertsAndBlockedFewItems(t *testing.T) {
	b := board.CalcHeightBudget(50, 0, 2, 0, 3, 4, 0)
	if b.MaxAlerts > 2 {
		t.Errorf("maxAlerts should not exceed alertCount=2, got %d", b.MaxAlerts)
	}
	if b.MaxBlocked > 3 {
		t.Errorf("maxBlocked should not exceed blockedCount=3, got %d", b.MaxBlocked)
	}
}

func TestCalcHeightBudget_AgentsCapped(t *testing.T) {
	b := board.CalcHeightBudget(50, 0, 0, 0, 0, 4, 8)
	if b.MaxAgents > 5 {
		t.Errorf("maxAgents should not exceed 5, got %d", b.MaxAgents)
	}
	if b.MaxAgents < 1 {
		t.Error("maxAgents must be at least 1 when agents > 0")
	}
}

func TestCalcHeightBudget_AgentsZeroWhenNoAgents(t *testing.T) {
	b := board.CalcHeightBudget(50, 0, 0, 0, 0, 4, 0)
	if b.MaxAgents != 0 {
		t.Errorf("maxAgents should be 0 when agentCount=0, got %d", b.MaxAgents)
	}
}

func TestCalcHeightBudget_WarningsAllocated(t *testing.T) {
	b := board.CalcHeightBudget(50, 2, 3, 0, 0, 4, 0)
	if b.MaxWarnings != 2 {
		t.Errorf("maxWarnings should be 2, got %d", b.MaxWarnings)
	}
	if b.MaxAlerts != 3 {
		t.Errorf("maxAlerts should be 3, got %d", b.MaxAlerts)
	}
}

func TestSortBeads_PriorityThenDate(t *testing.T) {
	beads := []BoardBead{
		{ID: "spi-p2-old", Priority: 2, UpdatedAt: "2026-03-25T10:00:00Z"},
		{ID: "spi-p1-old", Priority: 1, UpdatedAt: "2026-03-25T09:00:00Z"},
		{ID: "spi-p1-new", Priority: 1, UpdatedAt: "2026-03-26T09:00:00Z"},
		{ID: "spi-p2-new", Priority: 2, UpdatedAt: "2026-03-26T10:00:00Z"},
	}

	board.SortBeads(beads)

	want := []string{"spi-p1-new", "spi-p1-old", "spi-p2-new", "spi-p2-old"}
	for i, id := range want {
		if beads[i].ID != id {
			t.Fatalf("index %d: expected %s, got %s", i, id, beads[i].ID)
		}
	}
}

func TestSortBeads_FallsBackToCreatedAt(t *testing.T) {
	beads := []BoardBead{
		{ID: "spi-blocked-old", Priority: 1, CreatedAt: "2026-03-25 09:00:00"},
		{ID: "spi-blocked-new", Priority: 1, CreatedAt: "2026-03-26T09:00:00Z"},
	}

	board.SortBeads(beads)

	if beads[0].ID != "spi-blocked-new" || beads[1].ID != "spi-blocked-old" {
		t.Fatalf("created_at fallback sort mismatch: %#v", beads)
	}
}

func TestBoardTypeScopeNext(t *testing.T) {
	scope := board.TypeAll
	want := []board.TypeScope{
		board.TypeTask,
		board.TypeBug,
		board.TypeEpic,
		board.TypeDesign,
		board.TypeDecision,
		board.TypeOther,
		board.TypeAll,
	}
	for i, expected := range want {
		scope = scope.Next()
		if scope != expected {
			t.Fatalf("step %d: expected %v, got %v", i, expected, scope)
		}
	}
}

func TestFilterBoardTypeScope(t *testing.T) {
	cols := board.Columns{
		Alerts: []BoardBead{
			{ID: "spi-decision", Type: "decision"},
			{ID: "spi-bug", Type: "bug"},
		},
		Ready: []BoardBead{
			{ID: "spi-task", Type: "task"},
			{ID: "spi-feature", Type: "feature"},
			{ID: "spi-design", Type: "design"},
		},
		Review: []BoardBead{
			{ID: "spi-epic", Type: "epic"},
		},
		Blocked: []BoardBead{
			{ID: "spi-chore", Type: "chore"},
		},
	}

	taskOnly := board.FilterTypeScope(cols, board.TypeTask)
	if len(taskOnly.Ready) != 1 || taskOnly.Ready[0].ID != "spi-task" {
		t.Fatalf("task filter mismatch: %#v", taskOnly.Ready)
	}
	if len(taskOnly.Alerts) != 0 || len(taskOnly.Review) != 0 || len(taskOnly.Blocked) != 0 {
		t.Fatalf("task filter leaked non-task beads: %#v", taskOnly)
	}

	decisionOnly := board.FilterTypeScope(cols, board.TypeDecision)
	if len(decisionOnly.Alerts) != 1 || decisionOnly.Alerts[0].ID != "spi-decision" {
		t.Fatalf("decision filter mismatch: %#v", decisionOnly.Alerts)
	}

	otherOnly := board.FilterTypeScope(cols, board.TypeOther)
	if len(otherOnly.Ready) != 1 || otherOnly.Ready[0].ID != "spi-feature" {
		t.Fatalf("other filter ready mismatch: %#v", otherOnly.Ready)
	}
	if len(otherOnly.Blocked) != 1 || otherOnly.Blocked[0].ID != "spi-chore" {
		t.Fatalf("other filter blocked mismatch: %#v", otherOnly.Blocked)
	}
	if len(otherOnly.Alerts) != 0 || len(otherOnly.Review) != 0 {
		t.Fatalf("other filter leaked core types: %#v", otherOnly)
	}
}

func TestBoardModelSelectedBeadUsesTypeScope(t *testing.T) {
	m := board.BoardMode{
		Cols: board.Columns{
			Ready: []BoardBead{
				{ID: "spi-task", Type: "task"},
				{ID: "spi-bug", Type: "bug"},
			},
		},
		TypeScope: board.TypeBug,
	}

	m.ClampSelection()
	bead := m.SelectedBead()
	if bead == nil || bead.ID != "spi-bug" {
		t.Fatalf("expected filtered selected bead spi-bug, got %#v", bead)
	}
}

func TestShortTypeDecision(t *testing.T) {
	if got := board.ShortType("decision"); got != "dec" {
		t.Fatalf("shortType(decision) = %q, want %q", got, "dec")
	}
}

func TestAllColumnsIncludesEmpty(t *testing.T) {
	cols := board.Columns{
		Ready:      []BoardBead{{ID: "spi-1", Type: "task"}},
		InProgress: []BoardBead{{ID: "spi-2", Type: "task"}},
	}
	active := board.ActiveColumns(cols)
	all := board.AllColumns(cols)
	if len(active) != 2 {
		t.Fatalf("expected 2 active columns, got %d", len(active))
	}
	if len(all) != 5 {
		t.Fatalf("expected 5 total columns, got %d", len(all))
	}
}

func TestShowAllColsToggle(t *testing.T) {
	cols := board.Columns{
		Ready: []BoardBead{{ID: "spi-1", Type: "task"}},
	}
	m := board.BoardMode{Cols: cols}

	// Default: only non-empty columns
	display := m.DisplayColumns()
	if len(display) != 1 {
		t.Fatalf("expected 1 display column with ShowAllCols=false, got %d", len(display))
	}

	// Toggle on: all status columns (BACKLOG, READY, IN PROGRESS, HOOKED, DONE)
	m.ShowAllCols = true
	display = m.DisplayColumns()
	if len(display) != 5 {
		t.Fatalf("expected 5 display columns with ShowAllCols=true, got %d", len(display))
	}

	// Selection should work with empty columns
	m.SelCol = 2 // IN PROGRESS (empty)
	m.ClampSelection()
	bead := m.SelectedBead()
	if bead != nil {
		t.Fatalf("expected nil bead for empty column, got %v", bead)
	}

	// Navigate to READY (index 1) which has a bead
	m.SelCol = 1
	m.SelCard = 0
	bead = m.SelectedBead()
	if bead == nil || bead.ID != "spi-1" {
		t.Fatalf("expected spi-1, got %v", bead)
	}
}
