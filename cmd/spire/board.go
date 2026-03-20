package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
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

// boardColumns holds beads categorized into lifecycle columns.
type boardColumns struct {
	Alerts  []BoardBead // beads with "alert" label — things that need attention
	Ready   []BoardBead // open, unblocked, unowned
	Working []BoardBead // in_progress (wizards working)
	Review  []BoardBead // in_progress, has branch pushed (artificer reviewing)
	Merged  []BoardBead // recently closed (last 24h)
	Blocked []BoardBead // open with blocking deps
}

// boardJSON is the JSON output structure.
type boardJSON struct {
	Alerts  []BoardBead `json:"alerts"`
	Ready   []BoardBead `json:"ready"`
	Working []BoardBead `json:"working"`
	Review  []BoardBead `json:"review"`
	Merged  []BoardBead `json:"merged"`
	Blocked []BoardBead `json:"blocked"`
}

func cmdBoard(args []string) error {
	if err := requireDolt(); err != nil {
		return err
	}

	var (
		flagMine  bool
		flagReady bool
		flagJSON  bool
		flagEpic  string
	)
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--mine":
			flagMine = true
		case "--ready":
			flagReady = true
		case "--json":
			flagJSON = true
		case "--epic":
			if i+1 >= len(args) {
				return fmt.Errorf("--epic requires a bead ID")
			}
			i++
			flagEpic = args[i]
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire board [--mine] [--ready] [--epic <id>] [--json]", args[i])
		}
	}

	// Fetch all beads (open + recently closed)
	var allBeads []BoardBead
	if err := bdJSON(&allBeads, "list"); err != nil {
		return fmt.Errorf("board: %w", err)
	}

	// Also fetch recently closed beads
	var closedBeads []BoardBead
	_ = bdJSON(&closedBeads, "list", "--status=closed")

	identity, _ := detectIdentity("")

	// Categorize into columns
	cols := categorizeColumns(allBeads, closedBeads, identity)

	// Apply filters
	if flagEpic != "" {
		cols = filterEpic(cols, flagEpic)
	}
	if flagMine {
		cols.Ready = nil
		cols.Working = filterOwned(cols.Working, identity)
		cols.Review = filterOwned(cols.Review, identity)
		cols.Blocked = filterOwned(cols.Blocked, identity)
	}
	if flagReady {
		cols.Working = nil
		cols.Review = nil
		cols.Merged = nil
		cols.Blocked = nil
	}

	// Sort each column by priority then recency
	sortBeads(cols.Ready)
	sortBeads(cols.Working)
	sortBeads(cols.Review)
	sortBeads(cols.Merged)
	sortBeads(cols.Blocked)

	if flagJSON {
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

	printColumnarBoard(cols)
	return nil
}

