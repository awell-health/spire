package executor

// action_dispatch.go — dispatch.children action handler and extracted dispatch helpers.
// Moves wave/sequential/direct child execution behind a declared executor action.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/dolt"
	"github.com/awell-health/spire/pkg/formula"
	spgit "github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/lifecycle"
	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/awell-health/spire/pkg/steward/intent"
)

// useClusterChildDispatch reports whether executor child-dispatch sites
// should publish a WorkloadIntent through the .1-introduced cluster
// seam instead of calling Spawner.Spawn. Returns true only when both
// conditions hold:
//
//   - the active tower's effective deployment mode is cluster-native
//   - Deps.ClusterChildDispatcher is wired
//
// Centralized here so action handlers (graph_actions.go,
// action_dispatch.go, plus wizard_review.go via deps.ClusterChildDispatcher)
// never re-inspect tower config themselves; the mode decision is made
// once at executor construction (cmd/spire/executor_bridge.go) and read
// through this helper.
//
// A nil dispatcher in cluster-native mode is treated as "do not
// dispatch" — call sites then surface a fail-closed error rather than
// silently falling back to Spawner.Spawn. That keeps the cluster-native
// invariant (no direct backend.Spawn for executor child work) explicit.
func (e *Executor) useClusterChildDispatch() bool {
	if e == nil || e.deps == nil || e.deps.ClusterChildDispatcher == nil {
		return false
	}
	if e.deps.ActiveTowerConfig == nil {
		return false
	}
	tower, err := e.deps.ActiveTowerConfig()
	if err != nil || tower == nil {
		return false
	}
	return tower.EffectiveDeploymentMode() == config.DeploymentModeClusterNative
}

// childIntentRepoIdentity assembles the intent.RepoIdentity for a child
// dispatch from the active tower's registered binding for the given
// bead. Returns the zero value when no binding is available — Validate
// inside the dispatcher will reject empty values, surfacing the
// configuration gap rather than guessing.
func (e *Executor) childIntentRepoIdentity(beadID, baseBranch string) intent.RepoIdentity {
	prefix := prefixFromBeadID(beadID)
	url := e.runtimeRepoURL()
	bb := baseBranch
	if bb == "" {
		bb = e.runtimeBaseBranch()
	}
	return intent.RepoIdentity{
		URL:        url,
		BaseBranch: bb,
		Prefix:     prefix,
	}
}

// childIntentForApprentice builds a WorkloadIntent for an apprentice
// child run. The dispatcher (Dispatch implementations) is responsible
// for assigning DispatchSeq and invoking intent.Validate; the executor
// only fills the role/phase/runtime/repo identity that it owns.
//
// reasonTag is stamped on Reason for log/metric continuity (e.g.
// "implement", "fix", "review-fix", "wave-implement"). HandoffMode is
// the executor-resolved delivery contract.
func (e *Executor) childIntentForApprentice(beadID, baseBranch, reasonTag string, phase intent.Phase, handoffMode HandoffMode) intent.WorkloadIntent {
	return intent.WorkloadIntent{
		TaskID:       beadID,
		Reason:       "executor:" + reasonTag,
		RepoIdentity: e.childIntentRepoIdentity(beadID, baseBranch),
		FormulaPhase: string(phase),
		HandoffMode:  string(handoffMode),
		Role:         intent.RoleApprentice,
		Phase:        phase,
		Runtime: intent.Runtime{
			Image: clusterAgentImage(),
		},
	}
}

// childIntentForSage builds a WorkloadIntent for a sage child run
// (review or arbiter). Same shape as childIntentForApprentice but with
// Role=sage; the (sage, Phase) pair must appear in intent.Allowed.
func (e *Executor) childIntentForSage(beadID, baseBranch, reasonTag string, phase intent.Phase, handoffMode HandoffMode) intent.WorkloadIntent {
	return intent.WorkloadIntent{
		TaskID:       beadID,
		Reason:       "executor:" + reasonTag,
		RepoIdentity: e.childIntentRepoIdentity(beadID, baseBranch),
		FormulaPhase: string(phase),
		HandoffMode:  string(handoffMode),
		Role:         intent.RoleSage,
		Phase:        phase,
		Runtime: intent.Runtime{
			Image: clusterAgentImage(),
		},
	}
}

