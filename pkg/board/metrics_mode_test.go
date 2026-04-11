package board

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/awell-health/spire/pkg/olap"
)

// newTestMetricsMode creates a MetricsMode pre-populated with test data
// and a standard 80x24 terminal size.
func newTestMetricsMode() *MetricsMode {
	m := NewMetricsMode()
	m.width = 80
	m.height = 24
	m.dora = &olap.DORAMetrics{
		DeployFrequency:   5.0,
		LeadTimeSeconds:   840,
		ChangeFailureRate: 0.74,
		MTTRSeconds:       1500,
	}
	m.formulas = []olap.FormulaStats{
		{FormulaName: "task-default", TotalRuns: 42, SuccessRate: 92.0, AvgCostUSD: 1.50, AvgReviewRounds: 1.2},
		{FormulaName: "bug-fix", TotalRuns: 10, SuccessRate: 60.0, AvgCostUSD: 2.00, AvgReviewRounds: 2.1},
	}
	m.bugs = []olap.BugCausality{
		{BeadID: "spi-abc", FailureClass: "build_error", AttemptCount: 5, LastFailure: time.Now().Add(-2 * time.Hour)},
		{BeadID: "spi-def", FailureClass: "test_failure", AttemptCount: 2, LastFailure: time.Now().Add(-24 * time.Hour)},
	}
	m.costTrend = []olap.CostTrendPoint{
		{Date: time.Now().AddDate(0, 0, -1), TotalCost: 25.50, RunCount: 12},
		{Date: time.Now().AddDate(0, 0, -2), TotalCost: 8.00, RunCount: 5},
		{Date: time.Now().AddDate(0, 0, -3), TotalCost: 3.00, RunCount: 2},
	}
	m.toolUsage = []olap.ToolUsageStats{
		{FormulaName: "task-default", Phase: "implement", TotalRead: 120, TotalEdit: 40, ReadRatio: 0.75},
	}
	return m
}

func sendKey(m *MetricsMode, key string) {
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
}

// --- Navigation tests ---

func TestMetricsNavigation_HLMovesColumns(t *testing.T) {
	m := newTestMetricsMode()

	// Start at section 0 (top-left).
	if m.focusedSection != 0 {
		t.Fatalf("expected initial section 0, got %d", m.focusedSection)
	}

	// l moves right: 0 → 1.
	sendKey(m, "l")
	if m.focusedSection != 1 {
		t.Fatalf("expected section 1 after l, got %d", m.focusedSection)
	}

	// l at right column stays: 1 → 1.
	sendKey(m, "l")
	if m.focusedSection != 1 {
		t.Fatalf("expected section 1 (no wrap), got %d", m.focusedSection)
	}

	// h moves left: 1 → 0.
	sendKey(m, "h")
	if m.focusedSection != 0 {
		t.Fatalf("expected section 0 after h, got %d", m.focusedSection)
	}

	// h at left column stays: 0 → 0.
	sendKey(m, "h")
	if m.focusedSection != 0 {
		t.Fatalf("expected section 0 (no wrap), got %d", m.focusedSection)
	}
}

func TestMetricsNavigation_HLBottomRow(t *testing.T) {
	m := newTestMetricsMode()
	m.focusedSection = 2 // bottom-left

	sendKey(m, "l")
	if m.focusedSection != 3 {
		t.Fatalf("expected section 3 after l from 2, got %d", m.focusedSection)
	}

	sendKey(m, "l")
	if m.focusedSection != 3 {
		t.Fatalf("expected section 3 (no wrap), got %d", m.focusedSection)
	}

	sendKey(m, "h")
	if m.focusedSection != 2 {
		t.Fatalf("expected section 2 after h from 3, got %d", m.focusedSection)
	}

	sendKey(m, "h")
	if m.focusedSection != 2 {
		t.Fatalf("expected section 2 (no wrap), got %d", m.focusedSection)
	}
}

