package lifecycle

import (
	"testing"

	"github.com/awell-health/spire/pkg/store"
)

// legalStatuses enumerates every status string that may appear on a
// bead today. Includes the four statuses spi-sqqero Landing 3
// (spi-lkeuqy) introduces — awaiting_review, needs_changes,
// awaiting_human, merge_pending — so the predicates remain well-defined
// across the full taxonomy. None of them flip the predicates' answers
// under legacy semantics; later landings (Task 7+) re-tune the
// predicates against the new statuses.
var legalStatuses = []string{
	"open",
	"ready",
	"dispatched",
	"in_progress",
	"hooked",
	"deferred",
	"awaiting_review",
	"needs_changes",
	"awaiting_human",
	"merge_pending",
	"closed",
}

func TestIsActive(t *testing.T) {
	want := map[string]bool{
		"open":            true,
		"ready":           false,
		"dispatched":      false,
		"in_progress":     true,
		"hooked":          false,
		"deferred":        false,
		"awaiting_review": false,
		"needs_changes":   false,
		"awaiting_human":  false,
		"merge_pending":   false,
		"closed":          false,
	}
	for _, status := range legalStatuses {
		t.Run(status, func(t *testing.T) {
			b := &store.Bead{Status: status}
			got := IsActive(b)
			// Legacy expression — keep this in sync with the body
			// of IsActive in predicates.go.
			expected := b.Status == "in_progress" || b.Status == "open"
			if got != expected {
				t.Errorf("IsActive(%q) = %v, legacy expression = %v", status, got, expected)
			}
			if got != want[status] {
				t.Errorf("IsActive(%q) = %v, want %v", status, got, want[status])
			}
		})
	}
}

func TestIsMutable(t *testing.T) {
	want := map[string]bool{
		"open":            true,
		"ready":           true,
		"dispatched":      true,
		"in_progress":     true,
		"hooked":          true,
		"deferred":        true,
		"awaiting_review": true,
		"needs_changes":   true,
		"awaiting_human":  true,
		"merge_pending":   true,
		"closed":          false,
	}
	for _, status := range legalStatuses {
		t.Run(status, func(t *testing.T) {
			b := &store.Bead{Status: status}
			got := IsMutable(b)
			expected := b.Status != "closed"
			if got != expected {
				t.Errorf("IsMutable(%q) = %v, legacy expression = %v", status, got, expected)
			}
			if got != want[status] {
				t.Errorf("IsMutable(%q) = %v, want %v", status, got, want[status])
			}
		})
	}
}

func TestIsDispatchable(t *testing.T) {
	want := map[string]bool{
		"open":            true,
		"ready":           true,
		"dispatched":      false,
		"in_progress":     false,
		"hooked":          true,
		"deferred":        false,
		"awaiting_review": false,
		"needs_changes":   false,
		"awaiting_human":  false,
		"merge_pending":   false,
		"closed":          false,
	}
	for _, status := range legalStatuses {
		t.Run(status, func(t *testing.T) {
			b := &store.Bead{Status: status}
			got := IsDispatchable(b)
			expected := b.Status == "ready" || b.Status == "open" || b.Status == "hooked"
			if got != expected {
				t.Errorf("IsDispatchable(%q) = %v, legacy expression = %v", status, got, expected)
			}
			if got != want[status] {
				t.Errorf("IsDispatchable(%q) = %v, want %v", status, got, want[status])
			}
		})
	}
}

func TestPredicates_NilBead(t *testing.T) {
	if IsActive(nil) {
		t.Error("IsActive(nil) = true, want false")
	}
	if IsMutable(nil) {
		t.Error("IsMutable(nil) = true, want false")
	}
	if IsDispatchable(nil) {
		t.Error("IsDispatchable(nil) = true, want false")
	}
}
