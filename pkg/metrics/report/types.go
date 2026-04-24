// Package report assembles the /api/v1/metrics payload consumed by the
// spire-desktop MetricsView. Every exported type mirrors the TypeScript
// contract at spire-desktop/src/views/metrics/types.ts — field names are
// tagged camelCase so the JSON wire shape matches byte-for-byte.
//
// This package is intentionally independent of the backing OLAP driver:
// it talks to a narrow Reader interface so tests can stub the data
// source without spinning up DuckDB. The gateway wires an *olap.DB into
// a concrete Reader at request time.
package report

// MetricsResponse is the full payload returned from GET /api/v1/metrics.
// Mirrors the TypeScript MetricsResponse interface; fields marked
// "weekly" / "byType" wrap arrays so the wire shape matches the TS.
type MetricsResponse struct {
	Scope         string             `json:"scope"`
	Window        string             `json:"window"`
	GeneratedAt   string             `json:"generatedAt"`
	Hero          HeroData           `json:"hero"`
	Throughput    ThroughputBlock    `json:"throughput"`
	Lifecycle     LifecycleBlock     `json:"lifecycle"`
	BugAttachment BugAttachmentBlock `json:"bugAttachment"`
	Formulas      []FormulaRow       `json:"formulas"`
	CostDaily     []CostDay          `json:"costDaily"`
	Phases        []PhaseRow         `json:"phases"`
	Failures      FailuresBlock      `json:"failures"`
	Models        []ModelRow         `json:"models"`
	Tools         []ToolRow          `json:"tools"`
	Aspirational  AspirationalBlock  `json:"aspirational"`
}

// HeroData powers the Hero strip. All durations are seconds, all costs
// USD. WoW deltas are fractional (0.12 = +12%). Zero values render as
// "no data" on the frontend; nulls are avoided so panels can rely on
// numeric math without guarding.
type HeroData struct {
	DeployFreqPerWeek         float64   `json:"deployFreqPerWeek"`
	DeployFreqWoWDelta        float64   `json:"deployFreqWoWDelta"`
	LeadTimeP50Seconds        float64   `json:"leadTimeP50Seconds"`
	LeadTimeWoWDelta          float64   `json:"leadTimeWoWDelta"`
	LeadTimeSparkline         []float64 `json:"leadTimeSparkline"`
	ChangeFailureRate         float64   `json:"changeFailureRate"`
	ChangeFailureRateWoWDelta float64   `json:"changeFailureRateWoWDelta"`
	MTTRSeconds               float64   `json:"mttrSeconds"`
	MTTRWoWDelta              float64   `json:"mttrWoWDelta"`
	CostPerWeekUSD            float64   `json:"costPerWeekUSD"`
	CostWoWDelta              float64   `json:"costWoWDelta"`
	ActiveAgents              int       `json:"activeAgents"`
	ActiveAgentsHighWater     int       `json:"activeAgentsHighWater"`
}

// ThroughputBlock wraps the weekly throughput rows.
type ThroughputBlock struct {
	Weekly []ThroughputWeek `json:"weekly"`
}

// ThroughputWeek is one row of the throughput panel — runs per week
// split into success vs failure, plus that week's lead-time p50.
type ThroughputWeek struct {
	WeekStart          string  `json:"weekStart"`
	RunsSuccess        int     `json:"runsSuccess"`
	RunsFailure        int     `json:"runsFailure"`
	LeadTimeP50Seconds float64 `json:"leadTimeP50Seconds"`
}

// LifecycleBlock wraps the per-type lifecycle breakdown.
type LifecycleBlock struct {
	ByType []LifecycleByType `json:"byType"`
}

// LifecycleByType holds the six lifecycle-stage quantile rows for a
// single bead type. Stage names are "F→R", "F→S", "F→C", "R→S",
// "R→C", "S→C" — mirroring the TS LifecycleStage union.
type LifecycleByType struct {
	Type   string                `json:"type"`
	Stages []LifecycleStageStats `json:"stages"`
}

// LifecycleStageStats holds quantile_cont results for one stage plus
// the outlier bead IDs whose duration exceeded p95.
type LifecycleStageStats struct {
	Stage    string             `json:"stage"`
	P50      float64            `json:"p50"`
	P75      float64            `json:"p75"`
	P95      float64            `json:"p95"`
	P99      float64            `json:"p99"`
	Outliers []LifecycleOutlier `json:"outliers"`
}

// LifecycleOutlier is a single outlier bead — an object, not a plain
// ID — to match the TS `{beadId, durationSeconds}` shape.
type LifecycleOutlier struct {
	BeadID          string  `json:"beadId"`
	DurationSeconds float64 `json:"durationSeconds"`
}

