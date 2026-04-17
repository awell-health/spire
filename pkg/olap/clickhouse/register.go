package clickhouse

import "github.com/awell-health/spire/pkg/olap"

// init registers this subpackage with the olap factory so callers that
// blank-import pkg/olap/clickhouse get automatic factory dispatch. The
// factory package cannot import this subpackage directly (cycle:
// pkg/olap ↔ pkg/olap/clickhouse), so registration is the only clean
// way to wire the two together.
func init() {
	olap.RegisterClickHouseOpener(openForFactory)
}

// openForFactory adapts the concrete Open(cfg) signature to the
// olap.ReadWrite return type required by the factory.
func openForFactory(cfg olap.Config) (olap.ReadWrite, error) {
	return Open(cfg)
}
