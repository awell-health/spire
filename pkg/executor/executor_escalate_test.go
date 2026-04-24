package executor

import (
	"testing"

	"github.com/awell-health/spire/pkg/recovery"
)

func TestBuildSeedMetadata(t *testing.T) {
	tests := []struct {
		name        string
		parentID    string
		failureType string
		nodeCtx     string
		want        recovery.RecoveryMetadata
	}{
		{
			name:        "empty nodeCtx",
			parentID:    "spi-abc",
			failureType: "merge-failure",
			nodeCtx:     "",
			want: recovery.RecoveryMetadata{
				FailureClass:     "merge-failure",
				SourceBead:       "spi-abc",
				SourceStep:       "",
				SourceFlow:       "",
				FailureSignature: "merge-failure",
			},
		},
		{
			name:        "nodeCtx with step and flow",
			parentID:    "spi-def",
			failureType: "step-failure",
			nodeCtx:     "step=implement action=wizard.run flow=implement workspace=feature",
			want: recovery.RecoveryMetadata{
				FailureClass:     "step-failure",
				SourceBead:       "spi-def",
				SourceStep:       "implement",
				SourceFlow:       "implement",
				FailureSignature: "step-failure:implement",
			},
		},
		{
			name:        "nodeCtx step only no flow",
			parentID:    "spi-ghi",
			failureType: "build-failure",
			nodeCtx:     "step=review",
			want: recovery.RecoveryMetadata{
				FailureClass:     "build-failure",
				SourceBead:       "spi-ghi",
				SourceStep:       "review",
				SourceFlow:       "",
				FailureSignature: "build-failure:review",
			},
		},
		{
			name:        "nodeCtx flow only no step",
			parentID:    "spi-jkl",
			failureType: "repo-resolution",
			nodeCtx:     "action=wizard.run flow=implement",
			want: recovery.RecoveryMetadata{
				FailureClass:     "repo-resolution",
				SourceBead:       "spi-jkl",
				SourceStep:       "",
				SourceFlow:       "implement",
				FailureSignature: "repo-resolution",
			},
		},
		{
			name:        "step appears after other fields with flow",
			parentID:    "spi-mno",
			failureType: "step-failure",
			nodeCtx:     "action=wizard.run step=merge flow=review workspace=feature",
			want: recovery.RecoveryMetadata{
				FailureClass:     "step-failure",
				SourceBead:       "spi-mno",
				SourceStep:       "merge",
				SourceFlow:       "review",
				FailureSignature: "step-failure:merge",
			},
		},
		{
			name:        "flow is task-plan",
			parentID:    "spi-pqr",
			failureType: "step-failure",
			nodeCtx:     "step=verify-build flow=task-plan",
			want: recovery.RecoveryMetadata{
				FailureClass:     "step-failure",
				SourceBead:       "spi-pqr",
				SourceStep:       "verify-build",
				SourceFlow:       "task-plan",
				FailureSignature: "step-failure:verify-build",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildSeedMetadata(tt.parentID, tt.failureType, tt.nodeCtx)
			if got.FailureClass != tt.want.FailureClass {
				t.Errorf("FailureClass = %q, want %q", got.FailureClass, tt.want.FailureClass)
			}
			if got.SourceBead != tt.want.SourceBead {
				t.Errorf("SourceBead = %q, want %q", got.SourceBead, tt.want.SourceBead)
			}
			if got.SourceStep != tt.want.SourceStep {
				t.Errorf("SourceStep = %q, want %q", got.SourceStep, tt.want.SourceStep)
			}
			if got.SourceFlow != tt.want.SourceFlow {
				t.Errorf("SourceFlow = %q, want %q", got.SourceFlow, tt.want.SourceFlow)
			}
			if got.FailureSignature != tt.want.FailureSignature {
				t.Errorf("FailureSignature = %q, want %q", got.FailureSignature, tt.want.FailureSignature)
			}
			if got.SourceFormula != "" {
				t.Errorf("SourceFormula = %q, want empty (not yet wired)", got.SourceFormula)
			}
		})
	}
}

func TestSeedRecoveryMetadata_EmptyRecoveryID(t *testing.T) {
	// seedRecoveryMetadata with empty recoveryID should return immediately
	// without calling store.SetBeadMetadataMap (which would fail without a db).
	// If it doesn't guard, this test panics or errors.
	seedRecoveryMetadata("", "spi-parent", "merge-failure", "step=implement")
}

// TestMessageArchmage_DerivesPrefix verifies that MessageArchmage creates
// the message bead using the source bead's prefix, not a hardcoded one.
func TestMessageArchmage_DerivesPrefix(t *testing.T) {
	tests := []struct {
		name       string
		beadID     string
		wantPrefix string
	}{
		{"spi prefix", "spi-abc123", "spi"},
		{"spd prefix", "spd-ac5", "spd"},
		{"web prefix", "web-xyz", "web"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPrefix string
			deps := &Deps{
				CreateBead: func(opts CreateOpts) (string, error) {
					gotPrefix = opts.Prefix
					return "msg-001", nil
				},
				AddDepTyped: func(issueID, dependsOnID, depType string) error { return nil },
			}
			MessageArchmage("test-agent", tt.beadID, "test message", deps)
			if gotPrefix != tt.wantPrefix {
				t.Errorf("CreateBead prefix = %q, want %q", gotPrefix, tt.wantPrefix)
			}
		})
	}
}
