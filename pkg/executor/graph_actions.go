package executor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/dolt"
	"github.com/awell-health/spire/pkg/formula"
	spgit "github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// ActionResult is the return type from an action handler.
type ActionResult struct {
	Outputs map[string]string
	Error   error
	Hooked  bool
}

// ActionHandler is the signature for a graph action handler.
type ActionHandler func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult

// actionRegistry maps opcode strings to handler functions.
// graph.run is registered in init() to break the initialization cycle
// (actionGraphRun → RunNestedGraph → dispatchAction → actionRegistry).
var actionRegistry = map[string]ActionHandler{
	"wizard.run":             actionWizardRun,
	"check.design-linked":    actionCheckDesignLinked,
	"beads.materialize_plan": actionMaterializePlan,
	"dispatch.children":      actionDispatchChildren,
	"verify.run":             actionVerifyRun,
	"git.merge_to_main":      actionMergeToMain,
	"bead.finish":            actionBeadFinish,
	"human.approve":          actionHumanApprove,
	"noop":                   actionNoop,
}

func init() {
	actionRegistry["graph.run"] = actionGraphRun
}

// dispatchAction looks up step.Action in the action registry and calls the handler.
func (e *Executor) dispatchAction(stepName string, step StepConfig, state *GraphState) ActionResult {
	if step.Action == "" {
		return ActionResult{Error: fmt.Errorf("step %q has no action defined", stepName)}
	}
	// Interpolate {steps.X.outputs.Y} and {vars.Z} in With values.
	// Copy the map to avoid mutating the formula definition.
	if len(step.With) > 0 {
		copied := make(map[string]string, len(step.With))
		for k, v := range step.With {
			copied[k] = v
		}
		step.With = copied
		e.interpolateWith(&step, state)
	}
	handler, ok := actionRegistry[step.Action]
	if !ok {
		return ActionResult{Error: fmt.Errorf("unknown action %q for step %q", step.Action, stepName)}
	}
	return handler(e, stepName, step, state)
}

// interpolateWith resolves {steps.X.outputs.Y}, {vars.Z}, etc. in step.With values
// using the same context map that conditions use.
func (e *Executor) interpolateWith(step *StepConfig, state *GraphState) {
	if len(step.With) == 0 {
		return
	}
	ctx := e.buildConditionContext(state)
	for k, v := range step.With {
		if !strings.Contains(v, "{") {
			continue
		}
		for ctxKey, ctxVal := range ctx {
			v = strings.ReplaceAll(v, "{"+ctxKey+"}", ctxVal)
		}
		step.With[k] = v
	}
}

// --- Real implementations ---

// effectiveRepoPath returns the repo path from the v2 state or v3 graph state,
// whichever is available. This allows plan methods (which historically used
// e.state.RepoPath) to work in both v2 and v3 execution contexts.
func (e *Executor) effectiveRepoPath() string {
	if e.state != nil && e.state.RepoPath != "" {
		return e.state.RepoPath
	}
	if e.graphState != nil {
		return e.graphState.RepoPath
	}
	return "."
}

// actionWizardRun maps step.Flow to the existing wizard dispatch: calls
// e.deps.Spawner.Spawn() with the appropriate role and args based on step.Flow,
// or invokes executor planning methods directly for plan flows.
func actionWizardRun(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	flow := step.Flow
	if flow == "" {
		flow = step.With["flow"]
	}

	// Resolve workspace dir from step declaration or fall back to state.WorktreeDir.
	wsDir := state.WorktreeDir
	workspace := graphWorkspaceHandle(state, step.Workspace)
	if step.Workspace != "" {
		dir, err := e.resolveGraphWorkspace(step.Workspace, state)
		if err != nil {
			return ActionResult{Error: fmt.Errorf("resolve workspace %q for %s: %w", step.Workspace, stepName, err)}
		}
		wsDir = dir
		workspace = graphWorkspaceHandle(state, step.Workspace)
	}
	if workspace == nil && wsDir != "" {
		workspace = graphWorkspaceHandleForPath(state, wsDir)
		if workspace == nil {
			workspace = inferWorkspaceHandle(state.RepoPath, wsDir, state.StagingBranch, state.BaseBranch)
		}
	}

	switch flow {
	case "task-plan":
		return actionPlanTask(e, stepName, step, state)
	case "epic-plan":
		return actionPlanEpic(e, stepName, step, state)
	case "implement":
		extraArgs := []string{"--apprentice"}
		if wsDir != "" {
			extraArgs = append(extraArgs, "--worktree-dir", wsDir)
		}
		return wizardRunSpawn(e, stepName, step, state, agent.RoleApprentice, extraArgs, workspace)
	case "sage-review":
		extraArgs := []string{}
		if step.VerdictOnly {
			extraArgs = append(extraArgs, "--verdict-only")
		}
		if wsDir != "" {
			extraArgs = append(extraArgs, "--worktree-dir", wsDir)
		}
		result := wizardRunSpawn(e, stepName, step, state, agent.RoleSage, extraArgs, workspace)
		// Promote the review verdict into outputs so the formula can route on
		// steps.sage-review.outputs.verdict. The generic result field from
		// the sage review subprocess carries the verdict string.
		verdict := result.Outputs["result"]
		if verdict == "approve" || verdict == "request_changes" {
			result.Outputs["verdict"] = verdict
		}
		// Note: review_round is no longer mutated here. The interpreter tracks
		// completed_count mechanically for every step; the formula routes on
		// steps.sage-review.completed_count instead.
		return result
	case "recovery-verify":
		var extraArgs []string
		if wsDir != "" {
			extraArgs = append(extraArgs, "--worktree-dir", wsDir)
		}
		result := wizardRunSpawn(e, stepName, step, state, agent.RoleApprentice, extraArgs, workspace)
		// Promote the result field to verification_status so the formula can
		// route on steps.verify.outputs.verification_status. The apprentice
		// writes a generic "result" field (e.g. "pass" or "fail") to
		// result.json; the agentResultJSON struct only surfaces Result,
		// Branch, Commit — no arbitrary fields. This mirrors the sage-review
		// pattern that promotes result → verdict.
		if v := result.Outputs["result"]; v == "pass" || v == "fail" {
			result.Outputs["verification_status"] = v
		}
		return result
	case "review-fix":
		// Principle 1 (spi-1dk71j / spi-tlj32a): the fix apprentice produces
		// commits, so it MUST deliver a bundle — no borrowed handoff. Both
		// the fix apprentice and the worker-mode cleric repair (spi-icgqhi)
		// run through actionReviewFix's bundle-handoff path so neither
		// silently regresses to the borrowed model.
		return actionReviewFix(e, stepName, step, state, wsDir, workspace)
	case "arbiter":
		return actionArbiterEscalate(e, stepName, step, state)
	default:
		// For other flows, spawn with apprentice role + workspace if declared.
		var extraArgs []string
		if wsDir != "" {
			extraArgs = append(extraArgs, "--worktree-dir", wsDir)
		}
		return wizardRunSpawn(e, stepName, step, state, agent.RoleApprentice, extraArgs, workspace)
	}
}

