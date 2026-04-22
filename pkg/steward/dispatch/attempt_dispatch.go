// Package dispatch formalizes claim-then-emit as the only path by which
// cluster-native scheduling may hand work to a reconciler.
//
// The rule the package encodes is simple: nothing downstream of the
// scheduler sees a WorkloadIntent unless an attempt bead has already been
// atomically claimed. The AttemptHandle returned by a successful claim is
// the proof-of-claim token; DispatchEmitter.Emit refuses to run without a
// matching handle.
//
// The canonical ownership seam is the attempt bead — a row in the shared
// store whose atomic creation (via pkg/store) is what prevents two agents
// from dispatching the same work. Uniqueness is not enforced by any
// in-memory map, mutex, or sync.Map in this package; doing so would add a
// second, machine-local source of truth that is silently incorrect under
// multi-replica control planes.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/awell-health/spire/pkg/steward/intent"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// ErrNoClaimedAttempt is returned by DispatchEmitter.Emit when the caller
// hands it a nil handle or an intent whose AttemptID does not match the
// handle's AttemptID. It exists so emit sites cannot bypass the claim
// step: an emitter with no proof-of-claim must refuse to publish work.
var ErrNoClaimedAttempt = errors.New("dispatch: emit requires a matching claimed attempt handle")

// AttemptHandle is the proof-of-claim token a caller receives after an
// AttemptClaimer successfully claims an attempt bead. It carries the
// attempt bead's ID — the canonical ownership identifier — and the
// monotonic UTC time the claim was recorded. Callers MUST thread
// AttemptID into the WorkloadIntent they emit; DispatchEmitter.Emit
// cross-checks the two.
//
// A nil *AttemptHandle means "nothing claimed" and is the value callers
// get back when no ready work was available.
type AttemptHandle struct {
	AttemptID string
	ClaimedAt time.Time
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
// candidates from the selector and atomically claims the first one that
// is still free. Returning (nil, nil) means nothing ready was claimable —
// either the selector yielded no candidates, or every candidate was won
// by another agent between selection and the atomic claim.
//
// Implementations MUST use a shared-store atomic claim (pkg/store) as the
// uniqueness mechanism. They MUST NOT use a package-level sync.Map, a
// process mutex, or any other machine-local structure to decide who owns
// a bead.
type AttemptClaimer interface {
	ClaimNext(ctx context.Context, selector ReadyWorkSelector) (*AttemptHandle, error)
}

// DispatchEmitter is the emit half of the dispatch seam. Implementations
// MUST call ValidateHandle as the first step of Emit so that:
//
//   - a nil handle returns ErrNoClaimedAttempt, and
//   - an intent whose AttemptID does not match the handle's AttemptID
//     returns ErrNoClaimedAttempt.
//
// The contract guarantees that every emitted WorkloadIntent is backed by
// a currently-claimed attempt bead with the same AttemptID.
type DispatchEmitter interface {
	Emit(ctx context.Context, handle *AttemptHandle, intent intent.WorkloadIntent) error
}

// ValidateHandle is the guard every DispatchEmitter.Emit implementation
// MUST call before doing any work. It returns ErrNoClaimedAttempt when
// the handle is nil or when the handle's AttemptID does not match the
// WorkloadIntent's AttemptID.
//
// Exposing the check as a helper means the invariant lives in one place
// and every emitter — production transports and test fakes alike —
// exercises the same validation.
func ValidateHandle(handle *AttemptHandle, i intent.WorkloadIntent) error {
	if handle == nil {
		return ErrNoClaimedAttempt
	}
	if i.AttemptID != handle.AttemptID {
		return ErrNoClaimedAttempt
	}
	return nil
}

// ClaimThenEmit is the only allowed dispatch path. It claims first; if
// nothing is ready it returns early without emitting; otherwise it builds
// the WorkloadIntent from the claimed handle and emits it. The handle's
// AttemptID threads into the intent via buildIntent so the emitter's
// ValidateHandle check passes.
//
// Callers that want to dispatch work wire a claimer, an emitter, a
// selector, and a buildIntent closure that maps the handle to a
// WorkloadIntent (typically by stamping handle.AttemptID onto a
// pre-built template). The function returns whatever error the claimer
// or emitter raised, or nil when nothing was ready.
func ClaimThenEmit(
	ctx context.Context,
	claimer AttemptClaimer,
	emitter DispatchEmitter,
	selector ReadyWorkSelector,
	buildIntent func(*AttemptHandle) intent.WorkloadIntent,
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

// StoreSelector is the default pkg/store-backed ReadyWorkSelector. It
// returns the IDs of beads currently schedulable under the shared
// repo-registration store — i.e. beads without an open attempt child, no
// active deferral, and no other structural block.
//
// The returned order is whatever order pkg/store.GetSchedulableWork
// yields; StoreSelector imposes no additional ranking.
type StoreSelector struct{}

// SelectReady delegates to store.GetSchedulableWork and returns the
// schedulable bead IDs. Quarantined beads are dropped at this layer — a
// bead whose attempt graph is in an invariant-violation state is not a
// valid claim target, and the steward's quarantine path handles it
// elsewhere.
func (StoreSelector) SelectReady(_ context.Context) ([]string, error) {
	result, err := store.GetSchedulableWork(beads.WorkFilter{})
	if err != nil {
		return nil, fmt.Errorf("dispatch: select ready: %w", err)
	}
	if result == nil {
		return nil, nil
	}
	ids := make([]string, 0, len(result.Schedulable))
	for _, b := range result.Schedulable {
		ids = append(ids, b.ID)
	}
	return ids, nil
}

// StoreClaimer is the default pkg/store-backed AttemptClaimer. It
// atomically selects + claims via store.CreateAttemptBeadAtomic. Neither
// this struct nor the package holds any busy map, mutex, or sync.Map;
// uniqueness comes from the shared store's atomic attempt-bead creation.
//
// The zero value is not usable — AgentName must be set so the attempt
// bead carries ownership metadata. Model and Branch are optional; the
// executor may update them later.
type StoreClaimer struct {
	// AgentName identifies the agent recording the claim. Required.
	AgentName string
	// Model is the optional model label written on the attempt bead at
	// claim time. Callers that do not know the model yet leave this
	// empty and let the executor fill it in.
	Model string
	// Branch is the optional branch label written on the attempt bead
	// at claim time.
	Branch string
	// now is an override hook for tests; production leaves it nil and
	// ClaimNext stamps time.Now().UTC().
	now func() time.Time
}

// ClaimNext walks the candidate list returned by selector and attempts
// to claim the first candidate that is still free. The sequence for
// each candidate is:
//
//  1. store.GetActiveAttempt — if any active attempt exists, skip (the
//     bead is either already claimed or in an invariant state and the
//     selector should eventually stop offering it).
//  2. store.CreateAttemptBead — atomically create the attempt bead and
//     stamp it in_progress. The beads library enforces row-level
//     uniqueness at the underlying store layer, so two replicas calling
//     this concurrently on the same parent will see exactly one
//     attempt bead survive.
//
// Returns (nil, nil) once the candidate list is exhausted without a
// successful claim. Callers treat that as "nothing ready".
func (c *StoreClaimer) ClaimNext(ctx context.Context, selector ReadyWorkSelector) (*AttemptHandle, error) {
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
	for _, parentID := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		existing, gerr := store.GetActiveAttempt(parentID)
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
		attemptID, cerr := store.CreateAttemptBead(parentID, c.AgentName, c.Model, c.Branch)
		if cerr != nil {
			// Race with another agent between GetActiveAttempt and
			// CreateAttemptBead — the shared store is the tiebreaker,
			// and the loser moves on.
			continue
		}
		return &AttemptHandle{
			AttemptID: attemptID,
			ClaimedAt: c.nowFn(),
		}, nil
	}
	return nil, nil
}

func (c *StoreClaimer) nowFn() time.Time {
	if c == nil || c.now == nil {
		return time.Now().UTC()
	}
	return c.now().UTC()
}
