package intent

import (
	"strings"
	"testing"
)

// withRuntime returns a copy of i with a non-empty Runtime.Image so the
// table cases can focus on a single failure axis at a time.
func withRuntime(i WorkloadIntent) WorkloadIntent {
	i.Runtime = Runtime{Image: "spire-agent:dev"}
	return i
}

// TestValidate_AllowedPairs covers every (Role, Phase) pair listed in
// Allowed. Each must validate cleanly when Runtime.Image is populated.
// Adding a new pair to Allowed without adding a row here will surface
// as a coverage gap, not a silent acceptance.
func TestValidate_AllowedPairs(t *testing.T) {
	cases := []struct {
		name  string
		role  Role
		phase Phase
	}{
		{"wizard/implement", RoleWizard, PhaseImplement},
		{"apprentice/implement", RoleApprentice, PhaseImplement},
		{"apprentice/fix", RoleApprentice, PhaseFix},
		{"apprentice/review-fix", RoleApprentice, PhaseReviewFix},
		{"sage/review", RoleSage, PhaseReview},
		{"cleric/recovery", RoleCleric, PhaseRecovery},
	}

	// Sanity: the table covers every pair in Allowed exactly once.
	expected := 0
	for _, phases := range Allowed {
		expected += len(phases)
	}
	if len(cases) != expected {
		t.Fatalf("table size = %d, but Allowed has %d (Role,Phase) pairs; "+
			"keep this in sync so Validate coverage stays exhaustive",
			len(cases), expected)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i := withRuntime(WorkloadIntent{
				Role:  tc.role,
				Phase: tc.phase,
			})
			if err := Validate(i); err != nil {
				t.Errorf("Validate(%s/%s) = %v, want nil", tc.role, tc.phase, err)
			}
		})
	}
}

// TestValidate_RejectsContractViolations exercises every error path in
// Validate with a stable message-prefix assertion so callers can rely
// on the surfaced reason for routing/observability decisions.
func TestValidate_RejectsContractViolations(t *testing.T) {
	cases := []struct {
		name       string
		mut        func(*WorkloadIntent)
		wantPrefix string
	}{
		{
			name:       "empty role",
			mut:        func(i *WorkloadIntent) { i.Role = ""; i.Phase = PhaseImplement },
			wantPrefix: "intent missing role",
		},
		{
			name:       "empty phase",
			mut:        func(i *WorkloadIntent) { i.Role = RoleApprentice; i.Phase = "" },
			wantPrefix: "intent missing phase",
		},
		{
			name:       "unknown role",
			mut:        func(i *WorkloadIntent) { i.Role = "necromancer"; i.Phase = PhaseImplement },
			wantPrefix: `unknown role: "necromancer"`,
		},
		{
			name: "unknown phase",
			mut: func(i *WorkloadIntent) {
				i.Role = RoleApprentice
				i.Phase = "ponder"
			},
			wantPrefix: `unknown phase: "ponder"`,
		},
		{
			name: "unsupported pair: sage/implement",
			mut: func(i *WorkloadIntent) {
				i.Role = RoleSage
				i.Phase = PhaseImplement
			},
			wantPrefix: "unsupported role/phase combination: sage/implement",
		},
		{
			name: "unsupported pair: cleric/implement",
			mut: func(i *WorkloadIntent) {
				i.Role = RoleCleric
				i.Phase = PhaseImplement
			},
			wantPrefix: "unsupported role/phase combination: cleric/implement",
		},
		{
			name: "unsupported pair: wizard/recovery",
			mut: func(i *WorkloadIntent) {
				i.Role = RoleWizard
				i.Phase = PhaseRecovery
			},
			wantPrefix: "unsupported role/phase combination: wizard/recovery",
		},
		{
			name: "runtime missing image",
			mut: func(i *WorkloadIntent) {
				i.Role = RoleApprentice
				i.Phase = PhaseImplement
				i.Runtime = Runtime{} // empty Image
			},
			wantPrefix: "intent runtime missing image",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i := withRuntime(WorkloadIntent{})
			tc.mut(&i)
			err := Validate(i)
			if err == nil {
				t.Fatalf("Validate(%+v) = nil, want error %q", i, tc.wantPrefix)
			}
			if !strings.HasPrefix(err.Error(), tc.wantPrefix) {
				t.Errorf("Validate(%+v) error = %q, want prefix %q",
					i, err.Error(), tc.wantPrefix)
			}
		})
	}
}

// TestAllowed_NoFormulaPhaseRecovery is a regression guard for the
// cleric routing migration: the operator must NOT accept routing via
// formula_phase=recovery on any role other than cleric. Validate
// already enforces this — this test pins it loudly so a stray entry
// (e.g. wizard/recovery) cannot creep into Allowed.
func TestAllowed_NoFormulaPhaseRecovery(t *testing.T) {
	for role, phases := range Allowed {
		if role == RoleCleric {
			continue
		}
		if _, ok := phases[PhaseRecovery]; ok {
			t.Errorf("Allowed[%s] contains PhaseRecovery; only RoleCleric "+
				"is permitted to handle recovery (cluster contract)", role)
		}
	}
	if _, ok := Allowed[RoleCleric][PhaseRecovery]; !ok {
		t.Error("Allowed[RoleCleric][PhaseRecovery] missing; cleric routing requires it")
	}
}

// TestAllowed_RolesCovered enforces that every Role constant appears
// in Allowed with at least one phase. A Role without a phase would be
// unroutable — the operator switch has nothing to dispatch to.
func TestAllowed_RolesCovered(t *testing.T) {
	roles := []Role{RoleWizard, RoleApprentice, RoleSage, RoleCleric}
	for _, r := range roles {
		phases, ok := Allowed[r]
		if !ok || len(phases) == 0 {
			t.Errorf("Role %q has no phases in Allowed; every Role must "+
				"declare at least one supported phase", r)
		}
	}
}
