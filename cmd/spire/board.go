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
	Alerts  []BoardBead
	Ready   []BoardBead
	Working []BoardBead
	Review  []BoardBead
	Merged  []BoardBead
	Blocked []BoardBead
}

type boardJSON struct {
	Alerts  []BoardBead `json:"alerts"`
	Ready   []BoardBead `json:"ready"`
	Working []BoardBead `json:"working"`
	Review  []BoardBead `json:"review"`
	Merged  []BoardBead `json:"merged"`
	Blocked []BoardBead `json:"blocked"`
}

// --- Board options (shared between static and TUI mode) ---

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
		flagLive bool
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
		case "--live":
			flagLive = true
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
			return fmt.Errorf("unknown flag: %s\nusage: spire board [--live] [--mine] [--ready] [--epic <id>] [--json] [--interval 5s]", args[i])
		}
	}

	if flagJSON {
		cols := fetchBoard(opts)
		out := boardJSON{
			Alerts:  nonNil(cols.Alerts),
			Ready:   nonNil(cols.Ready),
			Working: nonNil(cols.Working),
			Review:  nonNil(cols.Review),
			Merged:  nonNil(cols.Merged),
			Blocked: nonNil(cols.Blocked),
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if flagLive {
		return runBoardTUI(opts)
	}

	// Static one-shot.
	cols := fetchBoard(opts)
	printColumnarBoard(cols, 0)
	return nil
}

// --- Data fetching ---

func fetchBoard(opts boardOpts) boardColumns {
	// Fetch open + in_progress beads via store API.
	openBeads, _ := storeListBoardBeads(beads.IssueFilter{
		ExcludeStatus: []beads.Status{beads.StatusClosed},
	})

	// Fetch recently closed beads (last 24h) for the Merged column.
	closedBeads, _ := storeListBoardBeads(beads.IssueFilter{
		Status: statusPtr(beads.StatusClosed),
	})

	// Fetch blocked beads with blocker IDs.
	blockedBeads, _ := storeGetBlockedIssues(beads.WorkFilter{})

	identity, _ := detectIdentity("")
	cols := categorizeColumnsFromStore(openBeads, closedBeads, blockedBeads, identity)

	if opts.epic != "" {
		cols = filterEpic(cols, opts.epic)
	}
	if opts.mine {
		cols.Ready = nil
		cols.Working = filterOwned(cols.Working, identity)
		cols.Review = filterOwned(cols.Review, identity)
		cols.Blocked = filterOwned(cols.Blocked, identity)
	}
	if opts.ready {
		cols.Working = nil
		cols.Review = nil
		cols.Merged = nil
		cols.Blocked = nil
	}

	sortBeads(cols.Ready)
	sortBeads(cols.Working)
	sortBeads(cols.Review)
	sortBeads(cols.Merged)
	sortBeads(cols.Blocked)

	return cols
}

// --- Bubbletea TUI ---

type boardModel struct {
	opts     boardOpts
	cols     boardColumns
	width    int
	height   int
	lastTick time.Time
	quitting bool
}

type tickMsg time.Time

func tickCmd(interval time.Duration) tea.Cmd {
	return tea.Tick(interval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func runBoardTUI(opts boardOpts) error {
	m := boardModel{
		opts:     opts,
		cols:     fetchBoard(opts),
		lastTick: time.Now(),
	}
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
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
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tickMsg:
		m.cols = fetchBoard(m.opts)
		m.lastTick = time.Now()
		return m, tickCmd(m.opts.interval)
	}
	return m, nil
}

func (m boardModel) View() string {
	if m.quitting {
		return ""
	}

	colWidth := 30
	if m.width > 0 {
		// Fit columns to terminal width.
		activeCols := countActiveCols(m.cols)
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

	// Header.
	header := lipgloss.NewStyle().Bold(true).Render("Spire Board")
	ts := lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render(m.lastTick.Format("15:04:05"))
	s.WriteString(header + "  " + ts + "\n\n")

	// Alerts.
	if len(m.cols.Alerts) > 0 {
		sortBeads(m.cols.Alerts)
		alertStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))
		s.WriteString(alertStyle.Render(fmt.Sprintf("⚠ ALERTS (%d)", len(m.cols.Alerts))) + "\n")
		for _, a := range m.cols.Alerts {
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
	type col struct {
		name  string
		color lipgloss.Color
		beads []BoardBead
	}
	columns := []col{
		{"READY", lipgloss.Color("2"), m.cols.Ready},
		{"WORKING", lipgloss.Color("6"), m.cols.Working},
		{"REVIEW", lipgloss.Color("3"), m.cols.Review},
		{"MERGED", lipgloss.Color("5"), m.cols.Merged},
	}

	// Filter empty columns.
	var active []col
	for _, c := range columns {
		if len(c.beads) > 0 {
			active = append(active, c)
		}
	}

	if len(active) > 0 {
		// Render each column as a string.
		molCache := newMoleculeProgressCache()
		rendered := make([]string, len(active))
		for i, c := range active {
			var cb strings.Builder
			headerStyle := lipgloss.NewStyle().Bold(true).Foreground(c.color)
			cb.WriteString(headerStyle.Render(fmt.Sprintf("%s (%d)", c.name, len(c.beads))))
			cb.WriteString("\n")
			sepStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
			cb.WriteString(sepStyle.Render(strings.Repeat("─", min(colWidth, len(c.name)+4))))
			cb.WriteString("\n")

			maxShow := 15
			if m.height > 0 {
				maxShow = (m.height - 8) / 4 // 4 lines per card, leave room for header/footer
				if maxShow < 3 {
					maxShow = 3
				}
			}

			for j, b := range c.beads {
				if j >= maxShow {
					dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
					cb.WriteString(dimStyle.Render(fmt.Sprintf("  ... +%d more", len(c.beads)-maxShow)))
					cb.WriteString("\n")
					break
				}
				progress := ""
				if c.name == "WORKING" {
					progress = molCache.get(b.ID)
				}
				cb.WriteString(renderCardStr(b, c.color, colWidth, progress))
			}
			rendered[i] = cb.String()
		}

		// Join columns horizontally.
		s.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, addGaps(rendered, colWidth)...))
		s.WriteString("\n")
	}

	// Blocked.
	if len(m.cols.Blocked) > 0 {
		blockedStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))
		s.WriteString(blockedStyle.Render(fmt.Sprintf("BLOCKED (%d)", len(m.cols.Blocked))) + "\n")

		maxShow := 8
		for i, b := range m.cols.Blocked {
			if i >= maxShow {
				dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
				s.WriteString(dimStyle.Render(fmt.Sprintf("  ... +%d more", len(m.cols.Blocked)-maxShow)) + "\n")
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

	// Footer.
	s.WriteString("\n")
	footerStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	s.WriteString(footerStyle.Render("q to quit • refreshes every " + m.opts.interval.String()))

	return s.String()
}

// renderCardStr renders a single bead card as a multi-line string for a column.
func renderCardStr(b BoardBead, color lipgloss.Color, width int, progress string) string {
	titleWidth := width - 4
	if titleWidth < 10 {
		titleWidth = 10
	}

	typeStr := shortType(b.Type)
	if progress != "" {
		typeStr += " " + progress
	}

	var s strings.Builder
	s.WriteString(fmt.Sprintf("%s %s %s\n", priStr(b.Priority), b.ID, typeStr))
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
	n := 0
	if len(cols.Ready) > 0 {
		n++
	}
	if len(cols.Working) > 0 {
		n++
	}
	if len(cols.Review) > 0 {
		n++
	}
	if len(cols.Merged) > 0 {
		n++
	}
	return n
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

// --- Shared helpers (used by both static and TUI) ---

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
		return false
	}

	// Build a set of blocked IDs for fast lookup.
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

		switch b.Status {
		case "in_progress":
			c.Working = append(c.Working, b)
		case "open":
			// Skip beads already in the Blocked column.
			if !blockedIDs[b.ID] {
				c.Ready = append(c.Ready, b)
			}
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
			c.Merged = append(c.Merged, b)
		}
	}

	return c
}

