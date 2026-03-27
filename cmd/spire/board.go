package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/steveyegge/beads"
	"golang.org/x/term"
)

// BoardBead extends the standard Bead with fields needed for board display.
type BoardBead struct {
	ID              string     `json:"id"`
	Title           string     `json:"title"`
	Description     string     `json:"description"`
	Status          string     `json:"status"`
	Priority        int        `json:"priority"`
	Type            string     `json:"issue_type"`
	Owner           string     `json:"owner"`
	CreatedAt       string     `json:"created_at"`
	UpdatedAt       string     `json:"updated_at"`
	Labels          []string   `json:"labels"`
	Parent          string     `json:"parent"`
	Dependencies    []BoardDep `json:"dependencies"`
	DependencyCount int        `json:"dependency_count"`
	DependentCount  int        `json:"dependent_count"`
}

type BoardDep struct {
	IssueID     string `json:"issue_id"`
	DependsOnID string `json:"depends_on_id"`
	Type        string `json:"type"`
}

type boardColumns struct {
	Alerts    []BoardBead
	Ready     []BoardBead
	Design    []BoardBead
	Plan      []BoardBead
	Implement []BoardBead
	Review    []BoardBead
	Merge     []BoardBead
	Done      []BoardBead
	Blocked   []BoardBead
}

type boardJSON struct {
	Alerts    []BoardBead `json:"alerts"`
	Ready     []BoardBead `json:"ready"`
	Design    []BoardBead `json:"design"`
	Plan      []BoardBead `json:"plan"`
	Implement []BoardBead `json:"implement"`
	Review    []BoardBead `json:"review"`
	Merge     []BoardBead `json:"merge"`
	Done      []BoardBead `json:"done"`
	Blocked   []BoardBead `json:"blocked"`
}

// --- Board options (shared between JSON output and TUI mode) ---

type boardOpts struct {
	mine     bool
	ready    bool
	epic     string
	interval time.Duration
}

// heightBudgetResult holds the computed layout parameters for the board.
type heightBudgetResult struct {
	maxCards   int  // max cards to show per column
	compact    bool // use 1-line compact cards instead of 4-line cards
	maxAlerts  int  // max alert lines to show
	maxBlocked int  // max blocked lines to show
	maxAgents  int  // max agent lines to show in the agent panel (0 = hidden)
}

// calcHeightBudget computes card limits based on terminal height.
// Returns permissive defaults when termHeight is zero (non-TTY or unknown).
func calcHeightBudget(termHeight, alertCount, blockedCount, colCount, agentCount int) heightBudgetResult {
	if termHeight <= 0 {
		maxAg := agentCount
		if maxAg > 5 {
			maxAg = 5
		}
		return heightBudgetResult{maxCards: 99, maxAlerts: alertCount, maxBlocked: 8, maxAgents: maxAg}
	}

	// Fixed rows: header(2) + column-header+separator(2) + footer(3: blank+keys+bead).
	const fixed = 7
	available := termHeight - fixed
	if available < 4 {
		available = 4
	}

	// Allocate up to 20% of available rows for alerts (min 1, max alertCount).
	maxAlerts := 0
	if alertCount > 0 {
		maxAlerts = available / 5
		if maxAlerts < 1 {
			maxAlerts = 1
		}
		if maxAlerts > alertCount {
			maxAlerts = alertCount
		}
		available -= maxAlerts + 2 // header(1) + lines + blank(1)
		if available < 4 {
			available = 4
		}
	}

	// Allocate up to 20% of remaining rows for blocked (min 1, max blockedCount).
	maxBlocked := 0
	if blockedCount > 0 {
		maxBlocked = available / 5
		if maxBlocked < 1 {
			maxBlocked = 1
		}
		if maxBlocked > blockedCount {
			maxBlocked = blockedCount
		}
		available -= maxBlocked + 2 // blank(1) + header(1) + lines
		if available < 4 {
			available = 4
		}
	}

	// Allocate up to 20% of remaining rows for the agent panel (min 1, max 5).
	maxAgents := 0
	if agentCount > 0 {
		maxAgents = available / 5
		if maxAgents < 1 {
			maxAgents = 1
		}
		cap := agentCount
		if cap > 5 {
			cap = 5
		}
		if maxAgents > cap {
			maxAgents = cap
		}
		available -= maxAgents + 1 // header(1) + agent lines
		if available < 4 {
			available = 4
		}
	}

	// Try fitting cards in normal mode (4 lines each).
	maxCards := available / 4
	compact := false
	if maxCards < 2 {
		// Switch to compact (1 line per card).
		compact = true
		maxCards = available
		if maxCards < 1 {
			maxCards = 1
		}
	}

	return heightBudgetResult{
		maxCards:   maxCards,
		compact:    compact,
		maxAlerts:  maxAlerts,
		maxBlocked: maxBlocked,
		maxAgents:  maxAgents,
	}
}

// renderCompactCard renders a single bead as a one-line string for TUI columns.
// When selected is true, a ▶ cursor is prepended to indicate the active card.
func renderCompactCard(b BoardBead, color lipgloss.Color, width int, selected bool) string {
	titleWidth := width - 26
	if titleWidth < 15 {
		titleWidth = 15
	}
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	owner := beadOwnerLabel(b)
	ownerStr := ""
	if owner != "" {
		ownerStr = " " + lipgloss.NewStyle().Foreground(color).Render(owner)
	}
	line := fmt.Sprintf("%s %s %s %s%s %s",
		priStr(b.Priority), b.ID, shortType(b.Type),
		truncate(b.Title, titleWidth), ownerStr,
		dimStyle.Render(timeAgo(b.UpdatedAt)))
	if selected {
		cursor := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2")).Render("▶")
		return cursor + " " + line + "\n"
	}
	return line + "\n"
}

