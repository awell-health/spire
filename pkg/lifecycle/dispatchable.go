package lifecycle

import (
	"context"
	"fmt"

	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// dispatchableDeps is the unexported dependency surface DispatchableBeads
// uses. It mirrors the serviceDeps pattern in service.go: production
// wiring goes through pkg/store and pkg/formula; tests substitute fakes
// via withDispatchableDeps so the function can be exercised end-to-end
// without a live database or formula tree.
type dispatchableDeps struct {
	ListBeads      func(filter beads.IssueFilter) ([]store.Bead, error)
	GetStepBeads   func(parentID string) ([]store.Bead, error)
	ResolveFormula func(b *store.Bead) (*formula.FormulaStepGraph, error)
}

// defaultDispatchableDeps wires the production implementations. The
// resolver mirrors service.go's defaultServiceDeps.ResolveFormula so the
// dispatchable path and the RecordEvent path agree on which formula a
// bead binds to.
var defaultDispatchableDeps = dispatchableDeps{
	ListBeads:    store.ListBeads,
	GetStepBeads: store.GetStepBeads,
	ResolveFormula: func(b *store.Bead) (*formula.FormulaStepGraph, error) {
		if b == nil {
			return nil, fmt.Errorf("lifecycle: nil bead")
		}
		return formula.ResolveV3(formula.BeadInfo{
			ID:     b.ID,
			Type:   b.Type,
			Labels: b.Labels,
		})
	},
}

// activeDispatchableDeps is the resolver DispatchableBeads consults.
// Tests swap it via withDispatchableDeps; production callers leave it
// untouched.
var activeDispatchableDeps = defaultDispatchableDeps

// withDispatchableDeps temporarily swaps activeDispatchableDeps. The
// returned closure restores the prior value. Defined unexported so only
// in-package tests can call it.
func withDispatchableDeps(d dispatchableDeps) func() {
	prev := activeDispatchableDeps
	activeDispatchableDeps = d
	return func() { activeDispatchableDeps = prev }
}

// DispatchableBeads returns beads currently in a status that some
// active step in the bead's formula declares as on_start. Used by the
// steward to decide which beads to dispatch a wizard for.
//
// Pre-filters by status not in {closed, filed} for efficiency. For each
// surviving bead the function resolves the formula and consults every
// step's [steps.X.lifecycle].on_start declaration: a bead is
// dispatchable when its current status equals an on_start whose owning
// step has not yet run for the bead's current attempt (no closed
// step:<name> child bead exists).
//
// Legacy fallback: when the resolved formula declares no
// [steps.X.lifecycle] blocks at all (external/user formulas that have
// not been ported to the new schema), or when formula resolution fails,
// the function falls through to IsDispatchable — preserving the legacy
// "ready | open | hooked" semantics introduced in Landing 1 so existing
// formulas keep working unchanged.
func DispatchableBeads(ctx context.Context) ([]*store.Bead, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	deps := activeDispatchableDeps

	// Pre-filter: exclude closed and filed beads. Anything else may be
	// dispatchable depending on the formula's declarations or the
	// legacy fallback predicate.
	filter := beads.IssueFilter{
		ExcludeStatus: []beads.Status{
			beads.StatusClosed,
			beads.Status("filed"),
		},
	}
	candidates, err := deps.ListBeads(filter)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: DispatchableBeads list: %w", err)
	}

	var dispatchable []*store.Bead
	for i := range candidates {
		b := &candidates[i]
		if isDispatchableForFormula(deps, b) {
			dispatchable = append(dispatchable, b)
		}
	}
	return dispatchable, nil
}

// isDispatchableForFormula reports whether bead is dispatchable per its
// formula's per-step lifecycle declarations. When the resolved formula
// declares no [steps.X.lifecycle] blocks anywhere, falls back to
// IsDispatchable's legacy semantics. Resolution errors and a nil
// formula also route to the fallback rather than dropping the bead, so
// a malformed or missing formula degrades to the pre-Landing-3
// behavior instead of silently filtering work out of the steward's
// queue.
func isDispatchableForFormula(deps dispatchableDeps, bead *store.Bead) bool {
	f, err := deps.ResolveFormula(bead)
	if err != nil || f == nil {
		return IsDispatchable(bead)
	}

	declared := false
	for stepName, step := range f.Steps {
		if step.Lifecycle == nil {
			continue
		}
		declared = true
		if step.Lifecycle.OnStart == "" {
			continue
		}
		if step.Lifecycle.OnStart != bead.Status {
			continue
		}
		if hasStepRun(deps, bead, stepName) {
			continue
		}
		return true
	}
	if !declared {
		return IsDispatchable(bead)
	}
	return false
}

// hasStepRun reports whether the named step has already executed for
// the bead's current attempt — i.e. a child step bead carrying the
// step:<name> label exists in the closed state. Reset cycles reopen
// step beads from closed back to open (see store.ReopenStepBead), so a
// closed step bead means the step ran and has not been rewound for the
// current attempt.
func hasStepRun(deps dispatchableDeps, bead *store.Bead, stepName string) bool {
	steps, err := deps.GetStepBeads(bead.ID)
	if err != nil {
		return false
	}
	for _, s := range steps {
		if store.StepBeadPhaseName(s) != stepName {
			continue
		}
		if s.Status == "closed" {
			return true
		}
	}
	return false
}
