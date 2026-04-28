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
		SourceFormula:      "task-default",
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

// TestFilterAndSortRecoveryPeers_TypeFilter pins the type-filter behavior:
// only beads with Type == "recovery" survive. Cleric foundation
// (spi-h2d7yn) — peer linkage must only chain across recovery beads.
func TestFilterAndSortRecoveryPeers_TypeFilter(t *testing.T) {
	in := []store.Bead{
		{ID: "spi-1", Type: "recovery", UpdatedAt: "2026-04-28T10:00:00Z"},
		{ID: "spi-2", Type: "task", UpdatedAt: "2026-04-28T11:00:00Z"},
		{ID: "spi-3", Type: "recovery", UpdatedAt: "2026-04-28T09:00:00Z"},
		{ID: "spi-4", Type: "bug", UpdatedAt: "2026-04-28T08:00:00Z"},
	}
	got := filterAndSortRecoveryPeers(in)
	if len(got) != 2 {
		t.Fatalf("filterAndSortRecoveryPeers length = %d, want 2 (recovery only)", len(got))
	}
	for _, b := range got {
		if b.Type != "recovery" {
			t.Errorf("non-recovery bead %s leaked through filter (type=%s)", b.ID, b.Type)
		}
	}
}

// TestFilterAndSortRecoveryPeers_MostRecentFirst pins the sort contract:
// peers must be ordered by UpdatedAt descending (most-recent first) so
// mostRecentPeerRecovery's "first non-self peer is the most recent" assertion
// holds. Cleric foundation (spi-h2d7yn).
func TestFilterAndSortRecoveryPeers_MostRecentFirst(t *testing.T) {
	in := []store.Bead{
		{ID: "spi-old", Type: "recovery", UpdatedAt: "2026-04-25T10:00:00Z"},
		{ID: "spi-new", Type: "recovery", UpdatedAt: "2026-04-28T10:00:00Z"},
		{ID: "spi-mid", Type: "recovery", UpdatedAt: "2026-04-26T10:00:00Z"},
	}
	got := filterAndSortRecoveryPeers(in)
	want := []string{"spi-new", "spi-mid", "spi-old"}
	if len(got) != len(want) {
		t.Fatalf("filterAndSortRecoveryPeers length = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].ID != w {
			t.Errorf("position %d: got %s, want %s (full order: %v)", i, got[i].ID, w, idsOf(got))
		}
	}
}

// TestFilterAndSortRecoveryPeers_TieBreakDeterministic pins the deterministic
// tie-break on equal timestamps: lexical descending on ID. Cleric foundation
// (spi-h2d7yn) — without a stable tie-break, the most-recent peer is
// ambiguous when two recoveries are filed in the same second, which would
// flake the related-dep wiring.
func TestFilterAndSortRecoveryPeers_TieBreakDeterministic(t *testing.T) {
	in := []store.Bead{
		{ID: "spi-aaa", Type: "recovery", UpdatedAt: "2026-04-28T10:00:00Z"},
		{ID: "spi-ccc", Type: "recovery", UpdatedAt: "2026-04-28T10:00:00Z"},
		{ID: "spi-bbb", Type: "recovery", UpdatedAt: "2026-04-28T10:00:00Z"},
	}
	got := filterAndSortRecoveryPeers(in)
	want := []string{"spi-ccc", "spi-bbb", "spi-aaa"}
	for i, w := range want {
		if got[i].ID != w {
			t.Errorf("position %d: got %s, want %s (full order: %v)", i, got[i].ID, w, idsOf(got))
		}
	}
}

// TestFilterAndSortRecoveryPeers_Empty handles the edge cases — nil input,
// no recovery beads, and recovery beads with empty UpdatedAt strings.
func TestFilterAndSortRecoveryPeers_Empty(t *testing.T) {
	if got := filterAndSortRecoveryPeers(nil); len(got) != 0 {
		t.Errorf("nil input should yield empty result, got %d beads", len(got))
	}
	noRecovery := []store.Bead{
		{ID: "spi-1", Type: "task"},
		{ID: "spi-2", Type: "bug"},
	}
	if got := filterAndSortRecoveryPeers(noRecovery); len(got) != 0 {
		t.Errorf("no recovery beads should yield empty result, got %d beads", len(got))
	}
	// Empty UpdatedAt parses to zero-value time.Time; equal-time tie-break
	// (lexical desc on ID) must kick in so the result is deterministic.
	emptyTimes := []store.Bead{
		{ID: "spi-1", Type: "recovery"},
		{ID: "spi-2", Type: "recovery"},
	}
	got := filterAndSortRecoveryPeers(emptyTimes)
	if len(got) != 2 || got[0].ID != "spi-2" {
		t.Errorf("empty timestamps: expected [spi-2, spi-1], got %v", idsOf(got))
	}
}

func idsOf(beads []store.Bead) []string {
	out := make([]string, len(beads))
	for i, b := range beads {
		out[i] = b.ID
	}
	return out
}
