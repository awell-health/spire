package executor

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/dolt"
	spgit "github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/awell-health/spire/pkg/store"
)

func init() {
	actionRegistry["cleric.execute"] = actionClericExecute
	actionRegistry["cleric.decide"] = handleDecide
	actionRegistry["cleric.learn"] = actionClericLearn
	actionRegistry["cleric.collect_context"] = actionClericCollectContext
	actionRegistry["cleric.verify"] = actionClericVerify
}

// DefaultMaxRecoveryAttempts is the number of recovery attempts before
// automatic escalation. Can be overridden via step.With["max_attempts"].
const DefaultMaxRecoveryAttempts = 3

// DefaultVerifyPollInterval is the polling interval for the cooperative
// verify loop (in seconds).
const DefaultVerifyPollInterval = 30

// DefaultVerifyTimeout is the maximum time to wait for a retry result
// (in seconds).
const DefaultVerifyTimeout = 600 // 10 minutes

// maxStepLoopCount is the safety valve for loop_to directives in the graph
// interpreter. If a step completes more than this many times, the loop is
// broken and the failure is escalated. The recovery decide step already
// escalates at max_attempts=3, so this is a secondary safety net.
const maxStepLoopCount = 5

// actionClericExecute is the ActionHandler for the "cleric.execute" opcode.
// It bridges formula step dispatch to the recovery action vocabulary.
//
// Reads With parameters:
//
//	action:          one of the RecoveryActionKind values (required)
//	source_bead_id:  bead being recovered (optional; falls back to recovery bead metadata)
//	step_target:     target step name for reset-to-step (optional)
//	resolution_kind: for annotate-resolution
//	learning_key:    for annotate-resolution
//	reusable:        "true"/"false" for annotate-resolution
//	reason:          for escalate
//
// Any additional With parameters are passed through as Params.
func actionClericExecute(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	actionKind := step.With["action"]
	if actionKind == "" {
		return ActionResult{Error: fmt.Errorf("cleric.execute step %q missing required with.action", stepName)}
	}

	// Agentic recovery steps: handle directly with full graph context.
	switch actionKind {
	case "collect_context":
		return handleCollectContext(e, stepName, step, state)
	case "decide":
		return handleDecide(e, stepName, step, state)
	case "execute":
		// Dispatch on the typed RepairPlan produced by decide. The plan is the
		// sole execute contract — no legacy action-string fallback.
		if state != nil {
			if ds, ok := state.Steps["decide"]; ok {
				if planJSON := ds.Outputs["plan"]; planJSON != "" {
					var plan recovery.RepairPlan
					if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
						return ActionResult{Error: fmt.Errorf("recovery execute: unmarshal decide plan: %w", err)}
					}
					return handlePlanExecute(e, stepName, step, state, plan)
				}
			}
		}
		return ActionResult{Error: fmt.Errorf("recovery execute: no plan from decide step")}
	case "verify":
		return handleVerify(e, stepName, step, state)
	case "learn":
		return handleLearn(e, stepName, step, state)
	case "finish":
		return handleFinish(e, stepName, step, state)
	case "record_error":
		return handleRecordExecuteError(e, stepName, step, state)
	}

	// Resolve source bead ID: prefer explicit param, fall back to recovery bead metadata.
	sourceBeadID := step.With["source_bead_id"]
	if sourceBeadID == "" {
		if bead, err := e.deps.GetBead(e.beadID); err == nil {
			sourceBeadID = bead.Meta(recovery.KeySourceBead)
		}
	}

	// For triage actions, inject test output and wizard log tail from
	// collect_context step outputs into params so doTriage can use them.
	params := step.With
	if actionKind == "triage" && state != nil {
		// Copy params to avoid mutating step.With.
		params = make(map[string]string, len(step.With))
		for k, v := range step.With {
			params[k] = v
		}
		if cs, ok := state.Steps["collect_context"]; ok {
			// Parse collect_context_result JSON to extract WizardLogTail.
			if ccJSON := cs.Outputs["collect_context_result"]; ccJSON != "" {
				var cc CollectContextResult
				if err := json.Unmarshal([]byte(ccJSON), &cc); err == nil {
					if cc.WizardLogTail != "" {
						params["wizard_log_tail"] = cc.WizardLogTail
						// Use wizard log tail as test output if no dedicated test_output.
						if params["test_output"] == "" {
							params["test_output"] = cc.WizardLogTail
						}
					}
				}
			}
		}
	}

	req := recovery.RecoveryActionRequest{
		Kind:         recovery.RecoveryActionKind(actionKind),
		BeadID:       e.beadID,
		SourceBeadID: sourceBeadID,
		StepTarget:   step.With["step_target"],
		Params:       params,
	}

	result := ExecuteRecoveryAction(e, req)

	// Map RecoveryActionResult to ActionResult.
	outputs := map[string]string{
		"status": "success",
		"action": actionKind,
	}
	if result.ResolutionKind != "" {
		outputs["resolution_kind"] = result.ResolutionKind
	}
	if result.VerificationStatus != "" {
		outputs["verification_status"] = result.VerificationStatus
	}
	if result.Output != "" {
		outputs["output"] = result.Output
	}

	if !result.Success {
		outputs["status"] = "failed"
		return ActionResult{
			Outputs: outputs,
			Error:   fmt.Errorf("recovery action %q failed: %s", actionKind, result.Error),
		}
	}

	// Merge result metadata into the recovery bead's issue metadata.
	if len(result.Metadata) > 0 {
		if err := store.SetBeadMetadataMap(req.BeadID, result.Metadata); err != nil {
			e.log("warning: merge recovery metadata after %q: %s", actionKind, err)
		}
	}

	return ActionResult{Outputs: outputs}
}

// ExecuteRecoveryAction is the pure mechanical dispatcher for recovery actions.
// It validates the action kind against the bounded vocabulary and delegates to
// the appropriate handler. ZFC-compliant: no reasoning, only execution of the
// named opcode.
func ExecuteRecoveryAction(e *Executor, req recovery.RecoveryActionRequest) recovery.RecoveryActionResult {
	if !recovery.KnownActions[req.Kind] {
		return recovery.RecoveryActionResult{
			Kind:    req.Kind,
			Success: false,
			Error:   fmt.Sprintf("unknown recovery action kind: %q", req.Kind),
		}
	}

	switch req.Kind {
	case recovery.ActionReset:
		// Legacy "reset" action is now equivalent to "resummon" (soft relabel only).
		return doResummon(e, req)
	case recovery.ActionResetHard:
		return doResetHard(e, req)
	case recovery.ActionResetToStep:
		return doResetToStep(e, req)
	case recovery.ActionResummon:
		return doResummon(e, req)
	case recovery.ActionVerifyClean:
		return doVerifyClean(e, req)
	case recovery.ActionAnnotateResolution:
		return doAnnotateResolution(e, req)
	case recovery.ActionEscalate:
		return doEscalate(e, req)
	case recovery.ActionTriage:
		return doTriage(e, req)
	default:
		return recovery.RecoveryActionResult{
			Kind:    req.Kind,
			Success: false,
			Error:   fmt.Sprintf("unhandled recovery action kind: %q", req.Kind),
		}
	}
}

// doResetHard performs a full destructive reset on the source bead via the
// injected HardResetBead callback: kills wizard process, deletes worktree,
// branches, graph state, internal DAG beads, strips labels, and sets the bead
// to open. The callback lives in cmd/spire because it needs registry and git
// access that pkg/executor cannot import.
func doResetHard(e *Executor, req recovery.RecoveryActionRequest) recovery.RecoveryActionResult {
	if req.SourceBeadID == "" {
		return failResult(req.Kind, "source_bead_id is required for reset-hard")
	}
	if e.deps.HardResetBead == nil {
		return failResult(req.Kind, "HardResetBead dep not wired")
	}
	if err := e.deps.HardResetBead(req.SourceBeadID); err != nil {
		return failResult(req.Kind, fmt.Sprintf("hard reset %s: %v", req.SourceBeadID, err))
	}

	e.log("recovery: reset-hard %s", req.SourceBeadID)

	return recovery.RecoveryActionResult{
		Kind:           req.Kind,
		Success:        true,
		Output:         fmt.Sprintf("hard reset %s: worktree, branches, graph state, internal beads deleted", req.SourceBeadID),
		ResolutionKind: "reset-hard",
		Metadata: map[string]string{
			recovery.KeyResolutionKind: "reset-hard",
		},
	}
}

