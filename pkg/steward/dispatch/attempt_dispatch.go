// Package dispatch formalizes claim-then-emit as the only path by which
// cluster-native scheduling may hand work to a reconciler.
//
// The rule the package encodes is simple: nothing downstream of the
// scheduler sees a WorkloadIntent unless a dispatch slot has been
// atomically reserved for (task_id, dispatch_seq). The ClaimHandle
// returned by a successful claim is the proof-of-claim token;
// DispatchEmitter.Emit refuses to run without a matching handle.
//
// The canonical ownership seam is the task — identified by its bead ID.
// Attempt beads are NOT created here; they are the wizard's concern
// once the dispatched workload pod starts. The claim step verifies that
// no active attempt exists for the task (preventing redispatch while a
// wizard is alive) and computes the next monotonically-increasing
// dispatch_seq for this task. Multiple replicas calling ClaimNext
// concurrently on the same task both compute the same dispatch_seq;
// the first one to INSERT into workload_intents wins (PK collision),
// and the loser's Publish returns a duplicate-key error that callers
// log and skip.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/awell-health/spire/pkg/lifecycle"
	"github.com/awell-health/spire/pkg/steward/intent"
	"github.com/awell-health/spire/pkg/store"
)

// ErrNoClaimedAttempt is returned by DispatchEmitter.Emit when the caller
// hands it a nil handle or an intent whose (TaskID, DispatchSeq) does not
// match the handle. It exists so emit sites cannot bypass the claim step:
// an emitter with no proof-of-claim must refuse to publish work.
//
// The name retains "Attempt" for historical continuity with callers that
// errors.Is against it; semantically the guard now covers task-keyed
// dispatch claims.
var ErrNoClaimedAttempt = errors.New("dispatch: emit requires a matching claimed dispatch handle")

// ClaimHandle is the proof-of-claim token a caller receives after an
// AttemptClaimer successfully claims a dispatch slot for a task. It
// carries the task's bead ID, the monotonic dispatch sequence number
// (1 on first dispatch; bumped on retry), and the UTC time the claim
// was recorded. Callers MUST thread (TaskID, DispatchSeq) into the
// WorkloadIntent they emit; DispatchEmitter.Emit cross-checks them.
//
// A nil *ClaimHandle means "nothing claimed" and is the value callers
// get back when no ready work was available.
type ClaimHandle struct {
	TaskID      string
	DispatchSeq int
	Reason      string
	ClaimedAt   time.Time
}

// ReadyWorkSelector presents candidate parent bead IDs to an
// AttemptClaimer. Returning an empty slice with a nil error means
// "nothing is ready right now".
//
// Implementations MUST NOT rank candidates by inferred policy or local
// heuristics; they return candidates in whatever deterministic order the
// underlying store yields. Separating selection from claim keeps the
// "what is ready" query pluggable (test fake, store-backed, filtered)
// without dragging ranking decisions into this package.
type ReadyWorkSelector interface {
	SelectReady(ctx context.Context) ([]string, error)
}

// AttemptClaimer is the claim half of the dispatch seam. ClaimNext walks
// candidates from the selector and reserves a dispatch slot for the
// first one that has no active attempt. Returning (nil, nil) means
// nothing ready was claimable — either the selector yielded no
// candidates, or every candidate already has a wizard working it.
//
// Implementations MUST derive uniqueness from the shared store: the
// (task_id, dispatch_seq) PK on workload_intents is the tiebreaker
// under multi-replica control planes. They MUST NOT use a package-level
// sync.Map, a process mutex, or any other machine-local structure to
// decide who owns a task.
type AttemptClaimer interface {
	ClaimNext(ctx context.Context, selector ReadyWorkSelector) (*ClaimHandle, error)
}

// DispatchEmitter is the emit half of the dispatch seam. Implementations
// MUST call ValidateHandle as the first step of Emit so that:
//
//   - a nil handle returns ErrNoClaimedAttempt, and
//   - an intent whose (TaskID, DispatchSeq) does not match the handle's
//     values returns ErrNoClaimedAttempt.
//
// The contract guarantees that every emitted WorkloadIntent is backed by
// a currently-claimed dispatch slot identified by (TaskID, DispatchSeq).
type DispatchEmitter interface {
	Emit(ctx context.Context, handle *ClaimHandle, intent intent.WorkloadIntent) error
}

