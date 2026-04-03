package recovery

import (
	"fmt"
	"time"
)

// FinishRecovery documents the learning (if not already written) then closes
// the recovery bead with a durable close comment.
func FinishRecovery(deps RecoveryDeps, beadID string, learning RecoveryLearning) error {
	if learning.ResolvedAt.IsZero() {
		learning.ResolvedAt = time.Now().UTC()
	}
	if err := DocumentLearning(deps, beadID, learning); err != nil {
		return err
	}
	closeComment := fmt.Sprintf("Resolved: %s \u2014 %s", learning.ResolutionKind, learning.Narrative)
	if err := deps.AddComment(beadID, closeComment); err != nil {
		return fmt.Errorf("finish recovery close comment: %w", err)
	}
	return deps.CloseBead(beadID)
}
