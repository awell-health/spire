# pkg/olap

OLAP store for derived metrics. Two backends, one factory.

## Backends

| Backend | File prefix | Build | Deployment |
|---------|-------------|-------|------------|
| DuckDB | `duckdb_*`, `schema.go`, `views.go` | cgo-only | local single-binary |
| ClickHouse | `clickhouse_*` | pure-Go | cluster (helm/spire) |

Dolt remains the source of truth. Everything here is ETL / materialized
analytics. `pkg/olap/etl.go` watermarks on `started_at` (agent_runs) and
`(updated_at, bead_id)` (bead_lifecycle) for idempotent incremental sync.

## Factory entry points

`pkg/olap/factory.go` dispatches by `Config.Backend` (or
`SPIRE_OLAP_BACKEND` env). Two variants:

- `OpenBackend(cfg) (ReadWrite, error)` — Writer + TraceReader. Works
  in both cgo and nocgo builds. Use from writers (`pkg/otel/receiver.go`,
  steward daemon, anywhere that only emits rows or reads traces).
- `OpenStore(cfg) (Store, error)` — adds MetricsReader. Only DuckDB
  implements it; ClickHouse returns an error at open time. Use from the
  metrics CLI (`cmd/spire/metrics.go`) that needs quantile/percentile
  aggregates.

See the design note at the top of `factory.go` for why the factory is
split into two interfaces instead of exposing Store everywhere.

## Schema parity

`schema.go` (DuckDB) and `clickhouse_schema.go` (ClickHouse) must stay
column-aligned. Any new column added to a table needs to land in both
files. Current table set:

| Table | Populated by | Both backends |
|-------|--------------|---------------|
| `agent_runs_olap` | ETL from Dolt `agent_runs` | ✓ |
| `bead_lifecycle_olap` | ETL from Dolt `bead_lifecycle` (hmdwm) | ✓ |
| `api_events` | OTLP receiver (obtep6 rate-limit events too) | ✓ |
| `tool_events` | OTLP receiver | ✓ |
| `tool_spans` | OTLP receiver | ✓ |
| `etl_cursor` | ETL bookkeeping | ✓ |
| `daily_formula_stats` | Views refresh | ✓ |
| `weekly_merge_stats` | Views refresh | ✓ |
| `phase_cost_breakdown` | Views refresh | ✓ |
| `tool_usage_stats` | Views refresh | ✓ |
| `failure_hotspots` | Views refresh | ✓ |

DuckDB types (`VARCHAR`, `BIGINT`, `DOUBLE`, `TIMESTAMP`) map to
ClickHouse types (`String`, `Int64`, `Float64`, `DateTime64(3)`). Enum-
like columns (`timing_bucket`, `phase_bucket` from wyktto/b1357m live in
the Dolt `agent_runs` row and reach OLAP via the ETL; if they are ever
materialized as their own OLAP column they should use
`LowCardinality(String)` in ClickHouse).

## Engines

- Append-only event tables (`tool_events`, `tool_spans`, `api_events`)
  — `MergeTree` ordered by `(tower, bead_id, timestamp)`.
- Upsert tables (`agent_runs_olap`, `bead_lifecycle_olap`, aggregates,
  `etl_cursor`) — `ReplacingMergeTree(synced_at)` keyed on the natural
  primary key. Reads should use `FINAL` or `GROUP BY` to see the deduped
  row — background merges collapse duplicates eventually but not
  immediately.

## Views

`views.go` holds the DuckDB aggregate-refresh DDL (DELETE + INSERT …
ON CONFLICT + `epoch()` + `INTERVAL N DAY`). `clickhouse_views.go`
holds the ClickHouse equivalents rewritten for `dateDiff('second', …)`,
`today() - N`, `toStartOfWeek`, `countIf`/`avgIf` conditional
aggregates, and plain INSERT (ReplacingMergeTree handles dedup).

## Testing

DuckDB-dependent tests (`cmd/spire/metrics*_test.go`,
`cmd/spire/observability_reader_test.go`) call `skipIfNoDuckDB(t)` so
`CGO_ENABLED=0 go test` skips instead of failing on missing driver.
ClickHouse tests against a live server are out of scope for unit tests;
they run in the cluster smoke tower.
