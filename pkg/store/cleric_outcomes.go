package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

// ClericOutcomesTableSQL creates the cleric_outcomes sidecar table.
//
// One row per cleric round captures the (failure_class, action, gate)
// triple plus the wizard's post-action observation. The promotion /
// demotion learning loop (spi-kl8x5y) walks the most recent finalized
// rows for a (failure_class, action) pair to decide whether the cleric
// should auto-approve a future match or surface it as demoted in the
// prompt context.
//
// Schema notes:
//   - gate is the human review verdict: "approve" | "reject" | "takeover".
//   - target_step is non-empty only for actions that target a specific
//     wizard step (e.g. reset --to <step>); otherwise NULL.
//   - finalized=false rows are pending wizard-observation fills (only the
//     approve path needs this; reject/takeover rows are written
//     finalized=true at handler time).
//   - wizard_post_action_success is a tri-state: NULL (pending),
//     0 (failure), 1 (success).
const ClericOutcomesTableSQL = `CREATE TABLE IF NOT EXISTS cleric_outcomes (
    id VARCHAR(32) NOT NULL PRIMARY KEY,
    recovery_bead_id VARCHAR(64) NOT NULL,
    source_bead_id VARCHAR(64) NOT NULL,
    failure_class VARCHAR(64) NOT NULL,
    action VARCHAR(64) NOT NULL,
    gate VARCHAR(16) NOT NULL,
    target_step VARCHAR(64),
    wizard_post_action_success TINYINT,
    finalized BOOLEAN NOT NULL DEFAULT FALSE,
    created_at DATETIME NOT NULL,
    finalized_at DATETIME,
    INDEX idx_failure_action_finalized (failure_class, action, finalized, finalized_at),
    INDEX idx_source_bead_pending (source_bead_id, finalized)
)`

// ClericOutcome represents a single recorded outcome of a cleric round.
type ClericOutcome struct {
	ID                      string
	RecoveryBeadID          string
	SourceBeadID            string
	FailureClass            string
	Action                  string
	Gate                    string // "approve" | "reject" | "takeover"
	TargetStep              string
	WizardPostActionSuccess sql.NullBool
	Finalized               bool
	CreatedAt               time.Time
	FinalizedAt             sql.NullTime
}

// EnsureClericOutcomesTable creates the cleric_outcomes table if it does
// not exist. Idempotent and safe to call on every startup.
func EnsureClericOutcomesTable(db *sql.DB) error {
	if _, err := db.Exec(ClericOutcomesTableSQL); err != nil {
		return fmt.Errorf("ensure cleric_outcomes table: %w", err)
	}
	return nil
}

// generateOutcomeID returns a random ID in the form "co-" + 8 hex chars.
func generateOutcomeID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "co-00000000"
	}
	return "co-" + hex.EncodeToString(b)
}

