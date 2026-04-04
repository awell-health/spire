package executor

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/repoconfig"
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

// learnOutput is the structured JSON output from the Claude learn prompt.
type learnOutput struct {
	LearningSummary string `json:"learning_summary"`
	ResolutionKind  string `json:"resolution_kind"`
	Reusable        bool   `json:"reusable"`
}

// actionRecoveryLearn is the ActionHandler for the "recovery.learn" opcode.
// It calls Claude to generate a learning summary from the recovery workflow's
// prior steps, then writes the learning to both bead metadata (narrative layer)
// and the recovery_learnings table (index layer for cross-bead queries).
//
// Best-effort: if Claude fails, returns a warning output rather than an Error
// so the graph can proceed to the finish step and close the bead.
func actionRecoveryLearn(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	// 1. Extract inputs from prior step outputs.
	sourceBead := stepOutput(state, "collect_context", "source_bead_id")
	failureClass := stepOutput(state, "collect_context", "failure_class")
	failureSig := stepOutput(state, "collect_context", "failure_sig")
	actionTaken := stepOutput(state, "remediate", "action")
	if actionTaken == "" {
		actionTaken = stepOutput(state, "remediate", "resolution_kind")
	}
	verifyOutcome := stepOutput(state, "verify", "verification_status")
	recoveryBeadID := e.beadID

	// Fall back to bead metadata for values not in step outputs.
	if sourceBead == "" || failureClass == "" {
		if bead, err := e.deps.GetBead(recoveryBeadID); err == nil {
			if sourceBead == "" {
				sourceBead = bead.Meta(recovery.KeySourceBead)
			}
			if failureClass == "" {
				failureClass = bead.Meta(recovery.KeyFailureClass)
			}
			if failureSig == "" {
				failureSig = bead.Meta(recovery.KeyFailureSignature)
			}
		}
	}

	// 2. Build Claude prompt.
	prompt := buildLearnPrompt(sourceBead, failureClass, failureSig, actionTaken, verifyOutcome)

	// 3. Call Claude (single-shot).
	model := repoconfig.ResolveModel(step.Model, e.repoModel())
	args := []string{
		"--dangerously-skip-permissions",
		"-p", prompt,
		"--model", model,
		"--output-format", "text",
		"--max-turns", "1",
	}
	raw, err := e.deps.ClaudeRunner(args, e.effectiveRepoPath())
	if err != nil {
		e.log("warning: learn step claude call failed: %s", err)
		return ActionResult{Outputs: map[string]string{
			"status":  "warning",
			"warning": fmt.Sprintf("claude call failed: %s", err),
		}}
	}

	// 4. Parse structured JSON output.
	var out learnOutput
	rawStr := strings.TrimSpace(string(raw))
	// Extract JSON from Claude response (may be wrapped in markdown code blocks).
	jsonStr := extractJSON(rawStr)
	if err := json.Unmarshal([]byte(jsonStr), &out); err != nil {
		e.log("warning: learn step parse output failed: %s (raw: %s)", err, rawStr)
		// Fall back to reasonable defaults from available data.
		out.LearningSummary = rawStr
		out.ResolutionKind = actionTaken
		out.Reusable = verifyOutcome == "clean"
	}

	// Override reusable based on verify outcome: dirty outcomes are not reusable.
	if verifyOutcome == "dirty" {
		out.Reusable = false
	}

	// 5. Write to bead metadata (narrative layer).
	meta := map[string]string{
		recovery.KeyLearningSummary: out.LearningSummary,
		recovery.KeyResolutionKind:  out.ResolutionKind,
		recovery.KeyOutcome:         verifyOutcome,
		recovery.KeyReusable:        strconv.FormatBool(out.Reusable),
		recovery.KeyResolvedAt:      time.Now().UTC().Format(time.RFC3339),
	}
	if err := store.SetBeadMetadataMap(recoveryBeadID, meta); err != nil {
		e.log("warning: learn step write metadata failed: %s", err)
	}

	// 6. Write to recovery_learnings table (index layer).
	// Best-effort: if this fails but metadata succeeded, continue to finish.
	if e.deps.WriteRecoveryLearning != nil {
		l := store.RecoveryLearningRecord{
			RecoveryBead:    recoveryBeadID,
			SourceBead:      sourceBead,
			FailureClass:    failureClass,
			FailureSig:      failureSig,
			ResolutionKind:  out.ResolutionKind,
			Outcome:         verifyOutcome,
			LearningSummary: out.LearningSummary,
			Reusable:        out.Reusable,
		}
		learningID, writeErr := e.deps.WriteRecoveryLearning(l)
		if writeErr != nil {
			e.log("warning: learn step write table failed: %s", writeErr)
		} else {
			// Write the learning ID back to metadata for cross-referencing.
			_ = store.SetBeadMetadata(recoveryBeadID, recovery.KeyLearningID, learningID)
		}
	}

	return ActionResult{Outputs: map[string]string{
		"resolution_kind":  out.ResolutionKind,
		"outcome":          verifyOutcome,
		"learning_summary": out.LearningSummary,
	}}
}

// stepOutput reads a named output from a prior step's state.
func stepOutput(state *GraphState, stepName, key string) string {
	if state == nil || state.Steps == nil {
		return ""
	}
	ss, ok := state.Steps[stepName]
	if !ok {
		return ""
	}
	if ss.Outputs == nil {
		return ""
	}
	return ss.Outputs[key]
}

// extractJSON extracts a JSON object from a string that may contain markdown
// code fences or surrounding text. Returns the original string if no JSON found.
func extractJSON(s string) string {
	// Try to find JSON object directly.
	if start := strings.Index(s, "{"); start >= 0 {
		if end := strings.LastIndex(s, "}"); end > start {
			return s[start : end+1]
		}
	}
	return s
}

// buildLearnPrompt constructs the Claude prompt for the learn step.
func buildLearnPrompt(sourceBead, failureClass, failureSig, actionTaken, verifyOutcome string) string {
	return fmt.Sprintf(`You are analyzing a recovery workflow outcome to generate a learning record.

## Recovery Context
- Source bead: %s
- Failure class: %s
- Failure signature: %s
- Action taken: %s
- Verify outcome: %s

## Instructions

Based on the recovery context above, produce a concise learning record. Respond with ONLY a JSON object (no markdown, no code fences, no explanation):

{
  "learning_summary": "<concise narrative: what failed, what was tried, what happened — 1-3 sentences>",
  "resolution_kind": "<one of: resummon, reset, do-nothing, escalated>",
  "reusable": <true if the action worked (outcome=clean), false if it didn't (outcome=dirty)>
}

Rules:
- learning_summary should be useful to future recovery workflows encountering the same failure class
- resolution_kind should match the action that was actually taken
- Set reusable to false when the outcome is "dirty" (the action didn't resolve the issue)
- Be factual and brief — this is a machine-readable record, not a report`, sourceBead, failureClass, failureSig, actionTaken, verifyOutcome)
}
