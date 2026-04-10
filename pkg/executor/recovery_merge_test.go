package executor

import (
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/store"
)

func TestMergeLearnings_SQLOnly(t *testing.T) {
	// When learnings exist only in SQL (human-authored via `spire resolve`),
	// they should appear in the merged results.
	sqlRows := []store.RecoveryLearningRow{
		{
			ID:              "lr-1",
			RecoveryBead:    "rec-100",
			SourceBead:      "spi-abc",
			FailureClass:    "implement-failed",
			ResolutionKind:  "reset",
			Outcome:         "clean",
			LearningSummary: "human learning from spire resolve",
			Reusable:        true,
			ResolvedAt:      time.Date(2026, 4, 5, 10, 0, 0, 0, time.UTC),
		},
	}

	merged := mergeLearnings(nil, sqlRows, 10)

	if len(merged) != 1 {
		t.Fatalf("expected 1 learning, got %d", len(merged))
	}
	if merged[0].BeadID != "rec-100" {
		t.Errorf("BeadID = %q, want rec-100", merged[0].BeadID)
	}
	if merged[0].LearningSummary != "human learning from spire resolve" {
		t.Errorf("LearningSummary = %q, want human learning", merged[0].LearningSummary)
	}
	if merged[0].Outcome != "clean" {
		t.Errorf("Outcome = %q, want clean", merged[0].Outcome)
	}
}

func TestMergeLearnings_MetadataOnly(t *testing.T) {
	// When learnings exist only in bead metadata (legacy), they still appear.
	metaLearnings := []store.RecoveryLearning{
		{
			BeadID:          "rec-200",
			SourceBead:      "spi-abc",
			FailureClass:    "implement-failed",
			ResolutionKind:  "resummon",
			Outcome:         "clean",
			LearningSummary: "legacy metadata learning",
			Reusable:        true,
			ResolvedAt:      "2026-04-05T09:00:00Z",
		},
	}

	merged := mergeLearnings(metaLearnings, nil, 10)

	if len(merged) != 1 {
		t.Fatalf("expected 1 learning, got %d", len(merged))
	}
	if merged[0].BeadID != "rec-200" {
		t.Errorf("BeadID = %q, want rec-200", merged[0].BeadID)
	}
	if merged[0].LearningSummary != "legacy metadata learning" {
		t.Errorf("LearningSummary = %q, want legacy metadata learning", merged[0].LearningSummary)
	}
}

func TestMergeLearnings_Dedup_SQLWins(t *testing.T) {
	// When the same recovery bead exists in both SQL and metadata,
	// the SQL version wins (no duplicates).
	metaLearnings := []store.RecoveryLearning{
		{
			BeadID:          "rec-300",
			SourceBead:      "spi-abc",
			FailureClass:    "implement-failed",
			LearningSummary: "stale metadata version",
			Outcome:         "clean",
			Reusable:        true,
			ResolvedAt:      "2026-04-05T10:00:00Z",
		},
	}
	sqlRows := []store.RecoveryLearningRow{
		{
			ID:              "lr-3",
			RecoveryBead:    "rec-300", // same recovery bead
			SourceBead:      "spi-abc",
			FailureClass:    "implement-failed",
			LearningSummary: "updated SQL version from spire resolve",
			Outcome:         "clean",
			Reusable:        true,
			ResolvedAt:      time.Date(2026, 4, 5, 10, 0, 0, 0, time.UTC),
		},
	}

	merged := mergeLearnings(metaLearnings, sqlRows, 10)

	if len(merged) != 1 {
		t.Fatalf("expected 1 learning (deduped), got %d", len(merged))
	}
	if merged[0].LearningSummary != "updated SQL version from spire resolve" {
		t.Errorf("should prefer SQL version, got LearningSummary = %q", merged[0].LearningSummary)
	}
}

func TestMergeLearnings_BothSources_DifferentBeads(t *testing.T) {
	// Learnings from different recovery beads in both sources should all appear.
	metaLearnings := []store.RecoveryLearning{
		{
			BeadID:          "rec-meta-1",
			SourceBead:      "spi-abc",
			FailureClass:    "implement-failed",
			LearningSummary: "metadata only",
			Reusable:        true,
			ResolvedAt:      "2026-04-05T08:00:00Z",
		},
	}
	sqlRows := []store.RecoveryLearningRow{
		{
			ID:              "lr-sql-1",
			RecoveryBead:    "rec-sql-1",
			SourceBead:      "spi-abc",
			FailureClass:    "implement-failed",
			LearningSummary: "sql only",
			Reusable:        true,
			ResolvedAt:      time.Date(2026, 4, 5, 12, 0, 0, 0, time.UTC),
		},
	}

	merged := mergeLearnings(metaLearnings, sqlRows, 10)

	if len(merged) != 2 {
		t.Fatalf("expected 2 learnings, got %d", len(merged))
	}
	// Should be sorted by resolved_at descending — SQL one is newer.
	if merged[0].BeadID != "rec-sql-1" {
		t.Errorf("first learning should be rec-sql-1 (newer), got %q", merged[0].BeadID)
	}
	if merged[1].BeadID != "rec-meta-1" {
		t.Errorf("second learning should be rec-meta-1 (older), got %q", merged[1].BeadID)
	}
}

