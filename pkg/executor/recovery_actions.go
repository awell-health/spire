package executor

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

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
	// Worktree is the provisioned workspace context. Chunk 3 still
	// populates this via ProvisionRecoveryWorktree or the wizard's
	// resumed staging worktree; chunk 4 will switch to resolveWorkspace
	// producing a handle+context pair up front.
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

	// Optional hooks for test injection. When nil, the defaults call the real
	// store.GetBead and an in-process dispatch via Spawner.
	GetBeadFn  func(id string) (store.Bead, error)
	DispatchFn func(cfg agent.SpawnConfig) (agent.Handle, error)
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
	"rebase-onto-base": mechanicalRebaseOntoBase,
	"cherry-pick":      mechanicalCherryPick,
	"rebuild":          mechanicalRebuild,
	"reset-to-step":    mechanicalResetToStep,
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

// actionTargetedFix is a tombstone left after spi-h32xj/chunk 3
// retired the targeted-fix action. Historical recovery beads may still
// reference the name through resume paths; calling through it now
// fails loudly and points callers at RepairModeWorker. Remove in a
// later chunk once metrics show zero hits.
func actionTargetedFix(_ *RecoveryActionCtx, _ recovery.RepairPlan, _ WorkspaceHandle) (*recovery.MechanicalRecipe, error) {
	return nil, fmt.Errorf("targeted-fix is retired; dispatch this intent as RepairModeWorker with the desired repair role")
}

// executeRecipe runs a promoted Recipe against ws. Stub in chunk 3 —
// recipe executability lands in spi-h32xj/chunk 7 alongside
// Recipe.ToRepairPlan().
func executeRecipe(_ *RecoveryActionCtx, _ recovery.RepairPlan, _ WorkspaceHandle) (RepairResult, error) {
	return RepairResult{}, fmt.Errorf("recipe execution not yet implemented")
}

// worktreeFromHandle reconstructs a WorktreeContext from a
// WorkspaceHandle so mechanical actions can reuse the spgit helpers
// (RunCommand, ConflictedFiles, EnsureRemoteRef) without threading a
// pre-built context through every signature. Chunk 4 replaces this
// with resolveWorkspace producing a live handle+context pair.
func worktreeFromHandle(ws WorkspaceHandle) *spgit.WorktreeContext {
	return &spgit.WorktreeContext{
		Dir:        ws.Path,
		Branch:     ws.Branch,
		BaseBranch: ws.BaseBranch,
	}
}

// ProvisionRecoveryWorktree creates a worktree for recovery operations
// using pkg/git APIs. The worktree is placed at
// <repoPath>/.worktrees/<beadID>-recovery on a branch named
// recovery/<beadID>, based on the target bead's feature branch (not
// the base branch). This ensures recovery actions operate on the
// bead's actual work.
//
// Kept for chunk 3 — chunk 4 replaces this with resolveWorkspace
// consuming a WorkspaceRequest.
func ProvisionRecoveryWorktree(repoPath, beadID, baseBranch string) (*spgit.WorktreeContext, func(), error) {
	dir := filepath.Join(repoPath, ".worktrees", beadID+"-recovery")
	branch := "recovery/" + beadID

	startPoint := "feat/" + beadID
	if b, err := store.GetBead(beadID); err == nil {
		if fb := store.HasLabel(b, "feat-branch:"); fb != "" {
			startPoint = fb
		}
	}

	base := repoconfig.ResolveBranchBase(baseBranch)
	rc := &spgit.RepoContext{Dir: repoPath, BaseBranch: base}
	wc, err := rc.CreateWorktreeNewBranch(dir, branch, startPoint)
	if err != nil {
		return nil, nil, fmt.Errorf("create recovery worktree at %s from %s: %w", dir, startPoint, err)
	}

	cleanup := func() {
		wc.Cleanup()
		rc2 := &spgit.RepoContext{Dir: repoPath, BaseBranch: base}
		_ = rc2.ForceDeleteBranch(branch)
	}
	return wc, cleanup, nil
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
