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

// GetBead fetches a single bead by ID. Routes through the gateway HTTPS
// API when the active tower is in gateway mode; otherwise reads directly
// from the local Dolt-backed store. Returns ErrNotFound when the bead
// does not exist (gateway-side or direct-side — callers match on a single
// sentinel via errors.Is).
func GetBead(id string) (Bead, error) {
	if t, ok := isGatewayMode(); ok {
		return getBeadGateway(t, id)
	}
	return getBeadDirect(id)
}

func getBeadDirect(id string) (Bead, error) {
	issue, err := GetIssue(id)
	if err != nil {
		return Bead{}, err
	}
	return IssueToBead(issue), nil
}

// GetIssue fetches a single bead as a raw beads.Issue by ID. Use this when
// you need fields not exposed on the lightweight Bead projection
// (assignee, acceptance_criteria, created_at, created_by, etc.).
// Dependencies are populated so Parent and related queries work off the
// returned issue.
//
// Gateway mode: the gateway only exposes the lightweight Bead shape over
// /api/v1/beads/{id}; the raw beads.Issue projection is not available.
// Fails closed with ErrGatewayUnsupported so callers either reach for
// GetBead instead or surface the missing transport.
func GetIssue(id string) (*beads.Issue, error) {
	if _, ok := isGatewayMode(); ok {
		return nil, gatewayUnsupportedErr("GetIssue")
	}
	s, ctx, err := getStore()
	if err != nil {
		return nil, err
	}
	issue, err := s.GetIssue(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get bead %s: %w", id, err)
	}
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
	return issue, nil
}

// ListBeads searches for beads matching the given filter. Excludes closed
// beads by default (matching bd list behavior). Routes through the gateway
// HTTPS API when the active tower is in gateway mode; otherwise reads
// directly from the local Dolt-backed store.
func ListBeads(filter beads.IssueFilter) ([]Bead, error) {
	if t, ok := isGatewayMode(); ok {
		return listBeadsGateway(t, filter)
	}
	return listBeadsDirect(filter)
}

