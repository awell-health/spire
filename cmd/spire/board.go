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
	ID              string       `json:"id"`
	Title           string       `json:"title"`
	Description     string       `json:"description"`
	Status          string       `json:"status"`
	Priority        int          `json:"priority"`
	Type            string       `json:"issue_type"`
	Owner           string       `json:"owner"`
	CreatedAt       string       `json:"created_at"`
	UpdatedAt       string       `json:"updated_at"`
	Labels          []string     `json:"labels"`
	Parent          string       `json:"parent"`
	Dependencies    []BoardDep   `json:"dependencies"`
	DependencyCount int          `json:"dependency_count"`
	DependentCount  int          `json:"dependent_count"`
}

type BoardDep struct {
	IssueID     string `json:"issue_id"`
	DependsOnID string `json:"depends_on_id"`
	Type        string `json:"type"`
}

// boardSections holds categorized beads for display.
type boardSections struct {
	Review  []BoardBead // in_progress, claimed by current user
	Ready   []BoardBead // open with no blocking deps
	Agents  []BoardBead // in_progress, claimed by someone else
	Blocked []BoardBead // open with blocking deps
}

// boardJSON is the JSON output structure.
type boardJSON struct {
	Review  []BoardBead `json:"review"`
	Ready   []BoardBead `json:"ready"`
	Agents  []BoardBead `json:"agents"`
	Blocked []BoardBead `json:"blocked"`
}

func cmdBoard(args []string) error {
	if err := requireDolt(); err != nil {
		return err
	}

	// Parse flags
	var (
		flagMine  bool
		flagReady bool
		flagJSON  bool
	)
	for _, arg := range args {
		switch arg {
		case "--mine":
			flagMine = true
		case "--ready":
			flagReady = true
		case "--json":
			flagJSON = true
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire board [--mine] [--ready] [--json]", arg)
		}
	}

	// Single call to bd list --json
	var beads []BoardBead
	if err := bdJSON(&beads, "list"); err != nil {
		return fmt.Errorf("board: %w", err)
	}

	// Detect current user identity for --mine and section assignment
	identity, _ := detectIdentity("")

	// Categorize beads
	sections := categorizeBeads(beads, identity)

	// Apply filters
	if flagMine {
		// Only show beads assigned to me or waiting for my review
		sections.Ready = nil
		sections.Agents = nil
		sections.Blocked = filterOwned(sections.Blocked, identity)
	}
	if flagReady {
		sections.Review = nil
		sections.Agents = nil
		sections.Blocked = nil
	}

	// Sort each section by priority (ascending), then by updated_at (most recent first)
	sortBeads(sections.Review)
	sortBeads(sections.Ready)
	sortBeads(sections.Agents)
	sortBeads(sections.Blocked)

	// Output
	if flagJSON {
		out := boardJSON{
			Review:  nonNil(sections.Review),
			Ready:   nonNil(sections.Ready),
			Agents:  nonNil(sections.Agents),
			Blocked: nonNil(sections.Blocked),
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	printBoard(sections)
	return nil
}

func categorizeBeads(beads []BoardBead, identity string) boardSections {
	var s boardSections

	for _, b := range beads {
		switch b.Status {
		case "in_progress":
			claimedBy := beadOwnerLabel(b)
			if claimedBy == "" {
				claimedBy = b.Owner
			}
			if isCurrentUser(claimedBy, identity) {
				s.Review = append(s.Review, b)
			} else {
				s.Agents = append(s.Agents, b)
			}

		case "open":
			if hasBlockingDeps(b) {
				s.Blocked = append(s.Blocked, b)
			} else {
				s.Ready = append(s.Ready, b)
			}

		// Skip closed beads entirely
		}
	}

	return s
}

// beadOwnerLabel extracts the owner:<name> label value.
func beadOwnerLabel(b BoardBead) string {
	for _, l := range b.Labels {
		if strings.HasPrefix(l, "owner:") {
			return l[6:]
		}
	}
	return ""
}

// isCurrentUser checks if a claimedBy value matches the current identity.
func isCurrentUser(claimedBy, identity string) bool {
	if claimedBy == "" || identity == "" {
		return false
	}
	// Exact match or prefix match (e.g., identity "spi" matches owner "spi")
	return claimedBy == identity ||
		strings.EqualFold(claimedBy, identity) ||
		strings.Contains(claimedBy, identity)
}

// hasBlockingDeps returns true if the bead has unresolved blocking dependencies.
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

// blockingDepIDs returns the IDs of beads that block this one.
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
		// More recently updated first
		return beads[i].UpdatedAt > beads[j].UpdatedAt
	})
}