// dispatchClusterChildAndWait emits the intent and returns. In
// cluster-native dispatch the executor never blocks on a local handle
// because no local process is created — the operator's reconciler picks
// up the intent asynchronously. This helper exists so call sites have
// one obvious shape (build intent → emit → return error) rather than
// inlining the same context.Background() + error wrapping at every
// site.
func (e *Executor) dispatchClusterChildAndWait(reasonTag string, wi intent.WorkloadIntent) error {
	if e == nil || e.deps == nil || e.deps.ClusterChildDispatcher == nil {
		return fmt.Errorf("cluster child dispatch (%s): no ClusterChildDispatcher wired", reasonTag)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := e.deps.ClusterChildDispatcher.Dispatch(ctx, wi); err != nil {
		return fmt.Errorf("cluster child dispatch (%s) for %s: %w", reasonTag, wi.TaskID, err)
	}
	return nil
}

// clusterAgentImage returns the agent container image the cluster
// reconciler should materialize the child pod from. The operator
// requires intent.Runtime.Image to be non-empty (intent.Validate
// rejects an empty value). The canonical source in cluster-native
// deployments is the SPIRE_AGENT_IMAGE env var the helm chart sets on
// the wizard pod — the same env var pkg/agent/backend_k8s.go already
// reads. Empty when unset; the dispatcher's Validate call will then
// surface the configuration gap rather than silently shipping a
// rejected intent.
//
// Defined as a package-level var so tests can override it without
// touching process env.
var clusterAgentImage = func() string {
	return os.Getenv("SPIRE_AGENT_IMAGE")
}

// actionDispatchChildren orchestrates child bead execution.
// Reads With parameters:
//
//	strategy: "dependency-wave" | "sequential" | "direct" (default: "direct")
//
// Workspace: resolved to a StagingWorktree for merge integration.
// Produces: status ("pass"/"fail"), failed_subtasks (comma-separated IDs)
func actionDispatchChildren(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	strategy := step.With["strategy"]
	if strategy == "" {
		strategy = "direct"
	}

	// Parse formula-declared conflict_max_turns.  When unset (empty string),
	// conflictMaxTurns stays 0 and the --max-turns flag is omitted entirely —
	// the executor does not invent a turn budget.
	var conflictMaxTurns int
	if raw := step.With["conflict_max_turns"]; raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			conflictMaxTurns = v
		}
	}
	resolver := e.conflictResolver(conflictMaxTurns)

	// Resolve staging worktree from the step's declared workspace when present.
	// This keeps dispatch aligned with the actual integration workspace branch
	// instead of assuming state.StagingBranch is the right ref.
	var stagingWt *spgit.StagingWorktree
	var err error
	if step.Workspace != "" {
		dir, wsErr := e.resolveGraphWorkspace(step.Workspace, state)
		if wsErr != nil {
			return ActionResult{Error: fmt.Errorf("resolve workspace %q for dispatch: %w", step.Workspace, wsErr)}
		}
		ws := state.Workspaces[step.Workspace]
		state.WorktreeDir = dir
		if ws.Kind == formula.WorkspaceKindStaging {
			stagingWt, err = e.ensureGraphStagingWorktree(state)
			if err != nil {
				return ActionResult{Error: fmt.Errorf("ensure staging workspace %q for dispatch: %w", step.Workspace, err)}
			}
		} else {
			if ws.Branch != "" {
				state.StagingBranch = ws.Branch
			}
			if ws.BaseBranch != "" {
				state.BaseBranch = ws.BaseBranch
			}
			stagingWt = spgit.ResumeStagingWorktree(state.RepoPath, dir, ws.Branch, ws.BaseBranch, e.log)
		}
	} else {
		stagingWt, err = e.ensureGraphStagingWorktree(state)
		if err != nil {
			return ActionResult{Error: fmt.Errorf("resolve workspace for dispatch: %w", err)}
		}
	}

	model := step.Model

	// Resolve apprentice concurrency cap (most-specific wins):
	//   step.With["max-apprentices"] > e.deps.MaxApprentices > default (3)
	// Env-var precedence (SPIRE_MAX_APPRENTICES) is applied in the CLI bridge
	// when Deps is built; by the time it reaches here it's already baked into
	// e.deps.MaxApprentices.
	maxApprentices := e.deps.MaxApprentices
	if raw := step.With["max-apprentices"]; raw != "" {
		if n, convErr := strconv.Atoi(raw); convErr == nil && n > 0 {
			maxApprentices = n
		}
	}
	if maxApprentices <= 0 {
		maxApprentices = repoconfig.DefaultMaxApprentices
	}

	switch strategy {
	case "dependency-wave":
		waves, waveErr := ComputeWaves(e.beadID, e.deps)
		if waveErr != nil {
			return ActionResult{Error: fmt.Errorf("compute waves: %w", waveErr)}
		}
		if len(waves) == 0 {
			e.log("no open subtasks for dispatch")
			return ActionResult{Outputs: map[string]string{"status": "pass", "dispatched": "0"}}
		}
		results, dispErr := e.dispatchWaveCore(waves, stagingWt, model, resolver, maxApprentices)
		return buildDispatchResult(results, dispErr)

	case "sequential":
		waves, waveErr := ComputeWaves(e.beadID, e.deps)
		if waveErr != nil {
			return ActionResult{Error: fmt.Errorf("compute waves: %w", waveErr)}
		}
		if len(waves) == 0 {
			e.log("no open subtasks for sequential dispatch")
			return ActionResult{Outputs: map[string]string{"status": "pass", "dispatched": "0"}}
		}
		// Flatten waves into ordered list.
		var subtasks []string
		for _, wave := range waves {
			subtasks = append(subtasks, wave...)
		}
		results, dispErr := e.dispatchSequentialCore(subtasks, stagingWt, model, resolver)
		return buildDispatchResult(results, dispErr)

	case "direct":
		dispErr := e.dispatchDirectCore(stagingWt, model, resolver)
		if dispErr != nil {
			return ActionResult{
				Outputs: map[string]string{"status": "fail"},
				Error:   fmt.Errorf("direct dispatch: %w", dispErr),
			}
		}
		return ActionResult{Outputs: map[string]string{"status": "pass", "dispatched": "1"}}

	default:
		return ActionResult{Error: fmt.Errorf("unknown dispatch strategy %q", strategy)}
	}
}

