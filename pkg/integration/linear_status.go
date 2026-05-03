package integration

// MapBeadStatusToLinearStateType returns the Linear workflow-state type
// (one of "backlog", "unstarted", "started", "completed", "canceled") that
// best represents a Spire bead's current status. Linear groups workflow
// states by these five canonical types; mapping to the type rather than a
// hand-picked state name keeps the integration portable across teams that
// rename their states ("Done" vs "Shipped" vs "Closed" all share type
// "completed").
//
// Landing 3 of spi-sqqero (parent epic spi-a76fxv) introduces four new
// statuses that callers must be able to translate when projecting bead
// state into Linear:
//
//	awaiting_review  → "started" (apprentice handoff is in flight)
//	needs_changes    → "started" (sage requested changes; still active work)
//	awaiting_human   → "started" (cleric paused for archmage input)
//	merge_pending    → "started" (approved; waiting on merge gate)
//
// Per spi-cooki3 the legacy `hooked` status is intentionally NOT enumerated
// here — Task 8 (spi-x7c67k) removes it from production code paths
// atomically. Anything not explicitly enumerated falls through to the
// default branch ("started"), which is also where stale `hooked` rows on
// disk would land if they survive long enough to be projected through
// this function.
func MapBeadStatusToLinearStateType(status string) string {
	switch status {
	case "open", "ready":
		return "unstarted"
	case "deferred", "blocked":
		return "backlog"
	case "in_progress",
		"dispatched",
		"awaiting_review",
		"needs_changes",
		"awaiting_human",
		"merge_pending":
		return "started"
	case "closed":
		return "completed"
	default:
		return "started"
	}
}
