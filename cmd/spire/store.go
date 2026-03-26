package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/beads"
)

var (
	activeStore beads.Storage
	storeCtx    context.Context
)

// ensureStore opens a beads store if one isn't already open.
// Uses BEADS_DIR env var or auto-discovers .beads/ directory.
func ensureStore() (beads.Storage, error) {
	if activeStore != nil {
		return activeStore, nil
	}
	beadsDir := resolveBeadsDir()
	if beadsDir == "" {
		return nil, fmt.Errorf("no .beads directory found")
	}
	ctx := context.Background()
	store, err := beads.OpenFromConfig(ctx, beadsDir)
	if err != nil {
		return nil, fmt.Errorf("open beads store: %w", err)
	}
	activeStore = store
	storeCtx = ctx
	return store, nil
}

// openStoreAt opens a beads store at a specific .beads directory.
// Closes any existing store first.
func openStoreAt(beadsDir string) (beads.Storage, error) {
	resetStore()
	ctx := context.Background()
	store, err := beads.OpenFromConfig(ctx, beadsDir)
	if err != nil {
		return nil, fmt.Errorf("open beads store at %s: %w", beadsDir, err)
	}
	activeStore = store
	storeCtx = ctx
	return store, nil
}

// resetStore closes the active store.
func resetStore() {
	if activeStore != nil {
		activeStore.Close()
		activeStore = nil
		storeCtx = nil
	}
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

// --- Convenience helpers ---

// storeGetBead fetches a single bead by ID.
func storeGetBead(id string) (Bead, error) {
	store, err := ensureStore()
	if err != nil {
		return Bead{}, err
	}
	issue, err := store.GetIssue(storeCtx, id)
	if err != nil {
		return Bead{}, fmt.Errorf("get bead %s: %w", id, err)
	}
	// GetIssue does not populate Dependencies — fetch them separately
	// so that Parent (derived from parent-child deps) is available.
	if issue.Dependencies == nil {
		if depsWithMeta, dErr := store.GetDependenciesWithMetadata(storeCtx, id); dErr == nil {
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

// storeListBeads searches for beads matching the given filter.
// Excludes closed beads by default (matching bd list behavior).
func storeListBeads(filter beads.IssueFilter) ([]Bead, error) {
	store, err := ensureStore()
	if err != nil {
		return nil, err
	}
	if filter.Status == nil && len(filter.ExcludeStatus) == 0 {
		filter.ExcludeStatus = []beads.Status{beads.StatusClosed}
	}
	issues, err := store.SearchIssues(storeCtx, "", filter)
	if err != nil {
		return nil, fmt.Errorf("list beads: %w", err)
	}
	return issuesToBeads(issues), nil
}

// storeListBoardBeads searches for beads with full board metadata.
func storeListBoardBeads(filter beads.IssueFilter) ([]BoardBead, error) {
	store, err := ensureStore()
	if err != nil {
		return nil, err
	}
	if filter.Status == nil && len(filter.ExcludeStatus) == 0 {
		filter.ExcludeStatus = []beads.Status{beads.StatusClosed}
	}
	issues, err := store.SearchIssues(storeCtx, "", filter)
	if err != nil {
		return nil, fmt.Errorf("list board beads: %w", err)
	}
	return issuesToBoardBeads(issues), nil
}

// storeCreateBead creates a new bead and returns its ID.
func storeCreateBead(opts createOpts) (string, error) {
	store, err := ensureStore()
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
	if err := store.CreateIssue(storeCtx, issue, storeActor()); err != nil {
		return "", fmt.Errorf("create bead: %w", err)
	}
	// CreateIssue populates issue.ID
	if opts.Parent != "" {
		dep := &beads.Dependency{
			IssueID:     issue.ID,
			DependsOnID: opts.Parent,
			Type:        beads.DepParentChild,
		}
		if err := store.AddDependency(storeCtx, dep, storeActor()); err != nil {
			return issue.ID, fmt.Errorf("add parent dep for %s: %w", issue.ID, err)
		}
	}
	return issue.ID, nil
}

// storeAddDep adds a blocking dependency: issueID depends on dependsOnID.
func storeAddDep(issueID, dependsOnID string) error {
	store, err := ensureStore()
	if err != nil {
		return err
	}
	dep := &beads.Dependency{
		IssueID:     issueID,
		DependsOnID: dependsOnID,
		Type:        beads.DepBlocks,
	}
	return store.AddDependency(storeCtx, dep, storeActor())
}

// storeCloseBead closes a bead.
func storeCloseBead(id string) error {
	store, err := ensureStore()
	if err != nil {
		return err
	}
	return store.CloseIssue(storeCtx, id, "", storeActor(), "")
}

// storeUpdateBead updates a bead's fields.
func storeUpdateBead(id string, updates map[string]interface{}) error {
	store, err := ensureStore()
	if err != nil {
		return err
	}
	return store.UpdateIssue(storeCtx, id, updates, storeActor())
}

// storeAddLabel adds a label to a bead.
func storeAddLabel(id, label string) error {
	store, err := ensureStore()
	if err != nil {
		return err
	}
	return store.AddLabel(storeCtx, id, label, storeActor())
}

// storeRemoveLabel removes a label from a bead.
func storeRemoveLabel(id, label string) error {
	store, err := ensureStore()
	if err != nil {
		return err
	}
	return store.RemoveLabel(storeCtx, id, label, storeActor())
}

// storeGetConfig gets a config value. Returns "" if key is not set.
// Real store errors (connection, missing table) are propagated.
func storeGetConfig(key string) (string, error) {
	store, err := ensureStore()
	if err != nil {
		return "", err
	}
	// beads GetConfig returns ("", nil) for unset keys,
	// so we can pass through directly.
	return store.GetConfig(storeCtx, key)
}

// storeSetConfig sets a config value.
func storeSetConfig(key, val string) error {
	store, err := ensureStore()
	if err != nil {
		return err
	}
	return store.SetConfig(storeCtx, key, val)
}

// storeDeleteConfig deletes a config key. Requires configDeleter sub-interface.
func storeDeleteConfig(key string) error {
	store, err := ensureStore()
	if err != nil {
		return err
	}
	cd, ok := store.(configDeleter)
	if !ok {
		return fmt.Errorf("store does not support DeleteConfig")
	}
	return cd.DeleteConfig(storeCtx, key)
}

// storeGetReadyWork returns beads that are ready to work on (no open blockers).
// Post-filters out workflow step beads and message beads so they don't
// appear as assignable work in the steward cycle.
func storeGetReadyWork(filter beads.WorkFilter) ([]Bead, error) {
	store, err := ensureStore()
	if err != nil {
		return nil, err
	}
	issues, err := store.GetReadyWork(storeCtx, filter)
	if err != nil {
		return nil, fmt.Errorf("get ready work: %w", err)
	}

	result := issuesToBeads(issues)

	// Post-filter: exclude workflow step beads, message beads, and design beads
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
		// Skip workflow step beads (parent carries workflow:* label)
		if b.Parent != "" {
			parent, perr := storeGetBead(b.Parent)
			if perr == nil && hasLabel(parent, "workflow:") != "" {
				continue
			}
		}
		filtered = append(filtered, b)
	}

	return filtered, nil
}

// storeGetBlockedIssues returns open beads that have unresolved blocking dependencies.
func storeGetBlockedIssues(filter beads.WorkFilter) ([]BoardBead, error) {
	store, err := ensureStore()
	if err != nil {
		return nil, err
	}
	blocked, err := store.GetBlockedIssues(storeCtx, filter)
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

// storeGetComments returns comments for a bead.
func storeGetComments(id string) ([]*beads.Comment, error) {
	store, err := ensureStore()
	if err != nil {
		return nil, err
	}
	return store.GetIssueComments(storeCtx, id)
}

// storeAddComment adds a comment to a bead.
func storeAddComment(id, text string) error {
	store, err := ensureStore()
	if err != nil {
		return err
	}
	_, err = store.AddIssueComment(storeCtx, id, storeActor(), text)
	return err
}

// storeGetChildren returns child beads of a parent.
func storeGetChildren(parentID string) ([]Bead, error) {
	store, err := ensureStore()
	if err != nil {
		return nil, err
	}
	issues, err := store.SearchIssues(storeCtx, "", beads.IssueFilter{
		ParentID: &parentID,
	})
	if err != nil {
		return nil, fmt.Errorf("get children of %s: %w", parentID, err)
	}
	return issuesToBeads(issues), nil
}

// storeCommitPending commits pending dolt changes. Requires pendingCommitter sub-interface.
func storeCommitPending(message string) error {
	store, err := ensureStore()
	if err != nil {
		return err
	}
	pc, ok := store.(pendingCommitter)
	if !ok {
		return fmt.Errorf("store does not support CommitPending")
	}
	_, err = pc.CommitPending(storeCtx, message)
	return err
}
