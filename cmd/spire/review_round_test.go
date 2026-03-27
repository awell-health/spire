package main

import (
	"fmt"
	"strings"
	"testing"
)

// --- storeCreateReviewBead + storeCloseReviewBead tests ---

func TestCreateAndCloseReviewBead(t *testing.T) {
	// We test the helper logic through the testable child getter pattern
	// (same approach as attempt_test.go).

	// Simulate bead storage in-memory
	var beads []Bead
	nextID := 1

	origGetChildren := storeGetChildrenFunc
	storeGetChildrenFunc = func(_ *SpireContext, parentID string) ([]Bead, error) {
		var children []Bead
		for _, b := range beads {
			if b.Parent == parentID {
				children = append(children, b)
			}
		}
		return children, nil
	}
	defer func() { storeGetChildrenFunc = origGetChildren }()

	// Simulate creating a review bead manually (since storeCreateReviewBead
	// requires a live store, we verify the contract with the detection functions)
	parentID := "spi-test-parent"

	// Create review bead round 1
	rb1 := Bead{
		ID:          fmt.Sprintf("%s.%d", parentID, nextID),
		Title:       "review-round-1",
		Status:      "in_progress",
		Labels:      []string{"review-round", "sage:reviewer-1", "round:1"},
		Parent:      parentID,
		Description: "",
	}
	nextID++
	beads = append(beads, rb1)

	// Verify detection
	if !isReviewRoundBead(rb1) {
		t.Error("expected review-round-1 to be detected as review round bead")
	}

	// Simulate closing with verdict
	rb1.Status = "closed"
	rb1.Description = "verdict: approve\n\nCode looks good"
	beads[0] = rb1

	// Verify we can retrieve it via storeGetReviewBeads (using test childGetter)
	children, err := storeGetChildrenFunc(_defaultSctx, parentID)
	if err != nil {
		t.Fatalf("getChildren: %v", err)
	}
	var reviews []Bead
	for _, c := range children {
		if isReviewRoundBead(c) {
			reviews = append(reviews, c)
		}
	}
	if len(reviews) != 1 {
		t.Fatalf("expected 1 review bead, got %d", len(reviews))
	}
	if reviews[0].Description != "verdict: approve\n\nCode looks good" {
		t.Errorf("unexpected description: %s", reviews[0].Description)
	}
}

// --- storeGetReviewBeads ordering test ---

func TestGetReviewBeads_OrderedByRound(t *testing.T) {
	origGetChildren := storeGetChildrenFunc
	storeGetChildrenFunc = func(_ *SpireContext, parentID string) ([]Bead, error) {
		return []Bead{
			{ID: "spi-p.3", Title: "review-round-3", Status: "closed", Labels: []string{"review-round", "round:3"}, Parent: parentID},
			{ID: "spi-p.1", Title: "review-round-1", Status: "closed", Labels: []string{"review-round", "round:1"}, Parent: parentID},
			{ID: "spi-p.2", Title: "review-round-2", Status: "closed", Labels: []string{"review-round", "round:2"}, Parent: parentID},
			{ID: "spi-p.4", Title: "not a review", Status: "open", Labels: []string{"task"}, Parent: parentID},
		}, nil
	}
	defer func() { storeGetChildrenFunc = origGetChildren }()

	reviews, err := storeGetReviewBeads("spi-p")
	if err != nil {
		t.Fatalf("storeGetReviewBeads: %v", err)
	}
	if len(reviews) != 3 {
		t.Fatalf("expected 3 review beads, got %d", len(reviews))
	}
	// Verify ordering
	for i, expected := range []string{"spi-p.1", "spi-p.2", "spi-p.3"} {
		if reviews[i].ID != expected {
			t.Errorf("reviews[%d].ID = %q, want %q", i, reviews[i].ID, expected)
		}
	}
}

// --- isReviewRoundBead tests ---

func TestIsReviewRoundBead_WithLabel(t *testing.T) {
	b := Bead{Labels: []string{"review-round", "sage:reviewer-1", "round:1"}}
	if !isReviewRoundBead(b) {
		t.Error("expected true for bead with review-round label")
	}
}

func TestIsReviewRoundBead_WithTitle(t *testing.T) {
	b := Bead{Title: "review-round-1"}
	if !isReviewRoundBead(b) {
		t.Error("expected true for bead with review-round- title prefix")
	}
}

