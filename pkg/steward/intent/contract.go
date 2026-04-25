package intent

// Cluster child-run dispatch contract.
//
// The cluster-native dispatch seam carries explicit Role + Phase +
// Runtime identity across the wizard→operator boundary. The operator
// routes by Role (no longer by FormulaPhase), validates the
// (Role, Phase) pair against Allowed, and materializes the pod from
// Runtime.Image / Runtime.Command / Runtime.Env without inferring
// identity from any local backend.
//
// This file is the single source of truth for the canonical role and
// phase enums and the (Role, Phase) allowlist. Operator routing and
// pod-builder selection both consume Validate and Allowed; no string
// literals at call sites.

import "fmt"

// Role identifies the cluster role a child run runs as. The operator
// routes WorkloadIntents by Role; pod-builder selection picks a builder
// per (Role, Phase). The set is closed — Validate rejects any Role not
// listed here.
type Role string

const (
	// RoleWizard is the per-bead orchestrator dispatched at bead-level
	// claim. It owns formula state and emits step-level intents from
	// inside the pod.
	RoleWizard Role = "wizard"
	// RoleApprentice is the one-shot implementer dispatched for
	// implement / fix / review-fix phases.
	RoleApprentice Role = "apprentice"
	// RoleSage is the one-shot reviewer dispatched for the review phase.
	RoleSage Role = "sage"
	// RoleCleric is the failure-recovery driver dispatched for the
	// recovery phase. The operator must route cleric via Role=cleric,
	// not via formula_phase=recovery (which is not a recognized phase
	// in the new contract).
	RoleCleric Role = "cleric"
)

// Phase identifies which formula phase a child run is performing. The
// (Role, Phase) pair drives pod-builder selection — see Allowed. The
// set is closed; Validate rejects any Phase not listed here.
//
// The PhaseImplement / PhaseFix / PhaseReview values declared in
// intent.go are untyped string constants and coerce into Phase
// transparently. The two Phase values that are new with this contract
// (PhaseReviewFix, PhaseRecovery) are declared below.
type Phase string

const (
	// PhaseReviewFix is the post-review apprentice re-engagement phase.
	// Distinct from PhaseFix so the operator can route it to the
	// apprentice builder via the (apprentice, review-fix) allowlist
	// entry instead of conflating it with the diagnostic fix phase.
	PhaseReviewFix Phase = "review-fix"
	// PhaseRecovery is the cleric recovery phase. Routed via
	// Role=cleric, NOT via formula_phase=recovery (the operator no
	// longer recognizes that phase string for routing decisions).
	PhaseRecovery Phase = "recovery"
)

// Runtime carries the explicit image/command/env/resources the operator
// needs to materialize a child pod without inferring identity from any
// local backend. Producers (executor, wizard, steward) populate it from
// their own configuration surfaces; the operator reads it verbatim.
//
// Resources is a Resources value (the same type WorkloadIntent already
// carries) so callers do not need to plumb a parallel resource shape.
type Runtime struct {
	// Image is the agent container image reference. Required — Validate
	// rejects an empty Image.
	Image string `json:"image"`
	// Command optionally overrides the main container command. When
	// empty the pod builder picks the role's canonical command (e.g.
	// "spire execute" for wizard, "spire apprentice run" for apprentice).
	Command []string `json:"command,omitempty"`
	// Env optionally adds extra environment variables on the main
	// container. The pod builder layers these on top of the canonical
	// env vocabulary; an Env entry never replaces a canonical key.
	Env map[string]string `json:"env,omitempty"`
	// Resources is the CPU/memory envelope. Empty defers to the pod
	// builder's defaults.
	Resources Resources `json:"resources,omitempty"`
}

// Allowed is the canonical (Role, Phase) allowlist. Any pair not
// present here fails Validate and SelectBuilder. Adding a new pair
// requires updating both the operator routing and pkg/agent's pod
// builder registry; the closed-set design keeps that in sync.
var Allowed = map[Role]map[Phase]struct{}{
	RoleWizard:     {PhaseImplement: {}},
	RoleApprentice: {PhaseImplement: {}, PhaseFix: {}, PhaseReviewFix: {}},
	RoleSage:       {PhaseReview: {}},
	RoleCleric:     {PhaseRecovery: {}},
}

// Validate enforces the cluster child-run dispatch contract on a
// WorkloadIntent. Operator and pod-builder code paths call Validate
// before consuming Role/Phase/Runtime. Returns nil iff the intent
// carries an explicit (Role, Phase) pair that appears in Allowed and a
// non-empty Runtime.Image.
//
// Error messages have stable prefixes so callers can table-test them:
//
//   - "intent missing role"                     — empty Role
//   - "intent missing phase"                    — empty Phase
//   - "unknown role: %q"                        — Role not in Allowed
//   - "unknown phase: %q"                       — Phase not in any Allowed[Role]
//   - "unsupported role/phase combination: r/p" — pair not in Allowed[role]
//   - "intent runtime missing image"            — empty Runtime.Image
func Validate(i WorkloadIntent) error {
	if i.Role == "" {
		return fmt.Errorf("intent missing role")
	}
	if i.Phase == "" {
		return fmt.Errorf("intent missing phase")
	}
	phases, roleOK := Allowed[i.Role]
	if !roleOK {
		return fmt.Errorf("unknown role: %q", i.Role)
	}
	if !isKnownPhase(i.Phase) {
		return fmt.Errorf("unknown phase: %q", i.Phase)
	}
	if _, pairOK := phases[i.Phase]; !pairOK {
		return fmt.Errorf("unsupported role/phase combination: %s/%s", i.Role, i.Phase)
	}
	if i.Runtime.Image == "" {
		return fmt.Errorf("intent runtime missing image")
	}
	return nil
}

// isKnownPhase reports whether p is any phase that appears in any
// Allowed[Role] set. Used by Validate to distinguish "unknown phase"
// from "unsupported pair" — a phase that no role accepts is unknown,
// not just unsupported in this combination.
func isKnownPhase(p Phase) bool {
	for _, phases := range Allowed {
		if _, ok := phases[p]; ok {
			return true
		}
	}
	return false
}
