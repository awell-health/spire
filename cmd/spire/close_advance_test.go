package main

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/store"
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
	origTower := activeTowerConfigFunc
	origGateway := gatewayCloseBeadFunc

	storeGetBeadFunc = func(id string) (Bead, error) { return Bead{ID: id, Status: string(beads.StatusOpen)}, nil }
	storeGetChildrenFunc = func(string) ([]Bead, error) { return nil, nil }
	storeListBeadsFunc = func(beads.IssueFilter) ([]Bead, error) { return nil, nil }
	storeCloseBeadFunc = func(string) error { return nil }
	storeRemoveLabelFunc = func(string, string) error { return nil }
	storeGetDependentsWithMetaFunc = func(string) ([]*beads.IssueWithDependencyMetadata, error) { return nil, nil }
	// Default to a non-gateway tower so cmdClose takes the direct-mode
	// branch. Tests that need the gateway path swap this seam explicitly.
	activeTowerConfigFunc = func() (*TowerConfig, error) { return nil, nil }
	gatewayCloseBeadFunc = func(context.Context, string) error {
		t.Fatalf("gatewayCloseBeadFunc must not be called in direct-mode tests")
		return nil
	}

	return func() {
		storeGetBeadFunc = origGetBead
		storeGetChildrenFunc = origGetChildren
		storeListBeadsFunc = origListBeads
		storeCloseBeadFunc = origClose
		storeRemoveLabelFunc = origRemoveLabel
		storeGetDependentsWithMetaFunc = origDependents
		activeTowerConfigFunc = origTower
		gatewayCloseBeadFunc = origGateway
	}
}

// TestCmdCloseGatewayModeRoutesToGatewayClient verifies that cmdClose, on
// a gateway-mode tower, posts to the gateway endpoint instead of running
// the direct-mode lifecycle. The direct-mode store hooks must not be
// invoked — close is owned server-side.
func TestCmdCloseGatewayModeRoutesToGatewayClient(t *testing.T) {
	restore := stubCloseStore(t)
	defer restore()

	storeGetBeadFunc = func(string) (Bead, error) {
		t.Fatalf("storeGetBeadFunc must not be called in gateway mode")
		return Bead{}, nil
	}
	storeGetChildrenFunc = func(string) ([]Bead, error) {
		t.Fatalf("storeGetChildrenFunc must not be called in gateway mode")
		return nil, nil
	}
	storeCloseBeadFunc = func(string) error {
		t.Fatalf("storeCloseBeadFunc must not be called in gateway mode")
		return nil
	}

	activeTowerConfigFunc = func() (*TowerConfig, error) {
		return &TowerConfig{Name: "cluster", Mode: config.TowerModeGateway, URL: "https://example.com"}, nil
	}

	var gatewayCalledWith string
	gatewayCloseBeadFunc = func(_ context.Context, id string) error {
		gatewayCalledWith = id
		return nil
	}

	if err := cmdClose([]string{"spi-parent"}); err != nil {
		t.Fatalf("cmdClose: %v", err)
	}
	if gatewayCalledWith != "spi-parent" {
		t.Fatalf("gatewayCloseBeadFunc id = %q, want spi-parent", gatewayCalledWith)
	}
}

// TestCmdCloseGatewayModePropagatesError verifies that the CLI surfaces
// a gateway-side close error verbatim — the user must see why the close
// failed (e.g. 404 from the gateway).
func TestCmdCloseGatewayModePropagatesError(t *testing.T) {
	restore := stubCloseStore(t)
	defer restore()

	activeTowerConfigFunc = func() (*TowerConfig, error) {
		return &TowerConfig{Name: "cluster", Mode: config.TowerModeGateway, URL: "https://example.com"}, nil
	}

	wantErr := errors.New("gatewayclient: not found")
	gatewayCloseBeadFunc = func(context.Context, string) error { return wantErr }

	err := cmdClose([]string{"spi-missing"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("cmdClose err = %v, want %v", err, wantErr)
	}
}

// TestCloseBeadLifecycleFailsClosedOnGatewayUnsupported verifies the
// defense-in-depth fail-closed guard inside closeBeadLifecycle: when
// child discovery returns store.ErrGatewayUnsupported, the parent must
// NOT be closed and the lifecycle must surface the error.
func TestCloseBeadLifecycleFailsClosedOnGatewayUnsupported(t *testing.T) {
	restore := stubCloseStore(t)
	defer restore()

	storeGetChildrenFunc = func(string) ([]Bead, error) {
		return nil, store.ErrGatewayUnsupported
	}

	closeCalled := false
	storeCloseBeadFunc = func(string) error {
		closeCalled = true
		return nil
	}

	bead := Bead{ID: "spi-parent", Status: string(beads.StatusOpen), Labels: []string{"phase:close"}}
	err := closeBeadLifecycle("spi-parent", bead)
	if err == nil {
		t.Fatalf("closeBeadLifecycle: expected error, got nil")
	}
	if !errors.Is(err, store.ErrGatewayUnsupported) {
		t.Fatalf("err = %v, want wrap of store.ErrGatewayUnsupported", err)
	}
	if closeCalled {
		t.Fatalf("storeCloseBeadFunc was called despite gateway-unsupported child discovery — fail-closed guard regressed")
	}
}
