package report

import "testing"

func TestSynthesizeLifecycle_SkipsMissingStages(t *testing.T) {
	in := []LifecycleByType{{
		Type: "bug",
		Stages: []LifecycleStageStats{
			// Only F→C has data — everything else is zero.
			{Stage: "F→R", Outliers: []LifecycleOutlier{}},
			{Stage: "F→S", Outliers: []LifecycleOutlier{}},
			{Stage: "F→C", P50: 500, P75: 1000, P95: 2000, P99: 4000, Outliers: []LifecycleOutlier{}},
			{Stage: "R→S", Outliers: []LifecycleOutlier{}},
			{Stage: "R→C", Outliers: []LifecycleOutlier{}},
			{Stage: "S→C", Outliers: []LifecycleOutlier{}},
		},
	}}
	out := synthesizeLifecycle(in)
	if len(out) != 1 {
		t.Fatalf("out length = %d", len(out))
	}
	// R→C and S→C should both be 500 (F→C - 0). R→S = 0 (F→S - F→R = 0).
	stages := out[0].Stages
	rs, rc, sc := stages[3], stages[4], stages[5]
	if rs.P50 != 0 {
		t.Errorf("R→S p50 = %v, want 0", rs.P50)
	}
	if rc.P50 != 500 {
		t.Errorf("R→C p50 = %v, want 500", rc.P50)
	}
	if sc.P50 != 500 {
		t.Errorf("S→C p50 = %v, want 500", sc.P50)
	}
}

func TestSynthesizeLifecycle_PreservesPopulatedStages(t *testing.T) {
	// When R→S is already populated, it should NOT be overwritten.
	in := []LifecycleByType{{
		Type: "task",
		Stages: []LifecycleStageStats{
			{Stage: "F→R", P50: 100, P75: 200, P95: 400, P99: 800, Outliers: []LifecycleOutlier{}},
			{Stage: "F→S", P50: 300, P75: 600, P95: 1200, P99: 2400, Outliers: []LifecycleOutlier{}},
			{Stage: "F→C", P50: 600, P75: 1200, P95: 2400, P99: 4800, Outliers: []LifecycleOutlier{}},
			{Stage: "R→S", P50: 42, P75: 84, P95: 168, P99: 336, Outliers: []LifecycleOutlier{}},
			{Stage: "R→C", Outliers: []LifecycleOutlier{}}, // zero → synth
			{Stage: "S→C", Outliers: []LifecycleOutlier{}}, // zero → synth
		},
	}}
	out := synthesizeLifecycle(in)
	if got := out[0].Stages[3].P50; got != 42 {
		t.Errorf("R→S p50 was overwritten: got %v, want 42 (live data preserved)", got)
	}
}

func TestNonNeg(t *testing.T) {
	if got := nonNeg(-1.5); got != 0 {
		t.Errorf("nonNeg(-1.5) = %v, want 0", got)
	}
	if got := nonNeg(3.14); got != 3.14 {
		t.Errorf("nonNeg(3.14) = %v, want 3.14", got)
	}
}

func TestSynthesizeBugAttachment_CopiesWithEmptyRecent(t *testing.T) {
	in := []BugAttachmentWeek{{
		WeekStart: "2026-04-13",
		ByParentType: []BugAttachmentByParent{
			{ParentType: "task", Parents: 10, ParentsWithBugs: 2},
		},
		RecentParents: []BugAttachmentRecentParent{
			{BeadID: "spi-1", Title: "live data", BugCount: 3},
		},
	}}
	out := synthesizeBugAttachment(in)
	if len(out) != 1 {
		t.Fatalf("out length = %d", len(out))
	}
	if len(out[0].RecentParents) != 0 {
		t.Errorf("RecentParents should be empty in synth, got %d entries", len(out[0].RecentParents))
	}
	if len(out[0].ByParentType) != 1 || out[0].ByParentType[0].Parents != 10 {
		t.Errorf("byParentType was not copied correctly: %+v", out[0].ByParentType)
	}
}

func TestPopulated(t *testing.T) {
	zero := LifecycleStageStats{}
	if populated(zero) {
		t.Error("zero stage should not be populated")
	}
	withP50 := LifecycleStageStats{P50: 0.1}
	if !populated(withP50) {
		t.Error("stage with non-zero p50 should be populated")
	}
}
