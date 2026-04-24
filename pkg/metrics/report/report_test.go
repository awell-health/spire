package report

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// fakeReader is an in-memory Reader for testing Build() without a
// DuckDB dependency. Each field stores the value returned by the
// corresponding Reader method; zero values work like "no data".
type fakeReader struct {
	throughput []ThroughputWeek
	lifecycle  []LifecycleByType
	bugAttach  []BugAttachmentWeek
	formulas   []FormulaRow
	costDaily  []CostDay
	phases     []PhaseRow
	failures   FailuresBlock
	models     []ModelRow
	tools      []ToolRow

	activeNow, activeHW       int
	costThis, costPrev        float64
	mttrThis, mttrPrev        float64

	failPanel string // when non-empty, the named panel's Query method returns an error
}

func (f *fakeReader) panelErr(name string) error {
	if f.failPanel == name {
		return errors.New("fake " + name + " error")
	}
	return nil
}

func (f *fakeReader) QueryThroughputWeekly(context.Context, Scope, time.Time, time.Time) ([]ThroughputWeek, error) {
	return f.throughput, f.panelErr("throughput")
}
func (f *fakeReader) QueryHeroActiveAgents(context.Context, Scope, time.Time, time.Time) (int, int, error) {
	return f.activeNow, f.activeHW, f.panelErr("activeAgents")
}
func (f *fakeReader) QueryHeroCostByWeek(context.Context, Scope, time.Time, time.Time) (float64, float64, error) {
	return f.costThis, f.costPrev, f.panelErr("cost")
}
func (f *fakeReader) QueryHeroMTTR(context.Context, Scope, time.Time, time.Time) (float64, float64, error) {
	return f.mttrThis, f.mttrPrev, f.panelErr("mttr")
}
func (f *fakeReader) QueryLifecycleByType(context.Context, Scope, time.Time, time.Time) ([]LifecycleByType, error) {
	return f.lifecycle, f.panelErr("lifecycle")
}
func (f *fakeReader) QueryBugAttachmentWeekly(context.Context, Scope, time.Time, time.Time) ([]BugAttachmentWeek, error) {
	return f.bugAttach, f.panelErr("bugAttach")
}
func (f *fakeReader) QueryFormulas(context.Context, Scope, time.Time, time.Time) ([]FormulaRow, error) {
	return f.formulas, f.panelErr("formulas")
}
func (f *fakeReader) QueryCostDaily(context.Context, Scope, time.Time, time.Time) ([]CostDay, error) {
	return f.costDaily, f.panelErr("costDaily")
}
func (f *fakeReader) QueryPhases(context.Context, Scope, time.Time, time.Time) ([]PhaseRow, error) {
	return f.phases, f.panelErr("phases")
}
func (f *fakeReader) QueryFailures(context.Context, Scope, time.Time, time.Time) (FailuresBlock, error) {
	return f.failures, f.panelErr("failures")
}
func (f *fakeReader) QueryModels(context.Context, Scope, time.Time, time.Time) ([]ModelRow, error) {
	return f.models, f.panelErr("models")
}
func (f *fakeReader) QueryTools(context.Context, Scope, time.Time, time.Time) ([]ToolRow, error) {
	return f.tools, f.panelErr("tools")
}