// staticCompactCardLine renders a single bead as a one-line static string.
func staticCompactCardLine(b BoardBead, color string, colWidth int) string {
	titleWidth := colWidth - 26
	if titleWidth < 15 {
		titleWidth = 15
	}
	owner := beadOwnerLabel(b)
	ownerStr := ""
	if owner != "" {
		ownerStr = " " + color + owner + reset
	}
	return fmt.Sprintf("%s %s %s %s%s %s%s%s",
		priorityStr(b.Priority), b.ID, shortType(b.Type),
		truncate(b.Title, titleWidth), ownerStr,
		dim, timeAgo(b.UpdatedAt), reset)
}

func cmdBoard(args []string) error {
	// Resolve .beads/ directory so board works from any directory.
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}
	if err := requireDolt(); err != nil {
		return err
	}

	var (
		flagJSON bool
		opts     boardOpts
	)
	opts.interval = 5 * time.Second

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--mine":
			opts.mine = true
		case "--ready":
			opts.ready = true
		case "--json":
			flagJSON = true
		case "--interval":
			if i+1 >= len(args) {
				return fmt.Errorf("--interval requires a value")
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil {
				return fmt.Errorf("--interval: invalid duration %q", args[i])
			}
			opts.interval = d
		case "--epic":
			if i+1 >= len(args) {
				return fmt.Errorf("--epic requires a bead ID")
			}
			i++
			opts.epic = args[i]
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire board [--mine] [--ready] [--epic <id>] [--json] [--interval 5s]", args[i])
		}
	}

	if flagJSON {
		cols, err := fetchBoard(opts)
		if err != nil {
			return err
		}
		out := boardJSON{
			Alerts:    nonNil(cols.Alerts),
			Ready:     nonNil(cols.Ready),
			Design:    nonNil(cols.Design),
			Plan:      nonNil(cols.Plan),
			Implement: nonNil(cols.Implement),
			Review:    nonNil(cols.Review),
			Merge:     nonNil(cols.Merge),
			Done:      nonNil(cols.Done),
			Blocked:   nonNil(cols.Blocked),
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return fmt.Errorf("spire board now launches the interactive TUI by default; use `spire board --json` for non-interactive output")
	}

	return runBoardTUI(opts)
}

// --- Data fetching ---

func fetchBoard(opts boardOpts) (boardColumns, error) {
	// Fetch open + in_progress beads via store API.
	openBeads, err := storeListBoardBeads(beads.IssueFilter{
		ExcludeStatus: []beads.Status{beads.StatusClosed},
	})
	if err != nil {
		return boardColumns{}, fmt.Errorf("board: list open beads: %w", err)
	}

	// Fetch recently closed beads (last 24h) for the Done column.
	// Best effort — an empty Done column is acceptable.
	closedBeads, _ := storeListBoardBeads(beads.IssueFilter{
		Status: statusPtr(beads.StatusClosed),
	})

	// Fetch blocked beads with blocker IDs.
	// Best effort — without this, blocked beads appear in Ready instead.
	blockedBeads, _ := storeGetBlockedIssues(beads.WorkFilter{})

	identity, _ := detectIdentity("")
	cols := categorizeColumnsFromStore(openBeads, closedBeads, blockedBeads, identity)

	if opts.epic != "" {
		cols = filterEpic(cols, opts.epic)
	}
	if opts.mine {
		cols.Ready = nil
		cols.Design = filterOwned(cols.Design, identity)
		cols.Plan = filterOwned(cols.Plan, identity)
		cols.Implement = filterOwned(cols.Implement, identity)
		cols.Review = filterOwned(cols.Review, identity)
		cols.Merge = filterOwned(cols.Merge, identity)
		cols.Blocked = filterOwned(cols.Blocked, identity)
	}
	if opts.ready {
		cols.Design = nil
		cols.Plan = nil
		cols.Implement = nil
		cols.Review = nil
		cols.Merge = nil
		cols.Done = nil
		cols.Blocked = nil
	}

	sortBeads(cols.Ready)
	sortBeads(cols.Design)
	sortBeads(cols.Plan)
	sortBeads(cols.Implement)
	sortBeads(cols.Review)
	sortBeads(cols.Merge)
	sortBeads(cols.Done)
	sortBeads(cols.Blocked)

	return cols, nil
}

// --- Bubbletea TUI ---

// boardColDef is a display column with a name, color, and bead slice.
type boardColDef struct {
	name  string
	color lipgloss.Color
	beads []BoardBead
}

type boardTypeScope int

const (
	boardTypeAll boardTypeScope = iota
	boardTypeTask
	boardTypeBug
	boardTypeEpic
	boardTypeDesign
	boardTypeDecision
	boardTypeOther
)

var boardTypeScopeOrder = []boardTypeScope{
	boardTypeAll,
	boardTypeTask,
	boardTypeBug,
	boardTypeEpic,
	boardTypeDesign,
	boardTypeDecision,
	boardTypeOther,
}

func (s boardTypeScope) next() boardTypeScope {
	for i, candidate := range boardTypeScopeOrder {
		if candidate == s {
			return boardTypeScopeOrder[(i+1)%len(boardTypeScopeOrder)]
		}
	}
	return boardTypeAll
}