// actionPlanTask routes the "task-plan" flow to the executor's inline planning
// method, which invokes Claude to produce an implementation plan comment.
func actionPlanTask(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	bead, err := e.deps.GetBead(e.beadID)
	if err != nil {
		return ActionResult{Error: fmt.Errorf("get bead for task plan: %w", err)}
	}

	if err := e.wizardPlanTask(bead, step.Model, step.MaxTurns); err != nil {
		return ActionResult{Error: fmt.Errorf("task plan: %w", err)}
	}

	return ActionResult{Outputs: map[string]string{"status": "planned"}}
}

// actionPlanEpic routes the "epic-plan" flow to the executor's inline planning
// method, which invokes Claude to break the epic into subtasks and create child beads.
func actionPlanEpic(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	bead, err := e.deps.GetBead(e.beadID)
	if err != nil {
		return ActionResult{Error: fmt.Errorf("get bead for epic plan: %w", err)}
	}

	if err := e.wizardPlanEpic(bead, step.Model, step.MaxTurns); err != nil {
		return ActionResult{Error: fmt.Errorf("epic plan: %w", err)}
	}

	return ActionResult{Outputs: map[string]string{"status": "planned"}}
}

func (e *Executor) runtimeBackend() string {
	if e.deps != nil && e.deps.RepoConfig != nil {
		if cfg := e.deps.RepoConfig(); cfg != nil && cfg.Agent.Backend != "" {
			return cfg.Agent.Backend
		}
	}
	return "process"
}

func (e *Executor) runtimeTowerName() string {
	if e.graphState != nil && e.graphState.TowerName != "" {
		return e.graphState.TowerName
	}
	return ""
}

func (e *Executor) runtimeBaseBranch() string {
	if e.graphState != nil && e.graphState.BaseBranch != "" {
		return e.graphState.BaseBranch
	}
	if e.state != nil && e.state.BaseBranch != "" {
		return e.state.BaseBranch
	}
	return ""
}

// runtimeRepoURL resolves the canonical repo origin URL for the executor's
// active bead/prefix. The source of truth is the active tower's registered-
// repo entry (LocalBindings keyed by bead prefix, with a fall-through to
// the tower's shared repos table via the ypoqx resolver in cmd/spire).
//
// In-process, we only consult ActiveTowerConfig() + LocalBindings: the
// shared repos table lookup lives in cmd/spire/repo_identity.go and is
// intentionally not plumbed here (pkg/executor must not reach into the
// dolt-level resolver). An empty return is a legitimate outcome when no
// tower is bound (e.g. unit tests) — withRuntimeContract callers still
// see cfg.RepoURL (if set) as the ultimate fallback.
//
// This helper is the canonical producer of Identity.RepoURL on every
// same-bead dispatch; no caller should rely on cfg.RepoURL being
// populated for a k8s substrate spawn to succeed.
func (e *Executor) runtimeRepoURL() string {
	if e == nil || e.deps == nil || e.deps.ActiveTowerConfig == nil {
		return ""
	}
	tower, err := e.deps.ActiveTowerConfig()
	if err != nil || tower == nil {
		return ""
	}
	prefix := store.PrefixFromID(e.beadID)
	if prefix == "" {
		return ""
	}
	if binding, ok := tower.LocalBindings[prefix]; ok && binding != nil {
		if binding.RepoURL != "" {
			return binding.RepoURL
		}
	}
	return ""
}

func (e *Executor) runtimeStep(state *GraphState, fallback string) string {
	if state != nil && state.ActiveStep != "" {
		return state.ActiveStep
	}
	if e.graphState != nil && e.graphState.ActiveStep != "" {
		return e.graphState.ActiveStep
	}
	return fallback
}

// runContext assembles the canonical RunContext for log/trace/metric
// emission from the executor's current state. This is the observability
// identity described in docs/design/spi-xplwy-runtime-contract.md §1.4.
//
// The assembled context is deliberately a snapshot: callers (e.log, span
// setup, metric labels) re-read it on each emission so late-bound fields
// (ActiveStep, AttemptBeadID, workspace resolution) reflect the current
// step without needing to push updates through a mutable handle.
//
// All fields render as empty strings when unavailable — missing values
// never drop the field from the log surface.
func (e *Executor) runContext(stepFallback string) RunContext {
	if e == nil {
		return RunContext{}
	}
	state := e.graphState
	run := RunContext{
		RunID: e.currentRunID,
		Role:  agent.RoleExecutor,
	}
	if state != nil {
		run.TowerName = state.TowerName
		run.BeadID = state.BeadID
		run.AttemptID = state.AttemptBeadID
		run.FormulaStep = state.ActiveStep
	}
	if run.BeadID == "" {
		run.BeadID = e.beadID
	}
	if run.AttemptID == "" {
		run.AttemptID = e.attemptID()
	}
	if run.FormulaStep == "" {
		run.FormulaStep = stepFallback
	}
	run.Prefix = prefixFromBeadID(run.BeadID)
	run.Backend = e.runtimeBackend()
	if state != nil && state.ActiveStep != "" {
		if ws := inferActiveWorkspaceHandle(state); ws != nil {
			run.WorkspaceKind = ws.Kind
			run.WorkspaceName = ws.Name
			run.WorkspaceOrigin = ws.Origin
		}
	}
	return run
}

// prefixFromBeadID extracts the bead prefix from a bead ID ("spi-abc" →
// "spi"). Empty input → empty output. This avoids a store.PrefixFromID
// import from the hot log path, which is called on every e.log emission.
func prefixFromBeadID(beadID string) string {
	if beadID == "" {
		return ""
	}
	for i, r := range beadID {
		if r == '-' {
			return beadID[:i]
		}
	}
	return ""
}

// inferActiveWorkspaceHandle returns the WorkspaceHandle for the active
// step's declared workspace, or nil when none can be resolved from state
// alone. Used by runContext() to stamp workspace_kind/name/origin on
// logs without reaching into the formula graph on every emission.
func inferActiveWorkspaceHandle(state *GraphState) *WorkspaceHandle {
	if state == nil || state.ActiveStep == "" {
		return nil
	}
	// When the active step's workspace name matches a persisted workspace,
	// use that. Otherwise fall back to matching by WorktreeDir (the common
	// case for staging workspaces).
	for name, ws := range state.Workspaces {
		if ws.Status == "active" {
			handle := ws.Handle()
			if handle.Name == "" {
				handle.Name = name
			}
			return &handle
		}
	}
	return nil
}

func graphWorkspaceHandle(state *GraphState, name string) *WorkspaceHandle {
	if state == nil || name == "" {
		return nil
	}
	ws, ok := state.Workspaces[name]
	if !ok {
		return nil
	}
	handle := ws.Handle()
	if handle.Name == "" {
		handle.Name = name
	}
	if handle.BaseBranch == "" {
		handle.BaseBranch = state.BaseBranch
	}
	if handle.Kind == WorkspaceKindRepo && handle.Path == "" {
		handle.Path = state.RepoPath
	}
	return &handle
}