// buildDispatchResult converts dispatch results into an ActionResult.
func buildDispatchResult(results []childResult, err error) ActionResult {
	if err != nil {
		return ActionResult{
			Outputs: map[string]string{"status": "fail"},
			Error:   err,
		}
	}

	var failed []string
	for _, r := range results {
		if r.Err != nil {
			failed = append(failed, r.BeadID)
		}
	}

	outputs := map[string]string{
		"status":     "pass",
		"dispatched": fmt.Sprintf("%d", len(results)),
	}
	if len(failed) > 0 {
		outputs["status"] = "fail"
		outputs["failed_subtasks"] = strings.Join(failed, ",")
	}
	return ActionResult{Outputs: outputs}
}

// childResult tracks the outcome of dispatching a single child.
type childResult struct {
	BeadID string
	Agent  string
	Branch string
	Err    error
}

// dispatchWaveCore executes children in dependency waves, merging each wave's
// branches into staging before proceeding to the next wave. It does NOT include
// build verification, build-fix retry, or subtask closing (those are separate
// formula steps).
//
// maxApprentices caps how many apprentice subprocesses run concurrently within
// a single wave. Callers pass the resolved value (>=1); a non-positive value
// falls back to repoconfig.DefaultMaxApprentices.
//
// After the initial waves complete, dispatchWaveCore re-queries the epic's
// children for any that were injected after the plan phase and have not yet
// been dispatched. Any such children are dispatched as additional waves until
// the set is empty. This closes the protocol gap described in spi-g4yi6j:
// before the fix, a `spire inject` after wave-1 could silently strand the new
// child because the wizard never re-scanned for ready children after wave
// completion.
func (e *Executor) dispatchWaveCore(waves [][]string, stagingWt *spgit.StagingWorktree, model string, resolver func(string, string) error, maxApprentices int) ([]childResult, error) {
	if maxApprentices <= 0 {
		maxApprentices = repoconfig.DefaultMaxApprentices
	}
	e.log("dispatching %d wave(s) (max %d concurrent apprentice(s))", len(waves), maxApprentices)

	// Cross-owner dispatch: the apprentice produces a delivery artifact
	// (bundle) that the parent merges into staging, OR takes the legacy
	// push-transport path that phase 5a quarantines as HandoffTransitional.
	// The selection is resolved once up front from the tower config so every
	// apprentice in every wave carries the same handoff mode.
	apprenticeHandoff := e.resolveApprenticeHandoff()

	var allResults []childResult
	startRef := ""

	// Track which bead IDs have been dispatched across all waves (including
	// re-scan waves). Pre-populate from the initial set so ComputeWaves can
	// be called with this as a skip-list.
	dispatched := make(map[string]bool)
	for _, w := range waves {
		for _, id := range w {
			dispatched[id] = true
		}
	}

	waveIdx := 0
	for _, wave := range waves {
		newStartRef, err := e.runDispatchWave(wave, waveIdx, startRef, stagingWt, model, resolver, maxApprentices, apprenticeHandoff, &allResults)
		if err != nil {
			return allResults, err
		}
		startRef = newStartRef
		waveIdx++
	}

	// Re-scan for late-injected children. An inject that arrives after the
	// initial wave set was computed (e.g. archmage runs `spire inject` while
	// wave-1 is mid-flight) writes a new subtask into the epic's children
	// list. Without a re-scan here the new child falls through dispatch and
	// the epic closes with it stranded (spi-g4yi6j).
	//
	// The re-scan depends on GetChildren + GetBlockedIssues being wired. In
	// production both are set on Deps; legacy test harnesses that bypass
	// actionDispatchChildren and call dispatchWaveCore directly sometimes
	// leave them nil — treat that as "no re-scan" so those tests still
	// exercise the wave-dispatch machinery without a panic in ComputeWaves.
	if e.deps == nil || e.deps.GetChildren == nil || e.deps.GetBlockedIssues == nil {
		return allResults, nil
	}
	for {
		newWaves, waveErr := ComputeWaves(e.beadID, e.deps)
		if waveErr != nil {
			e.log("wave re-scan: compute waves failed: %s", waveErr)
			break
		}
		// Flatten and filter out already-dispatched beads. Eager close at
		// the apprentice/bundle seam (closeChildAfterBundleSignal) means
		// children whose apprentice has produced a bundle outcome fall
		// out of ComputeWaves naturally — they're closed and ComputeWaves
		// only returns non-closed subtasks. The dispatched[id] map still
		// has work to do for in-flight (mid-dispatch, pre-bundle-signal)
		// children that are in_progress but not yet closed; without this
		// skip-list a late-inject re-scan landing while wave-N is still
		// dispatching would re-dispatch them.
		var batch []string
		for _, w := range newWaves {
			for _, id := range w {
				if !dispatched[id] {
					batch = append(batch, id)
				}
			}
		}
		if len(batch) == 0 {
			break
		}
		e.log("wave re-scan: %d late-injected child(ren) detected — dispatching as wave %d", len(batch), waveIdx+1)
		for _, id := range batch {
			dispatched[id] = true
		}
		newStartRef, err := e.runDispatchWave(batch, waveIdx, startRef, stagingWt, model, resolver, maxApprentices, apprenticeHandoff, &allResults)
		if err != nil {
			return allResults, err
		}
		startRef = newStartRef
		waveIdx++
	}

	return allResults, nil
}

