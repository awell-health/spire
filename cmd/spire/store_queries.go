package main

import (
	"fmt"
	"log"
	"time"

	"github.com/steveyegge/beads"
)

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

// storeGetDepsWithMeta returns all dependencies of a bead with their relationship metadata.
func storeGetDepsWithMeta(id string) ([]*beads.IssueWithDependencyMetadata, error) {
	store, err := ensureStore()
	if err != nil {
		return nil, err
	}
	return store.GetDependenciesWithMetadata(storeCtx, id)
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
			parent, perr := storeGetBead(b.Parent)
			if perr == nil && hasLabel(parent, "workflow:") != "" {
				continue
			}
		}
		// Skip beads with an active attempt child (someone is already working).
		// Fail closed: if storeGetActiveAttempt returns an error (e.g. multiple
		// open attempts), quarantine the bead rather than treating it as ready.
		attempt, aErr := storeGetActiveAttempt(b.ID)
		if aErr != nil {
			log.Printf("[store] quarantining %s (multiple open attempts): %v", b.ID, aErr)
			storeRaiseCorruptedBeadAlertFunc(b.ID, aErr)
			continue
		}
		if attempt != nil {
			continue
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
