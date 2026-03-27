package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/beads"
)

// TestLoadExecutorStateNilWhenMissing verifies that loadExecutorState returns nil
// (not an error) when no state file exists — this is the signal that controls
// the fresh-start vs resume path in cmdExecute.
func TestLoadExecutorStateNilWhenMissing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", dir)

	state, err := loadExecutorState("wizard-spi-abc")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if state != nil {
		t.Fatalf("expected nil state for missing file, got %+v", state)
	}
}

// TestLoadExecutorStateReturnsStateWhenPresent verifies that loadExecutorState
// returns the saved state when a state file exists — the resume path in cmdExecute
// uses this to skip re-claiming the bead.
func TestLoadExecutorStateReturnsStateWhenPresent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", dir)

	agentName := "wizard-spi-xyz"
	saved := &executorState{
		BeadID:    "spi-xyz",
		AgentName: agentName,
		Formula:   "spire-agent-work",
		Phase:     "implement",
		Subtasks:  make(map[string]subtaskState),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}

	ex := &formulaExecutor{
		beadID:    saved.BeadID,
		agentName: agentName,
		state:     saved,
	}
	if err := ex.saveState(); err != nil {
		t.Fatalf("saveState error: %v", err)
	}

	loaded, err := loadExecutorState(agentName)
	if err != nil {
		t.Fatalf("loadExecutorState error: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected non-nil state after save, got nil")
	}
	if loaded.BeadID != saved.BeadID {
		t.Errorf("BeadID = %q, want %q", loaded.BeadID, saved.BeadID)
	}
	if loaded.Phase != saved.Phase {
		t.Errorf("Phase = %q, want %q", loaded.Phase, saved.Phase)
	}
	if loaded.Formula != saved.Formula {
		t.Errorf("Formula = %q, want %q", loaded.Formula, saved.Formula)
	}
}

// TestExecutorStatePathIsolatedPerAgent verifies that different agent names
// produce different state paths (preventing cross-agent state pollution).
func TestExecutorStatePathIsolatedPerAgent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", dir)

	path1 := executorStatePath("wizard-spi-aaa")
	path2 := executorStatePath("wizard-spi-bbb")

	if path1 == path2 {
		t.Errorf("expected different paths for different agents, both got %q", path1)
	}
}

// TestCmdExecuteSkipsClaimWhenResuming verifies that when a state file exists,
// cmdExecute does not attempt to claim the bead (the claim would fail against
// a non-running store; if the test reaches the store call, the skip is broken).
func TestCmdExecuteSkipsClaimWhenResuming(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", dir)

	agentName := "wizard-spi-resume-test"

	// Write a state file for this agent so loadExecutorState returns non-nil.
	ex := &formulaExecutor{
		beadID:    "spi-resume-test",
		agentName: agentName,
		state: &executorState{
			BeadID:    "spi-resume-test",
			AgentName: agentName,
			Formula:   "spire-agent-work",
			Phase:     "implement",
			Subtasks:  make(map[string]subtaskState),
			StartedAt: time.Now().UTC().Format(time.RFC3339),
		},
	}
	if err := ex.saveState(); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	// Verify state is visible — the resume path depends on this returning non-nil.
	state, err := loadExecutorState(agentName)
	if err != nil {
		t.Fatalf("loadExecutorState: %v", err)
	}
	if state == nil {
		t.Fatal("state file exists but loadExecutorState returned nil — resume detection is broken")
	}

	// Clean up: remove state file (so future test runs start fresh).
	os.Remove(executorStatePath(agentName))
}

// newTestExecutor builds a formulaExecutor with injectable fakes for store and claude.
// beadData is the epic bead; children are its pre-filed subtasks.
func newTestExecutor(t *testing.T, beadData Bead, children []Bead) (*formulaExecutor, *testCommentStore) {
	t.Helper()
	cs := &testCommentStore{}
	e := &formulaExecutor{
		beadID:    beadData.ID,
		agentName: "wizard-test",
		state:     &executorState{RepoPath: t.TempDir()},
		log:       func(string, ...interface{}) {},
		beadGetter: func(id string) (Bead, error) {
			if id == beadData.ID {
				return beadData, nil
			}
			return Bead{}, fmt.Errorf("bead not found: %s", id)
		},
		childGetter: func(parentID string) ([]Bead, error) {
			if parentID == beadData.ID {
				return children, nil
			}
			return nil, nil
		},
		commentGetter: func(id string) ([]*beads.Comment, error) {
			return cs.get(id), nil
		},
		commentAdder: func(id, text string) error {
			cs.add(id, text)
			return nil
		},
	}
	return e, cs
}