func nonNil(beads []BoardBead) []BoardBead {
	if beads == nil {
		return []BoardBead{}
	}
	return beads
}

// --- Terminal output ---

// ANSI codes
const (
	bold    = "\033[1m"
	dim     = "\033[2m"
	red     = "\033[31m"
	yellow  = "\033[33m"
	green   = "\033[32m"
	cyan    = "\033[36m"
	reset   = "\033[0m"
)

func printBoard(s boardSections) {
	any := false

	if len(s.Review) > 0 {
		printSection("REVIEW", s.Review, renderReview)
		any = true
	}
	if len(s.Ready) > 0 {
		if any {
			fmt.Println()
		}
		printSection("READY", s.Ready, renderReady)
		any = true
	}
	if len(s.Agents) > 0 {
		if any {
			fmt.Println()
		}
		printSection("AGENTS", s.Agents, renderAgents)
		any = true
	}
	if len(s.Blocked) > 0 {
		if any {
			fmt.Println()
		}
		printSection("BLOCKED", s.Blocked, renderBlocked)
		any = true
	}

	if !any {
		fmt.Printf("%s(no work items)%s\n", dim, reset)
	}
}

type renderFunc func(b BoardBead) string

func printSection(name string, beads []BoardBead, render renderFunc) {
	fmt.Printf("%s%s (%d)%s\n", bold, name, len(beads), reset)
	for _, b := range beads {
		fmt.Printf("  %s\n", render(b))
	}
}

func renderReview(b BoardBead) string {
	claimedBy := beadOwnerLabel(b)
	if claimedBy == "" {
		claimedBy = b.Owner
	}
	ago := timeAgo(b.UpdatedAt)
	return fmt.Sprintf("%-12s %s  %-5s  %-40s  %sin_progress%s  claimed by %s  %s%s%s",
		b.ID, priorityStr(b.Priority), shortType(b.Type),
		truncate(b.Title, 40),
		cyan, reset,
		claimedBy,
		dim, ago, reset)
}

func renderReady(b BoardBead) string {
	return fmt.Sprintf("%-12s %s  %-5s  %-40s  %sunblocked%s",
		b.ID, priorityStr(b.Priority), shortType(b.Type),
		truncate(b.Title, 40),
		green, reset)
}

func renderAgents(b BoardBead) string {
	claimedBy := beadOwnerLabel(b)
	if claimedBy == "" {
		claimedBy = b.Owner
	}
	ago := timeAgo(b.UpdatedAt)
	return fmt.Sprintf("%-12s %s  %-5s  %-40s  %sin_progress%s  claimed by %s  %s%s%s",
		b.ID, priorityStr(b.Priority), shortType(b.Type),
		truncate(b.Title, 40),
		yellow, reset,
		claimedBy,
		dim, ago, reset)
}

func renderBlocked(b BoardBead) string {
	blockers := blockingDepIDs(b)
	blockerStr := ""
	if len(blockers) > 0 {
		blockerStr = fmt.Sprintf("  blocked by %s", strings.Join(blockers, ", "))
	}
	return fmt.Sprintf("%-12s %s  %-5s  %-40s  %s%sblocked%s%s",
		b.ID, priorityStr(b.Priority), shortType(b.Type),
		truncate(b.Title, 40),
		dim, red, reset,
		blockerStr)
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
