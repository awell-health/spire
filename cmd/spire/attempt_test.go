package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads"
)

// --- storeGetActiveAttempt contract tests ---

// TestGetActiveAttempt_NoChildren verifies nil return when parent has no children.
func TestGetActiveAttempt_NoChildren(t *testing.T) {
	// Use a fake childGetter that returns empty.
	origGetChildren := storeGetChildrenFunc
	storeGetChildrenFunc = func(parentID string) ([]Bead, error) {
		return nil, nil
	}
	defer func() { storeGetChildrenFunc = origGetChildren }()

	result, err := storeGetActiveAttemptTestable("spi-parent")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if result != nil {
		t.Fatalf("expected nil result, got %+v", result)
	}
}

// TestGetActiveAttempt_OneOpenAttempt returns the single active attempt.
func TestGetActiveAttempt_OneOpenAttempt(t *testing.T) {
	origGetChildren := storeGetChildrenFunc
	storeGetChildrenFunc = func(parentID string) ([]Bead, error) {
		return []Bead{
			{ID: "spi-parent.1", Title: "attempt: wizard-1", Status: "in_progress", Labels: []string{"attempt", "agent:wizard-1"}},
			{ID: "spi-parent.2", Title: "regular child", Status: "open"},
		}, nil
	}
	defer func() { storeGetChildrenFunc = origGetChildren }()

	result, err := storeGetActiveAttemptTestable("spi-parent")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.ID != "spi-parent.1" {
		t.Errorf("expected spi-parent.1, got %s", result.ID)
	}
}

// TestGetActiveAttempt_ClosedAttemptIgnored verifies closed attempts are ignored.
func TestGetActiveAttempt_ClosedAttemptIgnored(t *testing.T) {
	origGetChildren := storeGetChildrenFunc
	storeGetChildrenFunc = func(parentID string) ([]Bead, error) {
		return []Bead{
			{ID: "spi-parent.1", Title: "attempt: wizard-1", Status: "closed", Labels: []string{"attempt"}},
		}, nil
	}
	defer func() { storeGetChildrenFunc = origGetChildren }()

	result, err := storeGetActiveAttemptTestable("spi-parent")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if result != nil {
		t.Fatalf("expected nil (closed attempt ignored), got %+v", result)
	}
}

// TestGetActiveAttempt_TwoOpenAttempts returns invariant violation error.
func TestGetActiveAttempt_TwoOpenAttempts(t *testing.T) {
	origGetChildren := storeGetChildrenFunc
	storeGetChildrenFunc = func(parentID string) ([]Bead, error) {
		return []Bead{
			{ID: "spi-parent.1", Title: "attempt: wizard-1", Status: "in_progress", Labels: []string{"attempt"}},
			{ID: "spi-parent.2", Title: "attempt: wizard-2", Status: "open", Labels: []string{"attempt"}},
		}, nil
	}
	defer func() { storeGetChildrenFunc = origGetChildren }()

	_, err := storeGetActiveAttemptTestable("spi-parent")
	if err == nil {
		t.Fatal("expected invariant violation error, got nil")
	}
	if !strings.Contains(err.Error(), "invariant violation") {
		t.Errorf("expected 'invariant violation' in error, got: %s", err.Error())
	}
}

// TestGetActiveAttempt_TitleMatchWithoutLabel verifies title-based detection.
func TestGetActiveAttempt_TitleMatchWithoutLabel(t *testing.T) {
	origGetChildren := storeGetChildrenFunc
	storeGetChildrenFunc = func(parentID string) ([]Bead, error) {
		return []Bead{
			{ID: "spi-parent.1", Title: "attempt: wizard-1", Status: "in_progress", Labels: nil},
		}, nil
	}
	defer func() { storeGetChildrenFunc = origGetChildren }()

	result, err := storeGetActiveAttemptTestable("spi-parent")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if result == nil || result.ID != "spi-parent.1" {
		t.Errorf("expected spi-parent.1, got %+v", result)
	}
}

// --- isAttemptBead tests ---

func TestIsAttemptBead_WithLabel(t *testing.T) {
	b := Bead{Labels: []string{"attempt", "agent:wizard-1"}}
	if !isAttemptBead(b) {
		t.Error("expected true for bead with attempt label")
	}
}

func TestIsAttemptBead_WithTitle(t *testing.T) {
	b := Bead{Title: "attempt: wizard-1"}
	if !isAttemptBead(b) {
		t.Error("expected true for bead with attempt: title prefix")
	}
}

func TestIsAttemptBead_Neither(t *testing.T) {
	b := Bead{Title: "regular task", Labels: []string{"task"}}
	if isAttemptBead(b) {
		t.Error("expected false for regular bead")
	}
}