// doResetToStep performs a soft rewind: clears interrupt labels on the source
// bead and sets it back to in_progress so it can resume from the target step.
// The graph-state-level rewind (resetting step states to pending) is performed
// by the source bead's executor on resume via formula-declared resets.
func doResetToStep(e *Executor, req recovery.RecoveryActionRequest) recovery.RecoveryActionResult {
	if req.SourceBeadID == "" {
		return failResult(req.Kind, "source_bead_id is required for reset-to-step")
	}
	if req.StepTarget == "" {
		return failResult(req.Kind, "step_target is required for reset-to-step")
	}

	if _, err := e.deps.GetBead(req.SourceBeadID); err != nil {
		return failResult(req.Kind, fmt.Sprintf("get source bead: %v", err))
	}

	// Set source bead to in_progress (resuming, not restarting).
	if err := e.deps.UpdateBead(req.SourceBeadID, map[string]interface{}{"status": "in_progress"}); err != nil {
		return failResult(req.Kind, fmt.Sprintf("update source bead status: %v", err))
	}

	e.log("recovery: reset-to-step %s target=%s", req.SourceBeadID, req.StepTarget)

	return recovery.RecoveryActionResult{
		Kind:           req.Kind,
		Success:        true,
		Output:         fmt.Sprintf("reset %s to step %s", req.SourceBeadID, req.StepTarget),
		ResolutionKind: "reset-to-step",
		Metadata: map[string]string{
			recovery.KeyResolutionKind: "reset-to-step",
			"step_target":              req.StepTarget,
		},
	}
}

// doResummon clears interrupt state on the source bead and sets it back to open
// so a fresh agent can be summoned without wiping history.
func doResummon(e *Executor, req recovery.RecoveryActionRequest) recovery.RecoveryActionResult {
	if req.SourceBeadID == "" {
		return failResult(req.Kind, "source_bead_id is required for resummon")
	}

	if _, err := e.deps.GetBead(req.SourceBeadID); err != nil {
		return failResult(req.Kind, fmt.Sprintf("get source bead: %v", err))
	}

	// Set bead back to open for re-assignment.
	if err := e.deps.UpdateBead(req.SourceBeadID, map[string]interface{}{"status": "open"}); err != nil {
		return failResult(req.Kind, fmt.Sprintf("update source bead status: %v", err))
	}

	e.log("recovery: resummon %s (set to open for re-assignment)", req.SourceBeadID)

	return recovery.RecoveryActionResult{
		Kind:           req.Kind,
		Success:        true,
		Output:         fmt.Sprintf("resummon %s: set to open for re-assignment", req.SourceBeadID),
		ResolutionKind: "resummon",
		Metadata: map[string]string{
			recovery.KeyResolutionKind: "resummon",
		},
	}
}

// doVerifyClean checks bead health for the source bead: whether the bead is
// still active (not closed) and its status is healthy.
// Returns VerificationStatus = "clean" or "dirty".
func doVerifyClean(e *Executor, req recovery.RecoveryActionRequest) recovery.RecoveryActionResult {
	targetID := req.SourceBeadID
	if targetID == "" {
		targetID = req.BeadID
	}

	bead, err := e.deps.GetBead(targetID)
	if err != nil {
		return failResult(req.Kind, fmt.Sprintf("get bead %s: %v", targetID, err))
	}

	var issues []string

	// Check bead status.
	if bead.Status == "closed" {
		issues = append(issues, "bead is closed")
	}
	if bead.Status == "hooked" {
		issues = append(issues, "bead is still hooked")
	}

	status := "clean"
	if len(issues) > 0 {
		status = "dirty"
	}

	e.log("recovery: verify-clean %s: %s (%d issues)", targetID, status, len(issues))

	return recovery.RecoveryActionResult{
		Kind:               req.Kind,
		Success:            true,
		Output:             strings.Join(issues, "; "),
		VerificationStatus: status,
		Metadata: map[string]string{
			recovery.KeyVerificationStatus: status,
		},
	}
}

// doAnnotateResolution writes the resolution summary and learning data to the
// recovery bead's issue metadata. This is the document-phase opcode.
//
// Reads from req.Params:
//
//	resolution_kind:     how the issue was resolved
//	learning_key:        short key for future lookup
//	reusable:            "true"/"false" — whether this learning applies to future incidents
//	verification_status: outcome of verification
func doAnnotateResolution(e *Executor, req recovery.RecoveryActionRequest) recovery.RecoveryActionResult {
	meta := map[string]string{
		recovery.KeyResolvedAt: time.Now().UTC().Format(time.RFC3339),
	}

	if v := req.Params["resolution_kind"]; v != "" {
		meta[recovery.KeyResolutionKind] = v
	}
	if v := req.Params["learning_key"]; v != "" {
		meta[recovery.KeyLearningKey] = v
	}
	if v := req.Params["reusable"]; v != "" {
		meta[recovery.KeyReusable] = v
	}
	if v := req.Params["verification_status"]; v != "" {
		meta[recovery.KeyVerificationStatus] = v
	}
	if v := req.Params["learning_summary"]; v != "" {
		meta[recovery.KeyLearningSummary] = v
	}

	e.log("recovery: annotate-resolution on %s (%d fields)", req.BeadID, len(meta))

	return recovery.RecoveryActionResult{
		Kind:           req.Kind,
		Success:        true,
		Output:         fmt.Sprintf("annotated %d metadata fields on %s", len(meta), req.BeadID),
		ResolutionKind: req.Params["resolution_kind"],
		Metadata:       meta,
	}
}

// doEscalate marks the recovery bead as requiring human intervention. Writes
// an escalation comment and does NOT close the bead.
func doEscalate(e *Executor, req recovery.RecoveryActionRequest) recovery.RecoveryActionResult {
	// Write escalation context as a comment.
	reason := req.Params["reason"]
	if reason == "" {
		reason = "recovery action escalated"
	}
	_ = e.deps.AddComment(req.BeadID, fmt.Sprintf("Escalated: %s", reason))

	e.log("recovery: escalate %s — %s", req.BeadID, reason)

	return recovery.RecoveryActionResult{
		Kind:           req.Kind,
		Success:        true,
		Output:         fmt.Sprintf("escalated: %s", reason),
		ResolutionKind: "escalate",
		Metadata: map[string]string{
			recovery.KeyResolutionKind: "escalate",
		},
	}
}

