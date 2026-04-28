package logartifact

import (
	"context"
	"database/sql"
	"time"

	pkgstore "github.com/awell-health/spire/pkg/store"
)

// CompactionPolicy bounds the work CompactManifests does in one pass.
//
// The policy has TWO independent rules; a manifest row is pruned when
// EITHER applies. Together they bound the table size without
// over-pruning small/active beads:
//
//   - OlderThan: prune rows whose updated_at is older than this. The
//     hard age cap. Zero disables.
//   - PerBeadKeep: keep the N most-recent rows per bead and prune the
//     rest. The recency floor. Zero disables.
//
// CompactManifests does NOT touch the byte store (local filesystem or
// GCS objects). Object retention is owned independently:
//
//   - GCS: bucket lifecycle policy configured out-of-band (gsutil).
//   - Local: filesystem cleanup outside this package.
//   - Cloud Logging: GKE/operator retention setting.
//
// Three retention axes, three owners. CompactManifests only manages
// the tower-side index; pruning a manifest row leaves any associated
// object in place (the GCS lifecycle rule will eventually delete it).
// See docs/cluster-install.md "Three retention axes" section.
type CompactionPolicy struct {
	OlderThan   time.Duration
	PerBeadKeep int
	// Now overrides the wall-clock for tests; zero uses time.Now().
	Now time.Time
}

// CompactManifests prunes manifest rows in db according to policy and
// returns the number of rows deleted. The byte store is NOT touched.
//
// Wired into the steward periodic loop (pkg/steward) at a slow cadence
// — the table is small enough that hourly compaction is enough.
func CompactManifests(ctx context.Context, db *sql.DB, policy CompactionPolicy) (int, error) {
	return pkgstore.CompactLogArtifacts(ctx, db, pkgstore.LogArtifactCompactionPolicy{
		OlderThan:   policy.OlderThan,
		PerBeadKeep: policy.PerBeadKeep,
		Now:         policy.Now,
	})
}