func filterEpic(cols boardColumns, epicID string) boardColumns {
	match := func(b BoardBead) bool {
		return b.ID == epicID || b.Parent == epicID || strings.HasPrefix(b.ID, epicID+".")
	}
	return boardColumns{
		Alerts:  filterBeads(cols.Alerts, match),
		Ready:   filterBeads(cols.Ready, match),
		Working: filterBeads(cols.Working, match),
		Review:  filterBeads(cols.Review, match),
		Merged:  filterBeads(cols.Merged, match),
		Blocked: filterBeads(cols.Blocked, match),
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
		return beads[i].UpdatedAt > beads[j].UpdatedAt
	})
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
	if ts == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t, err = time.Parse("2006-01-02 15:04:05", ts)
		if err != nil {
			return ""
		}
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
	// Alerts.
	if len(cols.Alerts) > 0 {
		sortBeads(cols.Alerts)
		fmt.Printf("%s%s ALERTS (%d) %s\n", bold+red, "⚠", len(cols.Alerts), reset)
		for _, a := range cols.Alerts {
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
		{"WORKING", cyan, cols.Working},
		{"REVIEW", yellow, cols.Review},
		{"MERGED", magenta, cols.Merged},
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

		maxCards := 0
		for _, col := range active {
			if len(col.beads) > maxCards {
				maxCards = len(col.beads)
			}
		}

		molCache := newMoleculeProgressCache()
		for row := 0; row < maxCards*4; row++ {
			for i, col := range active {
				if i > 0 {
					fmt.Print("  ")
				}
				cardIdx := row / 4
				lineIdx := row % 4
				if cardIdx < len(col.beads) {
					progress := ""
					if col.header == "WORKING" {
						progress = molCache.get(col.beads[cardIdx].ID)
					}
					line := staticCardLine(col.beads[cardIdx], col.color, lineIdx, colWidth, progress)
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

	// Blocked.
	if len(cols.Blocked) > 0 {
		if len(active) > 0 {
			fmt.Println()
		}
		fmt.Printf("%s%sBLOCKED (%d)%s\n", bold, red, len(cols.Blocked), reset)
		for _, b := range cols.Blocked {
			blockers := blockingDepIDs(b)
			blockerStr := ""
			if len(blockers) > 0 {
				blockerStr = fmt.Sprintf(" %s← %s%s", dim, strings.Join(blockers, ", "), reset)
			}
			fmt.Printf("  %s %s %s%s\n", priorityStr(b.Priority), b.ID, truncate(b.Title, 40), blockerStr)
		}
	}
}

func staticCardLine(b BoardBead, color string, lineIdx, colWidth int, progress string) string {
	titleWidth := colWidth - 4
	if titleWidth < 10 {
		titleWidth = 10
	}
	switch lineIdx {
	case 0:
		typeStr := shortType(b.Type)
		if progress != "" {
			typeStr += " " + progress
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

// --- Molecule progress ---

// moleculeProgress returns the progress string "(N/M)" for a bead's workflow molecule.
// Returns "" if no molecule exists.
func moleculeProgress(beadID string) string {
	// Find molecule root by workflow:<beadID> label.
	mols, _ := storeListBeads(beads.IssueFilter{
		IDPrefix: "spi-",
		Labels:   []string{"workflow:" + beadID},
	})
	if len(mols) == 0 {
		return ""
	}

	// Get children (the step beads).
	children, err := storeGetChildren(mols[0].ID)
	if err != nil || len(children) == 0 {
		return ""
	}

	total := len(children)
	closed := 0
	for _, c := range children {
		if c.Status == "closed" {
			closed++
		}
	}

	return fmt.Sprintf("(%d/%d)", closed, total)
}

// moleculeProgressCache caches molecule progress lookups for one board render.
type moleculeProgressCache struct {
	cache map[string]string
}

func newMoleculeProgressCache() *moleculeProgressCache {
	return &moleculeProgressCache{cache: make(map[string]string)}
}

func (c *moleculeProgressCache) get(beadID string) string {
	if v, ok := c.cache[beadID]; ok {
		return v
	}
	v := moleculeProgress(beadID)
	c.cache[beadID] = v
	return v
}