func TestMergeLearnings_RespectsLimit(t *testing.T) {
	metaLearnings := []store.RecoveryLearning{
		{BeadID: "rec-a", ResolvedAt: "2026-04-05T10:00:00Z", Reusable: true},
		{BeadID: "rec-b", ResolvedAt: "2026-04-05T09:00:00Z", Reusable: true},
	}
	sqlRows := []store.RecoveryLearningRow{
		{RecoveryBead: "rec-c", ResolvedAt: time.Date(2026, 4, 5, 11, 0, 0, 0, time.UTC), Reusable: true},
	}

	merged := mergeLearnings(metaLearnings, sqlRows, 2)

	if len(merged) != 2 {
		t.Fatalf("expected 2 learnings (limited), got %d", len(merged))
	}
	// Newest two: rec-c (11:00) and rec-a (10:00)
	if merged[0].BeadID != "rec-c" {
		t.Errorf("first = %q, want rec-c", merged[0].BeadID)
	}
	if merged[1].BeadID != "rec-a" {
		t.Errorf("second = %q, want rec-a", merged[1].BeadID)
	}
}

func TestMergeLearnings_BothEmpty(t *testing.T) {
	merged := mergeLearnings(nil, nil, 10)
	if len(merged) != 0 {
		t.Errorf("expected 0 learnings, got %d", len(merged))
	}
}

func TestSqlRowToLearning_FieldMapping(t *testing.T) {
	row := store.RecoveryLearningRow{
		ID:              "lr-42",
		RecoveryBead:    "rec-42",
		SourceBead:      "spi-source",
		FailureClass:    "build-failed",
		FailureSig:      "sig-hash-abc",
		ResolutionKind:  "reset_to_step",
		Outcome:         "clean",
		LearningSummary: "the fix was to reset",
		Reusable:        true,
		ResolvedAt:      time.Date(2026, 4, 5, 15, 30, 0, 0, time.UTC),
		ExpectedOutcome: "clean",
	}

	l := sqlRowToLearning(row)

	if l.BeadID != "rec-42" {
		t.Errorf("BeadID = %q, want rec-42", l.BeadID)
	}
	if l.SourceBead != "spi-source" {
		t.Errorf("SourceBead = %q, want spi-source", l.SourceBead)
	}
	if l.FailureClass != "build-failed" {
		t.Errorf("FailureClass = %q, want build-failed", l.FailureClass)
	}
	if l.FailureSignature != "sig-hash-abc" {
		t.Errorf("FailureSignature = %q, want sig-hash-abc", l.FailureSignature)
	}
	if l.ResolutionKind != "reset_to_step" {
		t.Errorf("ResolutionKind = %q, want reset_to_step", l.ResolutionKind)
	}
	if l.Outcome != "clean" {
		t.Errorf("Outcome = %q, want clean", l.Outcome)
	}
	if l.LearningSummary != "the fix was to reset" {
		t.Errorf("LearningSummary = %q, want 'the fix was to reset'", l.LearningSummary)
	}
	if !l.Reusable {
		t.Error("Reusable should be true")
	}
	if l.ResolvedAt != "2026-04-05T15:30:00Z" {
		t.Errorf("ResolvedAt = %q, want 2026-04-05T15:30:00Z", l.ResolvedAt)
	}
}

func TestMergedListLearnings_RelapseDetection(t *testing.T) {
	// Verify that mergedListLearnings works with the relapseDeps interface —
	// specifically that SQL-sourced learnings with Outcome="clean" are eligible
	// for relapse marking (VerificationStatus will be empty from SQL, but
	// the relapse logic checks Outcome).
	now := time.Date(2026, 4, 5, 20, 0, 0, 0, time.UTC)
	resolvedAt := now.Add(-1 * time.Hour)

	// Simulate: a SQL-only learning with clean outcome.
	sqlLearning := store.RecoveryLearning{
		BeadID:       "rec-sql-relapse",
		SourceBead:   "spi-target",
		FailureClass: "build-failed",
		Outcome:      "clean",
		ResolvedAt:   resolvedAt.Format(time.RFC3339),
		// VerificationStatus is empty — this is the SQL-sourced case.
	}

	var markedBeadID string
	rd := relapseDeps{
		listLearnings: func(filter store.RecoveryLookupFilter) ([]store.RecoveryLearning, error) {
			return []store.RecoveryLearning{sqlLearning}, nil
		},
		setMetadata: func(beadID string, meta map[string]string) error {
			markedBeadID = beadID
			return nil
		},
		updateOutcomeSQL: func(beadID, outcome string) error { return nil },
	}

	checkAndMarkRelapseWith("spi-target", "build-failed", rd, now)

	// The relapse logic checks: VerificationStatus != "clean" && Outcome != "clean"
	// For SQL learning: "" != "clean" (true) && "clean" != "clean" (false) → false → doesn't skip
	// So the learning should be marked as relapsed.
	if markedBeadID != "rec-sql-relapse" {
		t.Errorf("expected rec-sql-relapse to be marked, got %q", markedBeadID)
	}
}
