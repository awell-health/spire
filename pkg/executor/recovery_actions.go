package executor

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	spgit "github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/awell-health/spire/pkg/store"
)

// RecoveryActionCtx carries the runtime-adjacent deps every repair
// function needs. Mechanical functions read only Log (via logf);
// SpawnRepairWorker reads the dispatcher and agent-run fields as well.
//
// spi-h32xj/chunk 3 retired the RecoveryAction registry this struct
// originally drove. It survives as the shared deps carrier so a single
// actionClericExecute callsite can build one and hand it to every
// RepairMode dispatch without juggling parallel context shapes.
type RecoveryActionCtx struct {
	DB         *sql.DB
	RepoPath   string
	BaseBranch string
	// Worktree is the provisioned workspace context. Populated by
	// buildRecoveryActionCtx through the shared runtime workspace
	// contract (spi-xplwy) — dispatch on plan.Workspace.Kind selects
	// between the target bead's borrowed staging worktree and a fresh
	// owned worktree off its feature branch.
	Worktree       *spgit.WorktreeContext
	RecoveryBeadID string
	TargetBeadID   string
	Params         map[string]string
	Log            func(string)

	// Dispatcher wiring (worker-only; left nil for mechanical-only calls).
	Spawner        agent.Backend
	RecordAgentRun func(run AgentRun) (string, error)
	AgentResultDir func(agentName string) string
	LogBaseDir     string
	ParentRunID    string
	AgentNamespace string

	// BuildRuntimeContract stamps the canonical runtime contract on cfg —
	// Identity (tower/prefix/RepoURL/base), Workspace, and Run (backend,
	// workspace kind/name/origin, handoff mode). It is the single
	// construction site for the worker spawn: SpawnRepairWorker never
	// hand-builds the process-only config that bypassed k8s substrate
	// validation (spi-6wiz9). buildRecoveryActionCtx wires this to a
	// closure over (*Executor).withRuntimeContract; tests may inject a
	// stub that mirrors the canonical shape.
	BuildRuntimeContract func(cfg agent.SpawnConfig, step, workspaceName string, ws WorkspaceHandle, mode HandoffMode) (agent.SpawnConfig, error)

	// Optional hooks for test injection. When nil, the defaults call the real
	// store.GetBead and an in-process dispatch via Spawner.
	GetBeadFn     func(id string) (store.Bead, error)
	DispatchFn    func(cfg agent.SpawnConfig) (agent.Handle, error)
	WaitForHandle func(agent.Handle) error
}

// logf is a nil-safe log helper so callers can skip wiring Log without
// tripping repair functions.
func (c *RecoveryActionCtx) logf(msg string) {
	if c == nil || c.Log == nil {
		return
	}
	c.Log(msg)
}

// RepairResult is the typed outcome of a non-worker repair (recipe
// replay today, other kinds in later chunks). Recipe carries the
// replayable form on success so the caller can persist it for
// promotion; Output captures any non-error diagnostic text.
type RepairResult struct {
	Recipe *recovery.MechanicalRecipe
	Output string
	// WorkerAttemptID is set only when the recipe dispatched through
	// SpawnRepairWorker. Mechanical recipes leave it empty. handleLearn
	// uses a non-empty value as the signal to stamp
	// RecoveryOutcome.HandoffMode = HandoffBorrowed for recipe plans.
	WorkerAttemptID string
}

// RepairWorkerResult is the outcome of a SpawnRepairWorker call.
// WorkerAttemptID is the agent-run row that recorded the worker
// (empty when RecordAgentRun is not wired). Output carries any
// diagnostic text the worker path wants to bubble up — keep it short;
// detailed logs live on the agent run.
type RepairWorkerResult struct {
	WorkerAttemptID string
	Output          string
}

// mechanicalAction is the signature for every RepairMode=mechanical
// dispatch. Functions receive the plan (Action/Params are available)
// and the provisioned WorkspaceHandle (helpers reconstruct a
// WorktreeContext from Path). A successful run returns a
// *MechanicalRecipe the caller can persist for promotion; a failed run
// returns a nil recipe and a descriptive error.
type mechanicalAction func(ctx *RecoveryActionCtx, plan recovery.RepairPlan, ws WorkspaceHandle) (*recovery.MechanicalRecipe, error)

