package recovery

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/runtime"
	"github.com/awell-health/spire/pkg/store"
)

// withPromotionStubs swaps the package-level store seams for stubs and
// returns a restore func. Tests MUST call the restore func in defer.
func withPromotionStubs(
	t *testing.T,
	snap func(string) (*store.PromotionSnapshot, error),
	demote func(string) error,
) func() {
	t.Helper()
	origSnap := getPromotionSnapshot
	origDemote := demotePromotedRows
	if snap != nil {
		getPromotionSnapshot = snap
	}
	if demote != nil {
		demotePromotedRows = demote
	}
	return func() {
		getPromotionSnapshot = origSnap
		demotePromotedRows = origDemote
	}
}

func TestLookupPromotionState_EmptySignatureReturnsZeroState(t *testing.T) {
	// Must not call the store at all.
	called := false
	restore := withPromotionStubs(t,
		func(string) (*store.PromotionSnapshot, error) {
			called = true
			return nil, nil
		},
		nil,
	)
	defer restore()

	state, err := LookupPromotionState("", 3)
	if err != nil {
		t.Fatalf("LookupPromotionState(\"\", 3) err = %v", err)
	}
	if called {
		t.Error("store was queried for empty signature (expected short-circuit)")
	}
	if state == nil || state.Promoted || state.Count != 0 || state.Recipe != nil {
		t.Errorf("state = %+v, want zero-value non-promoted", state)
	}
}

func TestLookupPromotionState_NonPositiveThresholdErrors(t *testing.T) {
	for _, threshold := range []int{0, -1, -99} {
		state, err := LookupPromotionState("sig", threshold)
		if err == nil {
			t.Errorf("threshold=%d: err = nil, want error", threshold)
		}
		if state != nil {
			t.Errorf("threshold=%d: state = %+v, want nil", threshold, state)
		}
		if err != nil && !strings.Contains(err.Error(), "must be positive") {
			t.Errorf("threshold=%d: err = %v, want positive-threshold error", threshold, err)
		}
	}
}

func TestLookupPromotionState_BelowThresholdNotPromoted(t *testing.T) {
	restore := withPromotionStubs(t,
		func(sig string) (*store.PromotionSnapshot, error) {
			return &store.PromotionSnapshot{
				FailureSig:   sig,
				CleanCount:   2,
				LatestRecipe: `{"kind":"builtin","action":"rebase-onto-base"}`,
			}, nil
		},
		nil,
	)
	defer restore()

	state, err := LookupPromotionState("sig", 3)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if state.Promoted {
		t.Error("Promoted = true, want false (count=2 < threshold=3)")
	}
	if state.Count != 2 {
		t.Errorf("Count = %d, want 2", state.Count)
	}
	if state.Recipe == nil || state.Recipe.Action != "rebase-onto-base" {
		t.Errorf("Recipe = %+v, want rebase-onto-base recipe", state.Recipe)
	}
}

func TestLookupPromotionState_AtThresholdPromoted(t *testing.T) {
	// Threshold boundary: count == threshold must promote.
	restore := withPromotionStubs(t,
		func(sig string) (*store.PromotionSnapshot, error) {
			return &store.PromotionSnapshot{
				FailureSig:   sig,
				CleanCount:   3,
				LatestRecipe: `{"kind":"builtin","action":"rebuild"}`,
			}, nil
		},
		nil,
	)
	defer restore()

	state, err := LookupPromotionState("sig", 3)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !state.Promoted {
		t.Error("Promoted = false, want true (count == threshold)")
	}
	if state.Recipe == nil || state.Recipe.Action != "rebuild" {
		t.Errorf("Recipe = %+v, want rebuild recipe", state.Recipe)
	}
}

func TestLookupPromotionState_AboveThresholdPromoted(t *testing.T) {
	restore := withPromotionStubs(t,
		func(sig string) (*store.PromotionSnapshot, error) {
			return &store.PromotionSnapshot{
				FailureSig:   sig,
				CleanCount:   10,
				LatestRecipe: `{"kind":"builtin","action":"rebuild"}`,
			}, nil
		},
		nil,
	)
	defer restore()

	state, err := LookupPromotionState("sig", 3)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !state.Promoted {
		t.Error("Promoted = false, want true (count > threshold)")
	}
}