func (s boardTypeScope) label() string {
	switch s {
	case boardTypeTask:
		return "tasks"
	case boardTypeBug:
		return "bugs"
	case boardTypeEpic:
		return "epics"
	case boardTypeDesign:
		return "designs"
	case boardTypeDecision:
		return "decisions"
	case boardTypeOther:
		return "other"
	default:
		return "all"
	}
}

func (s boardTypeScope) match(b BoardBead) bool {
	switch s {
	case boardTypeAll:
		return true
	case boardTypeTask:
		return b.Type == "task"
	case boardTypeBug:
		return b.Type == "bug"
	case boardTypeEpic:
		return b.Type == "epic"
	case boardTypeDesign:
		return b.Type == "design"
	case boardTypeDecision:
		return b.Type == "decision"
	case boardTypeOther:
		switch b.Type {
		case "task", "bug", "epic", "design", "decision":
			return false
		default:
			return true
		}
	default:
		return true
	}
}

// boardActiveColumns returns the non-empty columns in board display order.
// This is the authoritative ordered list used by both navigation and rendering.
func boardActiveColumns(cols boardColumns) []boardColDef {
	all := []boardColDef{
		{"READY", lipgloss.Color("2"), cols.Ready},
		{"DESIGN", lipgloss.Color("4"), cols.Design},
		{"PLAN", lipgloss.Color("6"), cols.Plan},
		{"IMPLEMENT", lipgloss.Color("6"), cols.Implement},
		{"REVIEW", lipgloss.Color("3"), cols.Review},
		{"MERGE", lipgloss.Color("5"), cols.Merge},
		{"DONE", lipgloss.Color("8"), cols.Done},
	}
	var active []boardColDef
	for _, c := range all {
		if len(c.beads) > 0 {
			active = append(active, c)
		}
	}
	return active
}

// boardPendingAction identifies an action to run after the TUI exits.
type boardPendingAction int

const (
	boardActionNone   boardPendingAction = iota
	boardActionFocus                     // print cmdFocus output, then relaunch
	boardActionLogs                      // tail wizard logs, then relaunch
	boardActionSummon                    // summon a wizard for the bead, then relaunch
	boardActionClaim                     // claim the bead, then relaunch
)

type boardModel struct {
	opts      boardOpts
	cols      boardColumns
	agents    []localWizard // alive local wizards from registry
	typeScope boardTypeScope
	width     int
	height    int
	lastTick  time.Time
	quitting  bool
	selCol    int // selected column index into boardActiveColumns(cols)
	selCard   int // selected card index within selCol
	// Pending action: set before tea.Quit so runBoardTUI can execute it.
	pendingAction boardPendingAction
	pendingBeadID string
}

func (m boardModel) visibleCols() boardColumns {
	return filterBoardTypeScope(m.cols, m.typeScope)
}

// clampSelection keeps selCol and selCard within valid bounds.
func (m *boardModel) clampSelection() {
	active := boardActiveColumns(m.visibleCols())
	if len(active) == 0 {
		m.selCol = 0
		m.selCard = 0
		return
	}
	if m.selCol < 0 {
		m.selCol = 0
	}
	if m.selCol >= len(active) {
		m.selCol = len(active) - 1
	}
	n := len(active[m.selCol].beads)
	if n == 0 {
		m.selCard = 0
		return
	}
	// Wrap vertically.
	m.selCard = ((m.selCard % n) + n) % n
}

// selectedBead returns a pointer to the currently selected bead, or nil.
func (m boardModel) selectedBead() *BoardBead {
	active := boardActiveColumns(m.visibleCols())
	if m.selCol < 0 || m.selCol >= len(active) {
		return nil
	}
	beads := active[m.selCol].beads
	if m.selCard < 0 || m.selCard >= len(beads) {
		return nil
	}
	return &beads[m.selCard]
}

type tickMsg time.Time

