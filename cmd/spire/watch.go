package main

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/steveyegge/beads"
)

func cmdWatch(args []string) error {
	var epicID string
	interval := 5 * time.Second
	once := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--interval":
			if i+1 >= len(args) {
				return fmt.Errorf("--interval requires a value")
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil {
				return fmt.Errorf("--interval: invalid duration %q", args[i])
			}
			interval = d
		case "--once":
			once = true
		default:
			if strings.HasPrefix(args[i], "-") {
				return fmt.Errorf("unknown flag: %s\nusage: spire watch [<epic-id>] [--interval 5s] [--once]", args[i])
			}
			epicID = args[i]
		}
	}

	if once {
		return renderWatch(epicID)
	}

	// Live mode: clear screen, render, repeat.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	renderWatch(epicID) //nolint:errcheck

	for {
		select {
		case <-sigCh:
			fmt.Println()
			return nil
		case <-ticker.C:
			clearScreen()
			renderWatch(epicID) //nolint:errcheck
		}
	}
}

func renderWatch(epicID string) error {
	if epicID != "" {
		return renderEpicWatch(epicID)
	}
	return renderTowerWatch()
}

// renderTowerWatch shows all active work across the tower.
func renderTowerWatch() error {
	// Load all beads.
	allBeads, err := storeListBoardBeads(beads.IssueFilter{})
	if err != nil {
		return fmt.Errorf("watch: %w", err)
	}

	// Load agents.
	agentBeads, _ := storeListBoardBeads(beads.IssueFilter{
		Labels: []string{"agent"},
		Status: statusPtr(beads.StatusOpen),
	})

	// Count wizards.
	wizardCount := 0
	for _, ab := range agentBeads {
		for _, l := range ab.Labels {
			if strings.HasPrefix(l, "name:") {
				name := l[5:]
				if name != "steward" && name != "mayor" && name != "spi" && name != "awell" {
					wizardCount++
				}
			}
		}
	}

	// Categorize.
	var working, ready []BoardBead
	for _, b := range allBeads {
		// Skip noise.
		for _, l := range b.Labels {
			if strings.HasPrefix(l, "msg") || l == "template" || strings.HasPrefix(l, "agent") {
				goto skip
			}
		}
		switch b.Status {
		case "in_progress":
			working = append(working, b)
		case "open":
			if !hasBlockingDeps(b) {
				ready = append(ready, b)
			}
		}
	skip:
	}

	sortBeads(working)
	sortBeads(ready)

	// Header.
	fmt.Printf("%sTOWER STATUS%s — %d wizard(s), %d working, %d ready\n",
		bold, reset, wizardCount, len(working), len(ready))
	fmt.Printf("%sUpdated: %s%s\n", dim, time.Now().Format("15:04:05"), reset)
	fmt.Println()

	if len(working) > 0 {
		fmt.Printf("%s%sWORKING%s\n", bold, cyan, reset)
		for _, b := range working {
			owner := beadOwnerLabel(b)
			if owner == "" {
				owner = "unknown"
			}
			elapsed := timeAgo(b.UpdatedAt)
			fmt.Printf("  %s%-12s%s  %s %-12s %s  %s%s%s\n",
				cyan, owner, reset,
				priorityStr(b.Priority),
				b.ID,
				truncate(b.Title, 35),
				dim, elapsed, reset)
		}
		fmt.Println()
	}

	if len(ready) > 0 {
		// Show top 10 ready.
		show := ready
		if len(show) > 10 {
			show = show[:10]
		}
		fmt.Printf("%s%sREADY%s (%d total, showing top %d by priority)\n", bold, green, reset, len(ready), len(show))
		for _, b := range show {
			fmt.Printf("  %s %-12s %s\n",
				priorityStr(b.Priority),
				b.ID,
				truncate(b.Title, 45))
		}
		if len(ready) > 10 {
			fmt.Printf("  %s... and %d more%s\n", dim, len(ready)-10, reset)
		}
	}

	return nil
}

// renderEpicWatch shows progress for a specific epic and its children.
func renderEpicWatch(epicID string) error {
	// Load the epic.
	allBeads, err := storeListBoardBeads(beads.IssueFilter{})
	if err != nil {
		return fmt.Errorf("watch: %w", err)
	}

	// Also load closed beads.
	closedBeads, _ := storeListBoardBeads(beads.IssueFilter{
		Status: statusPtr(beads.StatusClosed),
	})

	// Find the epic.
	var epic *BoardBead
	for i := range allBeads {
		if allBeads[i].ID == epicID {
			epic = &allBeads[i]
			break
		}
	}
	if epic == nil {
		// Check closed.
		for i := range closedBeads {
			if closedBeads[i].ID == epicID {
				epic = &closedBeads[i]
				break
			}
		}
	}
	if epic == nil {
		return fmt.Errorf("epic %s not found", epicID)
	}

	// Collect children from all beads (open + closed).
	all := append(allBeads, closedBeads...)
	var children []BoardBead
	for _, b := range all {
		if b.Parent == epicID || strings.HasPrefix(b.ID, epicID+".") {
			children = append(children, b)
		}
	}

	// Deduplicate (a child might appear in both open and closed lists during transition).
	seen := make(map[string]bool)
	var deduped []BoardBead
	for _, b := range children {
		if !seen[b.ID] {
			seen[b.ID] = true
			deduped = append(deduped, b)
		}
	}
	children = deduped

	// Count states.
	done, working, blocked, ready := 0, 0, 0, 0
	for _, b := range children {
		switch b.Status {
		case "closed":
			done++
		case "in_progress":
			working++
		case "open":
			if hasBlockingDeps(b) {
				blocked++
			} else {
				ready++
			}
		}
	}
	total := len(children)

	// Header.
	fmt.Printf("%sEPIC: %s%s — %s (%d/%d done)\n",
		bold, epicID, reset, truncate(epic.Title, 50), done, total)
	fmt.Printf("%sUpdated: %s%s\n", dim, time.Now().Format("15:04:05"), reset)
	fmt.Println()

	// Progress bar.
	barWidth := 40
	filled := 0
	if total > 0 {
		filled = done * barWidth / total
	}
	fmt.Printf("  [%s%s%s%s] %d/%d\n",
		green, strings.Repeat("█", filled), reset,
		strings.Repeat("░", barWidth-filled),
		done, total)
	fmt.Println()

	// Children list with status icons.
	for _, b := range children {
		icon := ""
		detail := ""

		switch b.Status {
		case "closed":
			icon = green + "✓" + reset
			detail = fmt.Sprintf("%smerged%s  %s", green, reset, timeAgo(b.UpdatedAt))
		case "in_progress":
			owner := beadOwnerLabel(b)
			if owner == "" {
				owner = "wizard"
			}
			elapsed := timeAgo(b.UpdatedAt)
			icon = cyan + "◐" + reset
			detail = fmt.Sprintf("%s%s%s  %s", cyan, owner, reset, elapsed)
		case "open":
			if hasBlockingDeps(b) {
				blockers := blockingDepIDs(b)
				icon = dim + "○" + reset
				detail = fmt.Sprintf("%sblocked by %s%s", dim, strings.Join(blockers, ", "), reset)
			} else {
				icon = yellow + "○" + reset
				detail = fmt.Sprintf("%sready%s", yellow, reset)
			}
		}

		fmt.Printf("  %s %s %-12s %s  %s\n",
			icon, priorityStr(b.Priority), b.ID, truncate(b.Title, 30), detail)
	}

	return nil
}
