package integration

import "testing"

func TestMapBeadStatusToLinearStateType(t *testing.T) {
	cases := []struct {
		status string
		want   string
	}{
		// Pre-Landing-3 statuses keep their existing mappings.
		{"open", "unstarted"},
		{"ready", "unstarted"},
		{"in_progress", "started"},
		{"dispatched", "started"},
		{"deferred", "backlog"},
		{"blocked", "backlog"},
		{"closed", "completed"},

		// Landing 3 (spi-sqqero / spi-a76fxv) statuses — the four added
		// by spi-lkeuqy. Each must round-trip into a "started" Linear
		// state so the issue stays visibly active while review/merge
		// work is in flight.
		{"awaiting_review", "started"},
		{"needs_changes", "started"},
		{"awaiting_human", "started"},
		{"merge_pending", "started"},

		// Default branch covers the soon-to-be-deleted `hooked` status
		// (handled by spi-x7c67k) plus any genuinely unknown value —
		// both should fall back to "started" rather than an explicit
		// case so the mapping doesn't carry a load-bearing reference
		// to `hooked` past Task 8.
		{"hooked", "started"},
		{"never-defined-status", "started"},
		{"", "started"},
	}

	for _, c := range cases {
		t.Run(c.status, func(t *testing.T) {
			got := MapBeadStatusToLinearStateType(c.status)
			if got != c.want {
				t.Errorf("MapBeadStatusToLinearStateType(%q) = %q, want %q", c.status, got, c.want)
			}
		})
	}
}
