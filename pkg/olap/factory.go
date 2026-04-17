package olap

import (
	"fmt"
	"os"
	"strings"
)

// Factory return type — design note.
//
// Two shapes were considered:
//
//  1. Return Store from every factory call, and have ClickHouse embed a
//     nil MetricsReader that panics or errors at call time.
//  2. Return a narrower ReadWrite (Writer + TraceReader) from the main
//     factory, and add a separate OpenStore helper for the DuckDB-only
//     metrics path.
//
// We went with (2). Rationale: the consumer refactor (epic spi-7pxyq
// wave 4) needs to keep `CGO_ENABLED=0 go build ./...` green. If the
// factory returned Store, cmd/spire/metrics.go could call MetricsReader
// methods on a ClickHouse backend and the linker would happily accept
// it — the compile-time signal that "metrics needs DuckDB" would be
// lost. Narrowing the factory to ReadWrite means cmd/spire/metrics.go
// must call OpenStore (which only compiles usefully with cgo, via the
// duckdb subpackage) — turning a runtime-panic bug into a build-time
// contract violation. That's the whole point of pluggable backends.
//
// Naming — note on OpenBackend vs Open.
//
// The legacy path-based entry point Open(path string) (*DB, error) is
// kept unchanged in db.go for backward compatibility with existing
// callers (cmd/spire/trace.go, cmd/spire/metrics.go, pkg/steward/
// daemon.go — all currently off-limits for this task). The new
// backend-dispatch factory therefore takes a different name,
// OpenBackend, to avoid a Go name collision. Once those callers are
// migrated (spi-4pa1g consumer refactor) and the legacy Open is
// removed from db.go (spi-czum9 DuckDB extraction), OpenBackend can
// be renamed back to Open with minimal churn.

const (
	// BackendDuckDB is the CGO-only local backend.
	BackendDuckDB = "duckdb"
	// BackendClickHouse is the pure-Go cluster backend.
	BackendClickHouse = "clickhouse"

	// envBackend selects the backend when Config.Backend is empty.
	envBackend = "SPIRE_OLAP_BACKEND"
)

// Config parameterises the backend chosen by Open / OpenStore.
type Config struct {
	// Backend selects which driver to use: BackendDuckDB or
	// BackendClickHouse. If empty, SPIRE_OLAP_BACKEND is consulted;
	// if that is also empty, BackendDuckDB is assumed.
	Backend string

	// Path is the local filesystem path for DuckDB. Ignored by the
	// ClickHouse backend.
	Path string

	// DSN is the ClickHouse connection string, e.g.
	// "clickhouse://host:9000/spire". Ignored by the DuckDB backend.
	DSN string
}

// resolveBackend returns the effective backend name, falling back first
// to SPIRE_OLAP_BACKEND and finally to BackendDuckDB.
func (c Config) resolveBackend() string {
	b := strings.ToLower(strings.TrimSpace(c.Backend))
	if b == "" {
		b = strings.ToLower(strings.TrimSpace(os.Getenv(envBackend)))
	}
	if b == "" {
		b = BackendDuckDB
	}
	return b
}

// OpenBackend returns a backend satisfying the narrower ReadWrite
// interface (Writer + TraceReader). Use this from callers that write
// trace data and/or read trace-scoped queries; it compiles in both cgo
// and nocgo builds (the unused branch returns a stub error).
//
// Named OpenBackend (not Open) because the legacy Open(path) entry
// point in db.go is preserved for backward compatibility. See the
// design note at the top of this file.
func OpenBackend(cfg Config) (ReadWrite, error) {
	switch cfg.resolveBackend() {
	case BackendDuckDB:
		return openDuckDB(cfg)
	case BackendClickHouse:
		return openClickHouse(cfg)
	default:
		return nil, fmt.Errorf("olap: unknown backend %q (valid: %s, %s)",
			cfg.Backend, BackendDuckDB, BackendClickHouse)
	}
}

// OpenStore returns a backend satisfying the full Store interface
// (Writer + TraceReader + MetricsReader). Only DuckDB implements
// MetricsReader today, so this call returns an error for any other
// backend — and it will only compile usefully in a cgo build.
func OpenStore(cfg Config) (Store, error) {
	backend := cfg.resolveBackend()
	if backend != BackendDuckDB {
		return nil, fmt.Errorf("olap: backend %q does not implement MetricsReader; "+
			"metrics-only queries require the duckdb backend (cgo build)", backend)
	}
	return openDuckDBStore(cfg)
}
