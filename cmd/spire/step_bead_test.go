package main

import (
	"fmt"
	"strings"
	"testing"
)

// --- storeCreateStepBead + storeCloseStepBead tests ---

// TestStepBead_CreateAndClose creates all phases, closes one, and verifies states.
func TestStepBead_CreateAndClose(t *testing.T) {
	// In-memory step bead store for testing.
	store := newFakeStepStore()

	// Create step beads for each phase.
	phases := []string{"design", "plan", "implement", "review", "merge"}
	for _, phase := range phases {
		id, err := store.create("spi-parent", phase)
		if err != nil {
			t.Fatalf("create step bead for %s: %v", phase, err)
		}
		if id == "" {
			t.Fatalf("expected non-empty ID for phase %s", phase)
		}
	}

	// Verify all step beads were created.
	steps := store.getSteps("spi-parent")
	if len(steps) != 5 {
		t.Fatalf("expected 5 step beads, got %d", len(steps))
	}

	// Close the design step.
	designID := store.stepIDs["spi-parent"]["design"]
	if err := store.close(designID); err != nil {
		t.Fatalf("close design step: %v", err)
	}

	// Verify design is closed, others are open.
	for _, s := range store.getSteps("spi-parent") {
		phase := hasLabel(s, "step:")
		if phase == "design" && s.Status != "closed" {
			t.Errorf("design step should be closed, got %s", s.Status)
		}
		if phase != "design" && s.Status == "closed" {
			t.Errorf("phase %s should not be closed", phase)
		}
	}
}

// --- storeGetStepBeads ordering tests ---

// TestStepBead_GetStepBeadsOrder verifies step beads are returned in creation order.
func TestStepBead_GetStepBeadsOrder(t *testing.T) {
	store := newFakeStepStore()

	phases := []string{"design", "plan", "implement", "review", "merge"}
	for _, phase := range phases {
		store.create("spi-order", phase)
	}

	steps := store.getSteps("spi-order")
	if len(steps) != 5 {
		t.Fatalf("expected 5 step beads, got %d", len(steps))
	}

	for i, step := range steps {
		name := hasLabel(step, "step:")
		if name != phases[i] {
			t.Errorf("step[%d] = %q, want %q", i, name, phases[i])
		}
	}
}

// --- storeGetActiveStep tests ---

// TestStepBead_GetActiveStep_OneActive returns the single active step.
func TestStepBead_GetActiveStep_OneActive(t *testing.T) {
	store := newFakeStepStore()

	store.create("spi-active", "design")
	store.create("spi-active", "implement")

	// Activate the design step.
	designID := store.stepIDs["spi-active"]["design"]
	store.activate(designID)

	active := store.getActive("spi-active")
	if active == nil {
		t.Fatal("expected active step")
	}
	if active.ID != designID {
		t.Errorf("active step = %s, want %s", active.ID, designID)
	}
}

// TestStepBead_GetActiveStep_NoneActive returns nil when no step is active.
func TestStepBead_GetActiveStep_NoneActive(t *testing.T) {
	store := newFakeStepStore()

	store.create("spi-none", "design")
	store.create("spi-none", "implement")

	active := store.getActive("spi-none")
	if active != nil {
		t.Errorf("expected nil active step, got %+v", active)
	}
}

// TestStepBead_GetActiveStep_TwoActive returns invariant violation.
func TestStepBead_GetActiveStep_TwoActive(t *testing.T) {
	store := newFakeStepStore()

	store.create("spi-two", "design")
	store.create("spi-two", "implement")

	store.activate(store.stepIDs["spi-two"]["design"])
	store.activate(store.stepIDs["spi-two"]["implement"])

	_, err := store.getActiveErr("spi-two")
	if err == nil {
		t.Fatal("expected invariant violation error")
	}
	if !strings.Contains(err.Error(), "invariant violation") {
		t.Errorf("expected 'invariant violation' in error, got: %s", err.Error())
	}
}

