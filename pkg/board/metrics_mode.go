package board

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/olap"
)

// Compile-time check: MetricsMode implements Mode.
var _ Mode = (*MetricsMode)(nil)

// metricsDataMsg carries fetched metrics data back to the Update loop.
type metricsDataMsg struct {
	dora            *olap.DORAMetrics
	formulas        []olap.FormulaStats
	bugs            []olap.BugCausality
	costTrend       []olap.CostTrendPoint
	toolUsage       []olap.ToolUsageStats
	toolEvents      []olap.ToolEventStats
	lifecycleByType []olap.LifecycleByType
	err             error
}

// metricsTickMsg triggers a periodic refresh.
type metricsTickMsg time.Time

// Section indices for the 2x2 grid.
const (
	secFormulaPerf = 0 // top-left
	secCostTrend   = 1 // top-right
	secBugHotspots = 2 // bottom-left
	secToolUsage   = 3 // bottom-right
)

// MetricsMode renders live metrics from DuckDB in the Board TUI.
type MetricsMode struct {
	width, height int
	towerName     string
	db            *olap.DB

	// Cached data (populated by async fetch).
	dora            *olap.DORAMetrics
	formulas        []olap.FormulaStats
	bugs            []olap.BugCausality
	costTrend       []olap.CostTrendPoint
	toolUsage       []olap.ToolUsageStats
	toolEvents      []olap.ToolEventStats
	lifecycleByType []olap.LifecycleByType

	loading     bool
	lastErr     error
	lastRefresh time.Time

	// Grid navigation state.
	focusedSection int    // 0–3 index into the 2x2 grid
	scrollOffset   [4]int // per-section vertical scroll offset
	sectionLines   [4]int // per-section total content lines (set during render)

	// showLifecyclePanel toggles the bottom-right section between the
	// Tool Usage view (default) and the Lifecycle P50/P95 view. Bound to 't'.
	showLifecyclePanel bool
}

// NewMetricsMode creates a new MetricsMode.
func NewMetricsMode() *MetricsMode {
	return &MetricsMode{}
}

func (m *MetricsMode) ID() ModeID      { return ModeMetrics }
func (m *MetricsMode) HasOverlay() bool { return false }

func (m *MetricsMode) Init() tea.Cmd { return nil }

func (m *MetricsMode) SetSize(w, h int) { m.width, m.height = w, h }

// OnActivate opens the DuckDB connection and starts fetching metrics.
func (m *MetricsMode) OnActivate() tea.Cmd {
	tc, err := config.ActiveTowerConfig()
	if err != nil {
		m.lastErr = fmt.Errorf("no active tower: %w", err)
		return nil
	}
	m.towerName = tc.Name
	olapPath := tc.OLAPPath()
	db, err := olap.Open(olapPath)
	if err != nil {
		m.lastErr = fmt.Errorf("open OLAP db: %w", err)
		return nil
	}
	m.db = db
	m.loading = true
	m.lastErr = nil
	return m.fetchMetrics()
}

// OnDeactivate closes the DuckDB connection.
func (m *MetricsMode) OnDeactivate() {
	if m.db != nil {
		m.db.Close()
		m.db = nil
	}
}

// HandleTowerChanged closes the existing DB, opens one for the new tower, and refetches.
func (m *MetricsMode) HandleTowerChanged(tc TowerChanged) tea.Cmd {
	if m.db != nil {
		m.db.Close()
		m.db = nil
	}
	m.towerName = tc.Name

	towerCfg, err := config.LoadTowerConfig(tc.Name)
	if err != nil {
		m.lastErr = fmt.Errorf("tower config: %w", err)
		return nil
	}
	db, err := olap.Open(towerCfg.OLAPPath())
	if err != nil {
		m.lastErr = fmt.Errorf("open OLAP db: %w", err)
		return nil
	}
	m.db = db
	m.loading = true
	m.lastErr = nil
	return m.fetchMetrics()
}