// runDispatchWave dispatches a single wave: spawns apprentices (capped by
// maxApprentices), collects their results, and merges successful bundles into
// staging. It appends to *allResults and returns the updated startRef for the
// next wave. Extracted from dispatchWaveCore so the same single-wave logic is
// shared between initial waves and re-scan waves.
func (e *Executor) runDispatchWave(
	wave []string,
	waveIdx int,
	startRef string,
	stagingWt *spgit.StagingWorktree,
	model string,
	resolver func(string, string) error,
	maxApprentices int,
	apprenticeHandoff HandoffMode,
	allResults *[]childResult,
) (string, error) {
	e.log("=== wave %d: %d subtask(s) ===", waveIdx, len(wave))

	type waveResult struct {
		BeadID string
		Agent  string
		Err    error
	}

	var wg sync.WaitGroup
	resultCh := make(chan waveResult, len(wave))
	sem := make(chan struct{}, maxApprentices)

	useCluster := e.useClusterChildDispatch()

	for i, subtaskID := range wave {
		wg.Add(1)
		go func(idx int, beadID string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			name := fmt.Sprintf("%s-w%d-%d", e.agentName, waveIdx+1, idx+1)
			e.log("  dispatching %s for %s", name, beadID)

			if err := lifecycle.RecordEvent(context.Background(), beadID, lifecycle.FormulaStepStarted{Step: "implement"}); err != nil {
				e.log("  warning: lifecycle dispatch transition for %s: %s", beadID, err)
			}

			started := time.Now()

			if useCluster {
				// Cluster-native: emit a WorkloadIntent through the
				// .1 seam. The operator materializes the apprentice
				// pod; no local handle to wait on. Treat publish
				// success as the wave entry's success — the parent's
				// bundle-apply / staging-merge cascade below picks up
				// the work via the BundleStore once the apprentice
				// pod produces it. Failures wrap in the same shape
				// the local-spawn path uses so callers see one error
				// vocabulary.
				wi := e.childIntentForApprentice(beadID, e.runtimeBaseBranch(), "wave-implement", intent.PhaseImplement, apprenticeHandoff)
				if dispErr := e.dispatchClusterChildAndWait("wave-implement", wi); dispErr != nil {
					e.recordAgentRun(name, beadID, e.beadID, model, "apprentice", "implement", started, dispErr,
						withParentRun(e.currentRunID))
					resultCh <- waveResult{BeadID: beadID, Agent: name, Err: dispErr}
					return
				}
				e.recordAgentRun(name, beadID, e.beadID, model, "apprentice", "implement", started, nil,
					withParentRun(e.currentRunID))
				resultCh <- waveResult{BeadID: beadID, Agent: name}
				return
			}

			extraArgs := []string{"--apprentice"}
			cfg := agent.SpawnConfig{
				Name:          name,
				BeadID:        beadID,
				Role:          agent.RoleApprentice,
				ExtraArgs:     extraArgs,
				StartRef:      startRef,
				LogPath:       filepath.Join(dolt.GlobalDir(), "wizards", name+".log"),
				AttemptID:     e.attemptID(),
				ApprenticeIdx: "0",
			}
			cfg, contractErr := e.withRuntimeContract(cfg, e.runtimeTowerName(), e.effectiveRepoPath(), e.runtimeBaseBranch(), "implement", "", nil, apprenticeHandoff)
			if contractErr != nil {
				e.recordAgentRun(name, beadID, e.beadID, model, "apprentice", "implement", started, contractErr,
					withParentRun(e.currentRunID))
				resultCh <- waveResult{BeadID: beadID, Agent: name, Err: contractErr}
				return
			}
			h, spawnErr := e.deps.Spawner.Spawn(cfg)
			if spawnErr != nil {
				e.recordAgentRun(name, beadID, e.beadID, model, "apprentice", "implement", started, spawnErr,
					withParentRun(e.currentRunID))
				resultCh <- waveResult{BeadID: beadID, Agent: name, Err: spawnErr}
				return
			}
			waitErr := e.waitHandleWithHeartbeat(h)
			e.recordAgentRun(name, beadID, e.beadID, model, "apprentice", "implement", started, waitErr,
				withParentRun(e.currentRunID))
			if waitErr != nil {
				resultCh <- waveResult{BeadID: beadID, Agent: name, Err: waitErr}
				return
			}
			resultCh <- waveResult{BeadID: beadID, Agent: name}
		}(i, subtaskID)
	}

	wg.Wait()
	close(resultCh)

	// Collect results.
	var errs []string
	var waveResults []childResult
	for r := range resultCh {
		cr := childResult{
			BeadID: r.BeadID,
			Agent:  r.Agent,
			Branch: e.resolveBranch(r.BeadID),
			Err:    r.Err,
		}
		waveResults = append(waveResults, cr)
		*allResults = append(*allResults, cr)
		if r.Err != nil {
			errs = append(errs, fmt.Sprintf("%s: %s", r.BeadID, r.Err))
		}
	}

	if len(errs) > 0 {
		e.log("wave %d: %d error(s): %s", waveIdx, len(errs), strings.Join(errs, "; "))
	}

	// Apply each successful apprentice's bundle into staging, then merge.
	// No-op signals skip merge entirely. The bundle is deleted only after
	// a successful merge so a conflict can be retried with the bundle
	// still present. The child bead is eagerly closed at the apprentice/
	// bundle seam (see closeChildAfterBundleSignal) as soon as
	// applyApprenticeBundle returns a clean Applied or NoOp outcome —
	// task completion is the apprentice's responsibility, not the
	// wizard's downstream merge mechanics. A subsequent MergeBranch
	// failure does NOT reopen the bead; the failure surfaces through the
	// wizard's existing error path / recovery bead naming the offending
	// child. The legacy push-fetch fallback (no BundleStore) keeps the
	// post-MergeBranch close — that path has no separate bundle signal.
	if stagingWt != nil {
		for _, cr := range waveResults {
			if cr.Err != nil {
				continue
			}
			if e.deps.BundleStore != nil {
				outcome, err := e.applyApprenticeBundle(cr.BeadID, 0, stagingWt)
				if err != nil {
					return startRef, fmt.Errorf("apply apprentice bundle for %s: %w", cr.BeadID, err)
				}
				if outcome.NoOp {
					e.closeChildAfterBundleSignal(cr.BeadID)
					continue
				}
				if outcome.Applied {
					e.closeChildAfterBundleSignal(cr.BeadID)
					if mergeErr := stagingWt.MergeBranch(outcome.Branch, resolver); mergeErr != nil {
						return startRef, fmt.Errorf("merge %s into staging: %w", outcome.Branch, mergeErr)
					}
					e.deleteApprenticeBundle(cr.BeadID, outcome.Handle)
					continue
				}
			}
			// Push-transport / legacy fallback: fetch the apprentice's
			// feat branch from the remote (idempotent; cheap) then merge.
			// No bundle-signal event in this path, so the close trigger
			// stays on MergeBranch success (spi-b2qjqv semantics).
			if cr.Branch == "" {
				continue
			}
			if fetchErr := stagingWt.FetchBranch("origin", cr.Branch); fetchErr != nil {
				e.log("fetch %s (best-effort): %s", cr.Branch, fetchErr)
			}
			if mergeErr := stagingWt.MergeBranch(cr.Branch, resolver); mergeErr != nil {
				return startRef, fmt.Errorf("merge %s into staging: %w", cr.Branch, mergeErr)
			}
			e.closeChildAfterBundleSignal(cr.BeadID)
		}

		// Update startRef for next wave.
		if sha, err := stagingWt.HeadSHA(); err == nil && sha != "" {
			startRef = sha
			e.log("next wave start-ref: %s", startRef)
		}
	}

	return startRef, nil
}