// BugAttachmentBlock wraps the weekly bug-attachment rows.
type BugAttachmentBlock struct {
	Weekly []BugAttachmentWeek `json:"weekly"`
}

// BugAttachmentWeek is one week of bug-attachment data: counts per
// parent type plus the top-N parents that attracted bugs that week.
type BugAttachmentWeek struct {
	WeekStart     string                      `json:"weekStart"`
	ByParentType  []BugAttachmentByParent     `json:"byParentType"`
	RecentParents []BugAttachmentRecentParent `json:"recentParents"`
}

// BugAttachmentByParent is one parent-type count row.
type BugAttachmentByParent struct {
	ParentType      string `json:"parentType"`
	Parents         int    `json:"parents"`
	ParentsWithBugs int    `json:"parentsWithBugs"`
}

// BugAttachmentRecentParent is a single parent bead that attracted
// bugs in the week, with its title for display.
type BugAttachmentRecentParent struct {
	BeadID   string `json:"beadId"`
	Title    string `json:"title"`
	BugCount int    `json:"bugCount"`
}

// FormulaRow is one row of the Formula Performance panel.
type FormulaRow struct {
	Name        string    `json:"name"`
	Runs        int       `json:"runs"`
	SuccessRate float64   `json:"successRate"`
	CostUSD     float64   `json:"costUSD"`
	RevsPerBead float64   `json:"revsPerBead"`
	Sparkline   []float64 `json:"sparkline"`
}

// CostDay is one day of cost + token + run count data with the three
// most expensive runs of the day.
type CostDay struct {
	Date    string          `json:"date"`
	CostUSD float64         `json:"costUSD"`
	Tokens  int64           `json:"tokens"`
	Runs    int             `json:"runs"`
	TopRuns []CostDayTopRun `json:"topRuns"`
}

// CostDayTopRun is one of the three most expensive runs for a day.
type CostDayTopRun struct {
	RunID   string  `json:"runId"`
	BeadID  string  `json:"beadId"`
	CostUSD float64 `json:"costUSD"`
}

// PhaseRow is one row of the Phase Funnel panel. ReachedFromStart is
// the fraction (0.0–1.0) of beads that reached this phase at least
// once — not the raw count.
type PhaseRow struct {
	Phase              string  `json:"phase"`
	Runs               int     `json:"runs"`
	SuccessRate        float64 `json:"successRate"`
	AvgCostUSD         float64 `json:"avgCostUSD"`
	AvgDurationSeconds float64 `json:"avgDurationSeconds"`
	ReachedFromStart   float64 `json:"reachedFromStart"`
}

// FailuresBlock holds the failure-classes breakdown and the top
// hotspot beads.
type FailuresBlock struct {
	Classes  []FailureClass   `json:"classes"`
	Hotspots []FailureHotspot `json:"hotspots"`
}

// FailureClass is one entry in the failure-class breakdown.
type FailureClass struct {
	Class      string  `json:"class"`
	Count      int     `json:"count"`
	Percentage float64 `json:"percentage"`
}

// FailureHotspot is one bead that has attracted repeated failures.
type FailureHotspot struct {
	BeadID           string `json:"beadId"`
	Title            string `json:"title"`
	Attempts         int    `json:"attempts"`
	LastFailureClass string `json:"lastFailureClass"`
	LastActivityIso  string `json:"lastActivityIso"`
}

// ModelRow is one row of the Model Mix panel.
type ModelRow struct {
	Model              string  `json:"model"`
	Runs               int     `json:"runs"`
	SuccessRate        float64 `json:"successRate"`
	CostUSD            float64 `json:"costUSD"`
	AvgDurationSeconds float64 `json:"avgDurationSeconds"`
	TotalTokens        int64   `json:"totalTokens"`
}

// ToolRow is one row of the Tool Usage panel.
type ToolRow struct {
	Tool          string  `json:"tool"`
	Calls         int     `json:"calls"`
	Failures      int     `json:"failures"`
	AvgDurationMs float64 `json:"avgDurationMs"`
}

// AspirationalBlock carries the optional "aspirational" overlay. When
// aspirational=false, both fields are nil. When aspirational=true and
// the live data is sparse, the overlay is synthesized to show the
// frontend a plausible-looking target shape. The UI badges the whole
// panel; individual rows are not flagged — the TS treats the overlay
// as a parallel panel.
type AspirationalBlock struct {
	LifecycleByType     []LifecycleByType   `json:"lifecycleByType,omitempty"`
	BugAttachmentWeekly []BugAttachmentWeek `json:"bugAttachmentWeekly,omitempty"`
}
