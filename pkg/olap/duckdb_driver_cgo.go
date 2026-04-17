//go:build cgo

package olap

// Registers the "duckdb" database/sql driver so sql.Open("duckdb", …) in
// db.go / etl.go works. Gated by //go:build cgo because go-duckdb requires
// CGO; CGO_ENABLED=0 builds skip this file entirely and must select a
// different backend (ClickHouse) via the factory.
import _ "github.com/marcboeker/go-duckdb"
