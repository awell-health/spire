package executor

// commit_producing_apprentice.go — Principle 1 dispatch path.
//
// Design context: spi-1dk71j ("Wizard absorbs cleric") and spi-tlj32a
// ("borrowed -> bundle handoff (uniform with wave apprentice)").
//
// Principle 1 (spi-1dk71j): any agent that produces git commits delivers a
// bundle. There is no "borrowed handoff" for commit-producing apprentices —
// the runtime contract MUST record HandoffBundle so the same delivery shape
// applies whether the apprentice runs in-process (local-native) or in a
// separate pod (cluster-native).
//
// Two dispatch surfaces use this path:
//   1. The fix step in subgraph-review (sage verdict=request_changes →
//      apprentice rewrites against feedback).
//   2. The worker-mode cleric repair dispatch (spi-icgqhi RepairMode=Worker
//      calls DispatchClericWorkerApprentice when its repair plan picks the
//      worker mode).
//
// Both paths converge on dispatchCommitProducingApprentice below: spawn an
// apprentice with HandoffBundle, wait for the bundle/signal, and apply it
// to the staging worktree via a per-attempt local ref. Apply is idempotent
// (force-fetch into a unique ref + ff merge into staging), so a borrowed
// staging worktree where the commits already landed via shared filesystem
// is a clean no-op rather than a conflict.

import (
	"context"
	"fmt"
	"strings"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/bundlestore"
	spgit "github.com/awell-health/spire/pkg/git"
)

// commitProducingApprenticeSpec describes one bundle-handoff apprentice
// dispatch. Role distinguishes the in-flight identity (fix vs cleric-worker)
// for logs/metrics; the contract — bundle artifact + signal at
// apprenticeSignalKey — is identical.
type commitProducingApprenticeSpec struct {
	StepName  string  // graph step name for naming/logging
	Step      StepConfig  // formula step (carries Workspace / With / etc.)
	State     *GraphState // graph state for the run
	Role      agent.SpawnRole // RoleApprentice for fix; RoleApprentice for cleric-worker
	RoleTag   string  // "fix" or "cleric-worker" — used in spawn name + ref name
	ExtraArgs []string // wizard CLI flags
	Workspace *WorkspaceHandle // workspace handle for the spawn (nil ok)
	StagingWt *spgit.StagingWorktree // bundle apply target (nil = no apply)
}

// dispatchCommitProducingApprentice is the unified entry point for any
// apprentice that produces git commits and therefore must deliver a bundle.
//
// The wave/sequential/direct dispatch in action_dispatch.go uses an
// equivalent pattern (resolveApprenticeHandoff + applyApprenticeBundle +
// MergeBranch) inlined for fan-out. This function is the single-apprentice
// shape used by review-fix and cleric-worker.
//
// Returns the spawn ActionResult unchanged. On success, the apprentice's
// bundle is applied to spec.StagingWt before return; a non-nil error from
// apply replaces the spawn result's Error.
func (e *Executor) dispatchCommitProducingApprentice(spec commitProducingApprenticeSpec) ActionResult {
	handoffMode := e.resolveApprenticeHandoff()
	result := wizardRunSpawnWithHandoff(
		e,
		spec.StepName,
		spec.Step,
		spec.State,
		spec.Role,
		spec.ExtraArgs,
		spec.Workspace,
		handoffMode,
	)
	if result.Error != nil {
		return result
	}

	if applyErr := e.applyCommitProducingBundle(spec); applyErr != nil {
		result.Error = applyErr
	}
	return result
}

