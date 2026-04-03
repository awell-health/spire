package recovery

import (
	"testing"

	"github.com/awell-health/spire/pkg/store"
)

func TestRecoveryMetadata_ToMap_RoundTrip(t *testing.T) {
	original := RecoveryMetadata{
		FailureClass:       "merge-failure",
		FailureSignature:   "merge-failure:implement",
		SourceBead:         "spi-abc",
		SourceFormula:      "spire-agent-work-v3",
		SourceStep:         "implement",
		ResolutionKind:     "resummon",
		VerificationStatus: "healthy",
		LearningKey:        "implement-merge-conflict",
		Reusable:           true,
		ResolvedAt:         "2026-04-03T20:00:00Z",
		LearningSummary:    "Merge conflict resolved by rebasing onto updated main",
	}

	m := original.ToMap()

	// Simulate a bead with this metadata.
	bead := store.Bead{
		ID:       "spi-recovery-1",
		Metadata: m,
	}

	got := RecoveryMetadataFromBead(bead)

	if got.FailureClass != original.FailureClass {
		t.Errorf("FailureClass = %q, want %q", got.FailureClass, original.FailureClass)
	}
	if got.FailureSignature != original.FailureSignature {
		t.Errorf("FailureSignature = %q, want %q", got.FailureSignature, original.FailureSignature)
	}
	if got.SourceBead != original.SourceBead {
		t.Errorf("SourceBead = %q, want %q", got.SourceBead, original.SourceBead)
	}
	if got.SourceFormula != original.SourceFormula {
		t.Errorf("SourceFormula = %q, want %q", got.SourceFormula, original.SourceFormula)
	}
	if got.SourceStep != original.SourceStep {
		t.Errorf("SourceStep = %q, want %q", got.SourceStep, original.SourceStep)
	}
	if got.ResolutionKind != original.ResolutionKind {
		t.Errorf("ResolutionKind = %q, want %q", got.ResolutionKind, original.ResolutionKind)
	}
	if got.VerificationStatus != original.VerificationStatus {
		t.Errorf("VerificationStatus = %q, want %q", got.VerificationStatus, original.VerificationStatus)
	}
	if got.LearningKey != original.LearningKey {
		t.Errorf("LearningKey = %q, want %q", got.LearningKey, original.LearningKey)
	}
	if got.Reusable != original.Reusable {
		t.Errorf("Reusable = %v, want %v", got.Reusable, original.Reusable)
	}
	if got.ResolvedAt != original.ResolvedAt {
		t.Errorf("ResolvedAt = %q, want %q", got.ResolvedAt, original.ResolvedAt)
	}
	if got.LearningSummary != original.LearningSummary {
		t.Errorf("LearningSummary = %q, want %q", got.LearningSummary, original.LearningSummary)
	}
}

func TestRecoveryMetadata_ToMap_EmptyFields(t *testing.T) {
	empty := RecoveryMetadata{}
	m := empty.ToMap()
	if len(m) != 0 {
		t.Errorf("ToMap on zero RecoveryMetadata should be empty, got %d entries: %v", len(m), m)
	}
}

func TestRecoveryMetadata_ToMap_PartialFields(t *testing.T) {
	partial := RecoveryMetadata{
		FailureClass: "build-failure",
		SourceBead:   "spi-xyz",
	}
	m := partial.ToMap()
	if len(m) != 2 {
		t.Errorf("expected 2 entries, got %d: %v", len(m), m)
	}
	if m[KeyFailureClass] != "build-failure" {
		t.Errorf("failure_class = %q, want %q", m[KeyFailureClass], "build-failure")
	}
	if m[KeySourceBead] != "spi-xyz" {
		t.Errorf("source_bead = %q, want %q", m[KeySourceBead], "spi-xyz")
	}
}

func TestRecoveryMetadata_Reusable_False_Omitted(t *testing.T) {
	meta := RecoveryMetadata{
		FailureClass: "test",
		Reusable:     false,
	}
	m := meta.ToMap()
	if _, ok := m[KeyReusable]; ok {
		t.Error("Reusable=false should not appear in ToMap")
	}
}

func TestRecoveryMetadataFromBead_NilMetadata(t *testing.T) {
	bead := store.Bead{ID: "spi-nil"}
	got := RecoveryMetadataFromBead(bead)
	if got.FailureClass != "" || got.LearningSummary != "" || got.Reusable {
		t.Errorf("expected zero RecoveryMetadata from nil metadata, got %+v", got)
	}
}
