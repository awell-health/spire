package workshop

// DefaultPhasesForType returns the default v2 phase pipeline for a bead type.
// Returns nil for types that are v3-only (no v2 phase pipeline).
func DefaultPhasesForType(beadType string) []string {
	switch beadType {
	case "task", "feature", "chore":
		return []string{"plan", "implement", "review", "merge"}
	case "bug":
		return []string{"plan", "implement", "review", "merge"}
	case "epic":
		return []string{"design", "plan", "implement", "review", "merge"}
	case "recovery":
		return nil // recovery is v3-only; no v2 phase pipeline
	default:
		return []string{"plan", "implement", "review", "merge"}
	}
}

// RecoveryStepDefault describes a canonical recovery step for compose scaffolding.
type RecoveryStepDefault struct {
	Name  string
	Flow  string
	Title string
}

// RecoveryStepDefaults returns the canonical step sequence for recovery formulas.
// Used by ComposeInteractive to pre-populate the graph builder when composing
// a recovery formula, and by callers needing a canonical ordered reference.
func RecoveryStepDefaults() []RecoveryStepDefault {
	return []RecoveryStepDefault{
		{"collect_context", "recovery-collect", "Collect failure context and prior recovery learnings"},
		{"design", "recovery-design", "Frame the failure and design remediation approach"},
		{"plan", "recovery-plan", "Plan bounded remediation actions"},
		{"remediate", "recovery-remediate", "Execute remediation"},
		{"verify", "recovery-verify", "Verify recovery"},
		{"document", "recovery-document", "Write durable learnings back onto the recovery bead"},
		{"finish", "bead.finish", "Close resolved recovery bead"},
	}
}
