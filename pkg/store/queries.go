package store

import (
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/steveyegge/beads"
)

// RecoveryLookupFilter specifies criteria for querying closed recovery beads.
// All string fields are optional; empty string means no filter on that field.
type RecoveryLookupFilter struct {
	FailureClass     string
	FailureSignature string
	SourceBead       string
	SourceFormula    string
	SourceStep       string
	ResolutionKind   string
	LearningKey      string
	Reusable         *bool
	Limit            int // 0 = default (10)
}

// RecoveryLearning is a read model for a closed recovery bead's durable outputs,
// populated from bead metadata written by the recovery document/finish lifecycle.
type RecoveryLearning struct {
	BeadID             string
	FailureClass       string
	FailureSignature   string
	SourceBead         string
	SourceFormula      string
	SourceStep         string
	ResolutionKind     string
	VerificationStatus string
	LearningKey        string
	Reusable           bool
	ResolvedAt         string
	LearningSummary    string // from learning_summary metadata key
	Outcome            string // from learning_outcome metadata key ("clean"/"dirty"/"relapsed")
}

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

	// Post-filter: exclude deferred beads, workflow step beads, message beads,
	// design beads, attempt beads, and beads with active attempt children.
	var filtered []Bead
	for _, b := range result {
		// Skip deferred beads (held back from agents until explicitly undeferred)
		if b.Status == "deferred" {
			continue
		}
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

// GetDependentsWithMeta returns all beads that depend on the given bead, with dependency metadata.
// This is the reverse of GetDepsWithMeta: it finds beads where the given bead is the dependency target.
func GetDependentsWithMeta(id string) ([]*beads.IssueWithDependencyMetadata, error) {
	s, ctx, err := getStore()
	if err != nil {
		return nil, err
	}
	return s.GetDependentsWithMetadata(ctx, id)
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

// GetChildrenBatch fetches children for multiple parent IDs and returns them
// grouped by parent ID. It reuses the same SearchIssues + ParentID filter logic
// as GetChildren, ensuring identically-structured Bead values.
func GetChildrenBatch(parentIDs []string) (map[string][]Bead, error) {
	if len(parentIDs) == 0 {
		return map[string][]Bead{}, nil
	}
	s, ctx, err := getStore()
	if err != nil {
		return nil, err
	}
	result := make(map[string][]Bead, len(parentIDs))
	for _, pid := range parentIDs {
		pid := pid
		issues, err := s.SearchIssues(ctx, "", beads.IssueFilter{
			ParentID: &pid,
		})
		if err != nil {
			return nil, fmt.Errorf("get children of %s: %w", pid, err)
		}
		result[pid] = IssuesToBeads(issues)
	}
	return result, nil
}

// GetChildrenBoardBatch fetches children as BoardBeads for multiple parent IDs.
// Returns full timestamp data (CreatedAt, UpdatedAt, ClosedAt) needed for metrics.
//
// NOTE: This performs N+1 queries (one SearchIssues per parent), consistent with
// GetChildrenBatch. For typical DORA windows (28 days) this is acceptable, but
// could be slow with hundreds of parents. A batch filter at the storage layer
// would be the fix if this becomes a bottleneck.
func GetChildrenBoardBatch(parentIDs []string) (map[string][]BoardBead, error) {
	if len(parentIDs) == 0 {
		return map[string][]BoardBead{}, nil
	}
	s, ctx, err := getStore()
	if err != nil {
		return nil, err
	}
	result := make(map[string][]BoardBead, len(parentIDs))
	for _, pid := range parentIDs {
		pid := pid
		issues, err := s.SearchIssues(ctx, "", beads.IssueFilter{
			ParentID: &pid,
		})
		if err != nil {
			return nil, fmt.Errorf("get children of %s: %w", pid, err)
		}
		result[pid] = IssuesToBoardBeads(issues)
	}
	return result, nil
}

// ListClosedRecoveryBeads queries closed recovery beads matching the given filter.
// Filter fields are applied as exact-match predicates against bead metadata.
// Results are ordered by resolved_at descending. Default limit is 10 when 0.
func ListClosedRecoveryBeads(filter RecoveryLookupFilter) ([]RecoveryLearning, error) {
	// Build metadata filter from non-empty fields.
	meta := make(map[string]string)
	if filter.FailureClass != "" {
		meta["failure_class"] = filter.FailureClass
	}
	if filter.FailureSignature != "" {
		meta["failure_signature"] = filter.FailureSignature
	}
	if filter.SourceBead != "" {
		meta["source_bead"] = filter.SourceBead
	}
	if filter.SourceFormula != "" {
		meta["source_formula"] = filter.SourceFormula
	}
	if filter.SourceStep != "" {
		meta["source_step"] = filter.SourceStep
	}
	if filter.ResolutionKind != "" {
		meta["resolution_kind"] = filter.ResolutionKind
	}
	if filter.LearningKey != "" {
		meta["learning_key"] = filter.LearningKey
	}
	if filter.Reusable != nil {
		if *filter.Reusable {
			meta["reusable"] = "true"
		} else {
			meta["reusable"] = "false"
		}
	}

	recoveryType := beads.IssueType("recovery")
	closedStatus := beads.StatusClosed
	results, err := ListBeadsByMetadata(meta, func(f *beads.IssueFilter) {
		f.IssueType = &recoveryType
		f.Status = &closedStatus
		f.ExcludeStatus = nil // override default exclusion of closed
	})
	if err != nil {
		return nil, fmt.Errorf("list closed recovery beads: %w", err)
	}

	// Hydrate RecoveryLearning from each bead's metadata.
	var learnings []RecoveryLearning
	for _, b := range results {
		rl := RecoveryLearning{
			BeadID:             b.ID,
			FailureClass:       b.Meta("failure_class"),
			FailureSignature:   b.Meta("failure_signature"),
			SourceBead:         b.Meta("source_bead"),
			SourceFormula:      b.Meta("source_formula"),
			SourceStep:         b.Meta("source_step"),
			ResolutionKind:     b.Meta("resolution_kind"),
			VerificationStatus: b.Meta("verification_status"),
			LearningKey:        b.Meta("learning_key"),
			Reusable:           b.Meta("reusable") == "true",
			ResolvedAt:         b.Meta("resolved_at"),
			LearningSummary:    b.Meta("learning_summary"),
			Outcome:            b.Meta("learning_outcome"),
		}
		learnings = append(learnings, rl)
	}

	// Sort by resolved_at descending (ISO dates sort lexically).
	sort.Slice(learnings, func(i, j int) bool {
		return learnings[i].ResolvedAt > learnings[j].ResolvedAt
	})

	// Apply limit.
	limit := filter.Limit
	if limit <= 0 {
		limit = 10
	}
	if len(learnings) > limit {
		learnings = learnings[:limit]
	}

	return learnings, nil
}