func tickCmd(interval time.Duration) tea.Cmd {
	return tea.Tick(interval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// fetchAgents loads alive local wizards from the wizard registry.
func fetchAgents() []localWizard {
	reg := loadWizardRegistry()
	reg = cleanDeadWizards(reg)
	return reg.Wizards
}

func runBoardTUI(opts boardOpts) error {
	for {
		cols, err := fetchBoard(opts)
		if err != nil {
			return err
		}
		m := boardModel{
			opts:     opts,
			cols:     cols,
			agents:   fetchAgents(),
			lastTick: time.Now(),
		}
		p := tea.NewProgram(m, tea.WithAltScreen())
		result, err := p.Run()
		if err != nil {
			return err
		}

		final, ok := result.(boardModel)
		if !ok || final.pendingAction == boardActionNone {
			break
		}

		// Execute the pending action on the raw terminal, then relaunch.
		if !executeBoardAction(final.pendingAction, final.pendingBeadID) {
			break
		}
	}
	return nil
}

func (m boardModel) Init() tea.Cmd {
	return tickCmd(m.opts.interval)
}

func (m boardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			m.quitting = true
			return m, tea.Quit

		// Column navigation.
		case "h", "left", "shift+tab":
			m.selCol--
			m.selCard = 0
			m.clampSelection()
		case "l", "right", "tab":
			m.selCol++
			m.selCard = 0
			m.clampSelection()

		// Card navigation.
		case "j", "down":
			m.selCard++
			m.clampSelection()
		case "k", "up":
			m.selCard--
			m.clampSelection()

			// Epic scoping toggle.
		case "e":
			if m.opts.epic != "" {
				m.opts.epic = ""
			} else if bead := m.selectedBead(); bead != nil {
				if bead.Type == "epic" {
					m.opts.epic = bead.ID
				} else if bead.Parent != "" {
					m.opts.epic = bead.Parent
				}
			}
			if newCols, err := fetchBoard(m.opts); err == nil {
				m.cols = newCols
			}
			m.clampSelection()
		case "t":
			m.typeScope = m.typeScope.next()
			m.clampSelection()

		// Actions on the selected bead.
		case "f":
			if bead := m.selectedBead(); bead != nil {
				m.pendingAction = boardActionFocus
				m.pendingBeadID = bead.ID
				m.quitting = true
				return m, tea.Quit
			}
		case "s":
			if bead := m.selectedBead(); bead != nil {
				m.pendingAction = boardActionSummon
				m.pendingBeadID = bead.ID
				m.quitting = true
				return m, tea.Quit
			}
		case "c":
			if bead := m.selectedBead(); bead != nil {
				m.pendingAction = boardActionClaim
				m.pendingBeadID = bead.ID
				m.quitting = true
				return m, tea.Quit
			}
		case "L":
			if bead := m.selectedBead(); bead != nil {
				m.pendingAction = boardActionLogs
				m.pendingBeadID = bead.ID
				m.quitting = true
				return m, tea.Quit
			}
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tickMsg:
		if cols, err := fetchBoard(m.opts); err == nil {
			m.cols = cols
		}
		m.agents = fetchAgents()
		m.lastTick = time.Now()
		m.clampSelection()
		return m, tickCmd(m.opts.interval)
	}
	return m, nil
}

func (m boardModel) View() string {
	if m.quitting {
		return ""
	}

	visibleCols := m.visibleCols()
	colWidth := 30
	if m.width > 0 {
		// Fit columns to terminal width.
		activeCols := countActiveCols(visibleCols)
		if activeCols > 0 {
			available := m.width - (activeCols-1)*2 // 2-char gap between columns
			cw := available / activeCols
			if cw > 50 {
				cw = 50
			}
			if cw > 20 {
				colWidth = cw
			}
		}
	}

	var s strings.Builder

	// Compute height budget before rendering any sections.
	budget := calcHeightBudget(m.height, len(visibleCols.Alerts), len(visibleCols.Blocked), countActiveCols(visibleCols), len(m.agents))

	// Header.
	header := lipgloss.NewStyle().Bold(true).Render("Spire Board")
	ts := lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render(m.lastTick.Format("15:04:05"))
	s.WriteString(header + "  " + ts + "\n\n")

	// Alerts (capped by budget).
	if len(visibleCols.Alerts) > 0 {
		sortBeads(visibleCols.Alerts)
		alertStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))
		s.WriteString(alertStyle.Render(fmt.Sprintf("⚠ ALERTS (%d)", len(visibleCols.Alerts))) + "\n")
		for i, a := range visibleCols.Alerts {
			if i >= budget.maxAlerts {
				dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
				s.WriteString(dimStyle.Render(fmt.Sprintf("  ... +%d more", len(visibleCols.Alerts)-budget.maxAlerts)) + "\n")
				break
			}
			alertType := ""
			for _, l := range a.Labels {
				if strings.HasPrefix(l, "alert:") {
					alertType = "[" + l[6:] + "] "
				}
			}
			s.WriteString(fmt.Sprintf("  %s %s%s\n", priStr(a.Priority), alertType, a.Title))
		}
		s.WriteString("\n")
	}

	// Build column content.
	active := boardActiveColumns(visibleCols)

	if len(active) > 0 {
		// Render each column as a string.
		rendered := make([]string, len(active))
		for i, c := range active {
			var cb strings.Builder
			headerStyle := lipgloss.NewStyle().Bold(true).Foreground(c.color)
			if i == m.selCol {
				headerStyle = headerStyle.Underline(true)
			}
			cb.WriteString(headerStyle.Render(fmt.Sprintf("%s (%d)", c.name, len(c.beads))))
			cb.WriteString("\n")
			sepStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
			cb.WriteString(sepStyle.Render(strings.Repeat("─", min(colWidth, len(c.name)+4))))
			cb.WriteString("\n")

			for j, b := range c.beads {
				if j >= budget.maxCards {
					dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
					cb.WriteString(dimStyle.Render(fmt.Sprintf("  ... +%d more", len(c.beads)-budget.maxCards)))
					cb.WriteString("\n")
					break
				}
				isSelected := (i == m.selCol && j == m.selCard)
				if budget.compact {
					cb.WriteString(renderCompactCard(b, c.color, colWidth, isSelected))
				} else {
					cb.WriteString(renderCardStr(b, c.color, colWidth, isSelected))
				}
			}
			rendered[i] = cb.String()
		}

		// Join columns horizontally.
		s.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, addGaps(rendered, colWidth)...))
		s.WriteString("\n")
	}

	// Blocked (capped by budget).
	if len(visibleCols.Blocked) > 0 {
		blockedStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))
		s.WriteString(blockedStyle.Render(fmt.Sprintf("BLOCKED (%d)", len(visibleCols.Blocked))) + "\n")

		for i, b := range visibleCols.Blocked {
			if i >= budget.maxBlocked {
				dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
				s.WriteString(dimStyle.Render(fmt.Sprintf("  ... +%d more", len(visibleCols.Blocked)-budget.maxBlocked)) + "\n")
				break
			}
			blockers := blockingDepIDs(b)
			blockerStr := ""
			if len(blockers) > 0 {
				bStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
				blockerStr = " " + bStyle.Render("← "+strings.Join(blockers, ", "))
			}
			s.WriteString(fmt.Sprintf("  %s %s %s%s\n", priStr(b.Priority), b.ID, truncate(b.Title, 40), blockerStr))
		}
	}

	// Agent panel (capped by budget).
	if budget.maxAgents > 0 && len(m.agents) > 0 {
		s.WriteString(renderAgentPanel(m.agents, budget.maxAgents))
	}

	// Footer: two lines — keybindings + contextual bead info.
	s.WriteString("\n")
	footerStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	scopeStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	epicInfo := ""
	if m.opts.epic != "" {
		epicInfo = " • epic:" + m.opts.epic
	}
	leftFooter := footerStyle.Render("j/k ↕  h/l ↔  tab  t type  f focus  s summon  c claim  L logs  e epic" + epicInfo + " • q quit • ↻ " + m.opts.interval.String())
	rightFooter := scopeStyle.Render("showing " + m.typeScope.label())
	if m.width > 0 {
		gap := m.width - lipgloss.Width(leftFooter) - lipgloss.Width(rightFooter)
		if gap > 1 {
			s.WriteString(leftFooter)
			s.WriteString(strings.Repeat(" ", gap))
			s.WriteString(rightFooter)
		} else {
			s.WriteString(leftFooter + "  " + rightFooter)
		}
	} else {
		s.WriteString(leftFooter + "  " + rightFooter)
	}
	s.WriteString("\n")
	// Second footer line: selected bead context.
	var footerParts []string
	if bead := m.selectedBead(); bead != nil {
		beadStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("7"))
		footerParts = append(footerParts, beadStyle.Render(bead.ID+"  "+truncate(bead.Title, 60)))
	}
	if len(footerParts) > 0 {
		s.WriteString(strings.Join(footerParts, footerStyle.Render("  •  ")))
	}

	return s.String()
}