// --- Formula pour tests ---

// TestExecutor_CreatesStepBeads verifies executor creates step beads matching formula phases.
func TestExecutor_CreatesStepBeads(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", dir)

	created := make(map[string]string) // phase -> id
	nextID := 1

	formula := &FormulaV2{
		Name:    "test-formula",
		Version: 2,
		Phases: map[string]PhaseConfig{
			"design":    {Role: "wizard"},
			"implement": {Role: "apprentice"},
			"review":    {Role: "sage"},
			"merge":     {Role: "skip"},
		},
	}

	e := &formulaExecutor{
		beadID:    "spi-pour",
		agentName: "wizard-pour",
		formula:   formula,
		state: &executorState{
			BeadID:    "spi-pour",
			AgentName: "wizard-pour",
		},
		log: func(string, ...interface{}) {},
		stepCreator: func(parentID, stepName string) (string, error) {
			id := fmt.Sprintf("spi-pour.step-%d", nextID)
			nextID++
			created[stepName] = id
			return id, nil
		},
		stepActivator: func(stepID string) error {
			return nil
		},
		stepCloser: func(stepID string) error {
			return nil
		},
	}

	err := e.ensureStepBeads()
	if err != nil {
		t.Fatalf("ensureStepBeads: %v", err)
	}

	// Verify step beads were created for all enabled phases.
	enabledPhases := formula.EnabledPhases()
	if len(created) != len(enabledPhases) {
		t.Errorf("created %d step beads, want %d", len(created), len(enabledPhases))
	}

	for _, phase := range enabledPhases {
		if _, ok := created[phase]; !ok {
			t.Errorf("no step bead created for phase %s", phase)
		}
	}

	// Verify state has step bead IDs.
	if len(e.state.StepBeadIDs) != len(enabledPhases) {
		t.Errorf("state has %d step bead IDs, want %d", len(e.state.StepBeadIDs), len(enabledPhases))
	}
}

// TestExecutor_EnsureStepBeads_Idempotent verifies ensureStepBeads is a no-op on resume.
func TestExecutor_EnsureStepBeads_Idempotent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", dir)

	creatorCalled := false

	e := &formulaExecutor{
		beadID:    "spi-resume",
		agentName: "wizard-resume",
		formula: &FormulaV2{
			Name:    "test-formula",
			Version: 2,
			Phases: map[string]PhaseConfig{
				"implement": {Role: "apprentice"},
			},
		},
		state: &executorState{
			BeadID:    "spi-resume",
			AgentName: "wizard-resume",
			StepBeadIDs: map[string]string{
				"implement": "spi-resume.step-1",
			},
		},
		log: func(string, ...interface{}) {},
		stepCreator: func(parentID, stepName string) (string, error) {
			creatorCalled = true
			return "", fmt.Errorf("should not be called")
		},
	}

	err := e.ensureStepBeads()
	if err != nil {
		t.Fatalf("ensureStepBeads: %v", err)
	}
	if creatorCalled {
		t.Error("stepCreator should not be called when StepBeadIDs already populated")
	}
}

// --- Phase transition tests ---