// dispatchSequentialCore executes children one at a time, merging each into
// staging before advancing. It does NOT include inline review, inline
// merge-to-main, or subtask closing (those are separate formula steps).
func (e *Executor) dispatchSequentialCore(subtasks []string, stagingWt *spgit.StagingWorktree, model string, resolver func(string, string) error) ([]childResult, error) {
	e.log("sequential dispatch: %d subtask(s)", len(subtasks))

	// Same cross-owner dispatch handoff as the wave path — the tower's
	// configured apprentice transport decides bundle vs transitional.
	apprenticeHandoff := e.resolveApprenticeHandoff()

	useCluster := e.useClusterChildDispatch()

	var allResults []childResult
	var startRef string

	for i, subtaskID := range subtasks {
		e.log("=== sequential step %d/%d: %s ===", i+1, len(subtasks), subtaskID)
		if err := lifecycle.RecordEvent(context.Background(), subtaskID, lifecycle.FormulaStepStarted{Step: "implement"}); err != nil {
			e.log("warning: lifecycle dispatch transition for %s: %s", subtaskID, err)
		}

		name := fmt.Sprintf("%s-seq-%d", e.agentName, i+1)
		started := time.Now()

		if useCluster {
			// Cluster-native: emit a WorkloadIntent through the .1
			// seam. The operator materializes the apprentice pod;
			// the executor records the dispatch and continues. The
			// downstream bundle-apply / merge cascade picks up the
			// child's work asynchronously through the BundleStore
			// once the apprentice produces it.
			wi := e.childIntentForApprentice(subtaskID, e.runtimeBaseBranch(), "sequential-implement", intent.PhaseImplement, apprenticeHandoff)
			if dispErr := e.dispatchClusterChildAndWait("sequential-implement", wi); dispErr != nil {
				e.recordAgentRun(name, subtaskID, e.beadID, model, "apprentice", "implement", started, dispErr,
					withParentRun(e.currentRunID))
				return allResults, fmt.Errorf("cluster dispatch apprentice for %s: %w", subtaskID, dispErr)
			}
			e.recordAgentRun(name, subtaskID, e.beadID, model, "apprentice", "implement", started, nil,
				withParentRun(e.currentRunID))

			featBranch := e.resolveBranch(subtaskID)
			cr := childResult{
				BeadID: subtaskID,
				Agent:  name,
				Branch: featBranch,
			}
			allResults = append(allResults, cr)
			continue
		}

		cfg := agent.SpawnConfig{
			Name:          name,
			BeadID:        subtaskID,
			Role:          agent.RoleApprentice,
			ExtraArgs:     []string{"--apprentice"},
			StartRef:      startRef,
			LogPath:       filepath.Join(dolt.GlobalDir(), "wizards", name+".log"),
			AttemptID:     e.attemptID(),
			ApprenticeIdx: "0",
		}
		cfg, contractErr := e.withRuntimeContract(cfg, e.runtimeTowerName(), e.effectiveRepoPath(), e.runtimeBaseBranch(), "implement", "", nil, apprenticeHandoff)
		if contractErr != nil {
			e.recordAgentRun(name, subtaskID, e.beadID, model, "apprentice", "implement", started, contractErr,
				withParentRun(e.currentRunID))
			return allResults, fmt.Errorf("handoff selection for %s: %w", subtaskID, contractErr)
		}
		handle, spawnErr := e.deps.Spawner.Spawn(cfg)
		if spawnErr != nil {
			e.recordAgentRun(name, subtaskID, e.beadID, model, "apprentice", "implement", started, spawnErr,
				withParentRun(e.currentRunID))
			return allResults, fmt.Errorf("spawn apprentice for %s: %w", subtaskID, spawnErr)
		}
		waitErr := e.waitHandleWithHeartbeat(handle)
		e.recordAgentRun(name, subtaskID, e.beadID, model, "apprentice", "implement", started, waitErr,
			withParentRun(e.currentRunID))

		featBranch := e.resolveBranch(subtaskID)
		cr := childResult{
			BeadID: subtaskID,
			Agent:  name,
			Branch: featBranch,
			Err:    waitErr,
		}
		allResults = append(allResults, cr)

		if waitErr != nil {
			return allResults, fmt.Errorf("apprentice %s failed: %w", subtaskID, waitErr)
		}

		// Apply the apprentice's bundle into staging, then merge. No-op
		// signals skip merge entirely. The bundle is deleted only after a
		// successful merge so a conflict can be retried with the bundle
		// still present. The child bead is eagerly closed at the
		// apprentice/bundle seam (see closeChildAfterBundleSignal) as
		// soon as applyApprenticeBundle returns a clean Applied or NoOp
		// outcome — task completion is the apprentice's responsibility,
		// not the wizard's downstream merge mechanics. A subsequent
		// MergeBranch failure does NOT reopen the bead; the failure
		// surfaces through the wizard's existing error path / recovery
		// bead naming the offending child. The legacy push-fetch
		// fallback (no BundleStore) keeps the post-MergeBranch close —
		// that path has no separate bundle signal.
		if stagingWt != nil {
			bundleHandled := false
			if e.deps.BundleStore != nil {
				outcome, err := e.applyApprenticeBundle(subtaskID, 0, stagingWt)
				if err != nil {
					return allResults, fmt.Errorf("apply apprentice bundle for %s: %w", subtaskID, err)
				}
				switch {
				case outcome.NoOp:
					e.closeChildAfterBundleSignal(subtaskID)
					bundleHandled = true
				case outcome.Applied:
					e.closeChildAfterBundleSignal(subtaskID)
					if mergeErr := stagingWt.MergeBranch(outcome.Branch, resolver); mergeErr != nil {
						return allResults, fmt.Errorf("merge %s into staging: %w", outcome.Branch, mergeErr)
					}
					e.deleteApprenticeBundle(subtaskID, outcome.Handle)
					bundleHandled = true
				}
			}
			if !bundleHandled {
				// Push-transport / legacy fallback: fetch the apprentice's
				// feat branch from the remote (idempotent) then merge. No
				// bundle-signal event in this path, so the close trigger
				// stays on MergeBranch success (spi-b2qjqv semantics).
				if fetchErr := stagingWt.FetchBranch("origin", featBranch); fetchErr != nil {
					e.log("fetch %s (best-effort): %s", featBranch, fetchErr)
				}
				if mergeErr := stagingWt.MergeBranch(featBranch, resolver); mergeErr != nil {
					return allResults, fmt.Errorf("merge %s into staging: %w", featBranch, mergeErr)
				}
				e.closeChildAfterBundleSignal(subtaskID)
			}
			// Update startRef for next child.
			if sha, err := stagingWt.HeadSHA(); err == nil && sha != "" {
				startRef = sha
				e.log("next sequential start-ref: %s", startRef)
			}
		}
	}

	return allResults, nil
}

