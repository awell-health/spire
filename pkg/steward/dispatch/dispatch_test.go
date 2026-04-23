package dispatch

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/steward/intent"
)

// fakeDispatchStore is an in-memory stand-in for the store-backed
// dispatch surface. It models two properties the dispatch contract rests on:
//
//  1. An active attempt on a task blocks redispatch until the attempt closes.
//     The fake tracks which task IDs currently have an "active attempt"
//     (set by the wizard path in production; set via setActiveAttempt here)
//     and drops them from SelectReady.
//  2. Each task gets a monotonically-increasing dispatch_seq on redispatch.
//     The fake hands out seq = (prior emits for that task) + 1.
//
// The fake is not a correctness mechanism via mutex or sync.Map; it is a
// plain map, and the test treats it as the single-replica, single-threaded
// shared store the real workload_intents PK emulates.
type fakeDispatchStore struct {
	readyIDs       []string        // candidate task IDs
	activeAttempts map[string]bool // tasks with an open wizard attempt
	priorEmits     map[string]int  // task_id -> count of prior emits
}

func newFakeDispatchStore(ready ...string) *fakeDispatchStore {
	s := &fakeDispatchStore{
		activeAttempts: make(map[string]bool),
		priorEmits:     make(map[string]int),
	}
	s.readyIDs = append(s.readyIDs, ready...)
	return s
}