// mechanicalActions is the single source of truth for
// RepairMode=mechanical dispatch, keyed by plan.Action. A miss is a
// decide/execute-vocabulary mismatch and surfaces as an error — we
// never fall back to "unknown action" silently.
var mechanicalActions = map[string]mechanicalAction{
	"rebase-onto-base":        mechanicalRebaseOntoBase,
	"cherry-pick":             mechanicalCherryPick,
	"rebuild":                 mechanicalRebuild,
	"reset-to-step":           mechanicalResetToStep,
	"retry-merge":             mechanicalRetryMerge,
	"cleanup-stale-worktrees": mechanicalCleanupStaleWorktrees,
}

// mechanicalRebaseOntoBase fetches origin/<base> and rebases the
// worktree branch onto it. The base branch is resolved through
// repoconfig.ResolveBranchBase so a literal "main" never leaks. On
// conflict, the rebase is aborted and the conflicted-file list is
// returned in the error for decide to reclassify.
func mechanicalRebaseOntoBase(ctx *RecoveryActionCtx, plan recovery.RepairPlan, ws WorkspaceHandle) (*recovery.MechanicalRecipe, error) {
	wc := worktreeFromHandle(ws)
	base := repoconfig.ResolveBranchBase(ws.BaseBranch)
	ref := "origin/" + base

	wc.EnsureRemoteRef("origin", base)

	if err := wc.RunCommand("git rebase " + ref); err != nil {
		files, _ := wc.ConflictedFiles()
		_ = wc.RunCommand("git rebase --abort")
		if len(files) > 0 {
			return nil, fmt.Errorf("rebase conflict in files: %s", strings.Join(files, ", "))
		}
		return nil, fmt.Errorf("rebase onto %s failed: %w", ref, err)
	}
	ctx.logf(fmt.Sprintf("rebase onto %s succeeded", ref))
	return recovery.NewBuiltinRecipe(plan.Action, plan.Params), nil
}

// mechanicalCherryPick cherry-picks plan.Params["commit"] into the
// worktree branch. Aborts on conflict and returns the conflicted-file
// list in the error. The commit is validated as a hex SHA before shell
// interpolation.
func mechanicalCherryPick(ctx *RecoveryActionCtx, plan recovery.RepairPlan, ws WorkspaceHandle) (*recovery.MechanicalRecipe, error) {
	commit := plan.Params["commit"]
	if commit == "" {
		return nil, fmt.Errorf("cherry-pick: missing 'commit' parameter")
	}
	if !validCommitSHA.MatchString(commit) {
		return nil, fmt.Errorf("cherry-pick: invalid commit hash %q (must be 7-40 hex characters)", commit)
	}

	wc := worktreeFromHandle(ws)
	if err := wc.RunCommand(fmt.Sprintf("git cherry-pick %s", commit)); err != nil {
		files, _ := wc.ConflictedFiles()
		_ = wc.RunCommand("git cherry-pick --abort")
		if len(files) > 0 {
			return nil, fmt.Errorf("cherry-pick conflict in files: %s", strings.Join(files, ", "))
		}
		return nil, fmt.Errorf("cherry-pick %s failed: %w", commit, err)
	}
	ctx.logf(fmt.Sprintf("cherry-pick %s succeeded", commit))
	return recovery.NewBuiltinRecipe(plan.Action, plan.Params), nil
}

// mechanicalRebuild runs 'go build ./...' in the worktree and captures
// output verbatim in the error on failure so decide can classify
// subsequent attempts.
func mechanicalRebuild(ctx *RecoveryActionCtx, plan recovery.RepairPlan, ws WorkspaceHandle) (*recovery.MechanicalRecipe, error) {
	wc := worktreeFromHandle(ws)
	output, err := wc.RunCommandOutput("go build ./...")
	if err != nil {
		return nil, fmt.Errorf("build failed: %w\n%s", err, output)
	}
	ctx.logf("rebuild succeeded")
	return recovery.NewBuiltinRecipe(plan.Action, plan.Params), nil
}