// testCommentStore is an in-memory comment store for tests.
type testCommentStore struct {
	mu       sync.Mutex
	comments map[string][]string // id → list of comment texts
}

func (s *testCommentStore) add(id, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.comments == nil {
		s.comments = make(map[string][]string)
	}
	s.comments[id] = append(s.comments[id], text)
}

func (s *testCommentStore) get(id string) []*beads.Comment {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*beads.Comment
	for _, text := range s.comments[id] {
		out = append(out, &beads.Comment{Text: text})
	}
	return out
}

func (s *testCommentStore) all(id string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.comments[id]...)
}

// TestWizardPlanEnrichesWhenChildrenExist verifies that wizardPlan() invokes
// enrichSubtasksWithChangeSpecs (not an early return) when pre-filed children exist,
// and that a change spec comment is posted on each subtask.
func TestWizardPlanEnrichesWhenChildrenExist(t *testing.T) {
	epic := Bead{ID: "spi-test-epic", Title: "Test epic", Description: "Epic desc"}
	children := []Bead{
		{ID: "spi-test-epic.1", Title: "Subtask one", Description: "First subtask"},
		{ID: "spi-test-epic.2", Title: "Subtask two", Description: "Second subtask"},
	}

	e, cs := newTestExecutor(t, epic, children)

	// claudeRunner returns a fake change spec for each invocation.
	callCount := 0
	e.claudeRunner = func(args []string, dir string) ([]byte, error) {
		callCount++
		return []byte("**Change spec: fake**\n\n**Files to modify:**\n- foo.go — add Bar()"), nil
	}

	pc := PhaseConfig{Model: "claude-opus-4-6"}
	if err := e.wizardPlan(pc); err != nil {
		t.Fatalf("wizardPlan returned unexpected error: %v", err)
	}

	// Claude must be invoked once per child subtask.
	if callCount != len(children) {
		t.Errorf("claudeRunner called %d times, want %d (once per subtask)", callCount, len(children))
	}

	// Each subtask must have a "Change spec:" comment.
	for _, child := range children {
		found := false
		for _, c := range cs.all(child.ID) {
			if strings.HasPrefix(c, "Change spec:") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("subtask %s has no change spec comment; comments: %v", child.ID, cs.all(child.ID))
		}
	}

	// Epic must have a summary comment.
	summaryFound := false
	for _, c := range cs.all(epic.ID) {
		if strings.Contains(c, "enriched") && strings.Contains(c, "change specs") {
			summaryFound = true
			break
		}
	}
	if !summaryFound {
		t.Errorf("epic %s missing enrichment summary comment; comments: %v", epic.ID, cs.all(epic.ID))
	}
}

// TestWizardPlanSkipsAlreadyEnrichedSubtasks verifies that enrichSubtasksWithChangeSpecs
// does not invoke Claude for subtasks that already have a "Change spec:" comment.
func TestWizardPlanSkipsAlreadyEnrichedSubtasks(t *testing.T) {
	epic := Bead{ID: "spi-test-enrich2", Title: "Epic", Description: ""}
	children := []Bead{
		{ID: "spi-test-enrich2.1", Title: "Already done", Description: ""},
		{ID: "spi-test-enrich2.2", Title: "Needs spec", Description: ""},
	}

	e, cs := newTestExecutor(t, epic, children)

	// Pre-populate subtask 1 with a change spec comment so it should be skipped.
	cs.add(children[0].ID, "Change spec:\n\nalready present")

	callCount := 0
	e.claudeRunner = func(args []string, dir string) ([]byte, error) {
		callCount++
		return []byte("**Change spec: new**\n\n- bar.go"), nil
	}

	pc := PhaseConfig{Model: "claude-opus-4-6"}
	if err := e.wizardPlan(pc); err != nil {
		t.Fatalf("wizardPlan returned unexpected error: %v", err)
	}

	// Claude should only be called for the unenriched subtask.
	if callCount != 1 {
		t.Errorf("claudeRunner called %d times, want 1 (skip already-enriched subtask)", callCount)
	}

	// The already-enriched subtask must not get a duplicate comment.
	specs := 0
	for _, c := range cs.all(children[0].ID) {
		if strings.HasPrefix(c, "Change spec:") {
			specs++
		}
	}
	if specs != 1 {
		t.Errorf("already-enriched subtask has %d change spec comments, want exactly 1", specs)
	}
}
