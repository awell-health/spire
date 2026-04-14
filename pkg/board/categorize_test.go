package board

import (
	"testing"
)

func TestCategorizeWithPhases_OpenBeadsGoToBacklog(t *testing.T) {
	open := []BoardBead{
		{ID: "spi-001", Title: "Open task", Status: "open", Type: "task", Priority: 2},
	}
	cols := CategorizeWithPhases(open, nil, nil, map[string]string{}, "me")
	if len(cols.Backlog) != 1 || cols.Backlog[0].ID != "spi-001" {
		t.Errorf("expected open bead in Backlog, got Backlog=%v", cols.Backlog)
	}
	if len(cols.Ready) != 0 {
		t.Errorf("expected no beads in Ready, got %v", cols.Ready)
	}
}

func TestCategorizeWithPhases_ReadyBeadsGoToReady(t *testing.T) {
	open := []BoardBead{
		{ID: "spi-002", Title: "Ready task", Status: "ready", Type: "task", Priority: 1},
	}
	cols := CategorizeWithPhases(open, nil, nil, map[string]string{}, "me")
	if len(cols.Ready) != 1 || cols.Ready[0].ID != "spi-002" {
		t.Errorf("expected ready bead in Ready, got Ready=%v", cols.Ready)
	}
	if len(cols.Backlog) != 0 {
		t.Errorf("expected no beads in Backlog, got %v", cols.Backlog)
	}
}

func TestCategorizeWithPhases_DeferredBeadsGoToBacklog(t *testing.T) {
	open := []BoardBead{
		{ID: "spi-003", Title: "Deferred task", Status: "deferred", Type: "task", Priority: 3},
	}
	cols := CategorizeWithPhases(open, nil, nil, map[string]string{}, "me")
	if len(cols.Backlog) != 1 || cols.Backlog[0].ID != "spi-003" {
		t.Errorf("expected deferred bead in Backlog, got Backlog=%v", cols.Backlog)
	}
	if len(cols.Ready) != 0 {
		t.Errorf("expected no beads in Ready, got %v", cols.Ready)
	}
}

func TestCategorizeWithPhases_DeferredSortedAfterOpen(t *testing.T) {
	open := []BoardBead{
		{ID: "spi-d1", Title: "Deferred", Status: "deferred", Type: "task", Priority: 1},
		{ID: "spi-o1", Title: "Open", Status: "open", Type: "task", Priority: 1},
	}
	cols := CategorizeWithPhases(open, nil, nil, map[string]string{}, "me")
	if len(cols.Backlog) != 2 {
		t.Fatalf("expected 2 beads in Backlog, got %d", len(cols.Backlog))
	}
	SortBeads(cols.Backlog)
	if cols.Backlog[0].ID != "spi-o1" {
		t.Errorf("expected open bead first after sort, got %s", cols.Backlog[0].ID)
	}
	if cols.Backlog[1].ID != "spi-d1" {
		t.Errorf("expected deferred bead second after sort, got %s", cols.Backlog[1].ID)
	}
}

func TestCategorizeWithPhases_InProgressRoutedByPhase(t *testing.T) {
	open := []BoardBead{
		{ID: "spi-ip1", Title: "Implementing", Status: "in_progress", Type: "task", Priority: 1},
		{ID: "spi-ip2", Title: "Designing", Status: "in_progress", Type: "task", Priority: 1},
	}
	phases := map[string]string{
		"spi-ip1": "implement",
		"spi-ip2": "design",
	}
	cols := CategorizeWithPhases(open, nil, nil, phases, "me")
	if len(cols.Implement) != 1 || cols.Implement[0].ID != "spi-ip1" {
		t.Errorf("expected spi-ip1 in Implement, got %v", cols.Implement)
	}
	if len(cols.Design) != 1 || cols.Design[0].ID != "spi-ip2" {
		t.Errorf("expected spi-ip2 in Design, got %v", cols.Design)
	}
	if len(cols.Backlog) != 0 {
		t.Errorf("expected no beads in Backlog, got %v", cols.Backlog)
	}
	if len(cols.Ready) != 0 {
		t.Errorf("expected no beads in Ready, got %v", cols.Ready)
	}
}

func TestAllColumns_BacklogBeforeReady(t *testing.T) {
	cols := Columns{}
	all := AllColumns(cols)
	var names []string
	for _, c := range all {
		names = append(names, c.Name)
	}
	backlogIdx := -1
	readyIdx := -1
	for i, name := range names {
		if name == "BACKLOG" {
			backlogIdx = i
		}
		if name == "READY" {
			readyIdx = i
		}
	}
	if backlogIdx == -1 {
		t.Fatal("BACKLOG column not found in AllColumns")
	}
	if readyIdx == -1 {
		t.Fatal("READY column not found in AllColumns")
	}
	if backlogIdx >= readyIdx {
		t.Errorf("BACKLOG (idx %d) should appear before READY (idx %d)", backlogIdx, readyIdx)
	}
}

func TestCategorizeWithPhases_MixedStatuses(t *testing.T) {
	open := []BoardBead{
		{ID: "spi-a", Title: "Open", Status: "open", Type: "task", Priority: 1},
		{ID: "spi-b", Title: "Ready", Status: "ready", Type: "task", Priority: 1},
		{ID: "spi-c", Title: "Deferred", Status: "deferred", Type: "task", Priority: 1},
		{ID: "spi-d", Title: "In progress", Status: "in_progress", Type: "task", Priority: 1},
	}
	closed := []BoardBead{
		{ID: "spi-e", Title: "Done", Status: "closed", Type: "task", Priority: 1},
	}
	phases := map[string]string{
		"spi-d": "implement",
	}
	cols := CategorizeWithPhases(open, closed, nil, phases, "me")

	if len(cols.Backlog) != 2 {
		t.Errorf("expected 2 beads in Backlog (open + deferred), got %d: %v", len(cols.Backlog), cols.Backlog)
	}
	if len(cols.Ready) != 1 || cols.Ready[0].ID != "spi-b" {
		t.Errorf("expected spi-b in Ready, got %v", cols.Ready)
	}
	if len(cols.Implement) != 1 || cols.Implement[0].ID != "spi-d" {
		t.Errorf("expected spi-d in Implement, got %v", cols.Implement)
	}
	if len(cols.Done) != 1 || cols.Done[0].ID != "spi-e" {
		t.Errorf("expected spi-e in Done, got %v", cols.Done)
	}
}