// doTriage dispatches a triage agent into the failing worktree to diagnose and
// fix the issue. The agent gets test failure output and a focused prompt. Max 2
// triage attempts per recovery bead; after that, returns failure so the caller
// can escalate.
func doTriage(e *Executor, req recovery.RecoveryActionRequest) recovery.RecoveryActionResult {
	if req.SourceBeadID == "" {
		return failResult(req.Kind, "source_bead_id is required for triage")
	}

	// Read triage count from recovery bead metadata; enforce budget of 2.
	triageCount := 0
	if bead, err := e.deps.GetBead(req.BeadID); err == nil {
		if tc := bead.Meta(recovery.KeyTriageCount); tc != "" {
			triageCount, _ = strconv.Atoi(tc)
		}
	}
	if triageCount >= 2 {
		return failResult(req.Kind, "triage budget exhausted (max 2 attempts); escalate instead")
	}

	// Derive the wizard name from the source bead to load its graph state.
	wizardName := resolveWizardName(req.SourceBeadID, e.deps)

	// Load the source bead's graph state to find the worktree path.
	var worktreeDir string
	var repoPath string
	var baseBranch string
	var towerName string
	var workspace *WorkspaceHandle
	gs, err := LoadGraphState(wizardName, e.deps.ConfigDir)
	if err == nil && gs != nil {
		repoPath = gs.RepoPath
		baseBranch = gs.BaseBranch
		towerName = gs.TowerName
		// Prefer "feature" workspace, fall back to first workspace with a Dir.
		if ws, ok := gs.Workspaces["feature"]; ok && ws.Dir != "" {
			worktreeDir = ws.Dir
			handle := ws.Handle()
			workspace = &handle
		} else {
			for name, ws := range gs.Workspaces {
				if ws.Dir != "" {
					worktreeDir = ws.Dir
					handle := ws.Handle()
					if handle.Name == "" {
						handle.Name = name
					}
					workspace = &handle
					break
				}
			}
		}
		// Fall back to top-level WorktreeDir.
		if worktreeDir == "" {
			worktreeDir = gs.WorktreeDir
			workspace = inferWorkspaceHandle(gs.RepoPath, gs.WorktreeDir, gs.StagingBranch, gs.BaseBranch)
		}
	}

	// Fall back: if no worktree dir from graph state, check for a feat-branch:
	// label on the source bead and create a worktree from that branch.
	// Graph state is absent in this path, so resolve the repo directory from
	// the bead's registered prefix via ResolveRepo.
	if worktreeDir == "" {
		sourceBead, berr := e.deps.GetBead(req.SourceBeadID)
		if berr == nil {
			branchLabel := store.HasLabel(sourceBead, "feat-branch:")
			if branchLabel != "" {
				// Resolve the source repo path from the bead's prefix.
				// gs.RepoPath is always empty here (no graph state), so we
				// must resolve from tower config.
				repoDir, _, resolvedBaseBranch, resolveErr := e.deps.ResolveRepo(req.SourceBeadID)
				if resolveErr != nil || repoDir == "" {
					return failResult(req.Kind, fmt.Sprintf("cannot resolve repo for bead %s: %v", req.SourceBeadID, resolveErr))
				}
				repoPath = repoDir
				baseBranch = resolvedBaseBranch

				// Verify branch exists
				checkCmd := exec.Command("git", "rev-parse", "--verify", branchLabel)
				checkCmd.Dir = repoDir
				if checkCmd.Run() == nil {
					// Create a worktree from the branch
					wtDir := filepath.Join(os.TempDir(), "spire-triage", req.SourceBeadID)
					os.MkdirAll(filepath.Dir(wtDir), 0755)
					addCmd := exec.Command("git", "worktree", "add", wtDir, branchLabel)
					addCmd.Dir = repoDir
					if addErr := addCmd.Run(); addErr == nil {
						worktreeDir = wtDir
						workspace = inferWorkspaceHandle(repoDir, wtDir, branchLabel, resolvedBaseBranch)
						e.log("triage: created worktree from branch %s at %s", branchLabel, wtDir)
						// Clean up worktree when triage completes.
						defer func() {
							rmCmd := exec.Command("git", "worktree", "remove", "--force", wtDir)
							rmCmd.Dir = repoDir
							rmCmd.Run()
							e.log("triage: cleaned up worktree %s", wtDir)
						}()
					}
				}
			}
		}
	}

	if worktreeDir == "" {
		return failResult(req.Kind, "cannot determine worktree directory — no graph state, no feat-branch label")
	}

	// Verify the worktree still exists on disk.
	if _, err := os.Stat(worktreeDir); os.IsNotExist(err) {
		return failResult(req.Kind, fmt.Sprintf("worktree no longer exists: %s", worktreeDir))
	}
	if workspace == nil {
		workspace = inferWorkspaceHandle(repoPath, worktreeDir, "", baseBranch)
	}

	// Read test output from params (injected by the execute step from collect_context).
	testOutput := req.Params["test_output"]
	wizardLogTail := req.Params["wizard_log_tail"]

	// Build a focused custom prompt for the triage agent.
	var prompt strings.Builder
	prompt.WriteString("You are a triage agent. Your job is to diagnose and fix a failing test or build issue.\n\n")
	prompt.WriteString("## Rules\n")
	prompt.WriteString("- Fix the failing code so tests/build pass. Do NOT redesign or restructure.\n")
	prompt.WriteString("- Run the validation commands to verify your fix before committing.\n")
	prompt.WriteString("- Commit your fix with a descriptive message.\n")
	prompt.WriteString("- Do NOT create PRs, push, or touch other branches.\n\n")

	prompt.WriteString(fmt.Sprintf("## Worktree\n%s\n\n", worktreeDir))

	if testOutput != "" {
		prompt.WriteString("## Test/Build Failure Output\n```\n")
		prompt.WriteString(testOutput)
		if !strings.HasSuffix(testOutput, "\n") {
			prompt.WriteString("\n")
		}
		prompt.WriteString("```\n\n")
	}

	if wizardLogTail != "" {
		prompt.WriteString("## Wizard Log Tail\n```\n")
		prompt.WriteString(wizardLogTail)
		if !strings.HasSuffix(wizardLogTail, "\n") {
			prompt.WriteString("\n")
		}
		prompt.WriteString("```\n\n")
	}

	// Include build/test commands from repo config if available.
	if e.deps.RepoConfig != nil {
		if rc := e.deps.RepoConfig(); rc != nil {
			prompt.WriteString("## Validation Commands\n")
			if rc.Runtime.Build != "" {
				prompt.WriteString(fmt.Sprintf("- Build: `%s`\n", rc.Runtime.Build))
			}
			if rc.Runtime.Test != "" {
				prompt.WriteString(fmt.Sprintf("- Test: `%s`\n", rc.Runtime.Test))
			}
			if rc.Runtime.Lint != "" {
				prompt.WriteString(fmt.Sprintf("- Lint: `%s`\n", rc.Runtime.Lint))
			}
			prompt.WriteString("\n")
		}
	}

	// Spawn the triage agent as an apprentice into the worktree.
	spawnName := fmt.Sprintf("triage-%s-%d", req.SourceBeadID, triageCount+1)
	started := time.Now()

	cfg := agent.SpawnConfig{
		Name:         spawnName,
		BeadID:       req.SourceBeadID,
		Role:         agent.RoleApprentice,
		ExtraArgs:    []string{"--worktree-dir", worktreeDir, "--no-review"},
		CustomPrompt: prompt.String(),
		LogPath:      filepath.Join(dolt.GlobalDir(), "wizards", spawnName+".log"),
	}
	// Triage runs inside the same bead's recovery flow — the apprentice
	// reuses the worktree the cleric provisioned and produces no
	// cross-owner artifact.
	cfg, contractErr := e.withRuntimeContract(cfg, towerName, repoPath, baseBranch, "triage", "triage", workspace, HandoffBorrowed)
	if contractErr != nil {
		return failResult(req.Kind, fmt.Sprintf("triage handoff selection: %v", contractErr))
	}
	handle, spawnErr := e.deps.Spawner.Spawn(cfg)
	if spawnErr != nil {
		return failResult(req.Kind, fmt.Sprintf("spawn triage agent: %v", spawnErr))
	}

	waitErr := handle.Wait()

	// Read result.json from the triage agent.
	var agentResult string
	if ar := e.readAgentResult(spawnName); ar != nil {
		agentResult = ar.Result
	} else if waitErr != nil {
		agentResult = "error"
	} else {
		agentResult = "success"
	}

	// Record the agent run.
	e.recordAgentRun(spawnName, req.SourceBeadID, "", "", string(agent.RoleApprentice), "triage", started, waitErr,
		withParentRun(e.currentRunID))

	// Increment triage count on recovery bead metadata.
	newCount := strconv.Itoa(triageCount + 1)
	if e.deps.SetBeadMetadata != nil {
		if err := e.deps.SetBeadMetadata(req.BeadID, map[string]string{
			recovery.KeyTriageCount: newCount,
		}); err != nil {
			e.log("warning: failed to persist triage count on %s: %v", req.BeadID, err)
		}
	}

	e.log("recovery: triage %s attempt %d result=%s", req.SourceBeadID, triageCount+1, agentResult)

	if agentResult != "success" {
		return recovery.RecoveryActionResult{
			Kind:    req.Kind,
			Success: false,
			Error:   fmt.Sprintf("triage agent returned %s", agentResult),
			Output:  fmt.Sprintf("triage attempt %d failed: %s", triageCount+1, agentResult),
			Metadata: map[string]string{
				recovery.KeyTriageCount: newCount,
			},
		}
	}

	return recovery.RecoveryActionResult{
		Kind:           req.Kind,
		Success:        true,
		Output:         fmt.Sprintf("triage attempt %d succeeded", triageCount+1),
		ResolutionKind: "triage",
		Metadata: map[string]string{
			recovery.KeyResolutionKind: "triage",
			recovery.KeyTriageCount:    newCount,
		},
	}
}

// failResult constructs a failed RecoveryActionResult.
func failResult(kind recovery.RecoveryActionKind, msg string) recovery.RecoveryActionResult {
	return recovery.RecoveryActionResult{
		Kind:    kind,
		Success: false,
		Error:   msg,
	}
}

// ---------------------------------------------------------------------------
// Agentic recovery handlers (spi-f8pga)
//
// These implement the 6-step agentic recovery formula:
//   collect_context → decide → execute → verify → learn → finish
//
// collect_context and decide/learn involve Claude calls; execute and verify
// delegate to existing mechanical handlers; finish closes the bead unless
// decide chose escalate, in which case it stays open.
// ---------------------------------------------------------------------------

// CollectContextResult is the structured output of the collect_context step.
type CollectContextResult struct {
	Diagnosis      *recovery.Diagnosis       `json:"diagnosis"`
	RankedActions  []recovery.RecoveryAction `json:"ranked_actions"`
	BeadLearnings  []store.RecoveryLearning  `json:"bead_learnings"`
	CrossLearnings []store.RecoveryLearning  `json:"cross_bead_learnings"`
	WizardLogTail  string                    `json:"wizard_log_tail,omitempty"`
}

// actionClericLearn is the ActionHandler for the "cleric.learn" opcode.
// Delegates to handleLearn for the agentic v3 recovery formula.
func actionClericLearn(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	return handleLearn(e, stepName, step, state)
}

