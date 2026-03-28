package store

import (
	"fmt"
	"log"
	"time"

	"github.com/steveyegge/beads"
)

// GetBead fetches a single bead by ID.
func GetBead(id string) (Bead, error) {
	s, ctx, err := getStore()
	if err != nil {
		return Bead{}, err
	}
	issue, err := s.GetIssue(ctx, id)
	if err != nil {
		return Bead{}, fmt.Errorf("get bead %s: %w", id, err)
	}
	// GetIssue does not populate Dependencies — fetch them separately
	// so that Parent (derived from parent-child deps) is available.
	if issue.Dependencies == nil {
		if depsWithMeta, dErr := s.GetDependenciesWithMetadata(ctx, id); dErr == nil {
			for _, dm := range depsWithMeta {
				issue.Dependencies = append(issue.Dependencies, &beads.Dependency{
					IssueID:     id,
					DependsOnID: dm.ID,
					Type:        dm.DependencyType,
				})
			}
		}
	}
	return IssueToBead(issue), nil
}

// ListBeads searches for beads matching the given filter.
// Excludes closed beads by default (matching bd list behavior).
func ListBeads(filter beads.IssueFilter) ([]Bead, error) {
	s, ctx, err := getStore()
	if err != nil {
		return nil, err
	}
	if filter.Status == nil && len(filter.ExcludeStatus) == 0 {
		filter.ExcludeStatus = []beads.Status{beads.StatusClosed}
	}
	issues, err := s.SearchIssues(ctx, "", filter)
	if err != nil {
		return nil, fmt.Errorf("list beads: %w", err)
	}
	return IssuesToBeads(issues), nil
}

// ListBoardBeads searches for beads with full board metadata.
func ListBoardBeads(filter beads.IssueFilter) ([]BoardBead, error) {
	s, ctx, err := getStore()
	if err != nil {
		return nil, err
	}
	if filter.Status == nil && len(filter.ExcludeStatus) == 0 {
		filter.ExcludeStatus = []beads.Status{beads.StatusClosed}
	}
	issues, err := s.SearchIssues(ctx, "", filter)
	if err != nil {
		return nil, fmt.Errorf("list board beads: %w", err)
	}
	return IssuesToBoardBeads(issues), nil
}

// GetDepsWithMeta returns all dependencies of a bead with their relationship metadata.
func GetDepsWithMeta(id string) ([]*beads.IssueWithDependencyMetadata, error) {
	s, ctx, err := getStore()
	if err != nil {
		return nil, err
	}
	return s.GetDependenciesWithMetadata(ctx, id)
}

// GetConfig gets a config value. Returns "" if key is not set.
// Real store errors (connection, missing table) are propagated.
func GetConfig(key string) (string, error) {
	s, ctx, err := getStore()
	if err != nil {
		return "", err
	}
	// beads GetConfig returns ("", nil) for unset keys,
	// so we can pass through directly.
	return s.GetConfig(ctx, key)
}

// GetReadyWork returns beads that are ready to work on (no open blockers).
// Post-filters out workflow step beads and message beads so they don't
// appear as assignable work in the steward cycle.
func GetReadyWork(filter beads.WorkFilter) ([]Bead, error) {
	s, ctx, err := getStore()
	if err != nil {
		return nil, err
	}
	issues, err := s.GetReadyWork(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("get ready work: %w", err)
	}

	result := IssuesToBeads(issues)

	// Post-filter: exclude workflow step beads, message beads, design beads,
	// attempt beads, and beads with active attempt children.
	var filtered []Bead
	for _, b := range result {
		// Skip message beads
		if ContainsLabel(b, "msg") {
			continue
		}
		// Skip design beads (thinking artifacts, not work items)
		if b.Type == "design" {
			continue
		}
		// Skip attempt beads (internal tracking, not assignable work)
		if IsAttemptBead(b) {
			continue
		}
		// Skip review-round beads (internal tracking, not assignable work)
		if IsReviewRoundBead(b) {
			continue
		}
		// Skip workflow step beads (phase tracking children of work beads)
		if IsStepBead(b) {
			continue
		}
		// Skip molecule step beads (parent carries workflow:* label)
		if b.Parent != "" {
			parent, perr := GetBead(b.Parent)
			if perr == nil && HasLabel(parent, "workflow:") != "" {
				continue
			}
		}
		// Skip beads with an active attempt child (someone is already working).
		// Fail closed: if GetActiveAttempt returns an error (e.g. multiple
		// open attempts), quarantine the bead rather than treating it as ready.
		attempt, aErr := GetActiveAttempt(b.ID)
		if aErr != nil {
			log.Printf("[store] quarantining %s (multiple open attempts): %v", b.ID, aErr)
			// Note: callers that need test-replaceable alert behavior should
			// use the bridge-level storeGetReadyWork wrapper instead.
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
func GetBlockedIssues(filter beads.WorkFilter) ([]BoardBead, error) {
	s, ctx, err := getStore()
	if err != nil {
		return nil, err
	}
	blocked, err := s.GetBlockedIssues(ctx, filter)
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
func GetComments(id string) ([]*beads.Comment, error) {
	s, ctx, err := getStore()
	if err != nil {
		return nil, err
	}
	return s.GetIssueComments(ctx, id)
}

// GetChildren returns child beads of a parent.
func GetChildren(parentID string) ([]Bead, error) {
	s, ctx, err := getStore()
	if err != nil {
		return nil, err
	}
	issues, err := s.SearchIssues(ctx, "", beads.IssueFilter{
		ParentID: &parentID,
	})
	if err != nil {
		return nil, fmt.Errorf("get children of %s: %w", parentID, err)
	}
	return IssuesToBeads(issues), nil
}
