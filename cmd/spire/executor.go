package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// executorState is the persistent state for a formula executor.
type executorState struct {
	BeadID       string                  `json:"bead_id"`
	AgentName    string                  `json:"agent_name"`
	Formula      string                  `json:"formula"`
	Phase        string                  `json:"phase"`
	Wave         int                     `json:"wave"`
	Subtasks     map[string]subtaskState `json:"subtasks"`
	ReviewRounds int                     `json:"review_rounds"`
	StartedAt    string                  `json:"started_at"`
	LastActionAt string                  `json:"last_action_at"`
}

// formulaExecutor drives a bead through its formula's phase pipeline.
type formulaExecutor struct {
	beadID    string
	agentName string
	formula   *FormulaV2
	state     *executorState
	spawner   AgentBackend
	log       func(string, ...interface{})
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
	}, nil
}

// Run drives the bead through its formula's phase pipeline until all phases
// are complete or the bead is closed.
func (e *formulaExecutor) Run() error {
	defer wizardRegistryRemove(e.agentName)
	defer e.saveState()

	for {
		phase := e.state.Phase
		pc, ok := e.formula.Phases[phase]
		if !ok {
			return fmt.Errorf("phase %q not in formula %s", phase, e.formula.Name)
		}

		e.log("phase: %s (role: %s)", phase, pc.GetRole())
		setPhase(e.beadID, phase)
		e.saveState()

		// Merge phase has its own handler regardless of role.
		if phase == "merge" {
			if err := e.executeMerge(pc); err != nil {
				return fmt.Errorf("phase merge: %w", err)
			}
			break // merge is terminal
		}

		var err error
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
		case "skip":
			e.log("skipping phase %s", phase)
		default:
			err = fmt.Errorf("unknown role %q for phase %s", pc.GetRole(), phase)
		}

		if err != nil {
			return fmt.Errorf("phase %s: %w", phase, err)
		}

		// Advance to next phase
		if !e.advancePhase() {
			break // no more phases
		}

		// Check if bead is closed
		bead, err := storeGetBead(e.beadID)
		if err != nil {
			return fmt.Errorf("check bead: %w", err)
		}
		if bead.Status == "closed" {
			e.log("bead closed — exiting")
			return nil
		}
	}

	e.log("all phases complete")
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

// executeDirect spawns one apprentice for the bead.
func (e *formulaExecutor) executeDirect(phase string, pc PhaseConfig) error {
	apprenticeName := fmt.Sprintf("%s-impl", e.agentName)
	e.log("dispatching apprentice %s", apprenticeName)

	extraArgs := []string{}
	if pc.NoHandoff {
		extraArgs = append(extraArgs, "--no-handoff")
	}

	handle, err := e.spawner.Spawn(SpawnConfig{
		Name:      apprenticeName,
		BeadID:    e.beadID,
		Role:      RoleApprentice,
		ExtraArgs: extraArgs,
	})
	if err != nil {
		return fmt.Errorf("spawn apprentice: %w", err)
	}

	if err := handle.Wait(); err != nil {
		e.log("apprentice failed: %s", err)
		return fmt.Errorf("apprentice: %w", err)
	}

	e.log("apprentice completed")
	return nil
}

