package executor

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/steveyegge/beads"
)

// These tests pin the base-branch propagation hardening in wizardPlanEpic:
// a child of an override epic (one carrying a base-branch: label) that fails to
// receive the propagated label must fail the plan step loud, rather than warn
// and produce a child that can later commit against "main"
// (apprentice/submit.go's fallback). Default-branch epics (no override label)
// must be entirely unaffected.

// planEpicDeps wires the minimal Deps to drive wizardPlanEpic through the
// fresh-plan path (no existing children) with a single parsed subtask.
func planEpicDeps(t *testing.T, addLabel func(id, label string) error, captureChildren *[]string) *Deps {
	t.Helper()
	return &Deps{
		ConfigDir:       func() (string, error) { return t.TempDir(), nil },
		GetComments:     func(id string) ([]*beads.Comment, error) { return nil, nil },
		GetDepsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) { return nil, nil },
		GetChildren:     func(parentID string) ([]Bead, error) { return nil, nil }, // fresh plan
		AddComment:      func(id, text string) error { return nil },
		ClaudeRunner: func(args []string, dir string, _ io.Writer) ([]byte, error) {
			// One subtask line in the JSON-per-line format wizardPlanEpic parses.
			return []byte(`{"title":"Task A","description":"do A"}`), nil
		},
		CreateBead: func(opts CreateOpts) (string, error) {
			id := opts.Parent + ".1"
			if captureChildren != nil {
				*captureChildren = append(*captureChildren, id)
			}
			return id, nil
		},
		ParseIssueType:    func(s string) beads.IssueType { return beads.IssueType(s) },
		AddDep:            func(issueID, dependsOnID string) error { return nil },
		AddLabel:          addLabel,
		IsAttemptBead:     func(b Bead) bool { return false },
		IsStepBead:        func(b Bead) bool { return false },
		IsReviewRoundBead: func(b Bead) bool { return false },
	}
}

// TestWizardPlanEpic_BaseBranchPropagationFailsLoud: override epic + AddLabel
// always errors → wizardPlanEpic returns an error naming base-branch.
func TestWizardPlanEpic_BaseBranchPropagationFailsLoud(t *testing.T) {
	deps := planEpicDeps(t, func(id, label string) error {
		return errors.New("store unavailable")
	}, nil)
	e := NewForTest("spi-epic", "wizard", &State{RepoPath: t.TempDir()}, deps)

	bead := Bead{ID: "spi-epic", Title: "Epic", Type: "epic", Labels: []string{"base-branch:release-1"}}
	err := e.wizardPlanEpic(bead, "claude-opus-4-6", 0)
	if err == nil {
		t.Fatal("wizardPlanEpic returned nil, want a base-branch propagation error")
	}
	if !strings.Contains(err.Error(), "base-branch") {
		t.Errorf("err = %q, want it to mention base-branch", err)
	}
}

// TestWizardPlanEpic_BaseBranchPropagationRetrySucceeds: AddLabel fails once
// then succeeds (transient blip) → no error.
func TestWizardPlanEpic_BaseBranchPropagationRetrySucceeds(t *testing.T) {
	calls := 0
	deps := planEpicDeps(t, func(id, label string) error {
		calls++
		if calls == 1 {
			return errors.New("transient")
		}
		return nil
	}, nil)
	e := NewForTest("spi-epic", "wizard", &State{RepoPath: t.TempDir()}, deps)

	bead := Bead{ID: "spi-epic", Title: "Epic", Type: "epic", Labels: []string{"base-branch:release-1"}}
	if err := e.wizardPlanEpic(bead, "claude-opus-4-6", 0); err != nil {
		t.Fatalf("wizardPlanEpic: %v (retry should absorb the transient failure)", err)
	}
	if calls != 2 {
		t.Errorf("AddLabel calls = %d, want 2 (one failure + one retry)", calls)
	}
}

