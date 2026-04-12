package main

import (
	"testing"

	"github.com/awell-health/spire/pkg/observability"
)

func TestMapPhaseToStep(t *testing.T) {
	steps := map[string]bool{
		"plan":      true,
		"implement": true,
		"review":    true,
		"merge":     true,
		"close":     true,
	}

	tests := []struct {
		name        string
		phase       string
		phaseBucket string
		stepNames   map[string]bool
		want        string
	}{
		// Direct match cases.
		{"direct match implement", "implement", "", steps, "implement"},
		{"direct match review", "review", "", steps, "review"},
		{"direct match plan", "plan", "", steps, "plan"},
		{"direct match merge", "merge", "", steps, "merge"},
		{"direct match close", "close", "", steps, "close"},

		// Bucket from phase_bucket column (DB provides it).
		{"explicit bucket implement", "build-fix", "implement", steps, "implement"},
		{"explicit bucket review", "sage-review", "review", steps, "review"},
		{"explicit bucket design", "validate-design", "design", steps, "plan"},

		// Bucket inferred from phase name when phase_bucket is empty.
		{"inferred bucket build-fix", "build-fix", "", steps, "implement"},
		{"inferred bucket sage-review", "sage-review", "", steps, "review"},
		{"inferred bucket review-fix", "review-fix", "", steps, "review"},
		{"inferred bucket validate-design", "validate-design", "", steps, "plan"},
		{"inferred bucket enrich-subtasks", "enrich-subtasks", "", steps, "plan"},
		{"inferred bucket auto-approve", "auto-approve", "", steps, "plan"},
		{"inferred bucket skip", "skip", "", steps, "plan"},
		{"inferred bucket waitForHuman", "waitForHuman", "", steps, "plan"},

		// Unknown phase returns empty string.
		{"unknown phase no bucket", "something-unknown", "", steps, ""},
		{"unknown phase unknown bucket", "something-unknown", "unknown-bucket", steps, ""},

		// Empty step names map — nothing matches.
		{"empty step names", "implement", "", map[string]bool{}, ""},

		// Step names without the target — bucket resolves but step doesn't exist.
		{"bucket resolves but step missing", "sage-review", "review", map[string]bool{"plan": true, "implement": true}, ""},

		// Nil step names map.
		{"nil step names", "implement", "", nil, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapPhaseToStep(tt.phase, tt.phaseBucket, tt.stepNames)
			if got != tt.want {
				t.Errorf("mapPhaseToStep(%q, %q, ...) = %q, want %q",
					tt.phase, tt.phaseBucket, got, tt.want)
			}
		})
	}
}

func TestFormatCost(t *testing.T) {
	tests := []struct {
		name string
		cost float64
		want string
	}{
		{"zero", 0.0, "0.0000"},
		{"sub-penny small", 0.001, "0.0010"},
		{"sub-penny boundary", 0.009, "0.0090"},
		{"sub-penny just under", 0.0099, "0.0099"},
		{"exactly one cent", 0.01, "0.01"},
		{"just over one cent", 0.011, "0.01"},
		{"typical small cost", 0.12, "0.12"},
		{"typical medium cost", 1.30, "1.30"},
		{"larger cost", 12.50, "12.50"},
		{"very small", 0.0001, "0.0001"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatCost(tt.cost)
			if got != tt.want {
				t.Errorf("formatCost(%v) = %q, want %q", tt.cost, got, tt.want)
			}
		})
	}
}

func TestFormatTokensK(t *testing.T) {
	tests := []struct {
		name string
		n    int
		want string
	}{
		{"zero", 0, "0"},
		{"small number", 500, "500"},
		{"just under 1K", 999, "999"},
		{"exactly 1K", 1000, "1K"},
		{"just over 1K", 1001, "1K"},
		{"typical in tokens", 12000, "12K"},
		{"typical out tokens", 2000, "2K"},
		{"large count", 89000, "89K"},
		{"very large", 150000, "150K"},
		{"1500 rounds down", 1500, "1K"},
		{"1999 rounds down", 1999, "1K"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatTokensK(tt.n)
			if got != tt.want {
				t.Errorf("formatTokensK(%d) = %q, want %q", tt.n, got, tt.want)
			}
		})
	}
}