// Update handles messages for the metrics mode.
func (m *MetricsMode) Update(msg tea.Msg) (Mode, tea.Cmd) {
	switch msg := msg.(type) {
	case metricsDataMsg:
		m.loading = false
		if msg.err != nil {
			m.lastErr = msg.err
		} else {
			m.lastErr = nil
			m.dora = msg.dora
			m.formulas = msg.formulas
			m.bugs = msg.bugs
			m.costTrend = msg.costTrend
			m.toolUsage = msg.toolUsage
			m.toolEvents = msg.toolEvents
			m.lifecycleByType = msg.lifecycleByType
			m.lastRefresh = time.Now()
			// Reset scroll offsets on data refresh to avoid stale positions.
			m.scrollOffset = [4]int{}
		}
		return m, scheduleMetricsTick()

	case metricsTickMsg:
		if m.db != nil {
			return m, m.fetchMetrics()
		}
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "r":
			if m.db != nil {
				m.loading = true
				return m, m.fetchMetrics()
			}
			return m, m.OnActivate()

		case "h", "left":
			// Move focus to left column (0←1, 2←3).
			if m.focusedSection%2 == 1 {
				m.focusedSection--
			}

		case "l", "right":
			// Move focus to right column (0→1, 2→3).
			if m.focusedSection%2 == 0 {
				m.focusedSection++
			}

		case "j", "down":
			visibleLines := m.sectionContentHeight()
			totalLines := m.sectionLines[m.focusedSection]
			if totalLines > visibleLines && m.scrollOffset[m.focusedSection] < totalLines-visibleLines {
				// Scroll down within focused section.
				m.scrollOffset[m.focusedSection]++
			} else if m.focusedSection < 2 {
				// At scroll bottom or no overflow — move focus to row below.
				m.focusedSection += 2
			}

		case "k", "up":
			if m.scrollOffset[m.focusedSection] > 0 {
				// Scroll up within focused section.
				m.scrollOffset[m.focusedSection]--
			} else if m.focusedSection >= 2 {
				// At scroll top — move focus to row above.
				m.focusedSection -= 2
			}

		case "J", "shift+j", "shift+down":
			visibleLines := m.sectionContentHeight()
			totalLines := m.sectionLines[m.focusedSection]
			maxOff := totalLines - visibleLines
			if maxOff < 0 {
				maxOff = 0
			}
			m.scrollOffset[m.focusedSection] += 5
			if m.scrollOffset[m.focusedSection] > maxOff {
				m.scrollOffset[m.focusedSection] = maxOff
			}

		case "K", "shift+k", "shift+up":
			m.scrollOffset[m.focusedSection] -= 5
			if m.scrollOffset[m.focusedSection] < 0 {
				m.scrollOffset[m.focusedSection] = 0
			}

		case "1":
			m.focusedSection = secFormulaPerf
		case "2":
			m.focusedSection = secCostTrend
		case "3":
			m.focusedSection = secBugHotspots
		case "4":
			m.focusedSection = secToolUsage

		case "t":
			// Toggle the bottom-right panel between Tool Usage (default) and
			// Lifecycle P50/P95. Reset the panel's scroll offset so the new
			// content renders from the top.
			m.showLifecyclePanel = !m.showLifecyclePanel
			m.scrollOffset[secToolUsage] = 0
		}
		return m, nil
	}
	return m, nil
}

// sectionContentHeight returns the number of visible content lines for the
// given section index (excluding the title line and separator). Bottom-row
// sections may be taller than top-row when gridHeight is odd.
func (m *MetricsMode) sectionContentHeight() int {
	return m.sectionRowHeight(m.focusedSection) - 2
}

// sectionRowHeight returns the row height for the given section index.
func (m *MetricsMode) sectionRowHeight(sectionIdx int) int {
	headerHeight := 2 // DORA header line + separator
	gridHeight := m.height - headerHeight
	if gridHeight < 4 {
		return 2 // minimum: title + separator
	}
	topRowHeight := gridHeight / 2
	if sectionIdx < 2 {
		return topRowHeight
	}
	return gridHeight - topRowHeight
}

// fetchMetrics returns a tea.Cmd that queries all metrics and returns a metricsDataMsg.
func (m *MetricsMode) fetchMetrics() tea.Cmd {
	db := m.db
	return func() tea.Msg {
		if db == nil {
			return metricsDataMsg{err: fmt.Errorf("no db connection")}
		}
		since := time.Now().AddDate(0, -3, 0)
		dora, _ := db.QueryDORA(since)
		formulas, _ := db.QueryFormulaPerformance(since)
		bugs, _ := db.QueryBugCausality(5)
		costTrend, _ := db.QueryCostTrend(30)
		toolUsage, _ := db.QueryToolUsage(since)
		toolEvents, _ := db.QueryToolEvents(since)
		lifecycleByType, _ := db.QueryLifecycleByType(since)
		return metricsDataMsg{
			dora:            dora,
			formulas:        formulas,
			bugs:            bugs,
			costTrend:       costTrend,
			toolUsage:       toolUsage,
			toolEvents:      toolEvents,
			lifecycleByType: lifecycleByType,
		}
	}
}

