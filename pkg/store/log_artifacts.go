package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// AgentLogArtifactsTableSQL is the canonical DDL for the agent_log_artifacts
// table. The table is the tower-side manifest/index for log artifacts whose
// bytes live in an external store (local filesystem or GCS); see design
// spi-7wzwk2 (substrate bead spi-b986in).
//
// The (bead_id, attempt_id, run_id, agent_name, role, phase, provider,
// stream, sequence) tuple is the artifact identity. The unique key on that
// tuple gives idempotent re-uploads: a backend that retries the same
// (identity, sequence) write either upserts or surfaces ErrLogArtifactExists
// rather than producing duplicate manifest rows.
//
// summary and tail are bounded text columns intended for desktop-safe
// previews. The hard byte caps (LogArtifactSummaryMaxBytes,
// LogArtifactTailMaxBytes) are enforced in Go before insert; the column
// type is LONGTEXT to leave headroom for future cap changes without DDL
// drift, but callers must never write more than the cap.
//
// Exported so cmd/spire and pkg/tower can apply the DDL idempotently
// during tower bootstrap.
const AgentLogArtifactsTableSQL = `CREATE TABLE IF NOT EXISTS agent_log_artifacts (
    id                 VARCHAR(64)  NOT NULL PRIMARY KEY,
    tower              VARCHAR(64)  NOT NULL,
    bead_id            VARCHAR(64)  NOT NULL,
    attempt_id         VARCHAR(64)  NOT NULL,
    run_id             VARCHAR(64)  NOT NULL,
    agent_name         VARCHAR(128) NOT NULL,
    role               VARCHAR(32)  NOT NULL,
    phase              VARCHAR(32)  NOT NULL,
    provider           VARCHAR(64)  NOT NULL DEFAULT '',
    stream             VARCHAR(32)  NOT NULL,
    sequence           INT          NOT NULL DEFAULT 0,
    object_uri         VARCHAR(1024) NOT NULL,
    byte_size          BIGINT       NULL,
    checksum           VARCHAR(128) NULL,
    status             VARCHAR(16)  NOT NULL,
    started_at         DATETIME     NULL,
    ended_at           DATETIME     NULL,
    created_at         DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at         DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    redaction_version  INT          NOT NULL DEFAULT 0,
    summary            LONGTEXT     NULL,
    tail               LONGTEXT     NULL,
    UNIQUE KEY uniq_log_artifact_identity (
        bead_id, attempt_id, run_id, agent_name, role, phase, provider, stream, sequence
    ),
    INDEX idx_log_artifact_bead (bead_id),
    INDEX idx_log_artifact_attempt (attempt_id)
)`

// Hard caps for the summary and tail columns. Enforced in Go before insert
// so the manifest never grows into a byte-store. ZFC: Dolt holds identity
// and bounded metadata only; raw transcript bytes go to the artifact store.
const (
	// LogArtifactSummaryMaxBytes caps the summary column at 4 KiB.
	LogArtifactSummaryMaxBytes = 4 * 1024
	// LogArtifactTailMaxBytes caps the tail column at 16 KiB.
	LogArtifactTailMaxBytes = 16 * 1024
)

// Log artifact status values. Mirrors logartifact.Status; defined here so
// pkg/store has no dependency on pkg/logartifact (the dependency is the
// other way around).
const (
	LogArtifactStatusWriting   = "writing"
	LogArtifactStatusFinalized = "finalized"
	LogArtifactStatusFailed    = "failed"
)

// ErrLogArtifactExists is returned by InsertLogArtifact when the unique
// identity tuple already has a manifest row. Callers performing idempotent
// re-uploads should fetch the existing row via GetLogArtifactByIdentity and
// treat it as the live manifest.
var ErrLogArtifactExists = errors.New("store: log artifact already exists for identity")

// LogArtifactRecord mirrors a row in agent_log_artifacts. It is the wire
// shape between pkg/store and any caller (notably pkg/logartifact) that
// holds an open *sql.DB.
type LogArtifactRecord struct {
	ID               string
	Tower            string
	BeadID           string
	AttemptID        string
	RunID            string
	AgentName        string
	Role             string
	Phase            string
	Provider         string
	Stream           string
	Sequence         int
	ObjectURI        string
	ByteSize         *int64 // nil until finalized
	Checksum         string // empty until finalized; format: sha256:<hex>
	Status           string
	StartedAt        *time.Time
	EndedAt          *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
	RedactionVersion int
	Summary          string
	Tail             string
}

