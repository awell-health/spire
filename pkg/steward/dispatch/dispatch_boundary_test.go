package dispatch

// Boundary tests for the ClaimThenEmit seam.
//
// These tests pin two invariants the steward's cluster-native dispatch
// rests on, independent of the rest of the contract exercised in
// dispatch_test.go:
//
//   1. Idempotency under repeated cycles. Given a single ready task,
//      N repeated ClaimThenEmit cycles emit exactly one WorkloadIntent.
//      Running the loop ten times is deliberate — it is enough to rule
//      out "every Nth cycle" behavior (a counter-gated suppressor) and
//      to catch a naive "emit on every cycle" regression that would
//      produce ten emits.
//
//   2. Emit-side proof-of-claim. A DispatchEmitter.Emit call whose
//      WorkloadIntent (TaskID, DispatchSeq) does not match the handle's
//      values must return ErrNoClaimedAttempt. This is the final guard
//      against a caller reusing a handle from one claim and building an
//      intent for a different slot.
//
// Both properties are part of the cluster-native seam contract and are
// load-bearing for multi-replica safety; the workload_intents
// (task_id, dispatch_seq) PK is the only ownership mechanism. If either
// property regresses, the scheduler could emit duplicate intents or
// leak unclaimed intents past the guard.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/steward/intent"
)

// boundaryDispatchStore is a single-ready-task fake backed by a flag
// recording whether the task has been dispatched. It deliberately does
// NOT share state with dispatch_test.go's fakeDispatchStore — this test
// owns its own minimal fixture to make the boundary explicit.
//
// Semantics match production: once a task is dispatched, subsequent
// SelectReady calls drop it until the wizard's attempt closes.
type boundaryDispatchStore struct {
	taskID     string
	dispatched bool
}

func newBoundaryDispatchStore(taskID string) *boundaryDispatchStore {
	return &boundaryDispatchStore{taskID: taskID}
}

// SelectReady returns the single task ID only while no dispatch has
// occurred, matching store.GetSchedulableWork's filter behavior.
func (s *boundaryDispatchStore) SelectReady(_ context.Context) ([]string, error) {
	if s.dispatched {
		return nil, nil
	}
	return []string{s.taskID}, nil
}

// markDispatched is the atomic-claim stand-in. Returns the dispatch_seq
// and true on the first call, (0, false) thereafter.
func (s *boundaryDispatchStore) markDispatched() (int, bool) {
	if s.dispatched {
		return 0, false
	}
	s.dispatched = true
	return 1, true
}

// boundaryClaimer is the minimal AttemptClaimer wired on top of
// boundaryDispatchStore. It defers uniqueness to the store — no mutex,
// no busy map — mirroring the production StoreClaimer's contract.
type boundaryClaimer struct {
	store *boundaryDispatchStore
}

func (c *boundaryClaimer) ClaimNext(ctx context.Context, selector ReadyWorkSelector) (*ClaimHandle, error) {
	ids, err := selector.SelectReady(ctx)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	seq, ok := c.store.markDispatched()
	if !ok {
		return nil, nil
	}
	return &ClaimHandle{TaskID: ids[0], DispatchSeq: seq, ClaimedAt: time.Now().UTC()}, nil
}

// boundaryEmitter records every Emit call that passes ValidateHandle.
// The emitter deliberately calls ValidateHandle as its first step —
// every production DispatchEmitter must do the same — so the
// proof-of-claim contract is exercised uniformly.
type boundaryEmitter struct {
	calls []intent.WorkloadIntent
}

func (e *boundaryEmitter) Emit(_ context.Context, h *ClaimHandle, i intent.WorkloadIntent) error {
	if err := ValidateHandle(h, i); err != nil {
		return err
	}
	e.calls = append(e.calls, i)
	return nil
}

