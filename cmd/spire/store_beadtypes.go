package main

import (
	"fmt"
	"log"
	"strings"

	"github.com/steveyegge/beads"
)

// --- Attempt bead helpers ---

// storeGetActiveAttempt returns the single open/in_progress attempt child of parentID.
// Returns (nil, nil) if no active attempt exists.
// Returns an error if more than one open attempt exists (invariant violation).
func storeGetActiveAttempt(parentID string) (*Bead, error) {
	children, err := storeGetChildren(parentID)
	if err != nil {
		return nil, err
	}

	var active []Bead
	for _, child := range children {
		if child.Status != "open" && child.Status != "in_progress" {
			continue
		}
		if !isAttemptBead(child) {
			continue
		}
		active = append(active, child)
	}

	switch len(active) {
	case 0:
		return nil, nil
	case 1:
		return &active[0], nil
	default:
		ids := make([]string, len(active))
		for i, a := range active {
			ids[i] = a.ID
		}
		return nil, fmt.Errorf("invariant violation: %d open attempt beads for %s: %s",
			len(active), parentID, strings.Join(ids, ", "))
	}
}

// storeCreateAttemptBead creates a child attempt bead under parentID.
// Sets status=in_progress and adds labels: attempt, agent:<agentName>, branch:<branch>.
// The model label is only added when model is non-empty (callers like cmdClaim
// may not know the model at claim time — the executor updates it later).
// Returns the attempt bead ID.
func storeCreateAttemptBead(parentID, agentName, model, branch string) (string, error) {
	labels := []string{
		"attempt",
		"agent:" + agentName,
		"branch:" + branch,
	}
	if model != "" {
		labels = append(labels, "model:"+model)
	}
	id, err := storeCreateBead(createOpts{
		Title:    "attempt: " + agentName,
		Priority: 3,
		Type:     beads.TypeTask,
		Labels:   labels,
		Parent:   parentID,
	})
	if err != nil {
		return "", fmt.Errorf("create attempt bead: %w", err)
	}
	// Transition to in_progress
	if uerr := storeUpdateBead(id, map[string]interface{}{
		"status": "in_progress",
	}); uerr != nil {
		return id, fmt.Errorf("set attempt in_progress: %w", uerr)
	}
	return id, nil
}

// storeCreateAttemptBeadAtomic checks for an existing active attempt before
// creating a new one. This narrows the TOCTOU race window between checking for
// an active attempt and creating one.
//
// Returns:
//   - (existingID, nil) if an active attempt by the same agent already exists
//   - (newID, nil) if no active attempt exists and a new one was created
//   - ("", error) if an active attempt by a different agent exists, or on failure
func storeCreateAttemptBeadAtomic(parentID, agentName, model, branch string) (string, error) {
	// Check for existing active attempt.
	existing, err := storeGetActiveAttempt(parentID)
	if err != nil {
		return "", fmt.Errorf("check active attempt: %w", err)
	}
	if existing != nil {
		owner := ""
		for _, l := range existing.Labels {
			if strings.HasPrefix(l, "agent:") {
				owner = l[6:]
				break
			}
		}
		if owner == agentName {
			return existing.ID, nil // reclaim — reuse existing attempt
		}
		return "", fmt.Errorf("active attempt %s already exists (agent: %s)", existing.ID, owner)
	}

	// No active attempt — create one.
	return storeCreateAttemptBead(parentID, agentName, model, branch)
}

// storeCloseAttemptBead closes an attempt bead and adds a result comment.
func storeCloseAttemptBead(attemptID, result string) error {
	if attemptID == "" {
		return nil
	}
	if result != "" {
		storeAddComment(attemptID, result)
	}
	return storeCloseBead(attemptID)
}

// isAttemptBead returns true if the bead is an attempt bead
// (has "attempt" label or title starts with "attempt:").
func isAttemptBead(b Bead) bool {
	if strings.HasPrefix(b.Title, "attempt:") {
		return true
	}
	return containsLabel(b, "attempt")
}

// isAttemptBoardBead returns true if the BoardBead is an attempt bead.
func isAttemptBoardBead(b BoardBead) bool {
	if strings.HasPrefix(b.Title, "attempt:") {
		return true
	}
	for _, l := range b.Labels {
		if l == "attempt" {
			return true
		}
	}
	return false
}

