// Package obsvocab is the single, export-only place that fixes the
// canonical observability vocabulary: the OTLP resource attribute keys
// receivers must accept, the legacy fallbacks that predate them, the
// OLAP tables that ingest and aggregate the signals, and the lifecycle
// columns the bead_lifecycle sidecar exposes.
//
// It exists so the three independent layers that have a stake in the
// vocabulary — pkg/otel (ingestion), pkg/olap (storage), and cmd/spire
// (CLI readers) — assert against the same constants instead of holding
// hand-copied duplicates. Hand-copied duplicates let a rename pass on
// one side while the other side keeps looking up the old name; the
// spi-ecnfe bug happened when the emitter switched to canonical keys
// while the receiver still matched legacy ones. Regression tests that
// don't pin this vocabulary in a shared symbol can silently rot in the
// same way.
//
// The package has no dependencies and exports only constants and flat
// slices so any test layer — or future production code — can import it
// without pulling in storage or ingestion internals.
package obsvocab

// Canonical OTLP resource attribute keys, post-spi-xplwy / spi-zm3b1.
// These are the keys the worker runtime injects via
// OTEL_RESOURCE_ATTRIBUTES and the only keys receivers should persist
// into tool_spans.bead_id / tool_events.step / etc. without a deprecation
// fallback.
const (
	AttrBeadID          = "bead_id"
	AttrAttemptID       = "attempt_id"
	AttrRunID           = "run_id"
	AttrTower           = "tower"
	AttrPrefix          = "prefix"
	AttrRole            = "role"
	AttrFormulaStep     = "formula_step"
	AttrBackend         = "backend"
	AttrWorkspaceKind   = "workspace_kind"
	AttrWorkspaceName   = "workspace_name"
	AttrWorkspaceOrigin = "workspace_origin"
	AttrHandoffMode     = "handoff_mode"
	AttrAgentName       = "agent.name"
	AttrSessionID       = "session.id"
	AttrServiceInstance = "service.instance.id"
)

// Legacy OTLP resource attribute keys preserved as a fallback during
// the migration window. Tests that emit with these keys lock in the
// receiver's backward-compat guarantee; when the fallback is removed
// the tests relying on these constants are the deliberate delete.
const (
	LegacyAttrBeadID = "bead.id"
	LegacyAttrStep   = "step"
)

// CanonicalAttrKeys enumerates the canonical keys in a deterministic
// order suitable for iteration and snapshot comparison. A rename in
// pkg/otel that forgets to update this slice will make the shared
// fixture disagree with the runtime — the test layer catches it
// before the rename reaches production.
var CanonicalAttrKeys = []string{
	AttrBeadID,
	AttrAttemptID,
	AttrRunID,
	AttrTower,
	AttrPrefix,
	AttrRole,
	AttrFormulaStep,
	AttrBackend,
	AttrWorkspaceKind,
	AttrWorkspaceName,
	AttrWorkspaceOrigin,
	AttrHandoffMode,
	AttrAgentName,
	AttrSessionID,
}

// LegacyAttrKeys enumerates the legacy keys the receiver still honours.
// Symmetric with CanonicalAttrKeys so test fixtures that iterate both
// slices can produce the full matrix.
var LegacyAttrKeys = []string{
	LegacyAttrBeadID,
	LegacyAttrStep,
}

// OLAP table names the observability pipeline writes into. Named here
// so the test layer references the same strings the storage layer
// creates DDL for. Renaming a table without updating this list makes
// the contract tests fail in one place instead of rotting silently.
const (
	TableToolSpans        = "tool_spans"
	TableToolEvents       = "tool_events"
	TableAPIEvents        = "api_events"
	TableAgentRunsOLAP    = "agent_runs_olap"
	TableBeadLifecycleOLAP = "bead_lifecycle_olap"
	TableETLCursor        = "etl_cursor"
)

// OTELTables enumerates the pure OTEL-ingest tables (i.e. populated
// directly by the receiver rather than by ETL from Dolt). These are
// the tables the layered regression suite must assert round-trip on
// for the tool_spans / tool_events / api_events triad.
var OTELTables = []string{
	TableToolSpans,
	TableToolEvents,
	TableAPIEvents,
}

// Canonical tool_spans columns. Captured here so schema drift is
// visible in one place — the contract tests assert the real DDL
// contains every column listed below.
var ToolSpansColumns = []string{
	"trace_id",
	"span_id",
	"parent_span_id",
	"session_id",
	"bead_id",
	"agent_name",
	"step",
	"span_name",
	"kind",
	"duration_ms",
	"success",
	"start_time",
	"end_time",
	"tower",
	"attributes",
}

// Canonical tool_events columns.
var ToolEventsColumns = []string{
	"session_id",
	"bead_id",
	"agent_name",
	"step",
	"tool_name",
	"duration_ms",
	"success",
	"timestamp",
	"tower",
	"provider",
	"event_kind",
}

// Canonical api_events columns.
var APIEventsColumns = []string{
	"session_id",
	"bead_id",
	"agent_name",
	"step",
	"provider",
	"model",
	"duration_ms",
	"input_tokens",
	"output_tokens",
	"cache_read_tokens",
	"cache_write_tokens",
	"cost_usd",
	"timestamp",
	"tower",
}

// Canonical bead_lifecycle_olap columns. These are the fields
// spi-hmdwm added for lifecycle analytics and the bead-detail block
// in `spire metrics --bead`. The contract tests pin every column so
// a rename in the migration surfaces during CI instead of at an
// operator's desk.
var BeadLifecycleColumns = []string{
	"bead_id",
	"bead_type",
	"filed_at",
	"ready_at",
	"started_at",
	"closed_at",
	"updated_at",
	"review_count",
	"fix_count",
	"arbiter_count",
	"synced_at",
}

// ETL cursor table name for the bead_lifecycle incremental sync
// (spi-ouuta composite cursor). Exposed separately from TableETLCursor
// because test fixtures need to reference the cursor key string
// directly when asserting the composite-cursor contract.
const LifecycleETLCursorKey = "bead_lifecycle"
