package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/beads"
)

// executorState is the persistent state for a formula executor.
type executorState struct {
	BeadID        string                  `json:"bead_id"`
	AgentName     string                  `json:"agent_name"`
	Formula       string                  `json:"formula"`
	Phase         string                  `json:"phase"`
	Wave          int                     `json:"wave"`
	Subtasks      map[string]subtaskState `json:"subtasks"`
	ReviewRounds  int                     `json:"review_rounds"`
	StartedAt     string                  `json:"started_at"`
	LastActionAt  string                  `json:"last_action_at"`
	StagingBranch string                  `json:"staging_branch,omitempty"`
	BaseBranch    string                  `json:"base_branch,omitempty"`
	RepoPath      string                  `json:"repo_path,omitempty"`
	AttemptBeadID string                  `json:"attempt_bead_id,omitempty"`
	StepBeadIDs   map[string]string       `json:"step_bead_ids,omitempty"` // phase name → step bead ID
	WorktreeDir   string                  `json:"worktree_dir,omitempty"`  // staging worktree directory path
}

// formulaExecutor drives a bead through its formula's phase pipeline.
type formulaExecutor struct {
	beadID    string
	agentName string
	formula   *FormulaV2
	state     *executorState
	spawner   AgentBackend
	log       func(string, ...interface{})

	// Injectable store/exec operations — set to real implementations by newExecutor.
	// Replaced in tests to avoid requiring a live dolt server.
	beadGetter           func(id string) (Bead, error)
	childGetter          func(parentID string) ([]Bead, error)
	commentGetter        func(id string) ([]*beads.Comment, error)
	commentAdder         func(id, text string) error
	claudeRunner         func(args []string, dir string) ([]byte, error)
	attemptCreator       func(parentID, agentName, model, branch string) (string, error)
	attemptCloser        func(attemptID, result string) error
	activeAttemptGetter  func(parentID string) (*Bead, error)

	// Step bead operations — injectable for testing.
	stepCreator   func(parentID, stepName string) (string, error)
	stepActivator func(stepID string) error
	stepCloser    func(stepID string) error

	// Single staging worktree shared across all phases (implement, review, merge).
	// Created once by ensureStagingWorktree(), cleaned up by Run() on exit.
	stagingWt *StagingWorktree
}

// newExecutor creates a formula executor for a bead.
// It loads or creates state, registers with the wizard registry, and resolves the formula.
func newExecutor(beadID, agentName string, formula *FormulaV2, spawner AgentBackend) (*formulaExecutor, error) {
	log := func(format string, a ...interface{}) {
		fmt.Fprintf(os.Stderr, "[%s] %s\n", agentName, fmt.Sprintf(format, a...))
	}

	// Load or create state
	state, err := loadExecutorState(agentName)
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}
	if state == nil {
		// Detect current phase from bead
		bead, err := storeGetBead(beadID)
		if err != nil {
			return nil, fmt.Errorf("get bead: %w", err)
		}
		phase := getPhase(bead)
		if phase == "" {
			// Start at first enabled phase
			enabled := formula.EnabledPhases()
			if len(enabled) > 0 {
				phase = enabled[0]
			} else {
				return nil, fmt.Errorf("formula %s has no enabled phases", formula.Name)
			}
		}
		state = &executorState{
			BeadID:    beadID,
			AgentName: agentName,
			Formula:   formula.Name,
			Phase:     phase,
			Subtasks:  make(map[string]subtaskState),
			StartedAt: time.Now().UTC().Format(time.RFC3339),
		}
	}

	// Register with wizard registry for inbox delivery
	wizardRegistryAdd(localWizard{
		Name:      agentName,
		PID:       os.Getpid(),
		BeadID:    beadID,
		StartedAt: state.StartedAt,
		Phase:     state.Phase,
	})

	return &formulaExecutor{
		beadID:    beadID,
		agentName: agentName,
		formula:   formula,
		state:     state,
		spawner:   spawner,
		log:       log,
		beadGetter:    storeGetBead,
		childGetter:   storeGetChildren,
		commentGetter: storeGetComments,
		commentAdder:  storeAddComment,
		claudeRunner: func(args []string, dir string) ([]byte, error) {
			cmd := exec.Command("claude", args...)
			cmd.Dir = dir
			cmd.Env = os.Environ()
			cmd.Stderr = os.Stderr
			return cmd.Output()
		},
		attemptCreator:      storeCreateAttemptBead,
		attemptCloser:       storeCloseAttemptBead,
		activeAttemptGetter: storeGetActiveAttempt,
		stepCreator:         storeCreateStepBead,
		stepActivator:       storeActivateStepBead,
		stepCloser:          storeCloseStepBead,
	}, nil
}

