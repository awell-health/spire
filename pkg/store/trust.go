package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// TrustLevel represents a repo's earned trust. Higher = more autonomy.
type TrustLevel int

const (
	TrustSandbox    TrustLevel = 0 // heavy review, no auto-merge
	TrustSupervised TrustLevel = 1 // sage review required, human approves merge
	TrustTrusted    TrustLevel = 2 // sage review, auto-merge if approved
	TrustAutonomous TrustLevel = 3 // auto-merge, human notified
)

// TrustLevelName returns a human-readable name for a trust level.
func TrustLevelName(level TrustLevel) string {
	switch level {
	case TrustSandbox:
		return "sandbox"
	case TrustSupervised:
		return "supervised"
	case TrustTrusted:
		return "trusted"
	case TrustAutonomous:
		return "autonomous"
	default:
		return fmt.Sprintf("unknown(%d)", int(level))
	}
}

// TrustRecord stores the trust state for a single repo within a tower.
type TrustRecord struct {
	RepoPrefix       string
	Tower            string
	Level            TrustLevel
	ConsecutiveClean int       // resets to 0 on any failure
	TotalMerges      int
	TotalReverts     int
	LastChangeAt     time.Time // last level change
	UpdatedAt        time.Time
}

// EnsureTrustTable creates the trust_levels table if it doesn't exist.
// Call this during steward startup or store initialization.
func EnsureTrustTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS trust_levels (
		repo_prefix VARCHAR(32) NOT NULL,
		tower VARCHAR(64) NOT NULL,
		level INT NOT NULL DEFAULT 0,
		consecutive_clean INT NOT NULL DEFAULT 0,
		total_merges INT NOT NULL DEFAULT 0,
		total_reverts INT NOT NULL DEFAULT 0,
		last_change_at DATETIME,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (repo_prefix, tower)
	)`)
	if err != nil {
		return fmt.Errorf("ensure trust_levels table: %w", err)
	}
	return nil
}

// GetTrustRecord returns the trust record for a repo, or a zero-value
// record (TrustSandbox) if none exists.
func GetTrustRecord(ctx context.Context, db *sql.DB, tower, prefix string) (*TrustRecord, error) {
	rec := &TrustRecord{
		RepoPrefix: prefix,
		Tower:      tower,
	}
	var lastChangeAt sql.NullString
	var updatedAt sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT level, consecutive_clean, total_merges, total_reverts, last_change_at, updated_at
		 FROM trust_levels WHERE tower = ? AND repo_prefix = ?`,
		tower, prefix,
	).Scan(&rec.Level, &rec.ConsecutiveClean, &rec.TotalMerges, &rec.TotalReverts, &lastChangeAt, &updatedAt)
	if err == sql.ErrNoRows {
		return rec, nil // zero-value record at TrustSandbox
	}
	if err != nil {
		return nil, fmt.Errorf("get trust record: %w", err)
	}
	if lastChangeAt.Valid {
		rec.LastChangeAt, _ = time.Parse("2006-01-02 15:04:05", lastChangeAt.String)
	}
	if updatedAt.Valid {
		rec.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05", updatedAt.String)
	}
	return rec, nil
}

// UpsertTrustRecord creates or updates a trust record.
func UpsertTrustRecord(ctx context.Context, db *sql.DB, rec TrustRecord) error {
	lastChange := rec.LastChangeAt.UTC().Format("2006-01-02 15:04:05")
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	_, err := db.ExecContext(ctx,
		`INSERT INTO trust_levels (repo_prefix, tower, level, consecutive_clean, total_merges, total_reverts, last_change_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE
		     level             = VALUES(level),
		     consecutive_clean = VALUES(consecutive_clean),
		     total_merges      = VALUES(total_merges),
		     total_reverts     = VALUES(total_reverts),
		     last_change_at    = VALUES(last_change_at),
		     updated_at        = VALUES(updated_at)`,
		rec.RepoPrefix, rec.Tower, rec.Level, rec.ConsecutiveClean,
		rec.TotalMerges, rec.TotalReverts, lastChange, now,
	)
	if err != nil {
		return fmt.Errorf("upsert trust record: %w", err)
	}
	return nil
}

// ListTrustRecords returns all trust records for a tower.
func ListTrustRecords(ctx context.Context, db *sql.DB, tower string) ([]TrustRecord, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT repo_prefix, tower, level, consecutive_clean, total_merges, total_reverts, last_change_at, updated_at
		 FROM trust_levels WHERE tower = ? ORDER BY repo_prefix`,
		tower,
	)
	if err != nil {
		return nil, fmt.Errorf("list trust records: %w", err)
	}
	defer rows.Close()
	var out []TrustRecord
	for rows.Next() {
		var r TrustRecord
		var lastChangeAt sql.NullString
		var updatedAt sql.NullString
		if err := rows.Scan(&r.RepoPrefix, &r.Tower, &r.Level, &r.ConsecutiveClean,
			&r.TotalMerges, &r.TotalReverts, &lastChangeAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan trust record: %w", err)
		}
		if lastChangeAt.Valid {
			r.LastChangeAt, _ = time.Parse("2006-01-02 15:04:05", lastChangeAt.String)
		}
		if updatedAt.Valid {
			r.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05", updatedAt.String)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecordMergeOutcome increments counters and returns the new trust record.
// On clean=true: increments consecutive_clean and total_merges.
// On clean=false: resets consecutive_clean to 0, increments total_reverts.
func RecordMergeOutcome(ctx context.Context, db *sql.DB, tower, prefix string, clean bool) (*TrustRecord, error) {
	rec, err := GetTrustRecord(ctx, db, tower, prefix)
	if err != nil {
		return nil, err
	}
	if clean {
		rec.ConsecutiveClean++
		rec.TotalMerges++
	} else {
		rec.ConsecutiveClean = 0
		rec.TotalReverts++
	}
	rec.UpdatedAt = time.Now()
	if err := UpsertTrustRecord(ctx, db, *rec); err != nil {
		return nil, err
	}
	return rec, nil
}
