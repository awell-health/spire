package executor

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
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

	// Resume: if sage-review was completed in a prior session, the verdict
	// is already persisted in bead labels/review beads. Pre-read it so the
	// walker can route to fix/arbiter/merge without re-dispatching sage.
	if localCompleted["sage-review"] {
		ctx["verdict"] = e.readVerdict()
		ctx["arbiter_decision"] = e.readArbiterDecision()
	}

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
	e.log("dispatching sage %s on %s (worktree: %s)", sageName, e.state.StagingBranch, e.state.WorktreeDir)

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
	e.recordAgentRun(sageName, e.beadID, "", model, "sage", "review", started, waitErr,
		withReviewStep("sage-review", e.state.ReviewRounds+1))
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

	// When the executor has a persisted staging worktree, run the fix directly
	// in it rather than spawning a subprocess that branches from main. Check
	// WorktreeDir (persisted state), not e.stagingWt (in-memory pointer) —
	// on a resumed executor, e.stagingWt may be nil even though the worktree
	// exists on disk. fixInStaging hydrates it via ensureStagingWorktree().
	if e.state.WorktreeDir != "" {
		return e.fixInStaging(cfg, implPC)
	}

	// No staging worktree — spawn a fix apprentice on a feature branch.
	return e.fixViaSubprocess(cfg, implPC)
}

// fixInStaging spawns a wizard-run subprocess in the executor's staging worktree.
// The wizard owns the full apprentice lifecycle (install, prompt, Claude with
// timeout, validation, commit via pkg/git). The executor owns only the policy:
// use the staging worktree, skip the post-fix merge.
func (e *Executor) fixInStaging(cfg formula.StepConfig, implPC PhaseConfig) error {
	// Hydrate the staging worktree if needed (resumed executor has WorktreeDir
	// persisted in state but e.stagingWt may be nil).
	if _, wtErr := e.ensureStagingWorktree(); wtErr != nil {
		return fmt.Errorf("ensure staging worktree for review-fix: %w", wtErr)
	}

	fixName := fmt.Sprintf("%s-fix-%d", e.agentName, e.state.ReviewRounds)
	e.log("dispatching review-fix in staging worktree %s", e.stagingWt.Dir)

	// Spawn wizard-run --review-fix --apprentice --worktree-dir <staging>.
	// The wizard resumes the worktree via pkg/git.ResumeWorktreeContext,
	// captures a session baseline, runs Claude with repo-configured timeout
	// and turns, validates (lint/build/test), and commits — all without
	// creating branches or managing worktree lifecycle.
	fixArgs := []string{"--review-fix", "--apprentice", "--worktree-dir", e.stagingWt.Dir}
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
	e.recordAgentRun(fixName, e.beadID, "", model, "apprentice", "review-fix", fixStarted, fixWaitErr,
		withReviewStep("fix", e.state.ReviewRounds+1))
	if fixWaitErr != nil {
		e.log("review-fix apprentice exited with error: %s", fixWaitErr)
	}

	// The wizard committed directly on the staging branch — no merge needed.
	e.log("fix committed on staging — skipping merge")
	return nil
}

// fixViaSubprocess spawns a fix apprentice as a wizard-run subprocess on a
// feature branch, then merges the result into the staging worktree.
func (e *Executor) fixViaSubprocess(cfg formula.StepConfig, implPC PhaseConfig) error {
	fixName := fmt.Sprintf("%s-fix-%d", e.agentName, e.state.ReviewRounds)
	fixBranchName := e.resolveBranch(e.beadID)
	e.log("dispatching fix apprentice on %s", fixBranchName)

	fixArgs := []string{"--review-fix", "--apprentice"}

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
	e.recordAgentRun(fixName, e.beadID, "", model, "apprentice", "review-fix", fixStarted, fixWaitErr,
		withReviewStep("fix", e.state.ReviewRounds+1))
	if fixWaitErr != nil {
		e.log("review-fix apprentice exited with error: %s", fixWaitErr)
	}

	// Merge fix branch into the staging worktree.
	if e.state.StagingBranch != "" {
		fixBranch := e.resolveBranch(e.beadID)
		e.log("merging fix branch %s into staging %s", fixBranch, e.state.StagingBranch)
		stagingWt, wtErr := e.ensureStagingWorktree()
		if wtErr != nil {
			EscalateHumanFailure(e.beadID, e.agentName, "review-fix-merge-conflict",
				fmt.Sprintf("ensure staging worktree for fix merge: %s", wtErr), e.deps)
			return fmt.Errorf("ensure staging worktree for fix merge: %w", wtErr)
		}
		fixBranchSHA, _ := revParseBranch(stagingWt.Dir, fixBranch)
		if mergeErr := stagingWt.MergeBranch(fixBranch, e.resolveConflicts); mergeErr != nil {
			EscalateHumanFailure(e.beadID, e.agentName, "review-fix-merge-conflict",
				fmt.Sprintf("merge fix branch %s into staging %s: %s", fixBranch, e.state.StagingBranch, mergeErr), e.deps)
			return fmt.Errorf("merge fix branch %s into staging %s: %w", fixBranch, e.state.StagingBranch, mergeErr)
		}
		postMergeSHA, _ := stagingWt.HeadSHA()
		e.log("merged %s (commit %s) into %s → %s", fixBranch, fixBranchSHA, e.state.StagingBranch, postMergeSHA)
	}

	return nil
}

// dispatchArbiter escalates to arbiter when review rounds are exhausted.
// [Review fix #6: respects cfg.Model override from the formula step]
func (e *Executor) dispatchArbiter(cfg formula.StepConfig) error {
	sageName := fmt.Sprintf("%s-sage", e.agentName)
	revPolicy := e.formula.GetRevisionPolicy()

	// Use formula step's model override if set.
	model := cfg.Model
	if model != "" {
		revPolicy.ArbiterModel = model
	}
	if model == "" {
		model = revPolicy.ArbiterModel
	}

	lastReview := &Review{Verdict: "request_changes", Summary: "Max review rounds reached"}
	started := time.Now()
	err := e.deps.ReviewEscalateToArbiter(e.beadID, sageName, lastReview, revPolicy, e.log)
	e.recordAgentRun(sageName, e.beadID, "", model, "arbiter", "review", started, err,
		withReviewStep("arbiter", e.state.ReviewRounds+1))
	return err
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
		e.log("sage verdict: approve (read from review label on %s)", e.beadID)
		return "approve"
	}
	reviews, _ := e.deps.GetReviewBeads(e.beadID)
	if len(reviews) > 0 {
		lastReview := reviews[len(reviews)-1]
		if lastReview.Status == "closed" {
			verdict := e.deps.ReviewBeadVerdict(lastReview)
			e.log("sage verdict: %s (read from review bead %s)", verdict, lastReview.ID)
			return verdict
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

// revParseBranch resolves the SHA of a branch ref from a given git directory.
func revParseBranch(dir, branch string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", branch).Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse %s: %w", branch, err)
	}
	return strings.TrimSpace(string(out)), nil
}

