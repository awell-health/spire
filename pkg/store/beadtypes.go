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
// Sets status=in_progress and adds labels: attempt, attempt:<N>,
// agent:<agentName>, branch:<branch>, reset-cycle:<C>.
//
// The attempt:<N> sequence number is monotonic across reset cycles — it scans
// all attempt children (open + closed) for the largest existing attempt:<N>
// label and increments it. The reset-cycle:<C> label is inherited from the
// parent bead so the board can later group historical attempts by cycle.
//
// The model label is only added when model is non-empty (callers like cmdClaim
// may not know the model at claim time -- the executor updates it later).
// Returns the attempt bead ID.
func CreateAttemptBead(parentID, agentName, model, branch string) (string, error) {
	nextN := MaxAttemptNumber(parentID) + 1
	cycle := ParentResetCycle(parentID)
	labels := []string{
		"attempt",
		fmt.Sprintf("attempt:%d", nextN),
		"agent:" + agentName,
		"branch:" + branch,
		fmt.Sprintf("reset-cycle:%d", cycle),
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
// Sets status=in_progress and adds labels: review-round, sage:<sageName>,
// round:<N>, reset-cycle:<C>.
//
// The caller is responsible for passing a monotonic round number — see
// MaxRoundNumber for the canonical lookup. The reset-cycle:<C> label is
// inherited from the parent bead so the board can later group historical
// rounds by cycle.
// Returns the review bead ID.
func CreateReviewBead(parentID, sageName string, round int) (string, error) {
	cycle := ParentResetCycle(parentID)
	labels := []string{
		"review-round",
		fmt.Sprintf("sage:%s", sageName),
		fmt.Sprintf("round:%d", round),
		fmt.Sprintf("reset-cycle:%d", cycle),
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

// MostRecentReviewRound returns the highest-numbered review-round child of
// parentID, regardless of status. Returns (nil, nil) when no review-round
// child exists.
//
// This is the lookup arbiter and sage share to identify the round whose
// verdict they are about to set: the arbiter writes the binding verdict on
// it; the sage refuses to write when it already carries an arbiter_verdict.
func MostRecentReviewRound(parentID string) (*Bead, error) {
	reviews, err := GetReviewBeads(parentID)
	if err != nil {
		return nil, err
	}
	if len(reviews) == 0 {
		return nil, nil
	}
	last := reviews[len(reviews)-1]
	return &last, nil
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

// ReopenStepBead transitions a closed/hooked step bead back to "open" — the
// pre-dispatch resting state. It's the rewind reconciliation counterpart to
// ActivateStepBead: callers reopening a step whose graph-state is "pending"
// (because a reset rewound the graph) must use this helper, not
// ActivateStepBead, otherwise the reused step bead is wrongly marked
// "in_progress" even though the graph step is not the active one.
//
// Status semantics:
//   - closed → open: rewound step beads return to their pre-run state.
//   - hooked → open: hooked step beads return to open (mirrors UnhookStepBead).
//   - open: no-op (idempotent).
//   - in_progress: rejected — this would be a downgrade of legitimately active
//     state. Callers must not route the actually-active step through here;
//     normal dispatch (ActivateStepBead) handles activation.
func ReopenStepBead(stepID string) error {
	b, err := GetBead(stepID)
	if err != nil {
		return fmt.Errorf("reopen step bead %s: %w", stepID, err)
	}
	if b.Type != "step" {
		return fmt.Errorf("reopen step bead %s: expected type=step, got type=%s", stepID, b.Type)
	}
	switch b.Status {
	case "open":
		return nil
	case "in_progress":
		return fmt.Errorf("reopen step bead %s: refusing to downgrade in_progress to open", stepID)
	case "closed", "hooked":
		return UpdateBead(stepID, map[string]interface{}{"status": "open"})
	default:
		return UpdateBead(stepID, map[string]interface{}{"status": "open"})
	}
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

// AttemptNumber extracts the attempt sequence number from an attempt bead's
// attempt:<N> label. Returns 0 if no such label is present (legacy attempts
// created before the monotonic counter was introduced lack this label).
func AttemptNumber(b Bead) int {
	val := HasLabel(b, "attempt:")
	if val == "" {
		return 0
	}
	n := 0
	fmt.Sscanf(val, "%d", &n)
	return n
}

// ResetCycleNumber extracts the reset cycle from a bead's reset-cycle:<N>
// label. Beads created before the feature shipped have no label; treat
// missing as cycle 1 (the implicit first cycle).
func ResetCycleNumber(b Bead) int {
	val := HasLabel(b, "reset-cycle:")
	if val == "" {
		return 1
	}
	n := 0
	fmt.Sscanf(val, "%d", &n)
	if n < 1 {
		return 1
	}
	return n
}

// MaxRoundNumberFromBeads returns the largest round:<N> label value found
// across the given review beads. Returns 0 when no review carries a parseable
// round label. Robust against malformed labels (round:, round:abc) — they are
// skipped, not panicked on.
func MaxRoundNumberFromBeads(reviews []Bead) int {
	max := 0
	for _, r := range reviews {
		if !IsReviewRoundBead(r) {
			continue
		}
		if n := ReviewRoundNumber(r); n > max {
			max = n
		}
	}
	return max
}

// MaxRoundNumber scans all review-round children of parentID (open + closed)
// and returns the highest round:<N> label value. Returns 0 when there are no
// reviews. The next monotonic round is therefore MaxRoundNumber(parentID)+1.
func MaxRoundNumber(parentID string) int {
	reviews, err := GetReviewBeads(parentID)
	if err != nil {
		return 0
	}
	return MaxRoundNumberFromBeads(reviews)
}

// MaxAttemptNumberFromBeads returns the largest attempt:<N> label value found
// across the given attempt beads. Returns 0 when none carry a parseable
// attempt label.
func MaxAttemptNumberFromBeads(beads []Bead) int {
	max := 0
	for _, b := range beads {
		if !IsAttemptBead(b) {
			continue
		}
		if n := AttemptNumber(b); n > max {
			max = n
		}
	}
	return max
}

// MaxAttemptNumber scans all attempt children of parentID (open + closed) and
// returns the highest attempt:<N> label value. Returns 0 when there are no
// attempts (or all predate the attempt:<N> label).
func MaxAttemptNumber(parentID string) int {
	children, err := GetChildren(parentID)
	if err != nil {
		return 0
	}
	return MaxAttemptNumberFromBeads(children)
}

// ParentResetCycle returns the parent bead's current reset-cycle. New
// attempt/review children should inherit this value at creation time so they
// can later be grouped by cycle on the board. Defaults to 1 when the parent
// has no label (pre-feature beads or first cycle).
func ParentResetCycle(parentID string) int {
	parent, err := GetBead(parentID)
	if err != nil {
		return 1
	}
	return ResetCycleNumber(parent)
}

// StepBeadPhaseName extracts the phase name from a step bead's step:<name> label.
// Returns "" if no step: label is found.
func StepBeadPhaseName(b Bead) string {
	return HasLabel(b, "step:")
}
