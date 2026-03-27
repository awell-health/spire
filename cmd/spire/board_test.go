package main

import "testing"

func TestCalcHeightBudget_NoTerminal(t *testing.T) {
	b := calcHeightBudget(0, 3, 5, 4, 0)
	if b.maxCards < 10 {
		t.Errorf("expected permissive maxCards for non-TTY, got %d", b.maxCards)
	}
	if b.compact {
		t.Error("expected compact=false for non-TTY")
	}
	if b.maxAlerts != 3 {
		t.Errorf("expected maxAlerts=3, got %d", b.maxAlerts)
	}
	if b.maxBlocked < 5 {
		t.Errorf("expected maxBlocked >= 5 for non-TTY with 5 blocked, got %d", b.maxBlocked)
	}
}

func TestCalcHeightBudget_TallTerminal(t *testing.T) {
	// 50 rows: plenty of space for regular cards.
	b := calcHeightBudget(50, 0, 0, 4, 0)
	if b.compact {
		t.Errorf("expected compact=false for tall terminal, got compact=true (maxCards=%d)", b.maxCards)
	}
	if b.maxCards < 5 {
		t.Errorf("expected maxCards >= 5 for 50-row terminal, got %d", b.maxCards)
	}
}

func TestCalcHeightBudget_ShortTerminal(t *testing.T) {
	// 12 rows: very tight — should trigger compact mode.
	b := calcHeightBudget(12, 0, 0, 4, 0)
	if !b.compact {
		t.Errorf("expected compact=true for 12-row terminal, got compact=false (maxCards=%d)", b.maxCards)
	}
	if b.maxCards < 1 {
		t.Error("maxCards must be at least 1")
	}
}

func TestCalcHeightBudget_AlertsCapped(t *testing.T) {
	// 30 rows, 10 alerts: alerts should be capped well below 10.
	b := calcHeightBudget(30, 10, 0, 4, 0)
	if b.maxAlerts >= 10 {
		t.Errorf("expected maxAlerts < 10 for 30-row terminal with 10 alerts, got %d", b.maxAlerts)
	}
	if b.maxAlerts < 1 {
		t.Error("maxAlerts must be at least 1")
	}
}

func TestCalcHeightBudget_BlockedCapped(t *testing.T) {
	// 30 rows, 10 blocked: blocked should be capped.
	b := calcHeightBudget(30, 0, 10, 4, 0)
	if b.maxBlocked >= 10 {
		t.Errorf("expected maxBlocked < 10 for 30-row terminal with 10 blocked, got %d", b.maxBlocked)
	}
	if b.maxBlocked < 1 {
		t.Error("maxBlocked must be at least 1")
	}
}

func TestCalcHeightBudget_AlertsAndBlockedFewItems(t *testing.T) {
	// When actual counts are small, caps should not exceed actual counts.
	b := calcHeightBudget(50, 2, 3, 4, 0)
	if b.maxAlerts > 2 {
		t.Errorf("maxAlerts should not exceed alertCount=2, got %d", b.maxAlerts)
	}
	if b.maxBlocked > 3 {
		t.Errorf("maxBlocked should not exceed blockedCount=3, got %d", b.maxBlocked)
	}
}

func TestCalcHeightBudget_AgentsCapped(t *testing.T) {
	// 50 rows, 8 agents: agent panel should be capped at 5.
	b := calcHeightBudget(50, 0, 0, 4, 8)
	if b.maxAgents > 5 {
		t.Errorf("maxAgents should not exceed 5, got %d", b.maxAgents)
	}
	if b.maxAgents < 1 {
		t.Error("maxAgents must be at least 1 when agents > 0")
	}
}

func TestCalcHeightBudget_AgentsZeroWhenNoAgents(t *testing.T) {
	b := calcHeightBudget(50, 0, 0, 4, 0)
	if b.maxAgents != 0 {
		t.Errorf("maxAgents should be 0 when agentCount=0, got %d", b.maxAgents)
	}
}

func TestSortBeads_PriorityThenDate(t *testing.T) {
	beads := []BoardBead{
		{ID: "spi-p2-old", Priority: 2, UpdatedAt: "2026-03-25T10:00:00Z"},
		{ID: "spi-p1-old", Priority: 1, UpdatedAt: "2026-03-25T09:00:00Z"},
		{ID: "spi-p1-new", Priority: 1, UpdatedAt: "2026-03-26T09:00:00Z"},
		{ID: "spi-p2-new", Priority: 2, UpdatedAt: "2026-03-26T10:00:00Z"},
	}

	sortBeads(beads)

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

	sortBeads(beads)

	if beads[0].ID != "spi-blocked-new" || beads[1].ID != "spi-blocked-old" {
		t.Fatalf("created_at fallback sort mismatch: %#v", beads)
	}
}

func TestBoardTypeScopeNext(t *testing.T) {
	scope := boardTypeAll
	want := []boardTypeScope{
		boardTypeTask,
		boardTypeBug,
		boardTypeEpic,
		boardTypeDesign,
		boardTypeDecision,
		boardTypeOther,
		boardTypeAll,
	}
	for i, expected := range want {
		scope = scope.next()
		if scope != expected {
			t.Fatalf("step %d: expected %v, got %v", i, expected, scope)
		}
	}
}

func TestFilterBoardTypeScope(t *testing.T) {
	cols := boardColumns{
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

	taskOnly := filterBoardTypeScope(cols, boardTypeTask)
	if len(taskOnly.Ready) != 1 || taskOnly.Ready[0].ID != "spi-task" {
		t.Fatalf("task filter mismatch: %#v", taskOnly.Ready)
	}
	if len(taskOnly.Alerts) != 0 || len(taskOnly.Review) != 0 || len(taskOnly.Blocked) != 0 {
		t.Fatalf("task filter leaked non-task beads: %#v", taskOnly)
	}

	decisionOnly := filterBoardTypeScope(cols, boardTypeDecision)
	if len(decisionOnly.Alerts) != 1 || decisionOnly.Alerts[0].ID != "spi-decision" {
		t.Fatalf("decision filter mismatch: %#v", decisionOnly.Alerts)
	}

	otherOnly := filterBoardTypeScope(cols, boardTypeOther)
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
	m := boardModel{
		cols: boardColumns{
			Ready: []BoardBead{
				{ID: "spi-task", Type: "task"},
				{ID: "spi-bug", Type: "bug"},
			},
		},
		typeScope: boardTypeBug,
	}

	m.clampSelection()
	bead := m.selectedBead()
	if bead == nil || bead.ID != "spi-bug" {
		t.Fatalf("expected filtered selected bead spi-bug, got %#v", bead)
	}
}

func TestShortTypeDecision(t *testing.T) {
	if got := shortType("decision"); got != "dec" {
		t.Fatalf("shortType(decision) = %q, want %q", got, "dec")
	}
}
