package executor

import (
	"fmt"
	"strconv"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/formula"
)

// executeReview replaces the hardcoded recursive review loop with a
// formula-driven step graph walk. The review-phase formula declares the
// step graph (sage-review, fix, arbiter, merge, discard) with compound
// condition guards; the walker evaluates conditions at runtime.
func (e *Executor) executeReview(phase string, pc PhaseConfig) error {
	// 1. Load the review step-graph formula.
	graph, err := formula.LoadReviewPhaseFormula()
	if err != nil {
		return fmt.Errorf("load review formula: %w", err)
	}

	// 2. Validate graph structure.
	if err := formula.ValidateGraph(graph); err != nil {
		return fmt.Errorf("invalid review formula: %w", err)
	}

	// 3. Pour sub-step beads (idempotent).
	if err := e.ensureReviewSubStepBeads(graph); err != nil {
		return fmt.Errorf("ensure review sub-step beads: %w", err)
	}

	// 4. Build initial condition context.
	maxRounds := "3"
	if v, ok := graph.Vars["max_rounds"]; ok && v.Default != "" {
		maxRounds = v.Default
	}
	ctx := map[string]string{
		"round":      strconv.Itoa(e.state.ReviewRounds),
		"max_rounds": maxRounds,
	}

	// 5. Walk loop.
	// localCompleted is the SOLE source of truth for step completion within
	// a review cycle. Bead status (completedReviewSteps) is only used on
	// entry to seed the initial state — after that, localCompleted drives
	// the walker. This avoids stale bead reads after resetReviewSubStep.
	localCompleted := e.completedReviewSteps() // seed from bead graph on entry
	for {
		next, err := formula.NextSteps(graph, localCompleted, ctx)
		if err != nil {
			return fmt.Errorf("review graph walk error: %w", err)
		}
		if len(next) == 0 {
			return fmt.Errorf("review graph stuck: no next steps, completed=%v, ctx=%v", localCompleted, ctx)
		}

		stepName := next[0] // review is sequential — take first ready step
		stepCfg := graph.Steps[stepName]

		// Activate sub-step bead. [Review fix #1: check error]
		if err := e.activateReviewSubStep(stepName); err != nil {
			return fmt.Errorf("activate review sub-step %s: %w", stepName, err)
		}

		// Terminal steps: execute FIRST, then close on success.
		// [Review fix #3: close after successful execution, not before]
		if formula.IsTerminal(graph, stepName) {
			if err := e.executeTerminalStep(stepName, stepCfg); err != nil {
				return fmt.Errorf("terminal step %s failed: %w", stepName, err)
			}
			if err := e.closeReviewSubStep(stepName); err != nil {
				return fmt.Errorf("close terminal sub-step %s: %w", stepName, err)
			}
			return nil
		}

		// Dispatch agent for this step.
		if err := e.dispatchReviewAgent(stepName, stepCfg, pc); err != nil {
			return fmt.Errorf("review step %s failed: %w", stepName, err)
		}

		// Read results and update context.
		ctx["verdict"] = e.readVerdict()
		ctx["arbiter_decision"] = e.readArbiterDecision()

		// Close sub-step bead. [Review fix #2: check error]
		if err := e.closeReviewSubStep(stepName); err != nil {
			return fmt.Errorf("close review sub-step %s: %w", stepName, err)
		}

		// Mark step completed in local tracker.
		localCompleted[stepName] = true

		// Fix step: reset both sage-review and fix beads for the next
		// review cycle, then increment the round counter and persist.
		if stepName == "fix" {
			if err := e.resetReviewSubStep("sage-review"); err != nil {
				return fmt.Errorf("reset sage-review sub-step: %w", err)
			}
			delete(localCompleted, "sage-review")
			if err := e.resetReviewSubStep("fix"); err != nil {
				return fmt.Errorf("reset fix sub-step: %w", err)
			}
			delete(localCompleted, "fix")
			e.state.ReviewRounds++
			ctx["round"] = strconv.Itoa(e.state.ReviewRounds)
			e.saveState()
		}
	}
}

// dispatchReviewAgent routes to the correct agent dispatch for a review step.
func (e *Executor) dispatchReviewAgent(stepName string, cfg formula.StepConfig, pc PhaseConfig) error {
	switch stepName {
	case "sage-review":
		return e.dispatchSageReview(cfg, pc)
	case "fix":
		return e.dispatchFix(cfg, pc)
	case "arbiter":
		return e.dispatchArbiter(cfg)
	default:
		return fmt.Errorf("unknown review step %q", stepName)
	}
}