func TestMetricsNavigation_JKMovesRowsWhenNoOverflow(t *testing.T) {
	m := newTestMetricsMode()
	// With only 2 formulas, content won't overflow the section.

	// j from top row → bottom row.
	sendKey(m, "j")
	if m.focusedSection != 2 {
		t.Fatalf("expected section 2 after j from 0 (no overflow), got %d", m.focusedSection)
	}

	// j from bottom row stays (boundary).
	sendKey(m, "j")
	if m.focusedSection != 2 {
		t.Fatalf("expected section 2 (no wrap), got %d", m.focusedSection)
	}

	// k from bottom row → top row.
	sendKey(m, "k")
	if m.focusedSection != 0 {
		t.Fatalf("expected section 0 after k from 2, got %d", m.focusedSection)
	}

	// k from top row stays (boundary).
	sendKey(m, "k")
	if m.focusedSection != 0 {
		t.Fatalf("expected section 0 (no wrap), got %d", m.focusedSection)
	}
}

func TestMetricsNavigation_JKScrollsWhenOverflow(t *testing.T) {
	m := newTestMetricsMode()

	// Add many formulas to cause overflow.
	m.formulas = make([]olap.FormulaStats, 30)
	for i := range m.formulas {
		m.formulas[i] = olap.FormulaStats{FormulaName: "formula", TotalRuns: i, SuccessRate: 80}
	}

	// Render once to populate sectionLines.
	m.View()

	visibleLines := m.sectionContentHeight()
	totalLines := m.sectionLines[secFormulaPerf]
	if totalLines <= visibleLines {
		t.Fatalf("expected overflow: totalLines=%d, visibleLines=%d", totalLines, visibleLines)
	}

	// j should scroll, not move row.
	sendKey(m, "j")
	if m.focusedSection != 0 {
		t.Fatalf("expected to stay at section 0 while scrolling, got %d", m.focusedSection)
	}
	if m.scrollOffset[0] != 1 {
		t.Fatalf("expected scrollOffset=1 after j, got %d", m.scrollOffset[0])
	}

	// Multiple j's should keep scrolling.
	sendKey(m, "j")
	sendKey(m, "j")
	if m.scrollOffset[0] != 3 {
		t.Fatalf("expected scrollOffset=3 after 3 j's, got %d", m.scrollOffset[0])
	}

	// k should scroll back up.
	sendKey(m, "k")
	if m.scrollOffset[0] != 2 {
		t.Fatalf("expected scrollOffset=2 after k, got %d", m.scrollOffset[0])
	}

	// Scroll all the way up.
	sendKey(m, "k")
	sendKey(m, "k")
	if m.scrollOffset[0] != 0 {
		t.Fatalf("expected scrollOffset=0 at top, got %d", m.scrollOffset[0])
	}

	// One more k at scroll top → should stay at section 0 (no row above).
	sendKey(m, "k")
	if m.focusedSection != 0 {
		t.Fatalf("expected section 0 at scroll boundary, got %d", m.focusedSection)
	}
}

func TestMetricsNavigation_TabCycles(t *testing.T) {
	m := newTestMetricsMode()

	expected := []int{1, 2, 3, 0, 1}
	for i, want := range expected {
		sendKey(m, "tab")
		if m.focusedSection != want {
			t.Fatalf("step %d: expected section %d after tab, got %d", i, want, m.focusedSection)
		}
	}
}

func TestMetricsNavigation_ShiftTabCyclesBackward(t *testing.T) {
	m := newTestMetricsMode()

	expected := []int{3, 2, 1, 0, 3}
	for i, want := range expected {
		m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
		if m.focusedSection != want {
			t.Fatalf("step %d: expected section %d after shift+tab, got %d", i, want, m.focusedSection)
		}
	}
}