func listBeadsDirect(filter beads.IssueFilter) ([]Bead, error) {
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
//
// Gateway mode: no client method yet — fails closed with ErrGatewayUnsupported.
func ListBoardBeads(filter beads.IssueFilter) ([]BoardBead, error) {
	if _, ok := isGatewayMode(); ok {
		return nil, gatewayUnsupportedErr("ListBoardBeads")
	}
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
	PopulateDependencies(ctx, s, issues)
	return IssuesToBoardBeads(issues), nil
}

// GetDepsWithMeta returns all dependencies of a bead with their relationship metadata.
//
// Gateway mode: no client method yet — fails closed with ErrGatewayUnsupported.
// The /api/v1/beads/{id}/deps endpoint returns BoardDep edges via ListDeps,
// not the IssueWithDependencyMetadata projection callers expect here.
func GetDepsWithMeta(id string) ([]*beads.IssueWithDependencyMetadata, error) {
	if _, ok := isGatewayMode(); ok {
		return nil, gatewayUnsupportedErr("GetDepsWithMeta")
	}
	s, ctx, err := getStore()
	if err != nil {
		return nil, err
	}
	return s.GetDependenciesWithMetadata(ctx, id)
}

// GetConfig gets a config value. Returns "" if key is not set.
// Real store errors (connection, missing table) are propagated.
//
// Gateway mode: no client method yet — fails closed with ErrGatewayUnsupported.
func GetConfig(key string) (string, error) {
	if _, ok := isGatewayMode(); ok {
		return "", gatewayUnsupportedErr("GetConfig")
	}
	s, ctx, err := getStore()
	if err != nil {
		return "", err
	}
	// beads GetConfig returns ("", nil) for unset keys,
	// so we can pass through directly.
	return s.GetConfig(ctx, key)
}

// GetReadyWork returns beads that are ready to work on (no open blockers).
// Filters out internal beads (message, step, attempt, review), child beads,
// design beads, and beads with active attempt children so they don't appear
// as assignable work in the steward cycle.
//
// Gateway mode: no client method yet — fails closed with ErrGatewayUnsupported.
// This intentionally blocks the steward's "schedule work" loop on a gateway
// tower; cluster-as-truth deployments run scheduling server-side.
func GetReadyWork(filter beads.WorkFilter) ([]Bead, error) {
	if _, ok := isGatewayMode(); ok {
		return nil, gatewayUnsupportedErr("GetReadyWork")
	}
	// Default to status="ready" when the caller doesn't specify — "ready"
	// is a Spire-specific custom active status registered in the beads
	// `custom_statuses` table, and `beads.Storage.GetReadyWork` hardcodes
	// its default status clause to `status IN ('open', 'in_progress')`.
	// Without this override the query never returns any ready bead, and
	// the steward cycle reports "ready: 0 beads" no matter how many
	// `spire ready`-marked beads exist. Callers who want a different
	// status (e.g. "open") pass it explicitly and we honor their choice.
	if filter.Status == "" {
		// "ready" is not a built-in beads Status constant (v1.0.0 ships
		// only open/in_progress/blocked/deferred/closed/pinned/hooked);
		// it's registered in custom_statuses as category="active".
		filter.Status = beads.Status("ready")
	}

	// SQL-level filtering: exclude internal bead types to reduce row count.
	for t := range InternalTypes {
		filter.ExcludeTypes = append(filter.ExcludeTypes, beads.IssueType(t))
	}

	s, ctx, err := getStore()
	if err != nil {
		return nil, err
	}
	issues, err := s.GetReadyWork(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("get ready work: %w", err)
	}

	result := IssuesToBeads(issues)

	// Post-filter: IsWorkBead is the Go-level safety net for internal type and
	// child-bead exclusion (backs up the SQL filter above). Additional policy
	// filters (deferred, design, active-attempt) are applied below.
	var filtered []Bead
	for _, b := range result {
		// Skip open beads — only status=ready beads should reach the steward.
		// The beads library returns both open and ready (both category=active),
		// but the scheduling path narrows to just ready.
		// TODO: use bd.StatusOpen constant when pkg/store imports pkg/bd
		if b.Status == "open" {
			continue
		}
		// IsWorkBead excludes internal types (message, step, attempt, review)
		// and child beads (Parent != "").
		if !IsWorkBead(b) {
			continue
		}
		// Skip deferred beads (held back from agents until explicitly undeferred)
		if b.Status == "deferred" {
			continue
		}
		// Skip hooked beads (parked waiting for a condition — approval, event, recovery)
		if b.Status == "hooked" {
			continue
		}
		// Skip design beads (thinking artifacts, not work items)
		if b.Type == "design" {
			continue
		}
		// Skip beads with an active attempt child (someone is already working).
		// Fail closed: if GetActiveAttempt returns an error (e.g. multiple
		// open attempts), quarantine the bead rather than treating it as ready.
		attempt, aErr := GetActiveAttempt(b.ID)
		if aErr != nil {
			log.Printf("[store] quarantining %s (multiple open attempts): %v", b.ID, aErr)
			continue
		}
		if attempt != nil {
			continue
		}
		filtered = append(filtered, b)
	}

	return filtered, nil
}

// GetBlockedIssues returns open beads that have unresolved blocking
// dependencies. Routes through the gateway HTTPS API when the active
// tower is in gateway mode; otherwise reads directly from the local
// Dolt-backed store. Gateway mode returns ID-only BoardBead rows today
// (the /api/v1/beads/blocked endpoint does not hydrate bodies).
func GetBlockedIssues(filter beads.WorkFilter) ([]BoardBead, error) {
	if t, ok := isGatewayMode(); ok {
		return getBlockedIssuesGateway(t, filter)
	}
	return getBlockedIssuesDirect(filter)
}

func getBlockedIssuesDirect(filter beads.WorkFilter) ([]BoardBead, error) {
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
//
// Gateway mode: no client method yet — fails closed with ErrGatewayUnsupported.
func GetDependentsWithMeta(id string) ([]*beads.IssueWithDependencyMetadata, error) {
	if _, ok := isGatewayMode(); ok {
		return nil, gatewayUnsupportedErr("GetDependentsWithMeta")
	}
	s, ctx, err := getStore()
	if err != nil {
		return nil, err
	}
	return s.GetDependentsWithMetadata(ctx, id)
}

// GetComments returns comments for a bead.
//
// Gateway mode: no client method yet — fails closed with ErrGatewayUnsupported.
func GetComments(id string) ([]*beads.Comment, error) {
	if _, ok := isGatewayMode(); ok {
		return nil, gatewayUnsupportedErr("GetComments")
	}
	s, ctx, err := getStore()
	if err != nil {
		return nil, err
	}
	return s.GetIssueComments(ctx, id)
}

// GetChildren returns child beads of a parent.
//
// Gateway mode: no client method yet — fails closed with ErrGatewayUnsupported.
func GetChildren(parentID string) ([]Bead, error) {
	if _, ok := isGatewayMode(); ok {
		return nil, gatewayUnsupportedErr("GetChildren")
	}
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
//
// Gateway mode: no client method yet — fails closed with ErrGatewayUnsupported.
func GetChildrenBatch(parentIDs []string) (map[string][]Bead, error) {
	if len(parentIDs) == 0 {
		return map[string][]Bead{}, nil
	}
	if _, ok := isGatewayMode(); ok {
		return nil, gatewayUnsupportedErr("GetChildrenBatch")
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
//
// Gateway mode: no client method yet — fails closed with ErrGatewayUnsupported.
func GetChildrenBoardBatch(parentIDs []string) (map[string][]BoardBead, error) {
	if len(parentIDs) == 0 {
		return map[string][]BoardBead{}, nil
	}
	if _, ok := isGatewayMode(); ok {
		return nil, gatewayUnsupportedErr("GetChildrenBoardBatch")
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
		PopulateDependencies(ctx, s, issues)
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

// BugFilter returns an IssueFilter for bug beads created in the last N days.
// Includes closed beads (bugs are often closed quickly after filing).
func BugFilter(bugType string, days int) beads.IssueFilter {
	t := beads.IssueType(bugType)
	since := time.Now().AddDate(0, 0, -days)
	return beads.IssueFilter{
		IssueType:     &t,
		ExcludeStatus: nil, // include closed bugs
		CreatedAfter:  &since,
	}
}

// DepCausedBy is the dependency type for bug causality tracking.
// A caused-by dep links a bug bead to the work bead that introduced the bug.
const DepCausedBy = "caused-by"

// GetCausedByDeps returns the beads that caused the given bug bead.
// Returns beads linked via caused-by dependency type.
func GetCausedByDeps(bugBeadID string) ([]Bead, error) {
	deps, err := GetDepsWithMeta(bugBeadID)
	if err != nil {
		return nil, fmt.Errorf("get caused-by deps for %s: %w", bugBeadID, err)
	}
	var causing []Bead
	for _, d := range deps {
		if string(d.DependencyType) == DepCausedBy {
			b, err := GetBead(d.ID)
			if err != nil {
				continue // skip unavailable beads
			}
			causing = append(causing, b)
		}
	}
	return causing, nil
}

// GetBugsCausedBy returns bug beads that have a caused-by dep pointing to the given bead.
// This is the reverse lookup: "which bugs did this work bead introduce?"
func GetBugsCausedBy(sourceBeadID string) ([]Bead, error) {
	dependents, err := GetDependentsWithMeta(sourceBeadID)
	if err != nil {
		return nil, fmt.Errorf("get bugs caused by %s: %w", sourceBeadID, err)
	}
	var bugs []Bead
	for _, d := range dependents {
		if string(d.DependencyType) == DepCausedBy {
			b, err := GetBead(d.ID)
			if err != nil {
				continue
			}
			if b.Type == "bug" {
				bugs = append(bugs, b)
			}
		}
	}
	return bugs, nil
}