// TestExecutor_TransitionStepBead verifies phase transitions close/activate step beads.
func TestExecutor_TransitionStepBead(t *testing.T) {
	var closed []string
	var activated []string

	e := &formulaExecutor{
		beadID:    "spi-trans",
		agentName: "wizard-trans",
		state: &executorState{
			StepBeadIDs: map[string]string{
				"design":    "spi-trans.step-1",
				"implement": "spi-trans.step-2",
				"review":    "spi-trans.step-3",
			},
		},
		log: func(string, ...interface{}) {},
		stepCloser: func(stepID string) error {
			closed = append(closed, stepID)
			return nil
		},
		stepActivator: func(stepID string) error {
			activated = append(activated, stepID)
			return nil
		},
	}

	// Transition from design to implement.
	e.transitionStepBead("design", "implement")

	if len(closed) != 1 || closed[0] != "spi-trans.step-1" {
		t.Errorf("closed = %v, want [spi-trans.step-1]", closed)
	}
	if len(activated) != 1 || activated[0] != "spi-trans.step-2" {
		t.Errorf("activated = %v, want [spi-trans.step-2]", activated)
	}

	// Transition from implement to review.
	e.transitionStepBead("implement", "review")

	if len(closed) != 2 || closed[1] != "spi-trans.step-2" {
		t.Errorf("closed = %v, want [spi-trans.step-1, spi-trans.step-2]", closed)
	}
	if len(activated) != 2 || activated[1] != "spi-trans.step-3" {
		t.Errorf("activated = %v, want [spi-trans.step-2, spi-trans.step-3]", activated)
	}
}

// TestExecutor_TransitionStepBead_FinalClose closes the last step bead with no next.
func TestExecutor_TransitionStepBead_FinalClose(t *testing.T) {
	var closed []string

	e := &formulaExecutor{
		state: &executorState{
			StepBeadIDs: map[string]string{
				"merge": "spi-final.step-5",
			},
		},
		log: func(string, ...interface{}) {},
		stepCloser: func(stepID string) error {
			closed = append(closed, stepID)
			return nil
		},
		stepActivator: func(stepID string) error {
			return nil
		},
	}

	// Final close (newPhase is empty).
	e.transitionStepBead("merge", "")

	if len(closed) != 1 || closed[0] != "spi-final.step-5" {
		t.Errorf("closed = %v, want [spi-final.step-5]", closed)
	}
}

// TestExecutor_TransitionStepBead_NoStepBeads is a no-op for legacy runs.
func TestExecutor_TransitionStepBead_NoStepBeads(t *testing.T) {
	called := false
	e := &formulaExecutor{
		state: &executorState{},
		log:   func(string, ...interface{}) {},
		stepCloser: func(stepID string) error {
			called = true
			return nil
		},
		stepActivator: func(stepID string) error {
			called = true
			return nil
		},
	}

	e.transitionStepBead("design", "implement")

	if called {
		t.Error("step operations should not be called when no step beads exist")
	}
}

// --- Board filtering tests ---