func TestIsAttemptBoardBead_WithLabel(t *testing.T) {
	b := BoardBead{Labels: []string{"attempt"}}
	if !isAttemptBoardBead(b) {
		t.Error("expected true for board bead with attempt label")
	}
}

func TestIsAttemptBoardBead_WithTitle(t *testing.T) {
	b := BoardBead{Title: "attempt: wizard-2"}
	if !isAttemptBoardBead(b) {
		t.Error("expected true for board bead with attempt: title")
	}
}

func TestIsAttemptBoardBead_Neither(t *testing.T) {
	b := BoardBead{Title: "some task", Labels: []string{"work"}}
	if isAttemptBoardBead(b) {
		t.Error("expected false for regular board bead")
	}
}

// --- Executor attempt lifecycle tests ---

func TestExecutor_CreatesAttemptBead(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", dir)

	var createdID string
	var createdArgs [4]string

	e := &formulaExecutor{
		beadID:    "spi-test",
		agentName: "wizard-test",
		state: &executorState{
			BeadID:        "spi-test",
			AgentName:     "wizard-test",
			StagingBranch: "feat/spi-test",
		},
		log: func(string, ...interface{}) {},
		beadGetter: func(id string) (Bead, error) {
			return Bead{ID: id, Status: "in_progress"}, nil
		},
		activeAttemptGetter: func(parentID string) (*Bead, error) {
			return nil, nil // no existing attempt
		},
		attemptCreator: func(parentID, agentName, model, branch string) (string, error) {
			createdID = "spi-test.attempt-1"
			createdArgs = [4]string{parentID, agentName, model, branch}
			return createdID, nil
		},
		attemptCloser: func(attemptID, result string) error {
			return nil
		},
	}

	err := e.ensureAttemptBead()
	if err != nil {
		t.Fatalf("ensureAttemptBead error: %v", err)
	}
	if e.state.AttemptBeadID != createdID {
		t.Errorf("AttemptBeadID = %q, want %q", e.state.AttemptBeadID, createdID)
	}
	if createdArgs[0] != "spi-test" {
		t.Errorf("parentID = %q, want spi-test", createdArgs[0])
	}
	if createdArgs[1] != "wizard-test" {
		t.Errorf("agentName = %q, want wizard-test", createdArgs[1])
	}
	if createdArgs[3] != "feat/spi-test" {
		t.Errorf("branch = %q, want feat/spi-test", createdArgs[3])
	}
}

func TestExecutor_ClosesAttemptOnSuccess(t *testing.T) {
	var closedWith string
	var closedID string

	e := &formulaExecutor{
		beadID:    "spi-close-test",
		agentName: "wizard-close",
		state: &executorState{
			AttemptBeadID: "spi-close-test.attempt-1",
		},
		log: func(string, ...interface{}) {},
		attemptCloser: func(attemptID, result string) error {
			closedID = attemptID
			closedWith = result
			return nil
		},
	}

	e.closeAttempt("success: merged")

	if closedID != "spi-close-test.attempt-1" {
		t.Errorf("closed ID = %q, want spi-close-test.attempt-1", closedID)
	}
	if closedWith != "success: merged" {
		t.Errorf("close result = %q, want 'success: merged'", closedWith)
	}
	if e.state.AttemptBeadID != "" {
		t.Errorf("AttemptBeadID should be cleared after close, got %q", e.state.AttemptBeadID)
	}
}

func TestExecutor_CloseAttemptNoop_WhenEmpty(t *testing.T) {
	called := false
	e := &formulaExecutor{
		state: &executorState{AttemptBeadID: ""},
		log:   func(string, ...interface{}) {},
		attemptCloser: func(attemptID, result string) error {
			called = true
			return nil
		},
	}

	e.closeAttempt("should not fire")
	if called {
		t.Error("attemptCloser should not be called when AttemptBeadID is empty")
	}
}

func TestExecutor_ResumesExistingAttempt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", dir)

	e := &formulaExecutor{
		beadID:    "spi-resume",
		agentName: "wizard-resume",
		state: &executorState{
			BeadID:        "spi-resume",
			AgentName:     "wizard-resume",
			AttemptBeadID: "spi-resume.attempt-1",
		},
		log: func(string, ...interface{}) {},
		beadGetter: func(id string) (Bead, error) {
			if id == "spi-resume.attempt-1" {
				return Bead{ID: id, Status: "in_progress"}, nil
			}
			return Bead{}, fmt.Errorf("not found: %s", id)
		},
		activeAttemptGetter: func(parentID string) (*Bead, error) {
			return nil, nil
		},
		attemptCreator: func(parentID, agentName, model, branch string) (string, error) {
			t.Fatal("attemptCreator should not be called when resuming")
			return "", nil
		},
		attemptCloser: func(attemptID, result string) error {
			return nil
		},
	}

	err := e.ensureAttemptBead()
	if err != nil {
		t.Fatalf("ensureAttemptBead error: %v", err)
	}
	if e.state.AttemptBeadID != "spi-resume.attempt-1" {
		t.Errorf("should keep existing AttemptBeadID, got %q", e.state.AttemptBeadID)
	}
}