// renderAgentPanel renders a compact live agent status panel.
// agents must already be alive (cleaned from registry).
func renderAgentPanel(agents []localWizard, maxAgents int) string {
	var s strings.Builder
	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	phaseStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("4"))

	shown := len(agents)
	if shown > maxAgents {
		shown = maxAgents
	}
	s.WriteString(headerStyle.Render(fmt.Sprintf("AGENTS (%d)", len(agents))) + "\n")
	for i := 0; i < shown; i++ {
		w := agents[i]
		phase := w.Phase
		if phase == "" {
			phase = "working"
		}
		elapsed := ""
		if t, err := time.Parse(time.RFC3339, w.StartedAt); err == nil {
			d := time.Since(t).Round(time.Second)
			h := int(d.Hours())
			m := int(d.Minutes()) % 60
			sec := int(d.Seconds()) % 60
			if h > 0 {
				elapsed = fmt.Sprintf("%dh%02dm", h, m)
			} else if m > 0 {
				elapsed = fmt.Sprintf("%dm%02ds", m, sec)
			} else {
				elapsed = fmt.Sprintf("%ds", sec)
			}
		}
		name := w.Name
		if len(name) > 28 {
			name = name[:27] + "…"
		}
		beadPart := ""
		if w.BeadID != "" {
			beadPart = "  " + w.BeadID
		}
		line := fmt.Sprintf("  %-28s%s  %s  %s",
			name,
			beadPart,
			phaseStyle.Render(phase),
			dimStyle.Render(elapsed),
		)
		s.WriteString(line + "\n")
	}
	if len(agents) > maxAgents {
		s.WriteString(dimStyle.Render(fmt.Sprintf("  ... +%d more", len(agents)-maxAgents)) + "\n")
	}
	return s.String()
}

// renderCardStr renders a single bead card as a multi-line string for a column.
// renderCardStr renders a single bead card as a multi-line string for a column.
// When selected is true, a ▶ cursor is prepended to indicate the active card.
func renderCardStr(b BoardBead, color lipgloss.Color, width int, selected ...bool) string {
	titleWidth := width - 4
	if titleWidth < 10 {
		titleWidth = 10
	}

	typeStr := shortType(b.Type)
	if phase := getBoardBeadPhase(b); phase != "" {
		typeStr += " [" + phase + "]"
	}

	isSel := len(selected) > 0 && selected[0]
	var s strings.Builder
	if isSel {
		cursor := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2")).Render("▶")
		s.WriteString(fmt.Sprintf("%s %s %s %s\n", cursor, priStr(b.Priority), b.ID, typeStr))
	} else {
		s.WriteString(fmt.Sprintf("%s %s %s\n", priStr(b.Priority), b.ID, typeStr))
	}
	s.WriteString(fmt.Sprintf("  %s\n", truncate(b.Title, titleWidth)))

	// Third line: context (owner, time, etc.)
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	colorStyle := lipgloss.NewStyle().Foreground(color)

	owner := beadOwnerLabel(b)
	if owner != "" {
		s.WriteString(fmt.Sprintf("  %s %s\n", colorStyle.Render(owner), dimStyle.Render(timeAgo(b.UpdatedAt))))
	} else {
		s.WriteString(fmt.Sprintf("  %s\n", dimStyle.Render(timeAgo(b.CreatedAt))))
	}

	s.WriteString("\n") // blank line between cards
	return s.String()
}

