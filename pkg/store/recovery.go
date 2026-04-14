package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

// RecoveryAttempt represents a single recovery action attempt against a target bead.
type RecoveryAttempt struct {
	ID             string    `json:"id"`
	RecoveryBeadID string    `json:"recovery_bead_id"`
	TargetBeadID   string    `json:"target_bead_id"`
	Action         string    `json:"action"`
	Params         string    `json:"params"`          // JSON-encoded parameters
	Outcome        string    `json:"outcome"`         // "success" | "failure" | "in_progress"
	Error          string    `json:"error"`            // error text, empty on success
	AttemptNumber  int       `json:"attempt_number"`
	CreatedAt      time.Time `json:"created_at"`
}

// generateAttemptID returns a random ID in the form "ra-" + 8 hex chars.
func generateAttemptID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "ra-00000000"
	}
	return "ra-" + hex.EncodeToString(b)
}

// EnsureRecoveryAttemptsTable creates the recovery_attempts table if it does not exist.
func EnsureRecoveryAttemptsTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS recovery_attempts (
		id VARCHAR(64) NOT NULL PRIMARY KEY,
		recovery_bead_id VARCHAR(64) NOT NULL,
		target_bead_id VARCHAR(64) NOT NULL,
		action VARCHAR(128) NOT NULL,
		params TEXT,
		outcome VARCHAR(32) NOT NULL DEFAULT 'in_progress',
		error_text TEXT,
		attempt_number INT NOT NULL DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		INDEX idx_recovery_bead_id (recovery_bead_id)
	)`)
	if err != nil {
		return fmt.Errorf("ensure recovery_attempts table: %w", err)
	}
	return nil
}

// RecordRecoveryAttempt inserts a new recovery attempt. Auto-sets ID and
// CreatedAt if they are empty/zero.
func RecordRecoveryAttempt(db *sql.DB, attempt RecoveryAttempt) error {
	if attempt.ID == "" {
		attempt.ID = generateAttemptID()
	}
	if attempt.CreatedAt.IsZero() {
		attempt.CreatedAt = time.Now().UTC()
	}
	_, err := db.Exec(
		`INSERT INTO recovery_attempts (id, recovery_bead_id, target_bead_id, action, params, outcome, error_text, attempt_number, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		attempt.ID, attempt.RecoveryBeadID, attempt.TargetBeadID,
		attempt.Action, attempt.Params, attempt.Outcome, attempt.Error,
		attempt.AttemptNumber, attempt.CreatedAt.UTC().Format("2006-01-02 15:04:05"),
	)
	if err != nil {
		return fmt.Errorf("insert recovery attempt: %w", err)
	}
	return nil
}

// UpdateAttemptOutcome updates the outcome and error text for an existing attempt.
func UpdateAttemptOutcome(db *sql.DB, attemptID string, outcome string, errText string) error {
	_, err := db.Exec(
		`UPDATE recovery_attempts SET outcome = ?, error_text = ? WHERE id = ?`,
		outcome, errText, attemptID,
	)
	if err != nil {
		return fmt.Errorf("update attempt outcome for %s: %w", attemptID, err)
	}
	return nil
}

// ListRecoveryAttempts returns all attempts for a recovery bead, ordered by
// attempt_number ASC.
func ListRecoveryAttempts(db *sql.DB, recoveryBeadID string) ([]RecoveryAttempt, error) {
	rows, err := db.Query(
		`SELECT id, recovery_bead_id, target_bead_id, action, params, outcome, error_text, attempt_number, created_at
		 FROM recovery_attempts
		 WHERE recovery_bead_id = ?
		 ORDER BY attempt_number ASC`,
		recoveryBeadID,
	)
	if err != nil {
		return nil, fmt.Errorf("list recovery attempts for %s: %w", recoveryBeadID, err)
	}
	defer rows.Close()
	return scanRecoveryAttempts(rows)
}

// GetLatestAttempt returns the most recent attempt for a recovery bead, or nil
// if no attempts exist.
func GetLatestAttempt(db *sql.DB, recoveryBeadID string) (*RecoveryAttempt, error) {
	row := db.QueryRow(
		`SELECT id, recovery_bead_id, target_bead_id, action, params, outcome, error_text, attempt_number, created_at
		 FROM recovery_attempts
		 WHERE recovery_bead_id = ?
		 ORDER BY attempt_number DESC
		 LIMIT 1`,
		recoveryBeadID,
	)
	a, err := scanSingleAttempt(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get latest attempt for %s: %w", recoveryBeadID, err)
	}
	return a, nil
}

// CountAttemptsByAction returns the number of attempts with a specific action
// for a recovery bead.
func CountAttemptsByAction(db *sql.DB, recoveryBeadID string, action string) (int, error) {
	var count int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM recovery_attempts WHERE recovery_bead_id = ? AND action = ?`,
		recoveryBeadID, action,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count attempts by action for %s/%s: %w", recoveryBeadID, action, err)
	}
	return count, nil
}

// scanRecoveryAttempts scans SQL result rows into RecoveryAttempt slices.
func scanRecoveryAttempts(rows *sql.Rows) ([]RecoveryAttempt, error) {
	var out []RecoveryAttempt
	for rows.Next() {
		var a RecoveryAttempt
		var params sql.NullString
		var errText sql.NullString
		var createdAt string
		if err := rows.Scan(
			&a.ID, &a.RecoveryBeadID, &a.TargetBeadID, &a.Action,
			&params, &a.Outcome, &errText, &a.AttemptNumber, &createdAt,
		); err != nil {
			return nil, fmt.Errorf("scan recovery attempt row: %w", err)
		}
		if params.Valid {
			a.Params = params.String
		}
		if errText.Valid {
			a.Error = errText.String
		}
		a.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
		out = append(out, a)
	}
	return out, rows.Err()
}

// scanSingleAttempt scans a single row into a RecoveryAttempt.
func scanSingleAttempt(row *sql.Row) (*RecoveryAttempt, error) {
	var a RecoveryAttempt
	var params sql.NullString
	var errText sql.NullString
	var createdAt string
	err := row.Scan(
		&a.ID, &a.RecoveryBeadID, &a.TargetBeadID, &a.Action,
		&params, &a.Outcome, &errText, &a.AttemptNumber, &createdAt,
	)
	if err != nil {
		return nil, err
	}
	if params.Valid {
		a.Params = params.String
	}
	if errText.Valid {
		a.Error = errText.String
	}
	a.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
	return &a, nil
}