func TestHasStepMetrics(t *testing.T) {
	tests := []struct {
		name  string
		steps []traceStep
		want  bool
	}{
		{"nil steps", nil, false},
		{"empty steps", []traceStep{}, false},
		{"all zero", []traceStep{
			{Name: "plan", Status: "open"},
			{Name: "implement", Status: "open"},
		}, false},
		{"tool calls only — no duration/cost/tokens", []traceStep{
			{Name: "implement", Status: "closed", ReadCalls: 5, EditCalls: 2, WriteCalls: 1},
		}, false},
		{"has duration", []traceStep{
			{Name: "plan", Status: "closed", Duration: 42},
		}, true},
		{"has cost", []traceStep{
			{Name: "plan", Status: "closed", CostUSD: 0.12},
		}, true},
		{"has tokens in", []traceStep{
			{Name: "plan", Status: "closed", TokensIn: 8000},
		}, true},
		{"has tokens out", []traceStep{
			{Name: "plan", Status: "closed", TokensOut: 2000},
		}, true},
		{"mixed — one step with data, one without", []traceStep{
			{Name: "plan", Status: "open"},
			{Name: "implement", Status: "closed", Duration: 200, CostUSD: 1.30},
		}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasStepMetrics(tt.steps)
			if got != tt.want {
				t.Errorf("hasStepMetrics(...) = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPopulateStepMetrics_Aggregation(t *testing.T) {
	// Test the aggregation logic with completed runs (deterministic — no time.Since).
	td := &traceData{
		ID: "spi-test",
		Steps: []traceStep{
			{Name: "plan", Status: "closed"},
			{Name: "implement", Status: "closed"},
			{Name: "review", Status: "closed"},
			{Name: "merge", Status: "open"},
		},
	}

	runs := []observability.StepRunRow{
		// Plan phase — single run.
		{
			Phase: "plan", PhaseBucket: "design",
			Duration: 42, CostUSD: 0.12,
			TokensIn: 8000, TokensOut: 2000,
			ToolCallsJSON: `{"Read": 8, "Edit": 0, "Write": 0}`,
			StartedAt: "2026-04-12T04:00:00Z", CompletedAt: "2026-04-12T04:00:42Z",
		},
		// Implement phase — two runs (retry).
		{
			Phase: "implement", PhaseBucket: "implement",
			Duration: 120, CostUSD: 0.80,
			TokensIn: 50000, TokensOut: 5000,
			ToolCallsJSON: `{"Read": 20, "Edit": 8, "Write": 2}`,
			StartedAt: "2026-04-12T04:01:00Z", CompletedAt: "2026-04-12T04:03:00Z",
		},
		{
			Phase: "build-fix", PhaseBucket: "implement",
			Duration: 80, CostUSD: 0.50,
			TokensIn: 39000, TokensOut: 3000,
			ToolCallsJSON: `{"Read": 11, "Edit": 4, "Write": 1}`,
			StartedAt: "2026-04-12T04:03:00Z", CompletedAt: "2026-04-12T04:04:20Z",
		},
		// Review phase — sage-review.
		{
			Phase: "sage-review", PhaseBucket: "review",
			Duration: 55, CostUSD: 0.30,
			TokensIn: 15000, TokensOut: 1000,
			ToolCallsJSON: `{"Read": 5, "Edit": 0, "Write": 0}`,
			StartedAt: "2026-04-12T04:05:00Z", CompletedAt: "2026-04-12T04:05:55Z",
		},
	}

	// Call the aggregation logic directly by simulating what populateStepMetrics does.
	// We can't call populateStepMetrics directly because it calls StepMetricsForBead (DB).
	// Instead, test the pure aggregation: mapPhaseToStep + tool_calls_json parsing.
	populateStepMetricsFromRuns(td, runs)

	// Verify plan step.
	plan := td.Steps[0]
	if plan.Duration != 42 {
		t.Errorf("plan.Duration = %d, want 42", plan.Duration)
	}
	if plan.CostUSD != 0.12 {
		t.Errorf("plan.CostUSD = %f, want 0.12", plan.CostUSD)
	}
	if plan.TokensIn != 8000 {
		t.Errorf("plan.TokensIn = %d, want 8000", plan.TokensIn)
	}
	if plan.TokensOut != 2000 {
		t.Errorf("plan.TokensOut = %d, want 2000", plan.TokensOut)
	}
	if plan.ReadCalls != 8 {
		t.Errorf("plan.ReadCalls = %d, want 8", plan.ReadCalls)
	}

	// Verify implement step (aggregated from implement + build-fix).
	impl := td.Steps[1]
	if impl.Duration != 200 {
		t.Errorf("implement.Duration = %d, want 200 (120+80)", impl.Duration)
	}
	if impl.CostUSD != 1.30 {
		t.Errorf("implement.CostUSD = %f, want 1.30 (0.80+0.50)", impl.CostUSD)
	}
	if impl.TokensIn != 89000 {
		t.Errorf("implement.TokensIn = %d, want 89000 (50000+39000)", impl.TokensIn)
	}
	if impl.TokensOut != 8000 {
		t.Errorf("implement.TokensOut = %d, want 8000 (5000+3000)", impl.TokensOut)
	}
	if impl.ReadCalls != 31 {
		t.Errorf("implement.ReadCalls = %d, want 31 (20+11)", impl.ReadCalls)
	}
	if impl.EditCalls != 12 {
		t.Errorf("implement.EditCalls = %d, want 12 (8+4)", impl.EditCalls)
	}
	if impl.WriteCalls != 3 {
		t.Errorf("implement.WriteCalls = %d, want 3 (2+1)", impl.WriteCalls)
	}

	// Verify review step.
	review := td.Steps[2]
	if review.Duration != 55 {
		t.Errorf("review.Duration = %d, want 55", review.Duration)
	}
	if review.CostUSD != 0.30 {
		t.Errorf("review.CostUSD = %f, want 0.30", review.CostUSD)
	}
	if review.ReadCalls != 5 {
		t.Errorf("review.ReadCalls = %d, want 5", review.ReadCalls)
	}

	// Verify merge step — no runs, should remain zero.
	merge := td.Steps[3]
	if merge.Duration != 0 || merge.CostUSD != 0 || merge.TokensIn != 0 {
		t.Errorf("merge should have no metrics, got duration=%d cost=%f tokensIn=%d",
			merge.Duration, merge.CostUSD, merge.TokensIn)
	}
}

func TestPopulateStepMetrics_EmptyRuns(t *testing.T) {
	td := &traceData{
		ID: "spi-test",
		Steps: []traceStep{
			{Name: "plan", Status: "open"},
		},
	}
	// Empty runs should be a no-op.
	populateStepMetricsFromRuns(td, nil)
	if td.Steps[0].Duration != 0 {
		t.Error("expected no metrics with nil runs")
	}
}

func TestPopulateStepMetrics_MalformedToolCallsJSON(t *testing.T) {
	td := &traceData{
		ID: "spi-test",
		Steps: []traceStep{
			{Name: "implement", Status: "closed"},
		},
	}
	runs := []observability.StepRunRow{
		{
			Phase: "implement", PhaseBucket: "implement",
			Duration: 100, CostUSD: 0.50,
			TokensIn: 10000, TokensOut: 1000,
			ToolCallsJSON: `not valid json`,
			StartedAt: "2026-04-12T04:00:00Z", CompletedAt: "2026-04-12T04:01:40Z",
		},
	}
	// Should not panic, just skip tool call parsing.
	populateStepMetricsFromRuns(td, runs)
	if td.Steps[0].Duration != 100 {
		t.Errorf("duration = %d, want 100", td.Steps[0].Duration)
	}
	if td.Steps[0].ReadCalls != 0 {
		t.Errorf("readCalls = %d, want 0 (malformed JSON)", td.Steps[0].ReadCalls)
	}
}

func TestPopulateStepMetrics_UnknownPhaseSkipped(t *testing.T) {
	td := &traceData{
		ID: "spi-test",
		Steps: []traceStep{
			{Name: "plan", Status: "closed"},
		},
	}
	runs := []observability.StepRunRow{
		{
			Phase: "totally-unknown", PhaseBucket: "",
			Duration: 100, CostUSD: 0.50,
			TokensIn: 10000, TokensOut: 1000,
			StartedAt: "2026-04-12T04:00:00Z", CompletedAt: "2026-04-12T04:01:40Z",
		},
	}
	populateStepMetricsFromRuns(td, runs)
	// Unknown phase should be skipped — plan remains zero.
	if td.Steps[0].Duration != 0 {
		t.Errorf("plan.Duration = %d, want 0 (unknown phase should be skipped)", td.Steps[0].Duration)
	}
}
