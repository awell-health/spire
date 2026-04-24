package executor

import (
	"fmt"
	"testing"
	"time"

	spgit "github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/recovery"
)

// fakeClock is a deterministic clock used by retry-merge tests to avoid
// blocking for real wall time. Each Sleep advances now by the requested
// duration; Now returns the current cumulative value.
type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time                  { return c.now }
func (c *fakeClock) Sleep(d time.Duration)           { c.now = c.now.Add(d) }
func newFakeClock() *fakeClock                       { return &fakeClock{now: time.Now()} }
func (c *fakeClock) asSleep() func(time.Duration)    { return c.Sleep }
func (c *fakeClock) asNow() func() time.Time         { return c.Now }
func (c *fakeClock) elapsed(start time.Time) string  { return c.now.Sub(start).String() }

// TestMechanicalRetryMerge_WallTimeBudget verifies acceptance #3: under
// persistent contention, mechanicalRetryMerge returns within 60±2s wall
// clock and attempts ≥4 backoff rounds before returning. We swap in a
// controllable clock and a stub merge that always fails, so the wall-time
// budget is what actually trips the loop.
func TestMechanicalRetryMerge_WallTimeBudget(t *testing.T) {
	clock := newFakeClock()
	start := clock.now

	var attempts int
	restoreAttempt := installRetryMergeAttempt(func(_ *spgit.RepoContext, _, _ string, _ []string) error {
		attempts++
		return fmt.Errorf("synthetic contention (attempt %d)", attempts)
	})
	defer restoreAttempt()

	// Keep the default 60s wall time and default backoff schedule, but use
	// the fake clock so time passes without blocking.
	restore := testSetRetryMergeTuning(60*time.Second, retryMergeBackoffs, clock.asSleep(), clock.asNow())
	defer restore()

	ctx := &RecoveryActionCtx{
		RepoPath:     "/tmp/fake-repo",
		TargetBeadID: "",
		Log:          func(string) {},
	}
	plan := recovery.RepairPlan{Mode: recovery.RepairModeMechanical, Action: "retry-merge"}
	ws := WorkspaceHandle{Branch: "feat/spi-test", BaseBranch: "main"}

	recipe, err := mechanicalRetryMerge(ctx, plan, ws)
	if err == nil {
		t.Fatalf("expected wall-time exhausted error, got success with recipe %+v", recipe)
	}

	elapsed := clock.now.Sub(start)
	// Tolerate the last backoff potentially extending the final iteration
	// slightly past 60s; the loop treats 60s as the floor to stop.
	if elapsed < 58*time.Second || elapsed > 62*time.Second {
		t.Errorf("elapsed = %s, want 60±2s", elapsed)
	}
	if attempts < 4 {
		t.Errorf("attempts = %d, want ≥4", attempts)
	}
}

// TestMechanicalRetryMerge_SucceedsAfterRace verifies that a merge that
// fails the first few rounds but succeeds on round N still returns a recipe
// (no error) and records the successful round.
func TestMechanicalRetryMerge_SucceedsAfterRace(t *testing.T) {
	clock := newFakeClock()

	var attempts int
	restoreAttempt := installRetryMergeAttempt(func(_ *spgit.RepoContext, _, _ string, _ []string) error {
		attempts++
		if attempts >= 3 {
			return nil
		}
		return fmt.Errorf("synthetic race (round %d)", attempts)
	})
	defer restoreAttempt()

	restore := testSetRetryMergeTuning(60*time.Second, retryMergeBackoffs, clock.asSleep(), clock.asNow())
	defer restore()

	ctx := &RecoveryActionCtx{
		RepoPath: "/tmp/fake-repo",
		Log:      func(string) {},
	}
	plan := recovery.RepairPlan{Mode: recovery.RepairModeMechanical, Action: "retry-merge"}
	ws := WorkspaceHandle{Branch: "feat/spi-test", BaseBranch: "main"}

	recipe, err := mechanicalRetryMerge(ctx, plan, ws)
	if err != nil {
		t.Fatalf("expected success, got err=%v", err)
	}
	if recipe == nil {
		t.Fatal("expected non-nil recipe on success")
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3 (fail, fail, succeed)", attempts)
	}
}

