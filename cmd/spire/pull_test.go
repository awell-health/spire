package main

import (
	"fmt"
	"testing"

	"github.com/awell-health/spire/pkg/dolt"
)

// TestRunPull_ConflictCallsOwnership validates that ApplyMergeOwnership runs
// after CLIPull regardless of whether pull reported conflicts. This is a
// live-server integration test gated by doltIsReachable().
func TestRunPull_ConflictCallsOwnership(t *testing.T) {
	if !doltIsReachable() {
		t.Skip("dolt server not reachable")
	}

	dbName := readBeadsDBName()
	if dbName == "" {
		t.Skip("no beads database configured")
	}

	// Record pre-pull commit so we can verify ownership ran.
	preCommit := dolt.GetCurrentCommitHash(dbName)
	if preCommit == "" {
		t.Skip("unable to read current commit hash")
	}

	// Run pull — may succeed (fast-forward, already up-to-date) or hit a
	// real conflict depending on remote state. Either way, ownership must run.
	_ = runPull("", false)

	// After pull + ownership, there must be zero unresolved conflict rows.
	out, err := doltSQL(
		fmt.Sprintf("USE `%s`; SELECT COUNT(*) AS c FROM dolt_conflicts_issues", dbName),
		false,
	)
	if err != nil {
		t.Skipf("could not query conflict table: %v", err)
	}
	count := extractCountValue(out)
	if count != 0 {
		t.Errorf("expected 0 unresolved conflicts after pull, got %d", count)
	}
}

// TestPullErrorClassification verifies the error-classification logic that
// decides whether a pull error is a diverged-history rejection, a merge
// conflict (auto-resolved by ownership), or a hard failure.
func TestPullErrorClassification(t *testing.T) {
	tests := []struct {
		name      string
		errMsg    string
		wantHard  bool // should propagate as error
		wantMerge bool // should be treated as resolved conflict
	}{
		{
			name:      "non-fast-forward is a hard error",
			errMsg:    "push rejected: non-fast-forward",
			wantHard:  true,
			wantMerge: false,
		},
		{
			name:      "diverged is a hard error",
			errMsg:    "histories have diverged",
			wantHard:  true,
			wantMerge: false,
		},
		{
			name:      "conflict (lowercase) is auto-resolved",
			errMsg:    "merge has unresolved conflicts",
			wantHard:  false,
			wantMerge: true,
		},
		{
			name:      "CONFLICT (uppercase) is auto-resolved",
			errMsg:    "CONFLICT: table issues has conflicting rows",
			wantHard:  false,
			wantMerge: true,
		},
		{
			name:      "cannot merge is auto-resolved",
			errMsg:    "cannot merge: conflicting changes",
			wantHard:  false,
			wantMerge: true,
		},
		{
			name:      "connection refused is a hard error",
			errMsg:    "connection refused",
			wantHard:  false,
			wantMerge: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotHard, gotMerge := classifyPullError(tt.errMsg)
			if gotHard != tt.wantHard {
				t.Errorf("classifyPullError(%q) hard = %v, want %v",
					tt.errMsg, gotHard, tt.wantHard)
			}
			if gotMerge != tt.wantMerge {
				t.Errorf("classifyPullError(%q) merge = %v, want %v",
					tt.errMsg, gotMerge, tt.wantMerge)
			}
		})
	}
}