func categorizeColumns(beads, closedBeads []BoardBead, identity string) boardColumns {
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

	for _, b := range beads {
		// Alerts always surface, even if they'd otherwise be skipped.
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
			if hasBlockingDeps(b) {
				c.Blocked = append(c.Blocked, b)
			} else {
				c.Ready = append(c.Ready, b)
			}
		}
	}

	// Recently closed → Merged column (last 24h)
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, b := range closedBeads {
		if skip(b) {
			continue
		}
		t, err := time.Parse(time.RFC3339, b.UpdatedAt)
		if err != nil {
			// Try alternate format
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

// --- Columnar terminal output ---

// ANSI codes
const (
	bold   = "\033[1m"
	dim    = "\033[2m"
	red    = "\033[31m"
	yellow = "\033[33m"
	green  = "\033[32m"
	cyan   = "\033[36m"
	magenta = "\033[35m"
	reset  = "\033[0m"
)

// card is a rendered bead for display in a column.
type card struct {
	lines []string
}

func printColumnarBoard(cols boardColumns) {
	// Alerts banner — always on top, full width, high visibility.
	if len(cols.Alerts) > 0 {
		sortBeads(cols.Alerts)
		fmt.Printf("%s%s ALERTS (%d) %s\n", bold+red, "⚠", len(cols.Alerts), reset)
		for _, a := range cols.Alerts {
			// Extract alert type from label.
			alertType := ""
			refBead := ""
			for _, l := range a.Labels {
				if strings.HasPrefix(l, "alert:") {
					alertType = l[6:]
				}
				if strings.HasPrefix(l, "ref:") {
					refBead = l[4:]
				}
			}
			typeStr := ""
			if alertType != "" {
				typeStr = fmt.Sprintf("[%s] ", alertType)
			}
			refStr := ""
			if refBead != "" {
				refStr = fmt.Sprintf(" %s→ %s%s", dim, refBead, reset)
			}
			fmt.Printf("  %s %s%s%s%s\n",
				priorityStr(a.Priority),
				typeStr,
				a.Title,
				refStr,
				"")
		}
		fmt.Println()
	}

	// Build card lists for each column.
	readyCards := renderCards(cols.Ready, renderReadyCard)
	workingCards := renderCards(cols.Working, renderWorkingCard)
	reviewCards := renderCards(cols.Review, renderReviewCard)
	mergedCards := renderCards(cols.Merged, renderMergedCard)

	// Column definitions.
	type column struct {
		header string
		color  string
		count  int
		cards  []card
	}

	columns := []column{
		{header: "READY", color: green, count: len(cols.Ready), cards: readyCards},
		{header: "WORKING", color: cyan, count: len(cols.Working), cards: workingCards},
		{header: "REVIEW", color: yellow, count: len(cols.Review), cards: reviewCards},
		{header: "MERGED", color: magenta, count: len(cols.Merged), cards: mergedCards},
	}

	// Filter out empty columns.
	var active []column
	for _, col := range columns {
		if col.count > 0 {
			active = append(active, col)
		}
	}

	if len(active) == 0 && len(cols.Blocked) == 0 {
		fmt.Printf("%s(no work items)%s\n", dim, reset)
		return
	}

	// Column width.
	colWidth := 30
	if len(active) <= 2 {
		colWidth = 38
	}
	if len(active) == 1 {
		colWidth = 50
	}

	// Print headers.
	if len(active) > 0 {
		for i, col := range active {
			if i > 0 {
				fmt.Print("  ")
			}
			header := fmt.Sprintf("%s%s%s (%d)%s", bold, col.color, col.header, col.count, reset)
			fmt.Print(header)
			// Pad to column width (accounting for ANSI codes).
			visible := len(col.header) + 2 + countDigits(col.count) + 1 // "HEADER (N)"
			pad := colWidth - visible
			if pad > 0 {
				fmt.Print(strings.Repeat(" ", pad))
			}
		}
		fmt.Println()

		// Separator line.
		for i, col := range active {
			if i > 0 {
				fmt.Print("  ")
			}
			sep := strings.Repeat("─", min(colWidth, len(col.header)+4))
			fmt.Printf("%s%s%s", dim, sep, reset)
			pad := colWidth - min(colWidth, len(col.header)+4)
			if pad > 0 {
				fmt.Print(strings.Repeat(" ", pad))
			}
		}
		fmt.Println()

		// Print cards row by row. Each card takes 3 lines + 1 blank.
		maxCards := 0
		for _, col := range active {
			if len(col.cards) > maxCards {
				maxCards = len(col.cards)
			}
		}

		linesPerCard := 4 // 3 content lines + 1 blank
		maxRows := maxCards * linesPerCard

		for row := 0; row < maxRows; row++ {
			for i, col := range active {
				if i > 0 {
					fmt.Print("  ")
				}

				cardIdx := row / linesPerCard
				lineIdx := row % linesPerCard

				if cardIdx < len(col.cards) && lineIdx < len(col.cards[cardIdx].lines) {
					line := col.cards[cardIdx].lines[lineIdx]
					// Pad to column width.
					visible := visibleLen(line)
					fmt.Print(line)
					pad := colWidth - visible
					if pad > 0 {
						fmt.Print(strings.Repeat(" ", pad))
					}
				} else {
					fmt.Print(strings.Repeat(" ", colWidth))
				}
			}
			fmt.Println()
		}
	}

	// Blocked beads shown below as a compact list.
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
			fmt.Printf("  %s %s %s%s\n",
				priorityStr(b.Priority),
				b.ID,
				truncate(b.Title, 40),
				blockerStr)
		}
	}
}

// --- Card renderers ---

func renderCards(beads []BoardBead, render func(BoardBead) card) []card {
	cards := make([]card, len(beads))
	for i, b := range beads {
		cards[i] = render(b)
	}
	return cards
}

func renderReadyCard(b BoardBead) card {
	return card{lines: []string{
		fmt.Sprintf("%s %s %s", priorityStr(b.Priority), b.ID, shortType(b.Type)),
		fmt.Sprintf("  %s", truncate(b.Title, 26)),
		fmt.Sprintf("  %s%s%s", dim, timeAgo(b.CreatedAt), reset),
	}}
}

func renderWorkingCard(b BoardBead) card {
	owner := beadOwnerLabel(b)
	if owner == "" {
		owner = b.Owner
	}
	elapsed := timeAgo(b.UpdatedAt)
	return card{lines: []string{
		fmt.Sprintf("%s %s %s", priorityStr(b.Priority), b.ID, shortType(b.Type)),
		fmt.Sprintf("  %s", truncate(b.Title, 26)),
		fmt.Sprintf("  %s%s%s %s%s%s", cyan, owner, reset, dim, elapsed, reset),
	}}
}

func renderReviewCard(b BoardBead) card {
	return card{lines: []string{
		fmt.Sprintf("%s %s %s", priorityStr(b.Priority), b.ID, shortType(b.Type)),
		fmt.Sprintf("  %s", truncate(b.Title, 26)),
		fmt.Sprintf("  %s%sartificer reviewing%s", yellow, dim, reset),
	}}
}

func renderMergedCard(b BoardBead) card {
	ago := timeAgo(b.UpdatedAt)
	return card{lines: []string{
		fmt.Sprintf("%s %s %s", priorityStr(b.Priority), b.ID, shortType(b.Type)),
		fmt.Sprintf("  %s", truncate(b.Title, 26)),
		fmt.Sprintf("  %s%s%s", dim, ago, reset),
	}}
}

// --- Helpers ---

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

func hasBlockingDeps(b BoardBead) bool {
	if b.DependencyCount > 0 {
		return true
	}
	for _, d := range b.Dependencies {
		if d.Type == "blocks" {
			return true
		}
	}
	return false
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

func priorityStr(p int) string {
	label := fmt.Sprintf("P%d", p)
	switch p {
	case 0:
		return bold + red + label + reset
	case 1:
		return bold + red + label + reset
	case 2:
		return yellow + label + reset
	case 3:
		return dim + label + reset
	case 4:
		return dim + label + reset
	default:
		return label
	}
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

// visibleLen returns the visible length of a string, ignoring ANSI escape codes.
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

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
