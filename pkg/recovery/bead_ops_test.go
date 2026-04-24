package recovery

import (
	"strings"
	"testing"

	"github.com/steveyegge/beads"
)

// fakeOps is an in-memory BeadOps for exercising the dependent-close
// helpers without a live dolt instance.
type fakeOps struct {
	dependents []*beads.IssueWithDependencyMetadata
	comments   map[string][]string
	closed     map[string]bool
}

func newFakeOps(items ...*beads.IssueWithDependencyMetadata) *fakeOps {
	return &fakeOps{
		dependents: items,
		comments:   make(map[string][]string),
		closed:     make(map[string]bool),
	}
}

func (f *fakeOps) GetDependentsWithMeta(id string) ([]*beads.IssueWithDependencyMetadata, error) {
	return f.dependents, nil
}

func (f *fakeOps) AddComment(id, text string) error {
	f.comments[id] = append(f.comments[id], text)
	return nil
}

func (f *fakeOps) CloseBead(id string) error {
	f.closed[id] = true
	return nil
}

// TestCloseRelatedDependents_AlertOnly verifies that
// CloseRelatedDependents with only the alert kind closes alert beads and
// leaves recovery beads alone.
func TestCloseRelatedDependents_AlertOnly(t *testing.T) {
	ops := newFakeOps(
		&beads.IssueWithDependencyMetadata{
			Issue: beads.Issue{
				ID:     "alert-1",
				Status: beads.StatusOpen,
				Labels: []string{"alert:merge-failure"},
			},
			DependencyType: "caused-by",
		},
		&beads.IssueWithDependencyMetadata{
			Issue: beads.Issue{
				ID:     "rec-1",
				Status: beads.StatusOpen,
				Labels: []string{"recovery-bead"},
			},
			DependencyType: "caused-by",
		},
	)

	err := CloseRelatedDependents(ops, "parent", []string{KindAlert}, []string{"caused-by", "recovery-for"}, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ops.closed["alert-1"] {
		t.Error("alert bead should be closed")
	}
	if ops.closed["rec-1"] {
		t.Error("recovery bead should not be closed with kinds=[alert]")
	}
}

// TestCloseRelatedDependents_BothKinds verifies closing both kinds in
// one pass.
func TestCloseRelatedDependents_BothKinds(t *testing.T) {
	ops := newFakeOps(
		&beads.IssueWithDependencyMetadata{
			Issue: beads.Issue{
				ID:     "alert-1",
				Status: beads.StatusOpen,
				Labels: []string{"alert:build-failure"},
			},
			DependencyType: "caused-by",
		},
		&beads.IssueWithDependencyMetadata{
			Issue: beads.Issue{
				ID:     "rec-1",
				Status: beads.StatusOpen,
				Labels: []string{"recovery-bead"},
			},
			DependencyType: "recovery-for",
		},
	)

	err := CloseRelatedDependents(ops, "parent", []string{KindRecovery, KindAlert}, []string{"caused-by", "recovery-for"}, "reset-cycle:3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, id := range []string{"alert-1", "rec-1"} {
		if !ops.closed[id] {
			t.Errorf("%s should be closed", id)
		}
		foundStamp := false
		for _, c := range ops.comments[id] {
			if strings.Contains(c, "reset-cycle:3") {
				foundStamp = true
				break
			}
		}
		if !foundStamp {
			t.Errorf("%s missing reset-cycle:3 comment, got %v", id, ops.comments[id])
		}
	}
}

// TestCloseRelatedDependents_WrongEdgeTypeIgnored verifies that dependents
// linked via non-recovery edges (blocked-by, discovered-from, parent-child)
// are NEVER closed, regardless of kinds. This is the safety invariant
// that prevents the cascade from overreaching to unrelated work.
func TestCloseRelatedDependents_WrongEdgeTypeIgnored(t *testing.T) {
	ops := newFakeOps(
		&beads.IssueWithDependencyMetadata{
			Issue: beads.Issue{
				ID:     "blocker",
				Status: beads.StatusOpen,
				Labels: []string{"alert:something"},
			},
			DependencyType: "blocked-by",
		},
		&beads.IssueWithDependencyMetadata{
			Issue: beads.Issue{
				ID:     "design",
				Status: beads.StatusOpen,
				Labels: []string{"recovery-bead"},
			},
			DependencyType: "discovered-from",
		},
	)

	err := CloseRelatedDependents(ops, "parent", []string{KindRecovery, KindAlert}, []string{"caused-by", "recovery-for"}, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ops.closed["blocker"] {
		t.Error("blocked-by dependent must not be closed — wrong edge type")
	}
	if ops.closed["design"] {
		t.Error("discovered-from dependent must not be closed — wrong edge type")
	}
}

// TestCloseRelatedDependents_ClosedBeadsIgnored verifies already-closed
// dependents are skipped.
func TestCloseRelatedDependents_ClosedBeadsIgnored(t *testing.T) {
	ops := newFakeOps(
		&beads.IssueWithDependencyMetadata{
			Issue: beads.Issue{
				ID:     "alert-closed",
				Status: beads.StatusClosed,
				Labels: []string{"alert:foo"},
			},
			DependencyType: "caused-by",
		},
	)
	err := CloseRelatedDependents(ops, "parent", []string{KindAlert}, []string{"caused-by", "recovery-for"}, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ops.closed["alert-closed"] {
		t.Error("already-closed alert should not be re-closed")
	}
	if len(ops.comments["alert-closed"]) > 0 {
		t.Error("already-closed alert should not receive comments")
	}
}

// TestIsAlertBead_Variants verifies the isAlertBead predicate matches
// both "alert" and "alert:*" labels.
func TestIsAlertBead_Variants(t *testing.T) {
	cases := []struct {
		labels []string
		want   bool
	}{
		{[]string{"alert"}, true},
		{[]string{"alert:merge-failure"}, true},
		{[]string{"alert:dispatch-failure"}, true},
		{[]string{"recovery-bead"}, false},
		{[]string{"some-other-label"}, false},
		{nil, false},
	}
	for _, c := range cases {
		item := &beads.IssueWithDependencyMetadata{
			Issue: beads.Issue{Labels: c.labels},
		}
		if got := isAlertBead(item); got != c.want {
			t.Errorf("isAlertBead(%v) = %v, want %v", c.labels, got, c.want)
		}
	}
}
