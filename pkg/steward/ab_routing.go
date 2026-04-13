package steward

import (
	"context"
	"database/sql"
	"hash/fnv"

	"github.com/awell-health/spire/pkg/store"
)

// ABRouter selects formula variants for beads based on active experiments.
type ABRouter struct{}

// NewABRouter creates an ABRouter.
func NewABRouter() *ABRouter {
	return &ABRouter{}
}

// SelectVariant checks if there's an active experiment for the given formula.
// If yes, uses hash(beadID) % 100 for deterministic assignment:
//   - If hash < trafficSplit*100: return variantB
//   - Else: return variantA
//
// If no active experiment, returns the original formulaName unchanged.
func (r *ABRouter) SelectVariant(ctx context.Context, db *sql.DB, tower, formulaName, beadID string) (string, error) {
	exp, err := store.GetActiveExperiment(ctx, db, tower, formulaName)
	if err != nil {
		return formulaName, err
	}
	if exp == nil {
		return formulaName, nil
	}

	bucket := hashBead(beadID)
	threshold := int(exp.TrafficSplit * 100)
	if bucket < threshold {
		return exp.VariantB, nil
	}
	return exp.VariantA, nil
}

// hashBead returns a deterministic 0-99 value for a bead ID.
// Uses FNV-1a hash for speed and good distribution.
func hashBead(beadID string) int {
	h := fnv.New32a()
	h.Write([]byte(beadID))
	return int(h.Sum32() % 100)
}
