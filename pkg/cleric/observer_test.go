package cleric

import (
	"errors"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/store"
)

// fakeObserverStore is an in-memory ObserverDeps backend used by the
// observer tests. It tracks calls so tests can assert which rows were
// finalized and with which success value.
type fakeObserverStore struct {
	pending  []store.ClericOutcome
	pendErr  error
	finalCh  []finalizeCall
	finalErr error
}

type finalizeCall struct {
	ID          string
	Success     bool
	FinalizedAt time.Time
}

func (f *fakeObserverStore) PendingForSourceBead(_ string) ([]store.ClericOutcome, error) {
	if f.pendErr != nil {
		return nil, f.pendErr
	}
	return f.pending, nil
}

func (f *fakeObserverStore) Finalize(id string, success bool, finalizedAt time.Time) error {
	if f.finalErr != nil {
		return f.finalErr
	}
	f.finalCh = append(f.finalCh, finalizeCall{ID: id, Success: success, FinalizedAt: finalizedAt})
	return nil
}

func (f *fakeObserverStore) deps(now time.Time) ObserverDeps {
	return ObserverDeps{
		PendingForSourceBead: f.PendingForSourceBead,
		Finalize:             f.Finalize,
		Now:                  func() time.Time { return now },
	}
}

func TestFinalizePendingOutcomes_MatchesTargetStep(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	f := &fakeObserverStore{
		pending: []store.ClericOutcome{
			{ID: "co-1", TargetStep: "implement"},
			{ID: "co-2", TargetStep: "review"},
		},
	}
	got := FinalizePendingOutcomes("spi-src", "implement", true, f.deps(now))
	if got != 1 {
		t.Fatalf("finalized = %d, want 1", got)
	}
	if len(f.finalCh) != 1 || f.finalCh[0].ID != "co-1" || !f.finalCh[0].Success {
		t.Errorf("finalize calls = %+v", f.finalCh)
	}
}

func TestFinalizePendingOutcomes_EmptyTargetStepIsWildcard(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	f := &fakeObserverStore{
		pending: []store.ClericOutcome{
			{ID: "co-wildcard", TargetStep: ""},
		},
	}
	got := FinalizePendingOutcomes("spi-src", "any-step", true, f.deps(now))
	if got != 1 {
		t.Fatalf("expected wildcard match (empty target_step), got %d", got)
	}
}

func TestFinalizePendingOutcomes_FailureMarksSuccessFalse(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	f := &fakeObserverStore{
		pending: []store.ClericOutcome{
			{ID: "co-1", TargetStep: "implement"},
		},
	}
	_ = FinalizePendingOutcomes("spi-src", "implement", false, f.deps(now))
	if len(f.finalCh) != 1 || f.finalCh[0].Success {
		t.Errorf("finalize success = %v, want false", f.finalCh[0].Success)
	}
}

func TestFinalizePendingOutcomes_PendingFetchErrorIsSilent(t *testing.T) {
	f := &fakeObserverStore{pendErr: errors.New("transient")}
	got := FinalizePendingOutcomes("spi-src", "implement", true, f.deps(time.Now()))
	if got != 0 {
		t.Fatalf("error path should finalize 0 rows, got %d", got)
	}
	if len(f.finalCh) != 0 {
		t.Fatalf("error path should not invoke Finalize, got %d calls", len(f.finalCh))
	}
}

func TestFinalizePendingOutcomes_NoSourceBeadIDSkipped(t *testing.T) {
	f := &fakeObserverStore{
		pending: []store.ClericOutcome{
			{ID: "co-1", TargetStep: "implement"},
		},
	}
	got := FinalizePendingOutcomes("", "implement", true, f.deps(time.Now()))
	if got != 0 {
		t.Fatalf("blank sourceBeadID should be a no-op, got %d", got)
	}
}

func TestFinalizePendingOutcomes_UnwiredSeamIsNoop(t *testing.T) {
	got := FinalizePendingOutcomes("spi-src", "implement", true, ObserverDeps{})
	if got != 0 {
		t.Fatalf("unwired deps should be a no-op, got %d", got)
	}
}

func TestFinalizePendingOutcomes_MixedTargetSteps(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	f := &fakeObserverStore{
		pending: []store.ClericOutcome{
			{ID: "co-explicit-match", TargetStep: "implement"},
			{ID: "co-wildcard", TargetStep: ""},
			{ID: "co-other", TargetStep: "review"},
		},
	}
	got := FinalizePendingOutcomes("spi-src", "implement", true, f.deps(now))
	if got != 2 {
		t.Fatalf("expected 2 finalizations (explicit + wildcard), got %d", got)
	}
	finalIDs := map[string]bool{}
	for _, c := range f.finalCh {
		finalIDs[c.ID] = true
	}
	if !finalIDs["co-explicit-match"] || !finalIDs["co-wildcard"] {
		t.Errorf("finalized = %v, want both co-explicit-match and co-wildcard", finalIDs)
	}
	if finalIDs["co-other"] {
		t.Errorf("co-other should not have been finalized")
	}
}
