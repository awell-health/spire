package executor

import (
	"errors"
	"fmt"
	"testing"

	spgit "github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/recovery"
)

// TestDiagnoseFailure_OverridesWithClassify is the Seam 4 proof that
// diagnoseFailure's ClassifyError override fires inside the executor: an
// initial label-driven diagnosis (e.g. FailStepFailure from
// "interrupted:step-failure") must be REPLACED by the more specific class +
// sub-class that ClassifyError emits for a recoverable error pattern.
//
// Without this override, Decide can't short-circuit to the deterministic
// mechanical repair — it only sees the generic FailStepFailure class and
// falls through to Claude or the resummon fallback. The test pins the
// contract end-to-end: raw error in → overridden (class, sub-class) out.
func TestDiagnoseFailure_OverridesWithClassify(t *testing.T) {
	cases := []struct {
		name       string
		failure    error
		wantClass  recovery.FailureClass
		wantSubCls string
	}{
		{
			name:       "merge-race sentinel overrides step-failure",
			failure:    fmt.Errorf("merge to main: %w", spgit.ErrMergeRace),
			wantClass:  recovery.FailMerge,
			wantSubCls: recovery.SubClassMergeRace,
		},
		{
			name:       "post-rebase-ff-only text overrides step-failure",
			failure:    errors.New("ff-only merge failed after rebase: Not possible to fast-forward"),
			wantClass:  recovery.FailMerge,
			wantSubCls: recovery.SubClassPostRebaseFF,
		},
		{
			name:       "merge-conflict text overrides step-failure",
			failure:    errors.New("rebase conflict in files: pkg/foo.go"),
			wantClass:  recovery.FailMerge,
			wantSubCls: recovery.SubClassMergeConflict,
		},
		{
			name:       "stale-worktree text overrides step-failure",
			failure:    errors.New("worktree add feat/spi-xyz: fatal: 'feat/spi-xyz' is already used by worktree at '/some/path'"),
			wantClass:  recovery.FailMerge,
			wantSubCls: recovery.SubClassStaleWorktree,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Build an executor whose Diagnose path will return a base
			// diagnosis carrying the label-driven classification
			// (step-failure). The override step then inspects tc.failure and
			// SHOULD replace FailureMode+SubClass with the more specific
			// merge-family class. Proving the override fires is the whole
			// point — if it doesn't, Decide sees only FailStepFailure and
			// the deterministic mechanical path never runs.
			e, state := newRecoveryTestExecutor(t)

			// Stamp a label on the target bead so recovery.Diagnose produces
			// a non-nil diagnosis with FailureMode=step-failure.
			origGetBead := e.deps.GetBead
			e.deps.GetBead = func(id string) (Bead, error) {
				b, err := origGetBead(id)
				if err != nil {
					return b, err
				}
				if id == e.beadID {
					b.Labels = append(b.Labels, "interrupted:step-failure")
				}
				return b, nil
			}

			got, _ := e.diagnoseFailure("merge", tc.failure, state)

			if got.FailureMode != tc.wantClass {
				t.Errorf("FailureMode = %q, want %q (ClassifyError override did not fire)",
					got.FailureMode, tc.wantClass)
			}
			if got.SubClass != tc.wantSubCls {
				t.Errorf("SubClass = %q, want %q (ClassifyError override did not set sub-class)",
					got.SubClass, tc.wantSubCls)
			}
		})
	}
}

// TestDiagnoseFailure_KeepsOriginalWhenClassifyUnknown verifies the inverse of
// the override: if ClassifyError returns FailUnknown, the base diagnosis
// (label-driven or stub) is preserved unchanged. This pins the "override only
// on specific match" contract — a generic error must not clobber the
// label-driven classification.
func TestDiagnoseFailure_KeepsOriginalWhenClassifyUnknown(t *testing.T) {
	e, state := newRecoveryTestExecutor(t)

	// Label-driven path produces FailStepFailure; a generic error without a
	// recognizable pattern must NOT trigger the override.
	origGetBead := e.deps.GetBead
	e.deps.GetBead = func(id string) (Bead, error) {
		b, err := origGetBead(id)
		if err != nil {
			return b, err
		}
		if id == e.beadID {
			b.Labels = append(b.Labels, "interrupted:step-failure")
		}
		return b, nil
	}

	got, _ := e.diagnoseFailure("implement", errors.New("unrelated generic failure"), state)

	if got.FailureMode != recovery.FailStepFailure {
		t.Errorf("FailureMode = %q, want %q (generic error should NOT override base diagnosis)",
			got.FailureMode, recovery.FailStepFailure)
	}
	if got.SubClass != "" {
		t.Errorf("SubClass = %q, want empty (ClassifyError returned FailUnknown → no sub-class set)", got.SubClass)
	}
}
