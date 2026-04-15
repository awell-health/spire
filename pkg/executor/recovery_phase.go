package executor

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
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
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
)

func init() {
	actionRegistry["cleric.execute"] = actionClericExecute
	actionRegistry["cleric.decide"] = actionClericDecide
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
		// Read chosen_action from the decide step's output.
		if state != nil {
			if ds, ok := state.Steps["decide"]; ok {
				if chosen := ds.Outputs["chosen_action"]; chosen != "" {
					actionKind = chosen
					e.log("recovery: execute using decide output: %s", actionKind)
				}
			}
		}
		if actionKind == "execute" {
			return ActionResult{Error: fmt.Errorf("recovery execute: no chosen_action from decide step")}
		}

		// Check if the chosen action is in the git-aware recovery action
		// registry (recovery_actions.go). If so, route through RunRecoveryAction
		// which handles worktree provisioning and per-attempt tracking.
		if _, isGitAware := GetAction(actionKind); isGitAware {
			return handleGitAwareExecute(e, stepName, step, state, actionKind)
		}
	case "verify":
		return handleVerify(e, stepName, step, state)
	case "learn":
		return handleLearn(e, stepName, step, state)
	case "finish":
		return handleFinish(e, stepName, step, state)
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
			"step_target":             req.StepTarget,
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
	// Pattern: check for agent: label on attempt beads, fall back to "wizard-<sourceBeadID>".
	// Uses first-match: the first child with an agent: label wins.
	wizardName := "wizard-" + req.SourceBeadID
	if children, err := e.deps.GetChildren(req.SourceBeadID); err == nil {
		found := false
		for _, c := range children {
			for _, l := range c.Labels {
				if strings.HasPrefix(l, "agent:") {
					wizardName = strings.TrimPrefix(l, "agent:")
					found = true
					break
				}
			}
			if found {
				break
			}
		}
	}

	// Load the source bead's graph state to find the worktree path.
	var worktreeDir string
	gs, err := LoadGraphState(wizardName, e.deps.ConfigDir)
	if err == nil && gs != nil {
		// Prefer "feature" workspace, fall back to first workspace with a Dir.
		if ws, ok := gs.Workspaces["feature"]; ok && ws.Dir != "" {
			worktreeDir = ws.Dir
		} else {
			for _, ws := range gs.Workspaces {
				if ws.Dir != "" {
					worktreeDir = ws.Dir
					break
				}
			}
		}
		// Fall back to top-level WorktreeDir.
		if worktreeDir == "" {
			worktreeDir = gs.WorktreeDir
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
				repoDir, _, _, resolveErr := e.deps.ResolveRepo(req.SourceBeadID)
				if resolveErr != nil || repoDir == "" {
					return failResult(req.Kind, fmt.Sprintf("cannot resolve repo for bead %s: %v", req.SourceBeadID, resolveErr))
				}

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

	handle, spawnErr := e.deps.Spawner.Spawn(agent.SpawnConfig{
		Name:         spawnName,
		BeadID:       req.SourceBeadID,
		Role:         agent.RoleApprentice,
		ExtraArgs:    []string{"--worktree-dir", worktreeDir, "--no-review"},
		CustomPrompt: prompt.String(),
		LogPath:      filepath.Join(dolt.GlobalDir(), "wizards", spawnName+".log"),
	})
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

// DecideResult is the structured output of the decide step (Claude response).
type DecideResult struct {
	ChosenAction    string  `json:"chosen_action"`
	Confidence      float64 `json:"confidence"`
	Reasoning       string  `json:"reasoning"`
	NeedsHuman      bool    `json:"needs_human"`
	ExpectedOutcome string  `json:"expected_outcome"`
}

// LearnResult is the structured output of the learn step (Claude response).
type LearnResult struct {
	LearningSummary string `json:"learning_summary"`
	ResolutionKind  string `json:"resolution_kind"`
	Reusable        bool   `json:"reusable"`
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

// handleDecide calls Claude with the collect_context result to choose a
// recovery action. Also incorporates FullRecoveryContext when available:
// avoids repeating failed actions, parses human comments as guidance, and
// auto-escalates when TotalAttempts >= max_attempts (default 3).
//
// Decision priority:
//  (a) human guidance if present
//  (b) git-state-driven (behind main → rebase, dirty worktree → rebuild,
//      conflict → resolve-conflicts)
//  (c) Claude-selected action
//  (d) fallback to resummon if unclear
func handleDecide(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	// Read collect_context result from previous step outputs.
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

	// Load FullRecoveryContext from state vars (set by collect_context).
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

	// Check max attempts — auto-escalate if threshold reached.
	maxAttempts := DefaultMaxRecoveryAttempts
	if ma := step.With["max_attempts"]; ma != "" {
		if parsed, err := strconv.Atoi(ma); err == nil && parsed > 0 {
			maxAttempts = parsed
		}
	}
	if fullCtx != nil && fullCtx.TotalAttempts >= maxAttempts {
		e.log("recovery: decide: auto-escalate — total attempts %d >= max %d", fullCtx.TotalAttempts, maxAttempts)
		return ActionResult{Outputs: map[string]string{
			"status":        "success",
			"chosen_action": "escalate",
			"confidence":    "1.00",
			"reasoning":     fmt.Sprintf("Auto-escalate: %d recovery attempts exhausted (max %d)", fullCtx.TotalAttempts, maxAttempts),
			"needs_human":   "true",
		}}
	}

	// (a) Check for human guidance in comments.
	if fullCtx != nil && len(fullCtx.HumanComments) > 0 {
		if guided := parseHumanGuidance(fullCtx.HumanComments, fullCtx.RepeatedFailures); guided != "" {
			e.log("recovery: decide: human guidance detected → %s", guided)
			return ActionResult{Outputs: map[string]string{
				"status":        "success",
				"chosen_action": guided,
				"confidence":    "0.90",
				"reasoning":     "Human guidance from bead comment",
				"needs_human":   "false",
			}}
		}
	}

	// (b) Git-state-driven decision when FullRecoveryContext is available.
	if fullCtx != nil {
		if gitAction := decideFromGitState(fullCtx); gitAction != "" {
			// Verify this action hasn't repeatedly failed.
			if fullCtx.RepeatedFailures[gitAction] < 2 {
				e.log("recovery: decide: git-state-driven → %s", gitAction)
				return ActionResult{Outputs: map[string]string{
					"status":        "success",
					"chosen_action": gitAction,
					"confidence":    "0.85",
					"reasoning":     fmt.Sprintf("Git state analysis: %s", gitStateReasoning(fullCtx, gitAction)),
					"needs_human":   "false",
				}}
			}
			e.log("recovery: decide: git-state suggests %s but it has %d prior failures, falling through to Claude",
				gitAction, fullCtx.RepeatedFailures[gitAction])
		}
	}

	// (c) Fall through to Claude-based decision.
	// Read triage count from recovery bead metadata for the decide prompt.
	triageCount := 0
	if bead, err := e.deps.GetBead(e.beadID); err == nil {
		if tc := bead.Meta(recovery.KeyTriageCount); tc != "" {
			triageCount, _ = strconv.Atoi(tc)
		}
	}

	// Query historical outcome statistics for the failure class.
	var stats *store.LearningStats
	if ccResult.Diagnosis != nil {
		failureClass := string(ccResult.Diagnosis.FailureMode)
		stats, _ = store.GetLearningStatsAuto(failureClass)
	}

	// Build Claude prompt — include FullRecoveryContext summary if available.
	prompt := buildDecidePrompt(ccResult, triageCount, stats)
	if fullCtx != nil {
		prompt += "\n\n## Full Recovery Context (git-aware)\n\n" + SummarizeContext(fullCtx)
	}

	// Call Claude.
	if e.deps.ClaudeRunner == nil {
		// (d) No Claude runner → fallback to resummon.
		e.log("recovery: decide: ClaudeRunner not available, falling back to resummon")
		return ActionResult{Outputs: map[string]string{
			"status":        "success",
			"chosen_action": "resummon",
			"confidence":    "0.50",
			"reasoning":     "Fallback: ClaudeRunner unavailable",
			"needs_human":   "false",
		}}
	}

	args := []string{
		"--dangerously-skip-permissions",
		"-p", prompt,
		"--output-format", "text",
		"--max-turns", "1",
	}

	out, err := e.deps.ClaudeRunner(args, e.effectiveRepoPath())
	if err != nil {
		// (d) Claude call failed → fallback to resummon.
		e.log("recovery: decide: claude call failed, falling back to resummon: %v", err)
		return ActionResult{Outputs: map[string]string{
			"status":        "success",
			"chosen_action": "resummon",
			"confidence":    "0.40",
			"reasoning":     fmt.Sprintf("Fallback: Claude call failed: %v", err),
			"needs_human":   "false",
		}}
	}

	// Parse Claude's JSON response.
	var result DecideResult
	if err := parseJSONFromClaude(out, &result); err != nil {
		return ActionResult{Error: fmt.Errorf("decide: parse claude response: %w", err)}
	}

	// Validate Claude's chosen action against repeated failures.
	if fullCtx != nil && fullCtx.RepeatedFailures[result.ChosenAction] >= 2 {
		originalAction := result.ChosenAction
		e.log("recovery: decide: Claude chose %q but it has %d prior failures — overriding to escalate",
			originalAction, fullCtx.RepeatedFailures[originalAction])
		result.ChosenAction = "escalate"
		result.Reasoning = fmt.Sprintf("Overridden: original choice %q has %d prior failures", originalAction, fullCtx.RepeatedFailures[originalAction])
		result.NeedsHuman = true
	}

	// Apply confidence threshold.
	if result.Confidence < 0.7 {
		result.NeedsHuman = true
	}

	outputs := map[string]string{
		"status":           "success",
		"chosen_action":    result.ChosenAction,
		"confidence":       fmt.Sprintf("%.2f", result.Confidence),
		"reasoning":        result.Reasoning,
		"needs_human":      fmt.Sprintf("%t", result.NeedsHuman),
		"expected_outcome": result.ExpectedOutcome,
	}

	// Store expected_outcome on recovery bead metadata for downstream comparison.
	if result.ExpectedOutcome != "" && e.deps.SetBeadMetadata != nil {
		_ = e.deps.SetBeadMetadata(e.beadID, map[string]string{
			recovery.KeyExpectedOutcome: result.ExpectedOutcome,
		})
	}

	// If needs_human, write comment.
	if result.NeedsHuman {
		_ = e.deps.AddComment(e.beadID, fmt.Sprintf(
			"Recovery decide: needs human intervention.\n\nChosen action: %s\nConfidence: %.2f\nReasoning: %s\nExpected outcome: %s",
			result.ChosenAction, result.Confidence, result.Reasoning, result.ExpectedOutcome,
		))
		e.log("recovery: decide needs-human (confidence=%.2f, action=%s)", result.Confidence, result.ChosenAction)
	} else {
		e.log("recovery: decide chose %q (confidence=%.2f)", result.ChosenAction, result.Confidence)
	}

	return ActionResult{Outputs: outputs}
}

// parseHumanGuidance scans human comments for action keywords and returns
// the matching recovery action name, or "" if no guidance is detected.
// Avoids suggesting actions that have repeatedly failed.
func parseHumanGuidance(comments []string, repeatedFailures map[string]int) string {
	guidanceMap := map[string]string{
		"rebase":            "rebase-onto-main",
		"try rebase":        "rebase-onto-main",
		"rebase onto main":  "rebase-onto-main",
		"cherry-pick":       "cherry-pick",
		"cherry pick":       "cherry-pick",
		"resolve conflicts": "resolve-conflicts",
		"resolve conflict":  "resolve-conflicts",
		"rebuild":           "rebuild",
		"try rebuild":       "rebuild",
		"resummon":          "resummon",
		"re-summon":         "resummon",
		"try again":         "resummon",
		"reset":             "reset-to-step",
		"reset to step":     "reset-to-step",
		"escalate":          "escalate",
		"fix":               "targeted-fix",
		"targeted fix":      "targeted-fix",
	}

	// Scan comments from most recent to oldest.
	for i := len(comments) - 1; i >= 0; i-- {
		lower := strings.ToLower(strings.TrimSpace(comments[i]))
		for keyword, action := range guidanceMap {
			if strings.Contains(lower, keyword) {
				// Don't follow guidance toward an action that keeps failing.
				if repeatedFailures != nil && repeatedFailures[action] >= 2 {
					continue
				}
				return action
			}
		}
	}
	return ""
}

// decideFromGitState returns a recovery action based on git diagnostics,
// or "" if no clear action is indicated.
func decideFromGitState(ctx *FullRecoveryContext) string {
	// Behind main and diverged → rebase (diverged implies conflicts likely).
	if ctx.GitState != nil && ctx.GitState.Diverged {
		return "rebase-onto-main"
	}

	// Behind main → rebase.
	if ctx.GitState != nil && ctx.GitState.BehindMain > 0 {
		return "rebase-onto-main"
	}

	// Dirty worktree (uncommitted changes, likely broken build) → rebuild.
	if ctx.WorktreeState != nil && ctx.WorktreeState.Exists && ctx.WorktreeState.IsDirty {
		return "rebuild"
	}

	return ""
}

// gitStateReasoning returns a human-readable explanation of why a
// git-state-driven action was chosen.
func gitStateReasoning(ctx *FullRecoveryContext, action string) string {
	switch action {
	case "resolve-conflicts":
		return "worktree has merge conflicts"
	case "rebase-onto-main":
		if ctx.GitState != nil {
			return fmt.Sprintf("branch is %d commits behind %s", ctx.GitState.BehindMain, ctx.GitState.MainRef)
		}
		return "branch is behind main"
	case "rebuild":
		return "worktree has uncommitted changes (dirty)"
	default:
		return action
	}
}

// handleLearn calls Claude with the action taken and verify outcome to extract
// a learning. Writes to both bead metadata and the recovery_learnings table.
func handleLearn(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	// Read decide result and verify outcome from step outputs.
	var chosenAction, confidence, reasoning, verifyOutcome, failureClass, failureSig, expectedOutcome string
	if state != nil {
		if ds, ok := state.Steps["decide"]; ok {
			chosenAction = ds.Outputs["chosen_action"]
			confidence = ds.Outputs["confidence"]
			reasoning = ds.Outputs["reasoning"]
			expectedOutcome = ds.Outputs["expected_outcome"]
		}
		if vs, ok := state.Steps["verify"]; ok {
			verifyOutcome = vs.Outputs["verification_status"]
		}
		if cs, ok := state.Steps["collect_context"]; ok {
			failureClass = cs.Outputs["failure_class"]
		}
	}

	if verifyOutcome == "" {
		verifyOutcome = "unknown"
	}

	// Get failure signature from recovery bead metadata.
	if bead, err := e.deps.GetBead(e.beadID); err == nil {
		if failureClass == "" {
			failureClass = bead.Meta(recovery.KeyFailureClass)
		}
		failureSig = bead.Meta(recovery.KeyFailureSignature)
	}

	// Build Claude prompt.
	prompt := buildLearnPrompt(chosenAction, confidence, reasoning, verifyOutcome, failureClass, failureSig, expectedOutcome)

	// Call Claude.
	if e.deps.ClaudeRunner == nil {
		return ActionResult{Error: fmt.Errorf("learn: ClaudeRunner not available")}
	}

	args := []string{
		"--dangerously-skip-permissions",
		"-p", prompt,
		"--output-format", "text",
		"--max-turns", "1",
	}

	out, err := e.deps.ClaudeRunner(args, e.effectiveRepoPath())
	if err != nil {
		return ActionResult{Error: fmt.Errorf("learn: claude call failed: %w", err)}
	}

	var result LearnResult
	if err := parseJSONFromClaude(out, &result); err != nil {
		return ActionResult{Error: fmt.Errorf("learn: parse claude response: %w", err)}
	}

	now := time.Now().UTC()
	outcome := "clean"
	if verifyOutcome != "clean" {
		outcome = "dirty"
	}

	// 1. Write to bead metadata via existing path.
	metaMap := map[string]string{
		recovery.KeyLearningSummary: result.LearningSummary,
		recovery.KeyResolutionKind: result.ResolutionKind,
		recovery.KeyResolvedAt:     now.Format(time.RFC3339),
	}
	if result.Reusable {
		metaMap[recovery.KeyReusable] = "true"
	}
	metaMap[recovery.KeyVerificationStatus] = outcome
	if expectedOutcome != "" {
		metaMap[recovery.KeyExpectedOutcome] = expectedOutcome
	}
	if err := store.SetBeadMetadataMap(e.beadID, metaMap); err != nil {
		e.log("recovery: learn: write bead metadata: %s", err)
	}

	// 2. Write to recovery_learnings SQL table.
	sourceBeadID := resolveSourceBead(e, step)
	learningRow := store.RecoveryLearningRow{
		ID:              generateLearningID(),
		RecoveryBead:    e.beadID,
		SourceBead:      sourceBeadID,
		FailureClass:    failureClass,
		FailureSig:      failureSig,
		ResolutionKind:  result.ResolutionKind,
		Outcome:         outcome,
		LearningSummary: result.LearningSummary,
		Reusable:        result.Reusable,
		ResolvedAt:      now,
		ExpectedOutcome: expectedOutcome,
	}
	if err := store.WriteRecoveryLearningAuto(learningRow); err != nil {
		// Non-fatal: the bead metadata write is the primary record.
		e.log("recovery: learn: write to recovery_learnings table: %s", err)
	}

	e.log("recovery: learn: %s (resolution=%s, outcome=%s, reusable=%t)",
		result.LearningSummary, result.ResolutionKind, outcome, result.Reusable)

	return ActionResult{Outputs: map[string]string{
		"status":           "success",
		"learning_summary": result.LearningSummary,
		"resolution_kind":  result.ResolutionKind,
		"outcome":          outcome,
		"reusable":         fmt.Sprintf("%t", result.Reusable),
	}}
}

// handleFinish writes a closing comment, cleans up recovery protocol labels,
// and conditionally closes the recovery bead. If the decide step chose
// escalate, the bead is left open so `spire resolve` can find it and write
// the human learning before closing.
func handleFinish(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	// Gather summary from step outputs.
	var chosenAction, outcome, reasoning string
	var needsHuman bool
	if state != nil {
		if ds, ok := state.Steps["decide"]; ok {
			chosenAction = ds.Outputs["chosen_action"]
			reasoning = ds.Outputs["reasoning"]
			needsHuman = ds.Outputs["needs_human"] == "true"
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
// Git-aware execute, cooperative verify, and opcode handlers (spi-qrwof)
// ---------------------------------------------------------------------------

// handleGitAwareExecute routes execution through the git-aware recovery action
// registry (RunRecoveryAction) which handles worktree provisioning and
// per-attempt tracking. This is the counterpart to the legacy
// ExecuteRecoveryAction dispatch for the new action vocabulary.
func handleGitAwareExecute(e *Executor, stepName string, step StepConfig, state *GraphState, actionName string) ActionResult {
	sourceBeadID := resolveSourceBead(e, step)
	if sourceBeadID == "" {
		return ActionResult{Error: fmt.Errorf("execute: cannot resolve source bead")}
	}

	// Build params from decide step outputs and step.With.
	params := make(map[string]string)
	for k, v := range step.With {
		params[k] = v
	}
	// Inject decide step outputs as params for the action.
	if state != nil {
		if ds, ok := state.Steps["decide"]; ok {
			for k, v := range ds.Outputs {
				if _, exists := params[k]; !exists {
					params[k] = v
				}
			}
		}
	}

	repoPath := e.effectiveRepoPath()

	// Resolve Dolt DB handle for attempt tracking. Nil-safe: if DoltDB
	// dep is not wired (local execution), attempt tracking is gracefully skipped.
	var db *sql.DB
	if e.deps != nil && e.deps.DoltDB != nil {
		db = e.deps.DoltDB()
	}

	actionCtx := &RecoveryActionCtx{
		DB:             db,
		RepoPath:       repoPath,
		RecoveryBeadID: e.beadID,
		TargetBeadID:   sourceBeadID,
		Params:         params,
		Log:            func(msg string) { e.log("recovery: %s", msg) },
	}

	err := RunRecoveryAction(actionCtx, actionName)

	outputs := map[string]string{
		"status": "success",
		"action": actionName,
	}

	if err != nil {
		outputs["status"] = "failed"
		outputs["error"] = err.Error()
		e.log("recovery: execute %s failed: %v", actionName, err)
		return ActionResult{
			Outputs: outputs,
			Error:   fmt.Errorf("recovery action %q failed: %w", actionName, err),
		}
	}

	e.log("recovery: execute %s succeeded", actionName)
	return ActionResult{Outputs: outputs}
}

// handleVerify implements the cooperative retry loop between the recovery
// executor and the target bead's wizard.
//
// On execute success:
//  1. Set a RetryRequest on the target bead via SetRetryRequest
//  2. Poll GetRetryResult every 30 seconds
//  3. On success: clear labels, mark attempt success, proceed to learn
//  4. On failure: clear result, mark attempt failure, loop back to decide
//  5. On timeout (10 min): treat as failure, loop back to decide
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

	// Read human guidance from decide step if available.
	guidance := ""
	if state != nil {
		if ds, ok := state.Steps["decide"]; ok {
			guidance = ds.Outputs["reasoning"]
		}
	}

	// Set retry request on the target bead using the mapped wizard phase.
	retryReq := RetryRequest{
		RecoveryBeadID: e.beadID,
		TargetBeadID:   sourceBeadID,
		FromStep:       mappedStep,
		AttemptNumber:  attemptNumber,
		Guidance:       guidance,
	}

	if err := SetRetryRequest(sourceBeadID, retryReq); err != nil {
		e.log("recovery: verify: failed to set retry request on %s: %v", sourceBeadID, err)
		return ActionResult{
			Outputs: map[string]string{
				"status":  "failed",
				"loop_to": "decide",
				"reason":  fmt.Sprintf("set retry request failed: %v", err),
			},
			Error: fmt.Errorf("set retry request: %w", err),
		}
	}

	e.log("recovery: verify: retry request set on %s (from_step=%s, attempt=%d)", sourceBeadID, mappedStep, attemptNumber)

	// Polling loop: check for retry result every interval, up to timeout.
	// Uses context + ticker so the loop is cancellable if the executor shuts down.
	pollInterval := time.Duration(DefaultVerifyPollInterval) * time.Second
	timeout := time.Duration(DefaultVerifyTimeout) * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Timeout: clean up and loop back to decide.
			_ = ClearRetryRequest(sourceBeadID)
			e.log("recovery: verify: polling timed out after %s", timeout)
			return ActionResult{Outputs: map[string]string{
				"status":  "failed",
				"loop_to": "decide",
				"reason":  fmt.Sprintf("verify polling timed out after %s", timeout),
			}}
		case <-ticker.C:
			// Poll for result.
		}

		result, found, err := GetRetryResult(sourceBeadID)
		if err != nil {
			e.log("recovery: verify: poll error: %v", err)
			continue
		}
		if !found {
			continue // Still waiting
		}

		// Result received — process it.
		if result.Success {
			// Success: clean up labels and proceed to learn.
			_ = ClearRetryRequest(sourceBeadID)
			_ = ClearRetryResult(sourceBeadID)
			e.log("recovery: verify: retry succeeded (step_reached=%s)", result.StepReached)
			return ActionResult{Outputs: map[string]string{
				"status":       "success",
				"step_reached": result.StepReached,
			}}
		}

		// Failure: clean up result, loop back to decide with fresh context.
		_ = ClearRetryResult(sourceBeadID)
		e.log("recovery: verify: retry failed (failed_step=%s, error=%s)", result.FailedStep, result.Error)

		// Re-build recovery context for the next decide iteration.
		repoPath := e.effectiveRepoPath()
		var verifyDB *sql.DB
		if e.deps != nil && e.deps.DoltDB != nil {
			verifyDB = e.deps.DoltDB()
		}
		freshCtx, freshErr := BuildRecoveryContext(verifyDB, repoPath, e.beadID)
		if freshErr == nil {
			if freshJSON, jsonErr := json.Marshal(freshCtx); jsonErr == nil {
				if state.Vars == nil {
					state.Vars = make(map[string]string)
				}
				state.Vars["full_recovery_context"] = string(freshJSON)
			}
		}

		return ActionResult{Outputs: map[string]string{
			"status":      "failed",
			"loop_to":     "decide",
			"failed_step": result.FailedStep,
			"error":       result.Error,
			"reason":      "retry failed",
		}}
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

// buildDecidePrompt constructs the Claude prompt for the decide step.
// triageCount is the number of triage attempts already made on this recovery bead.
func buildDecidePrompt(cc CollectContextResult, triageCount int, stats *store.LearningStats) string {
	var b strings.Builder
	b.WriteString("You are a cleric agent for Spire, an AI agent coordination system.\n\n")
	b.WriteString("A bead (work item) has been interrupted and needs recovery. Analyze the diagnosis and choose the best recovery action.\n\n")

	// Diagnosis context.
	if cc.Diagnosis != nil {
		diagJSON, _ := json.MarshalIndent(cc.Diagnosis, "", "  ")
		b.WriteString("## Diagnosis\n```json\n")
		b.Write(diagJSON)
		b.WriteString("\n```\n\n")
	}

	// Wizard log output.
	if cc.WizardLogTail != "" {
		b.WriteString("## Wizard Log Output (last ~100 lines)\n")
		b.WriteString("This is the actual output from the wizard that failed. Use it to understand WHY the failure occurred.\n\n")
		b.WriteString("```\n")
		b.WriteString(cc.WizardLogTail)
		if !strings.HasSuffix(cc.WizardLogTail, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("```\n\n")
	}

	// Ranked actions.
	if len(cc.RankedActions) > 0 {
		b.WriteString("## Available Actions (mechanically ranked)\n```json\n")
		actJSON, _ := json.MarshalIndent(cc.RankedActions, "", "  ")
		b.Write(actJSON)
		b.WriteString("\n```\n\n")
	}

	// Historical outcome statistics.
	if stats != nil && stats.TotalRecoveries > 0 {
		b.WriteString("## Historical Outcome Statistics\n\n")
		b.WriteString(fmt.Sprintf("Based on %d prior recoveries for failure class `%s`:\n\n",
			stats.TotalRecoveries, stats.FailureClass))
		b.WriteString("| Action | Attempts | Success Rate | Clean | Dirty | Relapsed |\n")
		b.WriteString("|--------|----------|-------------|-------|-------|----------|\n")
		for _, as := range stats.ActionStats {
			b.WriteString(fmt.Sprintf("| %s | %d | %.0f%% | %d | %d | %d |\n",
				as.ResolutionKind, as.Total, as.SuccessRate*100,
				as.CleanCount, as.DirtyCount, as.RelapsedCount))
		}
		if stats.PredictionAccuracy > 0 {
			b.WriteString(fmt.Sprintf("\nDecide agent prediction accuracy: %.0f%% (when expected_outcome was set).\n",
				stats.PredictionAccuracy*100))
		}
		b.WriteString("\nWeight your action choice by historical success rates. Prefer actions with >70% success rate for this failure class unless the specific circumstances call for a different approach.\n\n")
	}

	// Bead learnings.
	if len(cc.BeadLearnings) > 0 {
		b.WriteString("## Prior experience with this exact bead\n```json\n")
		blJSON, _ := json.MarshalIndent(cc.BeadLearnings, "", "  ")
		b.Write(blJSON)
		b.WriteString("\n```\n\n")
	}

	// Cross-bead learnings.
	if len(cc.CrossLearnings) > 0 {
		b.WriteString("## Similar incidents across the system (lower weight)\n```json\n")
		clJSON, _ := json.MarshalIndent(cc.CrossLearnings, "", "  ")
		b.Write(clJSON)
		b.WriteString("\n```\n\n")
	}

	b.WriteString("## Instructions\n")
	b.WriteString("Choose a recovery action. Output ONLY a JSON object with these fields:\n")
	b.WriteString("- `chosen_action`: one of \"reset\", \"resummon\", \"do_nothing\", \"escalate\", \"reset_to_step\", \"verify_clean\", \"triage\"\n")
	b.WriteString("- `confidence`: 0.0 to 1.0 — how confident you are this action will resolve the issue\n")
	b.WriteString("- `reasoning`: brief explanation of why you chose this action\n")
	b.WriteString("- `needs_human`: set to true if confidence < 0.7\n")
	b.WriteString("- `expected_outcome`: what you expect to happen after this action is taken. Include: what should succeed, how to verify it worked, and under what conditions this action would be the wrong choice.\n\n")

	// Log-referencing reasoning requirements.
	if cc.WizardLogTail != "" {
		b.WriteString("### CRITICAL: Log-Based Reasoning\n")
		b.WriteString("Wizard log output is available above. Your reasoning MUST reference specific errors or output lines from the log.\n")
		b.WriteString("Distinguish between:\n")
		b.WriteString("- **Infrastructure failures** (missing commands, env setup, dependency install) that resummon/reset may fix\n")
		b.WriteString("- **Code-level failures** (test assertions, missing env vars, type errors) that require code changes and resummon CANNOT fix\n\n")
	}

	// Triage guidance.
	b.WriteString("### Triage Action\n")
	b.WriteString("Choose `triage` when:\n")
	b.WriteString("- Failure class is `step-failure` at implement or review-fix steps\n")
	b.WriteString("- Test output shows clear code-level failures (assertion errors, type errors, compilation errors)\n")
	b.WriteString("- The worktree still exists (see diagnosis git state)\n")
	b.WriteString("- This is the first or second triage attempt (max 2)\n\n")
	b.WriteString("Do NOT choose `triage` for:\n")
	b.WriteString("- Infrastructure failures (missing commands, env setup, dependency install)\n")
	b.WriteString("- When the worktree has been cleaned up\n")
	b.WriteString("- When triage has already been tried twice\n\n")

	// Triage budget context.
	triageRemaining := 2 - triageCount
	if triageRemaining < 0 {
		triageRemaining = 0
	}
	b.WriteString(fmt.Sprintf("**Triage budget:** %d of 2 attempts used, %d remaining.\n", triageCount, triageRemaining))
	if triageCount >= 2 {
		b.WriteString("Triage budget is exhausted — do NOT choose `triage`.\n")
	}
	b.WriteString("\n")

	// Worktree existence from diagnosis.
	if cc.Diagnosis != nil && cc.Diagnosis.Git != nil {
		if cc.Diagnosis.Git.WorktreeExists {
			b.WriteString("**Worktree exists:** yes (triage is possible)\n\n")
		} else {
			b.WriteString("**Worktree exists:** no (triage is NOT possible — worktree was cleaned up)\n\n")
		}
	}

	// Relapse awareness.
	b.WriteString("### Relapse Awareness\n")
	b.WriteString("If prior learnings show outcome=\"relapsed\" for an action on this bead, do NOT choose that action again unless you can explain from the log why this time is different.\n")
	b.WriteString("A \"relapsed\" outcome means a prior recovery with that action appeared to succeed but the bead failed again with the same failure class within 24 hours.\n\n")

	b.WriteString("### Action Guide\n")
	b.WriteString("- `resummon`: Soft reset — clears interrupt labels, sets bead to open. Wizard resumes on the SAME branch with existing code. Use for transient/infrastructure failures.\n")
	b.WriteString("- `reset-hard`: Destructive reset — kills wizard, deletes worktree, branches, graph state, and internal DAG beads. Fresh start from scratch. Use when code is fundamentally broken and resuming would repeat the same mistakes.\n")
	b.WriteString("- `reset_to_step`: Rewinds to a specific step. Use for targeted re-execution of a single phase.\n")
	b.WriteString("- `escalate`: Marks for human intervention. Use when confidence is low or the problem is outside agent capability.\n")
	b.WriteString("- `do_nothing`: Valid if the source bead already appears clean.\n\n")
	b.WriteString("Output ONLY the JSON object, no markdown fences, no explanation outside the JSON.\n")

	return b.String()
}

// buildLearnPrompt constructs the Claude prompt for the learn step.
func buildLearnPrompt(chosenAction, confidence, reasoning, verifyOutcome, failureClass, failureSig, expectedOutcome string) string {
	var b strings.Builder
	b.WriteString("You are a cleric learning agent for Spire, an AI agent coordination system.\n\n")
	b.WriteString("A recovery action was taken. Analyze the result and extract a learning for future incidents.\n\n")

	b.WriteString("## Recovery Context\n")
	b.WriteString(fmt.Sprintf("- Chosen action: %s\n", chosenAction))
	b.WriteString(fmt.Sprintf("- Confidence: %s\n", confidence))
	b.WriteString(fmt.Sprintf("- Reasoning: %s\n", reasoning))
	b.WriteString(fmt.Sprintf("- Verify outcome: %s\n", verifyOutcome))
	b.WriteString(fmt.Sprintf("- Failure class: %s\n", failureClass))
	if failureSig != "" {
		b.WriteString(fmt.Sprintf("- Failure signature: %s\n", failureSig))
	}
	b.WriteString("\n")

	if expectedOutcome != "" {
		b.WriteString("## Expected Outcome (from decide step)\n")
		b.WriteString(fmt.Sprintf("The decide agent predicted: %s\n\n", expectedOutcome))
	}

	b.WriteString("## Instructions\n")
	b.WriteString("Output ONLY a JSON object with these fields:\n")
	b.WriteString("- `learning_summary`: concise description of what happened and what worked or didn't\n")
	b.WriteString("- `resolution_kind`: one of \"reset\", \"resummon\", \"do_nothing\", \"escalate\", \"reset_to_step\", \"verify_clean\", \"triage\"\n")
	b.WriteString("- `reusable`: true if this learning applies to future similar failures, false otherwise\n\n")

	if expectedOutcome != "" {
		b.WriteString("Compare the actual outcome against the expected outcome. If they diverge, note this in the learning summary.\n\n")
	}

	b.WriteString("Output ONLY the JSON object, no markdown fences, no explanation outside the JSON.\n")

	return b.String()
}

// parseJSONFromClaude extracts a JSON object from Claude's output, handling
// potential markdown fences and surrounding text.
func parseJSONFromClaude(out []byte, v interface{}) error {
	text := strings.TrimSpace(string(out))

	// Strip markdown fences if present.
	if strings.HasPrefix(text, "```") {
		lines := strings.Split(text, "\n")
		var jsonLines []string
		inBlock := false
		for _, line := range lines {
			if strings.HasPrefix(line, "```") {
				inBlock = !inBlock
				continue
			}
			if inBlock {
				jsonLines = append(jsonLines, line)
			}
		}
		text = strings.Join(jsonLines, "\n")
	}

	// Find JSON object boundaries.
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start >= 0 && end > start {
		text = text[start : end+1]
	}

	return json.Unmarshal([]byte(text), v)
}

// generateLearningID creates a random ID for a recovery learning row.
func generateLearningID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("rl-%d", time.Now().UnixNano())
	}
	return "rl-" + hex.EncodeToString(b)
}