func TestMetricsNavigation_JKScrollBoundsClamped(t *testing.T) {
	m := newTestMetricsMode()

	// Create overflow content.
	m.formulas = make([]olap.FormulaStats, 30)
	for i := range m.formulas {
		m.formulas[i] = olap.FormulaStats{FormulaName: "f", TotalRuns: i, SuccessRate: 50}
	}
	m.View()

	visibleLines := m.sectionContentHeight()
	totalLines := m.sectionLines[secFormulaPerf]
	maxOffset := totalLines - visibleLines

	// Scroll to the very bottom.
	for i := 0; i < maxOffset+5; i++ {
		sendKey(m, "j")
	}

	if m.scrollOffset[0] > maxOffset {
		t.Fatalf("scrollOffset %d exceeded max %d", m.scrollOffset[0], maxOffset)
	}

	// After reaching max scroll, next j should move to bottom row.
	if m.focusedSection != 2 {
		t.Fatalf("expected to move to section 2 after scrolling past end, got %d", m.focusedSection)
	}
}

func TestMetricsNavigation_DataRefreshResetsScroll(t *testing.T) {
	m := newTestMetricsMode()
	m.scrollOffset = [4]int{5, 3, 2, 1}

	m.Update(metricsDataMsg{
		dora:    m.dora,
		formulas: m.formulas,
	})

	for i, off := range m.scrollOffset {
		if off != 0 {
			t.Fatalf("expected scrollOffset[%d]=0 after data refresh, got %d", i, off)
		}
	}
}

// --- sectionContentHeight tests ---

func TestSectionContentHeight_EvenHeight(t *testing.T) {
	m := newTestMetricsMode()
	m.height = 24 // gridHeight = 22, topRow = 11, bottomRow = 11

	m.focusedSection = 0
	topH := m.sectionContentHeight()

	m.focusedSection = 2
	bottomH := m.sectionContentHeight()

	if topH != bottomH {
		t.Fatalf("even height: expected same content height, got top=%d bottom=%d", topH, bottomH)
	}
	if topH != 9 { // 11 - 2
		t.Fatalf("expected content height 9, got %d", topH)
	}
}

func TestSectionContentHeight_OddHeight(t *testing.T) {
	m := newTestMetricsMode()
	m.height = 25 // gridHeight = 23, topRow = 11, bottomRow = 12

	m.focusedSection = 0
	topH := m.sectionContentHeight()

	m.focusedSection = 2
	bottomH := m.sectionContentHeight()

	if topH != 9 { // 11 - 2
		t.Fatalf("expected top content height 9, got %d", topH)
	}
	if bottomH != 10 { // 12 - 2
		t.Fatalf("expected bottom content height 10, got %d", bottomH)
	}
}

func TestSectionContentHeight_TinyTerminal(t *testing.T) {
	m := newTestMetricsMode()
	m.height = 5 // gridHeight = 3, below minimum of 4

	m.focusedSection = 0
	h := m.sectionContentHeight()
	if h != 0 { // 2 - 2
		t.Fatalf("expected content height 0 for tiny terminal, got %d", h)
	}
}

// --- Render function tests ---

func TestRenderDORAHeader_NilData(t *testing.T) {
	m := newTestMetricsMode()
	m.dora = nil

	header := m.renderDORAHeader()
	if !strings.Contains(header, "DORA") {
		t.Fatal("expected DORA label in header")
	}
	if !strings.Contains(header, "no data") {
		t.Fatal("expected 'no data' when dora is nil")
	}
}

func TestRenderDORAHeader_WithData(t *testing.T) {
	m := newTestMetricsMode()

	header := m.renderDORAHeader()
	if !strings.Contains(header, "DORA") {
		t.Fatal("expected DORA label in header")
	}
	if !strings.Contains(header, "5.0/wk") {
		t.Fatal("expected deploy frequency in header")
	}
	if !strings.Contains(header, "74%") {
		t.Fatal("expected failure rate in header")
	}
}

func TestRenderDORAHeader_FailureRateColors(t *testing.T) {
	m := newTestMetricsMode()

	// Test low failure rate (should be green = color "2").
	m.dora.ChangeFailureRate = 0.05
	header := m.renderDORAHeader()
	if !strings.Contains(header, "5%") {
		t.Fatal("expected 5% in header")
	}

	// Test medium failure rate (should be yellow = color "3").
	m.dora.ChangeFailureRate = 0.15
	header = m.renderDORAHeader()
	if !strings.Contains(header, "15%") {
		t.Fatal("expected 15% in header")
	}

	// Test high failure rate (should be red = color "1").
	m.dora.ChangeFailureRate = 0.50
	header = m.renderDORAHeader()
	if !strings.Contains(header, "50%") {
		t.Fatal("expected 50% in header")
	}
}

