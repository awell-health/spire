//go:build cgo

package olap

import "fmt"

// openDuckDB is the cgo-enabled entry point. Once pkg/olap/duckdb is
// extracted (task spi-czum9), this stub is replaced with a real call
// into that subpackage.
func openDuckDB(cfg Config) (ReadWrite, error) {
	_ = cfg
	return nil, fmt.Errorf("olap: duckdb backend not yet implemented")
}

// openDuckDBStore is the cgo-enabled path for OpenStore. Stubbed the
// same way as openDuckDB until the duckdb subpackage lands.
func openDuckDBStore(cfg Config) (Store, error) {
	_ = cfg
	return nil, fmt.Errorf("olap: duckdb backend not yet implemented")
}
