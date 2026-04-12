// Package otel provides a lightweight OTLP gRPC trace receiver that writes
// tool invocation events directly to DuckDB. It replaces the PostToolUse
// hooks-based pipeline with native OpenTelemetry ingestion.
package otel

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/awell-health/spire/pkg/olap"
)

// ToolEvent represents a single tool invocation captured from an OTel span.
type ToolEvent struct {
	SessionID  string
	BeadID     string
	AgentName  string
	Step       string
	ToolName   string
	DurationMs int
	Success    bool
	Timestamp  time.Time
	Tower      string
}

// WriteBatch inserts a batch of tool events into DuckDB in a single transaction.
// It acquires the olap.DB write lock for the duration of the transaction to
// avoid races with ETL and other concurrent writers.
// This is the legacy path for callers that hold a persistent *olap.DB.
func WriteBatch(db *olap.DB, events []ToolEvent) error {
	if len(events) == 0 {
		return nil
	}

	return db.WithWriteLock(func(sqlDB *sql.DB) error {
		tx, err := sqlDB.BeginTx(context.Background(), nil)
		if err != nil {
			return fmt.Errorf("otel write: begin tx: %w", err)
		}
		defer tx.Rollback()

		if err := InsertBatchTx(tx, events); err != nil {
			return err
		}

		return tx.Commit()
	})
}

// WriteBatchPath inserts a batch of tool events using the open→write→close
// pattern via olap.WriteFunc. No persistent DB connection is held.
func WriteBatchPath(dbPath string, events []ToolEvent) error {
	if len(events) == 0 {
		return nil
	}
	return olap.WriteFunc(dbPath, func(tx *sql.Tx) error {
		return InsertBatchTx(tx, events)
	})
}

// InsertBatchTx inserts a batch of tool events inside an existing transaction.
// Used by both WriteBatch (persistent DB) and the daemon's DuckWriter (serialized
// writes via channel). Callers are responsible for begin/commit/rollback.
func InsertBatchTx(tx *sql.Tx, events []ToolEvent) error {
	if len(events) == 0 {
		return nil
	}

	stmt, err := tx.Prepare(`INSERT INTO tool_events
		(session_id, bead_id, agent_name, step, tool_name, duration_ms, success, timestamp, tower)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("otel write: prepare: %w", err)
	}
	defer stmt.Close()

	for _, e := range events {
		ts := e.Timestamp
		if ts.IsZero() {
			ts = time.Now().UTC()
		}
		if _, err := stmt.Exec(e.SessionID, e.BeadID, e.AgentName, e.Step,
			e.ToolName, e.DurationMs, e.Success, ts, e.Tower); err != nil {
			return fmt.Errorf("otel write: exec: %w", err)
		}
	}

	return nil
}