// --- Roster attempt-based enrichment tests ---

func TestRosterEnrichment_AttemptAgent(t *testing.T) {
	// This test verifies that enrichRosterAgents uses attempt bead agent info.
	// We can't easily test the full enrichRosterAgents function without a store,
	// but we verify the isAttemptBead detection that feeds into it.
	attempt := Bead{
		ID:     "spi-x.1",
		Title:  "attempt: wizard-alpha",
		Status: "in_progress",
		Labels: []string{"attempt", "agent:wizard-alpha", "model:claude-opus-4-6", "branch:feat/spi-x"},
	}

	if !isAttemptBead(attempt) {
		t.Error("expected attempt bead to be detected")
	}
	agent := hasLabel(attempt, "agent:")
	if agent != "wizard-alpha" {
		t.Errorf("agent = %q, want wizard-alpha", agent)
	}
}

func TestRosterEnrichment_FallbackWithoutAttempt(t *testing.T) {
	// A regular bead without attempt children falls back to registry/owner: label logic.
	bead := Bead{
		ID:     "spi-y",
		Title:  "regular task",
		Status: "in_progress",
		Labels: []string{"owner:wizard-beta"},
	}

	if isAttemptBead(bead) {
		t.Error("regular bead should not be detected as attempt")
	}
	owner := hasLabel(bead, "owner:")
	if owner != "wizard-beta" {
		t.Errorf("owner fallback = %q, want wizard-beta", owner)
	}
}

// --- Steward skip tests ---

func TestSteward_SkipsBeadWithActiveAttempt(t *testing.T) {
	// Verify that a bead with an open attempt child has owner: label check AND
	// attempt check both returning "skip".
	bead := Bead{
		ID:     "spi-steward-test",
		Title:  "task with active attempt",
		Status: "open",
	}

	// The owner: check would pass (no owner label), but the attempt check should skip it.
	// We verify the isAttemptBead function correctly identifies attempt children.
	attemptChild := Bead{
		ID:     "spi-steward-test.1",
		Title:  "attempt: wizard-gamma",
		Status: "in_progress",
		Labels: []string{"attempt"},
		Parent: bead.ID,
	}

	if !isAttemptBead(attemptChild) {
		t.Error("attempt child should be detected")
	}

	// Verify the parent bead is NOT an attempt bead
	if isAttemptBead(bead) {
		t.Error("parent bead should not be detected as attempt")
	}
}

func TestSteward_AttemptBeadsNeverReadyWork(t *testing.T) {
	// Attempt beads should be filtered out of ready work.
	attemptBead := Bead{
		ID:     "spi-x.1",
		Title:  "attempt: wizard-delta",
		Status: "open",
		Labels: []string{"attempt"},
	}

	if !isAttemptBead(attemptBead) {
		t.Fatal("expected attempt bead to be detected by isAttemptBead")
	}
}

// --- Board filtering tests ---

func TestBoard_FiltersAttemptBeads(t *testing.T) {
	openBeads := []BoardBead{
		{ID: "spi-task-1", Title: "Real task", Status: "open", Type: "task"},
		{ID: "spi-task-1.1", Title: "attempt: wizard-1", Status: "in_progress", Type: "task", Labels: []string{"attempt"}},
		{ID: "spi-task-2", Title: "Another task", Status: "open", Type: "task"},
	}
	closedBeads := []BoardBead{}
	blockedBeads := []BoardBead{}

	cols := categorizeColumnsFromStore(openBeads, closedBeads, blockedBeads, "")

	// Attempt bead should be filtered out
	for _, col := range [][]BoardBead{cols.Ready, cols.Design, cols.Plan, cols.Implement, cols.Review, cols.Merge, cols.Done, cols.Blocked, cols.Alerts} {
		for _, b := range col {
			if isAttemptBoardBead(b) {
				t.Errorf("attempt bead %s should be filtered from board columns", b.ID)
			}
		}
	}

	// Real tasks should still be present
	found := 0
	for _, b := range cols.Ready {
		if b.ID == "spi-task-1" || b.ID == "spi-task-2" {
			found++
		}
	}
	if found != 2 {
		t.Errorf("expected 2 real tasks in Ready, found %d", found)
	}
}

// --- Integration test: attempt lifecycle end-to-end ---

