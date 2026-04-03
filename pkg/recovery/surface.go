package recovery

import (
	"fmt"
	"strings"

	"github.com/awell-health/spire/pkg/store"
)

// FormatRecoveryContext formats a slice of recovery learnings into a
// deterministic, human-readable context block. Returns "" if empty.
func FormatRecoveryContext(learnings []store.RecoveryLearning) string {
	if len(learnings) == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "--- Recovery history (%d incidents) ---\n", len(learnings))
	for _, rl := range learnings {
		b.WriteString(rl.BeadID)
		if rl.FailureClass != "" {
			fmt.Fprintf(&b, " [%s]", rl.FailureClass)
		}
		if rl.ResolvedAt != "" {
			fmt.Fprintf(&b, " resolved %s", rl.ResolvedAt)
		}
		if rl.LearningSummary != "" {
			fmt.Fprintf(&b, ": %s", rl.LearningSummary)
		}
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return b.String()
}

// SurfaceRecoveryContext assembles prior closed recovery context for a bead.
// If filter.SourceBead is empty, it defaults to beadID so prior recoveries
// for this bead are surfaced. Delegates to deps.ListRecoveryLearnings then
// formats the result.
func SurfaceRecoveryContext(beadID string, deps *Deps, filter store.RecoveryLookupFilter) (string, error) {
	if filter.SourceBead == "" {
		filter.SourceBead = beadID
	}
	if deps.ListRecoveryLearnings == nil {
		return "", nil
	}
	learnings, err := deps.ListRecoveryLearnings(filter)
	if err != nil {
		return "", err
	}
	return FormatRecoveryContext(learnings), nil
}