func graphWorkspaceHandleForPath(state *GraphState, path string) *WorkspaceHandle {
	if state == nil || path == "" {
		return nil
	}
	if path == state.RepoPath {
		return inferWorkspaceHandle(state.RepoPath, path, "", state.BaseBranch)
	}
	for name, ws := range state.Workspaces {
		if ws.Dir != path {
			continue
		}
		handle := ws.Handle()
		if handle.Name == "" {
			handle.Name = name
		}
		if handle.BaseBranch == "" {
			handle.BaseBranch = state.BaseBranch
		}
		return &handle
	}
	return nil
}

func inferWorkspaceHandle(repoPath, path, branch, baseBranch string) *WorkspaceHandle {
	if path == "" && repoPath == "" {
		return nil
	}
	kind := WorkspaceKindOwnedWorktree
	resolvedPath := path
	if resolvedPath == "" || resolvedPath == repoPath {
		kind = WorkspaceKindRepo
		resolvedPath = repoPath
	}
	return &WorkspaceHandle{
		Kind:       kind,
		Branch:     branch,
		BaseBranch: baseBranch,
		Path:       resolvedPath,
		Origin:     WorkspaceOriginLocalBind,
		Borrowed:   kind != WorkspaceKindRepo,
	}
}

func normalizeWorkspaceHandle(workspace *WorkspaceHandle, workspaceName, repoPath, baseBranch string) *WorkspaceHandle {
	if workspace == nil {
		return &WorkspaceHandle{
			Name:       workspaceName,
			Kind:       WorkspaceKindRepo,
			BaseBranch: baseBranch,
			Path:       repoPath,
			Origin:     WorkspaceOriginLocalBind,
		}
	}
	handle := *workspace
	if handle.Name == "" {
		handle.Name = workspaceName
	}
	if handle.Kind == "" {
		if handle.Path == repoPath {
			handle.Kind = WorkspaceKindRepo
		} else {
			handle.Kind = WorkspaceKindOwnedWorktree
		}
	}
	if handle.BaseBranch == "" {
		handle.BaseBranch = baseBranch
	}
	if handle.Kind == WorkspaceKindRepo && handle.Path == "" {
		handle.Path = repoPath
	}
	if handle.Origin == "" {
		handle.Origin = WorkspaceOriginLocalBind
	}
	return &handle
}

// withRuntimeContract populates the canonical runtime-contract fields on a
// SpawnConfig. Every call site MUST pass an explicit HandoffMode — the
// executor is the single authority on handoff-mode selection (see
// docs/design/spi-xplwy-runtime-contract.md §1.3), so there is no "derive
// from workspace" auto-path. Pass HandoffNone for terminal/no-op spawns.
//
// As a side effect, when the selected mode is HandoffTransitional, this
// function bumps spire_handoff_transitional_total, emits the Warn-level
// deprecation log, and honors the SPIRE_FAIL_ON_TRANSITIONAL_HANDOFF gate
// via recordHandoffSelection. The returned error, when non-nil, must be
// propagated so the caller can fail the spawn.
//
// withRuntimeContract is a thin wrapper around the package-level
// PopulateRuntimeContract: it sources the executor-specific fields
// (attempt ID, repo URL from active tower, backend name, run ID,
// logger) from e's state and delegates the remaining population to the
// shared helper. Dispatch sites outside pkg/executor call
// PopulateRuntimeContract directly.
func (e *Executor) withRuntimeContract(cfg agent.SpawnConfig, towerName, repoPath, baseBranch, runStep, workspaceName string, workspace *WorkspaceHandle, mode HandoffMode) (agent.SpawnConfig, error) {
	if cfg.AttemptID == "" {
		cfg.AttemptID = e.attemptID()
	}
	// RepoURL is resolved from executor state (active tower's registered-
	// repo binding) rather than copied from cfg.RepoURL. This removes the
	// foot-gun that let same-bead k8s dispatches fail at buildSubstratePod
	// with ErrIdentityRequired because no caller had populated cfg.RepoURL
	// (spi-x7fus). If state resolution returns empty (unit tests, no tower
	// bound), PopulateRuntimeContract falls back to cfg.RepoURL.
	return PopulateRuntimeContract(cfg, RuntimeContractInputs{
		TowerName:     towerName,
		RepoURL:       e.runtimeRepoURL(),
		RepoPath:      repoPath,
		BaseBranch:    baseBranch,
		RunStep:       runStep,
		WorkspaceName: workspaceName,
		Workspace:     workspace,
		Backend:       e.runtimeBackend(),
		RunID:         e.currentRunID,
		HandoffMode:   mode,
		Log:           e.log,
	})
}

// actionArbiterEscalate routes the "arbiter" flow to the executor's arbiter
// escalation logic. The v2 dispatchArbiter uses e.formula and e.state which
// are nil in v3 graph mode, so this builds a RevisionPolicy from the graph
// state vars and calls ReviewEscalateToArbiter directly.
func actionArbiterEscalate(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	sageName := fmt.Sprintf("%s-sage", e.agentName)

	// Build revision policy from graph vars and step config.
	arbiterModel := step.Model
	if arbiterModel == "" {
		arbiterModel = "claude-opus-4-7"
	}
	maxRounds := 3
	if mr, ok := state.Vars["max_review_rounds"]; ok {
		if v, err := fmt.Sscanf(mr, "%d", &maxRounds); v == 0 || err != nil {
			maxRounds = 3
		}
	}

	revPolicy := RevisionPolicy{
		MaxRounds:    maxRounds,
		ArbiterModel: arbiterModel,
	}

	lastReview := &Review{Verdict: "request_changes", Summary: "Max review rounds reached"}
	started := time.Now()
	err := e.deps.ReviewEscalateToArbiter(e.beadID, sageName, lastReview, revPolicy, e.log)

	reviewRound := state.Counters["review_round"]
	reviewAttempt := state.Steps[stepName].CompletedCount + 1
	e.recordAgentRun(sageName, e.beadID, "", arbiterModel, "arbiter", "review", started, err,
		withReviewStep("arbiter", reviewRound+1),
		withParentRun(e.currentRunID),
		withAttemptNumber(reviewAttempt))

	if err != nil {
		return ActionResult{Error: fmt.Errorf("arbiter escalation: %w", err)}
	}

	// Read arbiter decision from bead labels.
	decision := ""
	if bead, bErr := e.deps.GetBead(e.beadID); bErr == nil {
		decision = e.deps.HasLabel(bead, "arbiter-decision:")
		if decision == "" && e.deps.ContainsLabel(bead, "review-approved") {
			decision = "merge"
		}
	}

	return ActionResult{Outputs: map[string]string{
		"arbiter_decision": decision,
		"result":           "escalated",
	}}
}