// SelectReady returns every ready task ID that does NOT currently have an
// active attempt. Mirrors store.GetSchedulableWork's filter behavior.
func (s *fakeDispatchStore) SelectReady(_ context.Context) ([]string, error) {
	out := make([]string, 0, len(s.readyIDs))
	for _, id := range s.readyIDs {
		if s.activeAttempts[id] {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}

// setActiveAttempt marks a task as currently claimed by a wizard. In
// production this happens when the wizard calls ensureAttemptBead.
func (s *fakeDispatchStore) setActiveAttempt(taskID string, active bool) {
	s.activeAttempts[taskID] = active
}

// recordEmit bumps priorEmits[taskID]; tests call this after a successful
// Emit to make subsequent dispatch_seq derivations match production's
// max(seq)+1 semantics.
func (s *fakeDispatchStore) recordEmit(taskID string) int {
	s.priorEmits[taskID]++
	return s.priorEmits[taskID]
}

// dispatchSeqFn returns the fake's view of "next dispatch_seq for taskID",
// intended as the DispatchSeqFn hook on StoreClaimer or a test-local
// claimer. Reads priorEmits + 1.
func (s *fakeDispatchStore) dispatchSeqFn(_ context.Context, taskID string) (int, error) {
	return s.priorEmits[taskID] + 1, nil
}

// fakeClaimer satisfies AttemptClaimer against a fakeDispatchStore. It
// delegates uniqueness to the store — no mutex, no busy map.
type fakeClaimer struct {
	store *fakeDispatchStore
	now   func() time.Time
}

func newFakeClaimer(s *fakeDispatchStore) *fakeClaimer {
	return &fakeClaimer{store: s}
}

func (c *fakeClaimer) ClaimNext(ctx context.Context, selector ReadyWorkSelector) (*ClaimHandle, error) {
	ids, err := selector.SelectReady(ctx)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		seq, _ := c.store.dispatchSeqFn(ctx, id)
		return &ClaimHandle{
			TaskID:      id,
			DispatchSeq: seq,
			ClaimedAt:   c.nowOr(time.Now),
		}, nil
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
// must. The store hook is notified on successful emit so dispatch_seq
// increments for subsequent claims on the same task.
type fakeEmitter struct {
	calls   []intent.WorkloadIntent
	handles []*ClaimHandle
	onEmit  error
	store   *fakeDispatchStore
}

func (e *fakeEmitter) Emit(_ context.Context, handle *ClaimHandle, i intent.WorkloadIntent) error {
	if err := ValidateHandle(handle, i); err != nil {
		return err
	}
	e.calls = append(e.calls, i)
	e.handles = append(e.handles, handle)
	if e.store != nil {
		e.store.recordEmit(i.TaskID)
		// Simulate the wizard creating an attempt once the pod starts:
		// mark the task active so redispatch is blocked.
		e.store.setActiveAttempt(i.TaskID, true)
	}
	return e.onEmit
}

func buildIntentStampingClaim(h *ClaimHandle) intent.WorkloadIntent {
	return intent.WorkloadIntent{
		TaskID:       h.TaskID,
		DispatchSeq:  h.DispatchSeq,
		Reason:       h.Reason,
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

// TestClaimThenEmit_EmitSeedsActiveAttempt pins property (a): a
// successful emit plus the subsequent wizard pod path (simulated here by
// the emitter marking the task active) leaves the task invisible to
// SelectReady. Without that, a second cycle would emit again.
func TestClaimThenEmit_EmitSeedsActiveAttempt(t *testing.T) {
	ctx := context.Background()
	st := newFakeDispatchStore("spi-alpha")
	claimer := newFakeClaimer(st)
	emitter := &fakeEmitter{store: st}

	if err := ClaimThenEmit(ctx, claimer, emitter, st, buildIntentStampingClaim); err != nil {
		t.Fatalf("ClaimThenEmit: %v", err)
	}

	if !st.activeAttempts["spi-alpha"] {
		t.Fatalf("expected task active on spi-alpha, active=%v", st.activeAttempts)
	}
	if got := len(emitter.calls); got != 1 {
		t.Fatalf("emitter calls = %d, want 1", got)
	}
	if emitter.calls[0].TaskID != "spi-alpha" {
		t.Fatalf("emitted TaskID = %q, want spi-alpha", emitter.calls[0].TaskID)
	}
	if emitter.calls[0].DispatchSeq != 1 {
		t.Fatalf("emitted DispatchSeq = %d, want 1", emitter.calls[0].DispatchSeq)
	}
	if emitter.calls[0].TaskID != emitter.handles[0].TaskID {
		t.Fatalf("intent.TaskID %q != handle.TaskID %q",
			emitter.calls[0].TaskID, emitter.handles[0].TaskID)
	}
}

// TestClaimThenEmit_SingleReadyBeadEmitsExactlyOnce pins property (b):
// N repeated ClaimThenEmit cycles against the same single ready task
// produce exactly ONE Emit call. The second and subsequent cycles see
// the task active (the emitter marks it so) and selector drops it.
func TestClaimThenEmit_SingleReadyBeadEmitsExactlyOnce(t *testing.T) {
	ctx := context.Background()
	st := newFakeDispatchStore("spi-beta")
	claimer := newFakeClaimer(st)
	emitter := &fakeEmitter{store: st}

	const N = 5
	for i := 0; i < N; i++ {
		if err := ClaimThenEmit(ctx, claimer, emitter, st, buildIntentStampingClaim); err != nil {
			t.Fatalf("cycle %d: ClaimThenEmit: %v", i, err)
		}
	}

	if got := len(emitter.calls); got != 1 {
		t.Fatalf("after %d cycles: emitter calls = %d, want 1", N, got)
	}
	if !st.activeAttempts["spi-beta"] {
		t.Fatalf("expected spi-beta active after %d cycles", N)
	}
}

// TestEmit_NilHandleReturnsErrNoClaimedAttempt pins property (c): calling
// Emit with a nil handle returns ErrNoClaimedAttempt. Every emitter that
// honors the seam contract (via ValidateHandle) has this behavior.
func TestEmit_NilHandleReturnsErrNoClaimedAttempt(t *testing.T) {
	emitter := &fakeEmitter{}
	err := emitter.Emit(context.Background(), nil, intent.WorkloadIntent{TaskID: "spi-whatever", DispatchSeq: 1})
	if !errors.Is(err, ErrNoClaimedAttempt) {
		t.Fatalf("Emit(nil handle) error = %v, want ErrNoClaimedAttempt", err)
	}
	if len(emitter.calls) != 0 {
		t.Fatalf("emitter recorded %d calls on nil-handle rejection, want 0", len(emitter.calls))
	}
}

// TestEmit_MismatchedTaskIDReturnsErrNoClaimedAttempt covers the
// TaskID leg of the ErrNoClaimedAttempt contract: Emit must refuse when
// the intent's TaskID does not match the handle's TaskID.
func TestEmit_MismatchedTaskIDReturnsErrNoClaimedAttempt(t *testing.T) {
	emitter := &fakeEmitter{}
	handle := &ClaimHandle{TaskID: "spi-alpha", DispatchSeq: 1, ClaimedAt: time.Now().UTC()}
	err := emitter.Emit(context.Background(), handle, intent.WorkloadIntent{TaskID: "spi-beta", DispatchSeq: 1})
	if !errors.Is(err, ErrNoClaimedAttempt) {
		t.Fatalf("Emit(mismatched TaskID) error = %v, want ErrNoClaimedAttempt", err)
	}
	if len(emitter.calls) != 0 {
		t.Fatalf("emitter recorded %d calls on mismatch rejection, want 0", len(emitter.calls))
	}
}

// TestEmit_MismatchedDispatchSeqReturnsErrNoClaimedAttempt covers the
// DispatchSeq leg of the guard.
func TestEmit_MismatchedDispatchSeqReturnsErrNoClaimedAttempt(t *testing.T) {
	emitter := &fakeEmitter{}
	handle := &ClaimHandle{TaskID: "spi-alpha", DispatchSeq: 1, ClaimedAt: time.Now().UTC()}
	err := emitter.Emit(context.Background(), handle, intent.WorkloadIntent{TaskID: "spi-alpha", DispatchSeq: 2})
	if !errors.Is(err, ErrNoClaimedAttempt) {
		t.Fatalf("Emit(mismatched DispatchSeq) error = %v, want ErrNoClaimedAttempt", err)
	}
}

// TestClaimThenEmit_NothingReadyDoesNotEmit pins the early-return path.
func TestClaimThenEmit_NothingReadyDoesNotEmit(t *testing.T) {
	ctx := context.Background()
	st := newFakeDispatchStore() // no ready work
	claimer := newFakeClaimer(st)
	emitter := &fakeEmitter{store: st}

	if err := ClaimThenEmit(ctx, claimer, emitter, st, buildIntentStampingClaim); err != nil {
		t.Fatalf("ClaimThenEmit: %v", err)
	}
	if got := len(emitter.calls); got != 0 {
		t.Fatalf("emitter calls = %d, want 0 when nothing ready", got)
	}
}

// TestClaimThenEmit_TwoDistinctReadyBeadsEmitTwice confirms the dispatch
// loop handles multiple independent ready tasks correctly: two cycles
// over a store with two ready tasks produce two Emit calls on distinct
// task IDs.
func TestClaimThenEmit_TwoDistinctReadyBeadsEmitTwice(t *testing.T) {
	ctx := context.Background()
	st := newFakeDispatchStore("spi-alpha", "spi-beta")
	claimer := newFakeClaimer(st)
	emitter := &fakeEmitter{store: st}

	for i := 0; i < 2; i++ {
		if err := ClaimThenEmit(ctx, claimer, emitter, st, buildIntentStampingClaim); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
	}
	if got := len(emitter.calls); got != 2 {
		t.Fatalf("emitter calls = %d, want 2", got)
	}
	if emitter.calls[0].TaskID == emitter.calls[1].TaskID {
		t.Fatalf("both emits used same TaskID %q; expected distinct", emitter.calls[0].TaskID)
	}
}

// TestClaimThenEmit_RetryAfterWizardDeathBumpsDispatchSeq pins the
// re-dispatch semantics: when a previous attempt closes (e.g. wizard
// pod died), the next dispatch for the same task bumps dispatch_seq.
// Same task ID, fresh (task_id, dispatch_seq) row.
func TestClaimThenEmit_RetryAfterWizardDeathBumpsDispatchSeq(t *testing.T) {
	ctx := context.Background()
	st := newFakeDispatchStore("spi-retry")
	claimer := newFakeClaimer(st)
	emitter := &fakeEmitter{store: st}

	// Cycle 1: fresh dispatch, seq=1, task becomes active.
	if err := ClaimThenEmit(ctx, claimer, emitter, st, buildIntentStampingClaim); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	if emitter.calls[0].DispatchSeq != 1 {
		t.Fatalf("cycle 1 seq = %d, want 1", emitter.calls[0].DispatchSeq)
	}

	// Wizard dies: the attempt closes. Simulate by clearing active.
	st.setActiveAttempt("spi-retry", false)

	// Cycle 2: task is ready again, dispatch_seq bumps to 2.
	if err := ClaimThenEmit(ctx, claimer, emitter, st, buildIntentStampingClaim); err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if len(emitter.calls) != 2 {
		t.Fatalf("emitter.calls = %d, want 2", len(emitter.calls))
	}
	if emitter.calls[1].TaskID != "spi-retry" {
		t.Fatalf("cycle 2 TaskID = %q, want spi-retry", emitter.calls[1].TaskID)
	}
	if emitter.calls[1].DispatchSeq != 2 {
		t.Fatalf("cycle 2 seq = %d, want 2", emitter.calls[1].DispatchSeq)
	}
}

// TestClaimThenEmit_HandleThreadsIntoIntent verifies the
// ClaimThenEmit → buildIntent → Emit path threads both TaskID and
// DispatchSeq end-to-end. If either gets dropped, ValidateHandle
// rejects and surfaces ErrNoClaimedAttempt.
func TestClaimThenEmit_HandleThreadsIntoIntent(t *testing.T) {
	ctx := context.Background()
	st := newFakeDispatchStore("spi-gamma")
	claimer := newFakeClaimer(st)
	emitter := &fakeEmitter{}

	// buildIntent that drops the TaskID must surface as ErrNoClaimedAttempt.
	dropTaskID := func(h *ClaimHandle) intent.WorkloadIntent {
		_ = h
		return intent.WorkloadIntent{TaskID: "wrong", DispatchSeq: 1}
	}
	err := ClaimThenEmit(ctx, claimer, emitter, st, dropTaskID)
	if !errors.Is(err, ErrNoClaimedAttempt) {
		t.Fatalf("mismatched TaskID = %v, want ErrNoClaimedAttempt", err)
	}

	// buildIntent that drops DispatchSeq must also fail.
	st2 := newFakeDispatchStore("spi-delta")
	claimer2 := newFakeClaimer(st2)
	emitter2 := &fakeEmitter{}
	dropSeq := func(h *ClaimHandle) intent.WorkloadIntent {
		return intent.WorkloadIntent{TaskID: h.TaskID, DispatchSeq: h.DispatchSeq + 99}
	}
	err = ClaimThenEmit(ctx, claimer2, emitter2, st2, dropSeq)
	if !errors.Is(err, ErrNoClaimedAttempt) {
		t.Fatalf("mismatched DispatchSeq = %v, want ErrNoClaimedAttempt", err)
	}
}

// TestClaimThenEmit_NilArgsRejected confirms the guard clauses for nil
// dependencies.
func TestClaimThenEmit_NilArgsRejected(t *testing.T) {
	ctx := context.Background()
	st := newFakeDispatchStore("spi-delta")
	claimer := newFakeClaimer(st)
	emitter := &fakeEmitter{}

	cases := []struct {
		name     string
		claimer  AttemptClaimer
		emitter  DispatchEmitter
		selector ReadyWorkSelector
		build    func(*ClaimHandle) intent.WorkloadIntent
	}{
		{"nil claimer", nil, emitter, st, buildIntentStampingClaim},
		{"nil emitter", claimer, nil, st, buildIntentStampingClaim},
		{"nil selector", claimer, emitter, nil, buildIntentStampingClaim},
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

// TestClaimHandle_ZeroValue pins the zero-value shape of ClaimHandle
// so downstream code can rely on `handle == nil` meaning "no claim" and
// handle.TaskID == "" meaning a zero-value misuse rather than a
// successful claim.
func TestClaimHandle_ZeroValue(t *testing.T) {
	var h ClaimHandle
	if h.TaskID != "" {
		t.Errorf("zero ClaimHandle.TaskID = %q, want empty", h.TaskID)
	}
	if h.DispatchSeq != 0 {
		t.Errorf("zero ClaimHandle.DispatchSeq = %d, want 0", h.DispatchSeq)
	}
	if !h.ClaimedAt.IsZero() {
		t.Errorf("zero ClaimHandle.ClaimedAt = %v, want zero time", h.ClaimedAt)
	}
}

// TestValidateHandle_HappyPath pins the positive leg of the guard:
// matching (TaskID, DispatchSeq) returns nil.
func TestValidateHandle_HappyPath(t *testing.T) {
	h := &ClaimHandle{TaskID: "spi-x", DispatchSeq: 1, ClaimedAt: time.Now().UTC()}
	i := intent.WorkloadIntent{TaskID: "spi-x", DispatchSeq: 1}
	if err := ValidateHandle(h, i); err != nil {
		t.Fatalf("ValidateHandle matching = %v, want nil", err)
	}
}

// TestClaimerSurfacePropagatesError confirms that a claimer error
// bubbles up through ClaimThenEmit (wrapped).
func TestClaimerSurfacePropagatesError(t *testing.T) {
	ctx := context.Background()
	sentinel := errors.New("claimer-boom")
	claimer := &errClaimer{err: sentinel}
	emitter := &fakeEmitter{}
	sel := &staticSelector{ids: []string{"spi-foo"}}

	err := ClaimThenEmit(ctx, claimer, emitter, sel, buildIntentStampingClaim)
	if !errors.Is(err, sentinel) {
		t.Fatalf("ClaimThenEmit error = %v, want wrap of sentinel", err)
	}
	if len(emitter.calls) != 0 {
		t.Fatalf("emitter.calls = %d on claimer error, want 0", len(emitter.calls))
	}
}

type errClaimer struct{ err error }

func (e *errClaimer) ClaimNext(_ context.Context, _ ReadyWorkSelector) (*ClaimHandle, error) {
	return nil, e.err
}

type staticSelector struct{ ids []string }

func (s *staticSelector) SelectReady(_ context.Context) ([]string, error) {
	return s.ids, nil
}

// TestClaimThenEmit_EmitErrorSurfaces confirms emitter errors pass
// through unchanged.
func TestClaimThenEmit_EmitErrorSurfaces(t *testing.T) {
	ctx := context.Background()
	st := newFakeDispatchStore("spi-echo")
	claimer := newFakeClaimer(st)
	sentinel := errors.New("publisher-offline")
	emitter := &fakeEmitter{onEmit: sentinel, store: st}

	err := ClaimThenEmit(ctx, claimer, emitter, st, buildIntentStampingClaim)
	if !errors.Is(err, sentinel) {
		t.Fatalf("ClaimThenEmit error = %v, want wrap of sentinel", err)
	}
	// Emit error means the (task_id, dispatch_seq) row never landed in
	// workload_intents; the task remains ready for the next cycle so
	// the steward can retry.
	_ = fmt.Sprint // keep fmt referenced for future test helpers
}

// Sanity-check: the interfaces compile with the fake implementations.
var (
	_ ReadyWorkSelector = (*fakeDispatchStore)(nil)
	_ AttemptClaimer    = (*fakeClaimer)(nil)
	_ DispatchEmitter   = (*fakeEmitter)(nil)
	_ ReadyWorkSelector = (*staticSelector)(nil)
	_ AttemptClaimer    = (*errClaimer)(nil)
)
