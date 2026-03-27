package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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

// --- Shared board helpers ---

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

// --- ANSI codes for static output (used by watch.go, roster.go, summon.go, board_actions.go) ---

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

func clearScreen() {
	fmt.Print("\033[2J\033[H")
}