// scheduleMetricsTick returns a tea.Cmd that fires a metricsTickMsg after 30 seconds.
func scheduleMetricsTick() tea.Cmd {
	return tea.Tick(30*time.Second, func(t time.Time) tea.Msg {
		return metricsTickMsg(t)
	})
}

// FooterHints returns keybinding hints for the metrics mode.
func (m *MetricsMode) FooterHints() string {
	parts := []string{"h/l=column", "j/k=scroll", "1-4=sections", "t=tool/lifecycle"}
	if !m.lastRefresh.IsZero() {
		parts = append(parts, fmt.Sprintf("updated %s", relativeTime(m.lastRefresh)))
	}
	parts = append(parts, "r=refresh")
	return strings.Join(parts, "  ")
}

// View renders the metrics dashboard as a DORA header + 2x2 grid.
func (m *MetricsMode) View() string {
	// Loading state.
	if m.loading && m.dora == nil {
		return m.centeredMessage("Loading metrics...")
	}

	// Error state with no data.
	if m.lastErr != nil && m.dora == nil {
		return m.centeredMessage(fmt.Sprintf("Error: %v\n\nPress r to retry", m.lastErr))
	}

	// Compute layout dimensions.
	headerHeight := 2 // DORA line + separator
	gridHeight := m.height - headerHeight
	if gridHeight < 4 {
		// Terminal too small for grid — show DORA header only.
		return m.renderDORAHeader() + "\n" + lipgloss.NewStyle().
			Foreground(lipgloss.Color("8")).Render("  Terminal too small for grid view")
	}

	var out strings.Builder

	// DORA header — always visible at top.
	out.WriteString(m.renderDORAHeader())
	out.WriteString("\n")

	// Grid dimensions.
	topRowHeight := gridHeight / 2
	bottomRowHeight := gridHeight - topRowHeight
	leftColWidth := m.width / 2
	rightColWidth := m.width - leftColWidth

	// Render the four sections, recording content line counts. The bottom-right
	// panel toggles between Tool Usage (default) and Lifecycle P50/P95 via 't'.
	formulaContent := m.renderFormulaContent()
	costContent := m.renderCostTrendContent()
	bugContent := m.renderBugContent()
	var bottomRightContent []string
	var bottomRightTitle string
	if m.showLifecyclePanel {
		bottomRightContent = m.renderLifecycleContent()
		bottomRightTitle = "Lifecycle P50/P95"
	} else {
		bottomRightContent = m.renderToolUsageContent()
		bottomRightTitle = "Tool Usage"
	}

	m.sectionLines[secFormulaPerf] = len(formulaContent)
	m.sectionLines[secCostTrend] = len(costContent)
	m.sectionLines[secBugHotspots] = len(bugContent)
	m.sectionLines[secToolUsage] = len(bottomRightContent)

	topLeft := m.renderSection("Formula Performance", formulaContent, leftColWidth, topRowHeight, secFormulaPerf)
	topRight := m.renderSection("Cost Trend (30d)", costContent, rightColWidth, topRowHeight, secCostTrend)
	bottomLeft := m.renderSection("Failure Hotspots", bugContent, leftColWidth, bottomRowHeight, secBugHotspots)
	bottomRight := m.renderSection(bottomRightTitle, bottomRightContent, rightColWidth, bottomRowHeight, secToolUsage)

	topRow := lipgloss.JoinHorizontal(lipgloss.Top, topLeft, topRight)
	bottomRow := lipgloss.JoinHorizontal(lipgloss.Top, bottomLeft, bottomRight)
	out.WriteString(topRow)
	out.WriteString("\n")
	out.WriteString(bottomRow)

	// Error banner (shown with stale data).
	if m.lastErr != nil {
		warnStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))
		out.WriteString("\n")
		out.WriteString(warnStyle.Render(fmt.Sprintf(" Error: %v — press r to retry", m.lastErr)))
	}

	return out.String()
}