// handleCollectContext assembles diagnosis JSON + prior learnings from the
// recovery_learnings table (per-bead and cross-bead). This is mechanical —
// no Claude call. Also calls BuildRecoveryContext to assemble the full
// git-aware recovery context and stores it in state.Vars for subsequent
// phases (decide, execute, verify).
func handleCollectContext(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	// Resolve source bead from recovery bead metadata.
	sourceBeadID := resolveSourceBead(e, step)
	if sourceBeadID == "" {
		return ActionResult{Error: fmt.Errorf("collect_context: cannot resolve source bead")}
	}

	failureClass := ""
	if state != nil && state.Vars != nil {
		failureClass = state.Vars["failure_class"]
	}
	if failureClass == "" {
		if bead, err := e.deps.GetBead(e.beadID); err == nil {
			failureClass = bead.Meta(recovery.KeyFailureClass)
		}
	}

	// Build recovery.Deps adapter from executor deps and call Diagnose.
	recDeps := executorToRecoveryDeps(e)
	diag, diagErr := recovery.Diagnose(sourceBeadID, recDeps)

	// If diagnosis fails, we still continue with partial context.
	var rankedActions []recovery.RecoveryAction
	var wizardName string
	if diagErr == nil && diag != nil {
		rankedActions = diag.Actions
		wizardName = diag.WizardName
		if failureClass == "" {
			failureClass = string(diag.FailureMode)
		}
	} else {
		e.log("recovery: collect_context diagnosis failed (continuing with partial context): %v", diagErr)
	}

	// Read wizard log tail (best-effort).
	wizardLogTail := readWizardLogTail(wizardName)

	// Query per-bead learnings via bead metadata (existing approach).
	beadLearnings := queryBeadLearnings(sourceBeadID, failureClass)

	// Query cross-bead learnings.
	crossLearnings := queryCrossBeadLearnings(failureClass, 5)

	result := CollectContextResult{
		Diagnosis:      diag,
		RankedActions:  rankedActions,
		BeadLearnings:  beadLearnings,
		CrossLearnings: crossLearnings,
		WizardLogTail:  wizardLogTail,
	}

	resultJSON, err := json.Marshal(result)
	if err != nil {
		return ActionResult{Error: fmt.Errorf("collect_context: marshal result: %w", err)}
	}

	e.log("recovery: collect_context for %s (failure_class=%s, %d ranked actions, %d bead learnings, %d cross learnings)",
		sourceBeadID, failureClass, len(rankedActions), len(beadLearnings), len(crossLearnings))

	outputs := map[string]string{
		"status":                 "success",
		"collect_context_result": string(resultJSON),
		"failure_class":          failureClass,
		"source_bead":            sourceBeadID,
	}

	// Also output verification_status if diagnosis available (for already-clean detection).
	if diag != nil {
		hasInterrupt := false
		for _, a := range diag.Actions {
			if a.Name != "" {
				hasInterrupt = true
				break
			}
		}
		if !hasInterrupt && diag.InterruptLabel == "" {
			outputs["verification_status"] = "clean"
		} else {
			outputs["verification_status"] = "dirty"
		}
	}

	// Build the full git-aware recovery context via BuildRecoveryContext.
	// This provides git diagnostics, attempt history, human comments, and
	// repeated failure tracking for the decide step's enhanced logic.
	repoPath := e.effectiveRepoPath()
	var collectDB *sql.DB
	if e.deps != nil && e.deps.DoltDB != nil {
		collectDB = e.deps.DoltDB()
	}
	fullCtx, fullCtxErr := BuildRecoveryContext(collectDB, repoPath, e.beadID)
	if fullCtxErr != nil {
		e.log("recovery: collect_context: BuildRecoveryContext failed (non-fatal): %v", fullCtxErr)
	} else {
		// Store the FullRecoveryContext as JSON in state.Vars for
		// downstream phases (decide, execute, verify).
		if fullCtxJSON, jsonErr := json.Marshal(fullCtx); jsonErr == nil {
			if state.Vars == nil {
				state.Vars = make(map[string]string)
			}
			state.Vars["full_recovery_context"] = string(fullCtxJSON)
			outputs["total_attempts"] = strconv.Itoa(fullCtx.TotalAttempts)
			if fullCtx.FailedStep != "" {
				outputs["failed_step"] = fullCtx.FailedStep
			}
		}
	}

	return ActionResult{Outputs: outputs}
}

// handleDecide delegates to recovery.Decide and serializes its RepairPlan
// into step outputs. The thin-wrapper shape is per design
// spi-h32xj-cleric-repair-loop §6 — the decision policy (human-guidance
// parsing, promoted-recipe replay, git-state heuristics, Claude fallback)
// lives in pkg/recovery and the executor only bridges state/deps.
//
// Outputs: the typed `plan` JSON (recovery.RepairPlan) plus scalar hints
// (confidence, reasoning, needs_human, expected_outcome) that downstream
// steps surface in comments. The retired `chosen_action` scalar was
// removed in Chunk 6 — callers derive the action by unmarshaling `plan`.
func handleDecide(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	var contextJSON string
	if state != nil {
		if ss, ok := state.Steps["collect_context"]; ok {
			contextJSON = ss.Outputs["collect_context_result"]
		}
	}
	if contextJSON == "" {
		return ActionResult{Error: fmt.Errorf("decide: no collect_context_result in step outputs")}
	}

	var ccResult CollectContextResult
	if err := json.Unmarshal([]byte(contextJSON), &ccResult); err != nil {
		return ActionResult{Error: fmt.Errorf("decide: unmarshal collect_context_result: %w", err)}
	}

	var fullCtx *FullRecoveryContext
	if state != nil && state.Vars != nil {
		if frcJSON := state.Vars["full_recovery_context"]; frcJSON != "" {
			fullCtx = &FullRecoveryContext{}
			if err := json.Unmarshal([]byte(frcJSON), fullCtx); err != nil {
				e.log("recovery: decide: failed to parse full_recovery_context (continuing without): %v", err)
				fullCtx = nil
			}
		}
	}

	maxAttempts := 0
	if ma := step.With["max_attempts"]; ma != "" {
		if parsed, err := strconv.Atoi(ma); err == nil && parsed > 0 {
			maxAttempts = parsed
		}
	}

	triageCount := 0
	failureSig := ""
	if e.deps.GetBead != nil {
		if bead, err := e.deps.GetBead(e.beadID); err == nil {
			if tc := bead.Meta(recovery.KeyTriageCount); tc != "" {
				triageCount, _ = strconv.Atoi(tc)
			}
			failureSig = bead.Meta(recovery.KeyFailureSignature)
		}
	}
	if failureSig == "" && ccResult.Diagnosis != nil {
		failureSig = string(ccResult.Diagnosis.FailureMode)
	}

	var capturedClaudeResult *recovery.DecideResult
	recDeps := recovery.Deps{
		RecoveryBeadID:   e.beadID,
		Logf:             e.log,
		MaxAttempts:      maxAttempts,
		TriageCount:      triageCount,
		FailureSignature: failureSig,
		RankedActions:    ccResult.RankedActions,
		BeadLearnings:    ccResult.BeadLearnings,
		CrossLearnings:   ccResult.CrossLearnings,
		WizardLogTail:    ccResult.WizardLogTail,
		LearningStats: func(failureClass string) (*store.LearningStats, error) {
			return store.GetLearningStatsAuto(failureClass)
		},
		PromotionThreshold: func(sig string) int {
			var cfg repoconfig.ClericConfig
			if e.deps.RepoConfig != nil {
				if rc := e.deps.RepoConfig(); rc != nil {
					cfg = rc.Cleric
				}
			}
			return repoconfig.ResolveClericPromotionThreshold(cfg, sig)
		},
		CaptureDecideResult: func(r recovery.DecideResult) {
			rr := r
			capturedClaudeResult = &rr
		},
	}

	if e.deps.AddComment != nil {
		recDeps.AddRecoveryBeadComment = func(text string) error {
			return e.deps.AddComment(e.beadID, text)
		}
	}
	if e.deps.SetBeadMetadata != nil {
		recDeps.SetRecoveryBeadMeta = func(meta map[string]string) error {
			return e.deps.SetBeadMetadata(e.beadID, meta)
		}
	}
	if e.deps.ClaudeRunner != nil {
		recDeps.ClaudeRunner = func(args []string, label string) ([]byte, error) {
			return e.runClaude(args, label)
		}
	}

	diagnosis := recovery.Diagnosis{}
	if ccResult.Diagnosis != nil {
		diagnosis = *ccResult.Diagnosis
	}

	var history []store.RecoveryAttempt
	if fullCtx != nil {
		recDeps.BranchDiagnostics = fullCtx.GitState
		recDeps.WorktreeDiagnostics = fullCtx.WorktreeState
		recDeps.ConflictedFiles = fullCtx.ConflictedFiles
		recDeps.HumanComments = fullCtx.HumanComments
		recDeps.ContextSummary = SummarizeContext(fullCtx)
		history = fullCtx.AttemptHistory
	}

	plan, err := recovery.Decide(context.Background(), diagnosis, history, recDeps)
	if err != nil {
		return ActionResult{Error: err}
	}

	planJSON, err := json.Marshal(plan)
	if err != nil {
		return ActionResult{Error: fmt.Errorf("decide: marshal plan: %w", err)}
	}

	needsHuman := plan.Mode == recovery.RepairModeEscalate
	outputs := map[string]string{
		"status":      "success",
		"plan":        string(planJSON),
		"confidence":  fmt.Sprintf("%.2f", plan.Confidence),
		"reasoning":   plan.Reason,
		"needs_human": fmt.Sprintf("%t", needsHuman),
	}
	if capturedClaudeResult != nil {
		outputs["expected_outcome"] = capturedClaudeResult.ExpectedOutcome
	}
	if plan.Mode == recovery.RepairModeRecipe {
		outputs["promoted"] = "true"
		for k, v := range plan.Params {
			outputs["recipe_param_"+k] = v
		}
	}

	return ActionResult{Outputs: outputs}
}

