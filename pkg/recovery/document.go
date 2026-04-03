package recovery

import (
	"fmt"
	"strconv"
	"time"
)

// DocumentLearning writes structured recovery metadata and a human narrative
// comment to the recovery bead. Idempotent: safe to call multiple times.
func DocumentLearning(deps RecoveryDeps, beadID string, learning RecoveryLearning) error {
	meta := map[string]interface{}{
		KeyResolutionKind:     string(learning.ResolutionKind),
		KeyVerificationStatus: string(learning.VerificationStatus),
		KeyLearningKey:        learning.LearningKey,
		KeyReusable:           strconv.FormatBool(learning.Reusable),
		KeyResolvedAt:         learning.ResolvedAt.UTC().Format(time.RFC3339),
	}
	if err := deps.UpdateBead(beadID, meta); err != nil {
		return fmt.Errorf("document learning metadata: %w", err)
	}
	comment := formatLearningNarrative(learning)
	return deps.AddComment(beadID, comment)
}

// formatLearningNarrative builds the human-readable comment block.
//
// Format:
//
//	Recovery documented [<resolved_at>]
//	Resolution: <resolution_kind>
//	Verification: <verification_status>
//	Reusable: yes/no  |  Key: <learning_key>
//	---
//	<narrative>
func formatLearningNarrative(l RecoveryLearning) string {
	reusable := "no"
	if l.Reusable {
		reusable = "yes"
	}
	return fmt.Sprintf(
		"Recovery documented [%s]\nResolution: %s\nVerification: %s\nReusable: %s  |  Key: %s\n---\n%s",
		l.ResolvedAt.UTC().Format(time.RFC3339),
		l.ResolutionKind,
		l.VerificationStatus,
		reusable,
		l.LearningKey,
		l.Narrative,
	)
}
