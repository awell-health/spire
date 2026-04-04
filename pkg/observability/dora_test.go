package observability

import (
	"math"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/store"
)

// ts returns an RFC3339 timestamp string offset by hours from a base time.
func ts(base time.Time, hoursOffset float64) string {
	return base.Add(time.Duration(hoursOffset * float64(time.Hour))).Format(time.RFC3339)
}

func TestComputeDORA_MergeFrequency(t *testing.T) {
	base := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC) // a Wednesday

	parents := []store.BoardBead{
		{ID: "p1", Status: "closed", ClosedAt: ts(base, 0), UpdatedAt: ts(base, 0)},
		{ID: "p2", Status: "closed", ClosedAt: ts(base, 1), UpdatedAt: ts(base, 1)},
		{ID: "p3", Status: "closed", ClosedAt: ts(base, 2), UpdatedAt: ts(base, 2)}, // last attempt failed
	}

	childMap := map[string][]store.BoardBead{
		"p1": {
			{ID: "a1", Title: "attempt: w1", Labels: []string{"attempt", "result:success"}, CreatedAt: ts(base, -2), UpdatedAt: ts(base, -0.5), ClosedAt: ts(base, -0.5)},
		},
		"p2": {
			{ID: "a2", Title: "attempt: w2", Labels: []string{"attempt", "result:success"}, CreatedAt: ts(base, -1), UpdatedAt: ts(base, 0.5), ClosedAt: ts(base, 0.5)},
		},
		"p3": {
			{ID: "a3", Title: "attempt: w3", Labels: []string{"attempt", "result:failure"}, CreatedAt: ts(base, 0), UpdatedAt: ts(base, 1.5), ClosedAt: ts(base, 1.5)},
		},
	}

	result := computeDORA(parents, childMap, DORAOpts{})

	// All 3 closed parents count as merges regardless of attempt result.
	if len(result.DeploymentFrequency) == 0 {
		t.Fatal("expected merge frequency data")
	}
	total := 0
	for _, wk := range result.DeploymentFrequency {
		total += wk.Merged
	}
	if total != 3 {
		t.Errorf("expected 3 merges, got %d", total)
	}
}

func TestComputeDORA_LeadTime(t *testing.T) {
	base := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)

	parents := []store.BoardBead{
		{ID: "p1", Status: "closed", ClosedAt: ts(base, 5), UpdatedAt: ts(base, 5)},
	}

	// Two attempts: first created at base-4h, last success closed at base+3h.
	// Lead time = 7 hours.
	childMap := map[string][]store.BoardBead{
		"p1": {
			{ID: "a1", Title: "attempt: w1", Labels: []string{"attempt", "result:failure"}, CreatedAt: ts(base, -4), UpdatedAt: ts(base, -3), ClosedAt: ts(base, -3)},
			{ID: "a2", Title: "attempt: w1", Labels: []string{"attempt", "result:success"}, CreatedAt: ts(base, 1), UpdatedAt: ts(base, 3), ClosedAt: ts(base, 3)},
		},
	}

	result := computeDORA(parents, childMap, DORAOpts{})

	if result.LeadTime == nil {
		t.Fatal("expected lead time data")
	}
	// Lead time = first attempt CreatedAt → last successful attempt ClosedAt = 7 hours
	if math.Abs(result.LeadTime.AvgHours-7.0) > 0.1 {
		t.Errorf("expected lead time ~7h, got %.1fh", result.LeadTime.AvgHours)
	}
	if result.LeadTime.Count != 1 {
		t.Errorf("expected 1 lead time measurement, got %d", result.LeadTime.Count)
	}
}

