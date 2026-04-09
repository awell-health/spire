package board

import (
	"fmt"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// WatchDeps holds external dependencies needed by the watch views.
type WatchDeps struct {
	// LoadWizardRegistry returns the current wizard registry.
	LoadWizardRegistry func() []LocalAgent
	// ProcessAlive checks if a PID is alive.
	ProcessAlive func(pid int) bool
}

// RenderWatch dispatches to epic or tower watch.
func RenderWatch(epicID string, deps WatchDeps) error {
	if epicID != "" {
		return RenderEpicWatch(epicID)
	}
	return RenderTowerWatch(deps)
}

// RenderTowerWatch shows all active work across the tower.
func RenderTowerWatch(deps WatchDeps) error {
	allBeads, err := store.ListBoardBeads(beads.IssueFilter{
		ExcludeStatus: []beads.Status{beads.StatusClosed, beads.StatusDeferred},
	})
	if err != nil {
		return fmt.Errorf("watch: %w", err)
	}

	blockedBeads, _ := store.GetBlockedIssues(beads.WorkFilter{})
	blockedIDs := make(map[string]bool, len(blockedBeads))
	for _, b := range blockedBeads {
		blockedIDs[b.ID] = true
	}

	aliveWizards := make(map[string]bool)
	if deps.LoadWizardRegistry != nil {
		agents := deps.LoadWizardRegistry()
		for _, w := range agents {
			if w.PID > 0 && deps.ProcessAlive != nil && deps.ProcessAlive(w.PID) {
				aliveWizards[w.Name] = true
			}
		}
	}

	agentBeads, _ := store.ListBoardBeads(beads.IssueFilter{
		Labels: []string{"agent"},
		Status: store.StatusPtr(beads.StatusOpen),
	})

	wizardCount := 0
	for _, ab := range agentBeads {
		for _, l := range ab.Labels {
			if strings.HasPrefix(l, "name:") {
				name := l[5:]
				if name != "steward" && name != "mayor" && name != "spi" && name != "awell" {
					if aliveWizards[name] {
						wizardCount++
					}
				}
			}
		}
	}

	var working, stale, ready []BoardBead
	for _, b := range allBeads {
		for _, l := range b.Labels {
			if strings.HasPrefix(l, "msg") || l == "template" || strings.HasPrefix(l, "agent") {
				goto skip
			}
		}
		switch b.Status {
		case "in_progress":
			owner := BeadOwnerLabel(b)
			if owner != "" && !aliveWizards[owner] {
				stale = append(stale, b)
			} else {
				working = append(working, b)
			}
		case "open":
			if !blockedIDs[b.ID] {
				ready = append(ready, b)
			}
		}
	skip:
	}

	SortBeads(working)
	SortBeads(stale)
	SortBeads(ready)

	fmt.Printf("%sTOWER STATUS%s — %d wizard(s), %d working, %d ready\n",
		Bold, Reset, wizardCount, len(working), len(ready))
	fmt.Printf("%sUpdated: %s  (Ctrl-C to exit)%s\n", Dim, time.Now().Format("15:04:05"), Reset)
	fmt.Println()

	if len(stale) > 0 {
		fmt.Printf("%s%sSTALE%s %s(owner process dead)%s\n", Bold, Red, Reset, Dim, Reset)
		for _, b := range stale {
			owner := BeadOwnerLabel(b)
			if owner == "" {
				owner = "unknown"
			}
			fmt.Printf("  %s%-12s%s  %s %-12s %s\n",
				Red, owner, Reset,
				PriorityStr(b.Priority),
				b.ID,
				Truncate(b.Title, 35))
		}
		fmt.Printf("  %sRun 'spire dismiss --all' to clean up%s\n", Dim, Reset)
		fmt.Println()
	}

	if len(working) > 0 {
		fmt.Printf("%s%sWORKING%s\n", Bold, Cyan, Reset)
		for _, b := range working {
			owner := BeadOwnerLabel(b)
			if owner == "" {
				owner = "unknown"
			}
			elapsed := TimeAgo(b.UpdatedAt)
			fmt.Printf("  %s%-12s%s  %s %-12s %s  %s%s%s\n",
				Cyan, owner, Reset,
				PriorityStr(b.Priority),
				b.ID,
				Truncate(b.Title, 35),
				Dim, elapsed, Reset)

			// Show DAG progress inline.
			dag := FetchDAGProgress(b.ID)
			if dag != nil && len(dag.Steps) > 0 {
				fmt.Printf("    %s\n", RenderPipelineCompactANSI(dag.Steps))
			}
			if dag != nil && len(dag.Reviews) > 0 {
				fmt.Printf("    %sreview:%s %s\n", Dim, Reset, RenderReviewSummaryANSI(dag.Reviews))
			}
		}
		fmt.Println()
	}

	if len(ready) > 0 {
		show := ready
		if len(show) > 10 {
			show = show[:10]
		}
		fmt.Printf("%s%sREADY%s (%d total, showing top %d by priority)\n", Bold, Green, Reset, len(ready), len(show))
		for _, b := range show {
			fmt.Printf("  %s %-12s %s\n",
				PriorityStr(b.Priority),
				b.ID,
				Truncate(b.Title, 45))
		}
		if len(ready) > 10 {
			fmt.Printf("  %s... and %d more%s\n", Dim, len(ready)-10, Reset)
		}
	}

	return nil
}

// RenderEpicWatch shows progress for a specific epic and its children.
func RenderEpicWatch(epicID string) error {
	allBeads, err := store.ListBoardBeads(beads.IssueFilter{
		ExcludeStatus: []beads.Status{beads.StatusClosed, beads.StatusDeferred},
	})
	if err != nil {
		return fmt.Errorf("watch: %w", err)
	}

	closedBeads, _ := store.ListBoardBeads(beads.IssueFilter{
		Status: store.StatusPtr(beads.StatusClosed),
	})

	blockedBeads, _ := store.GetBlockedIssues(beads.WorkFilter{})
	blockedMap := make(map[string]BoardBead, len(blockedBeads))
	for _, b := range blockedBeads {
		blockedMap[b.ID] = b
	}

	var epic *BoardBead
	for i := range allBeads {
		if allBeads[i].ID == epicID {
			epic = &allBeads[i]
			break
		}
	}
	if epic == nil {
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

	all := append(allBeads, closedBeads...)
	var children []BoardBead
	for _, b := range all {
		if b.Parent == epicID || strings.HasPrefix(b.ID, epicID+".") {
			children = append(children, b)
		}
	}

	seen := make(map[string]bool)
	var deduped []BoardBead
	for _, b := range children {
		if !seen[b.ID] {
			seen[b.ID] = true
			deduped = append(deduped, b)
		}
	}
	children = deduped

	// Filter out internal DAG beads for counting.
	done, working, blocked, ready := 0, 0, 0, 0
	realTotal := 0
	for _, b := range children {
		if store.IsStepBoardBead(b) || store.IsAttemptBoardBead(b) || store.IsReviewRoundBoardBead(b) {
			continue
		}
		realTotal++
		switch b.Status {
		case "closed":
			done++
		case "in_progress":
			working++
		case "open":
			if _, ok := blockedMap[b.ID]; ok {
				blocked++
			} else {
				ready++
			}
		}
	}
	total := realTotal

	fmt.Printf("%sEPIC: %s%s — %s (%d/%d done)\n",
		Bold, epicID, Reset, Truncate(epic.Title, 50), done, total)
	fmt.Printf("%sUpdated: %s%s\n", Dim, time.Now().Format("15:04:05"), Reset)
	fmt.Println()

	barWidth := 40
	filled := 0
	if total > 0 {
		filled = done * barWidth / total
	}
	fmt.Printf("  [%s%s%s%s] %d/%d\n",
		Green, strings.Repeat("█", filled), Reset,
		strings.Repeat("░", barWidth-filled),
		done, total)
	fmt.Println()

	for _, b := range children {
		// Skip internal DAG beads (step, attempt, review).
		if store.IsStepBoardBead(b) || store.IsAttemptBoardBead(b) || store.IsReviewRoundBoardBead(b) {
			continue
		}

		icon := ""
		detail := ""

		switch b.Status {
		case "closed":
			icon = Green + "✓" + Reset
			detail = fmt.Sprintf("%smerged%s  %s", Green, Reset, TimeAgo(b.UpdatedAt))
		case "in_progress":
			owner := BeadOwnerLabel(b)
			if owner == "" {
				owner = "wizard"
			}
			elapsed := TimeAgo(b.UpdatedAt)
			icon = Cyan + "◐" + Reset
			detail = fmt.Sprintf("%s%s%s  %s", Cyan, owner, Reset, elapsed)
		case "open":
			if bb, ok := blockedMap[b.ID]; ok {
				blockers := BlockingDepIDs(bb)
				icon = Dim + "○" + Reset
				detail = fmt.Sprintf("%sblocked by %s%s", Dim, strings.Join(blockers, ", "), Reset)
			} else {
				icon = Yellow + "○" + Reset
				detail = fmt.Sprintf("%sready%s", Yellow, Reset)
			}
		}

		fmt.Printf("  %s %s %-12s %s  %s\n",
			icon, PriorityStr(b.Priority), b.ID, Truncate(b.Title, 30), detail)

		// Show DAG pipeline for in-progress children.
		if b.Status == "in_progress" {
			dag := FetchDAGProgress(b.ID)
			if dag != nil && len(dag.Steps) > 0 {
				fmt.Printf("    %s\n", RenderPipelineCompactANSI(dag.Steps))
			}
			if dag != nil && dag.Attempt != nil {
				fmt.Printf("    %sattempt:%s %s\n", Dim, Reset, RenderAttemptANSI(dag.Attempt))
			}
			if dag != nil && len(dag.Reviews) > 0 {
				fmt.Printf("    %sreview:%s %s\n", Dim, Reset, RenderReviewSummaryANSI(dag.Reviews))
			}
		}
	}

	return nil
}
