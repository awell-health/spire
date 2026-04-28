package recovery

import (
	"encoding/json"
	"fmt"

	"github.com/awell-health/spire/pkg/runtime"
)

// RecipeKind discriminates MechanicalRecipe variants.
//
// - "builtin" names a single recovery action with static params.
// - "sequence" composes an ordered list of recipes to run in order.
//
// A nil *MechanicalRecipe means "no codified recipe captured" — those
// signatures will never promote to mechanical, which is the safe default
// for resolutions whose steps cannot be reliably replayed.
type RecipeKind string

const (
	RecipeKindBuiltin  RecipeKind = "builtin"
	RecipeKindSequence RecipeKind = "sequence"
)

// MechanicalRecipe is the codified, replayable form of an agentic recovery
// outcome. Captured on success (see pkg/executor/recovery_actions.go) and
// persisted on the recovery_learnings row alongside the resolution.
//
// When the promotion counter for a failure_signature reaches threshold, the
// cleric's decide step returns this recipe instead of dispatching an
// apprentice. A single failure of a promoted recipe demotes the signature
// back to the agentic default (see MarkDemoted / PromotionState).
type MechanicalRecipe struct {
	Kind   RecipeKind         `json:"kind"`
	Action string             `json:"action,omitempty"` // for kind=builtin — matches RecoveryAction.Name
	Params map[string]string  `json:"params,omitempty"` // for kind=builtin — passed through as ctx.Params
	Steps  []MechanicalRecipe `json:"steps,omitempty"`  // for kind=sequence
}

// Validate returns an error if the recipe is malformed.
func (r *MechanicalRecipe) Validate() error {
	if r == nil {
		return nil
	}
	switch r.Kind {
	case RecipeKindBuiltin:
		if r.Action == "" {
			return fmt.Errorf("recipe: builtin kind requires action")
		}
		if len(r.Steps) > 0 {
			return fmt.Errorf("recipe: builtin kind must not have steps")
		}
	case RecipeKindSequence:
		if len(r.Steps) == 0 {
			return fmt.Errorf("recipe: sequence kind requires at least one step")
		}
		if r.Action != "" {
			return fmt.Errorf("recipe: sequence kind must not have action")
		}
		for i := range r.Steps {
			if err := r.Steps[i].Validate(); err != nil {
				return fmt.Errorf("recipe: step[%d]: %w", i, err)
			}
		}
	default:
		return fmt.Errorf("recipe: unknown kind %q", r.Kind)
	}
	return nil
}

// NewBuiltinRecipe constructs a simple builtin recipe for action name with
// optional static params. Returns nil if name is empty (callers treat nil
// as "no recipe", which blocks promotion — a safe default).
func NewBuiltinRecipe(name string, params map[string]string) *MechanicalRecipe {
	if name == "" {
		return nil
	}
	p := map[string]string{}
	for k, v := range params {
		p[k] = v
	}
	return &MechanicalRecipe{
		Kind:   RecipeKindBuiltin,
		Action: name,
		Params: p,
	}
}

// MarshalRecipe serialises a recipe for storage in the mechanical_recipe
// column. A nil recipe serialises to the empty string, meaning "no recipe".
func MarshalRecipe(r *MechanicalRecipe) (string, error) {
	if r == nil {
		return "", nil
	}
	b, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("marshal mechanical recipe: %w", err)
	}
	return string(b), nil
}

// UnmarshalRecipe parses a serialised recipe. Empty input yields nil
// (meaning no recipe), not an error.
func UnmarshalRecipe(raw string) (*MechanicalRecipe, error) {
	if raw == "" {
		return nil, nil
	}
	var r MechanicalRecipe
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return nil, fmt.Errorf("unmarshal mechanical recipe: %w", err)
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

// ToRepairPlan converts a promoted recipe into the executable RepairPlan
// the cleric's execute step consumes. The outer Mode is RepairModeRecipe so
// the learn step can distinguish replayed outcomes from agentic ones; the
// inner Action / Params mirror the un-promoted mechanical or worker form so
// execution dispatches through the exact same runtime paths (design
// spi-h32xj §2 "Recipe" entry).
//
// A nil recipe produces a bare RepairPlan{Mode: RepairModeRecipe} so
// callers don't have to nil-check before propagating the mode — this shape
// fails dispatch loudly in handlePlanExecute rather than silently
// degrading.
//
// Only builtin recipes are dispatchable today; sequence recipes fall
// through to an empty Action.
func (r *MechanicalRecipe) ToRepairPlan() RepairPlan {
	plan := RepairPlan{Mode: RepairModeRecipe}
	if r == nil {
		return plan
	}
	var action string
	var params map[string]string
	if r.Kind == RecipeKindBuiltin {
		action = r.Action
		params = r.Params
	}
	plan.Action = action
	plan.Confidence = 1.0
	if action != "" {
		plan.Reason = fmt.Sprintf("Promoted mechanical recipe: %s", action)
	}
	plan.Workspace = workspaceFromAction(action)
	plan.Verify = VerifyPlan{
		Kind:     VerifyKindRecipePostcondition,
		StepName: action,
	}
	if len(params) > 0 {
		plan.Params = make(map[string]string, len(params))
		for k, v := range params {
			plan.Params[k] = v
		}
	}
	return plan
}

// workspaceFromAction returns the WorkspaceRequest an action requires,
// mirroring the dispatch shape the un-promoted mechanical or worker plan
// used. Keep this in sync with actionToRepairMode in decide.go — any
// action that promotes through ToRepairPlan must have a workspace mapping
// here or it will fail to dispatch when executed.
func workspaceFromAction(action string) WorkspaceRequest {
	switch action {
	case "rebase-onto-base", "cherry-pick", "rebuild":
		// Mechanical actions that mutate the feature branch run on a fresh
		// owned worktree off that branch — matches the un-promoted path.
		return WorkspaceRequest{Kind: runtime.WorkspaceKindOwnedWorktree}
	case "reset-to-step", "reset_to_step", "reset-hard", "reset_hard":
		// Record-only / graph-level mechanicals need a repo handle only.
		return WorkspaceRequest{Kind: runtime.WorkspaceKindRepo}
	case "resolve-conflicts", "resummon", "reset", "triage", "targeted-fix":
		// Worker actions run inside the target bead's paused staging
		// worktree so they can advance the in-progress operation.
		return WorkspaceRequest{Kind: runtime.WorkspaceKindBorrowedWorktree}
	default:
		return WorkspaceRequest{}
	}
}
