package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/steveyegge/beads"
)

// _defaultSctx is the package-level SpireContext used by wrapper functions
// during the transition to explicit SpireContext passing. Once all callers
// create and pass their own SpireContext, this will be removed.
var _defaultSctx = &SpireContext{}

// ensureStore opens a beads store using the default SpireContext.
// Deprecated: callers should create a SpireContext and call sc.EnsureStore().
func ensureStore() (beads.Storage, error) {
	return _defaultSctx.EnsureStore()
}

// openStoreAt opens a beads store at a specific .beads directory.
// Closes any existing store first.
// Deprecated: callers should create a SpireContext via NewSpireContextForTower().
func openStoreAt(beadsDir string) (beads.Storage, error) {
	_defaultSctx.Close()
	_defaultSctx = &SpireContext{BeadsDir: beadsDir}
	return _defaultSctx.EnsureStore()
}

// resetStore closes the active store.
// Deprecated: callers should call sc.Close() on their SpireContext.
func resetStore() {
	_defaultSctx.Close()
	_defaultSctx = &SpireContext{}
}

// storeActor returns the actor identity for store operations.
func storeActor() string {
	return "spire"
}

// --- Conversion helpers ---

// issueToBead converts a beads.Issue to spire's lightweight Bead type.
func issueToBead(issue *beads.Issue) Bead {
	parent := findParentID(issue.Dependencies)
	return Bead{
		ID:          issue.ID,
		Title:       issue.Title,
		Description: issue.Description,
		Status:      string(issue.Status),
		Priority:    issue.Priority,
		Type:        string(issue.IssueType),
		Labels:      issue.Labels,
		Parent:      parent,
	}
}

// issuesToBeads converts a slice of beads.Issue to spire's Bead type.
func issuesToBeads(issues []*beads.Issue) []Bead {
	result := make([]Bead, len(issues))
	for i, issue := range issues {
		result[i] = issueToBead(issue)
	}
	return result
}

// issueToBoardBead converts a beads.Issue to spire's BoardBead type.
func issueToBoardBead(issue *beads.Issue) BoardBead {
	parent := findParentID(issue.Dependencies)
	var deps []BoardDep
	for _, dep := range issue.Dependencies {
		deps = append(deps, BoardDep{
			IssueID:     dep.IssueID,
			DependsOnID: dep.DependsOnID,
			Type:        string(dep.Type),
		})
	}
	return BoardBead{
		ID:           issue.ID,
		Title:        issue.Title,
		Description:  issue.Description,
		Status:       string(issue.Status),
		Priority:     issue.Priority,
		Type:         string(issue.IssueType),
		Owner:        issue.Owner,
		CreatedAt:    issue.CreatedAt.Format(time.RFC3339),
		UpdatedAt:    issue.UpdatedAt.Format(time.RFC3339),
		Labels:       issue.Labels,
		Parent:       parent,
		Dependencies: deps,
	}
}

// issuesToBoardBeads converts a slice of beads.Issue to spire's BoardBead type.
func issuesToBoardBeads(issues []*beads.Issue) []BoardBead {
	result := make([]BoardBead, len(issues))
	for i, issue := range issues {
		result[i] = issueToBoardBead(issue)
	}
	return result
}

// findParentID extracts the parent ID from a dependency list.
func findParentID(deps []*beads.Dependency) string {
	for _, dep := range deps {
		if dep.Type == beads.DepParentChild {
			return dep.DependsOnID
		}
	}
	return ""
}

// --- Filter helpers ---

// statusPtr returns a pointer to a beads.Status value.
func statusPtr(s beads.Status) *beads.Status {
	return &s
}

// issueTypePtr returns a pointer to a beads.IssueType value.
func issueTypePtr(t beads.IssueType) *beads.IssueType {
	return &t
}

// parseStatus converts a status string to a beads.Status.
func parseStatus(s string) beads.Status {
	switch strings.ToLower(s) {
	case "open":
		return beads.StatusOpen
	case "in_progress":
		return beads.StatusInProgress
	case "blocked":
		return beads.StatusBlocked
	case "deferred":
		return beads.StatusDeferred
	case "closed":
		return beads.StatusClosed
	default:
		return beads.StatusOpen
	}
}

