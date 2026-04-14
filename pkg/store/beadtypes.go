package store

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/steveyegge/beads"
)

// ReviewFinding represents a single issue found during a code review.
// Stored as a JSON array in the "review_findings" metadata key.
type ReviewFinding struct {
	Severity string `json:"severity"`       // "error" or "warning"
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Message  string `json:"message"`
}

// --- Attempt bead helpers ---

// GetActiveAttempt returns the single open/in_progress attempt child of parentID.
// Returns (nil, nil) if no active attempt exists.
// Returns an error if more than one open attempt exists (invariant violation).
func GetActiveAttempt(parentID string) (*Bead, error) {
	children, err := GetChildren(parentID)
	if err != nil {
		return nil, err
	}

	var active []Bead
	for _, child := range children {
		if child.Status != "open" && child.Status != "in_progress" {
			continue
		}
		if !IsAttemptBead(child) {
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

// CreateAttemptBead creates a child attempt bead under parentID.
// Sets status=in_progress and adds labels: attempt, agent:<agentName>, branch:<branch>.
// The model label is only added when model is non-empty (callers like cmdClaim
// may not know the model at claim time -- the executor updates it later).
// Returns the attempt bead ID.
func CreateAttemptBead(parentID, agentName, model, branch string) (string, error) {
	labels := []string{
		"attempt",
		"agent:" + agentName,
		"branch:" + branch,
	}
	if model != "" {
		labels = append(labels, "model:"+model)
	}
	id, err := CreateBead(CreateOpts{
		Title:    "attempt: " + agentName,
		Priority: 3,
		Type:     beads.IssueType("attempt"),
		Labels:   labels,
		Parent:   parentID,
		Prefix:   PrefixFromID(parentID),
	})
	if err != nil {
		return "", fmt.Errorf("create attempt bead: %w", err)
	}
	// Transition to in_progress
	if uerr := UpdateBead(id, map[string]interface{}{
		"status": "in_progress",
	}); uerr != nil {
		return id, fmt.Errorf("set attempt in_progress: %w", uerr)
	}
	return id, nil
}

// CreateAttemptBeadAtomic checks for an existing active attempt before
// creating a new one. This narrows the TOCTOU race window between checking for
// an active attempt and creating one.
//
// Returns:
//   - (existingID, nil) if an active attempt by the same agent already exists
//   - (newID, nil) if no active attempt exists and a new one was created
//   - ("", error) if an active attempt by a different agent exists, or on failure
func CreateAttemptBeadAtomic(parentID, agentName, model, branch string) (string, error) {
	// Check for existing active attempt.
	existing, err := GetActiveAttempt(parentID)
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
			return existing.ID, nil // reclaim -- reuse existing attempt
		}
		return "", fmt.Errorf("active attempt %s already exists (agent: %s)", existing.ID, owner)
	}

	// No active attempt -- create one.
	return CreateAttemptBead(parentID, agentName, model, branch)
}

// CloseAttemptBead closes an attempt bead, adds a result: label, and adds a result comment.
func CloseAttemptBead(attemptID, result string) error {
	if attemptID == "" {
		return nil
	}
	if result != "" {
		AddLabel(attemptID, "result:"+result)
		AddComment(attemptID, result)
	}
	return CloseBead(attemptID)
}

// knownAttemptResults are the valid result values written by CloseAttemptBead.
var knownAttemptResults = map[string]bool{
	"success": true, "failure": true, "timeout": true, "error": true,
	"test_failure": true, "review_rejected": true, "stopped": true,
}

// AttemptResult extracts the result string from an attempt bead.
// Checks for a result: label first (fast path for new data), then falls back
// to the last comment that matches a known result value (legacy data).
// Returns "" if no result found.
func AttemptResult(b Bead) string {
	if v := HasLabel(b, "result:"); v != "" {
		return v
	}
	// Fallback: check comments for legacy attempt beads that lack the label.
	comments, err := GetComments(b.ID)
	if err != nil || len(comments) == 0 {
		return ""
	}
	// Walk backwards — the result comment is typically the last one.
	for i := len(comments) - 1; i >= 0; i-- {
		body := strings.TrimSpace(comments[i].Text)
		if knownAttemptResults[body] {
			return body
		}
	}
	return ""
}

// IsAttemptBead returns true if the bead is an attempt bead
// (has "attempt" label or title starts with "attempt:").
func IsAttemptBead(b Bead) bool {
	if strings.HasPrefix(b.Title, "attempt:") {
		return true
	}
	return ContainsLabel(b, "attempt")
}

// IsAttemptBoardBead returns true if the BoardBead is an attempt bead.
func IsAttemptBoardBead(b BoardBead) bool {
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

// --- Review round bead helpers ---

// CreateReviewBead creates a child review-round bead under parentID.
// Sets status=in_progress and adds labels: review-round, sage:<sageName>, round:<N>.
// The round number is determined by counting existing review children + 1.
// Returns the review bead ID.
func CreateReviewBead(parentID, sageName string, round int) (string, error) {
	labels := []string{
		"review-round",
		fmt.Sprintf("sage:%s", sageName),
		fmt.Sprintf("round:%d", round),
	}
	id, err := CreateBead(CreateOpts{
		Title:    fmt.Sprintf("review-round-%d", round),
		Priority: 3,
		Type:     beads.IssueType("review"),
		Labels:   labels,
		Parent:   parentID,
		Prefix:   PrefixFromID(parentID),
	})
	if err != nil {
		return "", fmt.Errorf("create review bead: %w", err)
	}
	// Transition to in_progress
	if uerr := UpdateBead(id, map[string]interface{}{
		"status": "in_progress",
	}); uerr != nil {
		return id, fmt.Errorf("set review bead in_progress: %w", uerr)
	}
	return id, nil
}

// CreateStepBead creates a child bead representing a workflow step.
// It has type=task, title="step:<stepName>", and labels: [workflow-step, step:<stepName>].
// The first step is created as in_progress (active), subsequent ones as open (pending).
func CreateStepBead(parentID, stepName string) (string, error) {
	labels := []string{"workflow-step", "step:" + stepName}
	id, err := CreateBead(CreateOpts{
		Title:    "step:" + stepName,
		Priority: 3,
		Type:     beads.IssueType("step"),
		Labels:   labels,
		Parent:   parentID,
		Prefix:   PrefixFromID(parentID),
	})
	if err != nil {
		return "", fmt.Errorf("create step bead %s for %s: %w", stepName, parentID, err)
	}
	return id, nil
}

// CloseReviewBead closes a review-round bead and sets its description to verdict+summary.
// It also writes structured metadata (review_verdict, error_count, warning_count, round)
// so verdict readers can query metadata instead of parsing the description.
// The findings slice, if non-nil, is marshalled to JSON and stored as review_findings metadata.
func CloseReviewBead(reviewID, verdict, summary string, errorCount, warningCount, round int, findings []ReviewFinding) error {
	if reviewID == "" {
		return nil
	}
	// Keep description for human readability.
	desc := fmt.Sprintf("verdict: %s\n\n%s", verdict, summary)
	if err := UpdateBead(reviewID, map[string]interface{}{
		"description": desc,
	}); err != nil {
		return fmt.Errorf("update review bead description: %w", err)
	}

	// Write structured metadata — the machine-readable twin.
	meta := map[string]string{
		"review_verdict": verdict,
		"error_count":    strconv.Itoa(errorCount),
		"warning_count":  strconv.Itoa(warningCount),
		"round":          strconv.Itoa(round),
	}
	if len(findings) > 0 {
		if b, err := json.Marshal(findings); err == nil {
			meta["review_findings"] = string(b)
		}
	}
	if err := SetBeadMetadataMap(reviewID, meta); err != nil {
		// Non-fatal: the bead is still useful with just the description.
		fmt.Fprintf(os.Stderr, "warning: set review metadata on %s: %s\n", reviewID, err)
	}

	return CloseBead(reviewID)
}

// GetReviewBeads returns all review-round child beads of parentID,
// ordered by creation time (via round label, ascending).
func GetReviewBeads(parentID string) ([]Bead, error) {
	children, err := GetChildren(parentID)
	if err != nil {
		return nil, err
	}
	var reviews []Bead
	for _, child := range children {
		if IsReviewRoundBead(child) {
			reviews = append(reviews, child)
		}
	}
	// Sort by round number (extracted from round:<N> label).
	// This gives creation-time ordering since round numbers are sequential.
	for i := 0; i < len(reviews); i++ {
		for j := i + 1; j < len(reviews); j++ {
			ri := ReviewRoundNumber(reviews[i])
			rj := ReviewRoundNumber(reviews[j])
			if rj < ri {
				reviews[i], reviews[j] = reviews[j], reviews[i]
			}
		}
	}
	return reviews, nil
}

// IsReviewRoundBead returns true if the bead is a review-round bead
// (has "review-round" label or title starts with "review-round-").
func IsReviewRoundBead(b Bead) bool {
	if strings.HasPrefix(b.Title, "review-round-") {
		return true
	}
	return ContainsLabel(b, "review-round")
}

// IsReviewRoundBoardBead returns true if the BoardBead is a review-round bead.
func IsReviewRoundBoardBead(b BoardBead) bool {
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

// --- Workflow step bead helpers ---

// ActivateStepBead sets a step bead to in_progress status.
func ActivateStepBead(stepID string) error {
	return UpdateBead(stepID, map[string]interface{}{
		"status": "in_progress",
	})
}

// CloseStepBead closes a workflow step bead.
func CloseStepBead(stepID string) error {
	return CloseBead(stepID)
}

// HookStepBead sets a step bead's status to 'hooked', indicating the step is
// parked waiting for a condition (human approval, external event, error recovery).
// Returns an error if the bead does not exist or is not of type=step.
func HookStepBead(stepID string) error {
	b, err := GetBead(stepID)
	if err != nil {
		return fmt.Errorf("hook step bead %s: %w", stepID, err)
	}
	if b.Type != "step" {
		return fmt.Errorf("hook step bead %s: expected type=step, got type=%s", stepID, b.Type)
	}
	return UpdateBead(stepID, map[string]interface{}{
		"status": "hooked",
	})
}

// UnhookStepBead transitions a hooked step bead back to 'open', for when a
// hooked condition is resolved and the step should be re-evaluated.
// Returns an error if the bead does not exist or is not of type=step.
func UnhookStepBead(stepID string) error {
	b, err := GetBead(stepID)
	if err != nil {
		return fmt.Errorf("unhook step bead %s: %w", stepID, err)
	}
	if b.Type != "step" {
		return fmt.Errorf("unhook step bead %s: expected type=step, got type=%s", stepID, b.Type)
	}
	return UpdateBead(stepID, map[string]interface{}{
		"status": "open",
	})
}

// GetHookedSteps returns all workflow-step children of a parent that have status=hooked.
func GetHookedSteps(parentID string) ([]Bead, error) {
	steps, err := GetStepBeads(parentID)
	if err != nil {
		return nil, err
	}
	var hooked []Bead
	for _, s := range steps {
		if s.Status == "hooked" {
			hooked = append(hooked, s)
		}
	}
	return hooked, nil
}

// GetStepBeads returns all workflow-step children of a parent bead, ordered by creation.
func GetStepBeads(parentID string) ([]Bead, error) {
	children, err := GetChildren(parentID)
	if err != nil {
		return nil, err
	}
	var steps []Bead
	for _, child := range children {
		if IsStepBead(child) {
			steps = append(steps, child)
		}
	}
	return steps, nil
}

// GetActiveStep returns the single in_progress step bead for a parent.
// Returns (nil, nil) if no step is active.
// Returns an error if more than one in_progress step exists (invariant violation).
func GetActiveStep(parentID string) (*Bead, error) {
	steps, err := GetStepBeads(parentID)
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

// IsStepBead returns true if the bead is a workflow step bead.
func IsStepBead(b Bead) bool {
	return ContainsLabel(b, "workflow-step")
}

// IsStepBoardBead returns true if the BoardBead is a workflow step bead.
func IsStepBoardBead(b BoardBead) bool {
	for _, l := range b.Labels {
		if l == "workflow-step" {
			return true
		}
	}
	return false
}

// --- Formula template bead helpers ---

// IsFormulaTemplateBead returns true if the bead is a formula template bead
// (has unresolved template variables like {{task}} in the title).
func IsFormulaTemplateBead(b Bead) bool {
	return strings.Contains(b.Title, "{{")
}

// IsFormulaTemplateBoardBead returns true if the BoardBead is a formula template bead.
func IsFormulaTemplateBoardBead(b BoardBead) bool {
	return strings.Contains(b.Title, "{{")
}

// ReviewRoundNumber extracts the round number from a review bead's round:<N> label.
// Returns 0 if not found.
func ReviewRoundNumber(b Bead) int {
	val := HasLabel(b, "round:")
	if val == "" {
		return 0
	}
	n := 0
	fmt.Sscanf(val, "%d", &n)
	return n
}

// StepBeadPhaseName extracts the phase name from a step bead's step:<name> label.
// Returns "" if no step: label is found.
func StepBeadPhaseName(b Bead) string {
	return HasLabel(b, "step:")
}