func TestLookupPromotionState_EmptyRecipeNotPromotedEvenAboveThreshold(t *testing.T) {
	// Safety: if count is above threshold but recipe is missing, must not
	// promote. Can happen only if the walker logic changes in the future —
	// this pins the invariant.
	restore := withPromotionStubs(t,
		func(sig string) (*store.PromotionSnapshot, error) {
			return &store.PromotionSnapshot{
				FailureSig:   sig,
				CleanCount:   5,
				LatestRecipe: "", // no recipe
			}, nil
		},
		nil,
	)
	defer restore()

	state, err := LookupPromotionState("sig", 3)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if state.Promoted {
		t.Error("Promoted = true, want false (no recipe → cannot promote)")
	}
	if state.Recipe != nil {
		t.Errorf("Recipe = %+v, want nil", state.Recipe)
	}
}

func TestLookupPromotionState_CorruptRecipeReturnsError(t *testing.T) {
	// Corrupt JSON in stored recipe must surface as an error so the decide
	// step can log it and fall through to the agentic default rather than
	// silently skip promotion.
	restore := withPromotionStubs(t,
		func(sig string) (*store.PromotionSnapshot, error) {
			return &store.PromotionSnapshot{
				FailureSig:   sig,
				CleanCount:   3,
				LatestRecipe: `{"kind":not-json`,
			}, nil
		},
		nil,
	)
	defer restore()

	state, err := LookupPromotionState("sig", 3)
	if err == nil {
		t.Fatal("err = nil, want parse error")
	}
	if !strings.Contains(err.Error(), "parse stored recipe") {
		t.Errorf("err = %v, want parse-stored-recipe error", err)
	}
	// state must still come back with count populated so caller can log context.
	if state == nil {
		t.Fatal("state = nil, want populated state for error context")
	}
	if state.Count != 3 {
		t.Errorf("Count = %d, want 3 (state populated before recipe parse)", state.Count)
	}
}

func TestLookupPromotionState_StoreErrorPropagates(t *testing.T) {
	restore := withPromotionStubs(t,
		func(string) (*store.PromotionSnapshot, error) {
			return nil, errors.New("dolt timeout")
		},
		nil,
	)
	defer restore()

	_, err := LookupPromotionState("sig", 3)
	if err == nil {
		t.Fatal("err = nil, want wrapped store error")
	}
	if !strings.Contains(err.Error(), "lookup promotion snapshot") {
		t.Errorf("err = %v, want wrapped 'lookup promotion snapshot' error", err)
	}
	if !strings.Contains(err.Error(), "dolt timeout") {
		t.Errorf("err = %v, want underlying 'dolt timeout' preserved", err)
	}
}

func TestLookupPromotionState_SnapshotFieldsSetOnState(t *testing.T) {
	restore := withPromotionStubs(t,
		func(sig string) (*store.PromotionSnapshot, error) {
			return &store.PromotionSnapshot{
				FailureSig:   sig,
				CleanCount:   4,
				LatestRecipe: `{"kind":"builtin","action":"rebase-onto-base","params":{"base":"main"}}`,
			}, nil
		},
		nil,
	)
	defer restore()

	state, err := LookupPromotionState("step-failure:merge", 3)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if state.FailureSig != "step-failure:merge" {
		t.Errorf("FailureSig = %q, want step-failure:merge", state.FailureSig)
	}
	if state.Threshold != 3 {
		t.Errorf("Threshold = %d, want 3", state.Threshold)
	}
	if state.Recipe.Params["base"] != "main" {
		t.Errorf("Recipe.Params[base] = %q, want main", state.Recipe.Params["base"])
	}
}

// ---------------------------------------------------------------------------
// MarkDemoted
// ---------------------------------------------------------------------------

func TestMarkDemoted_EmptySignatureIsNoop(t *testing.T) {
	called := false
	restore := withPromotionStubs(t,
		nil,
		func(string) error {
			called = true
			return nil
		},
	)
	defer restore()

	if err := MarkDemoted(""); err != nil {
		t.Fatalf("MarkDemoted(\"\") err = %v, want nil", err)
	}
	if called {
		t.Error("demote seam was called for empty sig (expected short-circuit)")
	}
}

