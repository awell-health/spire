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