func TestComputeDORA_ChangeFailureRate(t *testing.T) {
	base := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)

	parents := []store.BoardBead{
		{ID: "p1", Status: "closed", ClosedAt: ts(base, 0)},
	}

	// 4 attempts: 1 failure, 1 timeout, 1 error, 1 success = 3/4 = 75% failure rate
	childMap := map[string][]store.BoardBead{
		"p1": {
			{ID: "a1", Title: "attempt: w1", Labels: []string{"attempt", "result:failure"}, UpdatedAt: ts(base, -4)},
			{ID: "a2", Title: "attempt: w1", Labels: []string{"attempt", "result:timeout"}, UpdatedAt: ts(base, -3)},
			{ID: "a3", Title: "attempt: w1", Labels: []string{"attempt", "result:error"}, UpdatedAt: ts(base, -2)},
			{ID: "a4", Title: "attempt: w1", Labels: []string{"attempt", "result:success"}, UpdatedAt: ts(base, -1)},
		},
	}

	result := computeDORA(parents, childMap, DORAOpts{})

	if result.ChangeFailureRate == nil {
		t.Fatal("expected change failure rate data")
	}
	if result.ChangeFailureRate.TotalAttempts != 4 {
		t.Errorf("expected 4 total attempts, got %d", result.ChangeFailureRate.TotalAttempts)
	}
	if result.ChangeFailureRate.Failures != 3 {
		t.Errorf("expected 3 failures, got %d", result.ChangeFailureRate.Failures)
	}
	if math.Abs(result.ChangeFailureRate.Rate-75.0) > 0.1 {
		t.Errorf("expected 75%% failure rate, got %.1f%%", result.ChangeFailureRate.Rate)
	}
}

func TestComputeDORA_MTTR(t *testing.T) {
	base := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)

	parents := []store.BoardBead{
		{ID: "p1", Status: "closed", ClosedAt: ts(base, 10)},
	}

	// Failed attempt closed at base+0, next success closed at base+2 → MTTR = 2h
	childMap := map[string][]store.BoardBead{
		"p1": {
			{ID: "a1", Title: "attempt: w1", Labels: []string{"attempt", "result:failure"}, CreatedAt: ts(base, -1), UpdatedAt: ts(base, 0), ClosedAt: ts(base, 0)},
			{ID: "a2", Title: "attempt: w1", Labels: []string{"attempt", "result:success"}, CreatedAt: ts(base, 1), UpdatedAt: ts(base, 2), ClosedAt: ts(base, 2)},
		},
	}

	result := computeDORA(parents, childMap, DORAOpts{})

	if result.MTTR == nil {
		t.Fatal("expected MTTR data")
	}
	if math.Abs(result.MTTR.AvgHours-2.0) > 0.1 {
		t.Errorf("expected MTTR ~2h, got %.1fh", result.MTTR.AvgHours)
	}
	if result.MTTR.Count != 1 {
		t.Errorf("expected 1 recovery event, got %d", result.MTTR.Count)
	}
}

func TestComputeDORA_RetryRate(t *testing.T) {
	base := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)

	parents := []store.BoardBead{
		{ID: "p1", Status: "closed", ClosedAt: ts(base, 0)},
		{ID: "p2", Status: "closed", ClosedAt: ts(base, 1)},
	}

	childMap := map[string][]store.BoardBead{
		"p1": {
			{ID: "a1", Title: "attempt: w1", Labels: []string{"attempt", "result:failure"}, UpdatedAt: ts(base, -2)},
			{ID: "a2", Title: "attempt: w1", Labels: []string{"attempt", "result:failure"}, UpdatedAt: ts(base, -1)},
			{ID: "a3", Title: "attempt: w1", Labels: []string{"attempt", "result:success"}, UpdatedAt: ts(base, 0)},
		},
		"p2": {
			{ID: "a4", Title: "attempt: w2", Labels: []string{"attempt", "result:success"}, UpdatedAt: ts(base, 0)},
		},
	}

	result := computeDORA(parents, childMap, DORAOpts{})

	if result.RetryRate == nil {
		t.Fatal("expected retry rate data")
	}
	if result.RetryRate.TotalParents != 2 {
		t.Errorf("expected 2 parents, got %d", result.RetryRate.TotalParents)
	}
	if result.RetryRate.TotalAttempts != 4 {
		t.Errorf("expected 4 attempts, got %d", result.RetryRate.TotalAttempts)
	}
	if math.Abs(result.RetryRate.AvgAttempts-2.0) > 0.1 {
		t.Errorf("expected 2.0 avg attempts, got %.1f", result.RetryRate.AvgAttempts)
	}
	if result.RetryRate.MaxAttempts != 3 {
		t.Errorf("expected max 3 attempts, got %d", result.RetryRate.MaxAttempts)
	}
}