func TestRenderSection_FocusIndicator(t *testing.T) {
	m := newTestMetricsMode()
	m.focusedSection = 0

	focused := m.renderSection("Test", []string{"line1"}, 40, 10, 0)
	unfocused := m.renderSection("Test", []string{"line1"}, 40, 10, 1)

	if !strings.Contains(focused, "▶") {
		t.Fatal("focused section should have ▶ indicator")
	}
	if strings.Contains(unfocused, "▶") {
		t.Fatal("unfocused section should not have ▶ indicator")
	}
}

func TestRenderSection_ScrollOffset(t *testing.T) {
	m := newTestMetricsMode()
	m.focusedSection = 0

	lines := []string{"line0", "line1", "line2", "line3", "line4"}
	m.scrollOffset[0] = 2

	rendered := m.renderSection("Test", lines, 40, 5, 0)
	// With h=5, contentH = 5-2 = 3. Offset 2 means we see line2, line3, line4.
	if !strings.Contains(rendered, "line2") {
		t.Fatal("expected line2 visible after scroll offset 2")
	}
	if strings.Contains(rendered, "line0") {
		t.Fatal("line0 should be scrolled off")
	}
}

func TestRenderSection_EmptyContent(t *testing.T) {
	m := newTestMetricsMode()
	rendered := m.renderSection("Empty", []string{}, 40, 10, 0)
	if !strings.Contains(rendered, "Empty") {
		t.Fatal("section should still show title with empty content")
	}
}

func TestRenderFormulaContent_Empty(t *testing.T) {
	m := newTestMetricsMode()
	m.formulas = nil

	lines := m.renderFormulaContent()
	if len(lines) != 1 {
		t.Fatalf("expected 1 line for empty data, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "No formula data") {
		t.Fatal("expected 'No formula data' message")
	}
}

func TestRenderFormulaContent_SuccessRateColors(t *testing.T) {
	m := newTestMetricsMode()
	m.formulas = []olap.FormulaStats{
		{FormulaName: "green", TotalRuns: 10, SuccessRate: 95.0, AvgCostUSD: 1.0, AvgReviewRounds: 1.0},
		{FormulaName: "yellow", TotalRuns: 10, SuccessRate: 75.0, AvgCostUSD: 1.0, AvgReviewRounds: 1.0},
		{FormulaName: "red", TotalRuns: 10, SuccessRate: 50.0, AvgCostUSD: 1.0, AvgReviewRounds: 1.0},
	}

	lines := m.renderFormulaContent()
	// Header + 3 data rows.
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines, got %d", len(lines))
	}
	// Verify all formulas appear.
	combined := strings.Join(lines, "\n")
	for _, name := range []string{"green", "yellow", "red"} {
		if !strings.Contains(combined, name) {
			t.Fatalf("expected formula %q in output", name)
		}
	}
}

func TestRenderCostTrendContent_Empty(t *testing.T) {
	m := newTestMetricsMode()
	m.costTrend = nil

	lines := m.renderCostTrendContent()
	if len(lines) != 1 || !strings.Contains(lines[0], "No cost data") {
		t.Fatal("expected 'No cost data' for empty trend")
	}
}

func TestRenderCostTrendContent_LimitsTo10(t *testing.T) {
	m := newTestMetricsMode()
	m.costTrend = make([]olap.CostTrendPoint, 20)
	for i := range m.costTrend {
		m.costTrend[i] = olap.CostTrendPoint{Date: time.Now().AddDate(0, 0, -i), TotalCost: 5.0, RunCount: 3}
	}

	lines := m.renderCostTrendContent()
	// 1 header + 10 data rows (capped at 10).
	if len(lines) != 11 {
		t.Fatalf("expected 11 lines (header + 10 data), got %d", len(lines))
	}
}

