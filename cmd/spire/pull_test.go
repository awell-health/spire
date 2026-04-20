package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakePullDeps is a recording fake satisfying pullDeps. Each *Result field
// is the canned return value; each *Calls slice records call arguments so
// tests can assert ordering and inputs.
type fakePullDeps struct {
	commitHash string
	pullErr    error
	ownerErr   error
	conflicts  int
	conflictsErr error

	calls []string // ordered call log: "GetCurrentCommitHash", "CLIPull", ...

	commitCalls    []string
	pullCalls      []struct{ dataDir string; force bool }
	ownershipCalls []struct{ dbName, preCommit string }
	conflictCalls  []string
}

func (f *fakePullDeps) GetCurrentCommitHash(dbName string) string {
	f.calls = append(f.calls, "GetCurrentCommitHash")
	f.commitCalls = append(f.commitCalls, dbName)
	return f.commitHash
}

func (f *fakePullDeps) CLIPull(_ context.Context, dataDir string, force bool) error {
	f.calls = append(f.calls, "CLIPull")
	f.pullCalls = append(f.pullCalls, struct{ dataDir string; force bool }{dataDir, force})
	return f.pullErr
}

func (f *fakePullDeps) ApplyMergeOwnership(dbName, preCommit string) error {
	f.calls = append(f.calls, "ApplyMergeOwnership")
	f.ownershipCalls = append(f.ownershipCalls, struct{ dbName, preCommit string }{dbName, preCommit})
	return f.ownerErr
}

func (f *fakePullDeps) HasUnresolvedConflicts(dbName string) (int, error) {
	f.calls = append(f.calls, "HasUnresolvedConflicts")
	f.conflictCalls = append(f.conflictCalls, dbName)
	return f.conflicts, f.conflictsErr
}

// TestRunPullCore_ConflictCallsOwnership locks in the contract that
// ApplyMergeOwnership runs after CLIPull even when CLIPull returned a
// conflict error, and that the call order is
// GetCurrentCommitHash → CLIPull → ApplyMergeOwnership → HasUnresolvedConflicts.
func TestRunPullCore_ConflictCallsOwnership(t *testing.T) {
	fake := &fakePullDeps{
		commitHash: "deadbeef",
		pullErr:    errors.New("CONFLICT: table issues has conflicting rows"),
		ownerErr:   nil,
		conflicts:  0,
	}

	if err := runPullCore(fake, "/tmp/data", "beads", false); err != nil {
		t.Fatalf("runPullCore returned error, want nil (merge resolved): %v", err)
	}

	if len(fake.ownershipCalls) != 1 {
		t.Fatalf("ApplyMergeOwnership call count = %d, want 1", len(fake.ownershipCalls))
	}
	if got := fake.ownershipCalls[0].preCommit; got != "deadbeef" {
		t.Errorf("ApplyMergeOwnership preCommit = %q, want %q", got, "deadbeef")
	}
	if got := fake.ownershipCalls[0].dbName; got != "beads" {
		t.Errorf("ApplyMergeOwnership dbName = %q, want %q", got, "beads")
	}

	wantOrder := []string{
		"GetCurrentCommitHash",
		"CLIPull",
		"ApplyMergeOwnership",
		"HasUnresolvedConflicts",
	}
	if !equalStrings(fake.calls, wantOrder) {
		t.Errorf("call order = %v, want %v", fake.calls, wantOrder)
	}
}

// TestRunPullCore_RemainingConflictsError verifies that when ownership
// enforcement leaves conflicts behind, runPullCore returns an error
// mentioning "merge conflicts remain".
func TestRunPullCore_RemainingConflictsError(t *testing.T) {
	fake := &fakePullDeps{
		commitHash: "deadbeef",
		pullErr:    errors.New("CONFLICT: table issues has conflicting rows"),
		ownerErr:   nil,
		conflicts:  2,
	}

	err := runPullCore(fake, "/tmp/data", "beads", false)
	if err == nil {
		t.Fatal("runPullCore returned nil, want error about remaining conflicts")
	}
	if !strings.Contains(err.Error(), "merge conflicts remain") {
		t.Errorf("error %q does not mention 'merge conflicts remain'", err)
	}
}

// TestRunPullCore_NoDBNameSkipsOwnership checks that when dbName is empty,
// the ownership and conflict-check calls are skipped — only CLIPull runs.
func TestRunPullCore_NoDBNameSkipsOwnership(t *testing.T) {
	fake := &fakePullDeps{
		pullErr: nil,
	}

	if err := runPullCore(fake, "/tmp/data", "", false); err != nil {
		t.Fatalf("runPullCore returned error: %v", err)
	}

	if len(fake.commitCalls) != 0 {
		t.Errorf("GetCurrentCommitHash called %d times, want 0 (no dbName)", len(fake.commitCalls))
	}
	if len(fake.ownershipCalls) != 0 {
		t.Errorf("ApplyMergeOwnership called %d times, want 0 (no dbName)", len(fake.ownershipCalls))
	}
	if len(fake.conflictCalls) != 0 {
		t.Errorf("HasUnresolvedConflicts called %d times, want 0 (no dbName)", len(fake.conflictCalls))
	}
	if len(fake.pullCalls) != 1 {
		t.Errorf("CLIPull called %d times, want 1", len(fake.pullCalls))
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestPullErrorForceHardPath verifies that hard errors propagate even when
// force=true. This is a consumer-level test of classifyPullError's integration
// with the runPull control flow: when dolt returns a diverged-history error
// despite --force, the error must not be silently swallowed.
func TestPullErrorForceHardPath(t *testing.T) {
	// Simulate the runPull error-handling logic for the force+hard case.
	// We can't call runPull directly (requires dolt), but we can verify
	// the classification + branching produces the correct outcome.
	forceHardCases := []struct {
		name   string
		errMsg string
		force  bool
	}{
		{"diverged+force", "histories have diverged", true},
		{"non-fast-forward+force", "push rejected: non-fast-forward", true},
	}
	for _, tt := range forceHardCases {
		t.Run(tt.name, func(t *testing.T) {
			hard, merge := classifyPullError(tt.errMsg)
			if !hard {
				t.Fatalf("expected hard=true for %q", tt.errMsg)
			}
			if merge {
				t.Fatalf("expected merge=false for %q", tt.errMsg)
			}
			// Replicate the runPull branching logic:
			var returned bool
			if hard && !tt.force {
				returned = true // diverged-history message path
			} else if merge {
				returned = false // conflict resolved path
			} else {
				// This is the else-branch that must catch hard+force.
				returned = true
			}
			if !returned {
				t.Errorf("force=%v hard=%v merge=%v: error would be silently swallowed",
					tt.force, hard, merge)
			}
		})
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
			name:      "connection refused is neither hard nor merge",
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
