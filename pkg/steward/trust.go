package steward

import (
	"context"
	"database/sql"
	"time"

	"github.com/awell-health/spire/pkg/store"
)

// DefaultPromotionThreshold is the number of consecutive clean merges
// required to advance to the next trust level.
const DefaultPromotionThreshold = 10

// TrustChecker evaluates and updates trust levels for repos.
type TrustChecker struct {
	PromotionThreshold int // default: 10
}

// NewTrustChecker returns a TrustChecker with the default promotion threshold.
func NewTrustChecker() *TrustChecker {
	return &TrustChecker{PromotionThreshold: DefaultPromotionThreshold}
}

// RequiresSageReview returns true if the trust level requires a sage review
// before merge. Levels 0 (sandbox) and 1 (supervised) require review.
func (tc *TrustChecker) RequiresSageReview(level store.TrustLevel) bool {
	return level <= store.TrustSupervised
}

// AllowsAutoMerge returns true if the trust level permits auto-merge
// without human approval. Levels 2 (trusted) and 3 (autonomous).
func (tc *TrustChecker) AllowsAutoMerge(level store.TrustLevel) bool {
	return level >= store.TrustTrusted
}

// RecordAndEvaluate records a merge outcome and evaluates whether the
// trust level should change. Returns the updated record with any level
// changes applied. Promotes after PromotionThreshold consecutive clean
// merges. Demotes by one level on any revert/failure.
func (tc *TrustChecker) RecordAndEvaluate(ctx context.Context, db *sql.DB, tower, prefix string, clean bool) (*store.TrustRecord, error) {
	rec, err := store.RecordMergeOutcome(ctx, db, tower, prefix, clean)
	if err != nil {
		return nil, err
	}

	threshold := tc.PromotionThreshold
	if threshold <= 0 {
		threshold = DefaultPromotionThreshold
	}

	changed := false
	if clean && rec.ConsecutiveClean >= threshold && rec.Level < store.TrustAutonomous {
		rec.Level++
		rec.ConsecutiveClean = 0
		rec.LastChangeAt = time.Now()
		changed = true
	} else if !clean && rec.Level > store.TrustSandbox {
		rec.Level--
		rec.LastChangeAt = time.Now()
		changed = true
	}

	if changed {
		if err := store.UpsertTrustRecord(ctx, db, *rec); err != nil {
			return nil, err
		}
	}

	return rec, nil
}