// mechanicalResetToStep is the record-only reset-to-step mechanical.
// The actual graph reset happens in the legacy ExecuteRecoveryAction
// path (doResetToStep) — this function exists so the action is
// dispatchable through mechanicalActions and a recipe is captured on
// success.
func mechanicalResetToStep(ctx *RecoveryActionCtx, plan recovery.RepairPlan, ws WorkspaceHandle) (*recovery.MechanicalRecipe, error) {
	step := plan.Params["step"]
	if step == "" {
		return nil, fmt.Errorf("reset-to-step: missing 'step' parameter")
	}
	ctx.logf(fmt.Sprintf("marking reset to step: %s", step))
	return recovery.NewBuiltinRecipe(plan.Action, plan.Params), nil
}

// retryMergeWallTime bounds the total wall-clock time mechanicalRetryMerge
// spends retrying. Tests override this (via testSetRetryMergeTuning) to
// exercise the loop without waiting a minute per test case.
var retryMergeWallTime = 60 * time.Second

// retryMergeBackoffs is the exponential backoff schedule between retries.
// Capped at 4s so at least four rounds fit inside retryMergeWallTime, per
// acceptance #3 (≥4 rounds within 60±2s).
var retryMergeBackoffs = []time.Duration{
	200 * time.Millisecond,
	500 * time.Millisecond,
	1 * time.Second,
	2 * time.Second,
	4 * time.Second,
}

// retryMergeSleep is time.Sleep by default; tests swap in a fake that
// advances a clock instead of blocking.
var retryMergeSleep = time.Sleep

// retryMergeNow is time.Now by default; paired with retryMergeSleep so a
// fake clock stays consistent across both.
var retryMergeNow = time.Now

// retryMergeAttempt is the merge attempt closure the loop calls each round.
// Production builds a closure around RepoContext.PullFFOnly + MergeFFOnly;
// tests inject a stub that simulates contention or success on a chosen round.
// The function is overridable at the package level; mechanicalRetryMerge
// builds the real attempt lazily and dispatches through this hook so tests
// that override the hook don't need to stand up a full repo.
var retryMergeAttempt = realRetryMergeAttempt

// testSetRetryMergeTuning is a test-only helper that swaps the wall-time and
// backoff schedule so unit tests don't block for a full minute. The returned
// cleanup function restores defaults.
//
// NOT SAFE for parallel tests: the knobs swapped here
// (retryMergeWallTime, retryMergeBackoffs, retryMergeSleep, retryMergeNow,
// retryMergeAttempt) are package-level vars, so tests that call this helper
// MUST NOT use t.Parallel(). Converting retry-merge tuning to struct fields
// on RecoveryActionCtx is tracked as a follow-up; today this is an explicit
// non-parallel constraint.
func testSetRetryMergeTuning(wallTime time.Duration, backoffs []time.Duration, sleep func(time.Duration), now func() time.Time) func() {
	prevWall := retryMergeWallTime
	prevBackoffs := retryMergeBackoffs
	prevSleep := retryMergeSleep
	prevNow := retryMergeNow
	retryMergeWallTime = wallTime
	retryMergeBackoffs = backoffs
	if sleep != nil {
		retryMergeSleep = sleep
	}
	if now != nil {
		retryMergeNow = now
	}
	return func() {
		retryMergeWallTime = prevWall
		retryMergeBackoffs = prevBackoffs
		retryMergeSleep = prevSleep
		retryMergeNow = prevNow
	}
}

// realRetryMergeAttempt is the production merge attempt: pull origin/base
// into the main repo, then ff-only merge the staging branch. Returns nil on
// success.
func realRetryMergeAttempt(rc *spgit.RepoContext, baseBranch, stagingBranch string, env []string) error {
	if pullErr := rc.PullFFOnly("origin", baseBranch, env); pullErr != nil {
		return fmt.Errorf("pull %s: %w", baseBranch, pullErr)
	}
	if mergeErr := rc.MergeFFOnly(stagingBranch, env); mergeErr != nil {
		return fmt.Errorf("ff-only merge %s → %s: %w", stagingBranch, baseBranch, mergeErr)
	}
	return nil
}