// storeGetChildrenFunc is a test-replaceable function for storeGetChildren.
// In production this stays at its default (storeGetChildren).
var storeGetChildrenFunc = storeGetChildren

// storeGetActiveAttemptFunc is a test-replaceable function for storeGetActiveAttempt.
// In production this stays at its default (storeGetActiveAttempt).
var storeGetActiveAttemptFunc = storeGetActiveAttempt

// storeRaiseCorruptedBeadAlertFunc is a test-replaceable function for storeRaiseCorruptedBeadAlert.
// In production this stays at its default (storeRaiseCorruptedBeadAlert).
var storeRaiseCorruptedBeadAlertFunc = storeRaiseCorruptedBeadAlert

// storeCheckExistingAlertFunc checks whether an open corrupted-bead alert already
// exists for beadID. Test-replaceable to avoid needing a real store in unit tests.
var storeCheckExistingAlertFunc = func(beadID string) bool {
	existing, err := storeListBeads(beads.IssueFilter{
		Labels: []string{"alert:corrupted-bead", "ref:" + beadID},
	})
	return err == nil && len(existing) > 0
}

// storeCreateAlertFunc creates the alert bead for a corrupted bead.
// Test-replaceable to verify creation is skipped when dedup fires.
var storeCreateAlertFunc = func(beadID, msg string) error {
	_, err := storeCreateBead(createOpts{
		Title:    msg,
		Priority: 0,
		Type:     beads.TypeTask,
		Labels:   []string{"alert:corrupted-bead", "ref:" + beadID},
	})
	return err
}

// storeRaiseCorruptedBeadAlert creates a P0 alert bead flagging a bead with
// multiple open attempt children (invariant violation). The caller should
// already have logged the violation and excluded the bead from ready work.
// Alert creation is best-effort: errors are logged, not propagated.
// Deduplication: if an open alert already exists for beadID, no new alert is created.
func storeRaiseCorruptedBeadAlert(beadID string, violation error) {
	if storeCheckExistingAlertFunc(beadID) {
		log.Printf("[store] alert already exists for corrupted bead %s, skipping duplicate", beadID)
		return
	}
	msg := fmt.Sprintf("corrupted bead %s: %v", beadID, violation)
	if err := storeCreateAlertFunc(beadID, msg); err != nil {
		log.Printf("[store] failed to raise alert for corrupted bead %s: %v", beadID, err)
	}
}

// --- Review round bead helpers ---

// storeCreateReviewBead creates a child review-round bead under parentID.
// Sets status=in_progress and adds labels: review-round, sage:<sageName>, round:<N>.
// The round number is determined by counting existing review children + 1.
// Returns the review bead ID.
func storeCreateReviewBead(parentID, sageName string, round int) (string, error) {
	labels := []string{
		"review-round",
		fmt.Sprintf("sage:%s", sageName),
		fmt.Sprintf("round:%d", round),
	}
	id, err := storeCreateBead(createOpts{
		Title:    fmt.Sprintf("review-round-%d", round),
		Priority: 3,
		Type:     beads.TypeTask,
		Labels:   labels,
		Parent:   parentID,
	})
	if err != nil {
		return "", fmt.Errorf("create review bead: %w", err)
	}
	// Transition to in_progress
	if uerr := storeUpdateBead(id, map[string]interface{}{
		"status": "in_progress",
	}); uerr != nil {
		return id, fmt.Errorf("set review bead in_progress: %w", uerr)
	}
	return id, nil
}

// --- Workflow step bead helpers ---

// storeCreateStepBead creates a child bead representing a workflow step.
// It has type=task, title="step:<stepName>", and labels: [workflow-step, step:<stepName>].
// The first step is created as in_progress (active), subsequent ones as open (pending).
func storeCreateStepBead(parentID, stepName string) (string, error) {
	labels := []string{"workflow-step", "step:" + stepName}
	id, err := storeCreateBead(createOpts{
		Title:    "step:" + stepName,
		Priority: 3,
		Type:     beads.TypeTask,
		Labels:   labels,
		Parent:   parentID,
	})
	if err != nil {
		return "", fmt.Errorf("create step bead %s for %s: %w", stepName, parentID, err)
	}
	return id, nil
}

