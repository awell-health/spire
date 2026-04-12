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

// ToolEvent represents a single tool invocation captured from an OTel log record or span.
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
	Provider   string // "claude" or "codex"
	EventKind  string // "tool_result", "tool_decision", "user_prompt", etc.
}

// ToolSpan represents a single span from the OTel trace pipeline, stored in the
// tool_spans table for hierarchical waterfall rendering.
type ToolSpan struct {
	TraceID      string
	SpanID       string
	ParentSpanID string
	SessionID    string
	BeadID       string
	AgentName    string
	Step         string
	SpanName     string
	Kind         string // "tool", "llm_request", "interaction"
	DurationMs   int
	Success      bool
	StartTime    time.Time
	EndTime      time.Time
	Tower        string
	Attributes   string // JSON blob
}

// APIEvent represents an LLM API call captured from an OTel log record.
type APIEvent struct {
	SessionID       string
	BeadID          string
	AgentName       string
	Step            string
	Provider        string // "claude" or "codex"
	Model           string
	DurationMs      int
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64
	CacheWriteTokens int64
	CostUSD         float64
	Timestamp       time.Time
	Tower           string
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
		(session_id, bead_id, agent_name, step, tool_name, duration_ms, success, timestamp, tower, provider, event_kind)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
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
			e.ToolName, e.DurationMs, e.Success, ts, e.Tower, e.Provider, e.EventKind); err != nil {
			return fmt.Errorf("otel write: exec: %w", err)
		}
	}

	return nil
}

// InsertToolSpansTx inserts a batch of tool spans inside an existing transaction.
func InsertToolSpansTx(tx *sql.Tx, spans []ToolSpan) error {
	if len(spans) == 0 {
		return nil
	}

	stmt, err := tx.Prepare(`INSERT INTO tool_spans
		(trace_id, span_id, parent_span_id, session_id, bead_id, agent_name, step,
		 span_name, kind, duration_ms, success, start_time, end_time, tower, attributes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("otel write spans: prepare: %w", err)
	}
	defer stmt.Close()

	for _, s := range spans {
		if _, err := stmt.Exec(s.TraceID, s.SpanID, s.ParentSpanID, s.SessionID,
			s.BeadID, s.AgentName, s.Step, s.SpanName, s.Kind, s.DurationMs,
			s.Success, s.StartTime, s.EndTime, s.Tower, s.Attributes); err != nil {
			return fmt.Errorf("otel write spans: exec: %w", err)
		}
	}

	return nil
}

// InsertAPIEventsTx inserts a batch of API events inside an existing transaction.
func InsertAPIEventsTx(tx *sql.Tx, events []APIEvent) error {
	if len(events) == 0 {
		return nil
	}

	stmt, err := tx.Prepare(`INSERT INTO api_events
		(session_id, bead_id, agent_name, step, provider, model, duration_ms,
		 input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
		 cost_usd, timestamp, tower)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("otel write api events: prepare: %w", err)
	}
	defer stmt.Close()

	for _, e := range events {
		ts := e.Timestamp
		if ts.IsZero() {
			ts = time.Now().UTC()
		}
		if _, err := stmt.Exec(e.SessionID, e.BeadID, e.AgentName, e.Step,
			e.Provider, e.Model, e.DurationMs, e.InputTokens, e.OutputTokens,
			e.CacheReadTokens, e.CacheWriteTokens, e.CostUSD, ts, e.Tower); err != nil {
			return fmt.Errorf("otel write api events: exec: %w", err)
		}
	}

	return nil
}
