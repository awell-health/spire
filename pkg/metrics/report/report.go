package report

import (
	"context"
	"time"
)

// Options carry the parsed request inputs into Build().
type Options struct {
	Scope        Scope
	Window       Window
	Aspirational bool
	Now          time.Time // generatedAt; defaults to time.Now() when zero
}

// Build assembles the full /api/v1/metrics payload. It calls the
// reader once per panel; panels that fail cause Build to return an
// error — this surfaces as a 500 so the frontend's fixture fallback
// keeps the view usable.
//
// Each panel is evaluated sequentially since (a) DuckDB is
// single-writer-multi-reader but the read path is fast enough to
// serialize without hurting the <500ms target and (b) context
// cancellation propagates cleanly through sequential SQL calls.
func Build(ctx context.Context, r Reader, opts Options) (*MetricsResponse, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	resp := &MetricsResponse{
		Scope:       opts.Scope.String(),
		Window:      opts.Window.Range,
		GeneratedAt: now.UTC().Format(time.RFC3339),
		// Aspirational defaults to empty (both sub-fields omitempty so
		// the wire shape is `"aspirational":{}` when neither is filled).
		Aspirational: AspirationalBlock{},
	}

	throughput, err := r.QueryThroughputWeekly(ctx, opts.Scope, opts.Window.Since, opts.Window.Until)
	if err != nil {
		return nil, err
	}
	resp.Throughput = ThroughputBlock{Weekly: throughput}

	// Build hero from the throughput rows + three targeted queries.
	hero, err := buildHero(ctx, r, opts, throughput)
	if err != nil {
		return nil, err
	}
	resp.Hero = hero

	lifecycle, err := r.QueryLifecycleByType(ctx, opts.Scope, opts.Window.Since, opts.Window.Until)
	if err != nil {
		return nil, err
	}
	resp.Lifecycle = LifecycleBlock{ByType: lifecycle}

	bugAttach, err := r.QueryBugAttachmentWeekly(ctx, opts.Scope, opts.Window.Since, opts.Window.Until)
	if err != nil {
		return nil, err
	}
	resp.BugAttachment = BugAttachmentBlock{Weekly: bugAttach}

	formulas, err := r.QueryFormulas(ctx, opts.Scope, opts.Window.Since, opts.Window.Until)
	if err != nil {
		return nil, err
	}
	resp.Formulas = formulas

	costDaily, err := r.QueryCostDaily(ctx, opts.Scope, opts.Window.Since, opts.Window.Until)
	if err != nil {
		return nil, err
	}
	resp.CostDaily = costDaily

	phases, err := r.QueryPhases(ctx, opts.Scope, opts.Window.Since, opts.Window.Until)
	if err != nil {
		return nil, err
	}
	resp.Phases = phases

	failures, err := r.QueryFailures(ctx, opts.Scope, opts.Window.Since, opts.Window.Until)
	if err != nil {
		return nil, err
	}
	resp.Failures = failures

	models, err := r.QueryModels(ctx, opts.Scope, opts.Window.Since, opts.Window.Until)
	if err != nil {
		return nil, err
	}
	resp.Models = models

	tools, err := r.QueryTools(ctx, opts.Scope, opts.Window.Since, opts.Window.Until)
	if err != nil {
		return nil, err
	}
	resp.Tools = tools

	// Fill in the aspirational overlay last so it can reference the
	// live panel data when deciding what to synthesize.
	if opts.Aspirational {
		resp.Aspirational = buildAspirational(lifecycle, bugAttach)
	}

	// Empty-array safety — the TS contract expects arrays not null.
	if resp.Formulas == nil {
		resp.Formulas = []FormulaRow{}
	}
	if resp.CostDaily == nil {
		resp.CostDaily = []CostDay{}
	}
	if resp.Phases == nil {
		resp.Phases = []PhaseRow{}
	}
	if resp.Models == nil {
		resp.Models = []ModelRow{}
	}
	if resp.Tools == nil {
		resp.Tools = []ToolRow{}
	}
	if resp.Lifecycle.ByType == nil {
		resp.Lifecycle.ByType = []LifecycleByType{}
	}
	if resp.BugAttachment.Weekly == nil {
		resp.BugAttachment.Weekly = []BugAttachmentWeek{}
	}
	if resp.Throughput.Weekly == nil {
		resp.Throughput.Weekly = []ThroughputWeek{}
	}
	if resp.Failures.Classes == nil {
		resp.Failures.Classes = []FailureClass{}
	}
	if resp.Failures.Hotspots == nil {
		resp.Failures.Hotspots = []FailureHotspot{}
	}
	return resp, nil
}

// buildHero packs the Hero strip. Deploy freq / lead-time / failure
// rate / WoW deltas are computed from the throughput rows (which
// already span 12 weeks). Cost and MTTR query the reader directly so
// they can span windows independently.
func buildHero(ctx context.Context, r Reader, opts Options, weekly []ThroughputWeek) (HeroData, error) {
	h := HeroData{LeadTimeSparkline: make([]float64, 0, len(weekly))}
	for _, w := range weekly {
		h.LeadTimeSparkline = append(h.LeadTimeSparkline, w.LeadTimeP50Seconds)
	}

	// Deploy freq & failure rate — last week vs prior week.
	var curr, prev ThroughputWeek
	if n := len(weekly); n >= 1 {
		curr = weekly[n-1]
	}
	if n := len(weekly); n >= 2 {
		prev = weekly[n-2]
	}
	currDeploys := float64(curr.RunsSuccess)
	prevDeploys := float64(prev.RunsSuccess)
	h.DeployFreqPerWeek = currDeploys
	h.DeployFreqWoWDelta = wowDelta(currDeploys, prevDeploys)

	h.LeadTimeP50Seconds = curr.LeadTimeP50Seconds
	h.LeadTimeWoWDelta = wowDelta(curr.LeadTimeP50Seconds, prev.LeadTimeP50Seconds)

	currTotal := currDeploys + float64(curr.RunsFailure)
	prevTotal := prevDeploys + float64(prev.RunsFailure)
	if currTotal > 0 {
		h.ChangeFailureRate = float64(curr.RunsFailure) / currTotal
	}
	var prevRate float64
	if prevTotal > 0 {
		prevRate = float64(prev.RunsFailure) / prevTotal
	}
	h.ChangeFailureRateWoWDelta = wowDelta(h.ChangeFailureRate, prevRate)

	// Cost per week.
	currCost, prevCost, err := r.QueryHeroCostByWeek(ctx, opts.Scope, opts.Window.Since, opts.Window.Until)
	if err != nil {
		return h, err
	}
	h.CostPerWeekUSD = currCost
	h.CostWoWDelta = wowDelta(currCost, prevCost)

	// MTTR.
	currMTTR, prevMTTR, err := r.QueryHeroMTTR(ctx, opts.Scope, opts.Window.Since, opts.Window.Until)
	if err != nil {
		return h, err
	}
	h.MTTRSeconds = currMTTR
	h.MTTRWoWDelta = wowDelta(currMTTR, prevMTTR)

	// Active agents.
	now, hw, err := r.QueryHeroActiveAgents(ctx, opts.Scope, opts.Window.Since, opts.Window.Until)
	if err != nil {
		return h, err
	}
	h.ActiveAgents = now
	h.ActiveAgentsHighWater = hw

	return h, nil
}

// wowDelta returns the fractional change (this - prev) / prev. Zero
// prev returns 0 (not +inf) so the JSON stays clean.
func wowDelta(this, prev float64) float64 {
	if prev == 0 {
		return 0
	}
	return (this - prev) / prev
}