func TestComputeDORA_ReviewFriction(t *testing.T) {
	base := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)

	// p2 has no reviews — must NOT dilute AvgPerParent.
	parents := []store.BoardBead{
		{ID: "p1", Status: "closed", ClosedAt: ts(base, 10)},
		{ID: "p2", Status: "closed", ClosedAt: ts(base, 11)},
	}

	// 2 review rounds on p1: first lasted 1h, second lasted 0.5h. p2 has no reviews.
	childMap := map[string][]store.BoardBead{
		"p1": {
			{ID: "a1", Title: "attempt: w1", Labels: []string{"attempt", "result:success"}, UpdatedAt: ts(base, 0)},
			{ID: "r1", Title: "review-round-1", Labels: []string{"review-round", "round:1"}, Status: "closed", CreatedAt: ts(base, 1), UpdatedAt: ts(base, 2), ClosedAt: ts(base, 2)},
			{ID: "r2", Title: "review-round-2", Labels: []string{"review-round", "round:2"}, Status: "closed", CreatedAt: ts(base, 3), UpdatedAt: ts(base, 3.5), ClosedAt: ts(base, 3.5)},
		},
		"p2": {
			{ID: "a2", Title: "attempt: w2", Labels: []string{"attempt", "result:success"}, UpdatedAt: ts(base, 11)},
		},
	}

	result := computeDORA(parents, childMap, DORAOpts{})

	if result.ReviewFriction == nil {
		t.Fatal("expected review friction data")
	}
	if result.ReviewFriction.TotalReviews != 2 {
		t.Errorf("expected 2 reviews, got %d", result.ReviewFriction.TotalReviews)
	}
	// Avg duration = (1h + 0.5h) / 2 = 0.75h
	if math.Abs(result.ReviewFriction.AvgDurationH-0.75) > 0.1 {
		t.Errorf("expected avg duration ~0.75h, got %.2fh", result.ReviewFriction.AvgDurationH)
	}
	// AvgPerParent = 2 reviews / 1 parent-with-reviews = 2.0 (NOT 1.0 = 2/2)
	if math.Abs(result.ReviewFriction.AvgPerParent-2.0) > 0.01 {
		t.Errorf("expected AvgPerParent 2.0, got %.2f", result.ReviewFriction.AvgPerParent)
	}
	if result.ReviewFriction.ParentsWithRev != 1 {
		t.Errorf("expected ParentsWithRev=1, got %d", result.ReviewFriction.ParentsWithRev)
	}
}

func TestComputeDORA_EscalationRate(t *testing.T) {
	base := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)

	parents := []store.BoardBead{
		{ID: "p1", Status: "closed", ClosedAt: ts(base, 0)},
		{ID: "p2", Status: "closed", ClosedAt: ts(base, 1)},
	}

	childMap := map[string][]store.BoardBead{
		"p1": {
			{ID: "a1", Title: "attempt: w1", Labels: []string{"attempt", "result:success"}, UpdatedAt: ts(base, 0)},
			{ID: "s1", Title: "step:arbiter", Labels: []string{"workflow-step", "step:arbiter"}, Status: "closed"},
		},
		"p2": {
			{ID: "a2", Title: "attempt: w2", Labels: []string{"attempt", "result:success"}, UpdatedAt: ts(base, 0)},
			{ID: "s2", Title: "step:arbiter", Labels: []string{"workflow-step", "step:arbiter"}, Status: "open"}, // not activated
		},
	}

	result := computeDORA(parents, childMap, DORAOpts{})

	if result.EscalationRate == nil {
		t.Fatal("expected escalation rate data")
	}
	if result.EscalationRate.Escalated != 1 {
		t.Errorf("expected 1 escalation, got %d", result.EscalationRate.Escalated)
	}
	if math.Abs(result.EscalationRate.Rate-50.0) > 0.1 {
		t.Errorf("expected 50%% escalation rate, got %.1f%%", result.EscalationRate.Rate)
	}
}

