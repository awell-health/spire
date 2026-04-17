//go:build cgo

package duckdb

import "github.com/awell-health/spire/pkg/olap"

// init registers this subpackage with the olap factory so callers that
// blank-import pkg/olap/duckdb get automatic factory dispatch. The
// factory package cannot import this subpackage directly (cycle:
// pkg/olap ↔ pkg/olap/duckdb), so registration is the only clean way
// to wire the two together.
func init() {
	olap.RegisterDuckDBOpener(openForFactory)
	olap.RegisterDuckDBStoreOpener(openStoreForFactory)
}

// openForFactory adapts duckdb.Open(path) to the olap.ReadWrite return
// type required by the factory.
func openForFactory(cfg olap.Config) (olap.ReadWrite, error) {
	return Open(cfg.Path)
}

// openStoreForFactory adapts duckdb.Open(path) to the olap.Store return
// type required by OpenStore.
func openStoreForFactory(cfg olap.Config) (olap.Store, error) {
	return Open(cfg.Path)
}
