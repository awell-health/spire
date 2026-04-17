//go:build cgo

package olap

import "fmt"

// Import-cycle note.
//
// Same story as factory_clickhouse.go: pkg/olap/duckdb imports pkg/olap
// for the shared types (olap.SpanRecord, olap.Config, …) so its method
// signatures satisfy the olap.Store interface. A direct import in the
// other direction would create a cycle. The fix is the same — init-time
// registration. The duckdb subpackage's register.go calls
// RegisterDuckDBOpener / RegisterDuckDBStoreOpener from its init() and
// the factory dispatches through the registered function pointers.
//
// Callers that want the duckdb backend at runtime must blank-import
// pkg/olap/duckdb to trigger the init. cmd/spire does this in a
// cgo-tagged file so cluster (CGO_ENABLED=0) builds skip the import
// entirely.

// openDuckDBImpl / openDuckDBStoreImpl are populated at init by the
// duckdb subpackage when blank-imported. Nil means the subpackage has
// not been wired in.
var (
	openDuckDBImpl      func(cfg Config) (ReadWrite, error)
	openDuckDBStoreImpl func(cfg Config) (Store, error)
)

// RegisterDuckDBOpener wires the duckdb subpackage's ReadWrite opener
// into the factory. Called from pkg/olap/duckdb.init().
func RegisterDuckDBOpener(fn func(cfg Config) (ReadWrite, error)) {
	openDuckDBImpl = fn
}

// RegisterDuckDBStoreOpener wires the duckdb subpackage's full-Store
// opener into the factory. Called from pkg/olap/duckdb.init().
func RegisterDuckDBStoreOpener(fn func(cfg Config) (Store, error)) {
	openDuckDBStoreImpl = fn
}

// openDuckDB dispatches to the registered duckdb implementation.
func openDuckDB(cfg Config) (ReadWrite, error) {
	if openDuckDBImpl == nil {
		return nil, fmt.Errorf("olap: duckdb backend not registered; blank-import github.com/awell-health/spire/pkg/olap/duckdb to enable it")
	}
	return openDuckDBImpl(cfg)
}

// openDuckDBStore dispatches to the registered duckdb store opener.
func openDuckDBStore(cfg Config) (Store, error) {
	if openDuckDBStoreImpl == nil {
		return nil, fmt.Errorf("olap: duckdb backend not registered; blank-import github.com/awell-health/spire/pkg/olap/duckdb to enable it")
	}
	return openDuckDBStoreImpl(cfg)
}
