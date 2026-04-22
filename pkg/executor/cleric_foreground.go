package executor

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
)

// RunClericForeground executes the cleric-default formula synchronously
// against the recovery bead, emitting one recovery.PhaseEvent per step
// completion on events.
//
// Unlike the steward dispatch path, this entry point:
//   - runs in the caller's process (no Backend.Spawn)
//   - wires an OnStepCompleted observer so every phase completion
//     produces a PhaseEvent the foreground dispatcher can render
//   - returns the RecoveryOutcome for the CLI summary line
//
// The steward loop is bypassed: the executor runs directly through
// RunGraph. Events are emitted in-band (the runner does not own the
// channel; the caller owns it and closes after this function returns).
//
// Workers spawned by execute (worker-mode repair) block the phase on
// their exit — the observer fires after the handler returns, so the
// execute PhaseEvent reflects the final state of the dispatched
// repair, not a mid-flight snapshot.
func RunClericForeground(
	ctx context.Context,
	bead *store.Bead,
	deps *Deps,
	events chan<- recovery.PhaseEvent,
) (recovery.RecoveryOutcome, error) {
	if bead == nil {
		return recovery.RecoveryOutcome{}, fmt.Errorf("cleric foreground: bead is required")
	}
	if deps == nil {
		return recovery.RecoveryOutcome{}, fmt.Errorf("cleric foreground: deps is required")
	}

	graph, err := formula.LoadStepGraphByName("cleric-default")
	if err != nil {
		return recovery.RecoveryOutcome{}, fmt.Errorf("cleric foreground: load formula: %w", err)
	}

	agentName := "cleric-fg-" + bead.ID

	// Wrap OnStepCompleted so phase events are emitted for cleric
	// steps. Any prior observer on deps is preserved via composition.
	prior := deps.OnStepCompleted
	depsCopy := *deps
	depsCopy.OnStepCompleted = func(stepName string, outputs map[string]string, stepErr error) {
		if prior != nil {
			prior(stepName, outputs, stepErr)
		}
		if events == nil {
			return
		}
		ev := buildPhaseEvent(stepName, outputs, stepErr)
		select {
		case events <- ev:
		case <-ctx.Done():
		}
	}

	ex, err := NewGraph(bead.ID, agentName, graph, &depsCopy)
	if err != nil {
		return recovery.RecoveryOutcome{}, fmt.Errorf("cleric foreground: new graph: %w", err)
	}
	ex.SetFormulaSource("embedded")

	if runErr := ex.Run(); runErr != nil {
		outcome, _ := readOutcomeByBead(deps, bead.ID)
		return outcome, runErr
	}

	outcome, _ := readOutcomeByBead(deps, bead.ID)
	return outcome, nil
}

// readOutcomeByBead reloads the recovery bead through the injected
// GetBead dep (falling back to store.GetBead when deps is unwired)
// and pulls the persisted RecoveryOutcome via recovery.ReadOutcome.
// Returns a zero outcome with ok=false when no outcome has been
// written — that happens when the cleric crashed before finish,
// which the foreground CLI surfaces as an infra error.
func readOutcomeByBead(deps *Deps, beadID string) (recovery.RecoveryOutcome, bool) {
	var bead store.Bead
	var err error
	if deps != nil && deps.GetBead != nil {
		bead, err = deps.GetBead(beadID)
	} else {
		bead, err = store.GetBead(beadID)
	}
	if err != nil {
		return recovery.RecoveryOutcome{}, false
	}
	return recovery.ReadOutcome(bead)
}