// handleLearn assembles a RecoveryOutcome from step state and persists it
// through recovery.WriteOutcome — the single writer for outcome metadata and
// the recovery_learnings SQL table. Inputs are drawn from decide.outputs.plan
// (RepairPlan), verify.outputs (verdict/verify_kind), execute.outputs
// (worker_attempt_id), and collect_context.outputs (failure_class / failed
// step). No ad hoc metadata or label writes happen here.
func handleLearn(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	var plan recovery.RepairPlan
	var verifyVerdict recovery.VerifyVerdict
	var verifyKind recovery.VerifyKind
	var failureClass, failedStep, workerAttemptID string

	if state != nil {
		if ds, ok := state.Steps["decide"]; ok {
			if pjson := ds.Outputs["plan"]; pjson != "" {
				if err := json.Unmarshal([]byte(pjson), &plan); err != nil {
					e.log("recovery: learn: unmarshal decide plan (continuing with zero plan): %v", err)
					plan = recovery.RepairPlan{}
				}
			}
		}
		if vs, ok := state.Steps["verify"]; ok {
			verifyVerdict = recovery.VerifyVerdict(vs.Outputs["verdict"])
			verifyKind = recovery.VerifyKind(vs.Outputs["verify_kind"])
		}
		if cs, ok := state.Steps["collect_context"]; ok {
			failureClass = cs.Outputs["failure_class"]
			failedStep = cs.Outputs["failed_step"]
		}
		if es, ok := state.Steps["execute"]; ok {
			workerAttemptID = es.Outputs["worker_attempt_id"]
		}
	}

	bead, err := e.deps.GetBead(e.beadID)
	if err != nil {
		return ActionResult{Error: fmt.Errorf("learn: get recovery bead: %w", err)}
	}
	if failureClass == "" {
		failureClass = bead.Meta(recovery.KeyFailureClass)
	}

	sourceBeadID := resolveSourceBead(e, step)

	decision := recovery.DecisionResume
	if verifyVerdict != recovery.VerifyVerdictPass {
		decision = recovery.DecisionEscalate
	}
	if plan.Mode == recovery.RepairModeEscalate {
		decision = recovery.DecisionEscalate
	}

	outcome := recovery.RecoveryOutcome{
		RecoveryAttemptID: e.state.AttemptBeadID,
		SourceRunID:       e.currentRunID,
		SourceBeadID:      sourceBeadID,
		FailedStep:        failedStep,
		FailureClass:      recovery.FailureClass(failureClass),
		RepairMode:        plan.Mode,
		RepairAction:      plan.Action,
		WorkerAttemptID:   workerAttemptID,
		WorkspaceKind:     plan.Workspace.Kind,
		VerifyKind:        verifyKind,
		VerifyVerdict:     verifyVerdict,
		Decision:          decision,
	}

	if err := recovery.WriteOutcome(context.Background(), &bead, outcome); err != nil {
		e.log("recovery: learn: WriteOutcome: %s", err)
	}

	verificationStatus := "dirty"
	if verifyVerdict == recovery.VerifyVerdictPass {
		verificationStatus = "clean"
	}

	e.log("recovery: learn: outcome mode=%s action=%s verdict=%s decision=%s",
		plan.Mode, plan.Action, verifyVerdict, decision)

	return ActionResult{Outputs: map[string]string{
		"status":         "success",
		"verify_verdict": string(verifyVerdict),
		"decision":       string(decision),
		"repair_mode":    string(plan.Mode),
		"repair_action":  plan.Action,
		"outcome":        verificationStatus,
	}}
}

// handleRecordExecuteError posts a bead comment with the execute step's
// recorded error text so that when cleric re-enters the decide step, the
// prior failure shows up via spire focus's comment stream. The action is
// a no-op beyond commenting — the retry gating is handled by the formula's
// conditional edges (resets back to decide/execute/verify).
//
// Emits status="recorded" on success. If the execute step has no recorded
// error text, a placeholder message is posted instead (defensive: this
// should not happen under normal operation because the interpreter writes
// outputs.error before firing this step).
func handleRecordExecuteError(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	errMsg := ""
	if state != nil {
		if es, ok := state.Steps["execute"]; ok {
			errMsg = es.Outputs["error"]
		}
	}
	if errMsg == "" {
		errMsg = "(no error text recorded — check wizard logs)"
	}

	comment := fmt.Sprintf("Cleric execute errored — scheduling retry:\n\n```\n%s\n```", errMsg)
	if e.deps != nil && e.deps.AddComment != nil {
		if err := e.deps.AddComment(e.beadID, comment); err != nil {
			e.log("recovery: record_error: add comment: %s", err)
		}
	}

	e.log("recovery: record_error: recorded execute error, resetting decide/execute/verify for retry")
	return ActionResult{Outputs: map[string]string{"status": "recorded"}}
}

// handleFinish writes a closing comment, cleans up recovery protocol labels,
// and conditionally closes the recovery bead. If the decide step chose
// escalate, the bead is left open so `spire resolve` can find it and write
// the human learning before closing.
//
// The needs_human override can be triggered two ways:
//  1. decide step output needs_human=true (Claude chose to escalate)
//  2. step.With["needs_human"]="true" (formula forces escalation, e.g. when
//     the execute-error retry budget is exhausted)
func handleFinish(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	// Gather summary from step outputs.
	var chosenAction, outcome, reasoning string
	var needsHuman bool
	// Formula-level override: finish steps can force needs_human via step.With
	// (e.g. finish_needs_human_on_error when execute errors exhaust the budget).
	if step.With["needs_human"] == "true" {
		needsHuman = true
	}
	if state != nil {
		if ds, ok := state.Steps["decide"]; ok {
			if pjson := ds.Outputs["plan"]; pjson != "" {
				var plan recovery.RepairPlan
				if err := json.Unmarshal([]byte(pjson), &plan); err == nil {
					chosenAction = plan.Action
				}
			}
			reasoning = ds.Outputs["reasoning"]
			if ds.Outputs["needs_human"] == "true" {
				needsHuman = true
			}
		}
		if ls, ok := state.Steps["learn"]; ok {
			outcome = ls.Outputs["outcome"]
		}
		if outcome == "" {
			if vs, ok := state.Steps["verify"]; ok {
				if vs.Outputs["verification_status"] == "clean" || vs.Outputs["status"] == "success" {
					outcome = "clean"
				} else {
					outcome = "dirty"
				}
			}
		}
	}

	if chosenAction == "" {
		chosenAction = "unknown"
	}
	if outcome == "" {
		outcome = "unknown"
	}

	// Clean up recovery protocol labels on the target bead. This ensures
	// no stale retry request or result labels remain after the recovery
	// bead finishes, regardless of outcome. Guard against nil deps (tests).
	if e.deps != nil && e.deps.GetBead != nil {
		sourceBeadID := resolveSourceBead(e, step)
		if sourceBeadID != "" {
			if err := ClearRetryRequest(sourceBeadID); err != nil {
				e.log("recovery: finish: clear retry request on %s: %v", sourceBeadID, err)
			}
			if err := ClearRetryResult(sourceBeadID); err != nil {
				e.log("recovery: finish: clear retry result on %s: %v", sourceBeadID, err)
			}
		}
	}

	// Build closing comment.
	var comment strings.Builder
	comment.WriteString("Recovery closed.\n")
	comment.WriteString(fmt.Sprintf("Action: %s\n", chosenAction))
	comment.WriteString(fmt.Sprintf("Outcome: %s\n", outcome))
	if needsHuman {
		comment.WriteString("Human intervention was requested.\n")
	}
	if reasoning != "" {
		comment.WriteString(fmt.Sprintf("Reasoning: %s\n", reasoning))
	}

	_ = e.deps.AddComment(e.beadID, comment.String())

	// If decide chose escalate, leave the recovery bead open so that
	// `spire resolve` can find it, write the learning, and close it.
	if needsHuman {
		e.log("recovery: finish: leaving %s open (needs_human=true, action=%s)",
			e.beadID, chosenAction)
		return ActionResult{Outputs: map[string]string{
			"status":  "needs_human",
			"action":  chosenAction,
			"outcome": outcome,
		}}
	}

	// Close recovery bead for non-escalate paths.
	if err := e.deps.CloseBead(e.beadID); err != nil {
		e.log("recovery: finish: close bead %s: %s", e.beadID, err)
		return ActionResult{
			Outputs: map[string]string{"status": "failed"},
			Error:   fmt.Errorf("finish: close recovery bead: %w", err),
		}
	}

	e.log("recovery: finish: closed %s (action=%s, outcome=%s, needs_human=%t)",
		e.beadID, chosenAction, outcome, needsHuman)

	return ActionResult{Outputs: map[string]string{
		"status":  "success",
		"action":  chosenAction,
		"outcome": outcome,
	}}
}

