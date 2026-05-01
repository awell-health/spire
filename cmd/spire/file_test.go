package main

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// stubFileDeps swaps the test-replaceable seams used by cmdFile and returns
// a cleanup function that restores them. Callers replace individual seams
// after calling this.
//
// cmdFile may os.Chdir into the resolved instance path when --prefix matches
// a real registered instance (e.g. on developer machines). To keep the test
// hermetic, we snapshot and restore the cwd here.
func stubFileDeps(t *testing.T) {
	t.Helper()
	origGet := fileGetBead
	origCreate := fileCreateBead
	origAddDep := fileAddDepTyped
	origAddLabel := fileAddLabel
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		fileGetBead = origGet
		fileCreateBead = origCreate
		fileAddDepTyped = origAddDep
		fileAddLabel = origAddLabel
		_ = os.Chdir(cwd)
	})
	// Default no-op stubs so tests that don't touch a seam don't hit the real store.
	fileGetBead = func(id string) (Bead, error) {
		return Bead{ID: id, Type: "task"}, nil
	}
	fileCreateBead = func(opts createOpts) (string, error) {
		return "spi-newbead", nil
	}
	fileAddDepTyped = func(issueID, dependsOnID, depType string) error { return nil }
	fileAddLabel = func(id, label string) error { return nil }
}

func TestCmdFile_CausedBy_AddsDep(t *testing.T) {
	stubFileDeps(t)

	createCalled := false
	fileGetBead = func(id string) (Bead, error) {
		// caused-by lookup target is just confirmed to exist
		return Bead{ID: id, Type: "task"}, nil
	}
	fileCreateBead = func(opts createOpts) (string, error) {
		createCalled = true
		return "spi-newbead", nil
	}

	var depIssueID, depOnID, depType string
	fileAddDepTyped = func(issueID, dependsOnID, t string) error {
		depIssueID = issueID
		depOnID = dependsOnID
		depType = t
		return nil
	}

	if err := cmdFile([]string{"a bug", "--prefix", "spi", "-t", "bug", "-p", "2", "--caused-by", "spi-source"}); err != nil {
		t.Fatalf("cmdFile returned error: %v", err)
	}
	if !createCalled {
		t.Fatal("expected fileCreateBead to be called")
	}
	if depIssueID != "spi-newbead" || depOnID != "spi-source" || depType != "caused-by" {
		t.Fatalf("dep = %s→%s:%s, want spi-newbead→spi-source:caused-by", depIssueID, depOnID, depType)
	}
}

func TestCmdFile_CausedBy_NonexistentSourceErrorsBeforeCreation(t *testing.T) {
	stubFileDeps(t)

	fileGetBead = func(id string) (Bead, error) {
		return Bead{}, errors.New("not found")
	}
	createCalled := false
	fileCreateBead = func(opts createOpts) (string, error) {
		createCalled = true
		return "spi-shouldnotbecreated", nil
	}

	err := cmdFile([]string{"a bug", "--prefix", "spi", "-t", "bug", "-p", "2", "--caused-by", "spi-bogus"})
	if err == nil {
		t.Fatal("expected error from nonexistent --caused-by target")
	}
	if !strings.Contains(err.Error(), "--caused-by spi-bogus") {
		t.Errorf("expected error to reference --caused-by spi-bogus, got: %v", err)
	}
	if createCalled {
		t.Error("fileCreateBead must not be called when --caused-by validation fails")
	}
}

func TestCmdFile_CausedBy_DepAddFailureReturnsErrorWithBeadID(t *testing.T) {
	stubFileDeps(t)

	fileGetBead = func(id string) (Bead, error) {
		return Bead{ID: id, Type: "task"}, nil
	}
	fileCreateBead = func(opts createOpts) (string, error) {
		return "spi-created42", nil
	}
	fileAddDepTyped = func(issueID, dependsOnID, depType string) error {
		return errors.New("dolt unavailable")
	}

	err := cmdFile([]string{"a bug", "--prefix", "spi", "-t", "bug", "-p", "2", "--caused-by", "spi-source"})
	if err == nil {
		t.Fatal("expected error when caused-by dep-add fails")
	}
	if !strings.Contains(err.Error(), "spi-created42") {
		t.Errorf("expected error to include created bead ID spi-created42 for cleanup, got: %v", err)
	}
	if !strings.Contains(err.Error(), "spi-source") {
		t.Errorf("expected error to reference target bead spi-source, got: %v", err)
	}
}

