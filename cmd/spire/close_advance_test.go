package main

import (
	"errors"
	"reflect"
	"testing"

	"github.com/steveyegge/beads"
)

func TestCmdCloseAlreadyClosedParentClosesDirectWorkflowSteps(t *testing.T) {
	restore := stubCloseStore(t)
	defer restore()

	storeGetBeadFunc = func(id string) (Bead, error) {
		if id != "spi-parent" {
			return Bead{}, errors.New("unexpected bead")
		}
		return Bead{ID: id, Status: string(beads.StatusClosed), Labels: []string{"phase:close"}}, nil
	}
	storeGetChildrenFunc = func(parentID string) ([]Bead, error) {
		if parentID != "spi-parent" {
			return nil, nil
		}
		return []Bead{
			{ID: "spi-step-open", Status: string(beads.StatusOpen), Labels: []string{"workflow-step", "step:review"}},
			{ID: "spi-step-active", Status: "in_progress", Labels: []string{"workflow-step", "step:implement"}},
			{ID: "spi-step-closed", Status: string(beads.StatusClosed), Labels: []string{"workflow-step", "step:plan"}},
			{ID: "spi-real-child", Status: string(beads.StatusOpen), Labels: []string{"gateway"}},
		}, nil
	}

	var closed []string
	storeCloseBeadFunc = func(id string) error {
		closed = append(closed, id)
		return nil
	}

	if err := cmdClose([]string{"spi-parent"}); err != nil {
		t.Fatalf("cmdClose: %v", err)
	}

	want := []string{"spi-step-open", "spi-step-active"}
	if !reflect.DeepEqual(closed, want) {
		t.Fatalf("closed IDs = %v, want %v", closed, want)
	}
}

func TestCmdCloseOpenParentClosesDirectWorkflowStepsBeforeParent(t *testing.T) {
	restore := stubCloseStore(t)
	defer restore()

	storeGetBeadFunc = func(id string) (Bead, error) {
		if id != "spi-parent" {
			return Bead{}, errors.New("unexpected bead")
		}
		return Bead{ID: id, Status: string(beads.StatusOpen)}, nil
	}
	storeGetChildrenFunc = func(parentID string) ([]Bead, error) {
		if parentID != "spi-parent" {
			return nil, nil
		}
		return []Bead{
			{ID: "spi-step-implement", Status: "in_progress", Labels: []string{"workflow-step", "step:implement"}},
		}, nil
	}

	var closed []string
	storeCloseBeadFunc = func(id string) error {
		closed = append(closed, id)
		return nil
	}

	if err := cmdClose([]string{"spi-parent"}); err != nil {
		t.Fatalf("cmdClose: %v", err)
	}

	want := []string{"spi-step-implement", "spi-parent"}
	if !reflect.DeepEqual(closed, want) {
		t.Fatalf("closed IDs = %v, want %v", closed, want)
	}
}

func TestCmdCloseMoleculeChildrenUseCloseLifecycle(t *testing.T) {
	restore := stubCloseStore(t)
	defer restore()

	storeGetBeadFunc = func(id string) (Bead, error) {
		if id != "spi-parent" {
			return Bead{}, errors.New("unexpected bead")
		}
		return Bead{ID: id, Status: string(beads.StatusOpen)}, nil
	}
	storeListBeadsFunc = func(filter beads.IssueFilter) ([]Bead, error) {
		if reflect.DeepEqual(filter.Labels, []string{"workflow:spi-parent"}) {
			return []Bead{{ID: "spi-molecule", Status: string(beads.StatusOpen), Labels: []string{"workflow:spi-parent"}}}, nil
		}
		return nil, nil
	}
	storeGetChildrenFunc = func(parentID string) ([]Bead, error) {
		if parentID != "spi-molecule" {
			return nil, nil
		}
		return []Bead{
			{ID: "spi-molecule-step", Status: string(beads.StatusOpen), Labels: []string{"workflow-step", "step:close"}},
		}, nil
	}

	var closed []string
	storeCloseBeadFunc = func(id string) error {
		closed = append(closed, id)
		return nil
	}

	if err := cmdClose([]string{"spi-parent"}); err != nil {
		t.Fatalf("cmdClose: %v", err)
	}

	want := []string{"spi-molecule-step", "spi-molecule", "spi-parent"}
	if !reflect.DeepEqual(closed, want) {
		t.Fatalf("closed IDs = %v, want %v", closed, want)
	}
}

func TestStoreCloseStepBeadSkipsAlreadyClosedStep(t *testing.T) {
	origGetBead := storeGetBeadFunc
	defer func() { storeGetBeadFunc = origGetBead }()

	storeGetBeadFunc = func(id string) (Bead, error) {
		return Bead{ID: id, Status: string(beads.StatusClosed)}, nil
	}

	if err := storeCloseStepBead("spi-step-closed"); err != nil {
		t.Fatalf("storeCloseStepBead: %v", err)
	}
}

func stubCloseStore(t *testing.T) func() {
	t.Helper()

	origGetBead := storeGetBeadFunc
	origGetChildren := storeGetChildrenFunc
	origListBeads := storeListBeadsFunc
	origClose := storeCloseBeadFunc
	origRemoveLabel := storeRemoveLabelFunc
	origDependents := storeGetDependentsWithMetaFunc

	storeGetBeadFunc = func(id string) (Bead, error) { return Bead{ID: id, Status: string(beads.StatusOpen)}, nil }
	storeGetChildrenFunc = func(string) ([]Bead, error) { return nil, nil }
	storeListBeadsFunc = func(beads.IssueFilter) ([]Bead, error) { return nil, nil }
	storeCloseBeadFunc = func(string) error { return nil }
	storeRemoveLabelFunc = func(string, string) error { return nil }
	storeGetDependentsWithMetaFunc = func(string) ([]*beads.IssueWithDependencyMetadata, error) { return nil, nil }

	return func() {
		storeGetBeadFunc = origGetBead
		storeGetChildrenFunc = origGetChildren
		storeListBeadsFunc = origListBeads
		storeCloseBeadFunc = origClose
		storeRemoveLabelFunc = origRemoveLabel
		storeGetDependentsWithMetaFunc = origDependents
	}
}
