package report

import (
	"context"
	"database/sql"
	"time"
)

// Reader is the narrow data-access surface the report assembler needs.
// It is intentionally NOT olap.MetricsReader — those methods don't
// know about scope filters and produce shapes the panels post-process
// heavily. This interface lets the assembler stay testable with an
// in-memory fake while the production impl talks to DuckDB.
//
// Every method takes a request context so a dropped client cancels
// the pending queries.
type Reader interface {
	// Hero + Throughput.
	QueryThroughputWeekly(ctx context.Context, scope Scope, since, until time.Time) ([]ThroughputWeek, error)
	QueryHeroActiveAgents(ctx context.Context, scope Scope, since, until time.Time) (now, highWater int, err error)
	QueryHeroCostByWeek(ctx context.Context, scope Scope, since, until time.Time) (perWeekThis, perWeekPrev float64, err error)
	QueryHeroMTTR(ctx context.Context, scope Scope, since, until time.Time) (thisSec, prevSec float64, err error)

	// Lifecycle (single-query per scope; the panels derive 6 stages each).
	QueryLifecycleByType(ctx context.Context, scope Scope, since, until time.Time) ([]LifecycleByType, error)

	// Bug attachment (one row per week × parent_type).
	QueryBugAttachmentWeekly(ctx context.Context, scope Scope, since, until time.Time) ([]BugAttachmentWeek, error)

	// Formula performance + sparkline.
	QueryFormulas(ctx context.Context, scope Scope, since, until time.Time) ([]FormulaRow, error)

	// Cost daily + top runs.
	QueryCostDaily(ctx context.Context, scope Scope, since, until time.Time) ([]CostDay, error)

	// Phases funnel.
	QueryPhases(ctx context.Context, scope Scope, since, until time.Time) ([]PhaseRow, error)

	// Failures: classes + hotspots.
	QueryFailures(ctx context.Context, scope Scope, since, until time.Time) (FailuresBlock, error)

	// Models mix.
	QueryModels(ctx context.Context, scope Scope, since, until time.Time) ([]ModelRow, error)

	// Tools usage (from tool_events).
	QueryTools(ctx context.Context, scope Scope, since, until time.Time) ([]ToolRow, error)
}

// SQLReader is the production Reader implementation. It talks directly
// to a *sql.DB holding the DuckDB OLAP state. One instance is created
// per request by the gateway handler.
//
// No persistent connection pool is stashed here — the handler is
// expected to open/close the underlying *sql.DB so DuckDB's file lock
// does not leak between requests (see pkg/olap/db.go ReadFunc).
type SQLReader struct {
	DB *sql.DB
}

// NewSQLReader wraps a *sql.DB for use by Build(). Safe to call with a
// nil DB — every query method returns an error in that case rather
// than panicking, so the gateway handler can surface a clean 503.
func NewSQLReader(db *sql.DB) *SQLReader {
	return &SQLReader{DB: db}
}

// nullFloat returns the float value or 0 when the SQL value was NULL.
// Panels expect plain numbers (not nullable) per the TS contract.
func nullFloat(n sql.NullFloat64) float64 {
	if n.Valid {
		return n.Float64
	}
	return 0
}

// nullInt64 returns the int64 value or 0 when NULL.
func nullInt64(n sql.NullInt64) int64 {
	if n.Valid {
		return n.Int64
	}
	return 0
}

// nullString returns the string value or "" when NULL.
func nullString(n sql.NullString) string {
	if n.Valid {
		return n.String
	}
	return ""
}