// mechanicalRetryMerge is the time-bounded merge retry for the merge-race and
// post-rebase-ff-only sub-classes. On entry it runs an idempotent safe
// sibling-worktree cleanup (to clear any branch-in-use locks), resumes the
// staging worktree at ws.Path (captures HEAD SHA for session-scoped commit
// detection), then loops:
//
//  1. Pull origin/<baseBranch> into the main repo.
//  2. ff-only merge <stagingBranch> into <baseBranch>.
//  3. On success, return the captured recipe.
//  4. On failure, sleep per retryMergeBackoffs (capped at 4s) and retry.
//  5. Abort when total wall time ≥ retryMergeWallTime (default 60s).
//
// The merge operations themselves run against the main repo (parity with
// StagingWorktree.MergeToMain) — you can't ff-only-merge a branch into the
// branch you're on — but the staging worktree is the source of truth for the
// staging branch and is resumed here so any downstream logic (e.g. learning
// which commits landed this session) has a consistent handle.
//
// Wall-time exhaustion is returned as an error; Decide's next round then sees
// repeatedFailures["retry-merge"]++ and — after two failures — upgrades to
// resolve-conflicts (Worker). A third total failure auto-escalates via the
// totalAttempts >= maxAttempts guard in Decide.
func mechanicalRetryMerge(ctx *RecoveryActionCtx, plan recovery.RepairPlan, ws WorkspaceHandle) (*recovery.MechanicalRecipe, error) {
	if ctx == nil || ctx.RepoPath == "" {
		return nil, fmt.Errorf("retry-merge: missing repo context")
	}
	if ws.Branch == "" {
		return nil, fmt.Errorf("retry-merge: missing staging branch in workspace handle")
	}
	baseBranch := ws.BaseBranch
	if baseBranch == "" {
		baseBranch = "main"
	}

	log := func(format string, args ...interface{}) {
		ctx.logf(fmt.Sprintf(format, args...))
	}

	// Step 0: idempotent safe cleanup of stale sibling worktrees for this
	// bead. A prior wizard run may have left `.worktrees/<beadID>-*` paths
	// holding the bead's feature branch checked out; the safe variant
	// quarantines anything with uncommitted/in-flight work rather than
	// deleting it, then force-removes the rest.
	if ctx.TargetBeadID != "" {
		targetDir := filepath.Join(ctx.RepoPath, ".worktrees", ctx.TargetBeadID)
		fates := spgit.CleanupStaleSiblingWorktreesSafe(ctx.RepoPath, targetDir, log)
		for _, f := range fates {
			switch f.Action {
			case "renamed":
				log("retry-merge: sibling %s quarantined to %s (%s)", f.Path, f.NewPath, f.Reason)
			case "removed":
				log("retry-merge: sibling %s removed", f.Path)
			case "skipped-live":
				log("retry-merge: sibling %s skipped (%s)", f.Path, f.Reason)
			case "error":
				log("retry-merge: sibling %s cleanup error: %s", f.Path, f.Reason)
			}
		}
	}

	// Resume the staging worktree at ws.Path so session-scoped commit
	// detection has a baseline (StartSHA is captured from the worktree's HEAD
	// at resume time) and so the merge lifecycle mirrors StagingWorktree's
	// canonical pattern. The main-repo RepoContext used by PullFFOnly /
	// MergeFFOnly below is derived from stagingWt.RepoPath — it targets the
	// same repo we passed in, just wired through the staging wrapper so the
	// flow stays aligned with StagingWorktree.MergeToMain.
	stagingWt := spgit.ResumeStagingWorktree(ctx.RepoPath, ws.Path, ws.Branch, baseBranch, log)
	if stagingWt != nil && stagingWt.StartSHA != "" {
		log("retry-merge: resumed staging worktree at %s (startSHA=%s branch=%s)",
			ws.Path, stagingWt.StartSHA, ws.Branch)
	} else {
		log("retry-merge: resumed staging worktree at %s (branch=%s; no StartSHA — worktree may be bare)",
			ws.Path, ws.Branch)
	}
	repoDir := ctx.RepoPath
	if stagingWt != nil && stagingWt.RepoPath != "" {
		repoDir = stagingWt.RepoPath
	}
	rc := &spgit.RepoContext{Dir: repoDir, BaseBranch: baseBranch, Log: log}

	log("retry-merge: starting time-bounded retry (branch=%s base=%s wall=%s)",
		ws.Branch, baseBranch, retryMergeWallTime)

	start := retryMergeNow()
	var lastErr error
	for round := 1; ; round++ {
		elapsed := retryMergeNow().Sub(start)
		if elapsed >= retryMergeWallTime {
			log("retry-merge: wall-time budget exhausted after %s and %d rounds",
				elapsed.Round(time.Millisecond), round-1)
			if lastErr != nil {
				return nil, fmt.Errorf("retry-merge: wall-time exhausted after %d rounds (%s): %w",
					round-1, retryMergeWallTime, lastErr)
			}
			return nil, fmt.Errorf("retry-merge: wall-time exhausted after %d rounds (%s)",
				round-1, retryMergeWallTime)
		}

		log("retry-merge: round %d (elapsed %s)", round, elapsed.Round(time.Millisecond))
		if err := retryMergeAttempt(rc, baseBranch, ws.Branch, nil); err == nil {
			log("retry-merge: succeeded on round %d after %s",
				round, retryMergeNow().Sub(start).Round(time.Millisecond))
			return recovery.NewBuiltinRecipe(plan.Action, plan.Params), nil
		} else {
			lastErr = err
			log("retry-merge: round %d failed: %s", round, err)
		}

		// Pick the backoff for the next sleep. Rounds beyond the schedule
		// reuse the last (cap) duration — so ≥4 rounds comfortably fit in
		// a 60s budget.
		idx := round - 1
		if idx >= len(retryMergeBackoffs) {
			idx = len(retryMergeBackoffs) - 1
		}
		backoff := retryMergeBackoffs[idx]
		remaining := retryMergeWallTime - retryMergeNow().Sub(start)
		if backoff > remaining {
			backoff = remaining
		}
		if backoff <= 0 {
			continue
		}
		retryMergeSleep(backoff)
	}
}

