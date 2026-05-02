package lifecycle

import "github.com/awell-health/spire/pkg/store"

// IsActive reports whether the bead is currently being worked on or
// queued in the open backlog. The body mirrors the legacy
// `Status == "in_progress" || Status == "open"` pattern that appears
// across pkg/board, pkg/executor, pkg/recovery, and pkg/observability.
// Landing 2 reimplements this against the live status set; Landing 1
// preserves the existing semantics so introducing the predicate is a
// zero-behavior-change refactor seam.
func IsActive(b *store.Bead) bool {
	if b == nil {
		return false
	}
	return b.Status == "in_progress" || b.Status == "open"
}

// IsMutable reports whether the bead can still receive status writes.
// Closed beads are terminal in the legacy semantics, so the predicate
// is the inverse of "closed".
func IsMutable(b *store.Bead) bool {
	if b == nil {
		return false
	}
	return b.Status != "closed"
}

// IsDispatchable reports whether the bead is in a status the steward
// will consider for dispatch. Legacy semantics include "ready" (the
// primary dispatch state), "open" (pre-ready candidates), and "hooked"
// (parked beads that can be unhooked and dispatched).
func IsDispatchable(b *store.Bead) bool {
	if b == nil {
		return false
	}
	return b.Status == "ready" || b.Status == "open" || b.Status == "hooked"
}
