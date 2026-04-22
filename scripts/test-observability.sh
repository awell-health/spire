#!/usr/bin/env bash
# Layered regression suite for the Spire observability pipeline.
#
# Scope:
#   pkg/olap       — DB-backed contract tests (tool_spans, tool_events,
#                    api_events, bead_lifecycle_olap, ETL cursor
#                    idempotence, by-type rollup).
#   pkg/otel       — emit → parse → olap write → reader round-trip using
#                    the canonical runtime-contract resource attributes
#                    (spi-xplwy / spi-zm3b1) and the legacy fallback
#                    (spi-ecnfe).
#   cmd/spire      — CLI rendering against fake olap.TraceReader and
#                    beadMetricsReader; asserts JSON schema stability
#                    without opening analytics.db.
#
# An observability regression should fail in exactly one layer, so
# "which test fails" tells you whether the schema, the receiver, or
# the CLI reader drifted. Adding a new layer or swapping a backend
# should update this script, not the callers.
#
# Used by Makefile target `test-observability`. Intended to be runnable
# on a developer laptop in under a minute and by CI as a gate.

set -euo pipefail

# Run from repo root regardless of invocation directory.
cd "$(dirname "$0")/.."

echo "==> observability: pkg/olap contract + ETL tests"
go test ./pkg/olap/... -count=1 -timeout=120s

echo "==> observability: pkg/otel parse + round-trip tests"
go test ./pkg/otel/... -count=1 -timeout=120s

echo "==> observability: cmd/spire reader-seam + rendering tests"
# The full cmd/spire suite is kept in scope so schema changes that
# break unrelated CLI rendering are caught alongside the reader seams.
# If runtime becomes a concern, narrow with -run "Observability|Metrics|Trace|Lifecycle".
go test ./cmd/spire/... -count=1 -timeout=120s

echo "==> observability: all layers passed"