// dispatchSageReview spawns a sage agent for code review.
// [Review fix #5: preserves judgment path from old executeReview]
func (e *Executor) dispatchSageReview(cfg formula.StepConfig, pc PhaseConfig) error {
	sageName := fmt.Sprintf("%s-sage", e.agentName)
	e.log("dispatching sage %s", sageName)

	extraArgs := []string{}
	if cfg.VerdictOnly || pc.VerdictOnly {
		extraArgs = append(extraArgs, "--verdict-only")
	}
	if e.state.WorktreeDir != "" {
		extraArgs = append(extraArgs, "--worktree-dir", e.state.WorktreeDir)
	}

	model := cfg.Model
	if model == "" {
		model = pc.Model
	}

	started := time.Now()
	handle, err := e.deps.Spawner.Spawn(agent.SpawnConfig{
		Name:      sageName,
		BeadID:    e.beadID,
		Role:      agent.RoleSage,
		ExtraArgs: extraArgs,
	})
	if err != nil {
		return fmt.Errorf("spawn sage: %w", err)
	}
	waitErr := handle.Wait()
	e.recordAgentRun(sageName, e.beadID, "", model, "sage", "review", started, waitErr)
	if waitErr != nil {
		e.log("sage exited: %s — checking verdict", waitErr)
	}

	// Judgment: if enabled, add executor-judgment comment on request_changes only.
	// (Old behavior: judgment comments were only added for request_changes verdicts.)
	verdict := e.readVerdict()
	if pc.Judgment && verdict == "request_changes" {
		e.log("judgment: agreeing with sage feedback")
		e.deps.AddComment(e.beadID, fmt.Sprintf(
			"Executor judgment (round %d): agree — accepting sage feedback",
			e.state.ReviewRounds))
	}

	return nil
}

// dispatchFix spawns an apprentice to address review feedback.
// [Review fix #4: supports both wave-style and direct dispatch]
func (e *Executor) dispatchFix(cfg formula.StepConfig, pc PhaseConfig) error {
	implPC, ok := e.formula.Phases["implement"]
	if !ok {
		return fmt.Errorf("no implement phase for review-fix cycle")
	}

	e.state.Phase = "implement"
	e.saveState()
	defer func() {
		e.state.Phase = "review"
	}()

	if implPC.GetDispatch() == "wave" {
		fixName := fmt.Sprintf("%s-fix-%d", e.agentName, e.state.ReviewRounds)
		fixArgs := []string{"--review-fix", "--apprentice"}
		if e.state.WorktreeDir != "" {
			fixArgs = append(fixArgs, "--worktree-dir", e.state.WorktreeDir)
		}
		fixStarted := time.Now()
		fh, ferr := e.deps.Spawner.Spawn(agent.SpawnConfig{
			Name:      fixName,
			BeadID:    e.beadID,
			Role:      agent.RoleApprentice,
			ExtraArgs: fixArgs,
		})
		if ferr != nil {
			return fmt.Errorf("spawn review-fix: %w", ferr)
		}
		fixWaitErr := fh.Wait()
		model := cfg.Model
		if model == "" {
			model = implPC.Model
		}
		e.recordAgentRun(fixName, e.beadID, "", model, "apprentice", "review-fix", fixStarted, fixWaitErr)
		if fixWaitErr != nil {
			e.log("review-fix apprentice exited with error (will still attempt merge): %s", fixWaitErr)
		}

		// Merge fix branch into the shared staging worktree.
		if e.state.StagingBranch != "" {
			fixBranch := e.resolveBranch(e.beadID)
			e.log("merging fix branch %s into staging %s", fixBranch, e.state.StagingBranch)
			stagingWt, wtErr := e.ensureStagingWorktree()
			if wtErr != nil {
				EscalateHumanFailure(e.beadID, e.agentName, "review-fix-merge-conflict",
					fmt.Sprintf("ensure staging worktree for fix merge: %s", wtErr), e.deps)
				return fmt.Errorf("ensure staging worktree for fix merge: %w", wtErr)
			}
			if mergeErr := stagingWt.MergeBranch(fixBranch, e.resolveConflicts); mergeErr != nil {
				EscalateHumanFailure(e.beadID, e.agentName, "review-fix-merge-conflict",
					fmt.Sprintf("merge fix branch %s into staging %s: %s", fixBranch, e.state.StagingBranch, mergeErr), e.deps)
				return fmt.Errorf("merge fix branch %s into staging %s: %w", fixBranch, e.state.StagingBranch, mergeErr)
			}
		}
	} else {
		// Direct dispatch: spawn a fix apprentice in the staging worktree,
		// same as the wave path. executeDirect would start fresh from main
		// with no knowledge of the sage feedback — causing an infinite loop.
		fixName := fmt.Sprintf("%s-fix-%d", e.agentName, e.state.ReviewRounds)
		fixArgs := []string{"--review-fix", "--apprentice"}
		if e.state.WorktreeDir != "" {
			fixArgs = append(fixArgs, "--worktree-dir", e.state.WorktreeDir)
		}
		fixStarted := time.Now()
		fh, ferr := e.deps.Spawner.Spawn(agent.SpawnConfig{
			Name:      fixName,
			BeadID:    e.beadID,
			Role:      agent.RoleApprentice,
			ExtraArgs: fixArgs,
		})
		if ferr != nil {
			return fmt.Errorf("spawn review-fix: %w", ferr)
		}
		fixWaitErr := fh.Wait()
		model := cfg.Model
		if model == "" {
			model = implPC.Model
		}
		e.recordAgentRun(fixName, e.beadID, "", model, "apprentice", "review-fix", fixStarted, fixWaitErr)
		if fixWaitErr != nil {
			// Log but don't abort — the apprentice may have committed work even
			// though tests failed (pre-existing failures). Attempt the merge anyway;
			// the sage will judge whether the fix is sufficient.
			e.log("review-fix apprentice exited with error (will still attempt merge): %s", fixWaitErr)
		}

		// Merge fix branch into staging.
		if e.state.StagingBranch != "" {
			fixBranch := e.resolveBranch(e.beadID)
			e.log("merging fix branch %s into staging %s", fixBranch, e.state.StagingBranch)
			stagingWt, wtErr := e.ensureStagingWorktree()
			if wtErr != nil {
				EscalateHumanFailure(e.beadID, e.agentName, "review-fix-merge-conflict",
					fmt.Sprintf("ensure staging worktree for fix merge: %s", wtErr), e.deps)
				return fmt.Errorf("ensure staging worktree for fix merge: %w", wtErr)
			}
			if mergeErr := stagingWt.MergeBranch(fixBranch, e.resolveConflicts); mergeErr != nil {
				EscalateHumanFailure(e.beadID, e.agentName, "review-fix-merge-conflict",
					fmt.Sprintf("merge fix branch %s into staging %s: %s", fixBranch, e.state.StagingBranch, mergeErr), e.deps)
				return fmt.Errorf("merge fix branch %s into staging %s: %w", fixBranch, e.state.StagingBranch, mergeErr)
			}
		}
	}

	return nil
}