// TestMechanicalRetryMerge_AttemptsAtLeastFourRounds verifies the backoff
// schedule packs ≥4 rounds inside the wall-time budget even when each
// attempt returns immediately. This is the minimum count acceptance #3
// requires.
func TestMechanicalRetryMerge_AttemptsAtLeastFourRounds(t *testing.T) {
	clock := newFakeClock()

	var attempts int
	restoreAttempt := installRetryMergeAttempt(func(_ *spgit.RepoContext, _, _ string, _ []string) error {
		attempts++
		return fmt.Errorf("still failing")
	})
	defer restoreAttempt()

	restore := testSetRetryMergeTuning(60*time.Second, retryMergeBackoffs, clock.asSleep(), clock.asNow())
	defer restore()

	ctx := &RecoveryActionCtx{
		RepoPath: "/tmp/fake-repo",
		Log:      func(string) {},
	}
	plan := recovery.RepairPlan{Mode: recovery.RepairModeMechanical, Action: "retry-merge"}
	ws := WorkspaceHandle{Branch: "feat/spi-test", BaseBranch: "main"}

	_, err := mechanicalRetryMerge(ctx, plan, ws)
	if err == nil {
		t.Fatal("expected error — attempt stub always fails")
	}
	// With backoffs 200ms, 500ms, 1s, 2s, 4s, 4s, 4s, ..., 60s fits many
	// more than 4 rounds; this asserts the minimum contract.
	if attempts < 4 {
		t.Errorf("attempts = %d, want ≥4", attempts)
	}
}

// installRetryMergeAttempt swaps the package-level retryMergeAttempt hook
// with a stub and returns a restore function. Unlike testSetRetryMergeTuning,
// it operates on the attempt function itself rather than the timing vars.
func installRetryMergeAttempt(stub func(*spgit.RepoContext, string, string, []string) error) func() {
	prev := retryMergeAttempt
	retryMergeAttempt = stub
	return func() { retryMergeAttempt = prev }
}

// TestRecoveryDisabled_DefaultOff confirms acceptance #8: recoveryDisabled
// returns false (cycle ON) when both env vars are unset.
func TestRecoveryDisabled_DefaultOff(t *testing.T) {
	t.Setenv("SPIRE_DISABLE_INLINE_RECOVERY", "")
	t.Setenv("SPIRE_INLINE_RECOVERY", "")

	e := &Executor{}
	if e.recoveryDisabled() {
		t.Error("recoveryDisabled() = true, want false (default-on)")
	}
}

// TestRecoveryDisabled_KillSwitch confirms acceptance #8: the new kill-switch
// SPIRE_DISABLE_INLINE_RECOVERY skips the cycle.
func TestRecoveryDisabled_KillSwitch(t *testing.T) {
	t.Setenv("SPIRE_DISABLE_INLINE_RECOVERY", "1")

	e := &Executor{}
	if !e.recoveryDisabled() {
		t.Error("recoveryDisabled() = false, want true (kill-switch on)")
	}
}

// TestRecoveryDisabled_OldEnvVarIgnored confirms the deprecated opt-in env
// var is no longer recognized — setting SPIRE_INLINE_RECOVERY=1 used to be
// the only way to turn recovery on; now recovery is on by default and this
// env var has no effect.
func TestRecoveryDisabled_OldEnvVarIgnored(t *testing.T) {
	t.Setenv("SPIRE_DISABLE_INLINE_RECOVERY", "")
	t.Setenv("SPIRE_INLINE_RECOVERY", "1")

	e := &Executor{}
	if e.recoveryDisabled() {
		t.Error("recoveryDisabled() = true, want false (default-on; old env var is deprecated)")
	}
}

// TestRecoveryDisabled_RecursionGuard confirms the recovery formulas
// themselves still skip the cycle (to prevent recursion).
func TestRecoveryDisabled_RecursionGuard(t *testing.T) {
	t.Setenv("SPIRE_DISABLE_INLINE_RECOVERY", "")

	for _, formula := range []string{"cleric-default", "recovery", "spire-recovery-v3"} {
		t.Run(formula, func(t *testing.T) {
			e := &Executor{
				graphState: &GraphState{Formula: formula},
			}
			if !e.recoveryDisabled() {
				t.Errorf("recoveryDisabled() = false for formula %q, want true (recursion guard)", formula)
			}
		})
	}
}