func TestRenderCostTrendContent_CostColors(t *testing.T) {
	m := newTestMetricsMode()
	m.costTrend = []olap.CostTrendPoint{
		{Date: time.Now(), TotalCost: 25.0, RunCount: 1},  // red (>20)
		{Date: time.Now(), TotalCost: 10.0, RunCount: 1},  // yellow (>5)
		{Date: time.Now(), TotalCost: 2.0, RunCount: 1},   // default (<=5)
	}

	lines := m.renderCostTrendContent()
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines, got %d", len(lines))
	}
}

func TestRenderBugContent_Empty(t *testing.T) {
	m := newTestMetricsMode()
	m.bugs = nil

	lines := m.renderBugContent()
	if len(lines) != 1 || !strings.Contains(lines[0], "No failure hotspots") {
		t.Fatal("expected 'No failure hotspots' for empty data")
	}
}

func TestRenderBugContent_AttemptCountColors(t *testing.T) {
	m := newTestMetricsMode()
	m.bugs = []olap.BugCausality{
		{BeadID: "spi-a", FailureClass: "build", AttemptCount: 6, LastFailure: time.Now()},  // red (>=5)
		{BeadID: "spi-b", FailureClass: "test", AttemptCount: 3, LastFailure: time.Now()},   // yellow (>=3)
		{BeadID: "spi-c", FailureClass: "lint", AttemptCount: 1, LastFailure: time.Now()},    // default (<3)
	}

	lines := m.renderBugContent()
	if len(lines) != 4 { // header + 3
		t.Fatalf("expected 4 lines, got %d", len(lines))
	}
	combined := strings.Join(lines, "\n")
	for _, id := range []string{"spi-a", "spi-b", "spi-c"} {
		if !strings.Contains(combined, id) {
			t.Fatalf("expected bead %q in output", id)
		}
	}
}

func TestRenderToolUsageContent_Empty(t *testing.T) {
	m := newTestMetricsMode()
	m.toolUsage = nil

	lines := m.renderToolUsageContent()
	if len(lines) != 1 || !strings.Contains(lines[0], "No tool usage data") {
		t.Fatal("expected 'No tool usage data' for empty data")
	}
}