// wizardRunSpawn is the common spawn logic for wizard.run actions.
//
// Per-attempt naming: the spawn identifier is <agentName>-<stepName>-<N>, used
// for BOTH the orchestrator .log filename and the spawned wizard's result
// directory. That single stem is what pairs the orchestrator log with its
// claude transcripts in the board inspector. Re-running the same step (e.g.
// sage-review rounds, resummon) bumps N and produces a distinct sibling tree
// rather than overwriting the previous attempt.
//
// Legacy on-disk layouts written before this naming change (orchestrator log
// suffixed with -N but result dir unsuffixed) remain readable as raw files but
// their claude transcripts won't be paired by the inspector — migration of old
// trees is intentionally out of scope.
//
// Defaults to HandoffBorrowed (same-owner continuations such as
// sage-review and recovery-verify, where no commits are produced and no
// delivery protocol is needed). Bundle-handoff dispatch sites call
// wizardRunSpawnWithHandoff directly with the resolved bundle mode; see
// commit_producing_apprentice.go for the Principle 1 dispatch path.
func wizardRunSpawn(e *Executor, stepName string, step StepConfig, state *GraphState, role agent.SpawnRole, extraArgs []string, workspace *WorkspaceHandle) ActionResult {
	// principle1-exception: this is the wrapper default for non-commit
	// flows (sage-review, recovery-verify). Commit-producing callers go
	// through wizardRunSpawnWithHandoff with the resolved bundle mode —
	// the dispatch sites in commit_producing_apprentice.go enforce that.
	return wizardRunSpawnWithHandoff(e, stepName, step, state, role, extraArgs, workspace, HandoffBorrowed)
}

// wizardRunSpawnWithHandoff is wizardRunSpawn parameterized by HandoffMode.
// Callers that produce git commits (review-fix, cleric-worker) MUST pass
// the resolved bundle handoff (e.resolveApprenticeHandoff()) so the runtime
// contract on the spawned process records bundle delivery — that is the
// enforcement surface for Principle 1.
//
// HandoffBorrowed is permitted only for non-commit-producing dispatch paths
// (sage-review reads the diff; recovery-verify runs narrow checks). The
// principle1 audit test enforces that the literal HandoffBorrowed never
// appears in fix-adjacent or cleric-worker call sites.
func wizardRunSpawnWithHandoff(e *Executor, stepName string, step StepConfig, state *GraphState, role agent.SpawnRole, extraArgs []string, workspace *WorkspaceHandle, handoffMode HandoffMode) ActionResult {
	attemptNum := state.Steps[stepName].CompletedCount + 1
	// Override the spawn-naming counter with a monotonic value derived from
	// preserved child beads. CompletedCount is reset alongside graph_state.json
	// on every reset, so without this override a post-reset log filename
	// collides with a pre-reset file (e.g. wizard-<id>-sage-review-1.log or
	// wizard-<id>-implement-1.log overwritten on cycle 2). The bead-derived
	// counters (round:N on review-round beads, attempt:N on attempt beads)
	// survive resets because those beads are closed, not deleted.
	switch step.Flow {
	case "sage-review":
		// Sage creates the review-round-N bead inside its spawn. At dispatch
		// time max(round) reflects the prior completed round, so the next
		// round is max+1 — matching the round the sage will stamp.
		if maxRound := store.MaxRoundNumber(e.beadID); maxRound > 0 {
			attemptNum = maxRound + 1
		}
	case "review-fix":
		// Fix runs after sage-review has completed and stamped review-round-N.
		// Reuse that N so fix-N.log sits alongside sage-review-N.log for the
		// same round and stays unique across resets.
		if maxRound := store.MaxRoundNumber(e.beadID); maxRound > 0 {
			attemptNum = maxRound
		}
	case "implement":
		// Implement is paired with the attempt bead the executor created at
		// run start. attempt:N is monotonic across resets because attempt
		// beads are closed (not deleted) on reset.
		if maxAttempt := store.MaxAttemptNumber(e.beadID); maxAttempt > 0 {
			attemptNum = maxAttempt
		}
	}
	spawnName := fmt.Sprintf("%s-%s-%d", e.agentName, stepName, attemptNum)

	// Timing is per-attempt: captured after attemptNum is resolved so each
	// review-fix retry (or any re-dispatch of the step) records its own
	// duration_seconds rather than accumulating across attempts.
	started := time.Now()

	cfg := agent.SpawnConfig{
		Name:         spawnName,
		BeadID:       e.beadID,
		Role:         role,
		Provider:     e.resolveStepProvider(step),
		Step:         stepName,
		ExtraArgs:    extraArgs,
		CustomPrompt: step.With["prompt"],
		LogPath:      filepath.Join(dolt.GlobalDir(), "wizards", spawnName+".log"),
	}
	cfg, err := e.withRuntimeContract(cfg, state.TowerName, state.RepoPath, state.BaseBranch, stepName, step.Workspace, workspace, handoffMode)
	if err != nil {
		return ActionResult{Error: fmt.Errorf("handoff selection for %s: %w", stepName, err)}
	}
	// Validate the workspace on disk right before the spawn — heals missing
	// paths, paused rebases/merges, stale locks, and detached HEADs in-process
	// so resume either proceeds cleanly or fails with an actionable error.
	stepBeadID := state.StepBeadIDs[stepName]
	if vErr := e.validateWorkspaceForDispatch(state.RepoPath, stepName, stepBeadID, cfg.Workspace); vErr != nil {
		return ActionResult{Error: fmt.Errorf("workspace validation failed for step %s: %w", stepName, vErr)}
	}
	handle, err := e.deps.Spawner.Spawn(cfg)
	if err != nil {
		return ActionResult{Error: fmt.Errorf("spawn %s: %w", stepName, err)}
	}

	waitErr := handle.Wait()

	// Read result.json for outputs. If the child wrote result.json, trust
	// its declared output as the node's terminal value — regardless of
	// waitErr. The child may exit non-zero after writing (e.g. signal
	// during cleanup) but its declared result is authoritative.
	outputs := make(map[string]string)
	hasResultJSON := false
	if ar := e.readAgentResult(spawnName); ar != nil {
		hasResultJSON = true
		outputs["result"] = ar.Result
		if ar.Branch != "" {
			outputs["branch"] = ar.Branch
		}
		if ar.Commit != "" {
			outputs["commit"] = ar.Commit
		}
	} else if waitErr != nil {
		outputs["result"] = "error"
	} else {
		outputs["result"] = "success"
	}

	model := step.Model
	e.recordAgentRun(spawnName, e.beadID, "", model, string(role), stepName, started, waitErr,
		withParentRun(e.currentRunID),
		withAttemptNumber(attemptNum))

	// Propagate child process failure as a node error only when no
	// result.json was written. If the child declared its output, trust
	// it mechanically — the executor does not reinterpret subprocess
	// results.
	if waitErr != nil && !hasResultJSON {
		return ActionResult{Outputs: outputs, Error: fmt.Errorf("subprocess %s exited: %w", stepName, waitErr)}
	}

	return ActionResult{Outputs: outputs}
}

