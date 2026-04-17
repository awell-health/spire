package olap

import "fmt"

// Import-cycle note.
//
// The ClickHouse backend lives in pkg/olap/clickhouse and must import
// pkg/olap to reference the shared types (olap.SpanRecord,
// olap.TraceFilter, …) in its method signatures — otherwise those
// methods wouldn't satisfy the olap.TraceReader interface. That forces
// the dependency edge pkg/olap/clickhouse → pkg/olap.
//
// A direct import pkg/olap → pkg/olap/clickhouse from this file would
// therefore create a cycle and fail to build. To break it we use
// init-time registration: the subpackage calls RegisterClickHouseOpener
// from its own init(), and openClickHouse dispatches through the
// registered function pointer.
//
// Any caller that wants to select the ClickHouse backend at runtime
// must blank-import pkg/olap/clickhouse to trigger the init. The
// contract_test.go inside the subpackage triggers its own init
// automatically; the consumer refactor (spi-4pa1g) adds the blank
// import to cmd/spire and pkg/steward as needed.

// openClickHouseImpl is set by pkg/olap/clickhouse.init() via
// RegisterClickHouseOpener. Nil means the subpackage hasn't been
// blank-imported yet.
var openClickHouseImpl func(cfg Config) (ReadWrite, error)

// RegisterClickHouseOpener wires the ClickHouse subpackage into the
// factory without creating a package import cycle. Called from
// pkg/olap/clickhouse.init().
func RegisterClickHouseOpener(fn func(cfg Config) (ReadWrite, error)) {
	openClickHouseImpl = fn
}

// openClickHouse dispatches to the registered ClickHouse implementation
// if one has been wired in; otherwise it returns a not-registered error
// so the caller can tell the backend apart from a genuinely missing
// driver.
func openClickHouse(cfg Config) (ReadWrite, error) {
	if openClickHouseImpl == nil {
		return nil, fmt.Errorf("olap: clickhouse backend not registered; blank-import github.com/awell-health/spire/pkg/olap/clickhouse to enable it")
	}
	return openClickHouseImpl(cfg)
}