// addGaps pads each column string to colWidth and adds 2-char gaps.
func addGaps(columns []string, colWidth int) []string {
	style := lipgloss.NewStyle().Width(colWidth + 2)
	out := make([]string, len(columns))
	for i, c := range columns {
		out[i] = style.Render(c)
	}
	return out
}

func countActiveCols(cols boardColumns) int {
	return len(boardActiveColumns(cols))
}

func priStr(p int) string {
	label := fmt.Sprintf("P%d", p)
	switch p {
	case 0, 1:
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1")).Render(label)
	case 2:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Render(label)
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render(label)
	}
}

// --- Shared board helpers ---

// categorizeColumnsFromStore builds board columns from store API results.
// blockedBeads come from GetBlockedIssues and already have blocker metadata.
func categorizeColumnsFromStore(openBeads, closedBeads, blockedBeads []BoardBead, identity string) boardColumns {
	var c boardColumns

	isAlert := func(b BoardBead) bool {
		for _, l := range b.Labels {
			if l == "alert" || strings.HasPrefix(l, "alert:") {
				return true
			}
		}
		return false
	}

	skip := func(b BoardBead) bool {
		for _, l := range b.Labels {
			if strings.HasPrefix(l, "msg") || l == "template" || strings.HasPrefix(l, "agent") {
				return true
			}
		}
		// Skip attempt beads (internal tracking, not board-visible work)
		if isAttemptBoardBead(b) {
			return true
		}
		// Skip review-round beads (internal tracking, not board-visible work)
		if isReviewRoundBoardBead(b) {
			return true
		}
		// Skip workflow step beads (internal phase tracking, not board-visible work)
		if isStepBoardBead(b) {
			return true
		}
		return false
	}

	// Build a set of blocked IDs for fast lookup. Blocked beads are authoritative
	// from storeGetBlockedIssues (which includes blocker metadata); openBeads will
	// also contain these same beads (status=open), but we skip them via blockedIDs
	// so the Blocked column uses the richer representation with dependency info.
	blockedIDs := make(map[string]bool, len(blockedBeads))
	for _, b := range blockedBeads {
		if skip(b) {
			continue
		}
		blockedIDs[b.ID] = true
		c.Blocked = append(c.Blocked, b)
	}

	for _, b := range openBeads {
		if isAlert(b) && b.Status == "open" {
			c.Alerts = append(c.Alerts, b)
			continue
		}
		if skip(b) {
			continue
		}
		// Skip beads already in the Blocked column.
		if blockedIDs[b.ID] {
			continue
		}

		phase := getBoardBeadPhase(b)
		switch {
		case phase == "design":
			c.Design = append(c.Design, b)
		case phase == "plan":
			c.Plan = append(c.Plan, b)
		case phase == "implement":
			c.Implement = append(c.Implement, b)
		case strings.HasPrefix(phase, "review"):
			c.Review = append(c.Review, b)
		case phase == "merge":
			c.Merge = append(c.Merge, b)
		default:
			// No phase label → Ready.
			c.Ready = append(c.Ready, b)
		}
	}

	cutoff := time.Now().Add(-24 * time.Hour)
	for _, b := range closedBeads {
		if skip(b) {
			continue
		}
		t, err := time.Parse(time.RFC3339, b.UpdatedAt)
		if err != nil {
			t, err = time.Parse("2006-01-02 15:04:05", b.UpdatedAt)
		}
		if err == nil && t.After(cutoff) {
			c.Done = append(c.Done, b)
		}
	}

	return c
}

func filterEpic(cols boardColumns, epicID string) boardColumns {
	match := func(b BoardBead) bool {
		return b.ID == epicID || b.Parent == epicID || strings.HasPrefix(b.ID, epicID+".")
	}
	return boardColumns{
		Alerts:    filterBeads(cols.Alerts, match),
		Ready:     filterBeads(cols.Ready, match),
		Design:    filterBeads(cols.Design, match),
		Plan:      filterBeads(cols.Plan, match),
		Implement: filterBeads(cols.Implement, match),
		Review:    filterBeads(cols.Review, match),
		Merge:     filterBeads(cols.Merge, match),
		Done:      filterBeads(cols.Done, match),
		Blocked:   filterBeads(cols.Blocked, match),
	}
}

func filterBoardTypeScope(cols boardColumns, scope boardTypeScope) boardColumns {
	if scope == boardTypeAll {
		return cols
	}
	match := func(b BoardBead) bool {
		return scope.match(b)
	}
	return boardColumns{
		Alerts:    filterBeads(cols.Alerts, match),
		Ready:     filterBeads(cols.Ready, match),
		Design:    filterBeads(cols.Design, match),
		Plan:      filterBeads(cols.Plan, match),
		Implement: filterBeads(cols.Implement, match),
		Review:    filterBeads(cols.Review, match),
		Merge:     filterBeads(cols.Merge, match),
		Done:      filterBeads(cols.Done, match),
		Blocked:   filterBeads(cols.Blocked, match),
	}
}

func filterBeads(beads []BoardBead, pred func(BoardBead) bool) []BoardBead {
	var out []BoardBead
	for _, b := range beads {
		if pred(b) {
			out = append(out, b)
		}
	}
	return out
}

func beadOwnerLabel(b BoardBead) string {
	for _, l := range b.Labels {
		if strings.HasPrefix(l, "owner:") {
			return l[6:]
		}
	}
	return ""
}

func isCurrentUser(claimedBy, identity string) bool {
	if claimedBy == "" || identity == "" {
		return false
	}
	return claimedBy == identity ||
		strings.EqualFold(claimedBy, identity) ||
		strings.Contains(claimedBy, identity)
}

