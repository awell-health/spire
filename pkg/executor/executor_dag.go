package executor

import "fmt"

// ensureAttemptBead reuses an existing attempt bead (typically created by cmdClaim)
// or creates a new one if none exists.
func (e *Executor) ensureAttemptBead() error {
	// Determine model from formula for label updates.
	model := "unknown"
	if e.formula != nil && e.formula.Phases != nil {
		if pc, ok := e.formula.Phases[e.state.Phase]; ok && pc.Model != "" {
			model = pc.Model
		}
	}

	// If we already have an attempt bead from persisted state, verify it's still open.
	if e.state.AttemptBeadID != "" {
		b, err := e.deps.GetBead(e.state.AttemptBeadID)
		if err == nil && (b.Status == "open" || b.Status == "in_progress") {
			e.log("resuming existing attempt bead %s", e.state.AttemptBeadID)
			e.ensureAttemptModelLabel(e.state.AttemptBeadID, b, model)
			return nil
		}
		// Previous attempt was closed — will create a new one.
		e.state.AttemptBeadID = ""
	}

	// Check for an existing active attempt (e.g. created by cmdClaim).
	existing, err := e.deps.GetActiveAttempt(e.beadID)
	if err != nil {
		return err // invariant violation
	}
	if existing != nil {
		// Reuse if it belongs to this agent; reject if it belongs to another.
		agent := e.deps.HasLabel(*existing, "agent:")
		if agent == e.agentName {
			e.state.AttemptBeadID = existing.ID
			e.log("reusing attempt bead %s (created by claim)", existing.ID)
			e.ensureAttemptModelLabel(existing.ID, *existing, model)
			return e.saveState()
		}
		return fmt.Errorf("active attempt %s already exists (agent: %s)", existing.ID, agent)
	}

	// No existing attempt — create one (direct executor invocation, not via claim).
	branch := e.state.StagingBranch
	if branch == "" {
		branch = e.resolveBranch(e.beadID)
	}

	id, err := e.deps.CreateAttemptBead(e.beadID, e.agentName, model, branch)
	if err != nil {
		return err
	}
	e.state.AttemptBeadID = id
	e.log("created attempt bead %s", id)
	return e.saveState()
}

// ensureAttemptModelLabel adds the model:<model> label to an attempt bead if
// it's missing.
func (e *Executor) ensureAttemptModelLabel(attemptID string, b Bead, model string) {
	if model == "" || model == "unknown" {
		return
	}
	existingModel := e.deps.HasLabel(b, "model:")
	if existingModel == model {
		return // already correct
	}
	if existingModel != "" {
		// Model label exists but is wrong — remove it first.
		if err := e.deps.RemoveLabel(attemptID, "model:"+existingModel); err != nil {
			e.log("warning: remove stale model label from %s: %s", attemptID, err)
		}
	}
	if err := e.deps.AddLabel(attemptID, "model:"+model); err != nil {
		e.log("warning: add model label to attempt %s: %s", attemptID, err)
	} else {
		e.log("updated attempt %s model label to %s", attemptID, model)
	}
}

// closeAttempt closes the current attempt bead with the given result.
func (e *Executor) closeAttempt(result string) {
	if e.state.AttemptBeadID == "" {
		return
	}
	if err := e.deps.CloseAttemptBead(e.state.AttemptBeadID, result); err != nil {
		e.log("warning: close attempt bead %s: %s", e.state.AttemptBeadID, err)
	}
	e.state.AttemptBeadID = ""
}

// ensureStepBeads creates workflow step beads for each enabled formula phase.
func (e *Executor) ensureStepBeads() error {
	if len(e.state.StepBeadIDs) > 0 {
		e.log("step beads already created (%d phases)", len(e.state.StepBeadIDs))
		return nil
	}

	// Guard against crash-between-create-and-save: query existing step children
	// from the graph and rebuild StepBeadIDs before creating new ones.
	if children, err := e.deps.GetChildren(e.beadID); err != nil {
		e.log("warning: query step children for reconciliation: %s (will create fresh)", err)
	} else {
		rebuilt := make(map[string]string)
		for _, child := range children {
			if e.deps.ContainsLabel(child, "workflow-step") {
				if phase := e.deps.HasLabel(child, "step:"); phase != "" {
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

	phases := e.formula.EnabledPhases()
	if len(phases) == 0 {
		return nil
	}

	e.state.StepBeadIDs = make(map[string]string, len(phases))
	for i, phase := range phases {
		id, err := e.deps.CreateStepBead(e.beadID, phase)
		if err != nil {
			return fmt.Errorf("create step bead for %s: %w", phase, err)
		}
		e.state.StepBeadIDs[phase] = id
		e.log("created step bead %s for phase %s", id, phase)

		// Activate the first step bead (it matches the initial phase).
		if i == 0 {
			if err := e.deps.ActivateStepBead(id); err != nil {
				e.log("warning: activate step bead %s: %s", id, err)
			}
		}
	}

	return e.saveState()
}

// transitionStepBead closes the previous phase's step bead and activates the new one.
func (e *Executor) transitionStepBead(prevPhase, newPhase string) {
	if len(e.state.StepBeadIDs) == 0 {
		return // no step beads created (legacy run)
	}

	// Close previous step bead.
	if prevPhase != "" {
		if prevID, ok := e.state.StepBeadIDs[prevPhase]; ok {
			if err := e.deps.CloseStepBead(prevID); err != nil {
				e.log("warning: close step bead %s (%s): %s", prevID, prevPhase, err)
			}
		}
	}

	// Activate new step bead.
	if newID, ok := e.state.StepBeadIDs[newPhase]; ok {
		if err := e.deps.ActivateStepBead(newID); err != nil {
			e.log("warning: activate step bead %s (%s): %s", newID, newPhase, err)
		}
	}
}