// EnsureAgentLogArtifactsTable creates the agent_log_artifacts table if it
// does not exist. Safe to call on every tower startup.
func EnsureAgentLogArtifactsTable(db *sql.DB) error {
	if _, err := db.Exec(AgentLogArtifactsTableSQL); err != nil {
		return fmt.Errorf("ensure agent_log_artifacts table: %w", err)
	}
	return nil
}

// generateLogArtifactID returns a random ID in the form "log-" + 12 hex chars.
func generateLogArtifactID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "log-000000000000"
	}
	return "log-" + hex.EncodeToString(b)
}

// validateLogArtifactBounds rejects records whose summary/tail exceed the
// hard byte caps. Called from InsertLogArtifact before touching the DB so
// oversized previews never reach the manifest.
func validateLogArtifactBounds(rec LogArtifactRecord) error {
	if len(rec.Summary) > LogArtifactSummaryMaxBytes {
		return fmt.Errorf("log artifact summary exceeds %d bytes (got %d)",
			LogArtifactSummaryMaxBytes, len(rec.Summary))
	}
	if len(rec.Tail) > LogArtifactTailMaxBytes {
		return fmt.Errorf("log artifact tail exceeds %d bytes (got %d)",
			LogArtifactTailMaxBytes, len(rec.Tail))
	}
	return nil
}