// actionCheckDesignLinked extracts the design validation logic into a
// self-contained check that verifies design beads are linked and closed.
// When auto_create is "true", missing or unready design beads cause a Hooked
// result (graph parks) instead of a hard error.
func actionCheckDesignLinked(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	autoCreate := step.With["auto_create"] == "true"

	deps, err := e.deps.GetDepsWithMeta(e.beadID)
	if err != nil {
		return ActionResult{Error: fmt.Errorf("get deps: %w", err)}
	}

	// Find existing design bead via discovered-from dep.
	var designRef string
	var designOpen bool
	var designEmpty bool
	for _, dep := range deps {
		if dep.DependencyType != beads.DepDiscoveredFrom || dep.IssueType != "design" {
			continue
		}
		designRef = dep.ID
		if string(dep.Status) != "closed" {
			designOpen = true
			break
		}
		comments, _ := e.deps.GetComments(dep.ID)
		if len(comments) == 0 && dep.Description == "" {
			designEmpty = true
			break
		}
		// Design bead exists, closed, has content — success.
		return ActionResult{Outputs: map[string]string{"design_ref": designRef}}
	}

	// No design bead found.
	if designRef == "" {
		if !autoCreate {
			return ActionResult{Error: fmt.Errorf("no linked design bead found (discovered-from dep with type=design)")}
		}
		// Auto-create design bead.
		newID, err := e.deps.CreateBead(CreateOpts{
			Title:  "Design: " + e.beadID,
			Type:   e.deps.ParseIssueType("design"),
			Labels: []string{"needs-human"},
			Prefix: store.PrefixFromID(e.beadID),
		})
		if err != nil {
			return ActionResult{Error: fmt.Errorf("create design bead: %w", err)}
		}
		// Link via discovered-from dep (epic depends on design).
		if err := e.deps.AddDepTyped(e.beadID, newID, "discovered-from"); err != nil {
			e.log("warning: link design bead %s: %s", newID, err)
		}
		// Comment on both beads.
		e.deps.AddComment(e.beadID, fmt.Sprintf("Auto-created design bead %s — awaiting human design input.", newID))
		e.deps.AddComment(newID, fmt.Sprintf("Created automatically for epic %s. Please add design content and close when ready.", e.beadID))
		// Message archmage.
		e.sendArchmageMessage(fmt.Sprintf("Design bead %s created for epic %s — needs human design input.", newID, e.beadID))
		e.log("design-check: no linked design bead found — auto-created %s, waiting for human input", newID)
		return ActionResult{Hooked: true, Outputs: map[string]string{"design_ref": newID}}
	}

	// Design bead exists but is open or empty.
	if designOpen || designEmpty {
		if !autoCreate {
			if designOpen {
				return ActionResult{Error: fmt.Errorf("design bead %s is not closed", designRef)}
			}
			return ActionResult{Error: fmt.Errorf("design bead %s has no content", designRef)}
		}
		// Design bead exists but not ready — hook and wait.
		if designOpen {
			e.log("design-check: design bead %s is still open — waiting for it to be closed", designRef)
		} else {
			e.log("design-check: design bead %s is closed but has no content — waiting for design decisions", designRef)
		}
		return ActionResult{Hooked: true, Outputs: map[string]string{"design_ref": designRef}}
	}

	// Should not reach here.
	return ActionResult{Outputs: map[string]string{"design_ref": designRef}}
}

// actionHumanApprove implements the human.approve gate action.
//
// First run (CompletedCount == 0): posts a comment and returns Hooked so the
// graph parks. The step bead's hooked status (set by graph_interpreter) signals
// that approval is needed.
//
// Re-run after approval: CompletedCount > 0 means the hook was resolved,
// so return success to advance the graph.
func actionHumanApprove(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	if state.Steps[stepName].CompletedCount > 0 {
		// Hook was resolved — approved.
		return ActionResult{Outputs: map[string]string{"status": "approved"}}
	}

	// First run: post comment and park.
	title := step.Title
	if title == "" {
		title = stepName
	}
	e.deps.AddComment(e.beadID, fmt.Sprintf("Waiting for human approval before advancing past %s", title))
	return ActionResult{Hooked: true}
}

// sendArchmageMessage sends a message to the archmage about this bead.
// Uses the existing MessageArchmage pattern from executor_escalate.go.
func (e *Executor) sendArchmageMessage(msg string) {
	MessageArchmage(e.agentName, e.beadID, msg, e.deps)
}

// childHasSuccessfulAttempt returns true if childID has at least one closed
// attempt bead carrying a result:success label. Used by the close-cascade
// guard in actionBeadFinish to distinguish children whose code has landed
// from children that slipped through dispatch (no attempt bead at all) or
// failed mid-attempt. A missing or errored GetChildren is treated as
// "cannot confirm success" — conservative so the guard refuses to close
// rather than silently cascading.
func childHasSuccessfulAttempt(e *Executor, childID string) bool {
	if e == nil || e.deps == nil || e.deps.GetChildren == nil {
		return false
	}
	grandchildren, err := e.deps.GetChildren(childID)
	if err != nil {
		return false
	}
	for _, gc := range grandchildren {
		if !e.deps.IsAttemptBead(gc) {
			continue
		}
		if gc.Status != "closed" {
			continue
		}
		for _, l := range gc.Labels {
			if l == "result:success" {
				return true
			}
		}
	}
	return false
}

// commitAttributionRE matches Conventional-Commit subjects of the form
// "<type>(<bead-id>): ...", per CLAUDE.md. Capture group 1 is the bead ID.
// Kept permissive on the type token (alphanumerics) so new types added to
// the convention (e.g. "perf") do not silently slip past attribution.
var commitAttributionRE = regexp.MustCompile(`^[a-zA-Z]+\(([^)]+)\):`)

// commitAttributesBead returns true if subject is a Conventional-Commit
// header attributed to childID. Case-sensitive: bead IDs are lowercase by
// convention. Empty/malformed subjects return false.
func commitAttributesBead(subject, childID string) bool {
	if subject == "" || childID == "" {
		return false
	}
	m := commitAttributionRE.FindStringSubmatch(subject)
	if len(m) < 2 {
		return false
	}
	return m[1] == childID
}

// mergedCommitsAttributeBead reports whether any commit reachable from
// branchTip but not from base carries a "<type>(<childID>):" subject.
//
// Scoping: we list commits on base..branchTip only — typically the merged
// epic/feature branch's unique history since divergence from main. Bounded
// by the branch's own size, not the repo history, so the check is
// O(branch-commits) per call. When repoPath or branchTip is empty (unit-test
// paths) the function returns (false, nil) — the caller then falls back to
// the attempt-bead check.
//
// After a fast-forward merge, base and branchTip resolve to the same SHA;
// the base..branchTip range is then empty and would miss already-landed
// child commits. In that case we widen the scan to all commits reachable
// from branchTip and rely on the uniqueness of bead-ID attribution (random
// 6-char suffixes) to avoid false matches against unrelated history.
func mergedCommitsAttributeBead(repoPath, base, branchTip, childID string) (bool, error) {
	if repoPath == "" || branchTip == "" || childID == "" {
		return false, nil
	}
	rangeArg := branchTip
	if base != "" {
		rangeArg = base + ".." + branchTip
		if sameRef(repoPath, base, branchTip) {
			rangeArg = branchTip
		}
	}
	cmd := exec.Command("git", "-C", repoPath, "log", "--pretty=format:%s", rangeArg)
	out, err := cmd.Output()
	if err != nil {
		// Ref resolution failures (e.g. staging branch already pruned post-merge)
		// should not strand the epic — return false and let the caller fall back.
		return false, nil
	}
	for _, line := range strings.Split(string(out), "\n") {
		if commitAttributesBead(strings.TrimSpace(line), childID) {
			return true, nil
		}
	}
	return false, nil
}