// renderDORAHeader renders a single-line color-coded DORA summary.
func (m *MetricsMode) renderDORAHeader() string {
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	if m.dora == nil {
		return lipgloss.NewStyle().Bold(true).Width(m.width).Render(
			" DORA: " + dimStyle.Render("no data"))
	}

	greenStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	yellowStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	redStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("1"))

	dfStr := fmt.Sprintf("%.1f/wk", m.dora.DeployFrequency)
	ltStr := formatDuration(m.dora.LeadTimeSeconds)
	frStr := fmt.Sprintf("%.0f%%", m.dora.ChangeFailureRate*100)
	mttrStr := formatDuration(m.dora.MTTRSeconds)

	// Color failure rate: green <10%, yellow 10-25%, red >25%.
	frStyled := greenStyle.Render(frStr)
	if m.dora.ChangeFailureRate > 0.25 {
		frStyled = redStyle.Render(frStr)
	} else if m.dora.ChangeFailureRate > 0.10 {
		frStyled = yellowStyle.Render(frStr)
	}

	header := fmt.Sprintf(" DORA: %s  Lead: %s  Fail: %s  MTTR: %s",
		greenStyle.Render(dfStr), ltStr, frStyled, mttrStr)

	headerStyle := lipgloss.NewStyle().Bold(true).Width(m.width).
		BorderStyle(lipgloss.NormalBorder()).
		BorderBottom(true).
		BorderForeground(lipgloss.Color("8"))
	return headerStyle.Render(header)
}

// renderSection renders a titled, height-capped section box with scroll support.
func (m *MetricsMode) renderSection(title string, contentLines []string, w, h int, sectionIdx int) string {
	focused := m.focusedSection == sectionIdx
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	// Title styling — focused sections get highlighted.
	var titleLine string
	if focused {
		titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
		titleLine = titleStyle.Render("▶ " + title)
	} else {
		titleStyle := lipgloss.NewStyle().Bold(true)
		titleLine = titleStyle.Render("  " + title)
	}

	// Separator under title.
	sepWidth := w - 2
	if sepWidth < 1 {
		sepWidth = 1
	}
	sep := dimStyle.Render("  " + strings.Repeat("─", sepWidth))

	// Available content lines (height minus title and separator).
	contentH := h - 2
	if contentH < 1 {
		contentH = 1
	}

	// Apply scroll offset.
	offset := m.scrollOffset[sectionIdx]
	if offset > len(contentLines)-contentH {
		offset = len(contentLines) - contentH
	}
	if offset < 0 {
		offset = 0
	}
	m.scrollOffset[sectionIdx] = offset

	// Slice visible content.
	visible := contentLines
	if len(visible) > offset {
		visible = visible[offset:]
	}
	if len(visible) > contentH {
		visible = visible[:contentH]
	}

	// Pad to fill height if needed.
	for len(visible) < contentH {
		visible = append(visible, "")
	}

	var out strings.Builder
	out.WriteString(titleLine + "\n")
	out.WriteString(sep + "\n")
	for i, line := range visible {
		out.WriteString(line)
		if i < len(visible)-1 {
			out.WriteString("\n")
		}
	}

	return lipgloss.NewStyle().Width(w).Height(h).Render(out.String())
}

// renderFormulaContent returns lines for the Formula Performance section.
func (m *MetricsMode) renderFormulaContent() []string {
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	greenStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	yellowStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	redStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("1"))

	if len(m.formulas) == 0 {
		return []string{dimStyle.Render("  No formula data")}
	}

	lines := []string{
		dimStyle.Render(fmt.Sprintf("  %-16s %5s %7s %8s %6s", "Formula", "Runs", "Success", "Cost", "Revs")),
	}
	for _, f := range m.formulas {
		successStr := fmt.Sprintf("%.0f%%", f.SuccessRate)
		costStr := fmt.Sprintf("$%.2f", f.AvgCostUSD)
		reviewStr := fmt.Sprintf("%.1f", f.AvgReviewRounds)

		var successStyled string
		if f.SuccessRate >= 90 {
			successStyled = greenStyle.Render(fmt.Sprintf("%7s", successStr))
		} else if f.SuccessRate >= 70 {
			successStyled = yellowStyle.Render(fmt.Sprintf("%7s", successStr))
		} else {
			successStyled = redStyle.Render(fmt.Sprintf("%7s", successStr))
		}

		lines = append(lines, fmt.Sprintf("  %-16s %5d %s %8s %6s",
			Truncate(f.FormulaName, 16), f.TotalRuns, successStyled, costStr, reviewStr))
	}
	return lines
}

