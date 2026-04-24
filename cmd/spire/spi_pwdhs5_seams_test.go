package main

// Tests for spi-pwdhs5: lifecycle seams (sage→merge handoff, reset cascade
// completeness, bead-derived prefix). Each test is tagged with its
// spi-1dk71j seam number for traceability.

import (
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/executor"
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

// --- Seam 11 / 15 (Outcome → Step rewind, Wizard dead → Wizard resumed):
// orphan-attempt reconciler

// fakeOrphanSeams constructs a seams struct backed by in-memory fixtures.
type fakeOrphanSeams struct {
	children    map[string][]Bead
	list        []Bead
	graphStates map[string]*executor.GraphState
	alivePIDs   map[int]bool
	// capture side effects for assertions
	labels   map[string][]string
	comments map[string][]string
	closed   map[string]bool
}

func newFakeOrphanSeams() *fakeOrphanSeams {
	return &fakeOrphanSeams{
		children:    make(map[string][]Bead),
		graphStates: make(map[string]*executor.GraphState),
		alivePIDs:   make(map[int]bool),
		labels:      make(map[string][]string),
		comments:    make(map[string][]string),
		closed:      make(map[string]bool),
	}
}

func (f *fakeOrphanSeams) toSeams() orphanReconcilerSeams {
	return orphanReconcilerSeams{
		GetChildren: func(parentID string) ([]Bead, error) {
			return f.children[parentID], nil
		},
		ListBeads: func(filter beads.IssueFilter) ([]Bead, error) {
			return f.list, nil
		},
		LoadGraphState: func(agentName string) (*executor.GraphState, error) {
			return f.graphStates[agentName], nil
		},
		AddLabel: func(beadID, label string) error {
			f.labels[beadID] = append(f.labels[beadID], label)
			return nil
		},
		AddComment: func(beadID, text string) error {
			f.comments[beadID] = append(f.comments[beadID], text)
			return nil
		},
		CloseBead: func(beadID string) error {
			f.closed[beadID] = true
			return nil
		},
		ProcessAliveCheck: func(pid int) bool {
			return f.alivePIDs[pid]
		},
	}
}

// TestSeam11_ReconcileOrphanAttempts_DeadWizardNoStateCloses verifies the
// canonical orphan case: wizard registry shows no live process AND no
// graph_state.json on disk → attempt is closed with interrupted:orphan.
func TestSeam11_ReconcileOrphanAttempts_DeadWizardNoStateCloses(t *testing.T) {
	f := newFakeOrphanSeams()
	// Seed an orphaned attempt bead under spi-parent.
	f.children["spi-parent"] = []Bead{
		{
			ID:     "spi-parent.1",
			Status: "in_progress",
			Parent: "spi-parent",
			Title:  "attempt: wizard-spi-parent",
			Labels: []string{"attempt", "attempt:1", "agent:wizard-spi-parent"},
		},
	}
	// Dead wizard: pid 99999 not in alive set; no graph state.
	reg := wizardRegistry{Wizards: []localWizard{{Name: "wizard-spi-parent", PID: 99999, BeadID: "spi-parent"}}}

	reconcileOrphanAttemptsWithSeams([]string{"spi-parent"}, reg, f.toSeams())

	if !f.closed["spi-parent.1"] {
		t.Fatalf("expected orphan attempt closed, closed=%v", f.closed)
	}
	found := false
	for _, l := range f.labels["spi-parent.1"] {
		if l == "interrupted:orphan" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected interrupted:orphan label on orphaned attempt, got %v",
			f.labels["spi-parent.1"])
	}
}

// TestSeam15_ReconcileOrphanAttempts_LiveWizardSkipped verifies strict
// additivity gate A: a live wizard's attempt is NEVER closed by the
// reconciler, even when graph state is missing. False positives here
// cause real data loss.
func TestSeam15_ReconcileOrphanAttempts_LiveWizardSkipped(t *testing.T) {
	f := newFakeOrphanSeams()
	f.children["spi-live"] = []Bead{
		{
			ID:     "spi-live.1",
			Status: "in_progress",
			Parent: "spi-live",
			Labels: []string{"attempt", "attempt:1", "agent:wizard-spi-live"},
		},
	}
	// Mark the wizard as alive via the pid alive-check seam.
	f.alivePIDs[4242] = true
	reg := wizardRegistry{Wizards: []localWizard{{Name: "wizard-spi-live", PID: 4242, BeadID: "spi-live"}}}

	reconcileOrphanAttemptsWithSeams([]string{"spi-live"}, reg, f.toSeams())

	if f.closed["spi-live.1"] {
		t.Error("attempt under a LIVE wizard must not be closed — false positive causes data loss")
	}
}