// parseIssueType converts a type string to a beads.IssueType.
func parseIssueType(s string) beads.IssueType {
	switch strings.ToLower(s) {
	case "bug":
		return beads.TypeBug
	case "feature":
		return beads.TypeFeature
	case "task":
		return beads.TypeTask
	case "epic":
		return beads.TypeEpic
	case "chore":
		return beads.TypeChore
	case "design":
		return beads.IssueType("design")
	default:
		return beads.TypeTask
	}
}

// --- Local interfaces for sub-interface access ---

// configDeleter provides DeleteConfig for config unset operations.
type configDeleter interface {
	DeleteConfig(ctx context.Context, key string) error
}

// pendingCommitter provides CommitPending for dolt commit operations.
type pendingCommitter interface {
	CommitPending(ctx context.Context, actor string) (bool, error)
}

// --- Create options ---

// createOpts holds parameters for creating a bead via the store.
type createOpts struct {
	Title       string
	Description string
	Priority    int
	Type        beads.IssueType
	Labels      []string
	Parent      string // creates parent-child dep after create
	Prefix      string // sets Issue.PrefixOverride (the --rig equivalent)
}

// ============================================================
// SpireContext methods — the real implementations.
// ============================================================

// GetBead fetches a single bead by ID.
func (sc *SpireContext) GetBead(id string) (Bead, error) {
	store, err := sc.EnsureStore()
	if err != nil {
		return Bead{}, err
	}
	issue, err := store.GetIssue(sc.storeCtx, id)
	if err != nil {
		return Bead{}, fmt.Errorf("get bead %s: %w", id, err)
	}
	// GetIssue does not populate Dependencies — fetch them separately
	// so that Parent (derived from parent-child deps) is available.
	if issue.Dependencies == nil {
		if depsWithMeta, dErr := store.GetDependenciesWithMetadata(sc.storeCtx, id); dErr == nil {
			for _, dm := range depsWithMeta {
				issue.Dependencies = append(issue.Dependencies, &beads.Dependency{
					IssueID:     id,
					DependsOnID: dm.ID,
					Type:        dm.DependencyType,
				})
			}
		}
	}
	return issueToBead(issue), nil
}

// ListBeads searches for beads matching the given filter.
// Excludes closed beads by default (matching bd list behavior).
func (sc *SpireContext) ListBeads(filter beads.IssueFilter) ([]Bead, error) {
	store, err := sc.EnsureStore()
	if err != nil {
		return nil, err
	}
	if filter.Status == nil && len(filter.ExcludeStatus) == 0 {
		filter.ExcludeStatus = []beads.Status{beads.StatusClosed}
	}
	issues, err := store.SearchIssues(sc.storeCtx, "", filter)
	if err != nil {
		return nil, fmt.Errorf("list beads: %w", err)
	}
	return issuesToBeads(issues), nil
}

// ListBoardBeads searches for beads with full board metadata.
func (sc *SpireContext) ListBoardBeads(filter beads.IssueFilter) ([]BoardBead, error) {
	store, err := sc.EnsureStore()
	if err != nil {
		return nil, err
	}
	if filter.Status == nil && len(filter.ExcludeStatus) == 0 {
		filter.ExcludeStatus = []beads.Status{beads.StatusClosed}
	}
	issues, err := store.SearchIssues(sc.storeCtx, "", filter)
	if err != nil {
		return nil, fmt.Errorf("list board beads: %w", err)
	}
	return issuesToBoardBeads(issues), nil
}

