//go:build cgo

package duckdb

import (
	"testing"

	"github.com/awell-health/spire/pkg/olap"
	"github.com/awell-health/spire/pkg/olap/olaptest"
)

// TestDuckDBContract runs the shared Writer + TraceReader contract
// harness against the local DuckDB backend. ClickHouse runs the same
// harness under the `integration` build tag; this file proves both
// backends pass the same contract, which is what the layered
// regression suite relies on to catch schema/behaviour drift in one
// targeted test instead of downstream CLI surprise.
func TestDuckDBContract(t *testing.T) {
	olaptest.RunContractTests(t, func(tt *testing.T) (olap.Writer, olap.TraceReader) {
		tt.Helper()
		db, err := Open("")
		if err != nil {
			tt.Fatalf("Open in-memory duckdb: %v", err)
		}
		return db, db
	})
}

// TestDuckDBMetricsContract runs the MetricsReader contract harness.
// DuckDB is the only backend that satisfies MetricsReader today; this
// is where the metrics-surface contract lives for the full tree.
func TestDuckDBMetricsContract(t *testing.T) {
	olaptest.RunMetricsContractTests(t, func(tt *testing.T) olap.MetricsReader {
		tt.Helper()
		db, err := Open("")
		if err != nil {
			tt.Fatalf("Open in-memory duckdb: %v", err)
		}
		tt.Cleanup(func() { _ = db.Close() })
		return db
	})
}

// TestDuckDBOTELIngestContract runs the tool_events / api_events
// round-trip contract — the OTEL ingest surface beyond tool_spans —
// against a fresh in-memory DuckDB per subtest. This is the single
// place the canonical OTEL-ingest tables are asserted end-to-end for
// DuckDB; the CLI and receiver tests consume typed readers instead.
func TestDuckDBOTELIngestContract(t *testing.T) {
	olaptest.RunOTELIngestContract(t, storeFactory)
}

// TestDuckDBLifecycleContract runs the spi-hmdwm lifecycle analytics
// contract — per-bead intervals, by-type P50/P95 rollup, review/fix
// aggregation, and hierarchical child drill-down — against a fresh
// in-memory DuckDB per subtest.
func TestDuckDBLifecycleContract(t *testing.T) {
	olaptest.RunLifecycleContract(t, storeFactory)
}

// storeFactory returns a fresh DuckDB Store per subtest. In-memory
// DuckDB means subtests can't pollute each other's state; the harness
// handles the Close via t.Cleanup inside its own subtest functions,
// but we also register Close on the outer *testing.T as a belt.
func storeFactory(tt *testing.T) olap.Store {
	tt.Helper()
	db, err := Open("")
	if err != nil {
		tt.Fatalf("Open in-memory duckdb: %v", err)
	}
	return db
}
