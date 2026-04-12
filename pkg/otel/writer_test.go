package otel

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/olap"
)

func TestWriteBatch_Empty(t *testing.T) {
	db, err := olap.Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Zero events should be a no-op, no error.
	if err := WriteBatch(db, nil); err != nil {
		t.Fatalf("WriteBatch(nil): %v", err)
	}
	if err := WriteBatch(db, []ToolEvent{}); err != nil {
		t.Fatalf("WriteBatch(empty): %v", err)
	}
}

func TestWriteBatch_InsertsEvents(t *testing.T) {
	db, err := olap.Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC().Truncate(time.Second)
	events := []ToolEvent{
		{
			SessionID:  "sess-1",
			BeadID:     "spi-abc",
			AgentName:  "apprentice-spi-abc-0",
			Step:       "implement",
			ToolName:   "Read",
			DurationMs: 50,
			Success:    true,
			Timestamp:  now,
			Tower:      "test-tower",
		},
		{
			SessionID:  "sess-1",
			BeadID:     "spi-abc",
			AgentName:  "apprentice-spi-abc-0",
			Step:       "implement",
			ToolName:   "Edit",
			DurationMs: 120,
			Success:    true,
			Timestamp:  now.Add(time.Second),
			Tower:      "test-tower",
		},
		{
			SessionID:  "sess-1",
			BeadID:     "spi-abc",
			AgentName:  "apprentice-spi-abc-0",
			Step:       "implement",
			ToolName:   "Bash",
			DurationMs: 300,
			Success:    false,
			Timestamp:  now.Add(2 * time.Second),
			Tower:      "test-tower",
		},
	}

	if err := WriteBatch(db, events); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	// Verify rows were inserted.
	ctx := context.Background()
	var count int
	if err := db.SqlDB().QueryRowContext(ctx, "SELECT COUNT(*) FROM tool_events").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 rows, got %d", count)
	}

	// Verify a specific row's data.
	var toolName string
	var durationMs int
	var success bool
	err = db.SqlDB().QueryRowContext(ctx,
		"SELECT tool_name, duration_ms, success FROM tool_events WHERE tool_name = 'Bash'",
	).Scan(&toolName, &durationMs, &success)
	if err != nil {
		t.Fatalf("scan Bash row: %v", err)
	}
	if durationMs != 300 {
		t.Errorf("duration_ms = %d, want 300", durationMs)
	}
	if success {
		t.Error("expected success=false for Bash event")
	}
}

func TestWriteBatch_ZeroTimestampDefaultsToNow(t *testing.T) {
	db, err := olap.Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	before := time.Now().UTC().Add(-time.Second)
	events := []ToolEvent{
		{
			SessionID: "sess-2",
			ToolName:  "Grep",
			// Timestamp left as zero value
		},
	}

	if err := WriteBatch(db, events); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	ctx := context.Background()
	var ts time.Time
	if err := db.SqlDB().QueryRowContext(ctx,
		"SELECT timestamp FROM tool_events WHERE session_id = 'sess-2'",
	).Scan(&ts); err != nil {
		t.Fatalf("scan timestamp: %v", err)
	}

	if ts.Before(before) {
		t.Errorf("timestamp %v is before test start %v", ts, before)
	}
}

// --- InsertToolSpansTx ---

