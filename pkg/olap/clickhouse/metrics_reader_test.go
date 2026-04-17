package clickhouse

import (
	"testing"

	"github.com/awell-health/spire/pkg/olap"
)

// TestMetricsReaderNotImplemented is the runtime half of the split
// contract: Store must NOT implement olap.MetricsReader. Go cannot
// express "must not implement X" at compile time as a negative
// assertion, so we check it dynamically via a type assertion — the
// assertion MUST fail. The positive assertions (Writer, TraceReader)
// live as `var _ olap.Writer = (*Store)(nil)` declarations in
// clickhouse.go and guarantee the opposite direction.
//
// The whole point of keeping MetricsReader off ClickHouse is to give
// `spire metrics` a compile-time "this backend is DuckDB only" signal
// once the consumer refactor (spi-4pa1g) lands.
func TestMetricsReaderNotImplemented(t *testing.T) {
	var iface any = (*Store)(nil)
	if _, ok := iface.(olap.MetricsReader); ok {
		t.Fatalf("Store unexpectedly satisfies olap.MetricsReader; the split contract is broken")
	}
}
