package store

import (
	"database/sql"
	"log"
	"time"
)

// The bead_lifecycle sidecar table holds first-transition timestamps keyed by
// bead_id. The canonical beads table (owned by github.com/steveyegge/beads)
// does not expose these, so we maintain them out-of-band. Upserts use
// COALESCE for ready_at / started_at so the first event wins — idempotent
// across resumes, ready↔blocked bounces, and retry loops. closed_at always
// overwrites; reopens are out of scope.

// BeadLifecycleTableSQL is the canonical DDL for the bead_lifecycle sidecar
// table. Exported so cmd/spire can run it during tower init and migrations.
const BeadLifecycleTableSQL = `CREATE TABLE IF NOT EXISTS bead_lifecycle (
    bead_id       VARCHAR(64) PRIMARY KEY,
    bead_type     VARCHAR(32),
    filed_at      DATETIME,
    ready_at      DATETIME,
    started_at    DATETIME,
    closed_at     DATETIME,
    updated_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    review_count  INT,
    fix_count     INT,
    arbiter_count INT,
    INDEX idx_bead_type (bead_type),
    INDEX idx_updated_at (updated_at)
)`

// doltDB returns the raw *sql.DB underlying the active beads store when the
// storage backend exposes one. Tests that install a mock storage see ok=false
// and stamping becomes a no-op.
func doltDB() (*sql.DB, bool) {
	if activeStore == nil {
		return nil, false
	}
	type dbAccessor interface{ DB() *sql.DB }
	accessor, ok := activeStore.(dbAccessor)
	if !ok || accessor == nil {
		return nil, false
	}
	db := accessor.DB()
	if db == nil {
		return nil, false
	}
	return db, true
}

// ActiveDB returns the raw *sql.DB underlying the active beads store when the
// storage backend exposes one. It is the exported form of doltDB: callers
// outside pkg/store (e.g. the operator wiring a ClusterIdentityResolver, or
// integration harnesses running SQL migrations) need the connection without
// reaching into activeStore's internals. Returns ok=false when no store is
// open or the backend does not expose a *sql.DB (e.g. test mocks).
func ActiveDB() (*sql.DB, bool) {
	return doltDB()
}

// StampFiled records a bead's initial filing timestamp. Idempotent — once
// filed_at is set, subsequent calls do not overwrite it (COALESCE). Safe to
// call on every create path.
func StampFiled(beadID, beadType string, at time.Time) error {
	db, ok := doltDB()
	if !ok {
		return nil
	}
	_, err := db.Exec(`
        INSERT INTO bead_lifecycle (bead_id, bead_type, filed_at, updated_at)
        VALUES (?, ?, ?, NOW())
        ON DUPLICATE KEY UPDATE
            bead_type = COALESCE(bead_lifecycle.bead_type, VALUES(bead_type)),
            filed_at  = COALESCE(bead_lifecycle.filed_at,  VALUES(filed_at)),
            updated_at = NOW()
    `, beadID, beadType, at.UTC())
	return err
}

// StampReady records the first time a bead entered ready status. Idempotent
// across ready↔blocked bounces — the first transition wins.
func StampReady(beadID string, at time.Time) error {
	db, ok := doltDB()
	if !ok {
		return nil
	}
	_, err := db.Exec(`
        INSERT INTO bead_lifecycle (bead_id, ready_at, updated_at)
        VALUES (?, ?, NOW())
        ON DUPLICATE KEY UPDATE
            ready_at = COALESCE(bead_lifecycle.ready_at, VALUES(ready_at)),
            updated_at = NOW()
    `, beadID, at.UTC())
	return err
}

// StampStarted records the first time a bead entered in_progress status.
// Idempotent across resumes, retries, and attempt reclaims — the first start
// wins. This is load-bearing: queue_seconds = started_at - ready_at depends on
// started_at reflecting the true first execution, not the latest retry.
func StampStarted(beadID string, at time.Time) error {
	db, ok := doltDB()
	if !ok {
		return nil
	}
	_, err := db.Exec(`
        INSERT INTO bead_lifecycle (bead_id, started_at, updated_at)
        VALUES (?, ?, NOW())
        ON DUPLICATE KEY UPDATE
            started_at = COALESCE(bead_lifecycle.started_at, VALUES(started_at)),
            updated_at = NOW()
    `, beadID, at.UTC())
	return err
}

// StampClosed records when a bead was closed. Unlike the other stamps, this
// is last-write-wins — if a bead is reopened and re-closed (out of scope for
// this task but possible), the latest close stamps.
func StampClosed(beadID string, at time.Time) error {
	db, ok := doltDB()
	if !ok {
		return nil
	}
	_, err := db.Exec(`
        INSERT INTO bead_lifecycle (bead_id, closed_at, updated_at)
        VALUES (?, ?, NOW())
        ON DUPLICATE KEY UPDATE
            closed_at = VALUES(closed_at),
            updated_at = NOW()
    `, beadID, at.UTC())
	return err
}

// stampFiledBestEffort calls StampFiled and logs any error without propagating.
// Used from create paths where the bead write has already committed and we
// don't want to surface stamping failures as hard errors.
func stampFiledBestEffort(beadID, beadType string) {
	if err := StampFiled(beadID, beadType, time.Now().UTC()); err != nil {
		log.Printf("[store] lifecycle: stamp filed %s: %v", beadID, err)
	}
}

// stampStatusTransitionBestEffort inspects a status update and stamps the
// matching transition. Unknown statuses are ignored. Errors are logged; the
// SQL is designed to be idempotent so a missed stamp is recoverable by the
// next transition or by an explicit backfill.
func stampStatusTransitionBestEffort(beadID, newStatus string) {
	if beadID == "" || newStatus == "" {
		return
	}
	now := time.Now().UTC()
	var err error
	switch newStatus {
	case "ready":
		err = StampReady(beadID, now)
	case "in_progress":
		err = StampStarted(beadID, now)
	case "closed":
		err = StampClosed(beadID, now)
	case "awaiting_review":
		// No dedicated lifecycle stamp yet — review-time observability is
		// surfaced via recovery-bead metadata. Future work may add a column.
		return
	default:
		return
	}
	if err != nil {
		log.Printf("[store] lifecycle: stamp %s→%s: %v", beadID, newStatus, err)
	}
}

// BackfillBeadLifecycle seeds the sidecar table from the beads library's
// `issues` table on first run. It inserts one row per bead with filed_at =
// created_at and, when status=closed, closed_at = issues.closed_at (the beads
// schema tracks it explicitly). ready_at / started_at stay NULL for
// pre-feature beads — callers must tolerate that in queries and renderers.
//
// Idempotent: INSERT IGNORE means running this repeatedly adds only
// newly-discovered beads. Callers should invoke once per tower startup.
func BackfillBeadLifecycle(db *sql.DB) error {
	if db == nil {
		return nil
	}
	_, err := db.Exec(`
        INSERT IGNORE INTO bead_lifecycle (
            bead_id, bead_type, filed_at, ready_at, started_at, closed_at,
            updated_at, review_count, fix_count, arbiter_count
        )
        SELECT
            id,
            issue_type,
            created_at,
            NULL,
            NULL,
            closed_at,
            COALESCE(updated_at, created_at),
            NULL, NULL, NULL
        FROM issues
    `)
	return err
}