func TestMarkDemoted_SuccessNoError(t *testing.T) {
	got := ""
	restore := withPromotionStubs(t,
		nil,
		func(sig string) error {
			got = sig
			return nil
		},
	)
	defer restore()

	if err := MarkDemoted("step-failure:merge"); err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != "step-failure:merge" {
		t.Errorf("demote seam called with %q, want step-failure:merge", got)
	}
}

func TestMarkDemoted_WrapsStoreError(t *testing.T) {
	restore := withPromotionStubs(t,
		nil,
		func(string) error {
			return errors.New("dolt write conflict")
		},
	)
	defer restore()

	err := MarkDemoted("sig")
	if err == nil {
		t.Fatal("err = nil, want wrapped error")
	}
	if !strings.Contains(err.Error(), "mark demoted sig") {
		t.Errorf("err = %v, want 'mark demoted sig' wrapper", err)
	}
	if !strings.Contains(err.Error(), "dolt write conflict") {
		t.Errorf("err = %v, want underlying error preserved", err)
	}
}

// ---------------------------------------------------------------------------
// Recipe.ToRepairPlan — design spi-h32xj §6 chunk 7. A promoted recipe must
// dispatch through the same runtime paths as its un-promoted mechanical or
// worker form; the byte-identical-outcome guarantee starts with a plan that
// carries the identical Action + Params payload.
// ---------------------------------------------------------------------------

// TestToRepairPlan_RebaseMatchesUnpromotedDispatch is the promotion-level
// half of the "recipe promoted from mechanical repair produces same outcome
// when re-executed" test in design §7. It asserts the promoted plan (built
// from a recipe via ToRepairPlan) and an un-promoted plan (built from the
// agentic decide path via planForAction) route to the same dispatch key:
// identical Action, identical Params. Equal action + params → identical
// function call in handlePlanExecute → identical post-conditions on the
// workspace.
//
// The two plans' Mode fields differ by design — un-promoted carries
// Mechanical, recipe-wrapped carries Recipe — so the learn step can tell
// replays apart from agentic outcomes. The execute-level regression that
// this dispatch actually bottoms out in mechanicalActions["rebase-onto-base"]
// lives in pkg/executor/recovery_phase_test.go.
func TestToRepairPlan_RebaseMatchesUnpromotedDispatch(t *testing.T) {
	// Un-promoted path: decide emits this via planForAction when git state
	// suggests rebase-onto-base.
	unpromoted := planForAction("rebase-onto-base", 0.85, "git state analysis", false)
	if unpromoted.Mode != RepairModeMechanical {
		t.Fatalf("unpromoted Mode = %q, want %q", unpromoted.Mode, RepairModeMechanical)
	}

	// Promoted path: a clean recipe captured on a prior successful rebase,
	// replayed through ToRepairPlan.
	recipe := NewBuiltinRecipe("rebase-onto-base", nil)
	if recipe == nil {
		t.Fatal("NewBuiltinRecipe returned nil for valid action")
	}
	promoted := recipe.ToRepairPlan()

	// Outer mode differs by design (see doc comment above).
	if promoted.Mode != RepairModeRecipe {
		t.Errorf("promoted Mode = %q, want %q", promoted.Mode, RepairModeRecipe)
	}

	// Dispatch payload must match: same Action keys into the same
	// mechanicalActions entry in the executor.
	if promoted.Action != unpromoted.Action {
		t.Errorf("Action mismatch: promoted=%q unpromoted=%q — recipes would dispatch to a different function",
			promoted.Action, unpromoted.Action)
	}

	// Both plans carry no params for vanilla rebase-onto-base; if the
	// promoted path ever stamps phantom params the call to the mechanical
	// would diverge from the un-promoted one.
	if len(promoted.Params) != len(unpromoted.Params) {
		t.Errorf("Params length mismatch: promoted=%d unpromoted=%d", len(promoted.Params), len(unpromoted.Params))
	}
	for k, v := range unpromoted.Params {
		if promoted.Params[k] != v {
			t.Errorf("Params[%s] mismatch: promoted=%q unpromoted=%q", k, promoted.Params[k], v)
		}
	}
}