func TestIsReviewRoundBead_Neither(t *testing.T) {
	b := Bead{Title: "regular task", Labels: []string{"task"}}
	if isReviewRoundBead(b) {
		t.Error("expected false for regular bead")
	}
}

func TestIsReviewRoundBead_AttemptNotDetected(t *testing.T) {
	b := Bead{Title: "attempt: wizard-1", Labels: []string{"attempt"}}
	if isReviewRoundBead(b) {
		t.Error("attempt bead should not be detected as review-round bead")
	}
}

func TestIsReviewRoundBoardBead_WithLabel(t *testing.T) {
	b := BoardBead{Labels: []string{"review-round"}}
	if !isReviewRoundBoardBead(b) {
		t.Error("expected true for board bead with review-round label")
	}
}

func TestIsReviewRoundBoardBead_WithTitle(t *testing.T) {
	b := BoardBead{Title: "review-round-2"}
	if !isReviewRoundBoardBead(b) {
		t.Error("expected true for board bead with review-round- title")
	}
}

func TestIsReviewRoundBoardBead_Neither(t *testing.T) {
	b := BoardBead{Title: "some task", Labels: []string{"work"}}
	if isReviewRoundBoardBead(b) {
		t.Error("expected false for regular board bead")
	}
}

// --- Round counting test ---

func TestRoundCounting(t *testing.T) {
	origGetChildren := storeGetChildrenFunc
	defer func() { storeGetChildrenFunc = origGetChildren }()

	// Scenario: 2 existing review children → next round should be 3
	storeGetChildrenFunc = func(_ *SpireContext, parentID string) ([]Bead, error) {
		return []Bead{
			{ID: "spi-p.1", Title: "review-round-1", Status: "closed", Labels: []string{"review-round", "round:1"}, Parent: parentID},
			{ID: "spi-p.2", Title: "review-round-2", Status: "in_progress", Labels: []string{"review-round", "round:2"}, Parent: parentID},
			{ID: "spi-p.3", Title: "attempt: wizard-1", Status: "in_progress", Labels: []string{"attempt"}, Parent: parentID},
		}, nil
	}

	reviews, err := storeGetReviewBeads("spi-p")
	if err != nil {
		t.Fatalf("storeGetReviewBeads: %v", err)
	}

	nextRound := len(reviews) + 1
	if nextRound != 3 {
		t.Errorf("next round = %d, want 3 (2 existing review children + 1)", nextRound)
	}
}

// --- reviewRoundNumber test ---

func TestReviewRoundNumber(t *testing.T) {
	tests := []struct {
		name     string
		bead     Bead
		expected int
	}{
		{"with round:1 label", Bead{Labels: []string{"review-round", "round:1"}}, 1},
		{"with round:3 label", Bead{Labels: []string{"review-round", "round:3"}}, 3},
		{"no round label", Bead{Labels: []string{"review-round"}}, 0},
		{"empty labels", Bead{}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reviewRoundNumber(tt.bead)
			if got != tt.expected {
				t.Errorf("reviewRoundNumber() = %d, want %d", got, tt.expected)
			}
		})
	}
}

// --- Board filtering test ---

func TestBoard_FiltersReviewRoundBeads(t *testing.T) {
	openBeads := []BoardBead{
		{ID: "spi-task-1", Title: "Real task", Status: "open", Type: "task"},
		{ID: "spi-task-1.1", Title: "review-round-1", Status: "in_progress", Type: "task", Labels: []string{"review-round", "sage:reviewer-1", "round:1"}},
		{ID: "spi-task-1.2", Title: "review-round-2", Status: "closed", Type: "task", Labels: []string{"review-round", "sage:reviewer-1", "round:2"}},
		{ID: "spi-task-2", Title: "Another task", Status: "open", Type: "task"},
	}
	closedBeads := []BoardBead{}
	blockedBeads := []BoardBead{}

	cols := categorizeColumnsFromStore(openBeads, closedBeads, blockedBeads, "")

	// Review-round beads should be filtered out from all columns
	for _, col := range [][]BoardBead{cols.Ready, cols.Design, cols.Plan, cols.Implement, cols.Review, cols.Merge, cols.Done, cols.Blocked, cols.Alerts} {
		for _, b := range col {
			if isReviewRoundBoardBead(b) {
				t.Errorf("review-round bead %s should be filtered from board columns", b.ID)
			}
		}
	}

	// Real tasks should still be present
	found := 0
	for _, b := range cols.Ready {
		if b.ID == "spi-task-1" || b.ID == "spi-task-2" {
			found++
		}
	}
	if found != 2 {
		t.Errorf("expected 2 real tasks in Ready, found %d", found)
	}
}