// ValidateHandle is the guard every DispatchEmitter.Emit implementation
// MUST call before doing any work. It returns ErrNoClaimedAttempt when
// the handle is nil or when the handle's (TaskID, DispatchSeq) does not
// match the WorkloadIntent's values.
//
// Exposing the check as a helper means the invariant lives in one place
// and every emitter — production transports and test fakes alike —
// exercises the same validation.
func ValidateHandle(handle *ClaimHandle, i intent.WorkloadIntent) error {
	if handle == nil {
		return ErrNoClaimedAttempt
	}
	if i.TaskID != handle.TaskID {
		return ErrNoClaimedAttempt
	}
	if i.DispatchSeq != handle.DispatchSeq {
		return ErrNoClaimedAttempt
	}
	return nil
}

// ClaimThenEmit is the only allowed dispatch path. It claims first; if
// nothing is ready it returns early without emitting; otherwise it builds
// the WorkloadIntent from the claimed handle and emits it. The handle's
// (TaskID, DispatchSeq) threads into the intent via buildIntent so the
// emitter's ValidateHandle check passes.
//
// Callers that want to dispatch work wire a claimer, an emitter, a
// selector, and a buildIntent closure that maps the handle to a
// WorkloadIntent (typically by stamping the handle's identifiers onto a
// pre-built template). The function returns whatever error the claimer
// or emitter raised, or nil when nothing was ready.
func ClaimThenEmit(
	ctx context.Context,
	claimer AttemptClaimer,
	emitter DispatchEmitter,
	selector ReadyWorkSelector,
	buildIntent func(*ClaimHandle) intent.WorkloadIntent,
) error {
	if claimer == nil {
		return errors.New("dispatch: nil claimer")
	}
	if emitter == nil {
		return errors.New("dispatch: nil emitter")
	}
	if selector == nil {
		return errors.New("dispatch: nil selector")
	}
	if buildIntent == nil {
		return errors.New("dispatch: nil buildIntent")
	}
	handle, err := claimer.ClaimNext(ctx, selector)
	if err != nil {
		return fmt.Errorf("dispatch: claim: %w", err)
	}
	if handle == nil {
		return nil
	}
	return emitter.Emit(ctx, handle, buildIntent(handle))
}

// StoreSelector is the default lifecycle-backed ReadyWorkSelector. It
// returns the IDs of beads currently dispatchable per their formula's
// `[steps.X.lifecycle].on_start` declarations (or, for legacy formulas
// without lifecycle blocks, the historical "ready/open/hooked" predicate
// preserved by lifecycle.IsDispatchable). Beads with an active attempt
// child or invariant violation are filtered out at the per-bead policy
// layer below so the cluster claim path sees only valid targets.
//
// The returned order is whatever order lifecycle.DispatchableBeads
// yields; StoreSelector imposes no additional ranking.
type StoreSelector struct{}

// SelectReady delegates to lifecycle.DispatchableBeads (spi-jzs5xq) and
// returns the dispatchable bead IDs. The selector applies the same
// per-bead policy filters the steward's TowerCycle dispatch path uses:
// internal beads are dropped (via store.IsWorkBead), template beads are
// skipped, and beads with an active attempt — or an invariant-violation
// error from GetActiveAttempt — are excluded so the claim step never
// races a live wizard.
func (s StoreSelector) SelectReady(ctx context.Context) ([]string, error) {
	_ = s
	dispatchable, err := lifecycle.DispatchableBeads(ctx)
	if err != nil {
		return nil, fmt.Errorf("dispatch: select ready: %w", err)
	}
	ids := make([]string, 0, len(dispatchable))
	for _, b := range dispatchable {
		if b == nil {
			continue
		}
		if !store.IsWorkBead(*b) {
			continue
		}
		if store.ContainsLabel(*b, "template") {
			continue
		}
		attempt, aErr := store.GetActiveAttempt(b.ID)
		if aErr != nil {
			// Quarantine path: a bead whose attempt graph is in an
			// invariant-violation state is not a valid claim target.
			// The steward's TowerCycle path raises an alert; here the
			// safe action is to drop the candidate so the loser
			// doesn't race the wizard owning the broken attempt.
			continue
		}
		if attempt != nil {
			continue
		}
		ids = append(ids, b.ID)
	}
	return ids, nil
}

