package wizard

import (
	"errors"
	"testing"
)

// TestReviewFixFailureGuard_KilledWithNoStagedChanges is the regression test
// for spi-fljzd. When the fix apprentice's claude subprocess exits non-zero
// (e.g. signal: killed by timeout) AND nothing was staged, the guard must
// return a non-nil error so the graph interpreter treats the fix step as
// failed — preventing the formula-declared `resets` from firing against
// unchanged code and re-running sage-review on the same baseline SHA.
func TestReviewFixFailureGuard_KilledWithNoStagedChanges(t *testing.T) {
	killed := errors.New("signal: killed")
	err := reviewFixFailureGuard(true, false, killed)
	if err == nil {
		t.Fatal("expected error when review-fix apprentice is killed with no staged changes, got nil")
	}
	if !errors.Is(err, killed) {
		t.Errorf("expected wrapped killed error, got %v", err)
	}
}

// TestReviewFixFailureGuard_PartialChangesCommitted verifies that when
// claude is killed but managed to stage and commit partial changes, the
// guard does NOT fail — partial progress is better than no progress, and
// the build gate will catch broken code downstream.
func TestReviewFixFailureGuard_PartialChangesCommitted(t *testing.T) {
	killed := errors.New("signal: killed")
	if err := reviewFixFailureGuard(true, true, killed); err != nil {
		t.Errorf("expected nil when committed=true (partial progress), got %v", err)
	}
}

// TestReviewFixFailureGuard_SubprocessSucceededNoChanges verifies that a
// successful subprocess with zero staged changes is treated as legitimate
// (e.g. the issue was already fixed before the wizard ran). The guard
// must only fire on subprocess failure, not zero-staged alone.
func TestReviewFixFailureGuard_SubprocessSucceededNoChanges(t *testing.T) {
	if err := reviewFixFailureGuard(true, false, nil); err != nil {
		t.Errorf("expected nil when subprocess succeeded with no changes, got %v", err)
	}
}

// TestReviewFixFailureGuard_NotReviewFixMode verifies the guard is scoped
// to review-fix and does not fire on the normal implement path.
func TestReviewFixFailureGuard_NotReviewFixMode(t *testing.T) {
	killed := errors.New("signal: killed")
	if err := reviewFixFailureGuard(false, false, killed); err != nil {
		t.Errorf("expected nil when reviewFix=false, got %v", err)
	}
}