// RecordClericOutcome inserts a new cleric_outcomes row. Auto-sets ID and
// CreatedAt when blank. Approve-path rows are typically inserted with
// Finalized=false and updated later by the wizard observer; reject and
// takeover rows are inserted with Finalized=true.
func RecordClericOutcome(ctx context.Context, db *sql.DB, o ClericOutcome) error {
	if o.ID == "" {
		o.ID = generateOutcomeID()
	}
	if o.CreatedAt.IsZero() {
		o.CreatedAt = time.Now().UTC()
	}
	var targetStep interface{}
	if o.TargetStep != "" {
		targetStep = o.TargetStep
	}
	var success interface{}
	if o.WizardPostActionSuccess.Valid {
		if o.WizardPostActionSuccess.Bool {
			success = 1
		} else {
			success = 0
		}
	}
	var finalizedAt interface{}
	if o.Finalized {
		ts := o.CreatedAt
		if o.FinalizedAt.Valid {
			ts = o.FinalizedAt.Time
		}
		finalizedAt = ts.UTC().Format("2006-01-02 15:04:05")
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO cleric_outcomes
		   (id, recovery_bead_id, source_bead_id, failure_class, action,
		    gate, target_step, wizard_post_action_success, finalized,
		    created_at, finalized_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.ID, o.RecoveryBeadID, o.SourceBeadID, o.FailureClass, o.Action,
		o.Gate, targetStep, success, o.Finalized,
		o.CreatedAt.UTC().Format("2006-01-02 15:04:05"),
		finalizedAt,
	)
	if err != nil {
		return fmt.Errorf("insert cleric outcome: %w", err)
	}
	return nil
}

// FinalizeClericOutcome stamps wizard_post_action_success and
// finalized_at on a row by id. Call this from the wizard observer once
// the source bead's next-step transition is known.
func FinalizeClericOutcome(ctx context.Context, db *sql.DB, id string, success bool, finalizedAt time.Time) error {
	successInt := 0
	if success {
		successInt = 1
	}
	_, err := db.ExecContext(ctx,
		`UPDATE cleric_outcomes
		    SET wizard_post_action_success = ?, finalized = TRUE, finalized_at = ?
		  WHERE id = ?`,
		successInt, finalizedAt.UTC().Format("2006-01-02 15:04:05"), id,
	)
	if err != nil {
		return fmt.Errorf("finalize cleric outcome %s: %w", id, err)
	}
	return nil
}

// LastNFinalizedClericOutcomes returns the N most recently finalized rows
// for a (failure_class, action) pair, newest-first. Pending rows
// (finalized=FALSE) are deliberately excluded so an in-flight wizard
// observation does not break a streak.
func LastNFinalizedClericOutcomes(ctx context.Context, db *sql.DB, failureClass, action string, n int) ([]ClericOutcome, error) {
	if n <= 0 {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx,
		`SELECT id, recovery_bead_id, source_bead_id, failure_class, action,
		        gate, target_step, wizard_post_action_success, finalized,
		        created_at, finalized_at
		   FROM cleric_outcomes
		  WHERE failure_class = ? AND action = ? AND finalized = TRUE
		  ORDER BY finalized_at DESC, id DESC
		  LIMIT ?`,
		failureClass, action, n,
	)
	if err != nil {
		return nil, fmt.Errorf("query last-N cleric outcomes: %w", err)
	}
	defer rows.Close()
	return scanClericOutcomes(rows)
}

// PendingClericOutcomesForSourceBead returns every non-finalized row
// linked to a source bead. The wizard observer scans this set on each
// step transition and finalizes any whose target_step matches the
// just-completed step.
func PendingClericOutcomesForSourceBead(ctx context.Context, db *sql.DB, sourceBeadID string) ([]ClericOutcome, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, recovery_bead_id, source_bead_id, failure_class, action,
		        gate, target_step, wizard_post_action_success, finalized,
		        created_at, finalized_at
		   FROM cleric_outcomes
		  WHERE source_bead_id = ? AND finalized = FALSE
		  ORDER BY created_at ASC`,
		sourceBeadID,
	)
	if err != nil {
		return nil, fmt.Errorf("query pending cleric outcomes for %s: %w", sourceBeadID, err)
	}
	defer rows.Close()
	return scanClericOutcomes(rows)
}

// DemotedClericPair is a (failure_class, action) tuple whose most-recent
// 3 finalized outcomes are all rejections. Used by the cleric prompt-
// builder to surface "patterns the human keeps rejecting" as guidance.
type DemotedClericPair struct {
	FailureClass string
	Action       string
}

// ListDemotedClericPairs scans cleric_outcomes for every distinct
// (failure_class, action) and returns those whose most recent
// `threshold` finalized rows are all gate=reject. Order is unspecified.
//
// Walks distinct pairs server-side, then re-queries the last-N for each;
// fine for small data, and the table is bounded by recovery volume.
func ListDemotedClericPairs(ctx context.Context, db *sql.DB, threshold int) ([]DemotedClericPair, error) {
	if threshold <= 0 {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx,
		`SELECT DISTINCT failure_class, action FROM cleric_outcomes WHERE finalized = TRUE`)
	if err != nil {
		return nil, fmt.Errorf("query distinct cleric pairs: %w", err)
	}
	type pair struct{ fc, act string }
	var pairs []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.fc, &p.act); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan distinct cleric pair: %w", err)
		}
		pairs = append(pairs, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate distinct cleric pairs: %w", err)
	}

	var out []DemotedClericPair
	for _, p := range pairs {
		recent, err := LastNFinalizedClericOutcomes(ctx, db, p.fc, p.act, threshold)
		if err != nil {
			return nil, err
		}
		if len(recent) < threshold {
			continue
		}
		allReject := true
		for _, r := range recent {
			if r.Gate != "reject" {
				allReject = false
				break
			}
		}
		if allReject {
			out = append(out, DemotedClericPair{FailureClass: p.fc, Action: p.act})
		}
	}
	return out, nil
}

func scanClericOutcomes(rows *sql.Rows) ([]ClericOutcome, error) {
	var out []ClericOutcome
	for rows.Next() {
		var o ClericOutcome
		var targetStep sql.NullString
		var successInt sql.NullInt64
		var createdAt string
		var finalizedAt sql.NullString
		if err := rows.Scan(
			&o.ID, &o.RecoveryBeadID, &o.SourceBeadID, &o.FailureClass, &o.Action,
			&o.Gate, &targetStep, &successInt, &o.Finalized,
			&createdAt, &finalizedAt,
		); err != nil {
			return nil, fmt.Errorf("scan cleric outcome row: %w", err)
		}
		if targetStep.Valid {
			o.TargetStep = targetStep.String
		}
		if successInt.Valid {
			o.WizardPostActionSuccess = sql.NullBool{Bool: successInt.Int64 != 0, Valid: true}
		}
		o.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
		if finalizedAt.Valid {
			t, _ := time.Parse("2006-01-02 15:04:05", finalizedAt.String)
			o.FinalizedAt = sql.NullTime{Time: t, Valid: true}
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// --- Auto-dispatched wrappers (gateway-mode aware) ---

// RecordClericOutcomeAuto wraps RecordClericOutcome using the active
// store's DB. Gateway mode: cleric_outcomes is a sidecar SQL table owned
// by the local Dolt server; gateway-mode clients have no equivalent
// endpoint, so this fails closed with ErrGatewayUnsupported.
func RecordClericOutcomeAuto(o ClericOutcome) error {
	if _, ok := isGatewayMode(); ok {
		return gatewayUnsupportedErr("RecordClericOutcomeAuto")
	}
	db, err := getDB()
	if err != nil {
		return fmt.Errorf("get db for cleric outcome write: %w", err)
	}
	return RecordClericOutcome(context.Background(), db, o)
}

// FinalizeClericOutcomeAuto wraps FinalizeClericOutcome using the active
// store's DB. Fails closed in gateway mode (see RecordClericOutcomeAuto).
func FinalizeClericOutcomeAuto(id string, success bool, finalizedAt time.Time) error {
	if _, ok := isGatewayMode(); ok {
		return gatewayUnsupportedErr("FinalizeClericOutcomeAuto")
	}
	db, err := getDB()
	if err != nil {
		return fmt.Errorf("get db for cleric outcome finalize: %w", err)
	}
	return FinalizeClericOutcome(context.Background(), db, id, success, finalizedAt)
}

// LastNFinalizedClericOutcomesAuto wraps LastNFinalizedClericOutcomes
// using the active store's DB. Fails closed in gateway mode.
func LastNFinalizedClericOutcomesAuto(failureClass, action string, n int) ([]ClericOutcome, error) {
	if _, ok := isGatewayMode(); ok {
		return nil, gatewayUnsupportedErr("LastNFinalizedClericOutcomesAuto")
	}
	db, err := getDB()
	if err != nil {
		return nil, fmt.Errorf("get db for cleric outcome read: %w", err)
	}
	return LastNFinalizedClericOutcomes(context.Background(), db, failureClass, action, n)
}

// PendingClericOutcomesForSourceBeadAuto wraps
// PendingClericOutcomesForSourceBead using the active store's DB.
// Fails closed in gateway mode.
func PendingClericOutcomesForSourceBeadAuto(sourceBeadID string) ([]ClericOutcome, error) {
	if _, ok := isGatewayMode(); ok {
		return nil, gatewayUnsupportedErr("PendingClericOutcomesForSourceBeadAuto")
	}
	db, err := getDB()
	if err != nil {
		return nil, fmt.Errorf("get db for pending cleric outcomes: %w", err)
	}
	return PendingClericOutcomesForSourceBead(context.Background(), db, sourceBeadID)
}

// ListDemotedClericPairsAuto wraps ListDemotedClericPairs using the
// active store's DB. Fails closed in gateway mode.
func ListDemotedClericPairsAuto(threshold int) ([]DemotedClericPair, error) {
	if _, ok := isGatewayMode(); ok {
		return nil, gatewayUnsupportedErr("ListDemotedClericPairsAuto")
	}
	db, err := getDB()
	if err != nil {
		return nil, fmt.Errorf("get db for demoted cleric pairs: %w", err)
	}
	return ListDemotedClericPairs(context.Background(), db, threshold)
}