func blockingDepIDs(b BoardBead) []string {
	var ids []string
	for _, d := range b.Dependencies {
		if d.Type == "blocks" {
			ids = append(ids, d.DependsOnID)
		}
	}
	return ids
}

func filterOwned(beads []BoardBead, identity string) []BoardBead {
	var out []BoardBead
	for _, b := range beads {
		claimedBy := beadOwnerLabel(b)
		if claimedBy == "" {
			claimedBy = b.Owner
		}
		if isCurrentUser(claimedBy, identity) {
			out = append(out, b)
		}
	}
	return out
}

func sortBeads(beads []BoardBead) {
	sort.Slice(beads, func(i, j int) bool {
		if beads[i].Priority != beads[j].Priority {
			return beads[i].Priority < beads[j].Priority
		}
		left := boardSortTime(beads[i])
		right := boardSortTime(beads[j])
		if !left.Equal(right) {
			return left.After(right)
		}
		return beads[i].ID < beads[j].ID
	})
}

func boardSortTime(b BoardBead) time.Time {
	if t, ok := parseBoardTime(b.UpdatedAt); ok {
		return t
	}
	if t, ok := parseBoardTime(b.CreatedAt); ok {
		return t
	}
	return time.Time{}
}

func parseBoardTime(ts string) (time.Time, bool) {
	if ts == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err == nil {
		return t, true
	}
	t, err = time.Parse("2006-01-02 15:04:05", ts)
	if err == nil {
		return t, true
	}
	return time.Time{}, false
}

func nonNil(beads []BoardBead) []BoardBead {
	if beads == nil {
		return []BoardBead{}
	}
	return beads
}

