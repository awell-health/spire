package lifecycle

import (
	"context"
	"errors"
	"testing"

	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// fakeDispatchableEnv backs the test deps with in-memory tables so each
// test case can assemble a tiny bead/step graph and a synthetic formula
// without touching dolt or pkg/store globals.
type fakeDispatchableEnv struct {
	beads      []store.Bead
	stepsByID  map[string][]store.Bead
	formulaFor map[string]*formula.FormulaStepGraph
	resolveErr error
	listErr    error
	listFilter beads.IssueFilter
	listCalls  int
}

func (e *fakeDispatchableEnv) deps() dispatchableDeps {
	return dispatchableDeps{
		ListBeads: func(filter beads.IssueFilter) ([]store.Bead, error) {
			e.listCalls++
			e.listFilter = filter
			if e.listErr != nil {
				return nil, e.listErr
			}
			out := make([]store.Bead, 0, len(e.beads))
			for _, b := range e.beads {
				if statusExcluded(b.Status, filter.ExcludeStatus) {
					continue
				}
				out = append(out, b)
			}
			return out, nil
		},
		GetStepBeads: func(parentID string) ([]store.Bead, error) {
			return e.stepsByID[parentID], nil
		},
		ResolveFormula: func(b *store.Bead) (*formula.FormulaStepGraph, error) {
			if e.resolveErr != nil {
				return nil, e.resolveErr
			}
			if b == nil {
				return nil, nil
			}
			return e.formulaFor[b.ID], nil
		},
	}
}

func statusExcluded(status string, excluded []beads.Status) bool {
	for _, s := range excluded {
		if string(s) == status {
			return true
		}
	}
	return false
}

// TestDispatchableBeads_EmptyStore is the simplest invariant: an empty
// store yields no dispatchable beads and no error.
func TestDispatchableBeads_EmptyStore(t *testing.T) {
	env := &fakeDispatchableEnv{}
	restore := withDispatchableDeps(env.deps())
	defer restore()

	got, err := DispatchableBeads(context.Background())
	if err != nil {
		t.Fatalf("DispatchableBeads err = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
	if env.listCalls != 1 {
		t.Errorf("listCalls = %d, want 1", env.listCalls)
	}
}

// TestDispatchableBeads_PreFilterExcludesClosedAndFiled verifies the
// pre-filter is wired through to the store via ExcludeStatus rather
// than re-implemented in Go-side iteration. The store-level filter is
// load-bearing for efficiency: a tower with thousands of closed beads
// must not pay the full scan cost on every steward tick.
func TestDispatchableBeads_PreFilterExcludesClosedAndFiled(t *testing.T) {
	env := &fakeDispatchableEnv{}
	restore := withDispatchableDeps(env.deps())
	defer restore()

	if _, err := DispatchableBeads(context.Background()); err != nil {
		t.Fatalf("DispatchableBeads err = %v", err)
	}
	want := map[string]bool{"closed": true, "filed": true}
	got := map[string]bool{}
	for _, s := range env.listFilter.ExcludeStatus {
		got[string(s)] = true
	}
	for k := range want {
		if !got[k] {
			t.Errorf("ExcludeStatus missing %q (got %v)", k, got)
		}
	}
}

// TestDispatchableBeads_FormulaWithDeclarations covers the primary
// path: formulas declaring [steps.X.lifecycle].on_start drive
// dispatchability per-bead. The bead in `ready` matches the implement
// step's on_start; the bead in `awaiting_review` matches the merge
// step's on_start; the bead in `in_progress` does not match any
// declared on_start and therefore is not dispatchable under the
// formula-declared path.
func TestDispatchableBeads_FormulaWithDeclarations(t *testing.T) {
	f := &formula.FormulaStepGraph{
		Steps: map[string]formula.StepConfig{
			"implement": {Lifecycle: &formula.LifecycleConfig{OnStart: "ready"}},
			"merge":     {Lifecycle: &formula.LifecycleConfig{OnStart: "awaiting_review"}},
		},
	}
	beadReady := store.Bead{ID: "spi-ready", Status: "ready", Type: "task"}
	beadAwaiting := store.Bead{ID: "spi-awaiting", Status: "awaiting_review", Type: "task"}
	beadInProgress := store.Bead{ID: "spi-in-progress", Status: "in_progress", Type: "task"}
	env := &fakeDispatchableEnv{
		beads: []store.Bead{beadReady, beadAwaiting, beadInProgress},
		formulaFor: map[string]*formula.FormulaStepGraph{
			"spi-ready":       f,
			"spi-awaiting":    f,
			"spi-in-progress": f,
		},
	}
	restore := withDispatchableDeps(env.deps())
	defer restore()

	got, err := DispatchableBeads(context.Background())
	if err != nil {
		t.Fatalf("DispatchableBeads err = %v", err)
	}
	gotIDs := beadIDs(got)
	want := map[string]bool{"spi-ready": true, "spi-awaiting": true}
	if len(gotIDs) != len(want) {
		t.Fatalf("len = %d (%v), want 2 (%v)", len(gotIDs), gotIDs, want)
	}
	for _, id := range gotIDs {
		if !want[id] {
			t.Errorf("unexpected bead %q in result", id)
		}
	}
}

// TestDispatchableBeads_FormulaWithoutDeclarations exercises the legacy
// fallback. A formula whose Steps carry no Lifecycle blocks leaves the
// dispatch decision to IsDispatchable — so a bead in `ready` or `open`
// is dispatchable, and a bead in `in_progress` is not. This is the
// load-bearing back-compat contract for external/user formulas that
// have not been ported to the new schema.
func TestDispatchableBeads_FormulaWithoutDeclarations(t *testing.T) {
	legacy := &formula.FormulaStepGraph{
		Steps: map[string]formula.StepConfig{
			"implement": {Lifecycle: nil},
			"review":    {Lifecycle: nil},
		},
	}

	cases := []struct {
		status string
		want   bool
	}{
		{"ready", true},
		{"open", true},
		{"in_progress", false},
		{"dispatched", false},
		{"awaiting_review", false},
		{"needs_changes", false},
		{"awaiting_human", false},
		{"merge_pending", false},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			b := store.Bead{ID: "spi-" + tc.status, Status: tc.status, Type: "task"}
			env := &fakeDispatchableEnv{
				beads:      []store.Bead{b},
				formulaFor: map[string]*formula.FormulaStepGraph{b.ID: legacy},
			}
			restore := withDispatchableDeps(env.deps())
			defer restore()

			got, err := DispatchableBeads(context.Background())
			if err != nil {
				t.Fatalf("DispatchableBeads err = %v", err)
			}
			present := len(got) > 0
			if present != tc.want {
				t.Errorf("dispatchable for status=%q = %v, want %v", tc.status, present, tc.want)
			}
		})
	}
}

// TestDispatchableBeads_FormulaResolutionErrorFallsBack ensures a
// resolver failure still routes the bead through IsDispatchable rather
// than dropping it entirely. Dropping would silently shrink the
// steward's queue when a formula goes missing — a worse failure mode
// than over-dispatch.
func TestDispatchableBeads_FormulaResolutionErrorFallsBack(t *testing.T) {
	b := store.Bead{ID: "spi-ready", Status: "ready", Type: "task"}
	env := &fakeDispatchableEnv{
		beads:      []store.Bead{b},
		resolveErr: errors.New("formula missing"),
	}
	restore := withDispatchableDeps(env.deps())
	defer restore()

	got, err := DispatchableBeads(context.Background())
	if err != nil {
		t.Fatalf("DispatchableBeads err = %v", err)
	}
	if len(got) != 1 || got[0].ID != "spi-ready" {
		t.Errorf("got = %v, want [spi-ready] via legacy fallback", beadIDs(got))
	}
}

// TestDispatchableBeads_StepAlreadyRun pins the "hasn't yet run"
// qualifier: a closed step:<name> child bead means the step has
// already executed for the current attempt, so the parent bead is
// NOT dispatchable even though its status matches the step's on_start.
func TestDispatchableBeads_StepAlreadyRun(t *testing.T) {
	f := &formula.FormulaStepGraph{
		Steps: map[string]formula.StepConfig{
			"implement": {Lifecycle: &formula.LifecycleConfig{OnStart: "ready"}},
		},
	}
	parent := store.Bead{ID: "spi-parent", Status: "ready", Type: "task"}
	closedStep := store.Bead{
		ID:     "spi-step",
		Status: "closed",
		Type:   "step",
		Parent: "spi-parent",
		Labels: []string{"workflow-step", "step:implement"},
	}
	env := &fakeDispatchableEnv{
		beads:     []store.Bead{parent},
		stepsByID: map[string][]store.Bead{"spi-parent": {closedStep}},
		formulaFor: map[string]*formula.FormulaStepGraph{
			"spi-parent": f,
		},
	}
	restore := withDispatchableDeps(env.deps())
	defer restore()

	got, err := DispatchableBeads(context.Background())
	if err != nil {
		t.Fatalf("DispatchableBeads err = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got = %v, want empty (step already run)", beadIDs(got))
	}
}

// TestDispatchableBeads_StepReopenedAfterReset covers the reset
// pathway: when ReopenStepBead transitions a previously-closed step
// bead back to "open", the step is no longer "run" and the parent
// becomes dispatchable again. This is the scenario that lets a reset
// rewind execution without manually clearing step beads.
func TestDispatchableBeads_StepReopenedAfterReset(t *testing.T) {
	f := &formula.FormulaStepGraph{
		Steps: map[string]formula.StepConfig{
			"implement": {Lifecycle: &formula.LifecycleConfig{OnStart: "ready"}},
		},
	}
	parent := store.Bead{ID: "spi-parent", Status: "ready", Type: "task"}
	openStep := store.Bead{
		ID:     "spi-step",
		Status: "open",
		Type:   "step",
		Parent: "spi-parent",
		Labels: []string{"workflow-step", "step:implement"},
	}
	env := &fakeDispatchableEnv{
		beads:     []store.Bead{parent},
		stepsByID: map[string][]store.Bead{"spi-parent": {openStep}},
		formulaFor: map[string]*formula.FormulaStepGraph{
			"spi-parent": f,
		},
	}
	restore := withDispatchableDeps(env.deps())
	defer restore()

	got, err := DispatchableBeads(context.Background())
	if err != nil {
		t.Fatalf("DispatchableBeads err = %v", err)
	}
	if len(got) != 1 || got[0].ID != "spi-parent" {
		t.Errorf("got = %v, want [spi-parent]", beadIDs(got))
	}
}

// TestDispatchableBeads_MultipleStepsOneMatchingUnrun verifies the
// per-step iteration: when only one of several declared steps has a
// matching on_start AND has not yet run, that match alone is enough
// to make the parent dispatchable. Steps with mismatched on_start, or
// matching on_start but already-run, must not block the dispatch.
func TestDispatchableBeads_MultipleStepsOneMatchingUnrun(t *testing.T) {
	f := &formula.FormulaStepGraph{
		Steps: map[string]formula.StepConfig{
			"plan":      {Lifecycle: &formula.LifecycleConfig{OnStart: "ready"}},  // already run
			"implement": {Lifecycle: &formula.LifecycleConfig{OnStart: "ready"}},  // not run
			"review":    {Lifecycle: &formula.LifecycleConfig{OnStart: "merge_pending"}}, // mismatched status
		},
	}
	parent := store.Bead{ID: "spi-parent", Status: "ready", Type: "task"}
	closedPlan := store.Bead{
		ID:     "spi-step-plan",
		Status: "closed",
		Type:   "step",
		Parent: "spi-parent",
		Labels: []string{"workflow-step", "step:plan"},
	}
	env := &fakeDispatchableEnv{
		beads:     []store.Bead{parent},
		stepsByID: map[string][]store.Bead{"spi-parent": {closedPlan}},
		formulaFor: map[string]*formula.FormulaStepGraph{
			"spi-parent": f,
		},
	}
	restore := withDispatchableDeps(env.deps())
	defer restore()

	got, err := DispatchableBeads(context.Background())
	if err != nil {
		t.Fatalf("DispatchableBeads err = %v", err)
	}
	if len(got) != 1 || got[0].ID != "spi-parent" {
		t.Errorf("got = %v, want [spi-parent] via implement step", beadIDs(got))
	}
}

// TestDispatchableBeads_NonDispatchableStatusesUnderFormula ensures a
// bead whose formula declares lifecycle blocks but whose current
// status matches no on_start is correctly excluded — even from
// statuses the legacy predicate would have returned true for. The
// formula's declarations are authoritative once present.
func TestDispatchableBeads_NonDispatchableStatusesUnderFormula(t *testing.T) {
	f := &formula.FormulaStepGraph{
		Steps: map[string]formula.StepConfig{
			"implement": {Lifecycle: &formula.LifecycleConfig{OnStart: "needs_changes"}},
		},
	}
	// `ready` would be dispatchable under the legacy fallback, but the
	// formula's declarations take over once any step declares lifecycle
	// blocks — and no step declares on_start=ready.
	parent := store.Bead{ID: "spi-parent", Status: "ready", Type: "task"}
	env := &fakeDispatchableEnv{
		beads:      []store.Bead{parent},
		formulaFor: map[string]*formula.FormulaStepGraph{"spi-parent": f},
	}
	restore := withDispatchableDeps(env.deps())
	defer restore()

	got, err := DispatchableBeads(context.Background())
	if err != nil {
		t.Fatalf("DispatchableBeads err = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got = %v, want empty (formula-declared on_start does not match)", beadIDs(got))
	}
}

// TestDispatchableBeads_ListErrorPropagates ensures store errors are
// surfaced rather than swallowed — the steward needs to know when its
// dispatch source is failing.
func TestDispatchableBeads_ListErrorPropagates(t *testing.T) {
	env := &fakeDispatchableEnv{listErr: errors.New("dolt down")}
	restore := withDispatchableDeps(env.deps())
	defer restore()

	_, err := DispatchableBeads(context.Background())
	if err == nil {
		t.Fatal("expected list error to propagate")
	}
}

// TestDispatchableBeads_ContextCancelled ensures a cancelled context
// short-circuits before doing any store work. Useful when the steward
// is shutting down mid-cycle.
func TestDispatchableBeads_ContextCancelled(t *testing.T) {
	env := &fakeDispatchableEnv{}
	restore := withDispatchableDeps(env.deps())
	defer restore()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := DispatchableBeads(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if env.listCalls != 0 {
		t.Errorf("listCalls = %d, want 0 (context check should short-circuit)", env.listCalls)
	}
}

// beadIDs extracts IDs from a result slice for stable comparisons.
func beadIDs(beads []*store.Bead) []string {
	out := make([]string, len(beads))
	for i, b := range beads {
		out[i] = b.ID
	}
	return out
}
