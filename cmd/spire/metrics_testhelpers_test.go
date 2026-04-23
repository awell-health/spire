package main

import (
	"database/sql"
	"testing"
)

// duckdbAvailable reports whether the duckdb SQL driver is registered in
// this binary. DuckDB is CGO-only (github.com/marcboeker/go-duckdb pulls
// in a C compile), so `CGO_ENABLED=0 go test ./cmd/spire/` links without
// it and every test that tries to open a DuckDB file fails with
// "unknown driver \"duckdb\"". Tests that need a live DuckDB call
// skipIfNoDuckDB so CGO-off builds surface as skips instead of failures.
//
// Rationale lives in pkg/olap/factory.go's design note: DuckDB is the
// local-only backend; ClickHouse is the cluster backend. Test coverage
// of the DuckDB path is intentionally coupled to the cgo build tag.
func duckdbAvailable() bool {
	for _, d := range sql.Drivers() {
		if d == "duckdb" {
			return true
		}
	}
	return false
}

func skipIfNoDuckDB(t *testing.T) {
	t.Helper()
	if !duckdbAvailable() {
		t.Skip("duckdb driver not registered (CGO-off build); DuckDB-dependent test skipped — see pkg/olap/factory.go design note")
	}
}
