package main

import (
	"testing"

	"github.com/steveyegge/beads"
)

// TestCloseCausedByAlerts_CascadeClosesEmptyImplementAlert verifies that
// closeCausedByAlerts (called by spire close) cascade-closes an open
// empty-implement alert bead linked via a caused-by dep.
func TestCloseCausedByAlerts_CascadeClosesEmptyImplementAlert(t *testing.T) {
	var closedIDs []string

	origDeps := storeGetDependentsWithMetaFunc
	origClose := storeCloseBeadFunc
	defer func() {
		storeGetDependentsWithMetaFunc = origDeps
		storeCloseBeadFunc = origClose
	}()

	storeGetDependentsWithMetaFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		if id == "spi-src" {
			return []*beads.IssueWithDependencyMetadata{
				{
					Issue:          beads.Issue{ID: "spi-alert-1", Status: beads.StatusOpen, Labels: []string{"alert:empty-implement"}},
					DependencyType: "caused-by",
				},
			}, nil
		}
		return nil, nil
	}
	storeCloseBeadFunc = func(id string) error {
		closedIDs = append(closedIDs, id)
		return nil
	}

	closeCausedByAlerts("spi-src")

	if len(closedIDs) != 1 || closedIDs[0] != "spi-alert-1" {
		t.Errorf("expected [spi-alert-1] closed, got %v", closedIDs)
	}
}

// TestCloseCausedByAlerts_IgnoresRelatedDep verifies that closeCausedByAlerts
// does NOT close alerts linked via a related dep (only caused-by).
func TestCloseCausedByAlerts_IgnoresRelatedDep(t *testing.T) {
	var closedIDs []string

	origDeps := storeGetDependentsWithMetaFunc
	origClose := storeCloseBeadFunc
	defer func() {
		storeGetDependentsWithMetaFunc = origDeps
		storeCloseBeadFunc = origClose
	}()

	storeGetDependentsWithMetaFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return []*beads.IssueWithDependencyMetadata{
			{
				Issue:          beads.Issue{ID: "spi-alert-old", Status: beads.StatusOpen, Labels: []string{"alert:empty-implement"}},
				DependencyType: beads.DepRelated,
			},
		}, nil
	}
	storeCloseBeadFunc = func(id string) error {
		closedIDs = append(closedIDs, id)
		return nil
	}

	closeCausedByAlerts("spi-src")

	if len(closedIDs) != 0 {
		t.Errorf("expected no alerts closed (related dep should be ignored), got %v", closedIDs)
	}
}

// TestCloseCausedByAlerts_IgnoresAlreadyClosed verifies that closeCausedByAlerts
// does not attempt to re-close an already-closed alert.
func TestCloseCausedByAlerts_IgnoresAlreadyClosed(t *testing.T) {
	var closedIDs []string

	origDeps := storeGetDependentsWithMetaFunc
	origClose := storeCloseBeadFunc
	defer func() {
		storeGetDependentsWithMetaFunc = origDeps
		storeCloseBeadFunc = origClose
	}()

	storeGetDependentsWithMetaFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return []*beads.IssueWithDependencyMetadata{
			{
				Issue:          beads.Issue{ID: "spi-alert-done", Status: beads.StatusClosed, Labels: []string{"alert:empty-implement"}},
				DependencyType: "caused-by",
			},
		}, nil
	}
	storeCloseBeadFunc = func(id string) error {
		closedIDs = append(closedIDs, id)
		return nil
	}

	closeCausedByAlerts("spi-src")

	if len(closedIDs) != 0 {
		t.Errorf("expected no alerts closed (already closed), got %v", closedIDs)
	}
}

// TestCloseCausedByAlerts_IgnoresNonAlert verifies that closeCausedByAlerts
// does not close caused-by dependents that lack an alert label.
func TestCloseCausedByAlerts_IgnoresNonAlert(t *testing.T) {
	var closedIDs []string

	origDeps := storeGetDependentsWithMetaFunc
	origClose := storeCloseBeadFunc
	defer func() {
		storeGetDependentsWithMetaFunc = origDeps
		storeCloseBeadFunc = origClose
	}()

	storeGetDependentsWithMetaFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return []*beads.IssueWithDependencyMetadata{
			{
				Issue:          beads.Issue{ID: "spi-child", Status: beads.StatusOpen, Labels: []string{"step:implement"}},
				DependencyType: "caused-by",
			},
		}, nil
	}
	storeCloseBeadFunc = func(id string) error {
		closedIDs = append(closedIDs, id)
		return nil
	}

	closeCausedByAlerts("spi-src")

	if len(closedIDs) != 0 {
		t.Errorf("expected no beads closed (non-alert caused-by dep), got %v", closedIDs)
	}
}

