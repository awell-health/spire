package executor

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
)

func init() {
	actionRegistry["recovery.execute"] = actionRecoveryExecute
	actionRegistry["recovery.decide"] = actionRecoveryDecide
	actionRegistry["recovery.learn"] = actionRecoveryLearn
	actionRegistry["recovery.collect_context"] = actionRecoveryCollectContext
}

// actionRecoveryExecute is the ActionHandler for the "recovery.execute" opcode.
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
func actionRecoveryExecute(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	actionKind := step.With["action"]
	if actionKind == "" {
		return ActionResult{Error: fmt.Errorf("recovery.execute step %q missing required with.action", stepName)}
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

	req := recovery.RecoveryActionRequest{
		Kind:         recovery.RecoveryActionKind(actionKind),
		BeadID:       e.beadID,
		SourceBeadID: sourceBeadID,
		StepTarget:   step.With["step_target"],
		Params:       step.With,
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
		return doReset(e, req)
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
	default:
		return recovery.RecoveryActionResult{
			Kind:    req.Kind,
			Success: false,
			Error:   fmt.Sprintf("unhandled recovery action kind: %q", req.Kind),
		}
	}
}

// doReset performs a hard reset on the source bead: clears interrupt labels,
// removes needs-human, and sets status back to open. The bead is then available
// for re-assignment by the steward.
func doReset(e *Executor, req recovery.RecoveryActionRequest) recovery.RecoveryActionResult {
	if req.SourceBeadID == "" {
		return failResult(req.Kind, "source_bead_id is required for reset")
	}

	bead, err := e.deps.GetBead(req.SourceBeadID)
	if err != nil {
		return failResult(req.Kind, fmt.Sprintf("get source bead: %v", err))
	}

	// Remove interrupt and needs-human labels.
	for _, l := range bead.Labels {
		if strings.HasPrefix(l, "interrupted:") {
			_ = e.deps.RemoveLabel(req.SourceBeadID, l)
		}
	}
	_ = e.deps.RemoveLabel(req.SourceBeadID, "needs-human")

	// Set source bead back to open.
	if err := e.deps.UpdateBead(req.SourceBeadID, map[string]interface{}{"status": "open"}); err != nil {
		return failResult(req.Kind, fmt.Sprintf("update source bead status: %v", err))
	}

	e.log("recovery: reset %s to open", req.SourceBeadID)

	return recovery.RecoveryActionResult{
		Kind:           req.Kind,
		Success:        true,
		Output:         fmt.Sprintf("reset %s to open", req.SourceBeadID),
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

	bead, err := e.deps.GetBead(req.SourceBeadID)
	if err != nil {
		return failResult(req.Kind, fmt.Sprintf("get source bead: %v", err))
	}

	// Remove interrupt labels from source bead.
	for _, l := range bead.Labels {
		if strings.HasPrefix(l, "interrupted:") {
			_ = e.deps.RemoveLabel(req.SourceBeadID, l)
		}
	}
	_ = e.deps.RemoveLabel(req.SourceBeadID, "needs-human")

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

	bead, err := e.deps.GetBead(req.SourceBeadID)
	if err != nil {
		return failResult(req.Kind, fmt.Sprintf("get source bead: %v", err))
	}

	// Remove interrupt labels.
	for _, l := range bead.Labels {
		if strings.HasPrefix(l, "interrupted:") {
			_ = e.deps.RemoveLabel(req.SourceBeadID, l)
		}
	}
	_ = e.deps.RemoveLabel(req.SourceBeadID, "needs-human")

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

// doVerifyClean checks bead health for the source bead: interrupt labels,
// needs-human status, and whether the bead is still active.
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

	// Check for interrupt labels.
	for _, l := range bead.Labels {
		if strings.HasPrefix(l, "interrupted:") {
			issues = append(issues, "has interrupt label: "+l)
		}
	}

	// Check for needs-human.
	for _, l := range bead.Labels {
		if l == "needs-human" {
			issues = append(issues, "has needs-human label")
			break
		}
	}

	// Check bead status.
	if bead.Status == "closed" {
		issues = append(issues, "bead is closed")
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

// doEscalate marks the recovery bead as requiring human intervention. Adds a
// needs-human label, writes an escalation comment, and does NOT close the bead.
func doEscalate(e *Executor, req recovery.RecoveryActionRequest) recovery.RecoveryActionResult {
	// Add needs-human label to the recovery bead.
	_ = e.deps.AddLabel(req.BeadID, "needs-human")

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
// delegate to existing mechanical handlers; finish always closes the bead.
// ---------------------------------------------------------------------------

// CollectContextResult is the structured output of the collect_context step.
type CollectContextResult struct {
	Diagnosis      *recovery.Diagnosis       `json:"diagnosis"`
	RankedActions  []recovery.RecoveryAction `json:"ranked_actions"`
	BeadLearnings  []store.RecoveryLearning  `json:"bead_learnings"`
	CrossLearnings []store.RecoveryLearning  `json:"cross_bead_learnings"`
}

// DecideResult is the structured output of the decide step (Claude response).
type DecideResult struct {
	ChosenAction string  `json:"chosen_action"`
	Confidence   float64 `json:"confidence"`
	Reasoning    string  `json:"reasoning"`
	NeedsHuman   bool    `json:"needs_human"`
}

// LearnResult is the structured output of the learn step (Claude response).
type LearnResult struct {
	LearningSummary string `json:"learning_summary"`
	ResolutionKind  string `json:"resolution_kind"`
	Reusable        bool   `json:"reusable"`
}

// actionRecoveryLearn is the ActionHandler for the "recovery.learn" opcode.
// Delegates to handleLearn for the agentic v3 recovery formula.
func actionRecoveryLearn(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	return handleLearn(e, stepName, step, state)
}
// handleCollectContext assembles diagnosis JSON + prior learnings from the
// recovery_learnings table (per-bead and cross-bead). This is mechanical —
// no Claude call. Writes CollectContextResult JSON to step outputs.
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
	if diagErr == nil && diag != nil {
		rankedActions = diag.Actions
		if failureClass == "" {
			failureClass = string(diag.FailureMode)
		}
	} else {
		e.log("recovery: collect_context diagnosis failed (continuing with partial context): %v", diagErr)
	}

	// Query per-bead learnings via bead metadata (existing approach).
	beadLearnings := queryBeadLearnings(sourceBeadID, failureClass)

	// Query cross-bead learnings.
	crossLearnings := queryCrossBeadLearnings(failureClass, 5)

	result := CollectContextResult{
		Diagnosis:      diag,
		RankedActions:  rankedActions,
		BeadLearnings:  beadLearnings,
		CrossLearnings: crossLearnings,
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

	return ActionResult{Outputs: outputs}
}

// handleDecide calls Claude with the collect_context result to choose a
// recovery action. Outputs chosen_action, confidence, reasoning, needs_human.
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

	// Build Claude prompt.
	prompt := buildDecidePrompt(ccResult)

	// Call Claude.
	if e.deps.ClaudeRunner == nil {
		return ActionResult{Error: fmt.Errorf("decide: ClaudeRunner not available")}
	}

	args := []string{
		"--dangerously-skip-permissions",
		"-p", prompt,
		"--output-format", "text",
		"--max-turns", "1",
	}

	out, err := e.deps.ClaudeRunner(args, e.effectiveRepoPath())
	if err != nil {
		return ActionResult{Error: fmt.Errorf("decide: claude call failed: %w", err)}
	}

	// Parse Claude's JSON response.
	var result DecideResult
	if err := parseJSONFromClaude(out, &result); err != nil {
		return ActionResult{Error: fmt.Errorf("decide: parse claude response: %w", err)}
	}

	// Apply confidence threshold.
	if result.Confidence < 0.7 {
		result.NeedsHuman = true
	}

	outputs := map[string]string{
		"status":        "success",
		"chosen_action": result.ChosenAction,
		"confidence":    fmt.Sprintf("%.2f", result.Confidence),
		"reasoning":     result.Reasoning,
		"needs_human":   fmt.Sprintf("%t", result.NeedsHuman),
	}

	// If needs_human, set bead status and write comment.
	if result.NeedsHuman {
		_ = e.deps.AddLabel(e.beadID, "needs-human")
		_ = e.deps.AddComment(e.beadID, fmt.Sprintf(
			"Recovery decide: needs human intervention.\n\nChosen action: %s\nConfidence: %.2f\nReasoning: %s",
			result.ChosenAction, result.Confidence, result.Reasoning,
		))
		e.log("recovery: decide needs-human (confidence=%.2f, action=%s)", result.Confidence, result.ChosenAction)
	} else {
		e.log("recovery: decide chose %q (confidence=%.2f)", result.ChosenAction, result.Confidence)
	}

	return ActionResult{Outputs: outputs}
}

// handleLearn calls Claude with the action taken and verify outcome to extract
// a learning. Writes to both bead metadata and the recovery_learnings table.
func handleLearn(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	// Read decide result and verify outcome from step outputs.
	var chosenAction, confidence, reasoning, verifyOutcome, failureClass, failureSig string
	if state != nil {
		if ds, ok := state.Steps["decide"]; ok {
			chosenAction = ds.Outputs["chosen_action"]
			confidence = ds.Outputs["confidence"]
			reasoning = ds.Outputs["reasoning"]
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
	prompt := buildLearnPrompt(chosenAction, confidence, reasoning, verifyOutcome, failureClass, failureSig)

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

// handleFinish closes the recovery bead unconditionally. Writes a closing
// comment summarizing the action taken and outcome.
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
				if vs.Outputs["verification_status"] == "clean" {
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

	// Close recovery bead unconditionally.
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

// queryBeadLearnings queries per-bead learnings from closed recovery beads.
// Uses the existing bead-metadata approach via store.ListClosedRecoveryBeads.
func queryBeadLearnings(sourceBeadID, failureClass string) []store.RecoveryLearning {
	reusable := true
	filter := store.RecoveryLookupFilter{
		SourceBead:   sourceBeadID,
		FailureClass: failureClass,
		Reusable:     &reusable,
		Limit:        10,
	}
	learnings, err := store.ListClosedRecoveryBeads(filter)
	if err != nil {
		return nil
	}
	return learnings
}

// queryCrossBeadLearnings queries cross-bead learnings for a failure class.
func queryCrossBeadLearnings(failureClass string, limit int) []store.RecoveryLearning {
	reusable := true
	filter := store.RecoveryLookupFilter{
		FailureClass: failureClass,
		Reusable:     &reusable,
		Limit:        limit,
	}
	learnings, err := store.ListClosedRecoveryBeads(filter)
	if err != nil {
		return nil
	}
	return learnings
}

// buildDecidePrompt constructs the Claude prompt for the decide step.
func buildDecidePrompt(cc CollectContextResult) string {
	var b strings.Builder
	b.WriteString("You are a recovery decision agent for Spire, an AI agent coordination system.\n\n")
	b.WriteString("A bead (work item) has been interrupted and needs recovery. Analyze the diagnosis and choose the best recovery action.\n\n")

	// Diagnosis context.
	if cc.Diagnosis != nil {
		diagJSON, _ := json.MarshalIndent(cc.Diagnosis, "", "  ")
		b.WriteString("## Diagnosis\n```json\n")
		b.Write(diagJSON)
		b.WriteString("\n```\n\n")
	}

	// Ranked actions.
	if len(cc.RankedActions) > 0 {
		b.WriteString("## Available Actions (mechanically ranked)\n```json\n")
		actJSON, _ := json.MarshalIndent(cc.RankedActions, "", "  ")
		b.Write(actJSON)
		b.WriteString("\n```\n\n")
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
	b.WriteString("- `chosen_action`: one of \"reset\", \"resummon\", \"do_nothing\", \"escalate\", \"reset_to_step\", \"verify_clean\"\n")
	b.WriteString("- `confidence`: 0.0 to 1.0 — how confident you are this action will resolve the issue\n")
	b.WriteString("- `reasoning`: brief explanation of why you chose this action\n")
	b.WriteString("- `needs_human`: set to true if confidence < 0.7\n\n")
	b.WriteString("\"do_nothing\" is valid if the source bead already appears clean.\n\n")
	b.WriteString("Output ONLY the JSON object, no markdown fences, no explanation outside the JSON.\n")

	return b.String()
}

// buildLearnPrompt constructs the Claude prompt for the learn step.
func buildLearnPrompt(chosenAction, confidence, reasoning, verifyOutcome, failureClass, failureSig string) string {
	var b strings.Builder
	b.WriteString("You are a recovery learning agent for Spire, an AI agent coordination system.\n\n")
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

	b.WriteString("## Instructions\n")
	b.WriteString("Output ONLY a JSON object with these fields:\n")
	b.WriteString("- `learning_summary`: concise description of what happened and what worked or didn't\n")
	b.WriteString("- `resolution_kind`: one of \"reset\", \"resummon\", \"do_nothing\", \"escalate\", \"reset_to_step\", \"verify_clean\"\n")
	b.WriteString("- `reusable`: true if this learning applies to future similar failures, false otherwise\n\n")
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