func TestComputeDORA_ModelEfficiency(t *testing.T) {
	base := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)

	parents := []store.BoardBead{
		{ID: "p1", Status: "closed", ClosedAt: ts(base, 0)},
	}

	childMap := map[string][]store.BoardBead{
		"p1": {
			{ID: "a1", Title: "attempt: w1", Labels: []string{"attempt", "result:success", "model:opus"}, UpdatedAt: ts(base, -2)},
			{ID: "a2", Title: "attempt: w1", Labels: []string{"attempt", "result:failure", "model:opus"}, UpdatedAt: ts(base, -1)},
			{ID: "a3", Title: "attempt: w1", Labels: []string{"attempt", "result:success", "model:sonnet"}, UpdatedAt: ts(base, 0)},
		},
	}

	result := computeDORA(parents, childMap, DORAOpts{ShowModel: true})

	if len(result.ModelEfficiency) != 2 {
		t.Fatalf("expected 2 models, got %d", len(result.ModelEfficiency))
	}
	// Models are sorted alphabetically: opus, sonnet.
	opus := result.ModelEfficiency[0]
	if opus.Model != "opus" {
		t.Errorf("expected model 'opus', got %q", opus.Model)
	}
	if opus.Total != 2 || opus.Succeeded != 1 {
		t.Errorf("opus: expected 2 total / 1 succeeded, got %d / %d", opus.Total, opus.Succeeded)
	}
	if math.Abs(opus.SuccessRate-50.0) > 0.1 {
		t.Errorf("expected opus 50%% success rate, got %.1f%%", opus.SuccessRate)
	}

	sonnet := result.ModelEfficiency[1]
	if sonnet.Model != "sonnet" || sonnet.Total != 1 || sonnet.Succeeded != 1 {
		t.Errorf("sonnet: expected 1/1 success, got %d/%d", sonnet.Total, sonnet.Succeeded)
	}
}

func TestComputeDORA_PhaseDuration(t *testing.T) {
	base := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)

	parents := []store.BoardBead{
		{ID: "p1", Status: "closed", ClosedAt: ts(base, 10)},
	}

	childMap := map[string][]store.BoardBead{
		"p1": {
			{ID: "a1", Title: "attempt: w1", Labels: []string{"attempt", "result:success"}, UpdatedAt: ts(base, 0)},
			{ID: "s1", Title: "step:plan", Labels: []string{"workflow-step", "step:plan"}, Status: "closed", CreatedAt: ts(base, 0), ClosedAt: ts(base, 0.5)},
			{ID: "s2", Title: "step:implement", Labels: []string{"workflow-step", "step:implement"}, Status: "closed", CreatedAt: ts(base, 0.5), ClosedAt: ts(base, 2.5)},
			{ID: "s3", Title: "step:review", Labels: []string{"workflow-step", "step:review"}, Status: "open", CreatedAt: ts(base, 2.5)}, // still open, excluded
		},
	}

	result := computeDORA(parents, childMap, DORAOpts{ShowPhase: true})

	if len(result.PhaseDuration) != 2 {
		t.Fatalf("expected 2 phases, got %d", len(result.PhaseDuration))
	}
	// Alphabetically: implement, plan
	impl := result.PhaseDuration[0]
	if impl.Phase != "implement" {
		t.Errorf("expected phase 'implement', got %q", impl.Phase)
	}
	if math.Abs(impl.AvgHours-2.0) > 0.1 {
		t.Errorf("expected implement ~2h, got %.1fh", impl.AvgHours)
	}

	plan := result.PhaseDuration[1]
	if plan.Phase != "plan" {
		t.Errorf("expected phase 'plan', got %q", plan.Phase)
	}
	if math.Abs(plan.AvgHours-0.5) > 0.1 {
		t.Errorf("expected plan ~0.5h, got %.1fh", plan.AvgHours)
	}
}