// renderCostTrendContent returns lines for the Cost Trend section.
func (m *MetricsMode) renderCostTrendContent() []string {
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	yellowStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	redStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("1"))

	if len(m.costTrend) == 0 {
		return []string{dimStyle.Render("  No cost data")}
	}

	lines := []string{
		dimStyle.Render(fmt.Sprintf("  %-10s %8s %6s %5s", "Date", "Cost", "Tokens", "Runs")),
	}
	limit := len(m.costTrend)
	if limit > 10 {
		limit = 10
	}
	for _, c := range m.costTrend[:limit] {
		dateStr := c.Date.Format("Jan 02")
		costStr := fmt.Sprintf("$%.2f", c.TotalCost)
		tokStr := formatTokens(c.PromptTokens + c.CompletionTokens)

		var costStyled string
		if c.TotalCost > 20 {
			costStyled = redStyle.Render(fmt.Sprintf("%8s", costStr))
		} else if c.TotalCost > 5 {
			costStyled = yellowStyle.Render(fmt.Sprintf("%8s", costStr))
		} else {
			costStyled = fmt.Sprintf("%8s", costStr)
		}

		lines = append(lines, fmt.Sprintf("  %-10s %s %6s %5d", dateStr, costStyled, tokStr, c.RunCount))
	}
	return lines
}

// renderBugContent returns lines for the Failure Hotspots section.
func (m *MetricsMode) renderBugContent() []string {
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	yellowStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	redStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("1"))

	if len(m.bugs) == 0 {
		return []string{dimStyle.Render("  No failure hotspots")}
	}

	lines := []string{
		dimStyle.Render(fmt.Sprintf("  %-12s %-16s %6s %10s", "Bead", "Class", "Tries", "Last")),
	}
	for _, b := range m.bugs {
		lastStr := relativeTime(b.LastFailure)
		attStr := fmt.Sprintf("%d", b.AttemptCount)

		var attStyled string
		if b.AttemptCount >= 5 {
			attStyled = redStyle.Render(fmt.Sprintf("%6s", attStr))
		} else if b.AttemptCount >= 3 {
			attStyled = yellowStyle.Render(fmt.Sprintf("%6s", attStr))
		} else {
			attStyled = fmt.Sprintf("%6s", attStr)
		}

		// Dim unclassified failures; classified failures stand out.
		classStr := Truncate(b.FailureClass, 16)
		var classStyled string
		if b.FailureClass == "unknown" || b.FailureClass == "" {
			classStyled = dimStyle.Render(fmt.Sprintf("%-16s", classStr))
		} else {
			classStyled = yellowStyle.Render(fmt.Sprintf("%-16s", classStr))
		}

		lines = append(lines, fmt.Sprintf("  %-12s %s %s %10s",
			Truncate(b.BeadID, 12), classStyled, attStyled, lastStr))
	}
	return lines
}

// renderToolUsageContent returns lines for the Tool Usage section.
// Prefers OTel tool_events data when available; falls back to legacy tool_usage_stats.
func (m *MetricsMode) renderToolUsageContent() []string {
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	redStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("1"))

	// Prefer OTel tool_events when available.
	if len(m.toolEvents) > 0 {
		// Sort: count DESC → tool name ASC.
		sorted := make([]olap.ToolEventStats, len(m.toolEvents))
		copy(sorted, m.toolEvents)
		sort.Slice(sorted, func(i, j int) bool {
			if sorted[i].Count != sorted[j].Count {
				return sorted[i].Count > sorted[j].Count
			}
			return sorted[i].ToolName < sorted[j].ToolName
		})

		lines := []string{
			dimStyle.Render(fmt.Sprintf("  %-20s %6s %8s %5s", "Tool", "Calls", "Avg ms", "Fails")),
		}
		for _, t := range sorted {
			failStr := fmt.Sprintf("%d", t.FailureCount)
			var failStyled string
			if t.FailureCount > 0 {
				failStyled = redStyle.Render(fmt.Sprintf("%5s", failStr))
			} else {
				failStyled = fmt.Sprintf("%5s", failStr)
			}
			lines = append(lines, fmt.Sprintf("  %-20s %6d %8.0f %s",
				Truncate(t.ToolName, 20), t.Count, t.AvgDurationMs, failStyled))
		}
		return lines
	}

	// Fallback: legacy tool_usage_stats from agent_runs read_calls/edit_calls.
	if len(m.toolUsage) == 0 {
		return []string{dimStyle.Render("  No tool usage data")}
	}

	// Sort: ratio DESC → total DESC → formula ASC → phase ASC.
	sorted := make([]olap.ToolUsageStats, len(m.toolUsage))
	copy(sorted, m.toolUsage)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].ReadRatio != sorted[j].ReadRatio {
			return sorted[i].ReadRatio > sorted[j].ReadRatio
		}
		if sorted[i].TotalTools != sorted[j].TotalTools {
			return sorted[i].TotalTools > sorted[j].TotalTools
		}
		if sorted[i].FormulaName != sorted[j].FormulaName {
			return sorted[i].FormulaName < sorted[j].FormulaName
		}
		return sorted[i].Phase < sorted[j].Phase
	})

	lines := []string{
		dimStyle.Render(fmt.Sprintf("  %-14s %-10s %5s %5s %5s %6s", "Formula", "Phase", "Read", "Edit", "Total", "Ratio")),
	}
	for _, t := range sorted {
		ratioStr := fmt.Sprintf("%.0f%%", t.ReadRatio*100)
		lines = append(lines, fmt.Sprintf("  %-14s %-10s %5d %5d %5d %6s",
			Truncate(t.FormulaName, 14), Truncate(t.Phase, 10), t.TotalRead, t.TotalEdit, t.TotalTools, ratioStr))
	}
	return lines
}