// buildPhaseEvent converts a step-completion observation into a
// PhaseEvent. Phase names in the cleric-default formula map 1:1 to
// the canonical recovery phases; formula aliases (finish_needs_human,
// retry_on_error) flow through under their declared step name so the
// formatter can surface them distinctly.
func buildPhaseEvent(stepName string, outputs map[string]string, stepErr error) recovery.PhaseEvent {
	ev := recovery.PhaseEvent{
		Phase:   stepName,
		Step:    stepName,
		Details: map[string]any{},
		Ts:      time.Now().UTC(),
	}
	if stepErr != nil {
		ev.Err = stepErr.Error()
	}
	if outputs == nil {
		return ev
	}

	switch stepName {
	case "collect_context":
		copyDetail(ev.Details, outputs, "failure_class", "class")
		copyDetail(ev.Details, outputs, "source_bead", "source_bead")
		copyDetail(ev.Details, outputs, "source_attempt_id", "source_attempt_id")
		copyDetail(ev.Details, outputs, "failed_step", "failed_step")
		copyDetail(ev.Details, outputs, "verification_status", "verification_status")
		copyDetail(ev.Details, outputs, "total_attempts", "attempts")

	case "decide":
		ev.Branch = outputs["decide_branch"]
		ev.Reason = outputs["reasoning"]
		if conf := outputs["confidence"]; conf != "" {
			if f, err := strconv.ParseFloat(conf, 64); err == nil {
				ev.Confidence = f
			}
		}
		// Derive the action from the step's needs-human output path or
		// from the plan JSON if callers later choose to surface it.
		// decide today only publishes a JSON plan blob; the action is
		// embedded there. We keep the event Action empty unless the
		// outputs expose a scalar we can trust cheaply.
		if needsHuman := outputs["needs_human"]; needsHuman != "" {
			ev.Details["needs_human"] = needsHuman
		}
		if promoted := outputs["promoted"]; promoted != "" {
			ev.Details["promoted"] = promoted
		}

	case "execute":
		ev.Action = outputs["action"]
		if mode := outputs["mode"]; mode != "" {
			ev.Details["mode"] = mode
		}
		if wid := outputs["worker_attempt_id"]; wid != "" {
			ev.Details["apprentice"] = wid
		}
		if hm := outputs["handoff_mode"]; hm != "" {
			ev.Details["handoff"] = hm
		}
		if reason := outputs["reason"]; reason != "" {
			ev.Reason = reason
		}
		if st := outputs["status"]; st != "" && st != "success" {
			ev.Details["status"] = st
		}

	case "verify":
		ev.Verdict = outputs["verdict"]
		if ev.Verdict == "" {
			ev.Verdict = outputs["verification_status"]
		}
		if vk := outputs["verify_kind"]; vk != "" {
			ev.Details["kind"] = vk
		}
		if failedStep := outputs["failed_step"]; failedStep != "" {
			ev.Details["failed_step"] = failedStep
		}

	case "learn":
		copyDetail(ev.Details, outputs, "repair_mode", "mode")
		copyDetail(ev.Details, outputs, "repair_action", "recipe")
		copyDetail(ev.Details, outputs, "decision", "decision")
		copyDetail(ev.Details, outputs, "verify_verdict", "verdict")
		copyDetail(ev.Details, outputs, "outcome", "outcome")

	case "finish", "finish_needs_human", "finish_needs_human_on_error":
		ev.Action = outputs["action"]
		copyDetail(ev.Details, outputs, "status", "status")
		copyDetail(ev.Details, outputs, "outcome", "outcome")

	default:
		// Formula-declared steps we don't special-case (e.g.
		// retry_on_error, retry) still get an event so operators
		// see them fire. Copy the raw outputs wholesale.
		for k, v := range outputs {
			ev.Details[k] = v
		}
	}

	// Clean up empty Details so FormatPhaseEvent doesn't render an
	// empty trailer for phases that contributed nothing.
	if len(ev.Details) == 0 {
		ev.Details = nil
	}
	return ev
}

// copyDetail copies outputs[srcKey] into details[dstKey] iff the
// source value is non-empty. The dstKey distinct from srcKey keeps
// the rendered line compact (e.g. "failure_class" → "class").
func copyDetail(details map[string]any, outputs map[string]string, srcKey, dstKey string) {
	if v := outputs[srcKey]; v != "" {
		details[dstKey] = v
	}
}
