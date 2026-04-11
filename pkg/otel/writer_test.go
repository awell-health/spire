package otel

import (
	"context"
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