func TestRenderToolUsageContent_WithData(t *testing.T) {
	m := newTestMetricsMode()
	m.toolUsage = []olap.ToolUsageStats{
		{FormulaName: "task-default", Phase: "implement", TotalRead: 100, TotalEdit: 50, TotalTools: 150, ReadRatio: 0.67},
		{FormulaName: "bug-fix", Phase: "review", TotalRead: 30, TotalEdit: 10, TotalTools: 40, ReadRatio: 0.75},
	}

	lines := m.renderToolUsageContent()
	if len(lines) != 3 { // header + 2
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
	combined := strings.Join(lines, "\n")
	if !strings.Contains(combined, "task-default") {
		t.Fatal("expected task-default in output")
	}
	if !strings.Contains(combined, "67%") {
		t.Fatal("expected 67% read ratio in output")
	}
	if !strings.Contains(combined, "Total") {
		t.Fatal("expected 'Total' column header in output")
	}
	if !strings.Contains(combined, "150") {
		t.Fatal("expected total calls count 150 in output")
	}
}

func TestRenderToolUsageContent_BranchSelection(t *testing.T) {
	tests := []struct {
		name            string
		toolEvents      []olap.ToolEventStats
		toolUsage       []olap.ToolUsageStats
		wantContains    []string
		wantNotContains []string
		wantLineCount   int
	}{
		{
			name: "otel_path_when_toolEvents_populated",
			toolEvents: []olap.ToolEventStats{
				{ToolName: "FileRead", Count: 50, AvgDurationMs: 120.5, FailureCount: 0},
				{ToolName: "FileEdit", Count: 30, AvgDurationMs: 200.0, FailureCount: 2},
			},
			// Legacy data present but should be ignored when toolEvents exist.
			toolUsage: []olap.ToolUsageStats{
				{FormulaName: "task-default", Phase: "implement", TotalRead: 100, TotalEdit: 50, TotalTools: 150, ReadRatio: 0.67},
			},
			wantContains:    []string{"Tool", "Calls", "Avg ms", "Fails", "FileRead", "FileEdit"},
			wantNotContains: []string{"Formula", "Phase", "No tool usage data"},
			wantLineCount:   3, // header + 2 tools
		},
		{
			name:       "legacy_fallback_when_toolEvents_empty",
			toolEvents: nil,
			toolUsage: []olap.ToolUsageStats{
				{FormulaName: "task-default", Phase: "implement", TotalRead: 100, TotalEdit: 50, TotalTools: 150, ReadRatio: 0.67},
			},
			wantContains:    []string{"Formula", "Phase", "task-default", "67%"},
			wantNotContains: []string{"Avg ms", "Fails", "No tool usage data"},
			wantLineCount:   2, // header + 1 row
		},
		{
			name:            "no_data_when_both_empty",
			toolEvents:      nil,
			toolUsage:       nil,
			wantContains:    []string{"No tool usage data"},
			wantNotContains: []string{"Formula", "Calls"},
			wantLineCount:   1,
		},
		{
			name: "failure_count_gt_zero_rendered",
			toolEvents: []olap.ToolEventStats{
				{ToolName: "Bash", Count: 10, AvgDurationMs: 50.0, FailureCount: 3},
				{ToolName: "Grep", Count: 20, AvgDurationMs: 30.0, FailureCount: 0},
			},
			wantContains:  []string{"Bash", "Grep", "3"},
			wantLineCount: 3, // header + 2 tools
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewMetricsMode()
			m.width = 80
			m.height = 24
			m.toolEvents = tt.toolEvents
			m.toolUsage = tt.toolUsage

			lines := m.renderToolUsageContent()
			if len(lines) != tt.wantLineCount {
				t.Fatalf("expected %d lines, got %d", tt.wantLineCount, len(lines))
			}

			combined := strings.Join(lines, "\n")
			for _, want := range tt.wantContains {
				if !strings.Contains(combined, want) {
					t.Errorf("expected output to contain %q", want)
				}
			}
			for _, notWant := range tt.wantNotContains {
				if strings.Contains(combined, notWant) {
					t.Errorf("expected output NOT to contain %q", notWant)
				}
			}
		})
	}
}

func TestRenderToolUsageContent_OTelSortOrder(t *testing.T) {
	m := NewMetricsMode()
	m.width = 80
	m.height = 24
	m.toolEvents = []olap.ToolEventStats{
		{ToolName: "Edit", Count: 30, AvgDurationMs: 200.0, FailureCount: 0},
		{ToolName: "Read", Count: 50, AvgDurationMs: 120.5, FailureCount: 0},
		{ToolName: "Bash", Count: 50, AvgDurationMs: 80.0, FailureCount: 1},
	}

	lines := m.renderToolUsageContent()
	combined := strings.Join(lines, "\n")

	// Sorted by count DESC then name ASC: Bash(50) < Read(50) < Edit(30).
	bashIdx := strings.Index(combined, "Bash")
	readIdx := strings.Index(combined, "Read")
	editIdx := strings.Index(combined, "Edit")

	if bashIdx < 0 || readIdx < 0 || editIdx < 0 {
		t.Fatal("expected all three tool names in output")
	}
	if bashIdx > readIdx || readIdx > editIdx {
		t.Errorf("expected Bash before Read before Edit, got positions Bash=%d Read=%d Edit=%d",
			bashIdx, readIdx, editIdx)
	}
}

