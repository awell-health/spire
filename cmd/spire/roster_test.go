package main

import (
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/board"
)

func TestBuildAttemptWorkMap_DeriveWorkFromAttemptBeads(t *testing.T) {
	inProgress := []BoardBead{
		{
			ID:        "spi-abc",
			Title:     "Fix auth bug",
			Status:    "in_progress",
			UpdatedAt: "2026-03-27T12:00:00Z",
		},
		{
			ID:        "spi-abc.1",
			Title:     "attempt: wizard-alpha",
			Status:    "in_progress",
			Labels:    []string{"attempt", "agent:wizard-alpha", "model:claude-opus-4-6", "branch:feat/spi-abc"},
			Parent:    "spi-abc",
			UpdatedAt: "2026-03-27T12:05:00Z",
		},
	}
	ownerWork := map[string]BoardBead{}

	work, updatedAt := board.BuildAttemptWorkMap(inProgress, ownerWork)

	if len(work) != 1 {
		t.Fatalf("expected 1 entry in attemptWork, got %d", len(work))
	}
	w, ok := work["wizard-alpha"]
	if !ok {
		t.Fatal("expected entry for wizard-alpha")
	}
	if w.ID != "spi-abc" {
		t.Errorf("work.ID = %q, want spi-abc", w.ID)
	}
	if w.Title != "Fix auth bug" {
		t.Errorf("work.Title = %q, want Fix auth bug", w.Title)
	}
	if updatedAt["wizard-alpha"] != "2026-03-27T12:05:00Z" {
		t.Errorf("updatedAt = %q, want attempt bead time", updatedAt["wizard-alpha"])
	}
}

func TestBuildAttemptWorkMap_SkipsIfCoveredByOwnerLabel(t *testing.T) {
	inProgress := []BoardBead{
		{
			ID:        "spi-xyz",
			Title:     "Some task",
			Status:    "in_progress",
			Labels:    []string{"owner:wizard-beta"},
			UpdatedAt: "2026-03-27T11:00:00Z",
		},
		{
			ID:        "spi-xyz.1",
			Title:     "attempt: wizard-beta",
			Status:    "in_progress",
			Labels:    []string{"attempt", "agent:wizard-beta"},
			Parent:    "spi-xyz",
			UpdatedAt: "2026-03-27T11:01:00Z",
		},
	}
	ownerWork := map[string]BoardBead{
		"wizard-beta": inProgress[0],
	}

	work, _ := board.BuildAttemptWorkMap(inProgress, ownerWork)
	if len(work) != 0 {
		t.Fatalf("expected no entries (agent covered by owner:), got %d", len(work))
	}
}

func TestBuildAttemptWorkMap_SkipsAttemptWithMissingParent(t *testing.T) {
	inProgress := []BoardBead{
		{
			ID:        "spi-orphan.1",
			Title:     "attempt: wizard-gamma",
			Status:    "in_progress",
			Labels:    []string{"attempt", "agent:wizard-gamma"},
			Parent:    "spi-orphan",
			UpdatedAt: "2026-03-27T13:00:00Z",
		},
	}
	ownerWork := map[string]BoardBead{}

	work, _ := board.BuildAttemptWorkMap(inProgress, ownerWork)
	if len(work) != 0 {
		t.Fatalf("expected no entries (parent not in inProgress), got %d", len(work))
	}
}

func TestBuildRosterWorkItems_CollapsesProcessesByBead(t *testing.T) {
	agents := []board.RosterAgent{
		{
			Name:      "wizard-spi-39u-impl",
			Status:    "working",
			BeadID:    "spi-39u",
			BeadTitle: "spire close command",
			EpicID:    "spi-yanq",
			EpicTitle: "Operationalize steward around backend-driven execution",
			Timeout:   15 * time.Minute,
		},
		{
			Name:         "wizard-spi-39u",
			Status:       "working",
			BeadID:       "spi-39u",
			BeadTitle:    "spire close command",
			EpicID:       "spi-yanq",
			EpicTitle:    "Operationalize steward around backend-driven execution",
			Phase:   "implement",
			Elapsed: 14 * time.Second,
			Timeout: 15 * time.Minute,
		},
	}

	items := board.BuildRosterWorkItems(agents)
	if len(items) != 1 {
		t.Fatalf("expected 1 work item, got %d", len(items))
	}

	item := items[0]
	if item.BeadID != "spi-39u" {
		t.Fatalf("expected bead spi-39u, got %q", item.BeadID)
	}
	if item.EpicID != "spi-yanq" {
		t.Fatalf("expected epic spi-yanq, got %q", item.EpicID)
	}
	if item.Phase != "implement" {
		t.Fatalf("expected implement phase, got %q", item.Phase)
	}
	if item.Elapsed != 14*time.Second {
		t.Fatalf("expected phase elapsed 14s, got %s", item.Elapsed)
	}
	if item.Timeout != 15*time.Minute {
		t.Fatalf("expected 15m timeout, got %s", item.Timeout)
	}
	if len(item.AgentNames) != 2 {
		t.Fatalf("expected 2 agent names, got %d", len(item.AgentNames))
	}
	if item.AgentNames[0] != "wizard-spi-39u" || item.AgentNames[1] != "wizard-spi-39u-impl" {
		t.Fatalf("unexpected agent names: %#v", item.AgentNames)
	}
}

func TestGroupRosterWorkItemsByEpic_GroupsStandaloneSeparately(t *testing.T) {
	items := []board.RosterWorkItem{
		{
			BeadID:    "spi-yanq.1",
			BeadTitle: "Remove broken summon targeting and group roster by epic",
			EpicID:    "spi-yanq",
			EpicTitle: "Operationalize steward around backend-driven execution",
			Status:    "working",
		},
		{
			BeadID:    "spi-free",
			BeadTitle: "Standalone task",
			Status:    "working",
		},
	}

	groups := board.GroupRosterWorkItemsByEpic(items)
	if len(groups) != 2 {
		t.Fatalf("expected 2 epic groups, got %d", len(groups))
	}
	if groups[0].ID != "spi-yanq" {
		t.Fatalf("expected first group to be spi-yanq, got %q", groups[0].ID)
	}
	if len(groups[0].Items) != 1 || groups[0].Items[0].BeadID != "spi-yanq.1" {
		t.Fatalf("unexpected items in first group: %#v", groups[0].Items)
	}
	if groups[1].ID != "" {
		t.Fatalf("expected second group to be standalone, got %q", groups[1].ID)
	}
	if groups[1].Title != "Standalone Work" {
		t.Fatalf("expected standalone title, got %q", groups[1].Title)
	}
	if len(groups[1].Items) != 1 || groups[1].Items[0].BeadID != "spi-free" {
		t.Fatalf("unexpected items in standalone group: %#v", groups[1].Items)
	}
}