// executeWave dispatches apprentices in parallel waves using computeWaves.
func (e *formulaExecutor) executeWave(phase string, pc PhaseConfig) error {
	waves, err := computeWaves(e.beadID)
	if err != nil {
		return err
	}
	if len(waves) == 0 {
		e.log("no open subtasks")
		return nil
	}

	e.log("computed %d wave(s)", len(waves))

	// Resolve repo for build verification
	repoPath, _, _, err := wizardResolveRepo(e.beadID)
	if err != nil {
		return fmt.Errorf("resolve repo: %w", err)
	}

	// Create staging branch if configured
	if pc.StagingBranch != "" {
		branch := strings.ReplaceAll(pc.StagingBranch, "{bead-id}", e.beadID)
		e.log("creating staging branch %s", branch)
		exec.Command("git", "-C", repoPath, "checkout", "-B", branch).Run()
		storeAddLabel(e.beadID, "feat-branch:"+branch)
	}

	startWave := e.state.Wave
	for waveIdx := startWave; waveIdx < len(waves); waveIdx++ {
		wave := waves[waveIdx]
		e.state.Wave = waveIdx
		e.log("=== wave %d: %d subtask(s) ===", waveIdx, len(wave))

		type result struct {
			BeadID string
			Agent  string
			Err    error
		}

		var wg sync.WaitGroup
		resultCh := make(chan result, len(wave))

		for i, subtaskID := range wave {
			if st, ok := e.state.Subtasks[subtaskID]; ok && st.Status == "closed" {
				e.log("  %s already closed, skipping", subtaskID)
				continue
			}

			wg.Add(1)
			go func(idx int, beadID string) {
				defer wg.Done()
				name := fmt.Sprintf("%s-w%d-%d", e.agentName, waveIdx, idx)
				e.log("  dispatching %s for %s", name, beadID)

				extraArgs := []string{"--no-handoff"}
				h, spawnErr := e.spawner.Spawn(SpawnConfig{
					Name:      name,
					BeadID:    beadID,
					Role:      RoleApprentice,
					ExtraArgs: extraArgs,
				})
				if spawnErr != nil {
					resultCh <- result{BeadID: beadID, Agent: name, Err: spawnErr}
					return
				}
				if waitErr := h.Wait(); waitErr != nil {
					resultCh <- result{BeadID: beadID, Agent: name, Err: waitErr}
					return
				}
				resultCh <- result{BeadID: beadID, Agent: name}
			}(i, subtaskID)
		}

		wg.Wait()
		close(resultCh)

		// Collect results (single-threaded — no race)
		var errs []string
		for r := range resultCh {
			if r.Err != nil {
				errs = append(errs, fmt.Sprintf("%s: %s", r.BeadID, r.Err))
				continue
			}
			e.state.Subtasks[r.BeadID] = subtaskState{
				Status: "closed",
				Branch: fmt.Sprintf("feat/%s", r.BeadID),
				Agent:  r.Agent,
			}
		}

		e.saveState()

		if len(errs) > 0 {
			e.log("wave %d: %d error(s): %s", waveIdx, len(errs), strings.Join(errs, "; "))
		}

		// Verify build
		e.log("verifying build after wave %d", waveIdx)
		buildCmd := exec.Command("go", "build", "./cmd/spire/")
		buildCmd.Dir = repoPath
		buildCmd.Env = os.Environ()
		if out, buildErr := buildCmd.CombinedOutput(); buildErr != nil {
			e.log("build failed: %s\n%s", buildErr, string(out))
		}
	}

	return nil
}

