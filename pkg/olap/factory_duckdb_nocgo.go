//go:build !cgo

package olap

import "fmt"

// openDuckDB in a non-cgo build is a hard error: DuckDB requires CGO.
// This allows `CGO_ENABLED=0 go build ./...` to succeed while any
// attempt to actually select the duckdb backend at runtime fails fast.
func openDuckDB(cfg Config) (ReadWrite, error) {
	_ = cfg
	return nil, fmt.Errorf("olap: duckdb backend requires CGO build; rebuild with CGO_ENABLED=1 or set %s=%s",
		envBackend, BackendClickHouse)
}

// openDuckDBStore is the non-cgo variant of the MetricsReader path.
// Same story — errors out so the build stays green without cgo.
func openDuckDBStore(cfg Config) (Store, error) {
	_ = cfg
	return nil, fmt.Errorf("olap: duckdb backend requires CGO build; rebuild with CGO_ENABLED=1")
}