// CreateBead creates a new bead and returns its ID.
func (sc *SpireContext) CreateBead(opts createOpts) (string, error) {
	store, err := sc.EnsureStore()
	if err != nil {
		return "", err
	}
	issue := &beads.Issue{
		Title:       opts.Title,
		Description: opts.Description,
		Priority:    opts.Priority,
		Status:      beads.StatusOpen,
		IssueType:   opts.Type,
		Labels:      opts.Labels,
	}
	if opts.Prefix != "" {
		issue.PrefixOverride = opts.Prefix
	}
	if err := store.CreateIssue(sc.storeCtx, issue, storeActor()); err != nil {
		return "", fmt.Errorf("create bead: %w", err)
	}
	// CreateIssue populates issue.ID
	if opts.Parent != "" {
		dep := &beads.Dependency{
			IssueID:     issue.ID,
			DependsOnID: opts.Parent,
			Type:        beads.DepParentChild,
		}
		if err := store.AddDependency(sc.storeCtx, dep, storeActor()); err != nil {
			return issue.ID, fmt.Errorf("add parent dep for %s: %w", issue.ID, err)
		}
	}
	return issue.ID, nil
}

// AddDep adds a blocking dependency: issueID depends on dependsOnID.
func (sc *SpireContext) AddDep(issueID, dependsOnID string) error {
	return sc.AddDepTyped(issueID, dependsOnID, string(beads.DepBlocks))
}

// AddDepTyped adds a dependency with a specific type.
func (sc *SpireContext) AddDepTyped(issueID, dependsOnID, depType string) error {
	store, err := sc.EnsureStore()
	if err != nil {
		return err
	}
	dep := &beads.Dependency{
		IssueID:     issueID,
		DependsOnID: dependsOnID,
		Type:        beads.DependencyType(depType),
	}
	return store.AddDependency(sc.storeCtx, dep, storeActor())
}

// GetDepsWithMeta returns all dependencies of a bead with their relationship metadata.
func (sc *SpireContext) GetDepsWithMeta(id string) ([]*beads.IssueWithDependencyMetadata, error) {
	store, err := sc.EnsureStore()
	if err != nil {
		return nil, err
	}
	return store.GetDependenciesWithMetadata(sc.storeCtx, id)
}

// CloseBead closes a bead.
func (sc *SpireContext) CloseBead(id string) error {
	store, err := sc.EnsureStore()
	if err != nil {
		return err
	}
	return store.CloseIssue(sc.storeCtx, id, "", storeActor(), "")
}

// UpdateBead updates a bead's fields.
func (sc *SpireContext) UpdateBead(id string, updates map[string]interface{}) error {
	store, err := sc.EnsureStore()
	if err != nil {
		return err
	}
	return store.UpdateIssue(sc.storeCtx, id, updates, storeActor())
}

// AddLabel adds a label to a bead.
func (sc *SpireContext) AddLabel(id, label string) error {
	store, err := sc.EnsureStore()
	if err != nil {
		return err
	}
	return store.AddLabel(sc.storeCtx, id, label, storeActor())
}

// RemoveLabel removes a label from a bead.
func (sc *SpireContext) RemoveLabel(id, label string) error {
	store, err := sc.EnsureStore()
	if err != nil {
		return err
	}
	return store.RemoveLabel(sc.storeCtx, id, label, storeActor())
}

// GetConfig gets a config value. Returns "" if key is not set.
func (sc *SpireContext) GetConfig(key string) (string, error) {
	store, err := sc.EnsureStore()
	if err != nil {
		return "", err
	}
	return store.GetConfig(sc.storeCtx, key)
}

// SetConfig sets a config value.
func (sc *SpireContext) SetConfig(key, val string) error {
	store, err := sc.EnsureStore()
	if err != nil {
		return err
	}
	return store.SetConfig(sc.storeCtx, key, val)
}

// DeleteConfig deletes a config key. Requires configDeleter sub-interface.
func (sc *SpireContext) DeleteConfig(key string) error {
	store, err := sc.EnsureStore()
	if err != nil {
		return err
	}
	cd, ok := store.(configDeleter)
	if !ok {
		return fmt.Errorf("store does not support DeleteConfig")
	}
	return cd.DeleteConfig(sc.storeCtx, key)
}

