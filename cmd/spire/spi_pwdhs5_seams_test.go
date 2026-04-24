package main

// Tests for spi-pwdhs5: lifecycle seams (sage→merge handoff, reset cascade
// completeness, bead-derived prefix). Each test is tagged with its
// spi-1dk71j seam number for traceability.

import (
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// --- Seam 6 (Dispatch → Mechanical fn): bead-derived prefix helper ---

// TestSeam6_PrefixFromID covers every canonical prefix shape supported by
// store.PrefixFromID — Bug C's fix depends on this producing a stable,
// non-empty result for well-formed bead IDs. Empty/malformed inputs fall
// through to empty-string (the caller's programmer-error case).
func TestSeam6_PrefixFromID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"spi-pwdhs5", "spi"},
		{"oo-b9u", "oo"},
		{"spd-abc123", "spd"},
		{"spi-a3f8.1", "spi"},     // hierarchical IDs
		{"spi-a3f8.1.2.3", "spi"}, // deeper hierarchy
		{"hub-xyz", "hub"},
		{"", ""},             // empty input → empty prefix
		{"nothash", ""},      // no separator
		{"-leadingdash", ""}, // malformed
	}
	for _, c := range cases {
		got := store.PrefixFromID(c.in)
		if got != c.want {
			t.Errorf("PrefixFromID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- Seam 5 (Decide → Execute dispatch): resolver uses bead prefix ---

// TestSeam5_ResolveGraphStateStoreForBead_EmptyBeadIDRejected asserts the
// per-bead resolver refuses an empty beadID rather than silently falling
// back to the ambiguous empty-prefix form. This is the core of Bug C:
// before the fix, buildExecutorDepsForBead called
// resolveGraphStateStoreOrLocal("") even though beadID was in scope, which
// on a multi-prefix tower fell back to local-only mode and silently lost
// cluster-mode writes.
func TestSeam5_ResolveGraphStateStoreForBead_EmptyBeadIDRejected(t *testing.T) {
	_, err := resolveGraphStateStoreForBead("")
	if err == nil {
		t.Fatal("expected error for empty beadID, got nil (programmer error must fail loudly)")
	}
	if !strings.Contains(err.Error(), "beadID must be non-empty") {
		t.Errorf("error should mention 'beadID must be non-empty', got %q", err.Error())
	}
}

// TestSeam5_ResolveGraphStateStoreForBeadOrLocal_EmptyBeadIDPanics asserts
// the non-error-returning per-bead form panics on empty beadID. This is
// the form called inside buildExecutorDepsForBead where the struct
// literal cannot take an error — the fail-loudly contract is enforced
// via panic.
func TestSeam5_ResolveGraphStateStoreForBeadOrLocal_EmptyBeadIDPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for empty beadID, got none")
		}
	}()
	_ = resolveGraphStateStoreForBeadOrLocal("")
}

// --- Seam 3 (Step failure → recovery cycle start): alert cascade ---

// fakeBeadOps is an in-memory BeadOps for exercising
// CloseRelatedDependents without a live dolt instance.
type fakeBeadOps struct {
	dependents []*beads.IssueWithDependencyMetadata
	comments   map[string][]string
	closed     map[string]bool
	closeErr   error
}

func (f *fakeBeadOps) GetDependentsWithMeta(id string) ([]*beads.IssueWithDependencyMetadata, error) {
	return f.dependents, nil
}

func (f *fakeBeadOps) AddComment(id, text string) error {
	if f.comments == nil {
		f.comments = make(map[string][]string)
	}
	f.comments[id] = append(f.comments[id], text)
	return nil
}

func (f *fakeBeadOps) CloseBead(id string) error {
	if f.closeErr != nil {
		return f.closeErr
	}
	if f.closed == nil {
		f.closed = make(map[string]bool)
	}
	f.closed[id] = true
	return nil
}