// ---------------------------------------------------------------------------
// Plan-mode execute, cooperative verify, and opcode handlers (spi-h32xj §6)
// ---------------------------------------------------------------------------

// handlePlanExecute dispatches a typed RepairPlan produced by decide. It
// resolves the recovery workspace through the shared runtime contract
// (spi-xplwy) — dispatch on plan.Workspace.Kind selects between
// borrowing the target bead's staging worktree and provisioning a fresh
// owned worktree — then routes on plan.Mode to the matching execute
// surface:
//
//   - Noop       → no-op resume
//   - Mechanical → mechanicalActions[plan.Action]
//   - Worker     → SpawnRepairWorker
//   - Recipe     → executeRecipe (stub until chunk 7)
//   - Escalate   → terminal "needs_human" outcome
//
// Outputs surface: status, action (plan.Action or plan.Mode fallback), and
// mode. Step outputs for mechanical/worker/recipe dispatches add their own
// attempt IDs on top; see each branch below.
func handlePlanExecute(e *Executor, stepName string, step StepConfig, state *GraphState, plan recovery.RepairPlan) ActionResult {
	actionName := plan.Action
	if actionName == "" {
		actionName = string(plan.Mode)
	}
	outputs := map[string]string{
		"status": "success",
		"action": actionName,
		"mode":   string(plan.Mode),
	}

	switch plan.Mode {
	case recovery.RepairModeNoop:
		e.log("recovery: execute noop — resuming hooked bead without repair")
		return ActionResult{Outputs: outputs}
	case recovery.RepairModeEscalate:
		outputs["status"] = "needs_human"
		if plan.Reason != "" {
			outputs["reason"] = plan.Reason
		}
		e.log("recovery: execute escalate: %s", plan.Reason)
		return ActionResult{Outputs: outputs}
	}

	sourceBeadID := resolveSourceBead(e, step)
	if sourceBeadID == "" {
		return ActionResult{Error: fmt.Errorf("execute: cannot resolve source bead")}
	}

	actionCtx, ws, cleanup, err := e.buildRecoveryActionCtx(sourceBeadID, plan, step, state)
	if err != nil {
		return ActionResult{Error: fmt.Errorf("execute: provision workspace for %s: %w", actionName, err)}
	}
	if cleanup != nil {
		defer cleanup()
	}

	// Resolve the recovery bead's failure signature once so we can either
	// demote on failure or embed the signature alongside the captured
	// recipe on success.
	failureSig := ""
	if bead, berr := e.deps.GetBead(e.beadID); berr == nil {
		failureSig = bead.Meta(recovery.KeyFailureSignature)
	}

	var recipe *recovery.MechanicalRecipe
	var execErr error

	switch plan.Mode {
	case recovery.RepairModeMechanical:
		fn, ok := mechanicalActions[plan.Action]
		if !ok {
			execErr = fmt.Errorf("unknown mechanical action %q — decide/execute vocabulary mismatch", plan.Action)
		} else {
			recipe, execErr = fn(actionCtx, plan, ws)
		}
	case recovery.RepairModeWorker:
		var workerResult RepairWorkerResult
		workerResult, execErr = SpawnRepairWorker(actionCtx, plan, ws)
		if workerResult.WorkerAttemptID != "" {
			outputs["worker_attempt_id"] = workerResult.WorkerAttemptID
		}
		if workerResult.Output != "" {
			outputs["output"] = workerResult.Output
		}
	case recovery.RepairModeRecipe:
		var recipeResult RepairResult
		recipeResult, execErr = executeRecipe(actionCtx, plan, ws)
		recipe = recipeResult.Recipe
		if recipeResult.Output != "" {
			outputs["output"] = recipeResult.Output
		}
	default:
		execErr = fmt.Errorf("unsupported repair mode %q", plan.Mode)
	}

	if execErr != nil {
		outputs["status"] = "failed"
		outputs["error"] = execErr.Error()
		// Promotion demotion: if the chosen action was a promoted recipe
		// (decide step tagged "promoted=true"), this failure resets the
		// counter for this signature. One regression undoes promotion.
		if state != nil {
			if ds, ok := state.Steps["decide"]; ok && ds.Outputs["promoted"] == "true" && failureSig != "" {
				if derr := recovery.MarkDemoted(failureSig); derr != nil {
					e.log("recovery: demote %s after promoted-recipe failure: %v", failureSig, derr)
				} else {
					e.log("recovery: demoted %s (promoted recipe %s failed)", failureSig, actionName)
				}
			}
		}
		e.log("recovery: execute %s (mode=%s) failed: %v", actionName, plan.Mode, execErr)
		return ActionResult{
			Outputs: outputs,
			Error:   fmt.Errorf("recovery action %q failed: %w", actionName, execErr),
		}
	}

	// Recipe capture: serialise the recipe returned by the mechanical
	// dispatch into the step outputs so handleLearn can persist it on the
	// learning row. Worker-mode paths leave recipe nil and simply skip.
	if recipe != nil {
		if serialised, merr := recovery.MarshalRecipe(recipe); merr != nil {
			e.log("recovery: marshal recipe for %s: %v (continuing without capture)", actionName, merr)
		} else if serialised != "" {
			outputs["mechanical_recipe"] = serialised
			if failureSig != "" {
				outputs["failure_signature"] = failureSig
			}
		}
	}

	e.log("recovery: execute %s (mode=%s) succeeded", actionName, plan.Mode)
	return ActionResult{Outputs: outputs}
}

// buildRecoveryActionCtx resolves the workspace a RepairPlan requires via
// the shared runtime contract (spi-xplwy) and assembles the
// RecoveryActionCtx carrying every dep the mechanical and worker
// dispatches read. Dispatch on plan.Workspace.Kind mirrors the runtime
// model documented in spi-h32xj §3:
//
//   - repo:             pure-db op; no worktree provisioning.
//   - borrowed_worktree: resume the target bead's wizard staging worktree.
//   - owned_worktree:    fresh recovery branch off the target bead's feat
//     branch via pkg/git primitives; cleanup removes the worktree and
//     deletes the branch.
func (e *Executor) buildRecoveryActionCtx(sourceBeadID string, plan recovery.RepairPlan, step StepConfig, state *GraphState) (*RecoveryActionCtx, WorkspaceHandle, func(), error) {
	// Merge step.With and decide outputs so mechanical params flow through.
	params := make(map[string]string)
	for k, v := range step.With {
		params[k] = v
	}
	if state != nil {
		if ds, ok := state.Steps["decide"]; ok {
			for k, v := range ds.Outputs {
				if _, exists := params[k]; !exists {
					params[k] = v
				}
			}
		}
	}
	for k, v := range plan.Params {
		params[k] = v
	}

	repoPath := e.effectiveRepoPath()

	wc, ws, cleanup, err := e.resolveRepairWorkspace(sourceBeadID, plan)
	if err != nil {
		return nil, WorkspaceHandle{}, nil, err
	}

	baseBranch := ws.BaseBranch
	if baseBranch == "" {
		baseBranch = resolveBaseBranchForBead(sourceBeadID, e)
	}

	var db *sql.DB
	if e.deps != nil && e.deps.DoltDB != nil {
		db = e.deps.DoltDB()
	}

	actionCtx := &RecoveryActionCtx{
		DB:             db,
		RepoPath:       repoPath,
		BaseBranch:     baseBranch,
		Worktree:       wc,
		RecoveryBeadID: e.beadID,
		TargetBeadID:   sourceBeadID,
		Params:         params,
		Log:            func(msg string) { e.log("recovery: %s", msg) },
		Spawner:        e.deps.Spawner,
		RecordAgentRun: e.deps.RecordAgentRun,
		AgentResultDir: e.deps.AgentResultDir,
		LogBaseDir:     dolt.GlobalDir(),
		ParentRunID:    e.currentRunID,
		AgentNamespace: "cleric-repair",
	}

	return actionCtx, ws, cleanup, nil
}

