package executor

import (
	"fmt"

	"github.com/awell-health/spire/pkg/formula"
	"github.com/steveyegge/beads"
)

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

// --- Review sub-step bead management ---

// ensureReviewSubStepBeads creates sub-step beads under the step:review bead
// for each step in the review formula graph. Idempotent — skips steps that
// already have beads.
func (e *Executor) ensureReviewSubStepBeads(graph *formula.FormulaStepGraph) error {
	// Find the step:review bead (parent for sub-steps).
	reviewBeadID := e.state.StepBeadIDs["review"]
	if reviewBeadID == "" {
		// No step beads created (legacy run) — use main bead as parent.
		reviewBeadID = e.beadID
	}

	if e.state.ReviewStepBeadIDs == nil {
		e.state.ReviewStepBeadIDs = make(map[string]string)
	}

	// Reconcile from graph: check for existing sub-step beads.
	if len(e.state.ReviewStepBeadIDs) == 0 {
		if children, err := e.deps.GetChildren(reviewBeadID); err == nil {
			for _, child := range children {
				if e.deps.ContainsLabel(child, "review-substep") {
					if stepName := e.deps.HasLabel(child, "step:"); stepName != "" {
						e.state.ReviewStepBeadIDs[stepName] = child.ID
					}
				}
			}
			if len(e.state.ReviewStepBeadIDs) > 0 {
				e.log("reconciled %d review sub-step beads from graph", len(e.state.ReviewStepBeadIDs))
				return e.saveState()
			}
		}
	}

	if e.deps.CreateBead == nil {
		e.log("warning: CreateBead dep not available, skipping review sub-step bead creation")
		return nil
	}

	for stepName, stepCfg := range graph.Steps {
		if _, exists := e.state.ReviewStepBeadIDs[stepName]; exists {
			continue
		}
		title := stepCfg.Title
		if title == "" {
			title = stepName
		}
		id, err := e.deps.CreateBead(CreateOpts{
			Title:    title,
			Priority: 3,
			Type:     beads.TypeTask,
			Parent:   reviewBeadID,
			Labels:   []string{"workflow-step", "step:" + stepName, "review-substep"},
		})
		if err != nil {
			return fmt.Errorf("create review sub-step bead for %s: %w", stepName, err)
		}
		e.state.ReviewStepBeadIDs[stepName] = id
		e.log("created review sub-step bead %s for %s", id, stepName)
	}

	return e.saveState()
}

// activateReviewSubStep sets a review sub-step bead to in_progress.
func (e *Executor) activateReviewSubStep(stepName string) error {
	beadID, ok := e.state.ReviewStepBeadIDs[stepName]
	if !ok {
		return nil // no sub-step beads created (test/legacy)
	}
	return e.deps.ActivateStepBead(beadID)
}

// closeReviewSubStep closes a review sub-step bead.
func (e *Executor) closeReviewSubStep(stepName string) error {
	beadID, ok := e.state.ReviewStepBeadIDs[stepName]
	if !ok {
		return nil // no sub-step beads created (test/legacy)
	}
	return e.deps.CloseStepBead(beadID)
}

// completedReviewSteps returns a map of step names to completion status
// by checking the bead status for each review sub-step.
func (e *Executor) completedReviewSteps() map[string]bool {
	completed := make(map[string]bool)
	for stepName, beadID := range e.state.ReviewStepBeadIDs {
		b, err := e.deps.GetBead(beadID)
		if err != nil {
			continue
		}
		completed[stepName] = b.Status == "closed"
	}
	return completed
}

// resetReviewSubStep re-opens a review sub-step bead (sets status back to open).
// Used by the fix→sage-review loop reset.
func (e *Executor) resetReviewSubStep(stepName string) error {
	beadID, ok := e.state.ReviewStepBeadIDs[stepName]
	if !ok {
		return nil // no sub-step beads created (test/legacy)
	}
	return e.deps.UpdateBead(beadID, map[string]interface{}{"status": "open"})
}

// transitionStepBead closes the previous phase's step bead and activates the new one.
// Idempotent: skips close if the bead is already closed, skips activate if already
// in_progress or closed. This is defense-in-depth — the primary fix is that each
// subsystem only closes beads it owns.
func (e *Executor) transitionStepBead(prevPhase, newPhase string) {
	if len(e.state.StepBeadIDs) == 0 {
		return // no step beads created (legacy run)
	}

	// Close previous step bead (idempotent — skip if already closed).
	if prevPhase != "" {
		if prevID, ok := e.state.StepBeadIDs[prevPhase]; ok {
			if b, err := e.deps.GetBead(prevID); err == nil && b.Status == "closed" {
				e.log("step bead %s (%s) already closed — skipping", prevID, prevPhase)
			} else {
				if err := e.deps.CloseStepBead(prevID); err != nil {
					e.log("warning: close step bead %s (%s): %s", prevID, prevPhase, err)
				}
			}
		}
	}

	// Activate new step bead (skip if already closed or in_progress).
	if newPhase != "" {
		if newID, ok := e.state.StepBeadIDs[newPhase]; ok {
			if b, err := e.deps.GetBead(newID); err == nil && (b.Status == "closed" || b.Status == "in_progress") {
				// Already closed (e.g. parent bead merged) or already active — no-op.
			} else {
				if err := e.deps.ActivateStepBead(newID); err != nil {
					e.log("warning: activate step bead %s (%s): %s", newID, newPhase, err)
				}
			}
		}
	}
}

// closeAllOpenStepBeads closes all step beads that are not already closed.
// Used on exit paths (bead externally closed, error) to prevent leaked step beads.
func (e *Executor) closeAllOpenStepBeads() {
	for phase, stepID := range e.state.StepBeadIDs {
		b, err := e.deps.GetBead(stepID)
		if err != nil {
			continue
		}
		if b.Status != "closed" {
			if err := e.deps.CloseStepBead(stepID); err != nil {
				e.log("warning: close step bead %s (%s): %s", stepID, phase, err)
			}
		}
	}
}
