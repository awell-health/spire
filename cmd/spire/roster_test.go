package main

import (
	"testing"
	"time"
)

func TestBuildRosterWorkItems_CollapsesProcessesByBead(t *testing.T) {
	agents := []RosterAgent{
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
			Phase:        "implement",
			PhaseElapsed: 14 * time.Second,
			Timeout:      15 * time.Minute,
		},
	}

	items := buildRosterWorkItems(agents)
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
	items := []rosterWorkItem{
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

	groups := groupRosterWorkItemsByEpic(items)
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
