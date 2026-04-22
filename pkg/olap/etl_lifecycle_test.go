//go:build cgo

package olap

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"
)

// openMockDoltLifecycle spins up an in-memory DuckDB acting as a stand-in for
// the Dolt source and creates a bead_lifecycle table matching the columns
// queryDoltBeadLifecycle reads. DuckDB's standard-SQL tuple comparison and
// DATETIME semantics are a faithful enough stand-in for the Dolt/MySQL shape
// that ETL exercises.
func openMockDoltLifecycle(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatalf("open mock dolt: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE bead_lifecycle (
		bead_id       VARCHAR PRIMARY KEY,
		bead_type     VARCHAR,
		filed_at      TIMESTAMP,
		ready_at      TIMESTAMP,
		started_at    TIMESTAMP,
		closed_at     TIMESTAMP,
		updated_at    TIMESTAMP NOT NULL,
		review_count  INTEGER,
		fix_count     INTEGER,
		arbiter_count INTEGER
	)`); err != nil {
		t.Fatalf("create mock bead_lifecycle: %v", err)
	}
	return db
}

// TestSyncBeadLifecycle_BoundaryAtDatetimeSecond covers the failure mode that
// motivated the composite-cursor fix: more than 500 rows share one updated_at
// second, and a timestamp-only cursor would either stall (>=) or drop the
// overflow (>). The composite cursor must drain every row across iterations
// and then report zero work on the next call.
func TestSyncBeadLifecycle_BoundaryAtDatetimeSecond(t *testing.T) {
	olapDB, err := Open("")
	if err != nil {
		t.Fatalf("Open olap: %v", err)
	}
	defer olapDB.Close()

	mockDolt := openMockDoltLifecycle(t)

	// 501 rows is the minimum fixture that exercises the boundary (page size is
	// 500); larger counts add no extra coverage.
	const rowCount = 501
	sharedTS := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	for i := 0; i < rowCount; i++ {
		beadID := fmt.Sprintf("spi-%06d", i)
		if _, err := mockDolt.Exec(`INSERT INTO bead_lifecycle (
			bead_id, bead_type, filed_at, updated_at
		) VALUES (?, 'task', ?, ?)`, beadID, sharedTS, sharedTS); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}

	ctx := context.Background()
	etl := NewETL(olapDB)

	// Drain the ETL. Bounded iterations catch the silent-stall failure mode
	// (current code would loop forever returning the same 500 rows).
	const maxIters = 5
	perIter := make([]int, 0, maxIters)
	totalSynced := 0
	for i := 0; i < maxIters; i++ {
		n, err := etl.SyncBeadLifecycle(ctx, mockDolt)
		if err != nil {
			t.Fatalf("SyncBeadLifecycle iter %d: %v", i, err)
		}
		perIter = append(perIter, n)
		totalSynced += n
		if n == 0 {
			break
		}
	}
	if len(perIter) == 0 || perIter[len(perIter)-1] != 0 {
		t.Fatalf("ETL did not terminate after %d iterations: per-iter=%v", maxIters, perIter)
	}
	if totalSynced < rowCount {
		t.Errorf("total rows synced = %d, want at least %d (per-iter=%v)",
			totalSynced, rowCount, perIter)
	}

	// Every distinct bead_id must be in the target; none may be missing.
	var gotCount int
	if err := olapDB.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM bead_lifecycle_olap",
	).Scan(&gotCount); err != nil {
		t.Fatalf("count bead_lifecycle_olap: %v", err)
	}
	if gotCount != rowCount {
		t.Errorf("bead_lifecycle_olap row count = %d, want %d", gotCount, rowCount)
	}

	rows, err := olapDB.db.QueryContext(ctx,
		"SELECT bead_id FROM bead_lifecycle_olap ORDER BY bead_id",
	)
	if err != nil {
		t.Fatalf("select bead_ids: %v", err)
	}
	defer rows.Close()
	seen := make(map[string]bool, rowCount)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan id: %v", err)
		}
		seen[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	if len(seen) != rowCount {
		t.Errorf("distinct bead_ids in target = %d, want %d", len(seen), rowCount)
		for i := 0; i < rowCount; i++ {
			id := fmt.Sprintf("spi-%06d", i)
			if !seen[id] {
				t.Errorf("missing bead_id: %s", id)
				if len(seen) < rowCount-5 {
					break
				}
			}
		}
	}

	// The cursor must now serialize the composite (timestamp, last_bead_id)
	// form. Confirms the cursor we persist is the one the next run will read.
	var cursorVal string
	if err := olapDB.db.QueryRowContext(ctx,
		"SELECT last_id FROM etl_cursor WHERE table_name = 'bead_lifecycle'",
	).Scan(&cursorVal); err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	wantCursor := lifecycleCursor{
		UpdatedAt: sharedTS,
		BeadID:    fmt.Sprintf("spi-%06d", rowCount-1),
	}.String()
	if cursorVal != wantCursor {
		t.Errorf("cursor after drain = %q, want %q", cursorVal, wantCursor)
	}

	// Running again with no new rows must be a no-op (zero work, cursor
	// unchanged). This is the property that breaks under the old timestamp-
	// only cursor when many rows share a second.
	n, err := etl.SyncBeadLifecycle(ctx, mockDolt)
	if err != nil {
		t.Fatalf("SyncBeadLifecycle idle: %v", err)
	}
	if n != 0 {
		t.Errorf("idle sync returned %d rows, want 0", n)
	}
}

// TestSyncBeadLifecycle_LegacyBareTimestampCursor verifies that a cursor value
// written by the pre-fix code (bare RFC3339 timestamp, no '|' delimiter) is
// honored without a hard error and without dropping rows. Parsed as (T, ""),
// the tuple comparison re-scans all rows at T — idempotent because the
// upsert rewrites them to the same shape.
func TestSyncBeadLifecycle_LegacyBareTimestampCursor(t *testing.T) {
	olapDB, err := Open("")
	if err != nil {
		t.Fatalf("Open olap: %v", err)
	}
	defer olapDB.Close()

	mockDolt := openMockDoltLifecycle(t)

	baseTS := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	beadIDs := []string{"spi-aaaaa1", "spi-bbbbb1", "spi-ccccc1"}
	for _, id := range beadIDs {
		if _, err := mockDolt.Exec(`INSERT INTO bead_lifecycle (
			bead_id, bead_type, filed_at, updated_at
		) VALUES (?, 'task', ?, ?)`, id, baseTS, baseTS); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}

	// Seed the cursor with a bare timestamp — the exact shape the pre-fix
	// code wrote. No '|' delimiter, no bead_id component.
	ctx := context.Background()
	if _, err := olapDB.db.ExecContext(ctx,
		`INSERT INTO etl_cursor (table_name, last_id, last_synced)
		 VALUES ('bead_lifecycle', ?, now())`,
		baseTS.Format(time.RFC3339),
	); err != nil {
		t.Fatalf("seed legacy cursor: %v", err)
	}

	etl := NewETL(olapDB)
	total := 0
	for i := 0; i < 5; i++ {
		n, err := etl.SyncBeadLifecycle(ctx, mockDolt)
		if err != nil {
			t.Fatalf("SyncBeadLifecycle iter %d: %v", i, err)
		}
		total += n
		if n == 0 {
			break
		}
	}
	if total < len(beadIDs) {
		t.Errorf("rows drained after legacy cursor = %d, want at least %d", total, len(beadIDs))
	}

	var gotCount int
	if err := olapDB.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM bead_lifecycle_olap",
	).Scan(&gotCount); err != nil {
		t.Fatalf("count: %v", err)
	}
	if gotCount != len(beadIDs) {
		t.Errorf("bead_lifecycle_olap count = %d, want %d", gotCount, len(beadIDs))
	}

	// After one drain, the stored cursor is upgraded to the composite form.
	var cursorVal string
	if err := olapDB.db.QueryRowContext(ctx,
		"SELECT last_id FROM etl_cursor WHERE table_name = 'bead_lifecycle'",
	).Scan(&cursorVal); err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	want := lifecycleCursor{UpdatedAt: baseTS, BeadID: beadIDs[len(beadIDs)-1]}.String()
	if cursorVal != want {
		t.Errorf("cursor after legacy upgrade = %q, want %q", cursorVal, want)
	}
}

// TestLifecycleCursorRoundTrip covers the serialization helpers in isolation —
// composite, legacy bare-timestamp, and empty inputs.
func TestLifecycleCursorRoundTrip(t *testing.T) {
	ts := time.Date(2026, 4, 22, 18, 30, 45, 0, time.UTC)

	cases := []struct {
		name      string
		input     string
		wantTS    time.Time
		wantBead  string
		wantError bool
	}{
		{
			name:     "composite",
			input:    "2026-04-22T18:30:45Z|spi-abcdef",
			wantTS:   ts,
			wantBead: "spi-abcdef",
		},
		{
			name:     "legacy bare timestamp",
			input:    "2026-04-22T18:30:45Z",
			wantTS:   ts,
			wantBead: "",
		},
		{
			name:     "empty cursor",
			input:    "",
			wantTS:   time.Time{},
			wantBead: "",
		},
		{
			name:      "malformed timestamp",
			input:     "not-a-timestamp|spi-abc",
			wantError: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseLifecycleCursor(tc.input)
			if tc.wantError {
				if err == nil {
					t.Fatalf("want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLifecycleCursor: %v", err)
			}
			if !got.UpdatedAt.Equal(tc.wantTS) {
				t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, tc.wantTS)
			}
			if got.BeadID != tc.wantBead {
				t.Errorf("BeadID = %q, want %q", got.BeadID, tc.wantBead)
			}
		})
	}

	full := lifecycleCursor{UpdatedAt: ts, BeadID: "spi-xyz"}
	back, err := parseLifecycleCursor(full.String())
	if err != nil {
		t.Fatalf("round trip parse: %v", err)
	}
	if !back.UpdatedAt.Equal(full.UpdatedAt) || back.BeadID != full.BeadID {
		t.Errorf("round trip drift: got %+v, want %+v", back, full)
	}
}
