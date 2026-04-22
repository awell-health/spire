//go:build e2e

package helpers

import (
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
)

// WaitForClericDispatch blocks until the wisp's status transitions from
// "open" to "in_progress" (signalling steward hooked-sweep picked it up
// and a cleric agent was dispatched). The cleric-default formula's first
// signal is the in_progress transition — subsequent phases stamp more
// metadata.
//
// Returns the observed in_progress wisp. Fatals on timeout with the
// last-seen status so flake triage is straightforward.
func WaitForClericDispatch(t *testing.T, wispID string, timeout time.Duration) store.Bead {
	t.Helper()
	return WaitForBeadStatus(t, wispID, "in_progress", timeout)
}

// ReadOutcomeForWisp fetches the wisp bead then delegates to
// recovery.ReadOutcome, which parses the persisted RecoveryOutcome JSON
// blob (under metadata key recovery_outcome) written by WriteOutcome.
//
// Returns (outcome, true) when an outcome has been written, or
// (zero, false) for beads that never reached the finish step — the
// caller decides whether the absence is a test failure (e.g. after
// cleric dispatch completes) or a wait condition (e.g. right after
// the wisp is filed).
func ReadOutcomeForWisp(t *testing.T, wispID string) (recovery.RecoveryOutcome, bool) {
	t.Helper()
	b := GetBeadByID(t, wispID)
	return recovery.ReadOutcome(b)
}

// WaitForOutcome polls ReadOutcomeForWisp until a RecoveryOutcome is
// present or the timeout elapses. This is the signal that the cleric
// completed its finish step (see pkg/recovery/finish.go:WriteOutcome).
func WaitForOutcome(t *testing.T, wispID string, timeout time.Duration) recovery.RecoveryOutcome {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, ok := ReadOutcomeForWisp(t, wispID)
		if ok {
			return out
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("no RecoveryOutcome written for wisp=%s within %s — cleric finish step did not run", wispID, timeout)
	return recovery.RecoveryOutcome{}
}