// dispatchDirectCore spawns one apprentice for the bead, then merges the
// feat branch into staging.
func (e *Executor) dispatchDirectCore(stagingWt *spgit.StagingWorktree, model string, resolver func(string, string) error) error {
	apprenticeName := fmt.Sprintf("%s-impl", e.agentName)
	e.log("dispatching apprentice %s", apprenticeName)

	// Same cross-owner dispatch handoff selection: bundle when the tower
	// configures it, otherwise the quarantined transitional push path.
	apprenticeHandoff := e.resolveApprenticeHandoff()

	started := time.Now()

	if e.useClusterChildDispatch() {
		// Cluster-native: emit a WorkloadIntent through the .1 seam.
		// The operator materializes the apprentice pod from
		// Runtime.Image; the executor records the dispatch and
		// returns. Downstream bundle-apply / merge mechanics pick up
		// the apprentice's output asynchronously through the
		// BundleStore once the operator-materialized pod produces it.
		wi := e.childIntentForApprentice(e.beadID, e.runtimeBaseBranch(), "direct-implement", intent.PhaseImplement, apprenticeHandoff)
		if dispErr := e.dispatchClusterChildAndWait("direct-implement", wi); dispErr != nil {
			e.recordAgentRun(apprenticeName, e.beadID, "", model, "apprentice", "implement", started, dispErr,
				withParentRun(e.currentRunID))
			return fmt.Errorf("cluster dispatch apprentice: %w", dispErr)
		}
		e.recordAgentRun(apprenticeName, e.beadID, "", model, "apprentice", "implement", started, nil,
			withParentRun(e.currentRunID))
		e.log("apprentice dispatched (cluster-native)")
		return nil
	}

	cfg := agent.SpawnConfig{
		Name:          apprenticeName,
		BeadID:        e.beadID,
		Role:          agent.RoleApprentice,
		ExtraArgs:     []string{"--apprentice"},
		LogPath:       filepath.Join(dolt.GlobalDir(), "wizards", apprenticeName+".log"),
		AttemptID:     e.attemptID(),
		ApprenticeIdx: "0",
	}
	cfg, contractErr := e.withRuntimeContract(cfg, e.runtimeTowerName(), e.effectiveRepoPath(), e.runtimeBaseBranch(), "implement", "", nil, apprenticeHandoff)
	if contractErr != nil {
		e.recordAgentRun(apprenticeName, e.beadID, "", model, "apprentice", "implement", started, contractErr,
			withParentRun(e.currentRunID))
		return fmt.Errorf("handoff selection: %w", contractErr)
	}
	handle, err := e.deps.Spawner.Spawn(cfg)
	if err != nil {
		e.recordAgentRun(apprenticeName, e.beadID, "", model, "apprentice", "implement", started, err,
			withParentRun(e.currentRunID))
		return fmt.Errorf("spawn apprentice: %w", err)
	}

	waitErr := e.waitHandleWithHeartbeat(handle)
	e.recordAgentRun(apprenticeName, e.beadID, "", model, "apprentice", "implement", started, waitErr,
		withParentRun(e.currentRunID))
	if waitErr != nil {
		e.log("apprentice failed: %s", waitErr)
		return fmt.Errorf("apprentice: %w", waitErr)
	}

	e.log("apprentice completed")

	// Apply the apprentice's submitted bundle onto staging, then merge.
	// A no-op signal short-circuits: nothing to merge.
	if stagingWt == nil {
		return nil
	}
	if e.deps.BundleStore != nil {
		outcome, err := e.applyApprenticeBundle(e.beadID, 0, stagingWt)
		if err != nil {
			return fmt.Errorf("apply apprentice bundle: %w", err)
		}
		if outcome.NoOp {
			return nil
		}
		if outcome.Applied {
			if mergeErr := stagingWt.MergeBranch(outcome.Branch, resolver); mergeErr != nil {
				return fmt.Errorf("merge %s into staging: %w", outcome.Branch, mergeErr)
			}
			e.deleteApprenticeBundle(e.beadID, outcome.Handle)
			return nil
		}
	}

	// Push-transport / legacy fallback: fetch the apprentice's feat branch
	// from the remote (idempotent; succeeds silently if already local) then
	// merge.
	featBranch := fmt.Sprintf("feat/%s", e.beadID)
	e.log("merging %s into staging (push/legacy path)", featBranch)
	if fetchErr := stagingWt.FetchBranch("origin", featBranch); fetchErr != nil {
		e.log("fetch %s (best-effort): %s", featBranch, fetchErr)
	}
	if mergeErr := stagingWt.MergeBranch(featBranch, resolver); mergeErr != nil {
		return fmt.Errorf("merge %s into staging: %w", featBranch, mergeErr)
	}
	return nil
}

