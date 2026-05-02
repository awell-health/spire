package lifecycle

import (
	"testing"

	"github.com/awell-health/spire/pkg/store"
)

// legalStatuses enumerates every status string that may appear on a
// bead today. Includes the new statuses spi-sqqero Landing 3 will add
// (awaiting_review) so the predicates remain well-defined when those
// land — none of them flip the predicates' answers under legacy
// semantics.
var legalStatuses = []string{
	"open",
	"ready",
	"dispatched",
	"in_progress",
	"hooked",
	"deferred",
	"awaiting_review",
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
		"in_progress":    true,
		"hooked":          true,
		"deferred":        true,
		"awaiting_review": true,
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