// applyCommitProducingBundle fetches the apprentice's bundle from the
// BundleStore and merges it into the staging worktree. Uses a per-attempt
// local ref name (<roleTag>/<beadID>-<spawnAttempt>) so the apply does not
// collide with the staging branch when both the apprentice and the staging
// worktree target the same branch (the task-default review-fix shape, where
// staging IS feat/<beadID>).
//
// Idempotency (seam 9):
//   - Re-applying a bundle whose commits are already reachable from the
//     staging branch HEAD is a fast-forward no-op via MergeBranch.
//   - When BundleStore is unconfigured, the function logs and returns nil so
//     legacy push-transport configurations still work.
//   - When the apprentice signalled no-op (no changes), the merge is skipped.
func (e *Executor) applyCommitProducingBundle(spec commitProducingApprenticeSpec) error {
	if e.deps == nil || e.deps.BundleStore == nil {
		return nil
	}
	if spec.StagingWt == nil {
		return nil
	}

	idx := commitProducingApprenticeIdx(spec.RoleTag)
	bead, err := e.deps.GetBead(e.beadID)
	if err != nil {
		return fmt.Errorf("get bead for %s bundle: %w", spec.RoleTag, err)
	}
	role := bundlestore.ApprenticeRole(e.beadID, idx)
	sig, ok, err := bundlestore.SignalForRole(bead.Metadata, role)
	if err != nil {
		return fmt.Errorf("parse %s apprentice signal %s: %w", spec.RoleTag, role, err)
	}
	if !ok {
		// No signal — apprentice did not produce a bundle (e.g. push
		// transport, or a wizard build that predates the bundle path).
		// Nothing to apply; the borrowed-worktree commits are already in
		// the staging branch via shared fs.
		e.log("no %s apprentice signal for %s — skipping bundle apply", spec.RoleTag, role)
		return nil
	}
	if sig.Kind == bundlestore.SignalKindNoOp {
		e.log("%s apprentice signalled no-op — skipping bundle apply", spec.RoleTag)
		return nil
	}
	if sig.Kind != bundlestore.SignalKindBundle {
		return fmt.Errorf("unexpected %s signal kind %q for %s", spec.RoleTag, sig.Kind, role)
	}
	if sig.BundleKey == "" {
		return fmt.Errorf("%s bundle signal for %s has empty bundle key", spec.RoleTag, role)
	}

	handle := bundlestore.HandleForSignal(e.beadID, sig)

	attemptNum := spec.State.Steps[spec.StepName].CompletedCount + 1
	branch := commitProducingApprenticeBundleRef(spec.RoleTag, e.beadID, attemptNum)

	if err := e.fetchAndApplyBundle(handle, spec.StagingWt, branch); err != nil {
		return fmt.Errorf("apply %s apprentice bundle: %w", spec.RoleTag, err)
	}

	resolver := e.conflictResolver(0)
	if mergeErr := spec.StagingWt.MergeBranch(branch, resolver); mergeErr != nil {
		return fmt.Errorf("merge %s bundle %s into staging: %w", spec.RoleTag, branch, mergeErr)
	}

	e.deleteApprenticeBundle(e.beadID, handle)
	return nil
}

// fetchAndApplyBundle streams a bundle out of the BundleStore and writes it
// to a local branch ref in the staging worktree. Wraps the same git-fetch
// invocation applyApprenticeBundle uses, but lets the caller pick the local
// ref name so it can avoid the staging-branch checkout collision.
func (e *Executor) fetchAndApplyBundle(handle bundlestore.BundleHandle, stagingWt *spgit.StagingWorktree, branch string) error {
	rc, err := e.deps.BundleStore.Get(context.Background(), handle)
	if err != nil {
		return fmt.Errorf("get bundle %s: %w", handle.Key, err)
	}
	defer rc.Close()
	if err := stagingWt.ApplyBundleFromReader(rc, branch); err != nil {
		return fmt.Errorf("apply bundle to %s: %w", branch, err)
	}
	return nil
}

// commitProducingApprenticeIdx returns the apprentice fan-out index used by
// review-fix and cleric-worker. Both are single-apprentice dispatches, so
// the index is always 0 — the spec field is preserved on SpawnConfig for
// parity with the wave path. Apprentice signal lookup uses
// bundlestore.ApprenticeRole(beadID, 0) for these flows; the signal key is
// deterministic so a re-run produces the same metadata key (seam 8
// idempotency property).
func commitProducingApprenticeIdx(_ string) int {
	return 0
}

// commitProducingApprenticeBundleRef builds the per-attempt local ref name
// used as the bundle apply target. Including the role tag and attempt number
// isolates one round's apply from the next so an interrupted apply followed
// by a retry never overwrites a peer round's ref.
func commitProducingApprenticeBundleRef(roleTag, beadID string, attemptNum int) string {
	tag := strings.ReplaceAll(roleTag, "_", "-")
	return fmt.Sprintf("%s/%s-r%d", tag, beadID, attemptNum)
}

// actionReviewFix is the review-fix dispatch entry point. Replaces the prior
// borrowed-handoff path (graph_actions.go pre-spi-tlj32a) with the unified
// commit-producing-apprentice dispatch.
//
// The wizard subprocess receives --review-fix --apprentice and (for the
// borrowed-worktree mode that local-native still uses for performance) the
// existing --worktree-dir path; the wizard's own deliverApprenticeWork ships
// the bundle regardless. The executor side then applies that bundle to
// staging — idempotent in local mode (fast-forward to the same SHA), the
// only working delivery path in cluster mode.
func actionReviewFix(e *Executor, stepName string, step StepConfig, state *GraphState, wsDir string, workspace *WorkspaceHandle) ActionResult {
	extraArgs := []string{"--review-fix", "--apprentice"}
	if wsDir != "" {
		extraArgs = append(extraArgs, "--worktree-dir", wsDir)
	}

	stagingWt := reviewFixStagingWorktree(e, step, state)

	return e.dispatchCommitProducingApprentice(commitProducingApprenticeSpec{
		StepName:  stepName,
		Step:      step,
		State:     state,
		Role:      agent.RoleApprentice,
		RoleTag:   "fix",
		ExtraArgs: extraArgs,
		Workspace: workspace,
		StagingWt: stagingWt,
	})
}

