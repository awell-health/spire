package store

import (
	"testing"

	"github.com/steveyegge/beads"
)

// TestParseStatus_AwaitingReview pins the new awaiting_review status
// recognized by ParseStatus. Cleric foundation (spi-h2d7yn).
func TestParseStatus_AwaitingReview(t *testing.T) {
	got := ParseStatus("awaiting_review")
	if got != StatusAwaitingReview {
		t.Fatalf("ParseStatus(awaiting_review) = %q, want %q", got, StatusAwaitingReview)
	}
}

// TestValidStatusTransition_ClericFoundation pins the cleric epic's
// allowed-transition graph: in_progress → awaiting_review → {closed,
// in_progress}. Other transitions stay permissive.
func TestValidStatusTransition_ClericFoundation(t *testing.T) {
	cases := []struct {
		from beads.Status
		to   beads.Status
		want bool
	}{
		// Pinned cleric transitions out of awaiting_review.
		{StatusAwaitingReview, beads.StatusClosed, true},
		{StatusAwaitingReview, beads.StatusInProgress, true},
		// Disallowed: from awaiting_review to anything not in the allow-set.
		{StatusAwaitingReview, beads.StatusOpen, false},
		{StatusAwaitingReview, StatusHooked, false},
		{StatusAwaitingReview, beads.StatusBlocked, false},
		// Same-status writes are always allowed.
		{StatusAwaitingReview, StatusAwaitingReview, true},
		{beads.StatusInProgress, beads.StatusInProgress, true},
		// Permissive sources: only awaiting_review is constrained today.
		// Every other source status is unconstrained — including
		// in_progress → awaiting_review, which is the entry edge.
		{beads.StatusInProgress, StatusAwaitingReview, true},
		{beads.StatusInProgress, beads.StatusClosed, true},
		{beads.StatusInProgress, StatusHooked, true},
		{beads.StatusOpen, beads.StatusInProgress, true},
		{beads.StatusOpen, beads.StatusClosed, true},
		{StatusHooked, beads.StatusInProgress, true},
	}
	for _, c := range cases {
		got := ValidStatusTransition(c.from, c.to)
		if got != c.want {
			t.Errorf("ValidStatusTransition(%q→%q) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}