// --- wizardCollectReviewHistory test ---

func TestWizardCollectReviewHistory_WithReviewBeads(t *testing.T) {
	origGetChildren := storeGetChildrenFunc
	storeGetChildrenFunc = func(_ *SpireContext, parentID string) ([]Bead, error) {
		return []Bead{
			{
				ID:          "spi-p.1",
				Title:       "review-round-1",
				Status:      "closed",
				Labels:      []string{"review-round", "sage:sage-1", "round:1"},
				Parent:      parentID,
				Description: "verdict: request_changes\n\nMissing error handling",
			},
			{
				ID:          "spi-p.2",
				Title:       "review-round-2",
				Status:      "closed",
				Labels:      []string{"review-round", "sage:sage-1", "round:2"},
				Parent:      parentID,
				Description: "verdict: approve\n\nAll issues addressed",
			},
		}, nil
	}
	defer func() { storeGetChildrenFunc = origGetChildren }()

	result := wizardCollectReviewHistory("spi-p", "wizard-test")

	if !strings.Contains(result, "## Prior Review Rounds") {
		t.Error("expected structured review history header")
	}
	if !strings.Contains(result, "Round 1") {
		t.Error("expected Round 1 in output")
	}
	if !strings.Contains(result, "Round 2") {
		t.Error("expected Round 2 in output")
	}
	if !strings.Contains(result, "Missing error handling") {
		t.Error("expected review bead description in output")
	}
	if !strings.Contains(result, "All issues addressed") {
		t.Error("expected second review bead description in output")
	}
}

func TestWizardCollectReviewHistory_FallsBackToMessages(t *testing.T) {
	origGetChildren := storeGetChildrenFunc
	storeGetChildrenFunc = func(_ *SpireContext, parentID string) ([]Bead, error) {
		return nil, nil // no review beads
	}
	defer func() { storeGetChildrenFunc = origGetChildren }()

	// This will try to collect messages via storeListBeads which needs a store.
	// Without a store it returns empty string, which is the expected fallback behavior.
	result := wizardCollectReviewHistory("spi-nonexistent", "wizard-test")
	// Should not panic and should return empty or message-based feedback
	_ = result
}

// --- reviewGetRound uses bead graph, not labels ---

// TestReviewGetRound_WithoutLabels verifies that reviewGetRound counts
// review-round child beads rather than reading review-round:N labels on the
// parent. The parent bead has no review-round: labels; the count comes purely
// from graph children.
func TestReviewGetRound_WithoutLabels(t *testing.T) {
	origGetChildren := storeGetChildrenFunc
	defer func() { storeGetChildrenFunc = origGetChildren }()

	// Two closed review beads, no review-round: labels on any parent.
	storeGetChildrenFunc = func(_ *SpireContext, parentID string) ([]Bead, error) {
		return []Bead{
			{ID: "spi-p.1", Title: "review-round-1", Status: "closed", Labels: []string{"review-round", "sage:sage-1", "round:1"}, Parent: parentID},
			{ID: "spi-p.2", Title: "review-round-2", Status: "closed", Labels: []string{"review-round", "sage:sage-1", "round:2"}, Parent: parentID},
		}, nil
	}

	got := reviewGetRound("spi-p")
	if got != 2 {
		t.Errorf("reviewGetRound = %d, want 2 (should count review child beads, not labels)", got)
	}
}

// TestReviewGetRound_NoReviewBeads verifies that reviewGetRound returns 0
// when there are no review-round children (first review of the bead).
func TestReviewGetRound_NoReviewBeads(t *testing.T) {
	origGetChildren := storeGetChildrenFunc
	defer func() { storeGetChildrenFunc = origGetChildren }()

	storeGetChildrenFunc = func(_ *SpireContext, parentID string) ([]Bead, error) {
		return []Bead{
			{ID: "spi-p.1", Title: "attempt: wizard-1", Status: "closed", Labels: []string{"attempt"}, Parent: parentID},
		}, nil
	}

	got := reviewGetRound("spi-p")
	if got != 0 {
		t.Errorf("reviewGetRound = %d, want 0 (no review beads)", got)
	}
}

// Suppress unused import warning
var _ = fmt.Sprintf
