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

	"github.com/awell-health/spire/pkg/repoconfig"
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

	e.log("branch state resolved: repo=%s base=%s staging=%s",
		e.state.RepoPath, e.state.BaseBranch, e.state.StagingBranch)
	return e.saveState()
}

// Run drives the bead through its formula's phase pipeline until all phases
// are complete or the bead is closed.
func (e *formulaExecutor) Run() error {
	defer wizardRegistryRemove(e.agentName)
	defer e.saveState()

	// Resolve repo path, base branch, and staging branch once at startup.
	// All git operations read from e.state instead of computing independently.
	if err := e.resolveBranchState(); err != nil {
		return fmt.Errorf("resolve branch state: %w", err)
	}

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
		case "wizard":
			err = e.executeWizard(phase, pc)
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

// wizardValidateDesign checks that the epic has a linked design bead (discovered-from dep) that is
// closed and substantive. If missing or incomplete, labels the epic "needs-design"
// and pauses. If complete, advances.
func (e *formulaExecutor) wizardValidateDesign() error {
	// Find linked design beads via discovered-from deps
	deps, err := storeGetDepsWithMeta(e.beadID)
	if err != nil {
		return fmt.Errorf("get deps: %w", err)
	}

	var designBeads []Bead
	for _, dep := range deps {
		if string(dep.DependencyType) != string(beads.DepDiscoveredFrom) {
			continue
		}
		if dep.IssueType != "design" {
			continue
		}
		designBeads = append(designBeads, Bead{
			ID:          dep.ID,
			Title:       dep.Title,
			Description: dep.Description,
			Status:      string(dep.Status),
			Priority:    dep.Priority,
			Type:        string(dep.IssueType),
			Labels:      dep.Labels,
		})
	}

	if len(designBeads) == 0 {
		e.log("no linked design bead found — marking as needs-design")
		storeAddLabel(e.beadID, "needs-design")
		storeAddComment(e.beadID, "Wizard: no design bead linked. Create a design bead with `spire design`, then link it: `bd dep add "+e.beadID+" <design-id> --type discovered-from`")
		wizardMessageArchmage(e.agentName, e.beadID,
			fmt.Sprintf("Epic %s needs a design bead. No discovered-from dep found. Create one with `spire design`, then link it: `bd dep add %s <design-id> --type discovered-from`", e.beadID, e.beadID))
		return fmt.Errorf("epic %s has no linked design bead — label needs-design added", e.beadID)
	}

	// Check design bead completeness
	for _, db := range designBeads {
		if db.Status != "closed" {
			e.log("design bead %s is still open — waiting for it to be closed", db.ID)
			storeAddLabel(e.beadID, "needs-design")
			storeAddComment(e.beadID, fmt.Sprintf("Wizard: design bead %s is still open. Close it when the design is settled.", db.ID))
			wizardMessageArchmage(e.agentName, e.beadID,
				fmt.Sprintf("Epic %s is blocked: design bead %s is still open. Close it when the design is settled.", e.beadID, db.ID))
			return fmt.Errorf("design bead %s not yet closed", db.ID)
		}
	}

	// Check that design bead has substance (at least one comment)
	for _, db := range designBeads {
		comments, _ := storeGetComments(db.ID)
		if len(comments) == 0 && db.Description == "" {
			e.log("design bead %s has no content — needs enrichment", db.ID)
			storeAddLabel(e.beadID, "needs-design")
			storeAddComment(e.beadID, fmt.Sprintf("Wizard: design bead %s exists but has no content. Add design decisions as comments before proceeding.", db.ID))
			wizardMessageArchmage(e.agentName, e.beadID,
				fmt.Sprintf("Epic %s is blocked: design bead %s has no content. Add design decisions as comments before proceeding.", e.beadID, db.ID))
			return fmt.Errorf("design bead %s has no content", db.ID)
		}
	}

	// Design validated — remove needs-design label if present and log
	storeRemoveLabel(e.beadID, "needs-design")
	e.log("design validated: %d design bead(s) linked and closed", len(designBeads))
	storeAddComment(e.beadID, fmt.Sprintf("Wizard: design validated — %d design bead(s) linked and closed. Advancing to plan.", len(designBeads)))
	return nil
}

// wizardMessageArchmage sends a spire message to the archmage referencing the given bead.
// Errors are logged but do not block the caller.
func wizardMessageArchmage(from, beadID, message string) {
	labels := []string{"msg", "to:archmage", "from:" + from, "ref:" + beadID}
	if _, err := storeCreateBead(createOpts{
		Title:    message,
		Priority: 1,
		Type:     beads.TypeTask,
		Prefix:   "spi",
		Labels:   labels,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: message archmage: %s\n", err)
	}
}

// wizardPlan reads the design bead(s) and invokes Claude to break the epic into subtasks.
// It files the subtasks and posts the plan as a comment.
func (e *formulaExecutor) wizardPlan(pc PhaseConfig) error {
	bead, err := storeGetBead(e.beadID)
	if err != nil {
		return fmt.Errorf("get bead: %w", err)
	}

	// Collect design context from linked design beads
	var designContext strings.Builder
	for _, l := range bead.Labels {
		if strings.HasPrefix(l, "ref:") {
			refID := l[4:]
			refBead, refErr := storeGetBead(refID)
			if refErr != nil {
				continue
			}
			if refBead.Type == "design" {
				designContext.WriteString(fmt.Sprintf("--- Design bead %s: %s ---\n", refBead.ID, refBead.Title))
				if refBead.Description != "" {
					designContext.WriteString(refBead.Description + "\n")
				}
				comments, _ := storeGetComments(refBead.ID)
				for _, c := range comments {
					designContext.WriteString(fmt.Sprintf("[%s]: %s\n", c.Author, c.Text))
				}
				designContext.WriteString("\n")
			}
		}
	}

	// Also include epic description and comments
	epicContext := fmt.Sprintf("Epic: %s\nTitle: %s\nDescription: %s\n", bead.ID, bead.Title, bead.Description)
	epicComments, _ := storeGetComments(e.beadID)
	for _, c := range epicComments {
		epicContext += fmt.Sprintf("[%s]: %s\n", c.Author, c.Text)
	}

	// Check for existing children (resume case)
	children, _ := storeGetChildren(e.beadID)
	if len(children) > 0 {
		e.log("epic already has %d children — plan phase complete", len(children))
		return nil
	}

	prompt := fmt.Sprintf(`You are a Spire wizard planning an epic. Break the work into independent tasks that can be executed by parallel agents in isolated git worktrees.

All context is provided below — do NOT read files.

## Epic
%s

## Design Context
%s

## Planning rules

CRITICAL: Each task will be executed by a separate agent in its own git worktree. Agents CANNOT see each other's work. They run in parallel within a wave, then their branches are merged. Think carefully about:

1. SHARED FILES: If two tasks both need to modify the same file, they CANNOT be in the same wave. Put the foundational work (types, interfaces, shared utilities) in an earlier wave. Put consumers in a later wave.

2. DEPENDENCIES: If task B needs types/functions that task A creates, B depends on A. Use the "deps" field. Tasks with no deps run in the same wave (parallel).

3. NEGATIVE CONSTRAINTS: Each task must say what it does NOT do. If task A creates the query functions, task B's description must say "Do NOT create query functions — use the ones from task A." Without this, parallel agents will create conflicting implementations.

4. COMPLETE DESCRIPTIONS: Each task description must be self-contained. Include:
   - Exact file paths to create or modify
   - Function/component names and signatures
   - Expected behavior and edge cases
   - What files this task must NOT touch (handled by other tasks)
   Do NOT make agents read plan files or reference other tasks vaguely.

5. WAVE ORDERING: Wave 0 = shared foundations (types, queries, utilities). Wave 1 = features that consume wave 0. Wave 2 = integration, entry points, cross-cutting concerns.

## Output format

Output ONLY JSON objects, one per line, no other text. Each line:
{"title": "Short title", "description": "Complete task description with file paths, what to do, and what NOT to do", "deps": ["title-of-dependency"], "shared_files": ["path/to/shared/file.ts"], "do_not_touch": ["path/other-task-handles.ts"]}

- "shared_files": files this task creates/modifies that other tasks will consume (helps detect wave ordering issues)
- "do_not_touch": files this task must NOT modify (handled by another task)
- "deps": titles of tasks that must complete before this one starts
- Tasks with no deps run in parallel (same wave)
`, epicContext, designContext.String())

	model := pc.Model
	if model == "" {
		model = "claude-sonnet-4-6"
	}

	maxTurns := pc.GetMaxTurns()
	e.log("invoking Claude for plan generation (max_turns=%d)", maxTurns)
	cmd := exec.Command("claude",
		"--dangerously-skip-permissions",
		"-p", prompt,
		"--model", model,
		"--output-format", "text",
		"--max-turns", fmt.Sprintf("%d", maxTurns),
	)
	cmd.Dir = e.state.RepoPath
	cmd.Env = os.Environ()
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("claude plan: %w", err)
	}

	// Parse subtasks from output — extract JSON lines
	type planTask struct {
		Title       string   `json:"title"`
		Description string   `json:"description"`
		Deps        []string `json:"deps"`
		SharedFiles []string `json:"shared_files"`
		DoNotTouch  []string `json:"do_not_touch"`
	}

	var tasks []planTask
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var t planTask
		if jsonErr := json.Unmarshal([]byte(line), &t); jsonErr == nil && t.Title != "" {
			tasks = append(tasks, t)
		}
	}

	if len(tasks) == 0 {
		e.log("Claude produced no parseable subtasks — posting raw output as comment")
		storeAddComment(e.beadID, fmt.Sprintf("Wizard plan (raw):\n%s", string(out)))
		return fmt.Errorf("no subtasks parsed from plan output")
	}

	e.log("filing %d subtasks", len(tasks))

	// Create subtasks — enrich descriptions with coordination metadata
	titleToID := make(map[string]string)
	for _, t := range tasks {
		desc := t.Description
		if len(t.SharedFiles) > 0 {
			desc += "\n\nShared files (other tasks depend on these): " + strings.Join(t.SharedFiles, ", ")
		}
		if len(t.DoNotTouch) > 0 {
			desc += "\n\nDo NOT touch (handled by other tasks): " + strings.Join(t.DoNotTouch, ", ")
		}
		id, createErr := storeCreateBead(createOpts{
			Title:       t.Title,
			Description: desc,
			Priority:    bead.Priority,
			Type:        parseIssueType("task"),
			Parent:      e.beadID,
		})
		if createErr != nil {
			e.log("warning: create subtask %q: %s", t.Title, createErr)
			continue
		}
		titleToID[t.Title] = id
		e.log("  created %s: %s", id, t.Title)
	}

	// Add dependencies
	for _, t := range tasks {
		if len(t.Deps) == 0 {
			continue
		}
		taskID, ok := titleToID[t.Title]
		if !ok {
			continue
		}
		for _, depTitle := range t.Deps {
			depID, depOK := titleToID[depTitle]
			if !depOK {
				continue
			}
			storeAddDep(taskID, depID)
		}
	}

	// Post plan summary as comment
	var planSummary strings.Builder
	planSummary.WriteString(fmt.Sprintf("Wizard plan: %d subtasks\n\n", len(tasks)))
	for _, t := range tasks {
		id := titleToID[t.Title]
		deps := ""
		if len(t.Deps) > 0 {
			var depIDs []string
			for _, d := range t.Deps {
				if did, ok := titleToID[d]; ok {
					depIDs = append(depIDs, did)
				}
			}
			if len(depIDs) > 0 {
				deps = " ← " + strings.Join(depIDs, ", ")
			}
		}
		planSummary.WriteString(fmt.Sprintf("- %s: %s%s\n", id, t.Title, deps))
	}
	storeAddComment(e.beadID, planSummary.String())

	return nil
}