// TestCloseRelatedAlerts_CascadeClosesEmptyImplementAlert verifies that
// closeRelatedAlerts (called by spire resummon) cascade-closes an open
// empty-implement alert bead linked via a caused-by dep.
func TestCloseRelatedAlerts_CascadeClosesEmptyImplementAlert(t *testing.T) {
	var closedIDs []string

	origDeps := storeGetDependentsWithMetaFunc
	origClose := storeCloseBeadFunc
	defer func() {
		storeGetDependentsWithMetaFunc = origDeps
		storeCloseBeadFunc = origClose
	}()

	storeGetDependentsWithMetaFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return []*beads.IssueWithDependencyMetadata{
			{
				Issue:          beads.Issue{ID: "spi-alert-ei", Status: beads.StatusOpen, Labels: []string{"alert:empty-implement"}},
				DependencyType: "caused-by",
			},
		}, nil
	}
	storeCloseBeadFunc = func(id string) error {
		closedIDs = append(closedIDs, id)
		return nil
	}

	closeRelatedAlerts("spi-src")

	if len(closedIDs) != 1 || closedIDs[0] != "spi-alert-ei" {
		t.Errorf("expected [spi-alert-ei] closed, got %v", closedIDs)
	}
}

// TestCloseRelatedAlerts_AlsoClosesLegacyRelatedAlerts verifies that
// closeRelatedAlerts handles both caused-by (current) and related (legacy)
// alert deps, ensuring backward compatibility with old empty-implement alerts.
func TestCloseRelatedAlerts_AlsoClosesLegacyRelatedAlerts(t *testing.T) {
	var closedIDs []string

	origDeps := storeGetDependentsWithMetaFunc
	origClose := storeCloseBeadFunc
	defer func() {
		storeGetDependentsWithMetaFunc = origDeps
		storeCloseBeadFunc = origClose
	}()

	storeGetDependentsWithMetaFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return []*beads.IssueWithDependencyMetadata{
			{
				Issue:          beads.Issue{ID: "spi-alert-old", Status: beads.StatusOpen, Labels: []string{"alert:empty-implement"}},
				DependencyType: beads.DepRelated,
			},
			{
				Issue:          beads.Issue{ID: "spi-alert-new", Status: beads.StatusOpen, Labels: []string{"alert:empty-implement"}},
				DependencyType: "caused-by",
			},
		}, nil
	}
	storeCloseBeadFunc = func(id string) error {
		closedIDs = append(closedIDs, id)
		return nil
	}

	closeRelatedAlerts("spi-src")

	if len(closedIDs) != 2 {
		t.Errorf("expected 2 alerts closed (both related and caused-by), got %v", closedIDs)
	}
}

// TestCloseRelatedAlerts_IgnoresAlreadyClosed verifies that closeRelatedAlerts
// skips already-closed alert beads.
func TestCloseRelatedAlerts_IgnoresAlreadyClosed(t *testing.T) {
	var closedIDs []string

	origDeps := storeGetDependentsWithMetaFunc
	origClose := storeCloseBeadFunc
	defer func() {
		storeGetDependentsWithMetaFunc = origDeps
		storeCloseBeadFunc = origClose
	}()

	storeGetDependentsWithMetaFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return []*beads.IssueWithDependencyMetadata{
			{
				Issue:          beads.Issue{ID: "spi-alert-done", Status: beads.StatusClosed, Labels: []string{"alert:empty-implement"}},
				DependencyType: "caused-by",
			},
		}, nil
	}
	storeCloseBeadFunc = func(id string) error {
		closedIDs = append(closedIDs, id)
		return nil
	}

	closeRelatedAlerts("spi-src")

	if len(closedIDs) != 0 {
		t.Errorf("expected no alerts closed (already closed), got %v", closedIDs)
	}
}
