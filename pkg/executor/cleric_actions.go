package executor

import (
	"github.com/awell-health/spire/pkg/cleric"
	"github.com/awell-health/spire/pkg/store"
)

// init registers the four mechanical cleric.* action handlers in the
// executor's action registry. The handler logic lives in pkg/cleric so
// the package is testable independently of pkg/executor; the adapter
// functions below translate the executor's handler signature to the
// cleric package's stand-alone API.
//
// Cleric runtime (spi-hhkozk).
func init() {
	actionRegistry["cleric.publish"] = actionClericPublish
	actionRegistry["cleric.execute"] = actionClericExecute
	actionRegistry["cleric.takeover"] = actionClericTakeover
	actionRegistry["cleric.finish"] = actionClericFinish
}

// actionClericPublish parses the cleric Claude agent's stdout (the
// "result" output of the prior decide step) into a ProposedAction,
// persists it on the recovery bead, and transitions the bead to
// awaiting_review. Returns Hooked when persistence succeeds — the wait
// step that follows this in the formula is the actual park point, so
// publish itself is just bookkeeping.
func actionClericPublish(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	decideOut := lookupDecideOutput(state)
	res := cleric.Publish(e.beadID, decideOut, clericDepsFromExecutor(e))
	return clericResultToActionResult(res)
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

// clericDepsFromExecutor projects the executor's Deps onto the
// cleric.Deps surface, plus the package-level gateway client.
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
	}
}