// closeChildAfterBundleSignal transitions a child task bead to "closed"
// once the apprentice has produced its bundle outcome (Applied or NoOp).
// The seam is the apprentice/bundle boundary — task completion is owned
// by the apprentice, not by the wizard's downstream merge mechanics.
// runDispatchWave and dispatchSequentialCore call this on the bundle-
// store path immediately after applyApprenticeBundle returns a clean
// outcome, before MergeBranch runs. A subsequent merge failure does NOT
// reopen the bead — the failure surfaces through the wizard's existing
// error path / recovery bead naming the offending child.
//
// On the legacy push-fetch fallback (BundleStore == nil) there is no
// bundle-signal event, so the helper is invoked after MergeBranch
// success — preserving the spi-b2qjqv staging-merge close semantics for
// that migration-period path.
//
// Idempotent: short-circuits when GetBead reports the bead is already
// closed. The underlying CloseIssue path matches by id only with no
// status filter (see internal/storage/dolt/issues.go), so a redundant
// call would re-stamp closed_at and emit a duplicate EventClosed —
// which is why the status pre-check is load-bearing for the defensive
// cascade in actionBeadFinish.
//
// Errors are non-fatal: the helper only logs and returns. The defensive
// cascade in actionBeadFinish closes any survivors at epic close (e.g.
// if the wizard dies between applyApprenticeBundle returning and this
// helper running). Direct dispatch (dispatchDirectCore) does not call
// this — its single apprentice runs against the parent's own bead, so
// closing the parent mid-flight would be wrong.
func (e *Executor) closeChildAfterBundleSignal(childID string) {
	if e.deps == nil || e.deps.CloseBead == nil {
		return
	}
	if e.deps.GetBead != nil {
		if bead, err := e.deps.GetBead(childID); err == nil && bead.Status == "closed" {
			return
		}
	}
	if err := e.deps.CloseBead(childID); err != nil {
		e.log("warning: close child %s after bundle signal: %s", childID, err)
	}
}
