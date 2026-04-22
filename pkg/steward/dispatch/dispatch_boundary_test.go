package dispatch

// Boundary tests for the ClaimThenEmit seam.
//
// These tests pin two invariants the steward's cluster-native dispatch
// rests on, independent of the rest of the contract exercised in
// dispatch_test.go:
//
//   1. Idempotency under repeated cycles. Given a single ready attempt,
//      N repeated ClaimThenEmit cycles emit exactly one WorkloadIntent.
//      Running the loop ten times is deliberate — it is enough to rule
//      out "every Nth cycle" behavior (a counter-gated suppressor) and
//      to catch a naive "emit on every cycle" regression that would
//      produce ten emits.
//
//   2. Emit-side proof-of-claim. A DispatchEmitter.Emit call whose
//      WorkloadIntent.AttemptID does not match the handle's AttemptID
//      must return ErrNoClaimedAttempt. This is the final guard against
//      a caller reusing a handle from one claim and building an intent
//      for a different attempt.
//
// Both properties are part of the cluster-native seam contract and are
// load-bearing for multi-replica safety; the store's atomic attempt
// row is the only ownership mechanism. If either property regresses,
// the scheduler could emit duplicate intents or leak unclaimed
// intents past the guard.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/steward/intent"
)

// boundaryAttemptStore is a single-ready-attempt fake backed by a flag
// recording whether the one attempt has been opened. It deliberately
// does NOT share state with dispatch_test.go's fakeAttemptStore — this
// test owns its own minimal fixture to make the boundary explicit.
//
// Semantics match the production store: once the attempt is opened,
// SelectReady returns an empty slice (the scheduler's view of the work
// drops the bead on subsequent cycles), and openAttempt refuses to
// re-open.
type boundaryAttemptStore struct {
	beadID    string
	opened    bool
	serial    int
	serialID  string
}

func newBoundaryAttemptStore(beadID string) *boundaryAttemptStore {
	return &boundaryAttemptStore{beadID: beadID}
}

// SelectReady returns the single bead ID only while no attempt is open,
// matching store.GetSchedulableWork's filter behavior.
func (s *boundaryAttemptStore) SelectReady(_ context.Context) ([]string, error) {
	if s.opened {
		return nil, nil
	}
	return []string{s.beadID}, nil
}

// openAttempt is the atomic-claim stand-in. Returns the new attempt ID
// and true on the first call, ("", false) thereafter.
func (s *boundaryAttemptStore) openAttempt() (string, bool) {
	if s.opened {
		return "", false
	}
	s.serial++
	s.opened = true
	s.serialID = s.beadID + "/attempt-" + decString(s.serial)
	return s.serialID, true
}