func TestRenderToolUsageContent_FailureCountRedStyling(t *testing.T) {
	m := NewMetricsMode()
	m.width = 80
	m.height = 24
	m.toolEvents = []olap.ToolEventStats{
		{ToolName: "Bash", Count: 10, AvgDurationMs: 50.0, FailureCount: 3},
		{ToolName: "Grep", Count: 20, AvgDurationMs: 30.0, FailureCount: 0},
	}

	lines := m.renderToolUsageContent()
	// The failure-count cell for Bash (FailureCount=3) goes through redStyle.Render,
	// producing a longer string than the plain Sprintf path used for Grep (FailureCount=0).
	// Both data lines have the same column structure except the styled failure cell.
	var bashLine, grepLine string
	for _, l := range lines {
		if strings.Contains(l, "Bash") {
			bashLine = l
		}
		if strings.Contains(l, "Grep") {
			grepLine = l
		}
	}
	if bashLine == "" || grepLine == "" {
		t.Fatal("expected both Bash and Grep lines in output")
	}
	// Styled text (via lipgloss) is longer in bytes than plain text due to escape sequences.
	if len(bashLine) <= len(grepLine) {
		t.Errorf("expected Bash line (failure styled) to be longer than Grep line; Bash=%d Grep=%d",
			len(bashLine), len(grepLine))
	}
}

// --- View integration tests ---

func TestView_LoadingState(t *testing.T) {
	m := NewMetricsMode()
	m.width = 80
	m.height = 24
	m.loading = true

	view := m.View()
	if !strings.Contains(view, "Loading") {
		t.Fatal("expected loading message")
	}
}

func TestView_ErrorStateNoData(t *testing.T) {
	m := NewMetricsMode()
	m.width = 80
	m.height = 24
	m.lastErr = fmt.Errorf("connection failed")

	view := m.View()
	if !strings.Contains(view, "connection failed") {
		t.Fatal("expected error message in view")
	}
}

func TestView_TinyTerminal(t *testing.T) {
	m := newTestMetricsMode()
	m.height = 5 // gridHeight = 3, below threshold of 4

	view := m.View()
	if !strings.Contains(view, "DORA") {
		t.Fatal("expected DORA header even in tiny terminal")
	}
	if !strings.Contains(view, "too small") {
		t.Fatal("expected 'too small' message for tiny terminal")
	}
}

func TestView_NormalRenderHasAllSections(t *testing.T) {
	m := newTestMetricsMode()

	view := m.View()
	for _, section := range []string{"DORA", "Formula Performance", "Cost Trend", "Failure Hotspots", "Tool Usage"} {
		if !strings.Contains(view, section) {
			t.Fatalf("expected %q in view", section)
		}
	}
}

func TestFooterHints(t *testing.T) {
	m := newTestMetricsMode()
	hints := m.FooterHints()

	for _, hint := range []string{"h/l=column", "j/k=scroll", "tab=cycle", "r=refresh"} {
		if !strings.Contains(hints, hint) {
			t.Fatalf("expected %q in footer hints", hint)
		}
	}
}

func TestFormatTokens(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "—"},
		{500, "500"},
		{1500, "2K"},
		{340000, "340K"},
		{1200000, "1.2M"},
		{5500000, "5.5M"},
	}
	for _, tt := range tests {
		got := formatTokens(tt.n)
		if got != tt.want {
			t.Errorf("formatTokens(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestRenderCostTrendContent_TokensDisplayed(t *testing.T) {
	m := newTestMetricsMode()
	m.costTrend = []olap.CostTrendPoint{
		{Date: time.Now(), TotalCost: 10.0, RunCount: 5, PromptTokens: 500000, CompletionTokens: 150000},
		{Date: time.Now().AddDate(0, 0, -1), TotalCost: 3.0, RunCount: 2, PromptTokens: 0, CompletionTokens: 0},
	}

	lines := m.renderCostTrendContent()
	combined := strings.Join(lines, "\n")
	if !strings.Contains(combined, "Tokens") {
		t.Fatal("expected 'Tokens' column header in output")
	}
	if !strings.Contains(combined, "650K") {
		t.Fatal("expected 650K token count in output")
	}
	if !strings.Contains(combined, "—") {
		t.Fatal("expected dash for zero tokens")
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		seconds float64
		want    string
	}{
		{0, "0s"},
		{-5, "0s"},
		{30, "30s"},
		{120, "2m"},
		{3661, "1h 1m"},
		{90000, "1d 1h"},
	}
	for _, tt := range tests {
		got := formatDuration(tt.seconds)
		if got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.seconds, got, tt.want)
		}
	}
}
