package executor

import (
	"fmt"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
)

func init() {
	actionRegistry["recovery.execute"] = actionRecoveryExecute
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