// resolveBranchState resolves repo path, base branch, and staging branch once
// and stores them in the executor state. All git operations read from state
// instead of computing these independently.
func (e *formulaExecutor) resolveBranchState() error {
	// Already resolved (e.g. resumed from persisted state) — skip.
	if e.state.RepoPath != "" && e.state.BaseBranch != "" {
		e.log("branch state loaded from persisted state: repo=%s base=%s staging=%s",
			e.state.RepoPath, e.state.BaseBranch, e.state.StagingBranch)
		return nil
	}

	repoPath, _, baseBranch, err := wizardResolveRepo(e.beadID)
	if err != nil {
		return fmt.Errorf("resolve repo: %w", err)
	}
	if repoPath == "" {
		repoPath = "."
	}
	if baseBranch == "" {
		baseBranch = "main"
	}

	e.state.RepoPath = repoPath
	e.state.BaseBranch = baseBranch

	// Resolve staging branch from the implement phase config (if any).
	// The staging branch template lives in the implement phase's StagingBranch
	// field but is also referenced by merge and review-fix paths.
	for _, phaseName := range e.formula.EnabledPhases() {
		pc, ok := e.formula.Phases[phaseName]
		if ok && pc.StagingBranch != "" {
			e.state.StagingBranch = strings.ReplaceAll(pc.StagingBranch, "{bead-id}", e.beadID)
			break
		}
	}

	// Default staging branch to staging/<bead-id> if no formula override.
	// Every branch in the system is traceable to a bead — no generic names.
	if e.state.StagingBranch == "" {
		e.state.StagingBranch = "staging/" + e.beadID
	}

	e.log("branch state resolved: repo=%s base=%s staging=%s",
		e.state.RepoPath, e.state.BaseBranch, e.state.StagingBranch)
	return e.saveState()
}