// dispatchArbiter escalates to arbiter when review rounds are exhausted.
// [Review fix #6: respects cfg.Model override from the formula step]
func (e *Executor) dispatchArbiter(cfg formula.StepConfig) error {
	sageName := fmt.Sprintf("%s-sage", e.agentName)
	revPolicy := e.formula.GetRevisionPolicy()

	// Use formula step's model override if set.
	if cfg.Model != "" {
		revPolicy.ArbiterModel = cfg.Model
	}

	lastReview := &Review{Verdict: "request_changes", Summary: "Max review rounds reached"}
	return e.deps.ReviewEscalateToArbiter(e.beadID, sageName, lastReview, revPolicy, e.log)
}

// executeTerminalStep dispatches to TerminalMerge or TerminalDiscard.
func (e *Executor) executeTerminalStep(stepName string, cfg formula.StepConfig) error {
	switch stepName {
	case "merge":
		mergePC := PhaseConfig{}
		if pc, ok := e.formula.Phases["merge"]; ok {
			mergePC = pc
		}
		// Respect skip behavior/role for merge (e.g., in tests without git infrastructure).
		if mergePC.GetBehavior() == "skip" || mergePC.GetRole() == "skip" {
			e.log("skipping terminal merge step (behavior/role: skip)")
			return nil
		}
		return e.executeMerge(mergePC)
	case "discard":
		return TerminalDiscard(e.beadID, e.deps, e.log)
	default:
		return fmt.Errorf("unknown terminal step %q", stepName)
	}
}

// readVerdict reads the latest review verdict from review-round beads or labels.
func (e *Executor) readVerdict() string {
	bead, err := e.deps.GetBead(e.beadID)
	if err != nil {
		return ""
	}
	if e.deps.ContainsLabel(bead, "review-approved") {
		return "approve"
	}
	reviews, _ := e.deps.GetReviewBeads(e.beadID)
	if len(reviews) > 0 {
		lastReview := reviews[len(reviews)-1]
		if lastReview.Status == "closed" {
			return e.deps.ReviewBeadVerdict(lastReview)
		}
	}
	return ""
}

// readArbiterDecision reads the arbiter's decision from bead labels.
func (e *Executor) readArbiterDecision() string {
	bead, err := e.deps.GetBead(e.beadID)
	if err != nil {
		return ""
	}
	if decision := e.deps.HasLabel(bead, "arbiter-decision:"); decision != "" {
		return decision
	}
	// If bead was approved after arbiter, treat as merge.
	if e.deps.ContainsLabel(bead, "review-approved") {
		return "merge"
	}
	return ""
}