// executeReview dispatches a sage for review and handles the verdict.
func (e *formulaExecutor) executeReview(phase string, pc PhaseConfig) error {
	sageName := fmt.Sprintf("%s-sage", e.agentName)
	e.log("dispatching sage %s", sageName)

	extraArgs := []string{}
	if pc.VerdictOnly {
		extraArgs = append(extraArgs, "--verdict-only")
	}

	handle, err := e.spawner.Spawn(SpawnConfig{
		Name:      sageName,
		BeadID:    e.beadID,
		Role:      RoleSage,
		ExtraArgs: extraArgs,
	})
	if err != nil {
		return fmt.Errorf("spawn sage: %w", err)
	}
	if err := handle.Wait(); err != nil {
		e.log("sage exited: %s — checking verdict", err)
	}

	// Read verdict from bead labels
	bead, err := storeGetBead(e.beadID)
	if err != nil {
		return fmt.Errorf("get bead: %w", err)
	}

	if containsLabel(bead, "review-approved") {
		e.log("approved")
		return nil // advance to next phase (merge)
	}

	if containsLabel(bead, "review-feedback") {
		e.state.ReviewRounds++
		e.log("request changes (round %d)", e.state.ReviewRounds)

		// Check max rounds
		revPolicy := e.formula.GetRevisionPolicy()
		if e.state.ReviewRounds >= revPolicy.MaxRounds {
			e.log("max rounds reached — escalating to arbiter")
			lastReview := &Review{Verdict: "request_changes", Summary: "Max review rounds reached"}
			return reviewEscalateToArbiter(e.beadID, sageName, lastReview, revPolicy, e.log)
		}

		// Judgment (if enabled): log agreement with sage
		if pc.Judgment {
			// Collect feedback from latest comment
			comments, _ := storeGetComments(e.beadID)
			for i := len(comments) - 1; i >= 0; i-- {
				if strings.Contains(comments[i].Text, "request_changes") || strings.Contains(comments[i].Text, "Review round") {
					break
				}
			}

			// Simple judgment: for now, always agree with sage
			// TODO: invoke Claude for judgment when session management is implemented
			e.log("judgment: agreeing with sage feedback")
			storeAddComment(e.beadID, fmt.Sprintf("Executor judgment (round %d): agree — accepting sage feedback", e.state.ReviewRounds))
		}

		// Go back to implement phase
		storeRemoveLabel(e.beadID, "review-feedback")

		// Find the implement phase to re-execute
		if implPC, ok := e.formula.Phases["implement"]; ok {
			setPhase(e.beadID, "implement")
			e.state.Phase = "implement"
			e.saveState()

			if implPC.GetDispatch() == "wave" {
				// For wave mode: re-running waves won't help (subtasks closed).
				// Spawn a single review-fix apprentice.
				fixName := fmt.Sprintf("%s-fix-%d", e.agentName, e.state.ReviewRounds)
				fh, ferr := e.spawner.Spawn(SpawnConfig{
					Name:      fixName,
					BeadID:    e.beadID,
					Role:      RoleApprentice,
					ExtraArgs: []string{"--review-fix"},
				})
				if ferr != nil {
					return fmt.Errorf("spawn review-fix: %w", ferr)
				}
				fh.Wait()
			} else {
				e.executeDirect("implement", implPC)
			}

			// Return to review
			setPhase(e.beadID, phase)
			e.state.Phase = phase
			return e.executeReview(phase, pc) // recurse for next round
		}

		return fmt.Errorf("no implement phase for review-fix cycle")
	}

	// Check if bead was closed by sage (shouldn't happen with verdict-only)
	if bead.Status == "closed" {
		e.log("bead closed by sage")
		return nil
	}

	return fmt.Errorf("no verdict found after sage review")
}

// executeMerge handles the merge phase: creates PR, squash-merges, closes bead.
func (e *formulaExecutor) executeMerge(pc PhaseConfig) error {
	bead, err := storeGetBead(e.beadID)
	if err != nil {
		return fmt.Errorf("get bead: %w", err)
	}

	branch := hasLabel(bead, "feat-branch:")
	if branch == "" {
		if pc.StagingBranch != "" {
			branch = strings.ReplaceAll(pc.StagingBranch, "{bead-id}", e.beadID)
		} else {
			branch = fmt.Sprintf("feat/%s", e.beadID)
		}
	}

	repoPath, _, baseBranch, err := wizardResolveRepo(e.beadID)
	if err != nil {
		return fmt.Errorf("resolve repo: %w", err)
	}

	e.log("merging %s → %s", branch, baseBranch)
	if err := reviewMerge(e.beadID, bead.Title, branch, baseBranch, repoPath, e.log); err != nil {
		return fmt.Errorf("merge: %w", err)
	}

	// Close the bead
	storeRemoveLabel(e.beadID, "review-approved")
	storeRemoveLabel(e.beadID, "feat-branch:"+branch)
	storeRemoveLabel(e.beadID, "phase:merge")
	if err := storeCloseBead(e.beadID); err != nil {
		e.log("warning: close bead: %s", err)
	}
	e.log("merged and closed")
	return nil
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
		path, ferr := FindFormula(formulaName)
		if ferr != nil {
			return fmt.Errorf("find formula %s: %w", formulaName, ferr)
		}
		formula, err = LoadFormulaV2(path)
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

	// Claim bead if not already
	bead, _ := storeGetBead(beadID)
	if bead.Status != "in_progress" {
		os.Setenv("SPIRE_IDENTITY", agentName)
		if cerr := cmdClaim([]string{beadID}); cerr != nil {
			return fmt.Errorf("claim: %w", cerr)
		}
	}

	spawner := ResolveBackend("")

	executor, execErr := newExecutor(beadID, agentName, formula, spawner)
	if execErr != nil {
		return execErr
	}

	return executor.Run()
}