// ClericWorkerSpec describes a worker-mode cleric repair apprentice
// dispatch. The recovery dispatch (spi-icgqhi) builds this when its
// RepairPlan picks RepairMode=Worker and calls DispatchClericWorkerApprentice
// to drive the spawn-bundle-apply cycle.
//
// The contract is identical to a fix or wave apprentice: bundle handoff,
// deterministic signal key (apprentice-<beadID>-<idx> with idx=0 for
// single-apprentice dispatches), bundle artifact applied to staging. The
// only differences are RoleTag and the prompt content.
type ClericWorkerSpec struct {
	// StepName identifies the recovery-graph step that triggered this
	// dispatch (used for spawn naming so per-attempt isolation works).
	StepName string
	// Prompt is the repair-focused prompt body the cleric assembled. It is
	// passed to the apprentice via wizard.run's CustomPrompt seam so the
	// repair instructions ride alongside the standard apprentice harness.
	Prompt string
	// Workspace optionally pins the worktree the apprentice should resume.
	// When nil, the apprentice creates its own worktree via the standard
	// CmdWizardRun flow.
	Workspace *WorkspaceHandle
	// WorktreeDir, when non-empty, is forwarded as --worktree-dir to the
	// apprentice subprocess. Used by recovery flows that pin the apprentice
	// to the failing wizard's borrowed staging worktree.
	WorktreeDir string
	// StagingWt is the bundle apply target. Recovery callers pass the
	// staging worktree they expect the repair commits to land into; pass
	// nil to skip apply (e.g. when the recovery flow handles consumption
	// elsewhere).
	StagingWt *spgit.StagingWorktree
}

// DispatchClericWorkerApprentice is the public entry point for the
// worker-mode cleric repair dispatch (spi-icgqhi RepairMode=Worker). It
// goes through the same dispatchCommitProducingApprentice path as the fix
// step so a worker-mode cleric apprentice produces a signal indistinguishable
// in shape from a fix or wave apprentice signal — only RoleTag and Idx
// differ. Drift-resistance is enforced by TestWorkerRepairApprentice_SameSignalShape.
//
// state must be the recovery-graph GraphState the cleric is driving. The
// caller (spi-icgqhi recovery actions) is responsible for parking/advancing
// recovery state after this returns.
func (e *Executor) DispatchClericWorkerApprentice(spec ClericWorkerSpec, state *GraphState) ActionResult {
	extraArgs := []string{"--apprentice"}
	if spec.WorktreeDir != "" {
		extraArgs = append(extraArgs, "--worktree-dir", spec.WorktreeDir)
	}

	step := StepConfig{
		Action: "wizard.run",
		Flow:   "implement",
	}
	if spec.Prompt != "" {
		if step.With == nil {
			step.With = map[string]string{}
		}
		step.With["prompt"] = spec.Prompt
	}

	return e.dispatchCommitProducingApprentice(commitProducingApprenticeSpec{
		StepName:  spec.StepName,
		Step:      step,
		State:     state,
		Role:      agent.RoleApprentice,
		RoleTag:   "cleric-worker",
		ExtraArgs: extraArgs,
		Workspace: spec.Workspace,
		StagingWt: spec.StagingWt,
	})
}

// reviewFixStagingWorktree resolves the staging worktree the bundle apply
// will land into. For task-default the parent declares
// `[workspaces.feature]` and the subgraph-review fix step propagates it via
// state.WorktreeDir; for formulas that don't declare a workspace, the
// function returns nil and applyCommitProducingBundle short-circuits.
func reviewFixStagingWorktree(e *Executor, step StepConfig, state *GraphState) *spgit.StagingWorktree {
	if state == nil {
		return nil
	}
	if step.Workspace != "" {
		ws := state.Workspaces[step.Workspace]
		if ws.Dir != "" {
			return spgit.ResumeStagingWorktree(state.RepoPath, ws.Dir, ws.Branch, ws.BaseBranch, e.log)
		}
	}
	if state.WorktreeDir == "" {
		return nil
	}
	branch := state.StagingBranch
	if branch == "" {
		branch = e.resolveBranch(e.beadID)
	}
	return spgit.ResumeStagingWorktree(state.RepoPath, state.WorktreeDir, branch, state.BaseBranch, e.log)
}