func TestInsertToolSpansTx_Empty(t *testing.T) {
	db, err := olap.Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	err = db.WithWriteLock(func(sqlDB *sql.DB) error {
		tx, err := sqlDB.BeginTx(context.Background(), nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := InsertToolSpansTx(tx, nil); err != nil {
			return err
		}
		return tx.Commit()
	})
	if err != nil {
		t.Fatalf("InsertToolSpansTx(nil): %v", err)
	}
}

func TestInsertToolSpansTx_InsertsSpans(t *testing.T) {
	db, err := olap.Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC().Truncate(time.Second)
	spans := []ToolSpan{
		{
			TraceID:      "abc123",
			SpanID:       "span-1",
			ParentSpanID: "",
			SessionID:    "sess-1",
			BeadID:       "spi-spans",
			AgentName:    "apprentice-0",
			Step:         "implement",
			SpanName:     "Read",
			Kind:         "tool",
			DurationMs:   50,
			Success:      true,
			StartTime:    now,
			EndTime:      now.Add(50 * time.Millisecond),
			Tower:        "test-tower",
			Attributes:   `{"file.path":"/foo/bar.go"}`,
		},
		{
			TraceID:      "abc123",
			SpanID:       "span-2",
			ParentSpanID: "span-1",
			SessionID:    "sess-1",
			BeadID:       "spi-spans",
			AgentName:    "apprentice-0",
			Step:         "implement",
			SpanName:     "llm_request",
			Kind:         "llm_request",
			DurationMs:   200,
			Success:      true,
			StartTime:    now.Add(50 * time.Millisecond),
			EndTime:      now.Add(250 * time.Millisecond),
			Tower:        "test-tower",
			Attributes:   `{}`,
		},
		{
			TraceID:      "abc123",
			SpanID:       "span-3",
			ParentSpanID: "span-1",
			SessionID:    "sess-1",
			BeadID:       "spi-spans",
			AgentName:    "apprentice-0",
			Step:         "implement",
			SpanName:     "Bash",
			Kind:         "tool",
			DurationMs:   100,
			Success:      false,
			StartTime:    now.Add(250 * time.Millisecond),
			EndTime:      now.Add(350 * time.Millisecond),
			Tower:        "test-tower",
			Attributes:   `{"command":"go build"}`,
		},
	}

	err = db.WithWriteLock(func(sqlDB *sql.DB) error {
		tx, err := sqlDB.BeginTx(context.Background(), nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := InsertToolSpansTx(tx, spans); err != nil {
			return err
		}
		return tx.Commit()
	})
	if err != nil {
		t.Fatalf("InsertToolSpansTx: %v", err)
	}

	// Verify rows inserted.
	ctx := context.Background()
	var count int
	if err := db.SqlDB().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM tool_spans WHERE bead_id = 'spi-spans'",
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 spans, got %d", count)
	}

	// Verify specific span data (column order correctness).
	var spanName, kind, parentSpanID string
	var durationMs int
	var success bool
	err = db.SqlDB().QueryRowContext(ctx,
		"SELECT span_name, kind, parent_span_id, duration_ms, success FROM tool_spans WHERE span_id = 'span-3'",
	).Scan(&spanName, &kind, &parentSpanID, &durationMs, &success)
	if err != nil {
		t.Fatalf("scan span-3: %v", err)
	}
	if spanName != "Bash" {
		t.Errorf("span_name = %q, want Bash", spanName)
	}
	if kind != "tool" {
		t.Errorf("kind = %q, want tool", kind)
	}
	if parentSpanID != "span-1" {
		t.Errorf("parent_span_id = %q, want span-1", parentSpanID)
	}
	if durationMs != 100 {
		t.Errorf("duration_ms = %d, want 100", durationMs)
	}
	if success {
		t.Error("expected success=false for span-3")
	}
}

// --- InsertAPIEventsTx ---

func TestInsertAPIEventsTx_Empty(t *testing.T) {
	db, err := olap.Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	err = db.WithWriteLock(func(sqlDB *sql.DB) error {
		tx, err := sqlDB.BeginTx(context.Background(), nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := InsertAPIEventsTx(tx, nil); err != nil {
			return err
		}
		return tx.Commit()
	})
	if err != nil {
		t.Fatalf("InsertAPIEventsTx(nil): %v", err)
	}
}

func TestInsertAPIEventsTx_InsertsEvents(t *testing.T) {
	db, err := olap.Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC().Truncate(time.Second)
	events := []APIEvent{
		{
			SessionID:        "sess-1",
			BeadID:           "spi-api",
			AgentName:        "apprentice-0",
			Step:             "implement",
			Provider:         "claude",
			Model:            "claude-opus-4-6",
			DurationMs:       1500,
			InputTokens:      5000,
			OutputTokens:     2000,
			CacheReadTokens:  3000,
			CacheWriteTokens: 500,
			CostUSD:          0.12,
			Timestamp:        now,
			Tower:            "test-tower",
		},
		{
			SessionID:   "sess-2",
			BeadID:      "spi-api",
			AgentName:   "apprentice-1",
			Step:        "review",
			Provider:    "codex",
			Model:       "codex-mini",
			DurationMs:  800,
			InputTokens: 1000,
			CostUSD:     0.03,
			Timestamp:   now.Add(time.Second),
			Tower:       "test-tower",
		},
	}

	err = db.WithWriteLock(func(sqlDB *sql.DB) error {
		tx, err := sqlDB.BeginTx(context.Background(), nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := InsertAPIEventsTx(tx, events); err != nil {
			return err
		}
		return tx.Commit()
	})
	if err != nil {
		t.Fatalf("InsertAPIEventsTx: %v", err)
	}

	// Verify rows inserted.
	ctx := context.Background()
	var count int
	if err := db.SqlDB().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM api_events WHERE bead_id = 'spi-api'",
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 API events, got %d", count)
	}

	// Verify specific row data (column order correctness).
	var model, provider string
	var durationMs int
	var inputTokens, outputTokens, cacheRead, cacheWrite int64
	var costUSD float64
	err = db.SqlDB().QueryRowContext(ctx,
		`SELECT model, provider, duration_ms, input_tokens, output_tokens,
		        cache_read_tokens, cache_write_tokens, cost_usd
		 FROM api_events WHERE session_id = 'sess-1'`,
	).Scan(&model, &provider, &durationMs, &inputTokens, &outputTokens, &cacheRead, &cacheWrite, &costUSD)
	if err != nil {
		t.Fatalf("scan sess-1: %v", err)
	}
	if model != "claude-opus-4-6" {
		t.Errorf("model = %q, want claude-opus-4-6", model)
	}
	if provider != "claude" {
		t.Errorf("provider = %q, want claude", provider)
	}
	if durationMs != 1500 {
		t.Errorf("duration_ms = %d, want 1500", durationMs)
	}
	if inputTokens != 5000 {
		t.Errorf("input_tokens = %d, want 5000", inputTokens)
	}
	if outputTokens != 2000 {
		t.Errorf("output_tokens = %d, want 2000", outputTokens)
	}
	if cacheRead != 3000 {
		t.Errorf("cache_read_tokens = %d, want 3000", cacheRead)
	}
	if cacheWrite != 500 {
		t.Errorf("cache_write_tokens = %d, want 500", cacheWrite)
	}
	if costUSD != 0.12 {
		t.Errorf("cost_usd = %f, want 0.12", costUSD)
	}
}

func TestInsertAPIEventsTx_ZeroTimestamp(t *testing.T) {
	db, err := olap.Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	before := time.Now().UTC().Add(-time.Second)
	events := []APIEvent{
		{
			SessionID: "sess-zero-ts",
			BeadID:    "spi-api",
			Model:     "test-model",
			// Timestamp left as zero value
		},
	}

	err = db.WithWriteLock(func(sqlDB *sql.DB) error {
		tx, err := sqlDB.BeginTx(context.Background(), nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := InsertAPIEventsTx(tx, events); err != nil {
			return err
		}
		return tx.Commit()
	})
	if err != nil {
		t.Fatalf("InsertAPIEventsTx: %v", err)
	}

	ctx := context.Background()
	var ts time.Time
	if err := db.SqlDB().QueryRowContext(ctx,
		"SELECT timestamp FROM api_events WHERE session_id = 'sess-zero-ts'",
	).Scan(&ts); err != nil {
		t.Fatalf("scan timestamp: %v", err)
	}
	if ts.Before(before) {
		t.Errorf("timestamp %v is before test start %v", ts, before)
	}
}