func TestBuild_EmptyTower(t *testing.T) {
	r := &fakeReader{}
	win, err := ParseWindow("7d", "", "", time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{
		Scope:  Scope{},
		Window: win,
	}
	resp, err := Build(context.Background(), r, opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// The TS contract expects arrays not null — verify each panel
	// produces a non-nil slice even when the data source is empty.
	if resp.Throughput.Weekly == nil {
		t.Error("throughput.weekly is nil, want []")
	}
	if resp.Lifecycle.ByType == nil {
		t.Error("lifecycle.byType is nil, want []")
	}
	if resp.BugAttachment.Weekly == nil {
		t.Error("bugAttachment.weekly is nil, want []")
	}
	if resp.Formulas == nil {
		t.Error("formulas is nil, want []")
	}
	if resp.CostDaily == nil {
		t.Error("costDaily is nil, want []")
	}
	if resp.Phases == nil {
		t.Error("phases is nil, want []")
	}
	if resp.Models == nil {
		t.Error("models is nil, want []")
	}
	if resp.Tools == nil {
		t.Error("tools is nil, want []")
	}
	if resp.Failures.Classes == nil || resp.Failures.Hotspots == nil {
		t.Error("failures fields should be non-nil")
	}
	if resp.Scope != "all" {
		t.Errorf("scope = %q, want all", resp.Scope)
	}
	if resp.Window != "7d" {
		t.Errorf("window = %q, want 7d", resp.Window)
	}
}

func TestBuild_HeroWoWDelta(t *testing.T) {
	// Two-week throughput: previous 10 successes + 2 failures, current
	// 20 successes + 3 failures. Deploy freq WoW delta should be +100%.
	r := &fakeReader{
		throughput: []ThroughputWeek{
			{WeekStart: "2026-04-13", RunsSuccess: 10, RunsFailure: 2, LeadTimeP50Seconds: 3600},
			{WeekStart: "2026-04-20", RunsSuccess: 20, RunsFailure: 3, LeadTimeP50Seconds: 1800},
		},
	}
	win, _ := ParseWindow("7d", "", "", time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC))
	resp, err := Build(context.Background(), r, Options{Window: win})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if resp.Hero.DeployFreqPerWeek != 20 {
		t.Errorf("deployFreqPerWeek = %v, want 20", resp.Hero.DeployFreqPerWeek)
	}
	if resp.Hero.DeployFreqWoWDelta != 1.0 {
		t.Errorf("deployFreqWoWDelta = %v, want 1.0", resp.Hero.DeployFreqWoWDelta)
	}
	// Lead time went from 3600 to 1800 — a 50% drop.
	if resp.Hero.LeadTimeP50Seconds != 1800 {
		t.Errorf("leadTimeP50Seconds = %v, want 1800", resp.Hero.LeadTimeP50Seconds)
	}
	if resp.Hero.LeadTimeWoWDelta != -0.5 {
		t.Errorf("leadTimeWoWDelta = %v, want -0.5", resp.Hero.LeadTimeWoWDelta)
	}
	// Sparkline should mirror the lead-time values in order.
	if len(resp.Hero.LeadTimeSparkline) != 2 {
		t.Errorf("sparkline length = %d, want 2", len(resp.Hero.LeadTimeSparkline))
	}
}

func TestBuild_AspirationalFillsMissingStages(t *testing.T) {
	// Live lifecycle has only F→R, F→S, F→C populated. With
	// aspirational=true, the overlay should synth R→S, R→C, S→C
	// based on the differences.
	r := &fakeReader{
		lifecycle: []LifecycleByType{{
			Type: "task",
			Stages: []LifecycleStageStats{
				{Stage: "F→R", P50: 100, P75: 200, P95: 400, P99: 800, Outliers: []LifecycleOutlier{}},
				{Stage: "F→S", P50: 300, P75: 600, P95: 1200, P99: 2400, Outliers: []LifecycleOutlier{}},
				{Stage: "F→C", P50: 600, P75: 1200, P95: 2400, P99: 4800, Outliers: []LifecycleOutlier{}},
				{Stage: "R→S", P50: 0, P75: 0, P95: 0, P99: 0, Outliers: []LifecycleOutlier{}},
				{Stage: "R→C", P50: 0, P75: 0, P95: 0, P99: 0, Outliers: []LifecycleOutlier{}},
				{Stage: "S→C", P50: 0, P75: 0, P95: 0, P99: 0, Outliers: []LifecycleOutlier{}},
			},
		}},
	}
	win, _ := ParseWindow("7d", "", "", time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC))
	resp, err := Build(context.Background(), r, Options{Window: win, Aspirational: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(resp.Aspirational.LifecycleByType) != 1 {
		t.Fatalf("aspirational.lifecycleByType length = %d, want 1", len(resp.Aspirational.LifecycleByType))
	}
	task := resp.Aspirational.LifecycleByType[0]
	if task.Type != "task" {
		t.Errorf("type = %q, want task", task.Type)
	}
	if len(task.Stages) != 6 {
		t.Fatalf("stages length = %d, want 6", len(task.Stages))
	}
	// R→S = F→S − F→R = 200 (p50)
	rs := task.Stages[3]
	if rs.Stage != "R→S" {
		t.Errorf("stage[3] = %q, want R→S", rs.Stage)
	}
	if rs.P50 != 200 {
		t.Errorf("R→S p50 = %v, want 200", rs.P50)
	}
	// S→C = F→C − F→S = 300 (p50)
	sc := task.Stages[5]
	if sc.P50 != 300 {
		t.Errorf("S→C p50 = %v, want 300", sc.P50)
	}
}

func TestBuild_AspirationalOff(t *testing.T) {
	r := &fakeReader{}
	win, _ := ParseWindow("7d", "", "", time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC))
	resp, err := Build(context.Background(), r, Options{Window: win, Aspirational: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Aspirational.LifecycleByType) != 0 {
		t.Errorf("aspirational off: lifecycle should be empty, got %d entries", len(resp.Aspirational.LifecycleByType))
	}
	if len(resp.Aspirational.BugAttachmentWeekly) != 0 {
		t.Errorf("aspirational off: bug attachment should be empty, got %d entries", len(resp.Aspirational.BugAttachmentWeekly))
	}
}

func TestBuild_PanelErrorPropagates(t *testing.T) {
	for _, panel := range []string{"throughput", "lifecycle", "bugAttach", "formulas", "costDaily", "phases", "failures", "models", "tools", "cost", "mttr", "activeAgents"} {
		t.Run(panel, func(t *testing.T) {
			r := &fakeReader{failPanel: panel}
			win, _ := ParseWindow("7d", "", "", time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC))
			_, err := Build(context.Background(), r, Options{Window: win})
			if err == nil {
				t.Fatalf("%s panel: want error, got nil", panel)
			}
		})
	}
}

func TestBuild_JSONShapeMatchesContract(t *testing.T) {
	// Sanity check — encode the zero response and verify key JSON
	// field names are camelCase, matching TS.
	r := &fakeReader{}
	win, _ := ParseWindow("7d", "", "", time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC))
	resp, err := Build(context.Background(), r, Options{Window: win})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wantKeys := []string{
		`"scope":"all"`,
		`"window":"7d"`,
		`"generatedAt":`,
		`"hero":{`,
		`"deployFreqPerWeek":`,
		`"leadTimeSparkline":`,
		`"throughput":{"weekly":`,
		`"lifecycle":{"byType":`,
		`"bugAttachment":{"weekly":`,
		`"costDaily":`,
		`"aspirational":{`,
	}
	for _, k := range wantKeys {
		if !containsBytes(data, k) {
			t.Errorf("JSON missing %q (got %s)", k, string(data))
		}
	}
}

func containsBytes(haystack []byte, needle string) bool {
	n := []byte(needle)
	for i := 0; i+len(n) <= len(haystack); i++ {
		if string(haystack[i:i+len(n)]) == needle {
			return true
		}
	}
	return false
}

func TestWowDelta(t *testing.T) {
	if got := wowDelta(10, 0); got != 0 {
		t.Errorf("wowDelta(10, 0) = %v, want 0", got)
	}
	if got := wowDelta(15, 10); got != 0.5 {
		t.Errorf("wowDelta(15, 10) = %v, want 0.5", got)
	}
	if got := wowDelta(5, 10); got != -0.5 {
		t.Errorf("wowDelta(5, 10) = %v, want -0.5", got)
	}
}