// mechanicalCleanupStaleWorktrees runs the SAFE sibling-cleanup for this
// bead's `.worktrees/<beadID>*` paths. Unlike the prior unsafe variant, it
// quarantines anything with uncommitted changes, an in-progress rebase/merge,
// a mismatched branch, or a recent mtime — those get renamed to
// `.worktrees/.abandoned-<unix-ts>-<base>` via os.Rename rather than deleted.
// Only siblings that pass all four gates are force-removed.
//
// Idempotent: a second call with no un-quarantined siblings present is a no-op.
func mechanicalCleanupStaleWorktrees(ctx *RecoveryActionCtx, plan recovery.RepairPlan, ws WorkspaceHandle) (*recovery.MechanicalRecipe, error) {
	if ctx == nil || ctx.TargetBeadID == "" || ctx.RepoPath == "" {
		return nil, fmt.Errorf("cleanup-stale-worktrees: missing repo/target context")
	}
	log := func(format string, args ...interface{}) {
		ctx.logf(fmt.Sprintf(format, args...))
	}
	targetDir := filepath.Join(ctx.RepoPath, ".worktrees", ctx.TargetBeadID)
	log("cleaning stale sibling worktrees for %s (target=%s)", ctx.TargetBeadID, targetDir)
	fates := spgit.CleanupStaleSiblingWorktreesSafe(ctx.RepoPath, targetDir, log)
	for _, f := range fates {
		switch f.Action {
		case "removed":
			log("cleanup: sibling %s removed", f.Path)
		case "renamed":
			log("cleanup: sibling %s quarantined to %s (%s)", f.Path, f.NewPath, f.Reason)
		case "skipped-live":
			log("cleanup: sibling %s skipped (%s)", f.Path, f.Reason)
		case "error":
			log("cleanup: sibling %s error: %s", f.Path, f.Reason)
		}
	}
	return recovery.NewBuiltinRecipe(plan.Action, plan.Params), nil
}

// actionTargetedFix is a tombstone left after spi-h32xj/chunk 3
// retired the targeted-fix action. Historical recovery beads may still
// reference the name through resume paths; calling through it now
// fails loudly and points callers at RepairModeWorker. Remove in a
// later chunk once metrics show zero hits.
func actionTargetedFix(_ *RecoveryActionCtx, _ recovery.RepairPlan, _ WorkspaceHandle) (*recovery.MechanicalRecipe, error) {
	return nil, fmt.Errorf("targeted-fix is retired; dispatch this intent as RepairModeWorker with the desired repair role")
}