// GetReadyWork returns beads that are ready to work on (no open blockers).
// Post-filters out workflow step beads and message beads so they don't
// appear as assignable work in the steward cycle.
func (sc *SpireContext) GetReadyWork(filter beads.WorkFilter) ([]Bead, error) {
	store, err := sc.EnsureStore()
	if err != nil {
		return nil, err
	}
	issues, err := store.GetReadyWork(sc.storeCtx, filter)
	if err != nil {
		return nil, fmt.Errorf("get ready work: %w", err)
	}

	result := issuesToBeads(issues)

	// Post-filter: exclude workflow step beads, message beads, design beads,
	// attempt beads, and beads with active attempt children.
	var filtered []Bead
	for _, b := range result {
		// Skip message beads
		if containsLabel(b, "msg") {
			continue
		}
		// Skip design beads (thinking artifacts, not work items)
		if b.Type == "design" {
			continue
		}
		// Skip attempt beads (internal tracking, not assignable work)
		if isAttemptBead(b) {
			continue
		}
		// Skip review-round beads (internal tracking, not assignable work)
		if isReviewRoundBead(b) {
			continue
		}
		// Skip workflow step beads (phase tracking children of work beads)
		if isStepBead(b) {
			continue
		}
		// Skip molecule step beads (parent carries workflow:* label)
		if b.Parent != "" {
			parent, perr := sc.GetBead(b.Parent)
			if perr == nil && hasLabel(parent, "workflow:") != "" {
				continue
			}
		}
		// Skip beads with an active attempt child (someone is already working).
		// Fail closed: if GetActiveAttempt returns an error (e.g. multiple
		// open attempts), quarantine the bead rather than treating it as ready.
		attempt, aErr := sc.GetActiveAttempt(b.ID)
		if aErr != nil {
			log.Printf("[store] quarantining %s (multiple open attempts): %v", b.ID, aErr)
			storeRaiseCorruptedBeadAlertFunc(sc, b.ID, aErr)
			continue
		}
		if attempt != nil {
			continue
		}
		filtered = append(filtered, b)
	}

	return filtered, nil
}

// GetBlockedIssues returns open beads that have unresolved blocking dependencies.
func (sc *SpireContext) GetBlockedIssues(filter beads.WorkFilter) ([]BoardBead, error) {
	store, err := sc.EnsureStore()
	if err != nil {
		return nil, err
	}
	blocked, err := store.GetBlockedIssues(sc.storeCtx, filter)
	if err != nil {
		return nil, fmt.Errorf("get blocked issues: %w", err)
	}
	result := make([]BoardBead, 0, len(blocked))
	for _, bi := range blocked {
		bb := BoardBead{
			ID:              bi.ID,
			Title:           bi.Title,
			Description:     bi.Description,
			Status:          string(bi.Status),
			Priority:        bi.Priority,
			Type:            string(bi.IssueType),
			Owner:           bi.Owner,
			CreatedAt:       bi.CreatedAt.Format(time.RFC3339),
			UpdatedAt:       bi.UpdatedAt.Format(time.RFC3339),
			Labels:          bi.Labels,
			DependencyCount: bi.BlockedByCount,
		}
		for _, blockerID := range bi.BlockedBy {
			bb.Dependencies = append(bb.Dependencies, BoardDep{
				IssueID:     bi.ID,
				DependsOnID: blockerID,
				Type:        "blocks",
			})
		}
		result = append(result, bb)
	}
	return result, nil
}

// GetComments returns comments for a bead.
func (sc *SpireContext) GetComments(id string) ([]*beads.Comment, error) {
	store, err := sc.EnsureStore()
	if err != nil {
		return nil, err
	}
	return store.GetIssueComments(sc.storeCtx, id)
}

// AddComment adds a comment to a bead.
func (sc *SpireContext) AddComment(id, text string) error {
	store, err := sc.EnsureStore()
	if err != nil {
		return err
	}
	_, err = store.AddIssueComment(sc.storeCtx, id, storeActor(), text)
	return err
}

// GetChildren returns child beads of a parent.
func (sc *SpireContext) GetChildren(parentID string) ([]Bead, error) {
	store, err := sc.EnsureStore()
	if err != nil {
		return nil, err
	}
	issues, err := store.SearchIssues(sc.storeCtx, "", beads.IssueFilter{
		ParentID: &parentID,
	})
	if err != nil {
		return nil, fmt.Errorf("get children of %s: %w", parentID, err)
	}
	return issuesToBeads(issues), nil
}

// --- Attempt bead helpers ---

