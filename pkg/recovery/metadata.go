package recovery

import (
	"sort"
	"time"

	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// Metadata key constants — single source of truth for all recovery metadata keys.
const (
	KeyFailureClass       = "failure_class"
	KeyFailureSignature   = "failure_signature"
	KeySourceBead         = "source_bead"
	KeySourceFormula      = "source_formula"
	KeySourceStep         = "source_step"
	KeyResolutionKind     = "resolution_kind"
	KeyVerificationStatus = "verification_status"
	KeyLearningKey        = "learning_key"
	KeyReusable           = "reusable"
	KeyResolvedAt         = "resolved_at"
	KeyLearningSummary    = "learning_summary"
	KeyOutcome            = "learning_outcome"   // "clean" / "dirty" / "relapsed"
	KeyLearningID         = "learning_id"        // FK into recovery_learnings table
	KeyExpectedOutcome    = "expected_outcome"   // decide agent's prediction of what should happen
)

// RecoveryMetadata is the typed projection of recovery-specific bead metadata.
// All fields are string-valued except Reusable (bool). ResolvedAt is RFC3339.
type RecoveryMetadata struct {
	FailureClass       string
	FailureSignature   string
	SourceBead         string
	SourceFormula      string
	SourceStep         string
	ResolutionKind     string
	VerificationStatus string
	LearningKey        string
	Reusable           bool
	ResolvedAt         string // RFC3339; empty if unresolved
	LearningSummary    string
	Outcome            string // "clean" / "dirty" / "relapsed"
	LearningID         string // FK into recovery_learnings table
	ExpectedOutcome    string // decide agent's prediction of what should happen
}

// RecoveryMetadataFromBead extracts RecoveryMetadata from a bead's Metadata map.
func RecoveryMetadataFromBead(b store.Bead) RecoveryMetadata {
	return RecoveryMetadata{
		FailureClass:       b.Meta(KeyFailureClass),
		FailureSignature:   b.Meta(KeyFailureSignature),
		SourceBead:         b.Meta(KeySourceBead),
		SourceFormula:      b.Meta(KeySourceFormula),
		SourceStep:         b.Meta(KeySourceStep),
		ResolutionKind:     b.Meta(KeyResolutionKind),
		VerificationStatus: b.Meta(KeyVerificationStatus),
		LearningKey:        b.Meta(KeyLearningKey),
		Reusable:           b.Meta(KeyReusable) == "true",
		ResolvedAt:         b.Meta(KeyResolvedAt),
		LearningSummary:    b.Meta(KeyLearningSummary),
		Outcome:            b.Meta(KeyOutcome),
		LearningID:         b.Meta(KeyLearningID),
		ExpectedOutcome:    b.Meta(KeyExpectedOutcome),
	}
}

// ToMap returns a flat map suitable for store.SetBeadMetadataMap.
// Only non-zero fields are included; Reusable is omitted when false.
func (r RecoveryMetadata) ToMap() map[string]string {
	m := map[string]string{}
	if r.FailureClass != "" {
		m[KeyFailureClass] = r.FailureClass
	}
	if r.FailureSignature != "" {
		m[KeyFailureSignature] = r.FailureSignature
	}
	if r.SourceBead != "" {
		m[KeySourceBead] = r.SourceBead
	}
	if r.SourceFormula != "" {
		m[KeySourceFormula] = r.SourceFormula
	}
	if r.SourceStep != "" {
		m[KeySourceStep] = r.SourceStep
	}
	if r.ResolutionKind != "" {
		m[KeyResolutionKind] = r.ResolutionKind
	}
	if r.VerificationStatus != "" {
		m[KeyVerificationStatus] = r.VerificationStatus
	}
	if r.LearningKey != "" {
		m[KeyLearningKey] = r.LearningKey
	}
	if r.Reusable {
		m[KeyReusable] = "true"
	}
	if r.ResolvedAt != "" {
		m[KeyResolvedAt] = r.ResolvedAt
	}
	if r.LearningSummary != "" {
		m[KeyLearningSummary] = r.LearningSummary
	}
	if r.Outcome != "" {
		m[KeyOutcome] = r.Outcome
	}
	if r.LearningID != "" {
		m[KeyLearningID] = r.LearningID
	}
	if r.ExpectedOutcome != "" {
		m[KeyExpectedOutcome] = r.ExpectedOutcome
	}
	return m
}

// Apply writes all non-zero RecoveryMetadata fields onto the bead's issue
// metadata via the store metadata API.
func (r RecoveryMetadata) Apply(beadID string) error {
	return store.SetBeadMetadataMap(beadID, r.ToMap())
}

// GetRecoveryLearnings returns closed recovery beads whose metadata identifies
// them as reusable learnings for sourceBeadID. The query uses structured
// metadata filters (source_bead + reusable) rather than dependency traversal.
// Results are ordered by resolved_at desc (most recent first).
func GetRecoveryLearnings(sourceBeadID string) ([]store.Bead, error) {
	closedStatus := store.StatusPtr(beads.StatusClosed)
	learnings, err := store.ListBeadsByMetadata(
		map[string]string{
			KeySourceBead: sourceBeadID,
			KeyReusable:   "true",
		},
		func(f *beads.IssueFilter) {
			f.Status = closedStatus
		},
	)
	if err != nil {
		return nil, err
	}
	sort.Slice(learnings, func(i, j int) bool {
		ti, _ := time.Parse(time.RFC3339, learnings[i].Meta(KeyResolvedAt))
		tj, _ := time.Parse(time.RFC3339, learnings[j].Meta(KeyResolvedAt))
		return ti.After(tj)
	})
	return learnings, nil
}

// GetCrossBeadLearnings returns up to limit reusable learnings for failureClass
// across ALL beads (not scoped to a single source bead). Results are ordered by
// resolved_at DESC so the most recent patterns appear first.
func GetCrossBeadLearnings(failureClass string, limit int) ([]store.Bead, error) {
	closedStatus := store.StatusPtr(beads.StatusClosed)
	learnings, err := store.ListBeadsByMetadata(
		map[string]string{
			KeyFailureClass: failureClass,
			KeyReusable:     "true",
		},
		func(f *beads.IssueFilter) {
			f.Status = closedStatus
		},
	)
	if err != nil {
		return nil, err
	}
	sort.Slice(learnings, func(i, j int) bool {
		ti, _ := time.Parse(time.RFC3339, learnings[i].Meta(KeyResolvedAt))
		tj, _ := time.Parse(time.RFC3339, learnings[j].Meta(KeyResolvedAt))
		return ti.After(tj)
	})
	if limit > 0 && len(learnings) > limit {
		learnings = learnings[:limit]
	}
	return learnings, nil
}

// FindMatchingLearning returns the most recent closed recovery bead for
// sourceBeadID whose failure_class matches fc, or nil if none found.
func FindMatchingLearning(sourceBeadID string, fc FailureClass) (*store.Bead, error) {
	closedStatus := store.StatusPtr(beads.StatusClosed)
	learnings, err := store.ListBeadsByMetadata(
		map[string]string{
			KeySourceBead:   sourceBeadID,
			KeyReusable:     "true",
			KeyFailureClass: string(fc),
		},
		func(f *beads.IssueFilter) {
			f.Status = closedStatus
		},
	)
	if err != nil {
		return nil, err
	}
	if len(learnings) == 0 {
		return nil, nil
	}
	// Sort by resolved_at desc and return the most recent.
	sort.Slice(learnings, func(i, j int) bool {
		ti, _ := time.Parse(time.RFC3339, learnings[i].Meta(KeyResolvedAt))
		tj, _ := time.Parse(time.RFC3339, learnings[j].Meta(KeyResolvedAt))
		return ti.After(tj)
	})
	return &learnings[0], nil
}
