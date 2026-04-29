package steward

import (
	"testing"

	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
)

// Test the recoveryShouldResume helper used by SweepHookedSteps to
// distinguish closed-recovery beads that should resume the wizard from
// those that should not (takeover, no outcome). Cleric runtime
// (spi-hhkozk) + strict success contract (spi-skfsia finding 2).
//
// The strict contract requires BOTH cleric_outcome=approve+executed AND
// cleric_execute_success=true. The two-key check defends against an
// audit/listing path that writes the outcome string without the action
// having actually run; only cleric.Execute (on real success) sets the
// strict marker.
func TestRecoveryShouldResume(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]string
		want bool
	}{
		{
			name: "cleric finish + execute success triggers resume",
			meta: map[string]string{
				"cleric_outcome":         "approve+executed",
				"cleric_execute_success": "true",
			},
			want: true,
		},
		{
			name: "outcome approve+executed without success marker does NOT resume",
			meta: map[string]string{"cleric_outcome": "approve+executed"},
			want: false,
		},
		{
			name: "outcome approve+failed even with success marker does NOT resume",
			meta: map[string]string{
				"cleric_outcome":         "approve+failed",
				"cleric_execute_success": "true",
			},
			want: false,
		},
		{
			name: "cleric takeover does not resume",
			meta: map[string]string{},
			want: false,
		},
		{
			name: "no outcome at all does not resume",
			meta: nil,
			want: false,
		},
		{
			name: "legacy DecisionResume still triggers resume",
			meta: map[string]string{recovery.KeyRecoveryOutcome: `{"decision":"resume","outcome_id":"test","resolved_at":"2026-04-28T00:00:00Z","duration_seconds":1.0,"recipe_name":"foo","attempts":1,"resolution_kind":"recipe"}`},
			want: true,
		},
		{
			name: "legacy DecisionEscalate does not resume",
			meta: map[string]string{recovery.KeyRecoveryOutcome: `{"decision":"escalate","outcome_id":"test","resolved_at":"2026-04-28T00:00:00Z","duration_seconds":1.0,"recipe_name":"foo","attempts":1,"resolution_kind":"recipe"}`},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := recoveryShouldResume(store.Bead{Metadata: tc.meta})
			if got != tc.want {
				t.Errorf("recoveryShouldResume = %v, want %v", got, tc.want)
			}
		})
	}
}