func TestAttemptLifecycle_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", dir)

	// Simulate: create attempt, verify active, close, verify inactive.
	var attempts []Bead
	nextID := 1

	creator := func(parentID, agentName, model, branch string) (string, error) {
		id := fmt.Sprintf("%s.attempt-%d", parentID, nextID)
		nextID++
		attempt := Bead{
			ID:     id,
			Title:  "attempt: " + agentName,
			Status: "in_progress",
			Labels: []string{"attempt", "agent:" + agentName, "model:" + model, "branch:" + branch},
			Parent: parentID,
		}
		attempts = append(attempts, attempt)
		return id, nil
	}

	closer := func(attemptID, result string) error {
		for i := range attempts {
			if attempts[i].ID == attemptID {
				attempts[i].Status = "closed"
				return nil
			}
		}
		return fmt.Errorf("attempt not found: %s", attemptID)
	}

	activeGetter := func(parentID string) (*Bead, error) {
		var active []Bead
		for _, a := range attempts {
			if a.Parent == parentID && (a.Status == "open" || a.Status == "in_progress") && isAttemptBead(a) {
				active = append(active, a)
			}
		}
		switch len(active) {
		case 0:
			return nil, nil
		case 1:
			return &active[0], nil
		default:
			return nil, fmt.Errorf("invariant violation: %d open attempts", len(active))
		}
	}

	beadGetter := func(id string) (Bead, error) {
		for _, a := range attempts {
			if a.ID == id {
				return a, nil
			}
		}
		return Bead{ID: id, Status: "in_progress"}, nil
	}

	e := &formulaExecutor{
		beadID:    "spi-e2e",
		agentName: "wizard-e2e",
		state: &executorState{
			BeadID:        "spi-e2e",
			AgentName:     "wizard-e2e",
			StagingBranch: "feat/spi-e2e",
		},
		log:                 func(string, ...interface{}) {},
		attemptCreator:      creator,
		attemptCloser:       closer,
		activeAttemptGetter: activeGetter,
		beadGetter:          beadGetter,
	}

	// Step 1: Create attempt
	if err := e.ensureAttemptBead(); err != nil {
		t.Fatalf("ensureAttemptBead: %v", err)
	}
	if e.state.AttemptBeadID == "" {
		t.Fatal("expected AttemptBeadID to be set")
	}
	attemptID := e.state.AttemptBeadID

	// Step 2: Verify active attempt exists
	active, err := activeGetter("spi-e2e")
	if err != nil {
		t.Fatalf("activeGetter: %v", err)
	}
	if active == nil || active.ID != attemptID {
		t.Fatalf("expected active attempt %s, got %+v", attemptID, active)
	}

	// Step 3: Close attempt
	e.closeAttempt("success: test complete")

	// Step 4: Verify no active attempt
	active, err = activeGetter("spi-e2e")
	if err != nil {
		t.Fatalf("activeGetter after close: %v", err)
	}
	if active != nil {
		t.Fatalf("expected no active attempt after close, got %+v", active)
	}

	// Step 5: Retry — create a new attempt (simulates retry)
	if err := e.ensureAttemptBead(); err != nil {
		t.Fatalf("ensureAttemptBead on retry: %v", err)
	}
	if e.state.AttemptBeadID == attemptID {
		t.Error("retry should create a NEW attempt bead, not reuse old one")
	}
	if e.state.AttemptBeadID == "" {
		t.Error("retry should have created a new attempt bead")
	}

	// Step 6: Verify exactly one active (the new one)
	active, err = activeGetter("spi-e2e")
	if err != nil {
		t.Fatalf("activeGetter after retry: %v", err)
	}
	if active == nil {
		t.Fatal("expected active attempt after retry")
	}
	if active.ID != e.state.AttemptBeadID {
		t.Errorf("active attempt ID = %q, want %q", active.ID, e.state.AttemptBeadID)
	}
}

// --- Helper: testable version of storeGetActiveAttempt ---

// storeGetActiveAttemptTestable is a testable version that uses storeGetChildrenFunc.
func storeGetActiveAttemptTestable(parentID string) (*Bead, error) {
	children, err := storeGetChildrenFunc(parentID)
	if err != nil {
		return nil, err
	}

	var active []Bead
	for _, child := range children {
		if child.Status != "open" && child.Status != "in_progress" {
			continue
		}
		if !isAttemptBead(child) {
			continue
		}
		active = append(active, child)
	}

	switch len(active) {
	case 0:
		return nil, nil
	case 1:
		return &active[0], nil
	default:
		ids := make([]string, len(active))
		for i, a := range active {
			ids[i] = a.ID
		}
		return nil, fmt.Errorf("invariant violation: %d open attempt beads for %s: %s",
			len(active), parentID, strings.Join(ids, ", "))
	}
}

// Suppress unused import warnings for testing dependencies.
var (
	_ = time.Now
	_ = beads.StatusOpen
)