// InsertLogArtifact inserts a manifest row. Auto-fills ID and CreatedAt /
// UpdatedAt when zero. Returns the row's ID. Callers writing a fresh
// artifact pass status=writing; Finalize updates byte_size/checksum/status.
//
// Returns ErrLogArtifactExists if the (bead_id, attempt_id, run_id,
// agent_name, role, phase, provider, stream, sequence) tuple already has a
// row — callers performing idempotent re-uploads should fetch the existing
// row and reuse it rather than inserting a duplicate.
func InsertLogArtifact(ctx context.Context, db *sql.DB, rec LogArtifactRecord) (string, error) {
	if err := validateLogArtifactBounds(rec); err != nil {
		return "", err
	}
	if rec.ID == "" {
		rec.ID = generateLogArtifactID()
	}
	now := time.Now().UTC()
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	if rec.UpdatedAt.IsZero() {
		rec.UpdatedAt = now
	}
	if rec.Status == "" {
		rec.Status = LogArtifactStatusWriting
	}

	var startedAt, endedAt interface{}
	if rec.StartedAt != nil {
		startedAt = rec.StartedAt.UTC().Format("2006-01-02 15:04:05")
	}
	if rec.EndedAt != nil {
		endedAt = rec.EndedAt.UTC().Format("2006-01-02 15:04:05")
	}
	var byteSize interface{}
	if rec.ByteSize != nil {
		byteSize = *rec.ByteSize
	}
	var checksum interface{}
	if rec.Checksum != "" {
		checksum = rec.Checksum
	}
	var summary, tail interface{}
	if rec.Summary != "" {
		summary = rec.Summary
	}
	if rec.Tail != "" {
		tail = rec.Tail
	}

	_, err := db.ExecContext(ctx,
		`INSERT INTO agent_log_artifacts (
            id, tower, bead_id, attempt_id, run_id, agent_name, role, phase,
            provider, stream, sequence, object_uri, byte_size, checksum,
            status, started_at, ended_at, created_at, updated_at,
            redaction_version, summary, tail
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.Tower, rec.BeadID, rec.AttemptID, rec.RunID, rec.AgentName,
		rec.Role, rec.Phase, rec.Provider, rec.Stream, rec.Sequence, rec.ObjectURI,
		byteSize, checksum, rec.Status, startedAt, endedAt,
		rec.CreatedAt.UTC().Format("2006-01-02 15:04:05"),
		rec.UpdatedAt.UTC().Format("2006-01-02 15:04:05"),
		rec.RedactionVersion, summary, tail,
	)
	if err != nil {
		if isDuplicateKeyError(err) {
			return "", ErrLogArtifactExists
		}
		return "", fmt.Errorf("insert log artifact: %w", err)
	}
	return rec.ID, nil
}

// GetLogArtifact returns the manifest row with the given ID, or nil if no
// such row exists.
func GetLogArtifact(ctx context.Context, db *sql.DB, id string) (*LogArtifactRecord, error) {
	row := db.QueryRowContext(ctx,
		`SELECT `+logArtifactColumns+` FROM agent_log_artifacts WHERE id = ?`,
		id,
	)
	rec, err := scanLogArtifactRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get log artifact %s: %w", id, err)
	}
	return rec, nil
}

// GetLogArtifactByIdentity returns the manifest row matching the full
// identity tuple (bead/attempt/run/agent/role/phase/provider/stream/sequence)
// or nil if no such row exists. Used by callers performing idempotent
// re-uploads to look up the existing row after InsertLogArtifact returned
// ErrLogArtifactExists.
func GetLogArtifactByIdentity(
	ctx context.Context, db *sql.DB,
	beadID, attemptID, runID, agentName, role, phase, provider, stream string,
	sequence int,
) (*LogArtifactRecord, error) {
	row := db.QueryRowContext(ctx,
		`SELECT `+logArtifactColumns+` FROM agent_log_artifacts
         WHERE bead_id = ? AND attempt_id = ? AND run_id = ?
           AND agent_name = ? AND role = ? AND phase = ?
           AND provider = ? AND stream = ? AND sequence = ?`,
		beadID, attemptID, runID, agentName, role, phase, provider, stream, sequence,
	)
	rec, err := scanLogArtifactRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get log artifact by identity: %w", err)
	}
	return rec, nil
}

// ListLogArtifactsForBead returns every manifest row for a bead, ordered by
// (attempt_id, run_id, sequence) ascending so callers see the artifacts in
// the order they were produced.
func ListLogArtifactsForBead(ctx context.Context, db *sql.DB, beadID string) ([]LogArtifactRecord, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT `+logArtifactColumns+` FROM agent_log_artifacts
         WHERE bead_id = ?
         ORDER BY attempt_id ASC, run_id ASC, sequence ASC, created_at ASC`,
		beadID,
	)
	if err != nil {
		return nil, fmt.Errorf("list log artifacts for bead %s: %w", beadID, err)
	}
	defer rows.Close()
	return scanLogArtifactRows(rows)
}