// buildBoundaryIntent stamps the handle's (TaskID, DispatchSeq) onto a
// pre-built intent template, mirroring what cluster_dispatch's
// buildClusterIntent does in production.
func buildBoundaryIntent(h *ClaimHandle) intent.WorkloadIntent {
	return intent.WorkloadIntent{
		TaskID:       h.TaskID,
		DispatchSeq:  h.DispatchSeq,
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
// task produce exactly one Emit invocation total. The first cycle
// claims + emits; cycles 2..10 see SelectReady return empty (because
// the dispatch marker is set) and short-circuit without emitting.
func TestBoundary_ClaimThenEmit_TenCyclesEmitExactlyOnce(t *testing.T) {
	ctx := context.Background()
	st := newBoundaryDispatchStore("spi-boundary")
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
	if !st.dispatched {
		t.Fatalf("after %d cycles: expected boundary store dispatched flag set", N)
	}
	if emitter.calls[0].TaskID != st.taskID {
		t.Fatalf("emitted TaskID = %q, want %q (the single claimed task)",
			emitter.calls[0].TaskID, st.taskID)
	}
	if emitter.calls[0].DispatchSeq != 1 {
		t.Fatalf("emitted DispatchSeq = %d, want 1", emitter.calls[0].DispatchSeq)
	}
}

// TestBoundary_Emit_MismatchedTaskID pins the proof-of-claim guard:
// an emitter that receives a handle whose TaskID does not match the
// intent's TaskID must return ErrNoClaimedAttempt.
func TestBoundary_Emit_MismatchedTaskID(t *testing.T) {
	emitter := &boundaryEmitter{}
	handle := &ClaimHandle{
		TaskID:      "spi-alpha",
		DispatchSeq: 1,
		ClaimedAt:   time.Now().UTC(),
	}
	mismatched := intent.WorkloadIntent{
		TaskID:       "spi-beta", // not the handle's TaskID
		DispatchSeq:  1,
		FormulaPhase: "implement",
	}

	err := emitter.Emit(context.Background(), handle, mismatched)
	if !errors.Is(err, ErrNoClaimedAttempt) {
		t.Fatalf("Emit(mismatched TaskID) = %v, want ErrNoClaimedAttempt", err)
	}
	if len(emitter.calls) != 0 {
		t.Fatalf("emitter recorded %d calls on mismatch rejection, want 0", len(emitter.calls))
	}
}

// TestBoundary_Emit_NilHandle adds the companion guard test for the nil
// handle path — Emit called with (nil handle, anything) must return
// ErrNoClaimedAttempt.
func TestBoundary_Emit_NilHandle(t *testing.T) {
	emitter := &boundaryEmitter{}

	err := emitter.Emit(context.Background(), nil, intent.WorkloadIntent{TaskID: "spi-whatever", DispatchSeq: 1})
	if !errors.Is(err, ErrNoClaimedAttempt) {
		t.Fatalf("Emit(nil handle) = %v, want ErrNoClaimedAttempt", err)
	}
	if len(emitter.calls) != 0 {
		t.Fatalf("emitter recorded %d calls on nil-handle rejection, want 0", len(emitter.calls))
	}
}

// TestBoundary_ClaimThenEmit_TenCyclesIdentityStableAcrossSkips pins a
// subtler property that sits adjacent to the main idempotency
// invariant: the emit's (TaskID, DispatchSeq) must equal what the
// claimer actually produced. A regression that claimed one slot and
// emitted under a different one would fail ValidateHandle, but a
// regression that silently chose synthetic identifiers would pass emit
// yet detach the emitted intent from its claim row.
func TestBoundary_ClaimThenEmit_TenCyclesIdentityStableAcrossSkips(t *testing.T) {
	ctx := context.Background()
	st := newBoundaryDispatchStore("spi-stable")
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
	if emitter.calls[0].TaskID != st.taskID {
		t.Fatalf("emit TaskID = %q, want %q", emitter.calls[0].TaskID, st.taskID)
	}
	if emitter.calls[0].DispatchSeq != 1 {
		t.Fatalf("emit DispatchSeq = %d, want 1 (only one dispatch across 10 cycles)", emitter.calls[0].DispatchSeq)
	}
}

// Compile-time conformance for the fakes.
var (
	_ ReadyWorkSelector = (*boundaryDispatchStore)(nil)
	_ AttemptClaimer    = (*boundaryClaimer)(nil)
	_ DispatchEmitter   = (*boundaryEmitter)(nil)
)