// Run drives the bead through its formula's phase pipeline until all phases
// are complete or the bead is closed.
func (e *formulaExecutor) Run() error {
	defer wizardRegistryRemove(e.agentName)
	defer e.saveState()

	// Clean up the staging worktree on exit. Deferred early so it runs
	// before state save (defers execute LIFO).
	defer e.closeStagingWorktree()

	// Create attempt bead at start (or resume existing one).
	if err := e.ensureAttemptBead(); err != nil {
		e.log("warning: create attempt bead: %s", err)
		// Non-fatal — proceed without attempt tracking.
	}

	// Ensure attempt is closed on all exit paths (success, failure, panic).
	defer func() {
		if e.state.AttemptBeadID != "" {
			if cerr := e.attemptCloser(e.state.AttemptBeadID, "executor exited"); cerr != nil {
				e.log("warning: close attempt bead: %s", cerr)
			}
			e.state.AttemptBeadID = ""
		}
	}()

	// Resolve repo path, base branch, and staging branch once at startup.
	// All git operations read from e.state instead of computing independently.
	if err := e.resolveBranchState(); err != nil {
		e.closeAttempt("failure: repo-resolution: " + err.Error())
		escalateHumanFailure(e.beadID, e.agentName, "repo-resolution", err.Error())
		return fmt.Errorf("resolve branch state: %w", err)
	}

	// Create workflow step beads for each formula phase (or resume existing ones).
	if err := e.ensureStepBeads(); err != nil {
		e.log("warning: create step beads: %s", err)
		// Non-fatal — proceed without step bead tracking.
	}

	for {
		phase := e.state.Phase
		pc, ok := e.formula.Phases[phase]
		if !ok {
			e.closeAttempt(fmt.Sprintf("failure: unknown phase %q", phase))
			return fmt.Errorf("phase %q not in formula %s", phase, e.formula.Name)
		}

		e.log("phase: %s (role: %s)", phase, pc.GetRole())
		e.saveState()

		// Dispatch by behavior first (if set), then fall through to role.
		var err error
		behavior := pc.GetBehavior()

		switch {
		// --- Behavior-based dispatch (formula-driven) ---
		case behavior == "validate-design":
			err = e.wizardValidateDesign()
		case behavior == "generate-subtasks":
			err = e.wizardPlan(pc)
		case behavior == "enrich-subtasks":
			children, _ := e.childGetter(e.beadID)
			err = e.enrichSubtasksWithChangeSpecs(children, "", "", pc)
		case behavior == "sage-review":
			err = e.executeReview(phase, pc)
		case behavior == "auto-approve":
			e.log("auto-approve: skipping review")
		case behavior == "merge-to-main":
			if mergeErr := e.executeMerge(pc); mergeErr != nil {
				e.closeAttempt("failure: merge: " + mergeErr.Error())
				escalateHumanFailure(e.beadID, e.agentName, "merge-failure", mergeErr.Error())
				return fmt.Errorf("phase %s: %w", phase, mergeErr)
			}
			e.transitionStepBead(phase, "")
			e.closeAttempt("success: merged")
			return nil // merge is terminal
		case behavior == "skip":
			e.log("skipping phase %s (behavior: skip)", phase)

		// --- Role-based dispatch (legacy / default) ---
		case behavior == "" && phase == "merge":
			// Default merge behavior when no behavior set
			if mergeErr := e.executeMerge(pc); mergeErr != nil {
				e.closeAttempt("failure: merge: " + mergeErr.Error())
				escalateHumanFailure(e.beadID, e.agentName, "merge-failure", mergeErr.Error())
				return fmt.Errorf("phase merge: %w", mergeErr)
			}
			e.transitionStepBead(phase, "")
			e.closeAttempt("success: merged")
			return nil // merge is terminal
		case behavior == "":
			switch pc.GetRole() {
			case "human":
				err = e.waitForHuman(phase)
			case "apprentice":
				if pc.GetDispatch() == "wave" {
					err = e.executeWave(phase, pc)
				} else {
					err = e.executeDirect(phase, pc)
				}
			case "sage":
				err = e.executeReview(phase, pc)
			case "wizard":
				err = e.executeWizard(phase, pc)
			case "skip":
				e.log("skipping phase %s", phase)
			default:
				err = fmt.Errorf("unknown role %q for phase %s", pc.GetRole(), phase)
			}
		default:
			err = fmt.Errorf("unknown behavior %q for phase %s", behavior, phase)
		}

		if err != nil {
			e.closeAttempt(fmt.Sprintf("failure: phase %s: %s", phase, err.Error()))
			return fmt.Errorf("phase %s: %w", phase, err)
		}

		// Advance to next phase
		prevPhase := e.state.Phase
		if !e.advancePhase() {
			// Close the final step bead.
			e.transitionStepBead(prevPhase, "")
			break // no more phases
		}
		// Transition step beads: close previous, activate next.
		e.transitionStepBead(prevPhase, e.state.Phase)

		// Check if bead is closed
		bead, err := storeGetBead(e.beadID)
		if err != nil {
			e.closeAttempt("failure: check bead: " + err.Error())
			return fmt.Errorf("check bead: %w", err)
		}
		if bead.Status == "closed" {
			e.log("bead closed — exiting")
			e.closeAttempt("success: bead closed")
			return nil
		}
	}

	e.log("all phases complete")
	e.closeAttempt("success: all phases complete")
	// Clean up state file on success to avoid stale state on agent name reuse
	os.Remove(executorStatePath(e.agentName))
	return nil
}