// wizardGeneric handles a wizard phase by invoking Claude with the bead context.
// Used for phases that don't have specific logic (future extensibility).
func (e *formulaExecutor) wizardGeneric(phase string, pc PhaseConfig) error {
	bead, err := storeGetBead(e.beadID)
	if err != nil {
		return fmt.Errorf("get bead: %w", err)
	}

	focusContext, _ := wizardCaptureFocus(e.beadID)

	model := pc.Model
	if model == "" {
		model = "claude-sonnet-4-6"
	}

	prompt := fmt.Sprintf(`You are a Spire wizard handling the %s phase for bead %s.

Task: %s
Description: %s

Focus context:
%s

Complete this phase and output your results.`, phase, bead.ID, bead.Title, bead.Description, focusContext)

	maxTurns := pc.GetMaxTurns()
	cmd := exec.Command("claude",
		"--dangerously-skip-permissions",
		"-p", prompt,
		"--model", model,
		"--output-format", "text",
		"--max-turns", fmt.Sprintf("%d", maxTurns),
	)
	cmd.Dir = e.state.RepoPath
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// executeDirect spawns one apprentice for the bead.
func (e *formulaExecutor) executeDirect(phase string, pc PhaseConfig) error {
	apprenticeName := fmt.Sprintf("%s-impl", e.agentName)
	e.log("dispatching apprentice %s", apprenticeName)

	extraArgs := []string{}
	if pc.Apprentice {
		extraArgs = append(extraArgs, "--apprentice")
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

	repoPath := e.state.RepoPath
	stagingBranch := e.state.StagingBranch

	// Create staging branch if configured
	if stagingBranch != "" {
		e.log("creating staging branch %s", stagingBranch)
		exec.Command("git", "-C", repoPath, "checkout", "-B", stagingBranch).Run()
		storeAddLabel(e.beadID, "feat-branch:"+stagingBranch)
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

				// Mark subtask as in_progress before dispatching
				storeUpdateBead(beadID, map[string]interface{}{"status": "in_progress"})

				extraArgs := []string{"--apprentice"}
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

		// Merge child branches into staging branch
		if stagingBranch != "" {
			exec.Command("git", "-C", repoPath, "checkout", stagingBranch).Run()
			for _, subtaskID := range wave {
				st, ok := e.state.Subtasks[subtaskID]
				if !ok || st.Status != "closed" || st.Branch == "" {
					continue
				}
				if mergeErr := e.mergeChildBranch(repoPath, st.Branch, stagingBranch); mergeErr != nil {
					return fmt.Errorf("merge %s into %s: %w", st.Branch, stagingBranch, mergeErr)
				}
			}
		}

		// Verify build using formula's build command, repo config fallback, or hardcoded default
		if buildStr := e.resolveBuildCommand(pc); buildStr != "" {
			e.log("verifying build after wave %d: %s", waveIdx, buildStr)
			if buildErr := e.runBuildCommand(repoPath, buildStr); buildErr != nil {
				return fmt.Errorf("build verification failed after wave %d: %w", waveIdx, buildErr)
			}
		}
	}

	// Switch back to base branch so the sage can create a worktree from the staging branch.
	// During wave merges we checked out the staging branch in the main worktree —
	// the sage needs it free to create its own worktree.
	if stagingBranch != "" {
		e.log("switching back to %s for review phase", e.state.BaseBranch)
		exec.Command("git", "-C", repoPath, "checkout", e.state.BaseBranch).Run()
	}

	return nil
}

// mergeChildBranch merges a child branch into the staging branch.
// On conflict, it invokes Claude to resolve the conflicts.
func (e *formulaExecutor) mergeChildBranch(repoPath, childBranch, stagingBranch string) error {
	e.log("  merging %s into %s", childBranch, stagingBranch)

	// Fetch in case the apprentice pushed to remote
	exec.Command("git", "-C", repoPath, "fetch", "origin", childBranch).Run()

	// Try remote branch first, fall back to local
	branchRef := "origin/" + childBranch
	mergeCmd := exec.Command("git", "-C", repoPath, "merge", "--no-edit", branchRef)
	if _, mergeErr := mergeCmd.CombinedOutput(); mergeErr != nil {
		// Try local branch
		branchRef = childBranch
		mergeCmd2 := exec.Command("git", "-C", repoPath, "merge", "--no-edit", branchRef)
		if _, mergeErr2 := mergeCmd2.CombinedOutput(); mergeErr2 != nil {
			// Merge conflict — check if git is in a merge state
			statusCmd := exec.Command("git", "-C", repoPath, "status", "--porcelain")
			statusOut, _ := statusCmd.Output()
			if strings.Contains(string(statusOut), "UU ") || strings.Contains(string(statusOut), "AA ") {
				// There are conflicts — resolve via Claude
				e.log("  conflict detected, invoking Claude to resolve")
				if resolveErr := e.resolveConflicts(repoPath, childBranch); resolveErr != nil {
					// Abort the merge if resolution fails
					exec.Command("git", "-C", repoPath, "merge", "--abort").Run()
					return fmt.Errorf("conflict resolution failed: %w", resolveErr)
				}
				return nil
			}
			// Not a conflict — some other merge error
			exec.Command("git", "-C", repoPath, "merge", "--abort").Run()
			return fmt.Errorf("merge failed: %w", mergeErr2)
		}
	}
	return nil
}

// resolveConflicts invokes Claude to resolve merge conflicts in the working tree.
func (e *formulaExecutor) resolveConflicts(repoPath, childBranch string) error {
	// Get the list of conflicted files
	diffCmd := exec.Command("git", "-C", repoPath, "diff", "--name-only", "--diff-filter=U")
	diffOut, err := diffCmd.Output()
	if err != nil {
		return fmt.Errorf("list conflicts: %w", err)
	}
	conflictedFiles := strings.TrimSpace(string(diffOut))
	if conflictedFiles == "" {
		return fmt.Errorf("no conflicted files found")
	}

	// Build a prompt with the conflicts
	prompt := fmt.Sprintf(`You are resolving merge conflicts for branch %s being merged into the staging branch.

The following files have conflicts. For each file, read it, resolve the conflict markers (<<<<<<< ======= >>>>>>>), and write the resolved version. Keep both sides' changes where they don't contradict. When they do contradict, prefer the incoming branch (%s) since it has the newer implementation.

Conflicted files:
%s

After resolving all conflicts, stage them with git add.
Do NOT commit — the merge commit will be created automatically.`,
		childBranch, childBranch, conflictedFiles)

	// Invoke Claude to resolve
	cmd := exec.Command("claude",
		"--dangerously-skip-permissions",
		"-p", prompt,
		"--model", "claude-sonnet-4-6",
		"--max-turns", "10",
	)
	cmd.Dir = repoPath
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("claude resolve: %w", err)
	}

	// Verify all conflicts are resolved (no more conflict markers)
	statusCmd := exec.Command("git", "-C", repoPath, "status", "--porcelain")
	statusOut, _ := statusCmd.Output()
	if strings.Contains(string(statusOut), "UU ") {
		return fmt.Errorf("conflicts still unresolved after Claude")
	}

	// Complete the merge
	commitCmd := exec.Command("git", "-C", repoPath, "commit", "--no-edit")
	if out, commitErr := commitCmd.CombinedOutput(); commitErr != nil {
		return fmt.Errorf("commit merge: %s\n%s", commitErr, string(out))
	}

	e.log("  conflicts resolved by Claude")
	return nil
}

// resolveBuildCommand returns the build command to use for verification.
// Resolution order:
//  1. Current phase's Build field
//  2. Implement phase's Build field (build is most commonly configured there)
//  3. Repo config runtime.build (spire.yaml)
//  4. Empty string (no build verification)
func (e *formulaExecutor) resolveBuildCommand(pc PhaseConfig) string {
	// 1. Current phase config
	if pc.Build != "" {
		return pc.Build
	}
	// 2. Implement phase fallback (build commands live here for wave-based formulas)
	if impl, ok := e.formula.Phases["implement"]; ok && impl.Build != "" {
		return impl.Build
	}
	// 3. Repo config fallback
	if cfg, err := repoconfig.Load(e.state.RepoPath); err == nil && cfg.Runtime.Build != "" {
		return cfg.Runtime.Build
	}
	return ""
}

// runBuildCommand executes a build command string in the given repo directory.
// The command is split on spaces and run directly (no shell).
func (e *formulaExecutor) runBuildCommand(repoPath, buildStr string) error {
	parts := strings.Fields(buildStr)
	if len(parts) == 0 {
		return nil
	}
	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Dir = repoPath
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.log("build failed: %s\n%s", err, string(out))
		return fmt.Errorf("%s: %w\n%s", buildStr, err, string(out))
	}
	e.log("build passed")
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
					ExtraArgs: []string{"--review-fix", "--apprentice"},
				})
				if ferr != nil {
					return fmt.Errorf("spawn review-fix: %w", ferr)
				}
				if waitErr := fh.Wait(); waitErr != nil {
					return fmt.Errorf("review-fix apprentice failed: %w", waitErr)
				}

				// Merge fix branch into staging so the sage reviews the updated code.
				// Without this, the fix lands on feat/<bead-id> but the staging branch
				// (which gets merged to main) never gets the fix.
				if e.state.StagingBranch != "" {
					fixBranch := fmt.Sprintf("feat/%s", e.beadID)
					e.log("merging fix branch %s into staging %s", fixBranch, e.state.StagingBranch)
					exec.Command("git", "-C", e.state.RepoPath, "checkout", e.state.StagingBranch).Run()
					if mergeErr := e.mergeChildBranch(e.state.RepoPath, fixBranch, e.state.StagingBranch); mergeErr != nil {
						e.log("warning: merge fix into staging: %s", mergeErr)
					}
					exec.Command("git", "-C", e.state.RepoPath, "checkout", e.state.BaseBranch).Run()
				}
			} else {
				if dirErr := e.executeDirect("implement", implPC); dirErr != nil {
					return fmt.Errorf("review-fix direct failed: %w", dirErr)
				}
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

// executeMerge handles the merge phase: ff-only merge of staging branch into main.
// If main has moved ahead, it rebases the staging branch onto main in a temporary
// worktree, re-verifies the build, then retries the ff-only merge. Never force merges.
func (e *formulaExecutor) executeMerge(pc PhaseConfig) error {
	bead, err := storeGetBead(e.beadID)
	if err != nil {
		return fmt.Errorf("get bead: %w", err)
	}

	branch := hasLabel(bead, "feat-branch:")
	if branch == "" {
		if e.state.StagingBranch != "" {
			branch = e.state.StagingBranch
		} else {
			branch = fmt.Sprintf("feat/%s", e.beadID)
		}
	}

	repoPath := e.state.RepoPath
	baseBranch := e.state.BaseBranch

	// Load archmage identity for the push.
	var mergeEnv []string
	if tower, tErr := activeTowerConfig(); tErr == nil && tower != nil {
		mergeEnv = archmageGitEnv(tower)
	} else {
		mergeEnv = os.Environ()
	}

	// Run build verification on the staging/feature branch before merging to main.
	// This catches type errors, bad imports, and missing exports from merged code.
	if buildStr := e.resolveBuildCommand(pc); buildStr != "" {
		e.log("verifying build on %s before merge: %s", branch, buildStr)
		// Checkout the staging/feature branch to run the build
		if out, chkErr := exec.Command("git", "-C", repoPath, "checkout", branch).CombinedOutput(); chkErr != nil {
			return fmt.Errorf("checkout %s for build verification: %s\n%s", branch, chkErr, string(out))
		}
		if buildErr := e.runBuildCommand(repoPath, buildStr); buildErr != nil {
			// Switch back to base branch before returning the error
			exec.Command("git", "-C", repoPath, "checkout", baseBranch).Run()
			return fmt.Errorf("pre-merge build verification failed on %s: %w", branch, buildErr)
		}
	}

	// Review documentation for stale language before merging to main.
	// Parallel workers write docs against pre-merge code — READMEs may say
	// "planned" or "not yet implemented" for features that now exist.
	if docErr := e.reviewDocsForStaleness(repoPath, branch, baseBranch, pc); docErr != nil {
		e.log("warning: doc review: %s", docErr)
		// Non-fatal — proceed with merge even if doc review fails.
	}

	// Local merge: checkout main, merge the feature/staging branch, push
	e.log("merging %s → %s (local, committer: archmage)", branch, baseBranch)

	if out, err := exec.Command("git", "-C", repoPath, "checkout", baseBranch).CombinedOutput(); err != nil {
		return fmt.Errorf("checkout %s: %s\n%s", baseBranch, err, string(out))
	}

	// Ensure main is up to date before attempting ff-only merge.
	e.log("pulling %s before merge", baseBranch)
	pullCmd := exec.Command("git", "-C", repoPath, "pull", "--ff-only", "origin", baseBranch)
	pullCmd.Env = mergeEnv
	if out, pullErr := pullCmd.CombinedOutput(); pullErr != nil {
		e.log("warning: pull %s: %s\n%s", baseBranch, pullErr, string(out))
	}

	// The main worktree is already on baseBranch (executeWave switches back).
	// Verify we're on the right branch.
	headRef, _ := exec.Command("git", "-C", repoPath, "symbolic-ref", "--short", "HEAD").Output()
	if strings.TrimSpace(string(headRef)) != baseBranch {
		if out, err := exec.Command("git", "-C", repoPath, "checkout", baseBranch).CombinedOutput(); err != nil {
			return fmt.Errorf("checkout %s: %s\n%s", baseBranch, err, string(out))
		}
	}

	e.log("ff-only merge %s → %s (committer: archmage)", branch, baseBranch)

	// First attempt: fast-forward only merge from the main worktree.
	ffCmd := exec.Command("git", "-C", repoPath, "merge", "--ff-only", branch)
	ffCmd.Env = mergeEnv
	if out, ffErr := ffCmd.CombinedOutput(); ffErr != nil {
		e.log("ff-only failed: %s — rebasing staging onto %s", strings.TrimSpace(string(out)), baseBranch)

		// ff-only failed — main has diverged. Rebase staging onto main in a
		// temporary worktree so we don't disturb the main worktree's checkout.
		tmpDir, err := os.MkdirTemp("", fmt.Sprintf("spire-rebase-%s-", e.beadID))
		if err != nil {
			return fmt.Errorf("create temp dir: %w", err)
		}
		defer os.RemoveAll(tmpDir)

		wtPath := filepath.Join(tmpDir, "staging")

		// Create a worktree checking out the staging branch.
		if out, wtErr := exec.Command("git", "-C", repoPath, "worktree", "add", wtPath, branch).CombinedOutput(); wtErr != nil {
			return fmt.Errorf("create staging worktree: %s\n%s", wtErr, string(out))
		}
		defer func() {
			exec.Command("git", "-C", repoPath, "worktree", "remove", "--force", wtPath).Run()
		}()

		// Rebase the staging branch onto main.
		e.log("rebasing %s onto %s in staging worktree", branch, baseBranch)
		rebaseCmd := exec.Command("git", "-C", wtPath, "rebase", baseBranch)
		rebaseCmd.Env = os.Environ()
		if out, rbErr := rebaseCmd.CombinedOutput(); rbErr != nil {
			// Abort the rebase — never force merge.
			exec.Command("git", "-C", wtPath, "rebase", "--abort").Run()
			return fmt.Errorf("rebase %s onto %s failed (aborting, will not force merge): %s\n%s", branch, baseBranch, rbErr, string(out))
		}

		// Re-verify build in the staging worktree after rebase.
		e.log("verifying build after rebase")
		buildCmd := exec.Command("go", "build", "./cmd/spire/")
		buildCmd.Dir = wtPath
		buildCmd.Env = os.Environ()
		if out, buildErr := buildCmd.CombinedOutput(); buildErr != nil {
			return fmt.Errorf("build failed after rebase (aborting merge): %s\n%s", buildErr, string(out))
		}

		// Run tests after rebase.
		e.log("running tests after rebase")
		testCmd := exec.Command("go", "test", "./cmd/spire/")
		testCmd.Dir = wtPath
		testCmd.Env = os.Environ()
		if out, testErr := testCmd.CombinedOutput(); testErr != nil {
			return fmt.Errorf("tests failed after rebase (aborting merge): %s\n%s", testErr, string(out))
		}

		// Remove the worktree before retrying the merge (the branch ref is
		// already updated by the rebase — the worktree just holds a checkout).
		exec.Command("git", "-C", repoPath, "worktree", "remove", "--force", wtPath).Run()

		// Second attempt: ff-only should now succeed since staging was rebased.
		e.log("retrying ff-only merge after rebase")
		ffCmd2 := exec.Command("git", "-C", repoPath, "merge", "--ff-only", branch)
		ffCmd2.Env = mergeEnv
		if out, ffErr2 := ffCmd2.CombinedOutput(); ffErr2 != nil {
			return fmt.Errorf("ff-only merge failed even after rebase (will not force merge): %s\n%s", ffErr2, string(out))
		}
	}

	// Push main (with archmage identity)
	e.log("pushing %s", baseBranch)
	pushCmd := exec.Command("git", "-C", repoPath, "push", "origin", baseBranch)
	pushCmd.Env = mergeEnv
	if out, pushErr := pushCmd.CombinedOutput(); pushErr != nil {
		return fmt.Errorf("push %s: %s\n%s", baseBranch, pushErr, string(out))
	}

	// Clean up the feature/staging branch
	exec.Command("git", "-C", repoPath, "branch", "-d", branch).Run()
	exec.Command("git", "-C", repoPath, "push", "origin", "--delete", branch).Run()

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

// reviewDocsForStaleness checks documentation files modified on the staging branch
// for stale language ("planned", "TODO", "not yet implemented", "will be") that
// refers to functionality now present in the merged code. If stale docs are found,
// Claude fixes them and commits the changes on the staging branch.
func (e *formulaExecutor) reviewDocsForStaleness(repoPath, branch, baseBranch string, pc PhaseConfig) error {
	// Ensure we're on the staging/feature branch for the diff and potential commit.
	if out, err := exec.Command("git", "-C", repoPath, "checkout", branch).CombinedOutput(); err != nil {
		return fmt.Errorf("checkout %s for doc review: %s\n%s", branch, err, string(out))
	}

	// Find files changed relative to the base branch.
	diffCmd := exec.Command("git", "-C", repoPath, "diff", baseBranch, "--name-only")
	diffOut, err := diffCmd.Output()
	if err != nil {
		return fmt.Errorf("git diff --name-only: %w", err)
	}

	// Filter for documentation files.
	var docFiles []string
	for _, f := range strings.Split(strings.TrimSpace(string(diffOut)), "\n") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		base := strings.ToUpper(filepath.Base(f))
		switch {
		case base == "README.MD":
			docFiles = append(docFiles, f)
		case base == "PLAYBOOK.MD":
			docFiles = append(docFiles, f)
		case base == "ARCHITECTURE.MD":
			docFiles = append(docFiles, f)
		case base == "VISION.MD":
			docFiles = append(docFiles, f)
		case base == "PLAN.MD":
			docFiles = append(docFiles, f)
		case base == "LOCAL.MD":
			docFiles = append(docFiles, f)
		case base == "CLAUDE.MD":
			docFiles = append(docFiles, f)
		case strings.HasSuffix(strings.ToLower(f), ".md") && strings.Contains(strings.ToLower(filepath.Dir(f)), "doc"):
			// Any .md file under a docs/ directory
			docFiles = append(docFiles, f)
		}
	}

	if len(docFiles) == 0 {
		e.log("no documentation files changed — skipping doc review")
		return nil
	}

	e.log("reviewing %d documentation file(s) for stale language: %s", len(docFiles), strings.Join(docFiles, ", "))

	// Build a prompt that asks Claude to review and fix stale language.
	prompt := fmt.Sprintf(`You are reviewing documentation files after code branches have been merged into a staging branch. Parallel workers wrote these docs against pre-merge code. Some docs may say "planned", "TODO", "not yet implemented", "will be added", "coming soon", or similar language for features that NOW EXIST in the merged code.

Your job:
1. Read each documentation file listed below.
2. For each file, check if it contains stale language — phrases like "planned", "TODO", "not yet implemented", "will be", "coming soon", "future work", "not yet supported" — that refers to functionality that is NOW present in the codebase.
3. To determine what is actually implemented, look at the actual source code files (not just docs).
4. If you find stale language, fix it to reflect the current state of the code. Change "will be implemented" to "is implemented", remove "TODO" items that are done, etc.
5. If no fixes are needed, do nothing — do NOT make unnecessary changes.
6. If you made any changes, stage them with git add and commit with the message: docs: fix stale documentation after merge

Documentation files to review:
%s

IMPORTANT: Only fix genuinely stale language where the described feature now exists in code. Do NOT remove TODOs for things that are actually still pending. Be conservative — when in doubt, leave it alone.`, strings.Join(docFiles, "\n"))

	model := pc.Model
	if model == "" {
		model = "claude-sonnet-4-6"
	}

	cmd := exec.Command("claude",
		"--dangerously-skip-permissions",
		"-p", prompt,
		"--model", model,
		"--output-format", "text",
		"--max-turns", "3",
	)
	cmd.Dir = repoPath
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("claude doc review: %w", err)
	}

	e.log("documentation review complete")
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
