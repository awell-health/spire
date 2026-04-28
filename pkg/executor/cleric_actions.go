package executor

import (
	"github.com/awell-health/spire/pkg/cleric"
	"github.com/awell-health/spire/pkg/store"
)

// init registers the mechanical cleric.* action handlers in the
// executor's action registry. The handler logic lives in pkg/cleric so
// the package is testable independently of pkg/executor; the adapter
// functions below translate the executor's handler signature to the
// cleric package's stand-alone API.
//
// Cleric runtime (spi-hhkozk); cleric.reject + auto-approve fast-path
// added by spi-kl8x5y.
func init() {
	actionRegistry["cleric.publish"] = actionClericPublish
	actionRegistry["cleric.execute"] = actionClericExecute
	actionRegistry["cleric.takeover"] = actionClericTakeover
	actionRegistry["cleric.finish"] = actionClericFinish
	actionRegistry["cleric.reject"] = actionClericReject
}

// actionClericPublish parses the cleric Claude agent's stdout (the
// "result" output of the prior decide step) into a ProposedAction,
// persists it on the recovery bead, and transitions the bead to
// awaiting_review.
//
// Auto-approve fast-path (spi-kl8x5y): if Publish reports
// auto_approved=true (because IsPromoted matched the
// (failure_class, action) pair), this adapter pre-writes the
// downstream wait_for_gate step's outputs (gate=approve,
// rejection_comment=auto-approved). The formula's interpreter then
// completes wait_for_gate without parking and dispatches cleric.execute
// synchronously, so the recovery bead never enters awaiting_review.
func actionClericPublish(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	decideOut := lookupDecideOutput(state)
	res := cleric.Publish(e.beadID, decideOut, clericDepsFromExecutor(e))
	if res.Outputs["auto_approved"] == "true" && state != nil {
		preWriteWaitGateApproved(state, res.Outputs)
	}
	return clericResultToActionResult(res)
}

// preWriteWaitGateApproved seeds the wait_for_gate step's outputs with
// gate=approve + rejection_comment=auto-approved so the formula's
// waitStepResult observes all produced keys filled and completes the
// step on the next interpreter pass. The step's status is left at
// pending — the interpreter is responsible for advancing it once it
// sees the outputs.
func preWriteWaitGateApproved(state *GraphState, fromPublish map[string]string) {
	const stepName = "wait_for_gate"
	ss := state.Steps[stepName]
	if ss.Outputs == nil {
		ss.Outputs = map[string]string{}
	}
	gate := fromPublish["gate"]
	if gate == "" {
		gate = "approve"
	}
	rc := fromPublish["rejection_comment"]
	if rc == "" {
		rc = "auto-approved"
	}
	ss.Outputs["gate"] = gate
	ss.Outputs["rejection_comment"] = rc
	state.Steps[stepName] = ss
}

// actionClericExecute reads the persisted proposal from the recovery
// bead and dispatches it through the gateway client. Records the
// outcome on the bead. Always returns a normal completion (errors are
// recorded via metadata) unless the bead itself can't be loaded — the
// design wants a stub-gateway run to leave the bead in a state the
// human can manually take over from, not park a step.
func actionClericExecute(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	res := cleric.Execute(e.beadID, clericDepsFromExecutor(e))
	return clericResultToActionResult(res)
}

// actionClericTakeover applies the human-takeover semantics: source
// bead stays hooked + needs-manual label, recovery bead closes.
func actionClericTakeover(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	res := cleric.Takeover(e.beadID, clericDepsFromExecutor(e))
	return clericResultToActionResult(res)
}

// actionClericFinish records the cleric outcome (verb + failure class +
// execute result) on the recovery bead's metadata for the
// promotion/demotion learning loop, then closes the recovery bead.
func actionClericFinish(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	res := cleric.Finish(e.beadID, clericDepsFromExecutor(e))
	return clericResultToActionResult(res)
}

// actionClericReject records the rejection outcome on the recovery
// bead's cleric_outcomes row so the promotion/demotion learning loop
// sees the signal. Replaces the legacy noop action on
// requeue_after_reject. The formula's resets directive on that step
// still drives the requeue (decide / publish / wait_for_gate back to
// pending) — this handler just adds outcome bookkeeping.
func actionClericReject(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	res := cleric.Reject(e.beadID, clericDepsFromExecutor(e))
	return clericResultToActionResult(res)
}