// waitForHuman blocks the executor until the human transitions the phase.
func (e *formulaExecutor) waitForHuman(phase string) error {
	e.log("phase %s requires human action", phase)
	e.log("when ready, transition the phase and re-run:")
	e.log("  bd label remove %s \"phase:%s\"", e.beadID, phase)
	next := e.nextPhase(phase)
	if next != "" {
		e.log("  bd label add %s \"phase:%s\"", e.beadID, next)
	}
	return fmt.Errorf("waiting for human to complete %s phase", phase)
}

// executeWizard handles phases where the wizard (orchestrator) acts directly.
// The wizard invokes Claude for judgment/planning tasks rather than dispatching sub-agents.
func (e *formulaExecutor) executeWizard(phase string, pc PhaseConfig) error {
	switch phase {
	case "design":
		return e.wizardValidateDesign()
	case "plan":
		return e.wizardPlan(pc)
	default:
		// Generic wizard phase: invoke Claude with bead context
		return e.wizardGeneric(phase, pc)
	}
}

// --- State persistence ---

func executorStatePath(agentName string) string {
	dir, _ := configDir()
	return filepath.Join(dir, "runtime", agentName, "state.json")
}

func loadExecutorState(agentName string) (*executorState, error) {
	path := executorStatePath(agentName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var state executorState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func (e *formulaExecutor) saveState() error {
	path := executorStatePath(e.agentName)
	os.MkdirAll(filepath.Dir(path), 0755)
	e.state.LastActionAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(e.state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// --- Phase navigation ---

// advancePhase moves to the next enabled phase in the formula.
// Returns false if there are no more phases.
func (e *formulaExecutor) advancePhase() bool {
	next := e.nextPhase(e.state.Phase)
	if next == "" {
		return false
	}
	e.state.Phase = next
	return true
}

// nextPhase returns the next enabled phase after the given one, or "".
func (e *formulaExecutor) nextPhase(current string) string {
	enabled := e.formula.EnabledPhases()
	for i, p := range enabled {
		if p == current && i+1 < len(enabled) {
			return enabled[i+1]
		}
	}
	return ""
}

// --- Command entry point ---

// cmdExecute is the internal entry point for the formula executor.
// Usage: spire execute <bead-id> [--name <name>] [--formula <name>]
func cmdExecute(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: spire execute <bead-id> [--name <name>] [--formula <name>]")
	}

	beadID := args[0]
	agentName := "wizard-" + beadID
	formulaName := ""

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--name":
			if i+1 < len(args) {
				i++
				agentName = args[i]
			}
		case "--formula":
			if i+1 < len(args) {
				i++
				formulaName = args[i]
			}
		}
	}

	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	// Resolve formula
	var formula *FormulaV2
	var err error
	if formulaName != "" {
		formula, err = LoadFormulaByName(formulaName)
		if err != nil {
			return fmt.Errorf("load formula %s: %w", formulaName, err)
		}
	} else {
		bead, berr := storeGetBead(beadID)
		if berr != nil {
			return fmt.Errorf("get bead: %w", berr)
		}
		formula, err = ResolveFormula(bead)
	}
	if err != nil {
		return fmt.Errorf("load formula: %w", err)
	}

	// Skip claim when resuming an existing executor session.
	// loadExecutorState returns nil when no state file exists (fresh start).
	existingState, stateErr := loadExecutorState(agentName)
	if stateErr != nil {
		return fmt.Errorf("load state: %w", stateErr)
	}
	if existingState == nil {
		// Fresh start: claim bead if not already in progress.
		bead, _ := storeGetBead(beadID)
		if bead.Status != "in_progress" {
			os.Setenv("SPIRE_IDENTITY", agentName)
			if cerr := cmdClaim([]string{beadID}); cerr != nil {
				return fmt.Errorf("claim: %w", cerr)
			}
		}
	}

	spawner := ResolveBackend("")

	executor, execErr := newExecutor(beadID, agentName, formula, spawner)
	if execErr != nil {
		return execErr
	}

	return executor.Run()
}
