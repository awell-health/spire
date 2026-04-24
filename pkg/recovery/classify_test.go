package recovery

import (
	"errors"
	"fmt"
	"testing"

	"github.com/awell-health/spire/pkg/git"
)

// TestClassifyError_MergeRace verifies the sentinel ErrMergeRace wrapped with
// %w anywhere in the chain classifies as FailMerge / SubClassMergeRace. This is
// the contract that lets the recovery cycle auto-recover from merge races
// without round-tripping through Claude.
func TestClassifyError_MergeRace(t *testing.T) {
	// Direct sentinel.
	class, sub := ClassifyError(git.ErrMergeRace)
	if class != FailMerge || sub != SubClassMergeRace {
		t.Errorf("direct ErrMergeRace: got (%s, %s), want (%s, %s)",
			class, sub, FailMerge, SubClassMergeRace)
	}

	// Wrapped via %w (what actionMergeToMain actually produces).
	wrapped := fmt.Errorf("merge to main: %w", git.ErrMergeRace)
	class, sub = ClassifyError(wrapped)
	if class != FailMerge || sub != SubClassMergeRace {
		t.Errorf("wrapped ErrMergeRace: got (%s, %s), want (%s, %s)",
			class, sub, FailMerge, SubClassMergeRace)
	}

	// Double-wrapped.
	double := fmt.Errorf("step merge: %w", wrapped)
	class, sub = ClassifyError(double)
	if class != FailMerge || sub != SubClassMergeRace {
		t.Errorf("double-wrapped ErrMergeRace: got (%s, %s), want (%s, %s)",
			class, sub, FailMerge, SubClassMergeRace)
	}
}

// TestClassifyError_StaleWorktree verifies the regex matches both canonical
// git error strings for a worktree branch collision. Both phrasings ("already
// used by worktree at" and "already checked out at") appear across git
// versions — the classifier must recognize both.
func TestClassifyError_StaleWorktree(t *testing.T) {
	cases := []struct {
		name string
		msg  string
	}{
		{
			"used by worktree phrasing",
			"worktree add feat/spi-xyz at .worktrees/spi-xyz:\nfatal: 'feat/spi-xyz' is already used by worktree at '/Users/jb/awell/spire/.worktrees/spi-xyz-feature'",
		},
		{
			"checked out at phrasing",
			"fatal: 'feat/spi-abc' is already checked out at '/tmp/spi-abc-feature'",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, sub := ClassifyError(errors.New(tc.msg))
			if class != FailMerge || sub != SubClassStaleWorktree {
				t.Errorf("got (%s, %s), want (%s, %s)",
					class, sub, FailMerge, SubClassStaleWorktree)
			}
		})
	}
}

// TestClassifyError_UnknownDoesNotFalsePositive verifies unrelated errors stay
// unclassified so the general decision path takes over.
func TestClassifyError_UnknownDoesNotFalsePositive(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"generic", errors.New("build failed: cmd exit 1")},
		{"contains 'worktree' but not stale", errors.New("git worktree remove failed")},
		{"contains 'merge' but not race", errors.New("merge conflict in file.go")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, sub := ClassifyError(tc.err)
			if class != FailUnknown || sub != "" {
				t.Errorf("got (%s, %s), want (FailUnknown, \"\")", class, sub)
			}
		})
	}
}

// TestMergeSubClassAction_Mapping verifies the deterministic sub-class → action
// map stays in lockstep with the mechanical-action registry.
func TestMergeSubClassAction_Mapping(t *testing.T) {
	cases := []struct {
		subClass string
		want     string
	}{
		{SubClassMergeRace, "retry-merge"},
		{SubClassStaleWorktree, "cleanup-stale-worktrees"},
		{SubClassPostRebaseFF, "retry-merge"},
		{SubClassMergeConflict, "resolve-conflicts"},
		{"", ""},
		{"unknown-sub", ""},
	}
	for _, tc := range cases {
		t.Run(tc.subClass, func(t *testing.T) {
			got := mergeSubClassAction(tc.subClass)
			if got != tc.want {
				t.Errorf("mergeSubClassAction(%q) = %q, want %q", tc.subClass, got, tc.want)
			}
		})
	}
}

// TestClassifyError_PostRebaseFF verifies that an "ff-only merge failed after
// rebase" error routes to SubClassPostRebaseFF — the signal that main advanced
// again after a successful rebase, recoverable by retrying the merge.
func TestClassifyError_PostRebaseFF(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{
			"direct",
			errors.New("ff-only merge failed after rebase: Not possible to fast-forward"),
		},
		{
			"wrapped",
			fmt.Errorf("step merge: %w", errors.New("ff-only merge failed after rebase: some detail")),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, sub := ClassifyError(tc.err)
			if class != FailMerge || sub != SubClassPostRebaseFF {
				t.Errorf("got (%s, %s), want (%s, %s)", class, sub, FailMerge, SubClassPostRebaseFF)
			}
		})
	}
}

// TestClassifyError_MergeConflict verifies the three phrasings that mark a
// content collision during rebase. Each should route to SubClassMergeConflict
// so Decide dispatches a Worker (resolve-conflicts) instead of a blind retry.
func TestClassifyError_MergeConflict(t *testing.T) {
	cases := []struct {
		name string
		msg  string
	}{
		{
			"rebase conflict in files list",
			"rebase conflict in files: pkg/gateway/gateway_test.go",
		},
		{
			"porcelain UU marker",
			"git status says:\nUU file.go\nrebase stopped",
		},
		{
			"generic rebase conflict phrasing",
			"rebase --continue aborted due to conflict in next commit",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, sub := ClassifyError(errors.New(tc.msg))
			if class != FailMerge || sub != SubClassMergeConflict {
				t.Errorf("got (%s, %s), want (%s, %s)", class, sub, FailMerge, SubClassMergeConflict)
			}
		})
	}
}

// TestClassifyError_MergeRaceTakesPrecedence verifies that when both the
// sentinel ErrMergeRace is present in the chain AND the error text contains
// other patterns, the merge-race classification wins. This pins the priority
// order the classifier applies.
func TestClassifyError_MergeRaceTakesPrecedence(t *testing.T) {
	combined := fmt.Errorf("ff-only merge failed after rebase: %w", git.ErrMergeRace)
	class, sub := ClassifyError(combined)
	if class != FailMerge || sub != SubClassMergeRace {
		t.Errorf("got (%s, %s), want (FailMerge, merge-race)", class, sub)
	}
}