// StoreClaimer is the default pkg/store-backed AttemptClaimer. It
// reserves a dispatch slot for a task by verifying no active attempt
// exists and computing the next dispatch_seq. Crucially, it does NOT
// pre-create an attempt bead — that responsibility belongs solely to
// the wizard once the dispatched workload pod starts.
//
// Uniqueness comes from two complementary mechanisms:
//  1. GetActiveAttempt blocks redispatch while a wizard is alive (the
//     wizard created an attempt; redispatch of the same task would
//     race the wizard and must be refused until that attempt closes).
//  2. The workload_intents (task_id, dispatch_seq) PK enforces
//     single-winner semantics when two replicas concurrently publish
//     with the same sequence. The losing replica's Publish returns a
//     duplicate-key error that the caller logs and skips.
//
// The zero value is not usable — AgentName must be set so dispatch log
// lines carry ownership metadata. DispatchSeqFn is an optional hook for
// tests; production leaves it nil and ClaimNext derives the next
// sequence from the store.
type StoreClaimer struct {
	// AgentName identifies the steward replica recording the claim.
	// Required.
	AgentName string
	// Reason is an optional annotation carried onto the emitted
	// WorkloadIntent (e.g. "retry-after-pod-death"). Empty on fresh
	// dispatch.
	Reason string
	// DispatchSeqFn is a test hook that returns the next dispatch_seq
	// for a task. Production leaves it nil and ClaimNext calls
	// store.NextDispatchSeq.
	DispatchSeqFn func(ctx context.Context, taskID string) (int, error)
	// now is an override hook for tests; production leaves it nil and
	// ClaimNext stamps time.Now().UTC().
	now func() time.Time
}

// ClaimNext walks the candidate list returned by selector and reserves
// a dispatch slot for the first task that has no active attempt. The
// sequence for each candidate is:
//
//  1. store.GetActiveAttempt — if any active attempt exists, skip the
//     task. A live wizard already owns it; redispatch would race the
//     wizard. When the wizard finishes and the attempt closes the task
//     remains in_progress only until the close cascade flips it; once
//     the task is no longer ready, the selector stops offering it.
//  2. Derive the next dispatch_seq via store.NextDispatchSeq. The seq
//     is a monotonic counter scoped to the task; fresh tasks get 1,
//     retries get +1.
//  3. Return a ClaimHandle carrying (TaskID, DispatchSeq, Reason,
//     ClaimedAt). The caller is responsible for INSERTing the
//     workload_intents row under the composite PK — PK collision is
//     how uniqueness survives multi-replica races.
//
// Returns (nil, nil) once the candidate list is exhausted without a
// successful claim. Callers treat that as "nothing ready".
func (c *StoreClaimer) ClaimNext(ctx context.Context, selector ReadyWorkSelector) (*ClaimHandle, error) {
	if c == nil {
		return nil, errors.New("dispatch: nil StoreClaimer")
	}
	if c.AgentName == "" {
		return nil, errors.New("dispatch: StoreClaimer.AgentName is required")
	}
	if selector == nil {
		return nil, errors.New("dispatch: nil selector")
	}
	ids, err := selector.SelectReady(ctx)
	if err != nil {
		return nil, err
	}
	for _, taskID := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		existing, gerr := store.GetActiveAttempt(taskID)
		if gerr != nil {
			// Invariant violation or I/O error — skip; the steward's
			// quarantine path owns recovery. This is not an error we
			// propagate, because one bad candidate must not stop the
			// rest of the dispatch cycle.
			continue
		}
		if existing != nil {
			continue
		}
		seq, serr := c.nextDispatchSeq(ctx, taskID)
		if serr != nil {
			// I/O error deriving seq — skip this candidate.
			continue
		}
		return &ClaimHandle{
			TaskID:      taskID,
			DispatchSeq: seq,
			Reason:      c.Reason,
			ClaimedAt:   c.nowFn(),
		}, nil
	}
	return nil, nil
}

// nextDispatchSeq returns the next dispatch sequence for taskID,
// delegating to DispatchSeqFn when set (tests) or
// store.NextDispatchSeq in production.
func (c *StoreClaimer) nextDispatchSeq(ctx context.Context, taskID string) (int, error) {
	if c.DispatchSeqFn != nil {
		return c.DispatchSeqFn(ctx, taskID)
	}
	return store.NextDispatchSeq(taskID)
}

func (c *StoreClaimer) nowFn() time.Time {
	if c == nil || c.now == nil {
		return time.Now().UTC()
	}
	return c.now().UTC()
}
