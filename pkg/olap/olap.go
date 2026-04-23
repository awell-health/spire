// Package olap provides the pluggable OLAP layer for Spire analytics.
//
// Two backends live (or will live) in subpackages under this one:
//
//   - pkg/olap/duckdb    — CGO-only local backend that satisfies Store
//     (Writer + TraceReader + MetricsReader). Default for single-node
//     installs where the full metrics surface is required.
//
//   - pkg/olap/clickhouse — pure-Go cluster backend that satisfies
//     Writer + TraceReader but deliberately NOT MetricsReader. Default
//     for cluster/k8s installs where DuckDB's CGO dependency is
//     unavailable. The split is intentional: MetricsReader contains
//     aggregation queries using DuckDB-only SQL dialect, and the
//     ClickHouse implementation of those would be a separate effort.
//
// This file declares the interfaces and the shared types that flow
// across the backend boundary. Backend subpackages import these types
// without importing each other, avoiding the cgo/no-cgo compilation
// tangle that gave this epic its raison d'être.
package olap

import (
	"context"
	"database/sql"
	"time"
)

// Writer is the write path shared by every OLAP backend. Both backends
// must serialize concurrent writes however their storage engine requires
// (DuckDB via a single-goroutine channel; ClickHouse directly, since
// MergeTree supports concurrent writes).
type Writer interface {
	// Submit runs fn inside a transaction. Implementations may retry
	// on transient errors (e.g. DuckDB's file lock).
	Submit(fn func(*sql.Tx) error) error
	// Close releases the underlying connection pool.
	Close() error
}

// TraceReader is the read path for tracing/observability data that both
// backends must support. All methods return fully-hydrated value types
// (defined below) so the CLI / board can render without touching raw SQL.
//
// The two context-aware methods (QuerySpans, QueryTraces) are the new
// canonical entry points designed for multi-tenant cluster queries with
// explicit cancellation and filtering. The remaining bead-scoped methods
// mirror the calls cmd/spire/trace.go already makes against the DuckDB
// backend and are preserved here so the ClickHouse backend can implement
// them with SQL-dialect translation (argMax, LIMIT n BY, etc).
type TraceReader interface {
	// QuerySpans returns every span for a single trace_id, ordered by
	// start_time. Used by the waterfall view when the caller already
	// has a trace_id (e.g. follow-up from QueryTraces).
	QuerySpans(ctx context.Context, traceID string) ([]SpanRecord, error)

	// QueryTraces returns a list of trace summaries matching the filter.
	// Used for trace-browser listings.
	QueryTraces(ctx context.Context, filter TraceFilter) ([]TraceSummary, error)

	// QueryToolSpansByBead returns every span recorded for a bead,
	// ordered by start_time. Powers the per-bead waterfall in `spire trace`.
	QueryToolSpansByBead(beadID string) ([]SpanRecord, error)

	// QueryToolEventsByBead returns aggregated tool-call counts for a bead.
	QueryToolEventsByBead(beadID string) ([]ToolEventStats, error)

	// QueryToolEventsByStep returns per-step tool breakdowns for the bead.
	QueryToolEventsByStep(beadID string) ([]StepToolBreakdown, error)

	// QueryAPIEventsByBead returns LLM API call aggregates for a bead.
	QueryAPIEventsByBead(beadID string) ([]APIEventStats, error)
}

// MetricsReader is the read path for aggregate metrics / DORA / formula
// comparison queries. Implemented only by the DuckDB backend today —
// ClickHouse intentionally does NOT satisfy this interface (see package
// doc). Factory.Open returns a narrower ReadWrite; callers that require
// metrics must call OpenStore and run in a CGO build.
type MetricsReader interface {
	QuerySummary(since time.Time) (*SummaryStats, error)
	QueryModelBreakdown(since time.Time) ([]ModelStats, error)
	QueryPhaseBreakdown(since time.Time) ([]PhaseStats, error)
	QueryDORA(since time.Time) (*DORAMetrics, error)
	QueryTrends(since time.Time) ([]WeeklyTrend, error)
	QueryFailures(since time.Time) ([]FailureStats, error)
	QueryToolUsage(since time.Time) ([]ToolUsageStats, error)
	QueryToolEvents(since time.Time) ([]ToolEventStats, error)
	QueryBugCausality(limit int) ([]BugCausality, error)
	QueryFormulaPerformance(since time.Time) ([]FormulaStats, error)
	QueryCostTrend(days int) ([]CostTrendPoint, error)
	QueryLifecycleForBead(beadID string) (*BeadLifecycleIntervals, error)
	QueryLifecycleByType(since time.Time) ([]LifecycleByType, error)
	QueryReviewFixCounts(beadID string) (*ReviewFixCounts, error)
	QueryChildLifecycle(parentID string) ([]BeadLifecycleIntervals, error)
	QueryRateLimitEvents(window time.Duration) ([]RateLimitBucket, error)
}