// sameRef reports whether two git refs in repoPath resolve to the same
// commit SHA. Returns false if either ref fails to resolve — callers
// should treat that as "cannot determine equality" and keep their
// existing range.
func sameRef(repoPath, a, b string) bool {
	resolve := func(ref string) (string, bool) {
		cmd := exec.Command("git", "-C", repoPath, "rev-parse", "--verify", ref)
		out, err := cmd.Output()
		if err != nil {
			return "", false
		}
		return strings.TrimSpace(string(out)), true
	}
	shaA, okA := resolve(a)
	shaB, okB := resolve(b)
	if !okA || !okB {
		return false
	}
	return shaA == shaB
}

// childLandedOnBranch returns true if child has either a successful attempt
// bead or a commit on the merged branch attributed to its ID. This is the
// close-cascade guard's "landed" predicate. Wave apprentices implement
// multiple subtasks on one branch in one run, producing per-subtask commit
// attribution (feat(<child>): ...) but no per-subtask attempt beads. Commit
// attribution is a stronger proof of landing than an attempt bead.
func childLandedOnBranch(e *Executor, state *GraphState, childID string) bool {
	if childHasSuccessfulAttempt(e, childID) {
		return true
	}
	if state == nil {
		return false
	}
	ok, _ := mergedCommitsAttributeBead(state.RepoPath, state.BaseBranch, state.StagingBranch, childID)
	return ok
}

// actionBeadFinish closes the bead and sets executor to terminated.
// Reads With parameters:
//
//	status: "closed" | "done" | "wontfix" | "discard" (default: "closed")
//
// actionNoop is a no-op action that completes immediately with success.
// Used for terminal signal steps in nested graphs (e.g. subgraph-review merge/discard
// terminals) where the parent graph is responsible for the real side effects.
func actionNoop(_ *Executor, _ string, _ StepConfig, _ *GraphState) ActionResult {
	return ActionResult{Outputs: map[string]string{"status": "done"}}
}

//	outcome: alias for status (used by some formulas)
//
// For epic formulas, also closes orphan subtask beads.
//
// Records an agent_runs row with phase='close' on the successful close path
// and phase='discard' on the successful discard path so the bead-lifecycle
// terminal transitions are visible in the metrics surface.
func actionBeadFinish(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	started := time.Now()
	status := step.With["status"]
	if status == "" {
		status = step.With["outcome"]
	}

	switch status {
	case "closed", "done", "success", "resolved", "":
		// Close-cascade guard (defense in depth for spi-g4yi6j Fix A).
		//
		// Before cascading close to non-DAG children, verify each one either
		// is already closed (closed intentionally earlier, e.g. wontfix /
		// duplicate) or has a successful attempt bead (code landed). A child
		// that slipped through dispatch without an attempt would be force-
		// closed by the cascade below — silently stranding it against
		// SPIRE.md Principle 6 ("Beads close AFTER code lands on main").
		//
		// When stranded children are detected, label the epic needs-human
		// and message the archmage; refuse to close so the protocol gap
		// surfaces instead of going unnoticed.
		if children, childErr := e.deps.GetChildren(e.beadID); childErr == nil {
			var stranded []string
			for _, child := range children {
				if child.Status == "closed" {
					continue
				}
				if e.deps.IsAttemptBead(child) || e.deps.IsStepBead(child) || e.deps.IsReviewRoundBead(child) {
					continue
				}
				if !childLandedOnBranch(e, state, child.ID) {
					stranded = append(stranded, child.ID)
				}
			}
			if len(stranded) > 0 {
				if e.deps.AddLabel != nil {
					if lErr := e.deps.AddLabel(e.beadID, "needs-human"); lErr != nil {
						e.log("warning: add needs-human label to %s: %s", e.beadID, lErr)
					}
				}
				msg := fmt.Sprintf(
					"refuse close of %s: %d child(ren) stranded without landed code: %s",
					e.beadID, len(stranded), strings.Join(stranded, ", "))
				e.sendArchmageMessage(msg)
				e.log("%s", msg)
				return ActionResult{Error: fmt.Errorf("%s", msg)}
			}
		}

		// Close orphan subtask beads (for epic formulas).
		if children, childErr := e.deps.GetChildren(e.beadID); childErr == nil {
			for _, child := range children {
				if child.Status == "closed" {
					continue
				}
				if e.deps.IsAttemptBead(child) || e.deps.IsStepBead(child) || e.deps.IsReviewRoundBead(child) {
					continue
				}
				if err := e.deps.CloseBead(child.ID); err != nil {
					e.log("warning: close orphan subtask %s: %s", child.ID, err)
				}
			}
		}

		// Close the main bead.
		if err := e.deps.CloseBead(e.beadID); err != nil {
			return ActionResult{Error: fmt.Errorf("close bead: %w", err)}
		}

		// Close recovery + alert dependents. "related" is included for
		// backward compat with old alert beads that were linked via that
		// edge type before alerts.Raise standardised on "caused-by".
		if err := recovery.CloseRelatedDependents(executorBeadOps{e.deps}, e.beadID, []string{recovery.KindRecovery, recovery.KindAlert}, []string{"caused-by", "recovery-for", "related"}, "bead finished successfully"); err != nil {
			e.log("warning: close recovery beads: %v", err)
		}

		// Close the attempt bead.
		if state.AttemptBeadID != "" {
			if err := e.deps.CloseAttemptBead(state.AttemptBeadID, "success: "+stepName); err != nil {
				e.log("warning: close attempt bead: %s", err)
			}
			state.AttemptBeadID = ""
		}

		e.terminated = true
		e.recordAgentRun(e.agentName, e.beadID, "", step.Model, "wizard", "close", started, nil,
			withParentRun(e.currentRunID))
		return ActionResult{Outputs: map[string]string{"status": "closed"}}

	case "wontfix", "discard":
		if err := TerminalDiscard(e.beadID, e.deps, e.log); err != nil {
			return ActionResult{Error: fmt.Errorf("terminal discard: %w", err)}
		}
		e.terminated = true
		e.recordAgentRun(e.agentName, e.beadID, "", step.Model, "wizard", "discard", started, nil,
			withParentRun(e.currentRunID))
		return ActionResult{Outputs: map[string]string{"status": "discarded"}}

	case "escalate":
		EscalateGraphStepFailure(e.beadID, e.agentName, "bead-finish-escalate",
			"formula requested escalation", stepName, step.Action, step.Flow, step.Workspace, e.deps)
		return ActionResult{Outputs: map[string]string{"status": "escalated"}}

	default:
		return ActionResult{Error: fmt.Errorf("unknown bead.finish status %q", status)}
	}
}

