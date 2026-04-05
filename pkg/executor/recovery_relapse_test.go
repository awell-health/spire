package executor

import (
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/store"
)

func TestCheckAndMarkRelapse_EmptyInputs(t *testing.T) {
	called := false
	rd := relapseDeps{
		listLearnings: func(filter store.RecoveryLookupFilter) ([]store.RecoveryLearning, error) {
			called = true
			return nil, nil
		},
	}
	checkAndMarkRelapseWith("", "failure-class", rd, time.Now())
	if called {
		t.Error("should not query when sourceBeadID is empty")
	}

	checkAndMarkRelapseWith("spi-abc", "", rd, time.Now())
	if called {
		t.Error("should not query when failureClass is empty")
	}
}

func TestCheckAndMarkRelapse_CleanWithin24h(t *testing.T) {
	now := time.Date(2026, 4, 5, 20, 0, 0, 0, time.UTC)
	resolvedAt := now.Add(-2 * time.Hour) // 2 hours ago — within window.

	var metadataUpdated, sqlUpdated, commented bool
	var updatedBeadID, updatedOutcome string

	rd := relapseDeps{
		listLearnings: func(filter store.RecoveryLookupFilter) ([]store.RecoveryLearning, error) {
			return []store.RecoveryLearning{
				{
					BeadID:             "rec-001",
					SourceBead:         "spi-abc",
					FailureClass:       "implement-failed",
					VerificationStatus: "clean",
					Outcome:            "clean",
					ResolvedAt:         resolvedAt.Format(time.RFC3339),
				},
			}, nil
		},
		setMetadata: func(beadID string, meta map[string]string) error {
			metadataUpdated = true
			updatedBeadID = beadID
			return nil
		},
		updateOutcomeSQL: func(beadID, outcome string) error {
			sqlUpdated = true
			updatedOutcome = outcome
			return nil
		},
		addComment: func(beadID, text string) error {
			commented = true
			return nil
		},
	}

	checkAndMarkRelapseWith("spi-abc", "implement-failed", rd, now)

	if !metadataUpdated {
		t.Error("expected metadata to be updated")
	}
	if updatedBeadID != "rec-001" {
		t.Errorf("metadata updated for %q, want rec-001", updatedBeadID)
	}
	if !sqlUpdated {
		t.Error("expected SQL outcome to be updated")
	}
	if updatedOutcome != "relapsed" {
		t.Errorf("SQL outcome = %q, want relapsed", updatedOutcome)
	}
	if !commented {
		t.Error("expected comment to be added")
	}
}

func TestCheckAndMarkRelapse_CleanBeyond24h(t *testing.T) {
	now := time.Date(2026, 4, 5, 20, 0, 0, 0, time.UTC)
	resolvedAt := now.Add(-25 * time.Hour) // 25 hours ago — outside window.

	var metadataUpdated bool

	rd := relapseDeps{
		listLearnings: func(filter store.RecoveryLookupFilter) ([]store.RecoveryLearning, error) {
			return []store.RecoveryLearning{
				{
					BeadID:             "rec-002",
					SourceBead:         "spi-abc",
					FailureClass:       "implement-failed",
					VerificationStatus: "clean",
					Outcome:            "clean",
					ResolvedAt:         resolvedAt.Format(time.RFC3339),
				},
			}, nil
		},
		setMetadata: func(beadID string, meta map[string]string) error {
			metadataUpdated = true
			return nil
		},
	}

	checkAndMarkRelapseWith("spi-abc", "implement-failed", rd, now)

	if metadataUpdated {
		t.Error("should NOT mark relapse when resolved >24h ago")
	}
}

func TestCheckAndMarkRelapse_AlreadyRelapsed(t *testing.T) {
	now := time.Date(2026, 4, 5, 20, 0, 0, 0, time.UTC)
	resolvedAt := now.Add(-1 * time.Hour)

	var metadataUpdated bool

	rd := relapseDeps{
		listLearnings: func(filter store.RecoveryLookupFilter) ([]store.RecoveryLearning, error) {
			return []store.RecoveryLearning{
				{
					BeadID:             "rec-003",
					SourceBead:         "spi-abc",
					FailureClass:       "implement-failed",
					VerificationStatus: "clean",
					Outcome:            "relapsed", // already marked
					ResolvedAt:         resolvedAt.Format(time.RFC3339),
				},
			}, nil
		},
		setMetadata: func(beadID string, meta map[string]string) error {
			metadataUpdated = true
			return nil
		},
	}

	checkAndMarkRelapseWith("spi-abc", "implement-failed", rd, now)

	if metadataUpdated {
		t.Error("should NOT re-mark an already relapsed learning")
	}
}

func TestCheckAndMarkRelapse_DirtyOutcomeSkipped(t *testing.T) {
	now := time.Date(2026, 4, 5, 20, 0, 0, 0, time.UTC)
	resolvedAt := now.Add(-1 * time.Hour)

	var metadataUpdated bool

	rd := relapseDeps{
		listLearnings: func(filter store.RecoveryLookupFilter) ([]store.RecoveryLearning, error) {
			return []store.RecoveryLearning{
				{
					BeadID:             "rec-004",
					SourceBead:         "spi-abc",
					FailureClass:       "implement-failed",
					VerificationStatus: "dirty",
					Outcome:            "dirty", // already known-bad
					ResolvedAt:         resolvedAt.Format(time.RFC3339),
				},
			}, nil
		},
		setMetadata: func(beadID string, meta map[string]string) error {
			metadataUpdated = true
			return nil
		},
	}

	checkAndMarkRelapseWith("spi-abc", "implement-failed", rd, now)

	if metadataUpdated {
		t.Error("should NOT mark dirty outcomes as relapsed")
	}
}

func TestCheckAndMarkRelapse_VerificationCleanButOutcomeEmpty(t *testing.T) {
	// VerificationStatus=clean but Outcome="" — should still be marked as relapsed.
	now := time.Date(2026, 4, 5, 20, 0, 0, 0, time.UTC)
	resolvedAt := now.Add(-1 * time.Hour)

	var metadataUpdated bool

	rd := relapseDeps{
		listLearnings: func(filter store.RecoveryLookupFilter) ([]store.RecoveryLearning, error) {
			return []store.RecoveryLearning{
				{
					BeadID:             "rec-005",
					SourceBead:         "spi-abc",
					FailureClass:       "implement-failed",
					VerificationStatus: "clean",
					Outcome:            "", // empty — treated like clean
					ResolvedAt:         resolvedAt.Format(time.RFC3339),
				},
			}, nil
		},
		setMetadata: func(beadID string, meta map[string]string) error {
			metadataUpdated = true
			return nil
		},
		updateOutcomeSQL: func(beadID, outcome string) error { return nil },
	}

	checkAndMarkRelapseWith("spi-abc", "implement-failed", rd, now)

	if !metadataUpdated {
		t.Error("should mark relapse when VerificationStatus=clean even if Outcome is empty")
	}
}

func TestCheckAndMarkRelapse_NoLearnings(t *testing.T) {
	rd := relapseDeps{
		listLearnings: func(filter store.RecoveryLookupFilter) ([]store.RecoveryLearning, error) {
			return nil, nil
		},
		setMetadata: func(beadID string, meta map[string]string) error {
			t.Error("should not be called with no learnings")
			return nil
		},
	}

	checkAndMarkRelapseWith("spi-abc", "implement-failed", rd, time.Now())
}