func shortType(t string) string {
	switch t {
	case "feature":
		return "feat"
	case "task":
		return "task"
	case "bug":
		return "bug"
	case "epic":
		return "epic"
	case "chore":
		return "chore"
	case "decision":
		return "dec"
	default:
		return t
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

func timeAgo(ts string) string {
	t, ok := parseBoardTime(ts)
	if !ok {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%dw ago", int(d.Hours()/(24*7)))
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// --- Static output (non-TUI fallback) ---

// ANSI codes for static mode.
const (
	bold    = "\033[1m"
	dim     = "\033[2m"
	red     = "\033[31m"
	yellow  = "\033[33m"
	green   = "\033[32m"
	cyan    = "\033[36m"
	magenta = "\033[35m"
	reset   = "\033[0m"
)

func printColumnarBoard(cols boardColumns, _ int) {
	// Detect terminal height for height-aware layout.
	_, termHeight, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		termHeight = 0
	}

	// Compute height budget.
	var activeCols int
	for _, c := range [][]BoardBead{cols.Ready, cols.Design, cols.Plan, cols.Implement, cols.Review, cols.Merge, cols.Done} {
		if len(c) > 0 {
			activeCols++
		}
	}
	budget := calcHeightBudget(termHeight, len(cols.Alerts), len(cols.Blocked), activeCols, 0)

	// Alerts (capped by budget).
	if len(cols.Alerts) > 0 {
		sortBeads(cols.Alerts)
		fmt.Printf("%s%s ALERTS (%d) %s\n", bold+red, "⚠", len(cols.Alerts), reset)
		for i, a := range cols.Alerts {
			if i >= budget.maxAlerts {
				fmt.Printf("  %s... +%d more%s\n", dim, len(cols.Alerts)-budget.maxAlerts, reset)
				break
			}
			alertType := ""
			for _, l := range a.Labels {
				if strings.HasPrefix(l, "alert:") {
					alertType = "[" + l[6:] + "] "
				}
			}
			fmt.Printf("  %s %s%s\n", priorityStr(a.Priority), alertType, a.Title)
		}
		fmt.Println()
	}

	// Columns.
	type column struct {
		header string
		color  string
		beads  []BoardBead
	}
	columns := []column{
		{"READY", green, cols.Ready},
		{"DESIGN", cyan, cols.Design},
		{"PLAN", cyan, cols.Plan},
		{"IMPLEMENT", cyan, cols.Implement},
		{"REVIEW", yellow, cols.Review},
		{"MERGE", magenta, cols.Merge},
		{"DONE", dim, cols.Done},
	}

	var active []column
	for _, col := range columns {
		if len(col.beads) > 0 {
			active = append(active, col)
		}
	}

	if len(active) == 0 && len(cols.Blocked) == 0 {
		fmt.Printf("%s(no work items)%s\n", dim, reset)
		return
	}

	colWidth := 30
	if len(active) <= 2 {
		colWidth = 38
	}

	// Headers.
	if len(active) > 0 {
		for i, col := range active {
			if i > 0 {
				fmt.Print("  ")
			}
			header := fmt.Sprintf("%s%s%s (%d)%s", bold, col.color, col.header, len(col.beads), reset)
			fmt.Print(header)
			visible := len(col.header) + 2 + countDigits(len(col.beads)) + 1
			if pad := colWidth - visible; pad > 0 {
				fmt.Print(strings.Repeat(" ", pad))
			}
		}
		fmt.Println()

		for i, col := range active {
			if i > 0 {
				fmt.Print("  ")
			}
			sep := strings.Repeat("─", min(colWidth, len(col.header)+4))
			fmt.Printf("%s%s%s", dim, sep, reset)
			if pad := colWidth - min(colWidth, len(col.header)+4); pad > 0 {
				fmt.Print(strings.Repeat(" ", pad))
			}
		}
		fmt.Println()

		// Cap each column to budget.maxCards beads.
		type cappedColumn struct {
			header   string
			color    string
			beads    []BoardBead
			overflow int
		}
		capped := make([]cappedColumn, len(active))
		for i, col := range active {
			cc := cappedColumn{header: col.header, color: col.color}
			if len(col.beads) > budget.maxCards {
				cc.beads = col.beads[:budget.maxCards]
				cc.overflow = len(col.beads) - budget.maxCards
			} else {
				cc.beads = col.beads
			}
			capped[i] = cc
		}

		if budget.compact {
			// Compact mode: 1 line per card.
			maxRows := 0
			for _, cc := range capped {
				n := len(cc.beads)
				if cc.overflow > 0 {
					n++
				}
				if n > maxRows {
					maxRows = n
				}
			}
			for row := 0; row < maxRows; row++ {
				for i, cc := range capped {
					if i > 0 {
						fmt.Print("  ")
					}
					if row < len(cc.beads) {
						line := staticCompactCardLine(cc.beads[row], cc.color, colWidth)
						fmt.Print(line)
						if pad := colWidth - visibleLen(line); pad > 0 {
							fmt.Print(strings.Repeat(" ", pad))
						}
					} else if row == len(cc.beads) && cc.overflow > 0 {
						line := fmt.Sprintf("%s... +%d more%s", dim, cc.overflow, reset)
						fmt.Print(line)
						if pad := colWidth - visibleLen(line); pad > 0 {
							fmt.Print(strings.Repeat(" ", pad))
						}
					} else {
						fmt.Print(strings.Repeat(" ", colWidth))
					}
				}
				fmt.Println()
			}
		} else {
			// Normal mode: 4 lines per card.
			maxRows := 0
			for _, cc := range capped {
				n := len(cc.beads) * 4
				if cc.overflow > 0 {
					n++
				}
				if n > maxRows {
					maxRows = n
				}
			}
			for row := 0; row < maxRows; row++ {
				for i, cc := range capped {
					if i > 0 {
						fmt.Print("  ")
					}
					cardIdx := row / 4
					lineIdx := row % 4
					if cardIdx < len(cc.beads) {
						line := staticCardLine(cc.beads[cardIdx], cc.color, lineIdx, colWidth)
						fmt.Print(line)
						if pad := colWidth - visibleLen(line); pad > 0 {
							fmt.Print(strings.Repeat(" ", pad))
						}
					} else if cardIdx == len(cc.beads) && lineIdx == 0 && cc.overflow > 0 {
						line := fmt.Sprintf("%s... +%d more%s", dim, cc.overflow, reset)
						fmt.Print(line)
						if pad := colWidth - visibleLen(line); pad > 0 {
							fmt.Print(strings.Repeat(" ", pad))
						}
					} else {
						fmt.Print(strings.Repeat(" ", colWidth))
					}
				}
				fmt.Println()
			}
		}
	}

	// Blocked (capped by budget).
	if len(cols.Blocked) > 0 {
		if len(active) > 0 {
			fmt.Println()
		}
		fmt.Printf("%s%sBLOCKED (%d)%s\n", bold, red, len(cols.Blocked), reset)
		for i, b := range cols.Blocked {
			if i >= budget.maxBlocked {
				fmt.Printf("  %s... +%d more%s\n", dim, len(cols.Blocked)-budget.maxBlocked, reset)
				break
			}
			blockers := blockingDepIDs(b)
			blockerStr := ""
			if len(blockers) > 0 {
				blockerStr = fmt.Sprintf(" %s← %s%s", dim, strings.Join(blockers, ", "), reset)
			}
			fmt.Printf("  %s %s %s%s\n", priorityStr(b.Priority), b.ID, truncate(b.Title, 40), blockerStr)
		}
	}
}

func staticCardLine(b BoardBead, color string, lineIdx, colWidth int) string {
	titleWidth := colWidth - 4
	if titleWidth < 10 {
		titleWidth = 10
	}
	switch lineIdx {
	case 0:
		typeStr := shortType(b.Type)
		if phase := getBoardBeadPhase(b); phase != "" {
			typeStr += " [" + phase + "]"
		}
		return fmt.Sprintf("%s %s %s", priorityStr(b.Priority), b.ID, typeStr)
	case 1:
		return fmt.Sprintf("  %s", truncate(b.Title, titleWidth))
	case 2:
		owner := beadOwnerLabel(b)
		if owner != "" {
			return fmt.Sprintf("  %s%s%s %s%s%s", color, owner, reset, dim, timeAgo(b.UpdatedAt), reset)
		}
		return fmt.Sprintf("  %s%s%s", dim, timeAgo(b.CreatedAt), reset)
	default:
		return ""
	}
}

func priorityStr(p int) string {
	label := fmt.Sprintf("P%d", p)
	switch p {
	case 0, 1:
		return bold + red + label + reset
	case 2:
		return yellow + label + reset
	default:
		return dim + label + reset
	}
}

func visibleLen(s string) int {
	n := 0
	inEscape := false
	for i := 0; i < len(s); i++ {
		if s[i] == '\033' {
			inEscape = true
			continue
		}
		if inEscape {
			if s[i] == 'm' {
				inEscape = false
			}
			continue
		}
		n++
	}
	return n
}

func countDigits(n int) int {
	if n == 0 {
		return 1
	}
	d := 0
	for n > 0 {
		d++
		n /= 10
	}
	return d
}

func clearScreen() {
	fmt.Print("\033[2J\033[H")
}