// clericDepsFromExecutor projects the executor's Deps onto the
// cleric.Deps surface, plus the package-level gateway client and the
// learning store / outcome recorder.
func clericDepsFromExecutor(e *Executor) cleric.Deps {
	return cleric.Deps{
		GetBead:         e.deps.GetBead,
		SetBeadMetadata: e.deps.SetBeadMetadata,
		UpdateBead:      e.deps.UpdateBead,
		AddLabel:        e.deps.AddLabel,
		AddComment:      e.deps.AddComment,
		CloseBead:       e.deps.CloseBead,
		GetDepsWithMeta: e.deps.GetDepsWithMeta,
		Gateway:         cleric.DefaultGatewayClient,
		Learning:        cleric.DefaultLearning,
		RecordOutcome:   store.RecordClericOutcomeAuto,
	}
}

// clericResultToActionResult translates a cleric.HandlerResult into an
// ActionResult. The mapping is mechanical — outputs flow through, the
// error becomes ActionResult.Error which the formula engine treats per
// the step's on_error directive.
func clericResultToActionResult(res cleric.HandlerResult) ActionResult {
	return ActionResult{
		Outputs: res.Outputs,
		Error:   res.Err,
	}
}

// lookupDecideOutput returns the "result" field from the decide step's
// outputs. The wizard.run dispatcher writes the agent's stdout into the
// generic "result" key (see actionWizardRun's review-fix promotion
// pattern) — cleric reuses that surface so we don't need a bespoke
// stdout-passing channel between Claude and cleric.publish. The
// formula's decide step is named "decide" by convention (the cleric-
// default formula entry point), so we read steps.decide.outputs.result.
func lookupDecideOutput(state *GraphState) string {
	if state == nil {
		return ""
	}
	ss, ok := state.Steps["decide"]
	if !ok {
		return ""
	}
	if ss.Outputs == nil {
		return ""
	}
	if v, ok := ss.Outputs["result"]; ok {
		return v
	}
	return ss.Outputs["stdout"]
}

// finalizeClericOutcomesForStep runs the cleric promotion/demotion
// observer for the wizard's bead. Pending cleric_outcomes rows whose
// target_step matches stepName get finalized with the supplied success
// flag. Empty target_step rows are wildcard-matched on any step
// completion. Errors are silent — observer failures must never crash
// the wizard interpreter.
//
// Called from RunGraph after a step transitions to completed status. A
// recovery bead's executor is also a wizard run from the interpreter's
// perspective; pending rows are keyed on source_bead_id (set at
// cleric.finish via the recovery bead's caused-by edge), so calling
// this on a recovery bead is a harmless no-op.
//
// Cleric promotion/demotion (spi-kl8x5y).
func (e *Executor) finalizeClericOutcomesForStep(stepName string, success bool) {
	if e == nil || e.beadID == "" {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			e.log("warning: cleric outcome finalize panic on %s: %v", stepName, r)
		}
	}()
	cleric.FinalizePendingOutcomes(e.beadID, stepName, success, cleric.DefaultObserver)
}

// clericReadStoreDeps wires the package-level cleric.Deps for callers
// outside the executor (e.g. cmd/spire's wizard summon, which calls
// cleric.HasOpenRecovery directly). Production wires this in cmd/spire.
// Tests overwrite via the package var. Kept as a small helper so we
// don't repeat the pkg/store wiring in three places.
func clericReadStoreDeps() cleric.Deps {
	return cleric.Deps{
		GetBead:         store.GetBead,
		SetBeadMetadata: store.SetBeadMetadataMap,
		UpdateBead:      store.UpdateBead,
		AddLabel:        store.AddLabel,
		AddComment:      store.AddComment,
		CloseBead:       store.CloseBead,
		GetDepsWithMeta: store.GetDepsWithMeta,
		Gateway:         cleric.DefaultGatewayClient,
		Learning:        cleric.DefaultLearning,
		RecordOutcome:   store.RecordClericOutcomeAuto,
	}
}