// TestWizardPlanEpic_OverrideEpic_PropagatesLabelToChild: the child receives the
// epic's base-branch label verbatim.
func TestWizardPlanEpic_OverrideEpic_PropagatesLabelToChild(t *testing.T) {
	var applied []string
	deps := planEpicDeps(t, func(id, label string) error {
		applied = append(applied, id+"="+label)
		return nil
	}, nil)
	e := NewForTest("spi-epic", "wizard", &State{RepoPath: t.TempDir()}, deps)

	bead := Bead{ID: "spi-epic", Title: "Epic", Type: "epic", Labels: []string{"base-branch:release-1"}}
	if err := e.wizardPlanEpic(bead, "claude-opus-4-6", 0); err != nil {
		t.Fatalf("wizardPlanEpic: %v", err)
	}
	want := "spi-epic.1=base-branch:release-1"
	found := false
	for _, a := range applied {
		if a == want {
			found = true
		}
	}
	if !found {
		t.Errorf("applied labels = %v, want to include %q", applied, want)
	}
}

// TestWizardPlanEpic_Resume_BackfillsBaseBranchLabel: on the resume/enrich path
// (children already exist), an override-epic child that is missing its
// base-branch label gets it backfilled.
func TestWizardPlanEpic_Resume_BackfillsBaseBranchLabel(t *testing.T) {
	var applied []string
	deps := planEpicDeps(t, func(id, label string) error {
		applied = append(applied, id+"="+label)
		return nil
	}, nil)
	// Existing child with NO base-branch label → resume/enrich path.
	deps.GetChildren = func(parentID string) ([]Bead, error) {
		return []Bead{{ID: parentID + ".1", Title: "A"}}, nil
	}
	e := NewForTest("spi-epic", "wizard", &State{RepoPath: t.TempDir()}, deps)

	bead := Bead{ID: "spi-epic", Title: "Epic", Type: "epic", Labels: []string{"base-branch:release-1"}}
	if err := e.wizardPlanEpic(bead, "claude-opus-4-6", 0); err != nil {
		t.Fatalf("wizardPlanEpic: %v", err)
	}
	want := "spi-epic.1=base-branch:release-1"
	found := false
	for _, a := range applied {
		if a == want {
			found = true
		}
	}
	if !found {
		t.Errorf("applied labels = %v, want to include %q (resume must backfill)", applied, want)
	}
}

// TestWizardPlanEpic_Resume_SkipsAlreadyLabeledChild: a child that already
// carries the base-branch label is not re-labeled on resume (idempotent skip).
func TestWizardPlanEpic_Resume_SkipsAlreadyLabeledChild(t *testing.T) {
	var applied []string
	deps := planEpicDeps(t, func(id, label string) error {
		applied = append(applied, id+"="+label)
		return nil
	}, nil)
	deps.GetChildren = func(parentID string) ([]Bead, error) {
		return []Bead{{ID: parentID + ".1", Title: "A", Labels: []string{"base-branch:release-1"}}}, nil
	}
	e := NewForTest("spi-epic", "wizard", &State{RepoPath: t.TempDir()}, deps)

	bead := Bead{ID: "spi-epic", Title: "Epic", Type: "epic", Labels: []string{"base-branch:release-1"}}
	if err := e.wizardPlanEpic(bead, "claude-opus-4-6", 0); err != nil {
		t.Fatalf("wizardPlanEpic: %v", err)
	}
	for _, a := range applied {
		if a == "spi-epic.1=base-branch:release-1" {
			t.Errorf("backfill re-labeled an already-labeled child: %v", applied)
		}
	}
}

// TestWizardPlanEpic_DefaultEpic_NoLabel_NotAffected: a default-branch epic
// (no base-branch label) never enters the propagation block, so even an
// AddLabel that would error does not fail the plan. This is the zero-regression
// guard for the common case.
func TestWizardPlanEpic_DefaultEpic_NoLabel_NotAffected(t *testing.T) {
	deps := planEpicDeps(t, func(id, label string) error {
		return errors.New("would error if called for base-branch")
	}, nil)
	e := NewForTest("spi-epic", "wizard", &State{RepoPath: t.TempDir()}, deps)

	bead := Bead{ID: "spi-epic", Title: "Epic", Type: "epic"} // no base-branch label
	if err := e.wizardPlanEpic(bead, "claude-opus-4-6", 0); err != nil {
		t.Fatalf("wizardPlanEpic: %v (default-branch epic must be unaffected by the hardening)", err)
	}
}
