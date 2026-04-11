package board

import (
	"fmt"
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
	dora      *olap.DORAMetrics
	formulas  []olap.FormulaStats
	bugs      []olap.BugCausality
	costTrend []olap.CostTrendPoint
	toolUsage []olap.ToolUsageStats
	err       error
}

// metricsTickMsg triggers a periodic refresh.
type metricsTickMsg time.Time

// MetricsMode renders live metrics from DuckDB in the Board TUI.
type MetricsMode struct {
	width, height int
	towerName     string
	db            *olap.DB

	// Cached data (populated by async fetch).
	dora      *olap.DORAMetrics
	formulas  []olap.FormulaStats
	bugs      []olap.BugCausality
	costTrend []olap.CostTrendPoint
	toolUsage []olap.ToolUsageStats

	loading     bool
	lastErr     error
	lastRefresh time.Time
}

// NewMetricsMode creates a new MetricsMode.
func NewMetricsMode() *MetricsMode {
	return &MetricsMode{}
}

func (m *MetricsMode) ID() ModeID     { return ModeMetrics }
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

	towerCfg, err := config.ActiveTowerConfig()
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
			m.lastRefresh = time.Now()
		}
		return m, scheduleMetricsTick()

	case metricsTickMsg:
		if m.db != nil {
			return m, m.fetchMetrics()
		}
		return m, nil

	case tea.KeyMsg:
		if msg.String() == "r" {
			if m.db != nil {
				m.loading = true
				return m, m.fetchMetrics()
			}
			// No DB — try reactivating.
			return m, m.OnActivate()
		}
		return m, nil
	}
	return m, nil
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
		return metricsDataMsg{
			dora:      dora,
			formulas:  formulas,
			bugs:      bugs,
			costTrend: costTrend,
			toolUsage: toolUsage,
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
	if m.lastRefresh.IsZero() {
		return "r=refresh"
	}
	return fmt.Sprintf("Last updated: %s | r=refresh", relativeTime(m.lastRefresh))
}