// renderLifecycleContent returns lines for the Lifecycle P50/P95 panel,
// a compact per-bead-type rollup mirroring cmd/spire/metrics.go's
// renderLifecycleByType template. The column widths are narrower than the
// CLI version to fit the bottom-right board panel.
//
// Columns: TYPE, CNT, F→C P50, F→C P95, R→C P50, R→C P95,
// S→C P50, S→C P95, Q P50, Q P95.
func (m *MetricsMode) renderLifecycleContent() []string {
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	if len(m.lifecycleByType) == 0 {
		return []string{dimStyle.Render("  No lifecycle data")}
	}

	lines := []string{
		dimStyle.Render(fmt.Sprintf("  %-7s %3s %5s %5s %5s %5s %5s %5s %5s %5s",
			"TYPE", "CNT",
			"F→C50", "F→C95",
			"R→C50", "R→C95",
			"S→C50", "S→C95",
			"Q50", "Q95")),
	}
	for _, r := range m.lifecycleByType {
		lines = append(lines, fmt.Sprintf("  %-7s %3d %5s %5s %5s %5s %5s %5s %5s %5s",
			Truncate(r.BeadType, 7), r.Count,
			fmtSecondsCompact(r.FiledToClosedP50), fmtSecondsCompact(r.FiledToClosedP95),
			fmtSecondsCompact(r.ReadyToClosedP50), fmtSecondsCompact(r.ReadyToClosedP95),
			fmtSecondsCompact(r.StartedToClosedP50), fmtSecondsCompact(r.StartedToClosedP95),
			fmtSecondsCompact(r.QueueP50), fmtSecondsCompact(r.QueueP95),
		))
	}
	return lines
}

// fmtSecondsCompact formats a duration in seconds using the most-significant
// unit. Mirrors the fmtSeconds semantics from cmd/spire/metrics.go so the
// board display matches the CLI.
func fmtSecondsCompact(sec float64) string {
	if sec < 0 {
		return "—"
	}
	if sec < 120 {
		return fmt.Sprintf("%.0fs", sec)
	}
	if sec < 7200 {
		return fmt.Sprintf("%.1fm", sec/60)
	}
	return fmt.Sprintf("%.2fh", sec/3600)
}

// centeredMessage renders a message centered in the available space.
func (m *MetricsMode) centeredMessage(msg string) string {
	lines := strings.Split(msg, "\n")
	var s strings.Builder
	vpad := m.height / 2
	if vpad > 0 {
		s.WriteString(strings.Repeat("\n", vpad))
	}
	for _, line := range lines {
		pad := (m.width - len(line)) / 2
		if pad < 0 {
			pad = 0
		}
		s.WriteString(strings.Repeat(" ", pad) + line + "\n")
	}
	return s.String()
}

// formatTokens formats a token count as a compact string (e.g. "1.2M", "340K").
// Returns "—" for zero values.
func formatTokens(n int64) string {
	if n == 0 {
		return "—"
	}
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.0fK", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}

// formatDuration converts seconds to a human-readable duration string.
func formatDuration(seconds float64) string {
	if seconds <= 0 {
		return "0s"
	}
	d := time.Duration(seconds * float64(time.Second))
	h := int(d.Hours())
	min := int(d.Minutes()) % 60
	if h > 24 {
		days := h / 24
		return fmt.Sprintf("%dd %dh", days, h%24)
	}
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, min)
	}
	if min > 0 {
		return fmt.Sprintf("%dm", min)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

// relativeTime formats a time as a relative string (e.g. "2h ago", "3d ago").
func relativeTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		days := int(d.Hours()) / 24
		return fmt.Sprintf("%dd ago", days)
	}
}
