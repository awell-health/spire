// Package otel provides a lightweight OTLP gRPC trace receiver that writes
// tool invocation events directly to DuckDB. It replaces the PostToolUse
// hooks-based pipeline with native OpenTelemetry ingestion.
package otel

import (
	"context"
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
func WriteBatch(db *olap.DB, events []ToolEvent) error {
	if len(events) == 0 {
		return nil
	}

	sqlDB := db.SqlDB()
	tx, err := sqlDB.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("otel write: begin tx: %w", err)
	}
	defer tx.Rollback()

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

	return tx.Commit()
}