// ListLogArtifactsForAttempt returns every manifest row for an attempt,
// ordered by (run_id, sequence) ascending.
func ListLogArtifactsForAttempt(ctx context.Context, db *sql.DB, attemptID string) ([]LogArtifactRecord, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT `+logArtifactColumns+` FROM agent_log_artifacts
         WHERE attempt_id = ?
         ORDER BY run_id ASC, sequence ASC, created_at ASC`,
		attemptID,
	)
	if err != nil {
		return nil, fmt.Errorf("list log artifacts for attempt %s: %w", attemptID, err)
	}
	defer rows.Close()
	return scanLogArtifactRows(rows)
}

// UpdateLogArtifactStatus updates only the status column (and updated_at).
// Used by exporters to mark an in-flight artifact as failed without
// touching size/checksum.
func UpdateLogArtifactStatus(ctx context.Context, db *sql.DB, id, status string) error {
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	_, err := db.ExecContext(ctx,
		`UPDATE agent_log_artifacts SET status = ?, updated_at = ? WHERE id = ?`,
		status, now, id,
	)
	if err != nil {
		return fmt.Errorf("update log artifact status %s: %w", id, err)
	}
	return nil
}

// FinalizeLogArtifact records the byte size, checksum, ended_at, and sets
// status=finalized in a single update. Optional summary/tail are bounded
// in Go before write; pass empty strings to leave them unset.
//
// Idempotent on already-finalized rows: a second Finalize with the same
// values is a no-op (the UPDATE matches nothing new). Callers that want to
// detect the no-op can read the row's status before calling.
func FinalizeLogArtifact(
	ctx context.Context, db *sql.DB,
	id string, byteSize int64, checksum string, summary, tail string,
) error {
	if len(summary) > LogArtifactSummaryMaxBytes {
		return fmt.Errorf("log artifact summary exceeds %d bytes (got %d)",
			LogArtifactSummaryMaxBytes, len(summary))
	}
	if len(tail) > LogArtifactTailMaxBytes {
		return fmt.Errorf("log artifact tail exceeds %d bytes (got %d)",
			LogArtifactTailMaxBytes, len(tail))
	}
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	var summaryArg, tailArg interface{}
	if summary != "" {
		summaryArg = summary
	}
	if tail != "" {
		tailArg = tail
	}
	_, err := db.ExecContext(ctx,
		`UPDATE agent_log_artifacts SET
            byte_size  = ?,
            checksum   = ?,
            status     = ?,
            ended_at   = ?,
            updated_at = ?,
            summary    = COALESCE(?, summary),
            tail       = COALESCE(?, tail)
         WHERE id = ?`,
		byteSize, checksum, LogArtifactStatusFinalized, now, now,
		summaryArg, tailArg, id,
	)
	if err != nil {
		return fmt.Errorf("finalize log artifact %s: %w", id, err)
	}
	return nil
}

// logArtifactColumns is the column list used by the SELECT helpers. Kept
// in sync with scanLogArtifactRow / scanLogArtifactRows.
const logArtifactColumns = `
    id, tower, bead_id, attempt_id, run_id, agent_name, role, phase,
    provider, stream, sequence, object_uri, byte_size, checksum,
    status, started_at, ended_at, created_at, updated_at,
    redaction_version, summary, tail`

// rowScanner abstracts *sql.Row and *sql.Rows so scanLogArtifact can serve
// both single-row and multi-row paths.
type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanLogArtifactRow(row rowScanner) (*LogArtifactRecord, error) {
	rec := &LogArtifactRecord{}
	var (
		byteSize  sql.NullInt64
		checksum  sql.NullString
		startedAt sql.NullString
		endedAt   sql.NullString
		summary   sql.NullString
		tail      sql.NullString
		createdAt string
		updatedAt string
	)
	err := row.Scan(
		&rec.ID, &rec.Tower, &rec.BeadID, &rec.AttemptID, &rec.RunID,
		&rec.AgentName, &rec.Role, &rec.Phase, &rec.Provider, &rec.Stream,
		&rec.Sequence, &rec.ObjectURI, &byteSize, &checksum,
		&rec.Status, &startedAt, &endedAt, &createdAt, &updatedAt,
		&rec.RedactionVersion, &summary, &tail,
	)
	if err != nil {
		return nil, err
	}
	if byteSize.Valid {
		v := byteSize.Int64
		rec.ByteSize = &v
	}
	if checksum.Valid {
		rec.Checksum = checksum.String
	}
	if startedAt.Valid {
		t, perr := time.Parse("2006-01-02 15:04:05", startedAt.String)
		if perr == nil {
			tt := t
			rec.StartedAt = &tt
		}
	}
	if endedAt.Valid {
		t, perr := time.Parse("2006-01-02 15:04:05", endedAt.String)
		if perr == nil {
			tt := t
			rec.EndedAt = &tt
		}
	}
	if summary.Valid {
		rec.Summary = summary.String
	}
	if tail.Valid {
		rec.Tail = tail.String
	}
	rec.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
	rec.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05", updatedAt)
	return rec, nil
}

func scanLogArtifactRows(rows *sql.Rows) ([]LogArtifactRecord, error) {
	var out []LogArtifactRecord
	for rows.Next() {
		rec, err := scanLogArtifactRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan log artifact row: %w", err)
		}
		out = append(out, *rec)
	}
	return out, rows.Err()
}

// isDuplicateKeyError detects the MySQL/Dolt error returned when an INSERT
// violates a unique constraint. The error message contains "Duplicate entry"
// (canonical MySQL text) — we match on substring rather than driver type so
// the helper works under both real Dolt and the sqlmock test backend.
func isDuplicateKeyError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate entry") ||
		strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "uniqueness") // sqlmock-style fallback
}
