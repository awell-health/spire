package store

import (
	"testing"

	"github.com/steveyegge/beads"
)

// TestParseStatus_Landing3 pins the four statuses that spi-sqqero
// Landing 3 (spi-lkeuqy) introduces — awaiting_review (also pinned by
// the cleric foundation test), needs_changes, awaiting_human, and
// merge_pending. ParseStatus must round-trip each literal to its typed
// constant rather than silently downgrading to StatusOpen.
func TestParseStatus_Landing3(t *testing.T) {
	cases := []struct {
		in   string
		want beads.Status
	}{
		{"awaiting_review", StatusAwaitingReview},
		{"needs_changes", StatusNeedsChanges},
		{"awaiting_human", StatusAwaitingHuman},
		{"merge_pending", StatusMergePending},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := ParseStatus(c.in)
			if got != c.want {
				t.Errorf("ParseStatus(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestLanding3StatusConstantValues pins the literal string values of
// the four new typed constants. The string is the on-wire identity that
// formula TOMLs and external consumers (Linear sync, board, gateway)
// will encode against; if the literal drifts the cross-package
// taxonomy break is silent. Wiring the literal as a regression assertion
// keeps the contract explicit.
func TestLanding3StatusConstantValues(t *testing.T) {
	cases := []struct {
		got  beads.Status
		want string
	}{
		{StatusAwaitingReview, "awaiting_review"},
		{StatusNeedsChanges, "needs_changes"},
		{StatusAwaitingHuman, "awaiting_human"},
		{StatusMergePending, "merge_pending"},
	}
	for _, c := range cases {
		if string(c.got) != c.want {
			t.Errorf("constant string = %q, want %q", string(c.got), c.want)
		}
	}
}