// View renders the metrics dashboard. No I/O — only cached data.
func (m *MetricsMode) View() string {
	// Loading state.
	if m.loading && m.dora == nil {
		return m.centeredMessage("Loading metrics...")
	}

	// Error state with no data.
	if m.lastErr != nil && m.dora == nil {
		return m.centeredMessage(fmt.Sprintf("Error: %v\n\nPress r to retry", m.lastErr))
	}

	var s strings.Builder

	headerStyle := lipgloss.NewStyle().Bold(true)
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	greenStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	yellowStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	redStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("1"))

	// Section 1: DORA Metrics.
	s.WriteString(headerStyle.Render(" DORA Metrics") + "\n")
	s.WriteString(dimStyle.Render(" "+strings.Repeat("─", min(m.width-2, 60))) + "\n")

	if m.dora != nil {
		colW := max((m.width-4)/4, 16)
		labels := fmt.Sprintf(" %-*s %-*s %-*s %-*s",
			colW, "Deploy Freq", colW, "Lead Time", colW, "Failure Rate", colW, "MTTR")
		s.WriteString(dimStyle.Render(labels) + "\n")

		dfStr := fmt.Sprintf("%.1f/week", m.dora.DeployFrequency)
		ltStr := formatDuration(m.dora.LeadTimeSeconds)
		frStr := fmt.Sprintf("%.1f%%", m.dora.ChangeFailureRate*100)
		mttrStr := formatDuration(m.dora.MTTRSeconds)

		// Color failure rate: green <10%, yellow 10-25%, red >25%.
		frStyled := greenStyle.Render(frStr)
		if m.dora.ChangeFailureRate > 0.25 {
			frStyled = redStyle.Render(frStr)
		} else if m.dora.ChangeFailureRate > 0.10 {
			frStyled = yellowStyle.Render(frStr)
		}

		values := fmt.Sprintf(" %-*s %-*s %-*s %-*s",
			colW, greenStyle.Render(dfStr), colW, ltStr, colW, frStyled, colW, mttrStr)
		s.WriteString(values + "\n")
	} else {
		s.WriteString(dimStyle.Render(" No data") + "\n")
	}
	s.WriteString("\n")

	// Section 2: Formula Performance.
	s.WriteString(headerStyle.Render(" Formula Performance") + "\n")
	s.WriteString(dimStyle.Render(" "+strings.Repeat("─", min(m.width-2, 60))) + "\n")

	if len(m.formulas) > 0 {
		s.WriteString(dimStyle.Render(fmt.Sprintf(" %-18s %6s %8s %10s %12s", "Formula", "Runs", "Success", "Avg Cost", "Avg Reviews")) + "\n")
		for _, f := range m.formulas {
			successStr := fmt.Sprintf("%.1f%%", f.SuccessRate)
			costStr := fmt.Sprintf("$%.2f", f.AvgCostUSD)
			reviewStr := fmt.Sprintf("%.1f", f.AvgReviewRounds)

			// Color success rate: green >=90%, yellow 70-90%, red <70%.
			var successStyled string
			if f.SuccessRate >= 90 {
				successStyled = greenStyle.Render(fmt.Sprintf("%8s", successStr))
			} else if f.SuccessRate >= 70 {
				successStyled = yellowStyle.Render(fmt.Sprintf("%8s", successStr))
			} else {
				successStyled = redStyle.Render(fmt.Sprintf("%8s", successStr))
			}

			s.WriteString(fmt.Sprintf(" %-18s %6d %s %10s %12s\n",
				Truncate(f.FormulaName, 18), f.TotalRuns, successStyled, costStr, reviewStr))
		}
	} else {
		s.WriteString(dimStyle.Render(" No formula data") + "\n")
	}
	s.WriteString("\n")

	// Section 3: Top 5 Failure Hotspots.
	s.WriteString(headerStyle.Render(" Failure Hotspots") + "\n")
	s.WriteString(dimStyle.Render(" "+strings.Repeat("─", min(m.width-2, 60))) + "\n")

	if len(m.bugs) > 0 {
		s.WriteString(dimStyle.Render(fmt.Sprintf(" %-14s %-18s %10s %14s", "Bead", "Class", "Attempts", "Last Failure")) + "\n")
		for _, b := range m.bugs {
			lastStr := relativeTime(b.LastFailure)

			// Color attempts: yellow >=3, red >=5.
			attStr := fmt.Sprintf("%d", b.AttemptCount)
			var attStyled string
			if b.AttemptCount >= 5 {
				attStyled = redStyle.Render(fmt.Sprintf("%10s", attStr))
			} else if b.AttemptCount >= 3 {
				attStyled = yellowStyle.Render(fmt.Sprintf("%10s", attStr))
			} else {
				attStyled = fmt.Sprintf("%10s", attStr)
			}

			s.WriteString(fmt.Sprintf(" %-14s %-18s %s %14s\n",
				Truncate(b.BeadID, 14), Truncate(b.FailureClass, 18), attStyled, lastStr))
		}
	} else {
		s.WriteString(dimStyle.Render(" No failure hotspots") + "\n")
	}
	s.WriteString("\n")

	// Section 4: Cost Trend (last 7-10 days).
	s.WriteString(headerStyle.Render(" Cost Trend") + "\n")
	s.WriteString(dimStyle.Render(" "+strings.Repeat("─", min(m.width-2, 60))) + "\n")

	if len(m.costTrend) > 0 {
		s.WriteString(dimStyle.Render(fmt.Sprintf(" %-12s %10s %6s", "Date", "Cost", "Runs")) + "\n")
		limit := len(m.costTrend)
		if limit > 10 {
			limit = 10
		}
		for _, c := range m.costTrend[:limit] {
			dateStr := c.Date.Format("Jan 02")
			costStr := fmt.Sprintf("$%.2f", c.TotalCost)

			// Color cost: yellow >$5, red >$20.
			var costStyled string
			if c.TotalCost > 20 {
				costStyled = redStyle.Render(fmt.Sprintf("%10s", costStr))
			} else if c.TotalCost > 5 {
				costStyled = yellowStyle.Render(fmt.Sprintf("%10s", costStr))
			} else {
				costStyled = fmt.Sprintf("%10s", costStr)
			}

			s.WriteString(fmt.Sprintf(" %-12s %s %6d\n", dateStr, costStyled, c.RunCount))
		}
	} else {
		s.WriteString(dimStyle.Render(" No cost data") + "\n")
	}
	s.WriteString("\n")

	// Section 5: Tool Usage.
	s.WriteString(headerStyle.Render(" Tool Usage") + "\n")
	s.WriteString(dimStyle.Render(" "+strings.Repeat("─", min(m.width-2, 60))) + "\n")

	if len(m.toolUsage) > 0 {
		s.WriteString(dimStyle.Render(fmt.Sprintf(" %-18s %-12s %6s %6s %8s", "Formula", "Phase", "Read", "Edit", "Ratio")) + "\n")
		for _, t := range m.toolUsage {
			ratioStr := fmt.Sprintf("%.1f%%", t.ReadRatio*100)
			s.WriteString(fmt.Sprintf(" %-18s %-12s %6d %6d %8s\n",
				Truncate(t.FormulaName, 18), Truncate(t.Phase, 12), t.TotalRead, t.TotalEdit, ratioStr))
		}
	} else {
		s.WriteString(dimStyle.Render(" No tool usage data") + "\n")
	}

	// Error banner (shown with stale data).
	if m.lastErr != nil {
		s.WriteString("\n")
		warnStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))
		s.WriteString(warnStyle.Render(fmt.Sprintf(" Error: %v — press r to retry", m.lastErr)) + "\n")
	}

	return s.String()
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