// resolveRepairWorkspace materializes the workspace described by
// plan.Workspace. It is the cleric's entry point into the shared runtime
// workspace contract (spi-xplwy): every RepairMode that needs a
// substrate flows through a single Kind-keyed dispatch. plan.Workspace is
// authoritative; when the Kind is empty, execute defaults to
// owned_worktree (the mechanical case). Noop/escalate resolve no
// workspace — those modes short-circuit in handlePlanExecute before this
// helper runs.
func (e *Executor) resolveRepairWorkspace(sourceBeadID string, plan recovery.RepairPlan) (*spgit.WorktreeContext, WorkspaceHandle, func(), error) {
	const wsName = "recovery"
	kind := plan.Workspace.Kind
	if kind == "" {
		kind = WorkspaceKindOwnedWorktree
	}
	repoPath := e.effectiveRepoPath()
	baseBranch := resolveBaseBranchForBead(sourceBeadID, e)

	switch kind {
	case WorkspaceKindRepo:
		return nil, WorkspaceHandle{
			Name:       wsName,
			Kind:       WorkspaceKindRepo,
			Path:       repoPath,
			BaseBranch: baseBranch,
			Origin:     WorkspaceOriginLocalBind,
		}, nil, nil

	case WorkspaceKindBorrowedWorktree:
		borrowFrom := plan.Workspace.BorrowFrom
		if borrowFrom == "" {
			borrowFrom = sourceBeadID
		}
		wc, err := resumeWizardStagingWorktree(borrowFrom, e)
		if err != nil {
			return nil, WorkspaceHandle{}, nil, fmt.Errorf("borrow workspace from %s: %w", borrowFrom, err)
		}
		bb := wc.BaseBranch
		if bb == "" {
			bb = baseBranch
		}
		return wc, WorkspaceHandle{
			Name:       wsName,
			Kind:       WorkspaceKindBorrowedWorktree,
			Path:       wc.Dir,
			Branch:     wc.Branch,
			BaseBranch: bb,
			Origin:     WorkspaceOriginLocalBind,
			Borrowed:   true,
		}, nil, nil

	case WorkspaceKindOwnedWorktree:
		dir := filepath.Join(repoPath, ".worktrees", sourceBeadID+"-recovery")
		branch := "recovery/" + sourceBeadID
		startPoint := "feat/" + sourceBeadID
		if b, gerr := store.GetBead(sourceBeadID); gerr == nil {
			if fb := store.HasLabel(b, "feat-branch:"); fb != "" {
				startPoint = fb
			}
		}
		base := repoconfig.ResolveBranchBase(baseBranch)
		rc := &spgit.RepoContext{Dir: repoPath, BaseBranch: base}
		wc, err := rc.CreateWorktreeNewBranch(dir, branch, startPoint)
		if err != nil {
			return nil, WorkspaceHandle{}, nil, fmt.Errorf("create owned recovery worktree at %s from %s: %w", dir, startPoint, err)
		}
		cleanup := func() {
			wc.Cleanup()
			rc2 := &spgit.RepoContext{Dir: repoPath, BaseBranch: base}
			_ = rc2.ForceDeleteBranch(branch)
		}
		return wc, WorkspaceHandle{
			Name:       wsName,
			Kind:       WorkspaceKindOwnedWorktree,
			Path:       wc.Dir,
			Branch:     branch,
			BaseBranch: base,
			Origin:     WorkspaceOriginLocalBind,
		}, cleanup, nil

	default:
		return nil, WorkspaceHandle{}, nil, fmt.Errorf("resolve repair workspace: unsupported kind %q", kind)
	}
}

// handleVerify drives the cleric's verify step. It loads the RepairPlan
// from decide.outputs.plan, hands the embedded VerifyPlan off to the target
// bead's wizard via the cooperative retry protocol, and translates the
// wizard's VerifyVerdict into the step outputs consumed by the graph
// interpreter.
//
// VerifyKind dispatch (design spi-h32xj §5) lives on the wizard side — this
// function just carries the plan through runVerifyPlan. Legacy decide
// outputs without a typed plan fall back to an implicit Kind=rerun-step
// using the mapped wizard phase, preserving pre-chunk-5 behavior.
//
// Emits the legacy outputs (status/loop_to/step_reached/failed_step/error)
// side-by-side with the chunk-5 fields (verdict/decision/verify_kind) so
// downstream steps that haven't migrated keep working through the
// coexistence window.
//
// On execute failure: skip retry request, output loop_to=decide directly.
func handleVerify(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	// Check whether execute succeeded.
	var executeStatus, actionName, sourceBeadID string
	if state != nil {
		if es, ok := state.Steps["execute"]; ok {
			executeStatus = es.Outputs["status"]
			actionName = es.Outputs["action"]
		}
		if cs, ok := state.Steps["collect_context"]; ok {
			sourceBeadID = cs.Outputs["source_bead"]
		}
	}
	if sourceBeadID == "" {
		sourceBeadID = resolveSourceBead(e, step)
	}

	// On execute failure: skip retry request, loop back to decide.
	if executeStatus == "failed" || executeStatus == "" {
		e.log("recovery: verify: execute %s failed, looping back to decide", actionName)
		return ActionResult{Outputs: map[string]string{
			"status":  "failed",
			"loop_to": "decide",
			"reason":  "execute action failed",
		}}
	}

	// Determine failed step and attempt number from context.
	failedStep := ""
	wizardPhase := ""
	attemptNumber := 1
	if state != nil && state.Vars != nil {
		if frcJSON := state.Vars["full_recovery_context"]; frcJSON != "" {
			var fullCtx FullRecoveryContext
			if err := json.Unmarshal([]byte(frcJSON), &fullCtx); err == nil {
				failedStep = fullCtx.FailedStep
				wizardPhase = fullCtx.WizardPhase
				attemptNumber = fullCtx.TotalAttempts + 1
			}
		}
	}

	// Map graph step name to wizard-compatible phase. Prefer the flow-derived
	// WizardPhase (set from SourceFlow metadata) when available; otherwise
	// translate the raw failedStep via MapToWizardPhase.
	mappedStep := wizardPhase
	if mappedStep == "" {
		mappedStep = MapToWizardPhase(failedStep)
	} else if !KnownWizardPhases[mappedStep] {
		// WizardPhase from flow might also need mapping (e.g., "task-plan" → "design").
		mappedStep = MapToWizardPhase(mappedStep)
	}
	if mappedStep != failedStep {
		e.log("recovery: verify: mapped step %q → wizard phase %q", failedStep, mappedStep)
	}

	// Read human guidance and the typed RepairPlan from decide outputs.
	// A missing or unparseable plan is not fatal — the rerun-step path is
	// derived from mappedStep so legacy decide outputs continue to work.
	guidance := ""
	var plan recovery.RepairPlan
	if state != nil {
		if ds, ok := state.Steps["decide"]; ok {
			guidance = ds.Outputs["reasoning"]
			if pjson := ds.Outputs["plan"]; pjson != "" {
				if err := json.Unmarshal([]byte(pjson), &plan); err != nil {
					e.log("recovery: verify: failed to parse decide plan (falling back to implicit rerun-step): %v", err)
					plan = recovery.RepairPlan{}
				}
			}
		}
	}

	verdict, result, err := runVerifyPlan(e, plan, sourceBeadID, mappedStep, attemptNumber, guidance, state)

	verifyKind := plan.Verify.Kind
	if verifyKind == "" {
		verifyKind = recovery.VerifyKindRerunStep
	}

	outputs := map[string]string{
		"verdict":     string(verdict),
		"decision":    string(verdictToDecision(verdict)),
		"verify_kind": string(verifyKind),
	}

	if err != nil {
		outputs["status"] = "failed"
		outputs["loop_to"] = "decide"
		outputs["reason"] = fmt.Sprintf("set retry request failed: %v", err)
		return ActionResult{Outputs: outputs, Error: err}
	}

	switch verdict {
	case recovery.VerifyVerdictPass:
		outputs["status"] = "success"
		if result != nil {
			outputs["step_reached"] = result.StepReached
		}
		return ActionResult{Outputs: outputs}
	case recovery.VerifyVerdictTimeout:
		outputs["status"] = "failed"
		outputs["loop_to"] = "decide"
		outputs["reason"] = fmt.Sprintf("verify polling timed out after %s",
			time.Duration(DefaultVerifyTimeout)*time.Second)
		return ActionResult{Outputs: outputs}
	default:
		outputs["status"] = "failed"
		outputs["loop_to"] = "decide"
		outputs["reason"] = "retry failed"
		if result != nil {
			outputs["failed_step"] = result.FailedStep
			outputs["error"] = result.Error
		}
		return ActionResult{Outputs: outputs}
	}
}

// actionClericVerify is the ActionHandler for the "cleric.verify" opcode.
// Delegates to handleVerify for the cooperative retry loop.
func actionClericVerify(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	return handleVerify(e, stepName, step, state)
}

// ---------------------------------------------------------------------------
// Helper functions for agentic recovery
// ---------------------------------------------------------------------------

// resolveWizardName derives the wizard agent name for a given source bead.
// Checks children for an agent: label, falls back to "wizard-<sourceBeadID>".
func resolveWizardName(sourceBeadID string, deps *Deps) string {
	wizardName := "wizard-" + sourceBeadID
	if deps == nil || deps.GetChildren == nil {
		return wizardName
	}
	children, err := deps.GetChildren(sourceBeadID)
	if err != nil {
		return wizardName
	}
	for _, c := range children {
		for _, l := range c.Labels {
			if strings.HasPrefix(l, "agent:") {
				return strings.TrimPrefix(l, "agent:")
			}
		}
	}
	return wizardName
}