// ReadWrite is the minimum surface every OLAP backend must satisfy:
// write path plus trace reads. Used as the factory.Open return type so
// CGO-less ClickHouse builds still compile.
type ReadWrite interface {
	Writer
	TraceReader
}

// Store is the full OLAP surface: writes, trace reads, and metrics reads.
// DuckDB is the only backend that satisfies this today. Callers that
// need metrics must use factory.OpenStore (CGO-only).
type Store interface {
	Writer
	TraceReader
	MetricsReader
}

// ---------------------------------------------------------------------------
// Shared types that cross the backend boundary. Defined here (rather than
// in backend subpackages) so both backends import a single canonical
// package for type identity — critical for interface satisfaction.
// ---------------------------------------------------------------------------

// SpanRecord is a single row from the tool_spans table. Both backends
// return this shape from QueryToolSpansByBead / QuerySpans.
type SpanRecord struct {
	TraceID      string    `json:"trace_id"`
	SpanID       string    `json:"span_id"`
	ParentSpanID string    `json:"parent_span_id"`
	SpanName     string    `json:"span_name"`
	Kind         string    `json:"kind"`
	DurationMs   int       `json:"duration_ms"`
	Success      bool      `json:"success"`
	StartTime    time.Time `json:"start_time"`
	EndTime      time.Time `json:"end_time"`
	Attributes   string    `json:"attributes,omitempty"`
}

// TraceFilter narrows a QueryTraces call. Zero value matches everything
// subject to the default 200-row cap. BeadID scopes to a single bead;
// Since/Until form a half-open time window; Limit caps the result size
// (implementations should enforce a sane default, e.g. 200, when Limit==0).
type TraceFilter struct {
	BeadID string
	Since  time.Time
	Until  time.Time
	Limit  int
}

// TraceSummary is the list-item shape for QueryTraces. One row per
// trace_id; aggregate fields are derived from the underlying spans.
type TraceSummary struct {
	TraceID    string    `json:"trace_id"`
	BeadID     string    `json:"bead_id"`
	RootSpan   string    `json:"root_span"`
	SpanCount  int       `json:"span_count"`
	DurationMs int       `json:"duration_ms"`
	StartTime  time.Time `json:"start_time"`
	EndTime    time.Time `json:"end_time"`
	Success    bool      `json:"success"`
}

// DORAMetrics holds the four DORA performance metrics.
type DORAMetrics struct {
	DeployFrequency   float64 // deploys per week
	LeadTimeSeconds   float64 // avg seconds from first commit to deploy
	ChangeFailureRate float64 // ratio 0.0-1.0
	MTTRSeconds       float64 // mean time to recovery
}

// SummaryStats holds overall run statistics.
type SummaryStats struct {
	TotalRuns    int
	Successes    int
	Failures     int
	SuccessRate  float64
	AvgCostUSD   float64
	AvgDurationS float64
	TotalCostUSD float64
}

// ModelStats holds per-model aggregated statistics.
type ModelStats struct {
	Model        string
	RunCount     int
	SuccessRate  float64
	AvgCostUSD   float64
	AvgDurationS float64
	TotalTokens  int64
}

// PhaseStats holds per-phase aggregated statistics.
type PhaseStats struct {
	Phase        string
	RunCount     int
	SuccessRate  float64
	AvgCostUSD   float64
	AvgDurationS float64
}

// WeeklyTrend holds weekly aggregated metrics for trend display.
type WeeklyTrend struct {
	WeekStart    time.Time
	RunCount     int
	SuccessRate  float64
	TotalCostUSD float64
	MergeCount   int
}

// FailureStats holds failure breakdown by class.
type FailureStats struct {
	FailureClass string
	Count        int
	Percentage   float64
}

// ToolUsageStats holds tool call aggregates per formula/phase.
type ToolUsageStats struct {
	FormulaName string
	Phase       string
	TotalRead   int
	TotalEdit   int
	TotalTools  int
	ReadRatio   float64 // read / (read+edit)
}

// BugCausality holds failure hotspot data for repeated failures on a bead.
type BugCausality struct {
	BeadID       string
	FailureClass string
	AttemptCount int
	LastFailure  time.Time
}

// CostTrendPoint holds daily cost, token, and run count data.
type CostTrendPoint struct {
	Date             time.Time
	TotalCost        float64
	RunCount         int
	PromptTokens     int64
	CompletionTokens int64
}

// FormulaStats holds aggregated performance data for a formula name+version pair.
type FormulaStats struct {
	FormulaName     string
	FormulaVersion  string
	TotalRuns       int
	Successes       int
	SuccessRate     float64 // 0–100
	AvgCostUSD      float64
	AvgReviewRounds float64
	RunsLast30d     int
}

