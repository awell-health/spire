package main

import "fmt"

// ensureAttemptBead creates an attempt bead if one doesn't already exist in state.
// On retry (state has no attempt bead), creates a new one.
func (e *formulaExecutor) ensureAttemptBead() error {
	// If we already have an attempt bead from persisted state, verify it's still open.
	if e.state.AttemptBeadID != "" {
		b, err := e.beadGetter(e.state.AttemptBeadID)
		if err == nil && (b.Status == "open" || b.Status == "in_progress") {
			e.log("resuming existing attempt bead %s", e.state.AttemptBeadID)
			return nil
		}
		// Previous attempt was closed — will create a new one.
		e.state.AttemptBeadID = ""
	}

	// Check invariant: no open attempt should exist already.
	existing, err := e.activeAttemptGetter(e.beadID)
	if err != nil {
		return err // invariant violation
	}
	if existing != nil {
		// Another agent's attempt is still open — reuse if it's ours.
		agent := hasLabel(*existing, "agent:")
		if agent == e.agentName {
			e.state.AttemptBeadID = existing.ID
			e.log("reclaimed existing attempt bead %s", existing.ID)
			return nil
		}
		return fmt.Errorf("active attempt %s already exists (agent: %s)", existing.ID, agent)
	}

	// Determine model and branch.
	model := "unknown"
	if e.formula != nil && e.formula.Phases != nil {
		if pc, ok := e.formula.Phases[e.state.Phase]; ok && pc.Model != "" {
			model = pc.Model
		}
	}
	branch := e.state.StagingBranch
	if branch == "" {
		branch = fmt.Sprintf("feat/%s", e.beadID)
	}

	id, err := e.attemptCreator(e.beadID, e.agentName, model, branch)
	if err != nil {
		return err
	}
	e.state.AttemptBeadID = id
	e.log("created attempt bead %s", id)
	return e.saveState()
}

// closeAttempt closes the current attempt bead with the given result.
// It is idempotent — safe to call even if the attempt is already closed.
func (e *formulaExecutor) closeAttempt(result string) {
	if e.state.AttemptBeadID == "" {
		return
	}
	if err := e.attemptCloser(e.state.AttemptBeadID, result); err != nil {
		e.log("warning: close attempt bead %s: %s", e.state.AttemptBeadID, err)
	}
	e.state.AttemptBeadID = ""
}

// ensureStepBeads creates workflow step beads for each enabled formula phase.
// Called once at executor start. Idempotent in two ways:
//  1. If StepBeadIDs is already populated (from persisted state), this is a no-op.
//  2. If step beads already exist in the graph (crash between create and saveState),
//     they are reconciled into StepBeadIDs rather than duplicated.
func (e *formulaExecutor) ensureStepBeads() error {
	if len(e.state.StepBeadIDs) > 0 {
		e.log("step beads already created (%d phases)", len(e.state.StepBeadIDs))
		return nil
	}

	// Guard against crash-between-create-and-save: query existing step children
	// from the graph and rebuild StepBeadIDs before creating new ones.
	if e.childGetter != nil {
		if children, err := e.childGetter(e.beadID); err != nil {
			e.log("warning: query step children for reconciliation: %s (will create fresh)", err)
		} else {
			rebuilt := make(map[string]string)
			for _, child := range children {
				if containsLabel(child, "workflow-step") {
					if phase := hasLabel(child, "step:"); phase != "" {
						rebuilt[phase] = child.ID
					}
				}
			}
			if len(rebuilt) > 0 {
				e.state.StepBeadIDs = rebuilt
				e.log("reconciled %d existing step beads from graph (skipping creation)", len(rebuilt))
				return e.saveState()
			}
		}
	}

	phases := e.formula.EnabledPhases()
	if len(phases) == 0 {
		return nil
	}

	e.state.StepBeadIDs = make(map[string]string, len(phases))
	for i, phase := range phases {
		id, err := e.stepCreator(e.beadID, phase)
		if err != nil {
			return fmt.Errorf("create step bead for %s: %w", phase, err)
		}
		e.state.StepBeadIDs[phase] = id
		e.log("created step bead %s for phase %s", id, phase)

		// Activate the first step bead (it matches the initial phase).
		if i == 0 {
			if err := e.stepActivator(id); err != nil {
				e.log("warning: activate step bead %s: %s", id, err)
			}
		}
	}

	return e.saveState()
}

// transitionStepBead closes the previous phase's step bead and activates the new one.
// Called on phase transitions. Idempotent for the current phase.
func (e *formulaExecutor) transitionStepBead(prevPhase, newPhase string) {
	if len(e.state.StepBeadIDs) == 0 {
		return // no step beads created (legacy run)
	}

	// Close previous step bead.
	if prevPhase != "" {
		if prevID, ok := e.state.StepBeadIDs[prevPhase]; ok {
			if err := e.stepCloser(prevID); err != nil {
				e.log("warning: close step bead %s (%s): %s", prevID, prevPhase, err)
			}
		}
	}

	// Activate new step bead.
	if newID, ok := e.state.StepBeadIDs[newPhase]; ok {
		if err := e.stepActivator(newID); err != nil {
			e.log("warning: activate step bead %s (%s): %s", newID, newPhase, err)
		}
	}
}
