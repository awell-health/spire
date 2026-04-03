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
				FailureSignature: "merge-failure",
			},
		},
		{
			name:        "nodeCtx with step",
			parentID:    "spi-def",
			failureType: "step-failure",
			nodeCtx:     "step=implement action=wizard.run flow=implement workspace=feature",
			want: recovery.RecoveryMetadata{
				FailureClass:     "step-failure",
				SourceBead:       "spi-def",
				SourceStep:       "implement",
				FailureSignature: "step-failure:implement",
			},
		},
		{
			name:        "nodeCtx step only",
			parentID:    "spi-ghi",
			failureType: "build-failure",
			nodeCtx:     "step=review",
			want: recovery.RecoveryMetadata{
				FailureClass:     "build-failure",
				SourceBead:       "spi-ghi",
				SourceStep:       "review",
				FailureSignature: "build-failure:review",
			},
		},
		{
			name:        "nodeCtx without step prefix",
			parentID:    "spi-jkl",
			failureType: "repo-resolution",
			nodeCtx:     "action=wizard.run flow=implement",
			want: recovery.RecoveryMetadata{
				FailureClass:     "repo-resolution",
				SourceBead:       "spi-jkl",
				SourceStep:       "",
				FailureSignature: "repo-resolution",
			},
		},
		{
			name:        "step appears after other fields",
			parentID:    "spi-mno",
			failureType: "step-failure",
			nodeCtx:     "action=wizard.run step=merge workspace=feature",
			want: recovery.RecoveryMetadata{
				FailureClass:     "step-failure",
				SourceBead:       "spi-mno",
				SourceStep:       "merge",
				FailureSignature: "step-failure:merge",
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