// resumeWizardStagingWorktree resolves the wizard's staging worktree from graph
// state and either resumes it (dir exists) or recreates it (dir gone, branch
// exists). Returns an error if graph state is unavailable or has no staging info.
func resumeWizardStagingWorktree(sourceBeadID string, e *Executor) (*spgit.WorktreeContext, error) {
	wizardName := resolveWizardName(sourceBeadID, e.deps)
	gs, err := LoadGraphState(wizardName, e.deps.ConfigDir)
	if err != nil || gs == nil {
		return nil, fmt.Errorf("no graph state for wizard %s: %w", wizardName, err)
	}

	// Extract staging worktree info. Prefer Workspaces["staging"] (v3 formulas),
	// fall back to top-level WorktreeDir + StagingBranch (v2 compat).
	var dir, branch, baseBranch, repoPath string
	if ws, ok := gs.Workspaces["staging"]; ok && ws.Dir != "" {
		dir = ws.Dir
		branch = ws.Branch
		baseBranch = ws.BaseBranch
	}
	if dir == "" {
		dir = gs.WorktreeDir
	}
	if branch == "" {
		branch = gs.StagingBranch
	}
	if baseBranch == "" {
		baseBranch = gs.BaseBranch
	}
	if baseBranch == "" {
		baseBranch = repoconfig.DefaultBranchBase
	}
	repoPath = gs.RepoPath
	if repoPath == "" {
		repoPath = e.effectiveRepoPath()
	}

	if dir == "" {
		return nil, fmt.Errorf("wizard %s graph state has no staging worktree dir", wizardName)
	}
	if branch == "" {
		return nil, fmt.Errorf("wizard %s graph state has no staging branch", wizardName)
	}

	// If the worktree directory still exists on disk, resume it.
	if _, statErr := os.Stat(dir); statErr == nil {
		return spgit.ResumeWorktreeContext(dir, branch, baseBranch, repoPath,
			func(msg string, args ...any) { e.log("cleric-worktree: "+msg, args...) })
	}

	// Directory gone — recreate worktree from the staging branch.
	e.log("staging worktree %s gone; recreating from branch %s", dir, branch)
	rc := &spgit.RepoContext{Dir: repoPath, BaseBranch: baseBranch}
	return rc.CreateWorktree(dir, branch)
}

// resolveSourceBead resolves the source bead ID from step params or bead metadata.
func resolveSourceBead(e *Executor, step StepConfig) string {
	if id := step.With["source_bead_id"]; id != "" {
		return id
	}
	if bead, err := e.deps.GetBead(e.beadID); err == nil {
		return bead.Meta(recovery.KeySourceBead)
	}
	return ""
}

// resolveBaseBranchForBead resolves the base branch for a target bead by
// walking its parent chain for a `base-branch:` label. Falls back to
// repoconfig.DefaultBranchBase if no label is found anywhere in the chain.
// Used by recovery actions so rebases and diffs target the correct branch
// (never a hardcoded "main").
func resolveBaseBranchForBead(beadID string, e *Executor) string {
	visited := make(map[string]bool)
	current := beadID
	for current != "" && !visited[current] {
		visited[current] = true
		bead, err := e.deps.GetBead(current)
		if err != nil {
			break
		}
		if bb := e.deps.HasLabel(bead, "base-branch:"); bb != "" {
			return bb
		}
		current = bead.Parent
	}
	return repoconfig.DefaultBranchBase
}

// executorToRecoveryDeps builds a recovery.Deps from executor.Deps.
// Some fields are left nil — Diagnose handles nil-safe.
func executorToRecoveryDeps(e *Executor) *recovery.Deps {
	return &recovery.Deps{
		GetBead: func(id string) (recovery.DepBead, error) {
			b, err := e.deps.GetBead(id)
			if err != nil {
				return recovery.DepBead{}, err
			}
			return recovery.DepBead{
				ID: b.ID, Title: b.Title, Status: b.Status,
				Labels: b.Labels, Parent: b.Parent,
			}, nil
		},
		GetChildren: func(parentID string) ([]recovery.DepBead, error) {
			children, err := e.deps.GetChildren(parentID)
			if err != nil {
				return nil, err
			}
			result := make([]recovery.DepBead, len(children))
			for i, c := range children {
				result[i] = recovery.DepBead{
					ID: c.ID, Title: c.Title, Status: c.Status,
					Labels: c.Labels, Parent: c.Parent,
				}
			}
			return result, nil
		},
		GetDependentsWithMeta: func(id string) ([]recovery.DepDependent, error) {
			if e.deps.GetDependentsWithMeta == nil {
				return nil, nil
			}
			deps, err := e.deps.GetDependentsWithMeta(id)
			if err != nil {
				return nil, err
			}
			result := make([]recovery.DepDependent, len(deps))
			for i, d := range deps {
				result[i] = recovery.DepDependent{
					ID:             d.ID,
					Title:          d.Title,
					Status:         string(d.Status),
					Labels:         d.Labels,
					DependencyType: string(d.DependencyType),
				}
			}
			return result, nil
		},
		AddComment: e.deps.AddComment,
		CloseBead:  e.deps.CloseBead,
		// LoadExecutorState, CheckBranch*, LookupRegistry, ResolveRepo left nil.
		// Diagnose handles nil-safe for these optional capabilities.
	}
}

// queryBeadLearnings queries per-bead learnings from both bead metadata and the
// SQL recovery_learnings table. The SQL table is canonical (human learnings from
// `spire resolve` are written there only); bead metadata is the fallback for
// older learnings written before the table existed.
func queryBeadLearnings(sourceBeadID, failureClass string) []store.RecoveryLearning {
	reusable := true
	filter := store.RecoveryLookupFilter{
		SourceBead:   sourceBeadID,
		FailureClass: failureClass,
		Reusable:     &reusable,
		Limit:        10,
	}
	metaLearnings, err := store.ListClosedRecoveryBeads(filter)
	if err != nil {
		metaLearnings = nil
	}

	// Query SQL recovery_learnings table (canonical source for human learnings).
	var sqlRows []store.RecoveryLearningRow
	if sourceBeadID != "" && failureClass != "" {
		sqlRows, _ = store.GetBeadLearningsAuto(sourceBeadID, failureClass)
	}

	return mergeLearnings(metaLearnings, sqlRows, 10)
}

// queryCrossBeadLearnings queries cross-bead learnings from both bead metadata
// and the SQL recovery_learnings table, merging and deduplicating the results.
func queryCrossBeadLearnings(failureClass string, limit int) []store.RecoveryLearning {
	reusable := true
	filter := store.RecoveryLookupFilter{
		FailureClass: failureClass,
		Reusable:     &reusable,
		Limit:        limit,
	}
	metaLearnings, err := store.ListClosedRecoveryBeads(filter)
	if err != nil {
		metaLearnings = nil
	}

	// Query SQL recovery_learnings table (canonical source for human learnings).
	var sqlRows []store.RecoveryLearningRow
	if failureClass != "" {
		sqlRows, _ = store.GetCrossBeadLearningsAuto(failureClass, limit)
	}

	return mergeLearnings(metaLearnings, sqlRows, limit)
}

// sqlRowToLearning converts a SQL RecoveryLearningRow to the RecoveryLearning
// read model used by the decide and relapse paths.
func sqlRowToLearning(row store.RecoveryLearningRow) store.RecoveryLearning {
	return store.RecoveryLearning{
		BeadID:           row.RecoveryBead,
		FailureClass:     row.FailureClass,
		FailureSignature: row.FailureSig,
		SourceBead:       row.SourceBead,
		ResolutionKind:   row.ResolutionKind,
		Reusable:         row.Reusable,
		ResolvedAt:       row.ResolvedAt.UTC().Format(time.RFC3339),
		LearningSummary:  row.LearningSummary,
		Outcome:          row.Outcome,
	}
}

// mergeLearnings combines bead-metadata learnings with SQL-sourced learnings,
// deduplicating by recovery bead ID. SQL wins as the canonical source — if
// both sources have a learning for the same recovery bead, the SQL version is kept.
func mergeLearnings(metaLearnings []store.RecoveryLearning, sqlRows []store.RecoveryLearningRow, limit int) []store.RecoveryLearning {
	if limit <= 0 {
		limit = 10
	}

	// Start with SQL learnings (canonical).
	seen := make(map[string]bool, len(sqlRows))
	var merged []store.RecoveryLearning
	for _, row := range sqlRows {
		l := sqlRowToLearning(row)
		merged = append(merged, l)
		seen[l.BeadID] = true
	}

	// Add metadata learnings not already covered by SQL.
	for _, l := range metaLearnings {
		if !seen[l.BeadID] {
			merged = append(merged, l)
			seen[l.BeadID] = true
		}
	}

	// Sort by resolved_at descending.
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].ResolvedAt > merged[j].ResolvedAt
	})

	if len(merged) > limit {
		merged = merged[:limit]
	}

	return merged
}