// executeRecipe runs a promoted Recipe against ws. The plan's Action +
// Params are the replayable payload that decide stamped when the promotion
// snapshot crossed threshold; they're enough to reconstruct the builtin
// recipe and route through the SAME dispatch surface the un-promoted
// plan would use — mechanicalActions[Action] for mechanical recipes,
// SpawnRepairWorker for worker recipes. No second dispatch map, no
// shadow spawner (design spi-h32xj §6 chunk 7).
//
// On success the function re-captures the replayed recipe so the learn
// step can extend the promotion chain with another clean outcome. On
// failure it returns the underlying dispatch error verbatim so the
// caller's demote path (handlePlanExecute) can react and reset the
// promotion counter for this signature.
func executeRecipe(ctx *RecoveryActionCtx, plan recovery.RepairPlan, ws WorkspaceHandle) (RepairResult, error) {
	recipe := recovery.NewBuiltinRecipe(plan.Action, plan.Params)
	if recipe == nil {
		return RepairResult{}, fmt.Errorf("recipe execution: plan missing action — recipe is not replayable")
	}
	if recipe.Kind != recovery.RecipeKindBuiltin {
		return RepairResult{}, fmt.Errorf("recipe execution: only builtin kind is dispatchable, got %q", recipe.Kind)
	}

	// Mechanical recipe: dispatch through the canonical mechanicalActions
	// map. Same function, same params, same workspace — identical to the
	// un-promoted mechanical path.
	if fn, ok := mechanicalActions[recipe.Action]; ok {
		captured, err := fn(ctx, plan, ws)
		if err != nil {
			return RepairResult{}, err
		}
		// Re-stamp with the replayed recipe when the mechanical returned
		// nil (defensive) so the learn step always has a recipe to extend
		// the promotion chain with.
		if captured == nil {
			captured = recipe
		}
		return RepairResult{
			Recipe: captured,
			Output: fmt.Sprintf("recipe replayed (mechanical): %s", recipe.Action),
		}, nil
	}

	// Worker recipe: dispatch through the canonical SpawnRepairWorker.
	// Same spawner, same validation gates — identical to the un-promoted
	// worker path.
	workerResult, err := SpawnRepairWorker(ctx, plan, ws)
	if err != nil {
		return RepairResult{}, err
	}
	output := fmt.Sprintf("recipe replayed (worker): %s", recipe.Action)
	if workerResult.WorkerAttemptID != "" {
		output += fmt.Sprintf(" worker_attempt_id=%s", workerResult.WorkerAttemptID)
	}
	if workerResult.Output != "" {
		output += " " + workerResult.Output
	}
	return RepairResult{
		Recipe:          recipe,
		Output:          output,
		WorkerAttemptID: workerResult.WorkerAttemptID,
	}, nil
}

// worktreeFromHandle reconstructs a WorktreeContext from a
// WorkspaceHandle so mechanical actions and the worker-dispatch
// fallback can reuse the spgit helpers (RunCommand, ConflictedFiles,
// EnsureRemoteRef) without threading a pre-built context through every
// signature. buildRecoveryActionCtx provides a live context directly
// via resolveRepairWorkspace; this helper covers the path where
// SpawnRepairWorker is invoked with a bare WorkspaceHandle (e.g. from
// tests that bypass the builder).
func worktreeFromHandle(ws WorkspaceHandle) *spgit.WorktreeContext {
	return &spgit.WorktreeContext{
		Dir:        ws.Path,
		Branch:     ws.Branch,
		BaseBranch: ws.BaseBranch,
	}
}

// validCommitSHA matches a hex SHA (7-40 characters). Used to guard
// against command injection in mechanicals that interpolate commit
// hashes into shell commands.
var validCommitSHA = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)

// generateRecoveryAttemptID produces a random attempt ID in the same
// format as store.generateAttemptID ("ra-" + 8 hex chars). The caller
// retains the ID so later UpdateAttemptOutcome calls can reference it
// without the id being lost through RecordRecoveryAttempt's by-value
// passthrough.
func generateRecoveryAttemptID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "ra-00000000"
	}
	return "ra-" + hex.EncodeToString(b)
}
