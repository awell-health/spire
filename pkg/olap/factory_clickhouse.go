package olap

import "fmt"

// openClickHouse is the pure-Go ClickHouse factory entry point. No build
// tag — ClickHouse compiles with or without cgo. Stubbed until the
// pkg/olap/clickhouse subpackage lands (task spi-v0kn3).
func openClickHouse(cfg Config) (ReadWrite, error) {
	_ = cfg
	return nil, fmt.Errorf("olap: clickhouse backend not yet implemented")
}