// TestToRepairPlan_ParamsCopiedNotAliased pins the invariant that
// ToRepairPlan returns a plan whose Params map is independent of the
// recipe's Params — mutating one must not affect the other. Guards against
// a future regression where handlePlanExecute or a mechanical callback
// mutates plan.Params and silently corrupts the stored recipe.
func TestToRepairPlan_ParamsCopiedNotAliased(t *testing.T) {
	recipe := NewBuiltinRecipe("cherry-pick", map[string]string{"commit": "abc1234"})
	if recipe == nil {
		t.Fatal("NewBuiltinRecipe returned nil")
	}
	plan := recipe.ToRepairPlan()

	plan.Params["commit"] = "mutated"
	if recipe.Params["commit"] != "abc1234" {
		t.Errorf("recipe.Params[commit] = %q, want abc1234 — plan mutation leaked into recipe",
			recipe.Params["commit"])
	}

	recipe.Params["commit"] = "rewound"
	if plan.Params["commit"] != "mutated" {
		t.Errorf("plan.Params[commit] = %q, want mutated — recipe mutation leaked into plan",
			plan.Params["commit"])
	}
}

// TestToRepairPlan_NilRecipeReturnsBareRecipeMode pins the nil-safety
// contract: a nil recipe yields a RepairPlan whose Mode is Recipe and
// whose Action is empty. handlePlanExecute treats that as a dispatch
// error, which is the safe failure mode — we never want a nil recipe to
// silently resolve to "do nothing" or fall back to the agentic default.
func TestToRepairPlan_NilRecipeReturnsBareRecipeMode(t *testing.T) {
	var r *MechanicalRecipe
	plan := r.ToRepairPlan()
	if plan.Mode != RepairModeRecipe {
		t.Errorf("Mode = %q, want %q", plan.Mode, RepairModeRecipe)
	}
	if plan.Action != "" {
		t.Errorf("Action = %q, want empty", plan.Action)
	}
}

// TestToRepairPlan_WorkspaceKindMatchesActionFamily pins the workspace
// dispatch table: promoting a mechanical action onto a recipe plan must
// produce an owned-worktree workspace (matching the un-promoted
// mechanical's default provisioning), and promoting a worker action must
// produce a borrowed worktree. Drifting off this mapping means a promoted
// recipe would try to dispatch on the wrong workspace kind and the
// executor's resolveRepairWorkspace would either fail to find the bead's
// staging tree or provision a worktree the mechanical doesn't expect.
func TestToRepairPlan_WorkspaceKindMatchesActionFamily(t *testing.T) {
	cases := []struct {
		action   string
		wantKind runtime.WorkspaceKind
	}{
		{"rebase-onto-base", runtime.WorkspaceKindOwnedWorktree},
		{"cherry-pick", runtime.WorkspaceKindOwnedWorktree},
		{"rebuild", runtime.WorkspaceKindOwnedWorktree},
		{"reset-to-step", runtime.WorkspaceKindRepo},
		{"resolve-conflicts", runtime.WorkspaceKindBorrowedWorktree},
		{"resummon", runtime.WorkspaceKindBorrowedWorktree},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			plan := NewBuiltinRecipe(tc.action, nil).ToRepairPlan()
			if plan.Workspace.Kind != tc.wantKind {
				t.Errorf("%s: Workspace.Kind = %q, want %q", tc.action, plan.Workspace.Kind, tc.wantKind)
			}
			if plan.Verify.Kind != VerifyKindRecipePostcondition {
				t.Errorf("%s: Verify.Kind = %q, want %q", tc.action, plan.Verify.Kind, VerifyKindRecipePostcondition)
			}
			if plan.Verify.StepName != tc.action {
				t.Errorf("%s: Verify.StepName = %q, want %q", tc.action, plan.Verify.StepName, tc.action)
			}
		})
	}
}

// TestToRepairPlan_ParamsRoundTrip pins that non-trivial params survive
// the recipe → plan conversion with equal keys and values — the byte-
// identical-outcome guarantee at the Params level.
func TestToRepairPlan_ParamsRoundTrip(t *testing.T) {
	params := map[string]string{
		"commit":      "deadbeef",
		"step_target": "implement",
	}
	recipe := NewBuiltinRecipe("cherry-pick", params)
	plan := recipe.ToRepairPlan()
	if !reflect.DeepEqual(plan.Params, params) {
		t.Errorf("plan.Params = %v, want %v", plan.Params, params)
	}
}