// storeCloseReviewBead closes a review-round bead and sets its description to verdict+summary.
func storeCloseReviewBead(reviewID, verdict, summary string) error {
	if reviewID == "" {
		return nil
	}
	desc := fmt.Sprintf("verdict: %s\n\n%s", verdict, summary)
	if err := storeUpdateBead(reviewID, map[string]interface{}{
		"description": desc,
	}); err != nil {
		return fmt.Errorf("update review bead description: %w", err)
	}
	return storeCloseBead(reviewID)
}

// storeGetReviewBeads returns all review-round child beads of parentID,
// ordered by creation time (via round label, ascending).
func storeGetReviewBeads(parentID string) ([]Bead, error) {
	children, err := storeGetChildrenFunc(parentID)
	if err != nil {
		return nil, err
	}
	var reviews []Bead
	for _, child := range children {
		if isReviewRoundBead(child) {
			reviews = append(reviews, child)
		}
	}
	// Sort by round number (extracted from round:<N> label).
	// This gives creation-time ordering since round numbers are sequential.
	for i := 0; i < len(reviews); i++ {
		for j := i + 1; j < len(reviews); j++ {
			ri := reviewRoundNumber(reviews[i])
			rj := reviewRoundNumber(reviews[j])
			if rj < ri {
				reviews[i], reviews[j] = reviews[j], reviews[i]
			}
		}
	}
	return reviews, nil
}

// isReviewRoundBead returns true if the bead is a review-round bead
// (has "review-round" label or title starts with "review-round-").
func isReviewRoundBead(b Bead) bool {
	if strings.HasPrefix(b.Title, "review-round-") {
		return true
	}
	return containsLabel(b, "review-round")
}

// isReviewRoundBoardBead returns true if the BoardBead is a review-round bead.
func isReviewRoundBoardBead(b BoardBead) bool {
	if strings.HasPrefix(b.Title, "review-round-") {
		return true
	}
	for _, l := range b.Labels {
		if l == "review-round" {
			return true
		}
	}
	return false
}

// storeActivateStepBead sets a step bead to in_progress status.
func storeActivateStepBead(stepID string) error {
	return storeUpdateBead(stepID, map[string]interface{}{
		"status": "in_progress",
	})
}

// storeCloseStepBead closes a workflow step bead.
func storeCloseStepBead(stepID string) error {
	return storeCloseBead(stepID)
}

// storeGetStepBeads returns all workflow-step children of a parent bead, ordered by creation.
func storeGetStepBeads(parentID string) ([]Bead, error) {
	children, err := storeGetChildren(parentID)
	if err != nil {
		return nil, err
	}
	var steps []Bead
	for _, child := range children {
		if isStepBead(child) {
			steps = append(steps, child)
		}
	}
	return steps, nil
}

// storeGetActiveStep returns the single in_progress step bead for a parent.
// Returns (nil, nil) if no step is active.
// Returns an error if more than one in_progress step exists (invariant violation).
func storeGetActiveStep(parentID string) (*Bead, error) {
	steps, err := storeGetStepBeads(parentID)
	if err != nil {
		return nil, err
	}

	var active []Bead
	for _, s := range steps {
		if s.Status == "in_progress" {
			active = append(active, s)
		}
	}

	switch len(active) {
	case 0:
		return nil, nil
	case 1:
		return &active[0], nil
	default:
		ids := make([]string, len(active))
		for i, a := range active {
			ids[i] = a.ID
		}
		return nil, fmt.Errorf("invariant violation: %d in_progress step beads for %s: %s",
			len(active), parentID, strings.Join(ids, ", "))
	}
}

// isStepBead returns true if the bead is a workflow step bead.
func isStepBead(b Bead) bool {
	return containsLabel(b, "workflow-step")
}

// isStepBoardBead returns true if the BoardBead is a workflow step bead.
func isStepBoardBead(b BoardBead) bool {
	for _, l := range b.Labels {
		if l == "workflow-step" {
			return true
		}
	}
	return false
}

// reviewRoundNumber extracts the round number from a review bead's round:<N> label.
// Returns 0 if not found.
func reviewRoundNumber(b Bead) int {
	val := hasLabel(b, "round:")
	if val == "" {
		return 0
	}
	n := 0
	fmt.Sscanf(val, "%d", &n)
	return n
}

// stepBeadPhaseName extracts the phase name from a step bead's step:<name> label.
// Returns "" if no step: label is found.
func stepBeadPhaseName(b Bead) string {
	return hasLabel(b, "step:")
}