// TestSeam3_CloseRelatedDependents_ClosesAlertBeads verifies Bug B fix:
// CloseRelatedDependents with kinds=[recovery,alert] closes both kinds of
// dependents in one pass. Prior behavior (CloseRelatedRecoveryBeads)
// missed alerts and left them orphaned on the board after reset.
func TestSeam3_CloseRelatedDependents_ClosesAlertBeads(t *testing.T) {
	ops := &fakeBeadOps{
		dependents: []*beads.IssueWithDependencyMetadata{
			{
				Issue: beads.Issue{
					ID:     "spi-alert1",
					Status: beads.StatusOpen,
					Labels: []string{"alert:merge-failure"},
				},
				DependencyType: "caused-by",
			},
			{
				Issue: beads.Issue{
					ID:     "spi-alert2",
					Status: beads.StatusOpen,
					Labels: []string{"alert:dispatch-failure"},
				},
				DependencyType: "caused-by",
			},
			{
				Issue: beads.Issue{
					ID:     "spi-rec1",
					Status: beads.StatusOpen,
					Labels: []string{"recovery-bead"},
				},
				DependencyType: "caused-by",
			},
			{
				// Closed already — should NOT be re-closed.
				Issue: beads.Issue{
					ID:     "spi-alert-closed",
					Status: beads.StatusClosed,
					Labels: []string{"alert:stale"},
				},
				DependencyType: "caused-by",
			},
			{
				// Unrelated dependent — wrong edge type, should not be touched.
				Issue: beads.Issue{
					ID:     "spi-other",
					Status: beads.StatusOpen,
					Labels: []string{"alert:foo"},
				},
				DependencyType: "blocked-by",
			},
		},
	}

	err := recovery.CloseRelatedDependents(ops, "spi-parent",
		[]string{recovery.KindRecovery, recovery.KindAlert}, []string{"caused-by", "recovery-for"}, "reset-cycle:2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Both open alerts and the recovery bead should be closed.
	want := []string{"spi-alert1", "spi-alert2", "spi-rec1"}
	for _, id := range want {
		if !ops.closed[id] {
			t.Errorf("expected %s closed, but it was not", id)
		}
	}
	// Already-closed and wrong-edge-type beads should NOT be re-closed.
	if ops.closed["spi-alert-closed"] {
		t.Error("closed-already bead should not be re-closed")
	}
	if ops.closed["spi-other"] {
		t.Error("wrong-edge-type dependent should not be closed")
	}

	// Every closed bead must have a "Resolved: reset-cycle:2" comment.
	for _, id := range want {
		found := false
		for _, c := range ops.comments[id] {
			if strings.Contains(c, "reset-cycle:2") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %s to have reset-cycle:2 stamp comment, got %v",
				id, ops.comments[id])
		}
	}
}

// TestSeam3_CloseRelatedDependents_RecoveryOnly verifies backwards-
// compatible behavior: kinds=[recovery] closes only recovery beads.
// This is the success-path call from graph_actions.go where we don't
// want alerts closed on happy-path completion.
func TestSeam3_CloseRelatedDependents_RecoveryOnly(t *testing.T) {
	ops := &fakeBeadOps{
		dependents: []*beads.IssueWithDependencyMetadata{
			{
				Issue: beads.Issue{
					ID:     "spi-rec1",
					Status: beads.StatusOpen,
					Labels: []string{"recovery-bead"},
				},
				DependencyType: "caused-by",
			},
			{
				Issue: beads.Issue{
					ID:     "spi-alert1",
					Status: beads.StatusOpen,
					Labels: []string{"alert:merge-failure"},
				},
				DependencyType: "caused-by",
			},
		},
	}

	err := recovery.CloseRelatedDependents(ops, "spi-parent",
		[]string{recovery.KindRecovery}, []string{"caused-by", "recovery-for"}, "completed")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ops.closed["spi-rec1"] {
		t.Error("recovery bead should be closed")
	}
	if ops.closed["spi-alert1"] {
		t.Error("alert bead should NOT be closed when kinds=[recovery] only")
	}
}

// --- Bug B (reset cascade): buildProtectedBeadIDs narrowed to recovery ---

// TestBugB_BuildProtectedBeadIDs_DoesNotProtectAlerts verifies that alert
// beads fall through to the reset cascade — prior behavior protected them
// and they accumulated indefinitely across reset cycles.
func TestBugB_BuildProtectedBeadIDs_DoesNotProtectAlerts(t *testing.T) {
	// Seed a child with only an alert label (no recovery-bead label) —
	// buildProtectedBeadIDs uses isProtectedByLabel as the label-path check.
	alert := Bead{ID: "spi-alert", Labels: []string{"alert:merge-failure"}}
	recoveryChild := Bead{ID: "spi-rec", Labels: []string{"recovery-bead"}}

	// With the narrowed isProtectedByLabel, only the recovery bead should
	// be protected by label.
	if isProtectedByLabel(alert) {
		t.Error("alert-labeled bead should NOT be protected after Bug B fix")
	}
	if !isProtectedByLabel(recoveryChild) {
		t.Error("recovery-bead-labeled bead must still be protected")
	}
}

// Note: seams 11 and 15 (orphan-attempt reconciler) were removed in Phase 3 of the
// lifecycle-boundaries refactor (spi-pbuhit). The reconcileOrphanAttempts/
// reconcileOrphanAttemptsWithSeams helpers were deleted from cmd/spire/summon.go
// because pkg/beadlifecycle.OrphanSweep now owns this responsibility. Tests for
// OrphanSweep live in pkg/beadlifecycle/lifecycle_test.go.
