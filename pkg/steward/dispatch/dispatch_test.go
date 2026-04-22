package dispatch

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/steward/intent"
)

// fakeAttemptStore is an in-memory stand-in for pkg/store's attempt-bead
// semantics. It encodes the single property the dispatch contract rests on:
// once a bead has an open attempt, a selector that mirrors
// GetSchedulableWork stops offering that bead — so only one ClaimThenEmit
// cycle per bead ever reaches Emit.
//
// The store is not a correctness mechanism via mutex or sync.Map; it is a
// plain map, and the test treats it as the single-replica, single-threaded
// shared store the real attempt-bead row emulates. Tests exercise the
// sequential dispatch loop pkg/steward will run.
type fakeAttemptStore struct {
	readyIDs      []string          // IDs currently in the ready set
	attempts      map[string]string // parentID -> attemptID for OPEN attempts
	attemptSerial int
}

func newFakeAttemptStore(ready ...string) *fakeAttemptStore {
	s := &fakeAttemptStore{attempts: make(map[string]string)}
	s.readyIDs = append(s.readyIDs, ready...)
	return s
}

// SelectReady returns every ready ID that does NOT currently have an open
// attempt. That mirrors store.GetSchedulableWork's filter and is what
// drives the idempotent-emit property exercised in
// TestClaimThenEmit_SingleReadyBeadEmitsExactlyOnce.
func (s *fakeAttemptStore) SelectReady(_ context.Context) ([]string, error) {
	out := make([]string, 0, len(s.readyIDs))
	for _, id := range s.readyIDs {
		if _, open := s.attempts[id]; open {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}

// openAttempt atomically opens an attempt bead on parentID. Returns
// ("", false) when the parent already has an open attempt — callers treat
// that as "someone else already claimed".
func (s *fakeAttemptStore) openAttempt(parentID string) (string, bool) {
	if _, ok := s.attempts[parentID]; ok {
		return "", false
	}
	s.attemptSerial++
	attemptID := fmt.Sprintf("%s/attempt-%d", parentID, s.attemptSerial)
	s.attempts[parentID] = attemptID
	return attemptID, true
}

// hasOpenAttempt reports whether parentID currently has an open attempt.
// Tests read this to assert "a claim creates/marks the attempt bead open".
func (s *fakeAttemptStore) hasOpenAttempt(parentID string) bool {
	_, ok := s.attempts[parentID]
	return ok
}

// fakeClaimer satisfies AttemptClaimer against a fakeAttemptStore. It
// delegates uniqueness to the store — no mutex, no busy map, no sync.Map.
type fakeClaimer struct {
	store *fakeAttemptStore
	now   func() time.Time
}

func newFakeClaimer(s *fakeAttemptStore) *fakeClaimer {
	return &fakeClaimer{store: s}
}

func (c *fakeClaimer) ClaimNext(ctx context.Context, selector ReadyWorkSelector) (*AttemptHandle, error) {
	ids, err := selector.SelectReady(ctx)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if attemptID, ok := c.store.openAttempt(id); ok {
			return &AttemptHandle{AttemptID: attemptID, ClaimedAt: c.nowOr(time.Now)}, nil
		}
	}
	return nil, nil
}

func (c *fakeClaimer) nowOr(fallback func() time.Time) time.Time {
	if c.now != nil {
		return c.now().UTC()
	}
	return fallback().UTC()
}

// fakeEmitter records every Emit call that passes ValidateHandle. It is
// the single production-critical contract the emitter side enforces, so
// the fake calls ValidateHandle first exactly the way real transports
// must.
type fakeEmitter struct {
	calls   []intent.WorkloadIntent
	handles []*AttemptHandle
	onEmit  error
}

func (e *fakeEmitter) Emit(_ context.Context, handle *AttemptHandle, i intent.WorkloadIntent) error {
	if err := ValidateHandle(handle, i); err != nil {
		return err
	}
	e.calls = append(e.calls, i)
	e.handles = append(e.handles, handle)
	return e.onEmit
}

func buildIntentStampingAttemptID(h *AttemptHandle) intent.WorkloadIntent {
	return intent.WorkloadIntent{
		AttemptID:    h.AttemptID,
		FormulaPhase: "implement",
		RepoIdentity: intent.RepoIdentity{
			URL:        "https://example.com/repo.git",
			BaseBranch: "main",
			Prefix:     "spi",
		},
		Resources: intent.Resources{
			CPURequest:    "500m",
			CPULimit:      "1000m",
			MemoryRequest: "256Mi",
			MemoryLimit:   "1Gi",
		},
		HandoffMode: "bundle",
	}
}

// TestClaimThenEmit_ClaimMarksAttemptBeadOpen pins property (a): a
// successful claim leaves the backing attempt bead open in the shared
// store. The dispatch seam's correctness depends on this — without an
// open attempt bead, the selector would continue to offer the parent
// and a second cycle would emit again.
func TestClaimThenEmit_ClaimMarksAttemptBeadOpen(t *testing.T) {
	ctx := context.Background()
	st := newFakeAttemptStore("spi-alpha")
	claimer := newFakeClaimer(st)
	emitter := &fakeEmitter{}

	if err := ClaimThenEmit(ctx, claimer, emitter, st, buildIntentStampingAttemptID); err != nil {
		t.Fatalf("ClaimThenEmit: %v", err)
	}

	if !st.hasOpenAttempt("spi-alpha") {
		t.Fatalf("expected attempt open on spi-alpha, attempts=%v", st.attempts)
	}
	if got := len(emitter.calls); got != 1 {
		t.Fatalf("emitter calls = %d, want 1", got)
	}
	if got := emitter.calls[0].AttemptID; got == "" {
		t.Fatalf("emitted intent has empty AttemptID, want the claimed attempt ID")
	}
	if emitter.calls[0].AttemptID != emitter.handles[0].AttemptID {
		t.Fatalf("intent.AttemptID %q != handle.AttemptID %q",
			emitter.calls[0].AttemptID, emitter.handles[0].AttemptID)
	}
}

// TestClaimThenEmit_SingleReadyBeadEmitsExactlyOnce pins property (b):
// N repeated ClaimThenEmit cycles against the same single ready bead
// produce exactly ONE Emit call. The second and subsequent cycles see
// the attempt already open/claimed (because the selector mirrors
// GetSchedulableWork) and return nil without emitting.
func TestClaimThenEmit_SingleReadyBeadEmitsExactlyOnce(t *testing.T) {
	ctx := context.Background()
	st := newFakeAttemptStore("spi-beta")
	claimer := newFakeClaimer(st)
	emitter := &fakeEmitter{}

	const N = 5
	for i := 0; i < N; i++ {
		if err := ClaimThenEmit(ctx, claimer, emitter, st, buildIntentStampingAttemptID); err != nil {
			t.Fatalf("cycle %d: ClaimThenEmit: %v", i, err)
		}
	}

	if got := len(emitter.calls); got != 1 {
		t.Fatalf("after %d cycles: emitter calls = %d, want 1", N, got)
	}
	if !st.hasOpenAttempt("spi-beta") {
		t.Fatalf("expected attempt still open on spi-beta after %d cycles", N)
	}
}

// TestEmit_NilHandleReturnsErrNoClaimedAttempt pins property (c): calling
// Emit with a nil handle returns ErrNoClaimedAttempt. Every emitter that
// honors the seam contract (via ValidateHandle) has this behavior — the
// test calls the fake emitter directly so the Emit-side guard is pinned
// independent of ClaimThenEmit's orchestration.
func TestEmit_NilHandleReturnsErrNoClaimedAttempt(t *testing.T) {
	emitter := &fakeEmitter{}
	err := emitter.Emit(context.Background(), nil, intent.WorkloadIntent{AttemptID: "spi-whatever"})
	if !errors.Is(err, ErrNoClaimedAttempt) {
		t.Fatalf("Emit(nil handle) error = %v, want ErrNoClaimedAttempt", err)
	}
	if len(emitter.calls) != 0 {
		t.Fatalf("emitter recorded %d calls on nil-handle rejection, want 0", len(emitter.calls))
	}
}

// TestEmit_MismatchedAttemptIDReturnsErrNoClaimedAttempt covers the
// other leg of the ErrNoClaimedAttempt contract: Emit must refuse when
// the intent's AttemptID does not match the handle's AttemptID. This
// closes a subtle hole where a caller could claim one attempt and emit a
// WorkloadIntent built for a different attempt.
func TestEmit_MismatchedAttemptIDReturnsErrNoClaimedAttempt(t *testing.T) {
	emitter := &fakeEmitter{}
	handle := &AttemptHandle{AttemptID: "spi-alpha/attempt-1", ClaimedAt: time.Now().UTC()}
	err := emitter.Emit(context.Background(), handle, intent.WorkloadIntent{AttemptID: "spi-beta/attempt-9"})
	if !errors.Is(err, ErrNoClaimedAttempt) {
		t.Fatalf("Emit(mismatched AttemptID) error = %v, want ErrNoClaimedAttempt", err)
	}
	if len(emitter.calls) != 0 {
		t.Fatalf("emitter recorded %d calls on mismatch rejection, want 0", len(emitter.calls))
	}
}

// TestClaimThenEmit_NothingReadyDoesNotEmit pins the early-return path:
// when the selector yields no candidates, ClaimThenEmit returns nil
// without invoking Emit. This is what keeps the dispatch loop cheap in
// idle steady state.
func TestClaimThenEmit_NothingReadyDoesNotEmit(t *testing.T) {
	ctx := context.Background()
	st := newFakeAttemptStore() // no ready work
	claimer := newFakeClaimer(st)
	emitter := &fakeEmitter{}

	if err := ClaimThenEmit(ctx, claimer, emitter, st, buildIntentStampingAttemptID); err != nil {
		t.Fatalf("ClaimThenEmit: %v", err)
	}
	if got := len(emitter.calls); got != 0 {
		t.Fatalf("emitter calls = %d, want 0 when nothing ready", got)
	}
}

// TestClaimThenEmit_TwoDistinctReadyBeadsEmitTwice confirms the dispatch
// loop handles multiple independent ready beads correctly: two cycles
// over a store with two ready beads produce two Emit calls on distinct
// AttemptIDs. This balances the single-bead idempotency test by ruling
// out an over-suppressive implementation.
func TestClaimThenEmit_TwoDistinctReadyBeadsEmitTwice(t *testing.T) {
	ctx := context.Background()
	st := newFakeAttemptStore("spi-alpha", "spi-beta")
	claimer := newFakeClaimer(st)
	emitter := &fakeEmitter{}

	for i := 0; i < 2; i++ {
		if err := ClaimThenEmit(ctx, claimer, emitter, st, buildIntentStampingAttemptID); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
	}
	if got := len(emitter.calls); got != 2 {
		t.Fatalf("emitter calls = %d, want 2", got)
	}
	if emitter.calls[0].AttemptID == emitter.calls[1].AttemptID {
		t.Fatalf("both emits used same AttemptID %q; expected distinct", emitter.calls[0].AttemptID)
	}
}

// TestClaimThenEmit_HandleAttemptIDThreadsIntoIntent verifies the
// ClaimThenEmit → buildIntent → Emit path threads AttemptID end-to-end.
// If the handle's AttemptID does not reach the intent, ValidateHandle
// rejects and we surface ErrNoClaimedAttempt as the ClaimThenEmit error.
func TestClaimThenEmit_HandleAttemptIDThreadsIntoIntent(t *testing.T) {
	ctx := context.Background()
	st := newFakeAttemptStore("spi-gamma")
	claimer := newFakeClaimer(st)
	emitter := &fakeEmitter{}

	// A buggy buildIntent that drops the AttemptID must surface as
	// ErrNoClaimedAttempt from Emit — pinning the cross-check.
	badBuild := func(_ *AttemptHandle) intent.WorkloadIntent {
		return intent.WorkloadIntent{AttemptID: "wrong"}
	}
	err := ClaimThenEmit(ctx, claimer, emitter, st, badBuild)
	if !errors.Is(err, ErrNoClaimedAttempt) {
		t.Fatalf("ClaimThenEmit with mismatched buildIntent = %v, want ErrNoClaimedAttempt", err)
	}
}

// TestClaimThenEmit_NilArgsRejected confirms the guard clauses for nil
// dependencies — a caller wiring the seam incorrectly gets an explicit
// error rather than a nil-pointer panic. We do not test every combination;
// one per nil arg is enough to pin the contract.
func TestClaimThenEmit_NilArgsRejected(t *testing.T) {
	ctx := context.Background()
	st := newFakeAttemptStore("spi-delta")
	claimer := newFakeClaimer(st)
	emitter := &fakeEmitter{}

	cases := []struct {
		name     string
		claimer  AttemptClaimer
		emitter  DispatchEmitter
		selector ReadyWorkSelector
		build    func(*AttemptHandle) intent.WorkloadIntent
	}{
		{"nil claimer", nil, emitter, st, buildIntentStampingAttemptID},
		{"nil emitter", claimer, nil, st, buildIntentStampingAttemptID},
		{"nil selector", claimer, emitter, nil, buildIntentStampingAttemptID},
		{"nil buildIntent", claimer, emitter, st, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ClaimThenEmit(ctx, tc.claimer, tc.emitter, tc.selector, tc.build); err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

// TestAttemptHandle_ZeroValue pins the zero-value shape of AttemptHandle
// so downstream code can rely on `handle == nil` meaning "no claim" and
// handle.AttemptID == "" meaning a zero-value misuse rather than a
// successful claim.
func TestAttemptHandle_ZeroValue(t *testing.T) {
	var h AttemptHandle
	if h.AttemptID != "" {
		t.Errorf("zero AttemptHandle.AttemptID = %q, want empty", h.AttemptID)
	}
	if !h.ClaimedAt.IsZero() {
		t.Errorf("zero AttemptHandle.ClaimedAt = %v, want zero time", h.ClaimedAt)
	}
}

// TestValidateHandle_HappyPath pins the positive leg of the guard:
// matching handle + intent returns nil, and the emitter is free to
// proceed.
func TestValidateHandle_HappyPath(t *testing.T) {
	h := &AttemptHandle{AttemptID: "spi-x/attempt-1", ClaimedAt: time.Now().UTC()}
	i := intent.WorkloadIntent{AttemptID: "spi-x/attempt-1"}
	if err := ValidateHandle(h, i); err != nil {
		t.Fatalf("ValidateHandle matching = %v, want nil", err)
	}
}

// TestClaimerSurfacePropagatesError confirms that a claimer error
// bubbles up through ClaimThenEmit (wrapped). This is the "don't silently
// swallow" boundary for dispatch-loop observability.
func TestClaimerSurfacePropagatesError(t *testing.T) {
	ctx := context.Background()
	sentinel := errors.New("claimer-boom")
	claimer := &errClaimer{err: sentinel}
	emitter := &fakeEmitter{}
	sel := &staticSelector{ids: []string{"spi-foo"}}

	err := ClaimThenEmit(ctx, claimer, emitter, sel, buildIntentStampingAttemptID)
	if !errors.Is(err, sentinel) {
		t.Fatalf("ClaimThenEmit error = %v, want wrap of sentinel", err)
	}
	if len(emitter.calls) != 0 {
		t.Fatalf("emitter.calls = %d on claimer error, want 0", len(emitter.calls))
	}
}

type errClaimer struct{ err error }

func (e *errClaimer) ClaimNext(_ context.Context, _ ReadyWorkSelector) (*AttemptHandle, error) {
	return nil, e.err
}

type staticSelector struct{ ids []string }

func (s *staticSelector) SelectReady(_ context.Context) ([]string, error) {
	return s.ids, nil
}

// TestClaimThenEmit_EmitErrorSurfaces confirms emitter errors pass
// through unchanged — the claim has already happened, the attempt bead
// is open, and the caller needs the original error to decide how to
// recover (retry, mark failed, etc.).
func TestClaimThenEmit_EmitErrorSurfaces(t *testing.T) {
	ctx := context.Background()
	st := newFakeAttemptStore("spi-echo")
	claimer := newFakeClaimer(st)
	sentinel := errors.New("publisher-offline")
	emitter := &fakeEmitter{onEmit: sentinel}

	err := ClaimThenEmit(ctx, claimer, emitter, st, buildIntentStampingAttemptID)
	if !errors.Is(err, sentinel) {
		t.Fatalf("ClaimThenEmit error = %v, want wrap of sentinel", err)
	}
	if !st.hasOpenAttempt("spi-echo") {
		t.Fatalf("expected attempt still open after emit error")
	}
}

// Sanity-check: the interfaces compile with the fake implementations.
var (
	_ ReadyWorkSelector = (*fakeAttemptStore)(nil)
	_ AttemptClaimer    = (*fakeClaimer)(nil)
	_ DispatchEmitter   = (*fakeEmitter)(nil)
	_ ReadyWorkSelector = (*staticSelector)(nil)
	_ AttemptClaimer    = (*errClaimer)(nil)
)