// actionMergeToMain lands the staging branch onto the base branch.
// Reads With parameters:
//
//	strategy: "squash" | "merge" | "rebase" (default: "squash")
//	build: pre-merge build verification command (optional)
//	test: pre-merge test verification command (optional)
//	doc_patterns: comma-separated glob patterns for doc review (optional)
//
// Does NOT close beads — that's bead.finish.
//
// Records an agent_runs row with phase='merge' on every exit path that reaches
// the actual merge operation, so DORA deploy-count reflects successful merges
// (result='success') and failed-merge rounds (result='error'). Pre-merge errors
// that abort before the merge attempt itself (workspace resolution, build
// verification) do not produce a merge row — the merge never happened.
func actionMergeToMain(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	started := time.Now()

	// Resolve workspace and determine the actual branch being merged.
	// When a step declares a workspace, use that workspace's branch for merge
	// operations and cleanup. Fall back to state.StagingBranch for legacy paths.
	var stagingWt *spgit.StagingWorktree
	mergeBranch := state.StagingBranch // default for legacy/unresolved
	if step.Workspace != "" {
		dir, wsErr := e.resolveGraphWorkspace(step.Workspace, state)
		if wsErr != nil {
			return ActionResult{Error: fmt.Errorf("resolve workspace %q for merge: %w", step.Workspace, wsErr)}
		}
		ws := state.Workspaces[step.Workspace]
		stagingWt = spgit.ResumeStagingWorktree(state.RepoPath, dir, ws.Branch, ws.BaseBranch, e.log)
		mergeBranch = ws.Branch
	} else {
		var err error
		stagingWt, err = e.ensureGraphStagingWorktree(state)
		if err != nil {
			return ActionResult{Error: fmt.Errorf("ensure staging worktree for merge: %w", err)}
		}
	}

	buildStr := step.With["build"]
	testStr := step.With["test"]

	// Parse conflict_max_turns for the resolver turn budget.
	// When unset, conflictMaxTurns stays 0 and --max-turns is omitted,
	// letting Claude's own default govern.
	var conflictMaxTurns int
	if raw := step.With["conflict_max_turns"]; raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			conflictMaxTurns = v
		}
	}
	resolver := e.conflictResolver(conflictMaxTurns)

	// Pre-merge build verification.
	if buildStr != "" {
		e.log("verifying build before merge: %s", buildStr)
		if buildErr := stagingWt.RunBuild(buildStr); buildErr != nil {
			return ActionResult{Error: fmt.Errorf("pre-merge build verification failed: %w", buildErr)}
		}
	}

	// Doc review (optional).
	if docPatternsStr := step.With["doc_patterns"]; docPatternsStr != "" {
		patterns := strings.Split(docPatternsStr, ",")
		if docErr := e.reviewDocsForStaleness(stagingWt.Dir, mergeBranch, state.BaseBranch, patterns, step.Model); docErr != nil {
			e.log("warning: doc review: %s", docErr)
		}
	}

	// Merge workspace branch -> main.
	mergeEnv := os.Environ()
	if tower, tErr := e.deps.ActiveTowerConfig(); tErr == nil && tower != nil {
		mergeEnv = e.deps.ArchmageGitEnv(tower)
	}

	// workingStarted splits the startup (setup/validation) phase from the
	// working (actual merge + push) phase for the timing-bucket attribution.
	workingStarted := time.Now()

	e.log("merging %s -> %s", mergeBranch, state.BaseBranch)
	if mergeErr := stagingWt.MergeToMain(state.BaseBranch, mergeEnv, buildStr, testStr, resolver); mergeErr != nil {
		e.recordAgentRun(e.agentName, e.beadID, "", step.Model, "wizard", "merge", started, mergeErr,
			withParentRun(e.currentRunID),
			withStartupSeconds(workingStarted.Sub(started).Seconds()),
			withWorkingSeconds(time.Since(workingStarted).Seconds()))
		return ActionResult{Error: fmt.Errorf("merge to main: %w", mergeErr)}
	}

	// Push main.
	rc := &spgit.RepoContext{Dir: state.RepoPath, BaseBranch: state.BaseBranch, Log: e.log}
	if pushErr := rc.Push("origin", state.BaseBranch, mergeEnv); pushErr != nil {
		e.log("merge push failed: %v", pushErr)
		e.recordAgentRun(e.agentName, e.beadID, "", step.Model, "wizard", "merge", started, pushErr,
			withParentRun(e.currentRunID),
			withStartupSeconds(workingStarted.Sub(started).Seconds()),
			withWorkingSeconds(time.Since(workingStarted).Seconds()))
		return ActionResult{Error: fmt.Errorf("push %s: %w", state.BaseBranch, pushErr)}
	}

	// Clean up the actual branch that was merged (best-effort).
	rc.DeleteBranch(mergeBranch)
	rc.DeleteRemoteBranch("origin", mergeBranch)

	e.recordAgentRun(e.agentName, e.beadID, "", step.Model, "wizard", "merge", started, nil,
		withParentRun(e.currentRunID),
		withStartupSeconds(workingStarted.Sub(started).Seconds()),
		withWorkingSeconds(time.Since(workingStarted).Seconds()))

	return ActionResult{Outputs: map[string]string{"merged": "true"}}
}

// actionVerifyRun runs build and/or test commands in the declared workspace.
// Reads With parameters:
//
//	command: single command string (backward compat)
//	build: build command string (optional)
//	test: test command string (optional)
//
// Produces: status ("pass"/"fail"), error_log (error message if failed)
func actionVerifyRun(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	// Support both "command" (backward compat) and separate "build"/"test" params.
	buildCmd := step.With["build"]
	testCmd := step.With["test"]
	if buildCmd == "" && testCmd == "" {
		buildCmd = step.With["command"]
	}
	// Fall back to spire.yaml runtime config. The embedded formulas intentionally
	// omit build/test commands — spire.yaml is the source of truth for how a repo
	// builds and tests.
	if buildCmd == "" && testCmd == "" {
		if rc := e.deps.RepoConfig(); rc != nil {
			buildCmd = rc.Runtime.Build
			testCmd = rc.Runtime.Test
		}
	}
	if buildCmd == "" && testCmd == "" {
		return ActionResult{Outputs: map[string]string{"status": "pass", "result": "skipped"}}
	}

	// Resolve workspace: prefer step-declared workspace, fall back to staging worktree.
	var stagingWt *spgit.StagingWorktree
	if step.Workspace != "" {
		dir, wsErr := e.resolveGraphWorkspace(step.Workspace, state)
		if wsErr != nil {
			return ActionResult{Error: fmt.Errorf("resolve workspace %q for verify: %w", step.Workspace, wsErr)}
		}
		stagingWt = spgit.ResumeStagingWorktree(state.RepoPath, dir,
			state.Workspaces[step.Workspace].Branch, state.Workspaces[step.Workspace].BaseBranch, e.log)
	} else {
		var err error
		stagingWt, err = e.ensureGraphStagingWorktree(state)
		if err != nil {
			return ActionResult{Error: fmt.Errorf("ensure staging worktree: %w", err)}
		}
	}

	// Run install command (e.g. pnpm install) before build/test.
	// Staging worktrees are fresh checkouts with no node_modules.
	installCmd := step.With["install"]
	if installCmd == "" {
		if rc := e.deps.RepoConfig(); rc != nil {
			installCmd = rc.Runtime.Install
		}
	}
	if installCmd != "" {
		if installErr := stagingWt.RunBuild(installCmd); installErr != nil {
			return ActionResult{
				Outputs: map[string]string{
					"status":    "fail",
					"result":    "failed",
					"error_log": "install failed: " + installErr.Error(),
				},
			}
		}
	}

	// Run build command.
	if buildCmd != "" {
		if buildErr := stagingWt.RunBuild(buildCmd); buildErr != nil {
			return ActionResult{
				Outputs: map[string]string{
					"status":    "fail",
					"result":    "failed",
					"error_log": buildErr.Error(),
				},
			}
		}
	}

	// Run test command.
	if testCmd != "" {
		if testErr := stagingWt.RunBuild(testCmd); testErr != nil {
			return ActionResult{
				Outputs: map[string]string{
					"status":    "fail",
					"result":    "failed",
					"error_log": testErr.Error(),
				},
			}
		}
	}

	return ActionResult{Outputs: map[string]string{"status": "pass", "result": "passed"}}
}