func TestCmdFile_Design_RejectsNonDesignBead(t *testing.T) {
	stubFileDeps(t)

	fileGetBead = func(id string) (Bead, error) {
		return Bead{ID: id, Type: "task"}, nil
	}
	createCalled := false
	fileCreateBead = func(opts createOpts) (string, error) {
		createCalled = true
		return "spi-shouldnotbecreated", nil
	}

	err := cmdFile([]string{"epic", "--prefix", "spi", "-t", "epic", "-p", "1", "--design", "spi-tasknoT"})
	if err == nil {
		t.Fatal("expected error when --design points at a non-design bead")
	}
	msg := err.Error()
	if !strings.Contains(msg, "--design spi-tasknoT") {
		t.Errorf("expected error to reference --design spi-tasknoT, got: %v", err)
	}
	if !strings.Contains(msg, "type=task, not design") {
		t.Errorf("expected error to mention type=task, not design, got: %v", err)
	}
	if !strings.Contains(msg, "use --ref") {
		t.Errorf("expected error to point at --ref escape hatch, got: %v", err)
	}
	if createCalled {
		t.Error("fileCreateBead must not be called when --design validation fails")
	}
}

func TestCmdFile_Design_AcceptsDesignBeadAndAddsDiscoveredFrom(t *testing.T) {
	stubFileDeps(t)

	fileGetBead = func(id string) (Bead, error) {
		return Bead{ID: id, Type: "design"}, nil
	}
	fileCreateBead = func(opts createOpts) (string, error) {
		return "spi-newepic", nil
	}

	var depIssueID, depOnID, depType string
	fileAddDepTyped = func(issueID, dependsOnID, t string) error {
		depIssueID = issueID
		depOnID = dependsOnID
		depType = t
		return nil
	}

	if err := cmdFile([]string{"epic", "--prefix", "spi", "-t", "epic", "-p", "1", "--design", "spi-design1"}); err != nil {
		t.Fatalf("cmdFile returned error: %v", err)
	}
	if depIssueID != "spi-newepic" || depOnID != "spi-design1" || depType != "discovered-from" {
		t.Fatalf("dep = %s→%s:%s, want spi-newepic→spi-design1:discovered-from", depIssueID, depOnID, depType)
	}
}

func TestCmdFile_Ref_AcceptsNonDesignBeadWithoutValidation(t *testing.T) {
	stubFileDeps(t)

	getCalled := false
	fileGetBead = func(id string) (Bead, error) {
		getCalled = true
		return Bead{ID: id, Type: "task"}, nil
	}
	fileCreateBead = func(opts createOpts) (string, error) {
		return "spi-newtask", nil
	}

	var depIssueID, depOnID, depType string
	fileAddDepTyped = func(issueID, dependsOnID, t string) error {
		depIssueID = issueID
		depOnID = dependsOnID
		depType = t
		return nil
	}

	if err := cmdFile([]string{"task", "--prefix", "spi", "-t", "task", "-p", "2", "--ref", "spi-anything"}); err != nil {
		t.Fatalf("cmdFile returned error: %v", err)
	}
	if getCalled {
		t.Error("fileGetBead must NOT be called for --ref (escape hatch is unvalidated)")
	}
	if depIssueID != "spi-newtask" || depOnID != "spi-anything" || depType != "discovered-from" {
		t.Fatalf("dep = %s→%s:%s, want spi-newtask→spi-anything:discovered-from", depIssueID, depOnID, depType)
	}
}

// TestCmdFile_DesignAndCausedBy_RunIndependently verifies that --design and
// --caused-by both apply atomically when used together.
func TestCmdFile_DesignAndCausedBy_RunIndependently(t *testing.T) {
	stubFileDeps(t)

	fileGetBead = func(id string) (Bead, error) {
		switch id {
		case "spi-design1":
			return Bead{ID: id, Type: "design"}, nil
		case "spi-source":
			return Bead{ID: id, Type: "task"}, nil
		}
		return Bead{}, errors.New("not found")
	}
	fileCreateBead = func(opts createOpts) (string, error) {
		return "spi-newbead", nil
	}

	type dep struct{ issueID, onID, depType string }
	var deps []dep
	fileAddDepTyped = func(issueID, dependsOnID, t string) error {
		deps = append(deps, dep{issueID, dependsOnID, t})
		return nil
	}

	args := []string{"epic", "--prefix", "spi", "-t", "epic", "-p", "1", "--design", "spi-design1", "--caused-by", "spi-source"}
	if err := cmdFile(args); err != nil {
		t.Fatalf("cmdFile returned error: %v", err)
	}

	var sawDesign, sawCausedBy bool
	for _, d := range deps {
		if d.onID == "spi-design1" && d.depType == "discovered-from" {
			sawDesign = true
		}
		if d.onID == "spi-source" && d.depType == "caused-by" {
			sawCausedBy = true
		}
	}
	if !sawDesign {
		t.Errorf("expected discovered-from dep to spi-design1, got %+v", deps)
	}
	if !sawCausedBy {
		t.Errorf("expected caused-by dep to spi-source, got %+v", deps)
	}
}
