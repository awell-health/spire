package recovery

import (
	"sort"
	"time"

	"github.com/awell-health/spire/pkg/store"
)

// Metadata key constants — single source of truth for all recovery metadata labels.
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
	return m
}

// Apply writes all non-zero RecoveryMetadata fields onto the bead.
func (r RecoveryMetadata) Apply(beadID string) error {
	return store.SetBeadMetadataMap(beadID, r.ToMap())
}

// GetRecoveryLearnings returns closed recovery beads that are dependents of
// parentBeadID (via "caused-by" or "recovery-for" dep for backward compat)
// and have reusable=true. Results are ordered by resolved_at desc (most recent first).
func GetRecoveryLearnings(parentBeadID string) ([]store.Bead, error) {
	deps, err := store.GetDependentsWithMeta(parentBeadID)
	if err != nil {
		return nil, err
	}
	var learnings []store.Bead
	for _, dep := range deps {
		depType := string(dep.DependencyType)
		if depType != "caused-by" && depType != "recovery-for" {
			continue
		}
		if string(dep.Status) != "closed" {
			continue
		}
		b, err := store.GetBead(dep.ID)
		if err != nil {
			continue
		}
		if b.Meta(KeyReusable) != "true" {
			continue
		}
		learnings = append(learnings, b)
	}
	sort.Slice(learnings, func(i, j int) bool {
		ti, _ := time.Parse(time.RFC3339, learnings[i].Meta(KeyResolvedAt))
		tj, _ := time.Parse(time.RFC3339, learnings[j].Meta(KeyResolvedAt))
		return ti.After(tj)
	})
	return learnings, nil
}

// FindMatchingLearning returns the most recent closed recovery bead for
// parentBeadID whose failure_class matches fc, or nil if none found.
func FindMatchingLearning(parentBeadID string, fc FailureClass) (*store.Bead, error) {
	learnings, err := GetRecoveryLearnings(parentBeadID)
	if err != nil {
		return nil, err
	}
	for i := range learnings {
		if learnings[i].Meta(KeyFailureClass) == string(fc) {
			return &learnings[i], nil
		}
	}
	return nil, nil
}