// actionDispatchChildren is implemented in action_dispatch.go.

// actionMaterializePlan verifies that child beads were created by the
// preceding wizard plan step (epic-plan or task-plan). In the v2 flow, the
// wizard's epic-plan behavior BOTH generates the plan AND creates child beads
// in a single step. The materialize action confirms children exist and reports
// the count — it does not re-create them.
//
// This step exists to make the dependency between planning and dispatch
// explicit in the graph: dispatch.children needs children to exist, and
// this step gates that with a clear error if planning failed to produce any.
func actionMaterializePlan(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	allChildren, err := e.deps.GetChildren(e.beadID)
	if err != nil {
		return ActionResult{Error: fmt.Errorf("get children for %s: %w", e.beadID, err)}
	}

	// Filter out internal DAG beads (step, attempt, review-round) that
	// are created by ensureStepBeads/ensureAttemptBead, not by planning.
	var realChildren []Bead
	for _, c := range allChildren {
		if e.deps.IsAttemptBead(c) || e.deps.IsStepBead(c) || e.deps.IsReviewRoundBead(c) {
			continue
		}
		realChildren = append(realChildren, c)
	}

	if len(realChildren) == 0 {
		return ActionResult{Error: fmt.Errorf("no subtask beads found for %s — plan step may have failed to create children", e.beadID)}
	}

	e.log("materialize: found %d subtask(s) for %s", len(realChildren), e.beadID)

	outputs := map[string]string{
		"status":      "pass",
		"child_count": fmt.Sprintf("%d", len(realChildren)),
	}
	return ActionResult{Outputs: outputs}
}

// actionGraphRun executes a nested step-graph formula as a sub-graph within
// the current executor. It loads the named graph, runs the interpreter loop
// inline (without the deferred cleanup that RunGraph applies), and captures
// the terminal step's outputs for the parent graph to route on.
//
// Key design decisions:
//   - Uses RunNestedGraph (not RunGraph) to avoid deferred cleanup of the
//     parent's staging worktree, registry entry, and graph state file.
//   - The nested graph gets its own GraphState with a derived agent name
//     (parent-stepName) so state files don't collide.
//   - Parent vars are copied into the sub-state so subgraph-review can access
//     max_review_rounds, branch, etc.
//   - The terminal step name from the sub-graph becomes the "outcome" output,
//     which parent steps route on (e.g. steps.review.outputs.outcome == "merge").
func actionGraphRun(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	graphName := step.Graph
	if graphName == "" {
		graphName = step.With["graph"]
	}
	if graphName == "" {
		return ActionResult{Error: fmt.Errorf("graph.run: no graph name specified")}
	}

	// Load the nested graph formula.
	subGraph, err := formula.LoadStepGraphByName(graphName)
	if err != nil {
		return ActionResult{Error: fmt.Errorf("load nested graph %q: %w", graphName, err)}
	}

	// Load or create sub-state for the nested graph.
	// On resume, the sub-state may already be persisted from a prior interrupted run.
	subAgentName := e.agentName + "-" + stepName
	subState, loadErr := LoadGraphState(subAgentName, e.deps.ConfigDir)
	if loadErr != nil {
		e.log("warning: load nested graph state for %s: %s (starting fresh)", subAgentName, loadErr)
	}
	if subState == nil {
		subState = NewGraphState(subGraph, e.beadID, subAgentName)
		subState.TowerName = state.TowerName // inherit tower scope from parent

		// Copy parent vars into sub-state (e.g. max_review_rounds, base_branch).
		for k, v := range state.Vars {
			subState.Vars[k] = v
		}

		// Copy branch/workspace info so the sub-graph can resolve the same
		// integration surface the parent step is already using.
		subState.RepoPath = state.RepoPath
		subState.BaseBranch = state.BaseBranch
		subState.StagingBranch = state.StagingBranch
		subState.WorktreeDir = state.WorktreeDir
		if err := e.propagateGraphRunWorkspace(step, state, subState); err != nil {
			return ActionResult{Error: fmt.Errorf("resolve workspace %q for graph.run %s: %w", step.Workspace, stepName, err)}
		}
	} else {
		e.log("resuming nested graph %s from persisted state (active: %s)", subAgentName, subState.ActiveStep)

		// Always refresh branch and workspace info from the parent state.
		subState.RepoPath = state.RepoPath
		subState.BaseBranch = state.BaseBranch
		subState.StagingBranch = state.StagingBranch
		subState.WorktreeDir = state.WorktreeDir
		if err := e.propagateGraphRunWorkspace(step, state, subState); err != nil {
			e.log("warning: resolve workspace %q on resume: %s", step.Workspace, err)
		}
	}

	// Run the nested graph using the isolated interpreter (saves after each step).
	runErr := e.RunNestedGraph(subGraph, subState)

	// Capture outputs from the terminal step that completed.
	outputs := make(map[string]string)
	for name, ss := range subState.Steps {
		if ss.Status == "completed" && formula.IsTerminal(subGraph, name) {
			outputs["outcome"] = name
			for k, v := range ss.Outputs {
				outputs[k] = v
			}
			break
		}
	}

	if runErr != nil {
		// Sub-state was saved after each step by RunNestedGraph.
		// On interrupt/failure it persists for resume.
		return ActionResult{Outputs: outputs, Error: runErr}
	}

	// Do NOT remove nested state here — the parent has not yet durably saved
	// this step as completed. If the process dies between here and the parent
	// save, the nested progress would be lost. The parent interpreter cleans
	// up nested state files after its own save (crash-safe ordering).

	return ActionResult{Outputs: outputs}
}

func (e *Executor) propagateGraphRunWorkspace(step StepConfig, parentState, subState *GraphState) error {
	if step.Workspace == "" {
		return nil
	}

	ws, ok := parentState.Workspaces[step.Workspace]
	if !ok || ws.Dir == "" {
		dir, err := e.resolveGraphWorkspace(step.Workspace, parentState)
		if err != nil {
			return err
		}
		ws = parentState.Workspaces[step.Workspace]
		if ws.Dir == "" {
			ws.Dir = dir
		}
	}

	subState.WorktreeDir = ws.Dir
	if subState.Workspaces == nil {
		subState.Workspaces = make(map[string]WorkspaceState)
	}
	subState.Workspaces[step.Workspace] = ws

	if step.Workspace == "staging" || ws.Kind == formula.WorkspaceKindStaging {
		if ws.Branch != "" {
			subState.StagingBranch = ws.Branch
		}
		if ws.BaseBranch != "" {
			subState.BaseBranch = ws.BaseBranch
		}
	}

	return nil
}