// TestBoard_FiltersStepBeads verifies step beads don't appear in board columns.
func TestBoard_FiltersStepBeads(t *testing.T) {
	openBeads := []BoardBead{
		{ID: "spi-task-1", Title: "Real task", Status: "open", Type: "task"},
		{ID: "spi-task-1.1", Title: "step:design", Status: "in_progress", Type: "task",
			Labels: []string{"workflow-step", "step:design"}},
		{ID: "spi-task-1.2", Title: "step:implement", Status: "open", Type: "task",
			Labels: []string{"workflow-step", "step:implement"}},
		{ID: "spi-task-2", Title: "Another task", Status: "open", Type: "task"},
	}
	closedBeads := []BoardBead{}
	blockedBeads := []BoardBead{}

	cols := categorizeColumnsFromStore(openBeads, closedBeads, blockedBeads, "")

	// Step beads should be filtered out.
	allCols := [][]BoardBead{
		cols.Ready, cols.Design, cols.Plan, cols.Implement,
		cols.Review, cols.Merge, cols.Done, cols.Blocked, cols.Alerts,
	}
	for _, col := range allCols {
		for _, b := range col {
			if isStepBoardBead(b) {
				t.Errorf("step bead %s should be filtered from board columns", b.ID)
			}
		}
	}

	// Real tasks should still be present.
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

// --- isStepBead / isStepBoardBead tests ---

func TestIsStepBead_WithLabel(t *testing.T) {
	b := Bead{Labels: []string{"workflow-step", "step:design"}}
	if !isStepBead(b) {
		t.Error("expected true for bead with workflow-step label")
	}
}

func TestIsStepBead_WithoutLabel(t *testing.T) {
	b := Bead{Title: "step:design", Labels: []string{"step:design"}}
	if isStepBead(b) {
		t.Error("expected false for bead without workflow-step label")
	}
}

func TestIsStepBoardBead_WithLabel(t *testing.T) {
	b := BoardBead{Labels: []string{"workflow-step", "step:implement"}}
	if !isStepBoardBead(b) {
		t.Error("expected true for board bead with workflow-step label")
	}
}

func TestIsStepBoardBead_WithoutLabel(t *testing.T) {
	b := BoardBead{Labels: []string{"step:implement"}}
	if isStepBoardBead(b) {
		t.Error("expected false for board bead without workflow-step label")
	}
}

// --- stepBeadPhaseName tests ---

func TestStepBeadPhaseName(t *testing.T) {
	b := Bead{Labels: []string{"workflow-step", "step:implement"}}
	name := stepBeadPhaseName(b)
	if name != "implement" {
		t.Errorf("stepBeadPhaseName = %q, want implement", name)
	}
}

func TestStepBeadPhaseName_NoLabel(t *testing.T) {
	b := Bead{Labels: []string{"workflow-step"}}
	name := stepBeadPhaseName(b)
	if name != "" {
		t.Errorf("stepBeadPhaseName = %q, want empty", name)
	}
}

// --- Fake step store for tests ---

type fakeStepStore struct {
	beads   map[string]Bead            // id -> bead
	stepIDs map[string]map[string]string // parentID -> phase -> stepID
	nextID  int
}

func newFakeStepStore() *fakeStepStore {
	return &fakeStepStore{
		beads:   make(map[string]Bead),
		stepIDs: make(map[string]map[string]string),
		nextID:  1,
	}
}

func (s *fakeStepStore) create(parentID, stepName string) (string, error) {
	id := fmt.Sprintf("%s.step-%d", parentID, s.nextID)
	s.nextID++
	b := Bead{
		ID:     id,
		Title:  "step:" + stepName,
		Status: "open",
		Labels: []string{"workflow-step", "step:" + stepName},
		Parent: parentID,
	}
	s.beads[id] = b
	if s.stepIDs[parentID] == nil {
		s.stepIDs[parentID] = make(map[string]string)
	}
	s.stepIDs[parentID][stepName] = id
	return id, nil
}

func (s *fakeStepStore) activate(stepID string) error {
	b, ok := s.beads[stepID]
	if !ok {
		return fmt.Errorf("step %s not found", stepID)
	}
	b.Status = "in_progress"
	s.beads[stepID] = b
	return nil
}

func (s *fakeStepStore) close(stepID string) error {
	b, ok := s.beads[stepID]
	if !ok {
		return fmt.Errorf("step %s not found", stepID)
	}
	b.Status = "closed"
	s.beads[stepID] = b
	return nil
}

func (s *fakeStepStore) getSteps(parentID string) []Bead {
	phaseMap := s.stepIDs[parentID]
	if phaseMap == nil {
		return nil
	}
	// Return in creation order (by ID suffix).
	var result []Bead
	for i := 1; i <= s.nextID; i++ {
		id := fmt.Sprintf("%s.step-%d", parentID, i)
		if b, ok := s.beads[id]; ok {
			result = append(result, b)
		}
	}
	return result
}

func (s *fakeStepStore) getActive(parentID string) *Bead {
	b, _ := s.getActiveErr(parentID)
	return b
}

func (s *fakeStepStore) getActiveErr(parentID string) (*Bead, error) {
	steps := s.getSteps(parentID)
	var active []Bead
	for _, step := range steps {
		if step.Status == "in_progress" {
			active = append(active, step)
		}
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
		return nil, fmt.Errorf("invariant violation: %d in_progress step beads for %s: %s",
			len(active), parentID, strings.Join(ids, ", "))
	}
}
