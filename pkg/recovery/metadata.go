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
	KeySourceFlow         = "source_flow"
	KeyResolutionKind     = "resolution_kind"
	KeyVerificationStatus = "verification_status"
	KeyLearningKey        = "learning_key"
	KeyReusable           = "reusable"
	KeyResolvedAt         = "resolved_at"
	KeyLearningSummary    = "learning_summary"
	KeyOutcome            = "learning_outcome"   // "clean" / "dirty" / "relapsed"
	KeyLearningID         = "learning_id"        // FK into recovery_learnings table
	KeyExpectedOutcome    = "expected_outcome"   // decide agent's prediction of what should happen
	KeyTriageCount        = "triage_count"       // number of triage attempts on this recovery bead (max 2)
)

// RecoveryMetadata is the typed projection of recovery-specific bead metadata.
// All fields are string-valued except Reusable (bool). ResolvedAt is RFC3339.
type RecoveryMetadata struct {
	FailureClass       string
	FailureSignature   string
	SourceBead         string
	SourceFormula      string
	SourceStep         string
	SourceFlow         string
	ResolutionKind     string
	VerificationStatus string
	LearningKey        string
	Reusable           bool
	ResolvedAt         string // RFC3339; empty if unresolved
	LearningSummary    string
	Outcome            string // "clean" / "dirty" / "relapsed"
	LearningID         string // FK into recovery_learnings table
	ExpectedOutcome    string // decide agent's prediction of what should happen
	TriageCount        string // number of triage attempts (max 2)
}

// RecoveryMetadataFromBead extracts RecoveryMetadata from a bead's Metadata map.
func RecoveryMetadataFromBead(b store.Bead) RecoveryMetadata {
	return RecoveryMetadata{
		FailureClass:       b.Meta(KeyFailureClass),
		FailureSignature:   b.Meta(KeyFailureSignature),
		SourceBead:         b.Meta(KeySourceBead),
		SourceFormula:      b.Meta(KeySourceFormula),
		SourceStep:         b.Meta(KeySourceStep),
		SourceFlow:         b.Meta(KeySourceFlow),
		ResolutionKind:     b.Meta(KeyResolutionKind),
		VerificationStatus: b.Meta(KeyVerificationStatus),
		LearningKey:        b.Meta(KeyLearningKey),
		Reusable:           b.Meta(KeyReusable) == "true",
		ResolvedAt:         b.Meta(KeyResolvedAt),
		LearningSummary:    b.Meta(KeyLearningSummary),
		Outcome:            b.Meta(KeyOutcome),
		LearningID:         b.Meta(KeyLearningID),
		ExpectedOutcome:    b.Meta(KeyExpectedOutcome),
		TriageCount:        b.Meta(KeyTriageCount),
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
	if r.SourceFlow != "" {
		m[KeySourceFlow] = r.SourceFlow
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
	if r.TriageCount != "" {
		m[KeyTriageCount] = r.TriageCount
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

// PeerRecoveries returns all recovery beads caused-by sourceBeadID, ordered
// by created_at descending (most recent first). The cleric reads the chain
// through `related` deps to load prior proposals, rejections, takeovers, and
// outcomes; the executor uses this helper to find the most-recent peer to
// link a freshly filed recovery to. Cleric foundation (spi-h2d7yn).
//
// Filters by structured metadata `source_bead == sourceBeadID` rather than
// dep traversal so the helper works even before the caused-by edge has been
// committed (the helper is read-only and should not depend on dep ordering).
func PeerRecoveries(sourceBeadID string) (peers []store.Bead, err error) {
	if sourceBeadID == "" {
		return nil, nil
	}
	// Defensive guard: tests that exercise the escalation path with a nil
	// activeStore (e.g. pkg/executor's seam tests) would otherwise panic
	// inside ListBeads. The peer-link is best-effort — when the store is
	// unreachable, we simply skip the related-dep wiring.
	defer func() {
		if r := recover(); r != nil {
			peers = nil
			err = nil
		}
	}()
	peers, err = store.ListBeadsByMetadata(
		map[string]string{
			KeySourceBead: sourceBeadID,
		},
		nil,
	)
	if err != nil {
		return nil, err
	}
	return filterAndSortRecoveryPeers(peers), nil
}

// filterAndSortRecoveryPeers narrows a metadata-keyed bead list to the
// type=recovery rows and orders them most-recent-first by UpdatedAt. Equal
// timestamps fall back to lexical ID compare (descending) so the ordering is
// deterministic across test runs. Extracted for unit testing — store-backed
// callers go through PeerRecoveries.
func filterAndSortRecoveryPeers(beads []store.Bead) []store.Bead {
	var out []store.Bead
	for _, b := range beads {
		if b.Type != "recovery" {
			continue
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		// store.Bead exposes UpdatedAt but not CreatedAt; for freshly-filed
		// recovery beads (the common case for peer-linking) UpdatedAt ≈
		// CreatedAt.
		ti, _ := time.Parse(time.RFC3339, out[i].UpdatedAt)
		tj, _ := time.Parse(time.RFC3339, out[j].UpdatedAt)
		if ti.Equal(tj) {
			return out[i].ID > out[j].ID
		}
		return ti.After(tj)
	})
	return out
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