// GetActiveAttempt returns the single open/in_progress attempt child of parentID.
// Returns (nil, nil) if no active attempt exists.
// Returns an error if more than one open attempt exists (invariant violation).
func (sc *SpireContext) GetActiveAttempt(parentID string) (*Bead, error) {
	children, err := sc.GetChildren(parentID)
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

// CreateAttemptBead creates a child attempt bead under parentID.
// Sets status=in_progress and adds labels: attempt, agent:<agentName>, branch:<branch>.
// The model label is only added when model is non-empty.
// Returns the attempt bead ID.
func (sc *SpireContext) CreateAttemptBead(parentID, agentName, model, branch string) (string, error) {
	labels := []string{
		"attempt",
		"agent:" + agentName,
		"branch:" + branch,
	}
	if model != "" {
		labels = append(labels, "model:"+model)
	}
	id, err := sc.CreateBead(createOpts{
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
	if uerr := sc.UpdateBead(id, map[string]interface{}{
		"status": "in_progress",
	}); uerr != nil {
		return id, fmt.Errorf("set attempt in_progress: %w", uerr)
	}
	return id, nil
}

// CreateAttemptBeadAtomic checks for an existing active attempt before
// creating a new one. This narrows the TOCTOU race window.
//
// Returns:
//   - (existingID, nil) if an active attempt by the same agent already exists
//   - (newID, nil) if no active attempt exists and a new one was created
//   - ("", error) if an active attempt by a different agent exists, or on failure
func (sc *SpireContext) CreateAttemptBeadAtomic(parentID, agentName, model, branch string) (string, error) {
	// Check for existing active attempt.
	existing, err := sc.GetActiveAttempt(parentID)
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
	return sc.CreateAttemptBead(parentID, agentName, model, branch)
}

// CloseAttemptBead closes an attempt bead and adds a result comment.
func (sc *SpireContext) CloseAttemptBead(attemptID, result string) error {
	if attemptID == "" {
		return nil
	}
	if result != "" {
		sc.AddComment(attemptID, result)
	}
	return sc.CloseBead(attemptID)
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

// storeGetChildrenFunc is a test-replaceable function for GetChildren.
// In production this stays at its default.
var storeGetChildrenFunc = func(sc *SpireContext, parentID string) ([]Bead, error) {
	return sc.GetChildren(parentID)
}

// storeGetActiveAttemptFunc is a test-replaceable function for GetActiveAttempt.
// In production this stays at its default.
var storeGetActiveAttemptFunc = func(sc *SpireContext, parentID string) (*Bead, error) {
	return sc.GetActiveAttempt(parentID)
}

// storeRaiseCorruptedBeadAlertFunc is a test-replaceable function for RaiseCorruptedBeadAlert.
// In production this stays at its default.
var storeRaiseCorruptedBeadAlertFunc = func(sc *SpireContext, beadID string, violation error) {
	sc.RaiseCorruptedBeadAlert(beadID, violation)
}

// storeCheckExistingAlertFunc checks whether an open corrupted-bead alert already
// exists for beadID. Test-replaceable to avoid needing a real store in unit tests.
var storeCheckExistingAlertFunc = func(sc *SpireContext, beadID string) bool {
	existing, err := sc.ListBeads(beads.IssueFilter{
		Labels: []string{"alert:corrupted-bead", "ref:" + beadID},
	})
	return err == nil && len(existing) > 0
}

// storeCreateAlertFunc creates the alert bead for a corrupted bead.
// Test-replaceable to verify creation is skipped when dedup fires.
var storeCreateAlertFunc = func(sc *SpireContext, beadID, msg string) error {
	_, err := sc.CreateBead(createOpts{
		Title:    msg,
		Priority: 0,
		Type:     beads.TypeTask,
		Labels:   []string{"alert:corrupted-bead", "ref:" + beadID},
	})
	return err
}

// RaiseCorruptedBeadAlert creates a P0 alert bead flagging a bead with
// multiple open attempt children (invariant violation). The caller should
// already have logged the violation and excluded the bead from ready work.
// Alert creation is best-effort: errors are logged, not propagated.
// Deduplication: if an open alert already exists for beadID, no new alert is created.
func (sc *SpireContext) RaiseCorruptedBeadAlert(beadID string, violation error) {
	if storeCheckExistingAlertFunc(sc, beadID) {
		log.Printf("[store] alert already exists for corrupted bead %s, skipping duplicate", beadID)
		return
	}
	msg := fmt.Sprintf("corrupted bead %s: %v", beadID, violation)
	if err := storeCreateAlertFunc(sc, beadID, msg); err != nil {
		log.Printf("[store] failed to raise alert for corrupted bead %s: %v", beadID, err)
	}
}

// --- Review round bead helpers ---

// CreateReviewBead creates a child review-round bead under parentID.
// Sets status=in_progress and adds labels: review-round, sage:<sageName>, round:<N>.
// Returns the review bead ID.
func (sc *SpireContext) CreateReviewBead(parentID, sageName string, round int) (string, error) {
	labels := []string{
		"review-round",
		fmt.Sprintf("sage:%s", sageName),
		fmt.Sprintf("round:%d", round),
	}
	id, err := sc.CreateBead(createOpts{
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
	if uerr := sc.UpdateBead(id, map[string]interface{}{
		"status": "in_progress",
	}); uerr != nil {
		return id, fmt.Errorf("set review bead in_progress: %w", uerr)
	}
	return id, nil
}

// --- Workflow step bead helpers ---

// CreateStepBead creates a child bead representing a workflow step.
// It has type=task, title="step:<stepName>", and labels: [workflow-step, step:<stepName>].
func (sc *SpireContext) CreateStepBead(parentID, stepName string) (string, error) {
	labels := []string{"workflow-step", "step:" + stepName}
	id, err := sc.CreateBead(createOpts{
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

// CloseReviewBead closes a review-round bead and sets its description to verdict+summary.
func (sc *SpireContext) CloseReviewBead(reviewID, verdict, summary string) error {
	if reviewID == "" {
		return nil
	}
	desc := fmt.Sprintf("verdict: %s\n\n%s", verdict, summary)
	if err := sc.UpdateBead(reviewID, map[string]interface{}{
		"description": desc,
	}); err != nil {
		return fmt.Errorf("update review bead description: %w", err)
	}
	return sc.CloseBead(reviewID)
}

// GetReviewBeads returns all review-round child beads of parentID,
// ordered by creation time (via round label, ascending).
func (sc *SpireContext) GetReviewBeads(parentID string) ([]Bead, error) {
	children, err := storeGetChildrenFunc(sc, parentID)
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

// ActivateStepBead sets a step bead to in_progress status.
func (sc *SpireContext) ActivateStepBead(stepID string) error {
	return sc.UpdateBead(stepID, map[string]interface{}{
		"status": "in_progress",
	})
}

// CloseStepBead closes a workflow step bead.
func (sc *SpireContext) CloseStepBead(stepID string) error {
	return sc.CloseBead(stepID)
}

// GetStepBeads returns all workflow-step children of a parent bead, ordered by creation.
func (sc *SpireContext) GetStepBeads(parentID string) ([]Bead, error) {
	children, err := sc.GetChildren(parentID)
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

// GetActiveStep returns the single in_progress step bead for a parent.
// Returns (nil, nil) if no step is active.
// Returns an error if more than one in_progress step exists (invariant violation).
func (sc *SpireContext) GetActiveStep(parentID string) (*Bead, error) {
	steps, err := sc.GetStepBeads(parentID)
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

// CommitPending commits pending dolt changes. Requires pendingCommitter sub-interface.
func (sc *SpireContext) CommitPending(message string) error {
	store, err := sc.EnsureStore()
	if err != nil {
		return err
	}
	pc, ok := store.(pendingCommitter)
	if !ok {
		return fmt.Errorf("store does not support CommitPending")
	}
	_, err = pc.CommitPending(sc.storeCtx, message)
	return err
}

// ============================================================
// Package-level wrapper functions (transitional).
// These delegate to _defaultSctx and will be removed once all
// callers create and pass their own SpireContext.
// ============================================================

func storeGetBead(id string) (Bead, error)                            { return _defaultSctx.GetBead(id) }
func storeListBeads(filter beads.IssueFilter) ([]Bead, error)         { return _defaultSctx.ListBeads(filter) }
func storeListBoardBeads(filter beads.IssueFilter) ([]BoardBead, error) { return _defaultSctx.ListBoardBeads(filter) }
func storeCreateBead(opts createOpts) (string, error)                  { return _defaultSctx.CreateBead(opts) }
func storeAddDep(issueID, dependsOnID string) error                    { return _defaultSctx.AddDep(issueID, dependsOnID) }
func storeAddDepTyped(issueID, dependsOnID, depType string) error      { return _defaultSctx.AddDepTyped(issueID, dependsOnID, depType) }
func storeGetDepsWithMeta(id string) ([]*beads.IssueWithDependencyMetadata, error) { return _defaultSctx.GetDepsWithMeta(id) }
func storeCloseBead(id string) error                                   { return _defaultSctx.CloseBead(id) }
func storeUpdateBead(id string, updates map[string]interface{}) error   { return _defaultSctx.UpdateBead(id, updates) }
func storeAddLabel(id, label string) error                             { return _defaultSctx.AddLabel(id, label) }
func storeRemoveLabel(id, label string) error                          { return _defaultSctx.RemoveLabel(id, label) }
func storeGetConfig(key string) (string, error)                        { return _defaultSctx.GetConfig(key) }
func storeSetConfig(key, val string) error                             { return _defaultSctx.SetConfig(key, val) }
func storeDeleteConfig(key string) error                               { return _defaultSctx.DeleteConfig(key) }
func storeGetReadyWork(filter beads.WorkFilter) ([]Bead, error)        { return _defaultSctx.GetReadyWork(filter) }
func storeGetBlockedIssues(filter beads.WorkFilter) ([]BoardBead, error) { return _defaultSctx.GetBlockedIssues(filter) }
func storeGetComments(id string) ([]*beads.Comment, error)             { return _defaultSctx.GetComments(id) }
func storeAddComment(id, text string) error                            { return _defaultSctx.AddComment(id, text) }
func storeGetChildren(parentID string) ([]Bead, error)                 { return _defaultSctx.GetChildren(parentID) }
func storeGetActiveAttempt(parentID string) (*Bead, error)             { return _defaultSctx.GetActiveAttempt(parentID) }
func storeCreateAttemptBead(parentID, agentName, model, branch string) (string, error) { return _defaultSctx.CreateAttemptBead(parentID, agentName, model, branch) }
func storeCreateAttemptBeadAtomic(parentID, agentName, model, branch string) (string, error) { return _defaultSctx.CreateAttemptBeadAtomic(parentID, agentName, model, branch) }
func storeCloseAttemptBead(attemptID, result string) error             { return _defaultSctx.CloseAttemptBead(attemptID, result) }
func storeCreateReviewBead(parentID, sageName string, round int) (string, error) { return _defaultSctx.CreateReviewBead(parentID, sageName, round) }
func storeCloseReviewBead(reviewID, verdict, summary string) error     { return _defaultSctx.CloseReviewBead(reviewID, verdict, summary) }
func storeGetReviewBeads(parentID string) ([]Bead, error)              { return _defaultSctx.GetReviewBeads(parentID) }
func storeCreateStepBead(parentID, stepName string) (string, error)    { return _defaultSctx.CreateStepBead(parentID, stepName) }
func storeActivateStepBead(stepID string) error                        { return _defaultSctx.ActivateStepBead(stepID) }
func storeCloseStepBead(stepID string) error                           { return _defaultSctx.CloseStepBead(stepID) }
func storeGetStepBeads(parentID string) ([]Bead, error)                { return _defaultSctx.GetStepBeads(parentID) }
func storeGetActiveStep(parentID string) (*Bead, error)                { return _defaultSctx.GetActiveStep(parentID) }
func storeCommitPending(message string) error                          { return _defaultSctx.CommitPending(message) }
func storeRaiseCorruptedBeadAlert(beadID string, violation error)      { _defaultSctx.RaiseCorruptedBeadAlert(beadID, violation) }