func decString(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// boundaryClaimer is the minimal AttemptClaimer wired on top of
// boundaryAttemptStore. It defers uniqueness to the store — no mutex,
// no busy map — mirroring the production StoreClaimer's contract.
type boundaryClaimer struct {
	store *boundaryAttemptStore
}

func (c *boundaryClaimer) ClaimNext(ctx context.Context, selector ReadyWorkSelector) (*AttemptHandle, error) {
	ids, err := selector.SelectReady(ctx)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	attemptID, ok := c.store.openAttempt()
	if !ok {
		return nil, nil
	}
	return &AttemptHandle{AttemptID: attemptID, ClaimedAt: time.Now().UTC()}, nil
}

// boundaryEmitter records every Emit call that passes ValidateHandle.
// The emitter deliberately calls ValidateHandle as its first step —
// every production DispatchEmitter must do the same — so the
// proof-of-claim contract is exercised uniformly.
type boundaryEmitter struct {
	calls []intent.WorkloadIntent
}

func (e *boundaryEmitter) Emit(_ context.Context, h *AttemptHandle, i intent.WorkloadIntent) error {
	if err := ValidateHandle(h, i); err != nil {
		return err
	}
	e.calls = append(e.calls, i)
	return nil
}

// buildBoundaryIntent stamps the handle's AttemptID onto a pre-built
// intent template, mirroring what cluster_dispatch.buildClusterIntent
// does in production.
func buildBoundaryIntent(h *AttemptHandle) intent.WorkloadIntent {
	return intent.WorkloadIntent{
		AttemptID:    h.AttemptID,
		FormulaPhase: "implement",
		RepoIdentity: intent.RepoIdentity{
			URL:        "https://example.test/repo.git",
			BaseBranch: "main",
			Prefix:     "spi",
		},
		HandoffMode: "bundle",
	}
}

// TestBoundary_ClaimThenEmit_TenCyclesEmitExactlyOnce pins the
// idempotency property: ten ClaimThenEmit cycles over a single ready
// attempt produce exactly one Emit invocation total. The first cycle
// claims + emits; cycles 2..10 see SelectReady return empty (because
// the attempt is now open) and short-circuit without emitting.
//
// Ten iterations is deliberate. Five (as in TestClaimThenEmit_SingleReadyBeadEmitsExactlyOnce)
// proves the property; ten is chosen because the task spec calls for
// N=10 as a stronger signal against subtler regressions (e.g. "every
// 7th cycle the guard is bypassed").
func TestBoundary_ClaimThenEmit_TenCyclesEmitExactlyOnce(t *testing.T) {
	ctx := context.Background()
	st := newBoundaryAttemptStore("spi-boundary")
	claimer := &boundaryClaimer{store: st}
	emitter := &boundaryEmitter{}

	const N = 10
	for i := 0; i < N; i++ {
		if err := ClaimThenEmit(ctx, claimer, emitter, st, buildBoundaryIntent); err != nil {
			t.Fatalf("cycle %d: ClaimThenEmit: %v", i, err)
		}
	}

	if got := len(emitter.calls); got != 1 {
		t.Fatalf("after %d cycles: emitter.calls = %d, want 1", N, got)
	}
	if !st.opened {
		t.Fatalf("after %d cycles: expected boundary store attempt still open, got opened=%v", N, st.opened)
	}
	if emitter.calls[0].AttemptID != st.serialID {
		t.Fatalf("emitted AttemptID = %q, want %q (the single claimed attempt)",
			emitter.calls[0].AttemptID, st.serialID)
	}
}

// TestBoundary_Emit_MismatchedAttemptID pins the proof-of-claim guard:
// an emitter that receives a handle whose AttemptID does not match the
// intent's AttemptID must return ErrNoClaimedAttempt and record no
// call. This is the final barrier against a caller leaking an intent
// that was never claimed with the handle it was emitted under.
//
// The test calls boundaryEmitter.Emit directly — independent of
// ClaimThenEmit — so the property is pinned on the Emit side even if a
// regression hides the violation inside the orchestration helper.
func TestBoundary_Emit_MismatchedAttemptID(t *testing.T) {
	emitter := &boundaryEmitter{}
	handle := &AttemptHandle{
		AttemptID: "spi-alpha/attempt-1",
		ClaimedAt: time.Now().UTC(),
	}
	mismatched := intent.WorkloadIntent{
		AttemptID: "spi-alpha/attempt-7", // not the handle's AttemptID
		FormulaPhase: "implement",
	}

	err := emitter.Emit(context.Background(), handle, mismatched)
	if !errors.Is(err, ErrNoClaimedAttempt) {
		t.Fatalf("Emit(mismatched AttemptID) = %v, want ErrNoClaimedAttempt", err)
	}
	if len(emitter.calls) != 0 {
		t.Fatalf("emitter recorded %d calls on mismatch rejection, want 0", len(emitter.calls))
	}
}

// TestBoundary_Emit_NilHandle adds the companion guard test for the nil
// handle path — Emit called with (nil handle, anything) must return
// ErrNoClaimedAttempt. The property is already pinned at the
// ValidateHandle layer; this test locks it in at the Emit layer too,
// the level callers actually invoke in production.
func TestBoundary_Emit_NilHandle(t *testing.T) {
	emitter := &boundaryEmitter{}

	err := emitter.Emit(context.Background(), nil, intent.WorkloadIntent{AttemptID: "spi-whatever"})
	if !errors.Is(err, ErrNoClaimedAttempt) {
		t.Fatalf("Emit(nil handle) = %v, want ErrNoClaimedAttempt", err)
	}
	if len(emitter.calls) != 0 {
		t.Fatalf("emitter recorded %d calls on nil-handle rejection, want 0", len(emitter.calls))
	}
}

// TestBoundary_ClaimThenEmit_TenCyclesAttemptIDStableAcrossSkips pins a
// subtler property that sits adjacent to the main idempotency
// invariant: the AttemptID on the single emit must equal the attempt
// the claimer actually opened. A regression that claimed one attempt
// and emitted under a different one would fail the proof-of-claim
// guard, but a regression that silently chose a synthetic AttemptID
// would pass the emit step yet detach the emitted intent from the
// ownership row. Asserting the stored serial ID matches the emitted
// AttemptID closes that gap.
func TestBoundary_ClaimThenEmit_TenCyclesAttemptIDStableAcrossSkips(t *testing.T) {
	ctx := context.Background()
	st := newBoundaryAttemptStore("spi-stable")
	claimer := &boundaryClaimer{store: st}
	emitter := &boundaryEmitter{}

	for i := 0; i < 10; i++ {
		if err := ClaimThenEmit(ctx, claimer, emitter, st, buildBoundaryIntent); err != nil {
			t.Fatalf("cycle %d: ClaimThenEmit: %v", i, err)
		}
	}

	if len(emitter.calls) != 1 {
		t.Fatalf("emitter.calls = %d, want 1", len(emitter.calls))
	}
	if emitter.calls[0].AttemptID != st.serialID {
		t.Fatalf("emit AttemptID = %q, want %q (must match the claim's serial ID)",
			emitter.calls[0].AttemptID, st.serialID)
	}
	if st.serial != 1 {
		t.Fatalf("store serial = %d, want 1 (only one open attempt across %d cycles)",
			st.serial, 10)
	}
}

// Compile-time conformance for the fakes.
var (
	_ ReadyWorkSelector = (*boundaryAttemptStore)(nil)
	_ AttemptClaimer    = (*boundaryClaimer)(nil)
	_ DispatchEmitter   = (*boundaryEmitter)(nil)
)