// ToolEventStats holds aggregated tool event statistics from the tool_events table.
type ToolEventStats struct {
	ToolName      string  `json:"tool_name"`
	Count         int     `json:"count"`
	AvgDurationMs float64 `json:"avg_duration_ms"`
	FailureCount  int     `json:"failure_count"`
	Step          string  `json:"step,omitempty"`
}

// StepToolBreakdown holds per-step tool usage for the trace view.
type StepToolBreakdown struct {
	Step  string           `json:"step"`
	Tools []ToolEventStats `json:"tools"`
}

// APIEventStats holds aggregated API event statistics.
type APIEventStats struct {
	Model             string  `json:"model"`
	Count             int     `json:"count"`
	AvgDurationMs     float64 `json:"avg_duration_ms"`
	TotalCostUSD      float64 `json:"total_cost_usd"`
	TotalInputTokens  int64   `json:"total_input_tokens"`
	TotalOutputTokens int64   `json:"total_output_tokens"`
}

// RateLimitBucket is one day's worth of rate-limit events from api_events
// (rows with event_type='rate_limit'). Callers aggregate or render buckets
// from QueryRateLimitEvents over a caller-supplied window.
type RateLimitBucket struct {
	Day   time.Time `json:"day"`
	Count int       `json:"count"`
}

// BeadLifecycle holds the canonical lifecycle timestamps for a single bead.
// Any of FiledAt / ReadyAt / StartedAt / ClosedAt may be zero (NULL in the
// backing store) — pre-feature beads never received ready/started stamps,
// and in-flight beads have no close time yet. Renderers and consumers must
// treat zero values as "unknown" rather than the Unix epoch.
type BeadLifecycle struct {
	BeadID    string    `json:"bead_id"`
	BeadType  string    `json:"bead_type,omitempty"`
	FiledAt   time.Time `json:"filed_at,omitempty"`
	ReadyAt   time.Time `json:"ready_at,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	ClosedAt  time.Time `json:"closed_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// BeadLifecycleIntervals holds derived interval durations for a single bead.
// Each field is a *float64 pointer so we can distinguish "zero" from "missing"
// (a bead that hasn't closed yet legitimately has no filed-to-closed
// duration; rendering that as 0s would misreport execution time).
type BeadLifecycleIntervals struct {
	BeadLifecycle
	FiledToClosedSeconds   *float64 `json:"filed_to_closed_seconds,omitempty"`
	ReadyToClosedSeconds   *float64 `json:"ready_to_closed_seconds,omitempty"`
	StartedToClosedSeconds *float64 `json:"started_to_closed_seconds,omitempty"`
	QueueSeconds           *float64 `json:"queue_seconds,omitempty"` // started_at - ready_at
}

// LifecycleByType holds aggregated lifecycle timings grouped by bead_type.
// Only closed beads contribute — open beads have no terminal timestamp.
// Each percentile field is *float64 to distinguish "NULL in SQL" (entire
// population had no value for the underlying CASE expression — e.g.
// pre-feature beads with no ready_at) from a legitimate zero duration.
// Renderers should print "—" when the pointer is nil.
type LifecycleByType struct {
	BeadType           string   `json:"bead_type"`
	Count              int      `json:"count"`
	FiledToClosedP50   *float64 `json:"filed_to_closed_p50_seconds,omitempty"`
	FiledToClosedP95   *float64 `json:"filed_to_closed_p95_seconds,omitempty"`
	ReadyToClosedP50   *float64 `json:"ready_to_closed_p50_seconds,omitempty"`
	ReadyToClosedP95   *float64 `json:"ready_to_closed_p95_seconds,omitempty"`
	StartedToClosedP50 *float64 `json:"started_to_closed_p50_seconds,omitempty"`
	StartedToClosedP95 *float64 `json:"started_to_closed_p95_seconds,omitempty"`
	QueueP50           *float64 `json:"queue_p50_seconds,omitempty"`
	QueueP95           *float64 `json:"queue_p95_seconds,omitempty"`
}

// ReviewFixCounts holds derived review/fix/arbiter counts for a single bead.
// Values are aggregated from agent_runs_olap (the live source of truth) rather
// than the denormalized counters on bead_lifecycle, to avoid drift.
type ReviewFixCounts struct {
	BeadID       string `json:"bead_id"`
	ReviewCount  int    `json:"review_count"`
	FixCount     int    `json:"fix_count"`
	ArbiterCount int    `json:"arbiter_count"`
	// MaxReviewRounds is the highest review_round recorded across any run
	// for this bead — a coarser proxy useful when review_step is unavailable.
	MaxReviewRounds int `json:"max_review_rounds"`
}