func TestComputeDORA_EmptyParents(t *testing.T) {
	result := computeDORA(nil, nil, DORAOpts{})

	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if len(result.DeploymentFrequency) != 0 {
		t.Errorf("expected empty deployment frequency")
	}
	if result.LeadTime != nil {
		t.Error("expected nil lead time")
	}
	if result.ChangeFailureRate != nil {
		t.Error("expected nil change failure rate")
	}
	if result.MTTR != nil {
		t.Error("expected nil MTTR")
	}
	if result.RetryRate != nil {
		t.Error("expected nil retry rate")
	}
}

func TestComputeDORA_MergeFrequency_PreDAGBead(t *testing.T) {
	base := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	parents := []store.BoardBead{
		{ID: "p1", Status: "closed", ClosedAt: ts(base, 0)},
	}
	childMap := map[string][]store.BoardBead{
		"p1": {}, // no attempt children — pre-DAG bead
	}
	result := computeDORA(parents, childMap, DORAOpts{})
	if len(result.DeploymentFrequency) == 0 {
		t.Fatal("expected merge frequency data for pre-DAG closed bead")
	}
	if result.DeploymentFrequency[0].Merged != 1 {
		t.Errorf("expected 1 merge, got %d", result.DeploymentFrequency[0].Merged)
	}
}

func TestComputeDORA_ChangeFailureRate_ReviewRejected(t *testing.T) {
	base := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	parents := []store.BoardBead{
		{ID: "p1", Status: "closed", ClosedAt: ts(base, 2)},
	}
	childMap := map[string][]store.BoardBead{
		"p1": {
			{ID: "a1", Title: "attempt: w1", Labels: []string{"attempt", "result:review_rejected"}, UpdatedAt: ts(base, 1)},
			{ID: "a2", Title: "attempt: w1", Labels: []string{"attempt", "result:success"}, UpdatedAt: ts(base, 2)},
		},
	}
	result := computeDORA(parents, childMap, DORAOpts{})
	if result.ChangeFailureRate == nil {
		t.Fatal("expected change failure rate data")
	}
	if result.ChangeFailureRate.TotalAttempts != 2 {
		t.Errorf("expected 2 total attempts, got %d", result.ChangeFailureRate.TotalAttempts)
	}
	if result.ChangeFailureRate.Failures != 1 {
		t.Errorf("expected 1 failure (review_rejected), got %d", result.ChangeFailureRate.Failures)
	}
}

func TestComputeDORA_ParentsWithNoAttempts(t *testing.T) {
	base := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)

	parents := []store.BoardBead{
		{ID: "p1", Status: "closed", ClosedAt: ts(base, 0)},
	}
	childMap := map[string][]store.BoardBead{
		"p1": {}, // no children at all
	}

	result := computeDORA(parents, childMap, DORAOpts{})

	// No attempts → still counts as merge (closed parent = merge), but no lead time, no failure rate.
	if len(result.DeploymentFrequency) == 0 {
		t.Fatal("expected merge frequency data for parent with no attempts")
	}
	if result.DeploymentFrequency[0].Merged != 1 {
		t.Errorf("expected 1 merge, got %d", result.DeploymentFrequency[0].Merged)
	}
	if result.LeadTime != nil {
		t.Error("expected nil lead time for parent with no attempts")
	}
	// EscalationRate should still be computed (0/1).
	if result.EscalationRate == nil {
		t.Fatal("expected escalation rate even with no attempts")
	}
	if result.EscalationRate.Escalated != 0 {
		t.Errorf("expected 0 escalations, got %d", result.EscalationRate.Escalated)
	}
}
