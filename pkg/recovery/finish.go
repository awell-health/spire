package recovery

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/awell-health/spire/pkg/store"
)

// KeyRecoveryOutcome is the metadata key under which the structured
// RecoveryOutcome JSON blob is persisted on the recovery bead. WriteOutcome
// is the sole writer of this key; ReadOutcome is the sole reader.
const KeyRecoveryOutcome = "recovery_outcome"

// FinishRecovery documents the learning (if not already written) then closes
// the recovery bead with a durable close comment.
func FinishRecovery(deps RecoveryDeps, beadID string, learning RecoveryLearning) error {
	if learning.ResolvedAt.IsZero() {
		learning.ResolvedAt = time.Now().UTC()
	}
	if err := DocumentLearning(deps, beadID, learning); err != nil {
		return err
	}
	closeComment := fmt.Sprintf("Resolved: %s — %s", learning.ResolutionKind, learning.Narrative)
	if err := deps.AddComment(beadID, closeComment); err != nil {
		return fmt.Errorf("finish recovery close comment: %w", err)
	}
	return deps.CloseBead(beadID)
}

// WriteOutcome persists a RecoveryOutcome to bead metadata (as a JSON blob
// under KeyRecoveryOutcome) and to the recovery_learnings SQL table. This is
// the single authoritative writer of the RecoveryOutcome shape — no other
// code path should emit this record.
//
// For decide-time query compatibility, a small set of fields is also mirrored
// to individual metadata keys (failure_class, source_bead, resolution_kind,
// resolved_at, verification_status, reusable). The retired learning_outcome
// scalar is intentionally not written.
func WriteOutcome(ctx context.Context, bead *store.Bead, out RecoveryOutcome) error {
	if bead == nil {
		return fmt.Errorf("write outcome: bead is nil")
	}
	blob, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("write outcome: marshal: %w", err)
	}

	resolvedAt := time.Now().UTC()
	verifyOutcome := "dirty"
	if out.VerifyVerdict == VerifyVerdictPass {
		verifyOutcome = "clean"
	}
	reusable := out.Decision == DecisionResume

	meta := map[string]string{
		KeyRecoveryOutcome:    string(blob),
		KeyResolvedAt:         resolvedAt.Format(time.RFC3339),
		KeyVerificationStatus: verifyOutcome,
	}
	if out.FailureClass != "" {
		meta[KeyFailureClass] = string(out.FailureClass)
	}
	if out.SourceBeadID != "" {
		meta[KeySourceBead] = out.SourceBeadID
	}
	if out.RepairAction != "" {
		meta[KeyResolutionKind] = out.RepairAction
	}
	if reusable {
		meta[KeyReusable] = "true"
	}
	if err := store.SetBeadMetadataMap(bead.ID, meta); err != nil {
		return fmt.Errorf("write outcome: set metadata on %s: %w", bead.ID, err)
	}

	row := store.RecoveryLearningRow{
		ID:             generateOutcomeID(),
		RecoveryBead:   bead.ID,
		SourceBead:     out.SourceBeadID,
		FailureClass:   string(out.FailureClass),
		FailureSig:     bead.Meta(KeyFailureSignature),
		ResolutionKind: out.RepairAction,
		Outcome:        verifyOutcome,
		Reusable:       reusable,
		ResolvedAt:     resolvedAt,
	}
	if err := store.WriteRecoveryLearningAuto(row); err != nil {
		return fmt.Errorf("write outcome: recovery_learnings sql: %w", err)
	}
	return nil
}

// ReadOutcome parses the persisted RecoveryOutcome from bead metadata. Returns
// (zero, false) when no outcome has been written — e.g. older beads or beads
// without a recovery attempt.
func ReadOutcome(bead store.Bead) (RecoveryOutcome, bool) {
	raw := bead.Meta(KeyRecoveryOutcome)
	if raw == "" {
		return RecoveryOutcome{}, false
	}
	var out RecoveryOutcome
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return RecoveryOutcome{}, false
	}
	return out, true
}

func generateOutcomeID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("ro-%d", time.Now().UnixNano())
	}
	return "ro-" + hex.EncodeToString(b)
}