// TestSeam15_ReconcileOrphanAttempts_GraphStateExistsSkipped verifies
// strict additivity gate B: even a dead wizard is NOT treated as orphan
// if graph state exists on disk. The state means the wizard might resume.
func TestSeam15_ReconcileOrphanAttempts_GraphStateExistsSkipped(t *testing.T) {
	f := newFakeOrphanSeams()
	f.children["spi-resume"] = []Bead{
		{
			ID:     "spi-resume.1",
			Status: "in_progress",
			Parent: "spi-resume",
			Labels: []string{"attempt", "agent:wizard-spi-resume"},
		},
	}
	// Wizard is dead (empty live set) BUT graph state exists.
	f.graphStates["wizard-spi-resume"] = &executor.GraphState{BeadID: "spi-resume", ActiveStep: "implement"}
	reg := wizardRegistry{}

	reconcileOrphanAttemptsWithSeams([]string{"spi-resume"}, reg, f.toSeams())

	if f.closed["spi-resume.1"] {
		t.Error("attempt with existing graph state must not be closed — wizard may resume")
	}
}

// TestSeam11_ReconcileOrphanAttempts_Idempotent verifies calling the
// reconciler twice closes nothing new on the second run. The first pass
// closes the orphan; the second sees the closed attempt and skips it.
func TestSeam11_ReconcileOrphanAttempts_Idempotent(t *testing.T) {
	f := newFakeOrphanSeams()
	f.children["spi-idem"] = []Bead{
		{
			ID:     "spi-idem.1",
			Status: "in_progress",
			Parent: "spi-idem",
			Labels: []string{"attempt", "agent:wizard-spi-idem"},
		},
	}
	reg := wizardRegistry{}

	// Update seam to reflect closure between passes.
	seams := f.toSeams()
	origClose := seams.CloseBead
	seams.CloseBead = func(id string) error {
		if err := origClose(id); err != nil {
			return err
		}
		// Simulate the DB — mark status closed so subsequent GetChildren reads reflect it.
		for i, c := range f.children["spi-idem"] {
			if c.ID == id {
				f.children["spi-idem"][i].Status = "closed"
			}
		}
		return nil
	}

	reconcileOrphanAttemptsWithSeams([]string{"spi-idem"}, reg, seams)
	firstClosedCount := len(f.closed)

	// Second run — should not re-close.
	before := make(map[string]bool, len(f.closed))
	for k, v := range f.closed {
		before[k] = v
	}
	reconcileOrphanAttemptsWithSeams([]string{"spi-idem"}, reg, seams)
	if len(f.closed) != firstClosedCount {
		t.Errorf("second run changed closed set (idempotency broken): before=%v after=%v",
			before, f.closed)
	}
}

// TestSeam11_ReconcileOrphanAttempts_IgnoresClosedAttempts verifies
// already-closed attempt beads are not touched. Closed beads are already
// reconciled and double-labeling would introduce noise.
func TestSeam11_ReconcileOrphanAttempts_IgnoresClosedAttempts(t *testing.T) {
	f := newFakeOrphanSeams()
	f.children["spi-closed"] = []Bead{
		{
			ID:     "spi-closed.1",
			Status: "closed",
			Parent: "spi-closed",
			Labels: []string{"attempt", "agent:wizard-spi-closed"},
		},
	}
	reg := wizardRegistry{}

	reconcileOrphanAttemptsWithSeams([]string{"spi-closed"}, reg, f.toSeams())

	if f.closed["spi-closed.1"] {
		t.Error("already-closed attempt should not be re-closed")
	}
	if len(f.labels["spi-closed.1"]) > 0 {
		t.Errorf("already-closed attempt should not have labels added, got %v", f.labels["spi-closed.1"])
	}
}
