package board

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/steveyegge/beads"

	"github.com/awell-health/spire/pkg/dolt"
	"github.com/awell-health/spire/pkg/store"
)

// Inspector tab constants.
const (
	InspectorTabDetails = 0
	InspectorTabLogs    = 1
)

// InspectorData holds the fetched detail data for the inspector pane.
type InspectorData struct {
	Bead        BoardBead
	Phase       string
	Comments    []*beads.Comment
	Children    []Bead
	ChildPhases map[string]string                    // phase for in-progress children
	Deps        []*beads.IssueWithDependencyMetadata // what this bead depends on
	Dependents  []*beads.IssueWithDependencyMetadata // what depends on this bead
	DAG         *DAGProgress
	Messages    []BoardBead // messages referencing this bead
	DesignBeads []BoardBead // design beads linked via discovered-from deps
	LogContent  string      // cached wizard log content (loaded in FetchInspectorData, not View)
}

// FetchInspectorData loads all detail data for a bead from the store.
func FetchInspectorData(b BoardBead) InspectorData {
	data := InspectorData{
		Bead:  b,
		Phase: GetBoardBeadPhase(b),
	}

	if comments, err := store.GetComments(b.ID); err == nil {
		data.Comments = comments
	}
	if children, err := store.GetChildren(b.ID); err == nil {
		// Filter out internal beads (step, attempt, review-round).
		var visible []Bead
		for _, c := range children {
			if store.IsStepBead(c) || store.IsAttemptBead(c) || store.IsReviewRoundBead(c) {
				continue
			}
			visible = append(visible, c)
		}
		data.Children = visible
		data.ChildPhases = make(map[string]string)
		for _, c := range visible {
			if c.Status == "in_progress" {
				data.ChildPhases[c.ID] = GetPhase(c)
			}
		}
	}
	if deps, err := store.GetDepsWithMeta(b.ID); err == nil {
		data.Deps = deps
	}
	if dependents, err := store.GetDependentsWithMeta(b.ID); err == nil {
		data.Dependents = dependents
	}
	data.DAG = FetchDAGProgress(b.ID)

	// Design beads: look through deps for discovered-from links.
	if data.Deps != nil {
		for _, dep := range data.Deps {
			if string(dep.DependencyType) == "discovered-from" {
				if bb, err := store.GetBead(dep.ID); err == nil {
					data.DesignBeads = append(data.DesignBeads, BoardBead{
						ID:          bb.ID,
						Title:       bb.Title,
						Description: bb.Description,
						Status:      bb.Status,
						Priority:    bb.Priority,
						Type:        bb.Type,
						Labels:      bb.Labels,
					})
				}
			}
		}
	}

	// Messages: beads referencing this bead via ref: and msg labels.
	if msgs, err := store.ListBoardBeads(beads.IssueFilter{
		Labels: []string{"msg", "ref:" + b.ID},
	}); err == nil {
		data.Messages = msgs
	}

	// Wizard log content: read and cache here (not in View).
	wizardName := "wizard-" + b.ID
	logDir := filepath.Join(dolt.GlobalDir(), "wizards")
	candidates := []string{
		filepath.Join(logDir, wizardName+".log"),
		filepath.Join(logDir, wizardName+"-fix.log"),
	}
	for _, path := range candidates {
		if content, err := os.ReadFile(path); err == nil {
			data.LogContent = string(content)
			break
		}
	}

	return data
}

// RenderInspector renders the full inspector view for a bead.
func RenderInspector(data InspectorData, width, height, scrollOffset int) string {
	if width < 40 {
		width = 40
	}
	contentWidth := width - 4
	if contentWidth > 100 {
		contentWidth = 100
	}

	var lines []string

	// Header bar.
	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	lines = append(lines, headerStyle.Render("INSPECTOR")+"  "+dimStyle.Render("Esc to close"))
	lines = append(lines, strings.Repeat("─", Min(contentWidth, 60)))

	b := data.Bead

	// Title + ID.
	titleStyle := lipgloss.NewStyle().Bold(true)
	lines = append(lines, titleStyle.Render(b.ID)+"  "+PriStr(b.Priority)+"  "+ShortType(b.Type))
	lines = append(lines, titleStyle.Render(b.Title))
	lines = append(lines, "")

	// Status / Phase / Owner.
	statusLine := "Status: " + renderStatus(b.Status)
	if data.Phase != "" {
		statusLine += "  Phase: " + lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Render(data.Phase)
	}
	lines = append(lines, statusLine)

	owner := BeadOwnerLabel(b)
	if owner != "" {
		lines = append(lines, "Owner: "+lipgloss.NewStyle().Foreground(lipgloss.Color("5")).Render(owner))
	}
	if b.Parent != "" {
		lines = append(lines, "Parent: "+dimStyle.Render(b.Parent))
	}
	timeParts := []string{}
	if b.CreatedAt != "" {
		timeParts = append(timeParts, "Created: "+formatInspectorTime(b.CreatedAt))
	}
	if b.UpdatedAt != "" {
		timeParts = append(timeParts, "Updated: "+formatInspectorTime(b.UpdatedAt))
	}
	if len(timeParts) > 0 {
		lines = append(lines, dimStyle.Render(strings.Join(timeParts, "  ")))
	}

	// DAG pipeline.
	if data.DAG != nil && len(data.DAG.Steps) > 0 {
		lines = append(lines, "")
		lines = append(lines, sectionHeader("Pipeline"))
		lines = append(lines, "  "+RenderPipelineLipgloss(b.ID))
		if data.DAG.Attempt != nil {
			a := data.DAG.Attempt
			parts := []string{lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Render(a.Agent)}
			if a.Model != "" {
				parts = append(parts, dimStyle.Render(a.Model))
			}
			if a.Branch != "" {
				parts = append(parts, dimStyle.Render(a.Branch))
			}
			lines = append(lines, "  Attempt: "+strings.Join(parts, " "))
		}
	}

	// Description.
	if b.Description != "" {
		lines = append(lines, "")
		lines = append(lines, sectionHeader("Description"))
		for _, dl := range wrapText(b.Description, contentWidth-2) {
			lines = append(lines, "  "+dl)
		}
	}

	// Labels.
	if len(b.Labels) > 0 {
		lines = append(lines, "")
		lines = append(lines, sectionHeader("Labels"))
		labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("4"))
		var labelParts []string
		for _, l := range b.Labels {
			labelParts = append(labelParts, labelStyle.Render(l))
		}
		lines = append(lines, "  "+strings.Join(labelParts, "  "))
	}

	// Children.
	if len(data.Children) > 0 {
		lines = append(lines, "")
		lines = append(lines, sectionHeader(fmt.Sprintf("Children (%d)", len(data.Children))))
		for _, c := range data.Children {
			statusIcon := statusIconStr(c.Status)
			lines = append(lines, fmt.Sprintf("  %s %s %s %s", statusIcon, c.ID, ShortType(c.Type), Truncate(c.Title, contentWidth-30)))
		}
	}

	// Dependencies (what this bead depends on).
	if len(data.Deps) > 0 {
		lines = append(lines, "")
		lines = append(lines, sectionHeader(fmt.Sprintf("Dependencies (%d)", len(data.Deps))))
		for _, d := range data.Deps {
			depType := string(d.DependencyType)
			statusIcon := statusIconStr(string(d.Status))
			lines = append(lines, fmt.Sprintf("  %s %s [%s] %s", statusIcon, d.ID, depType, Truncate(d.Title, contentWidth-35)))
		}
	}

	// Dependents (what depends on this bead).
	if len(data.Dependents) > 0 {
		lines = append(lines, "")
		lines = append(lines, sectionHeader(fmt.Sprintf("Dependents (%d)", len(data.Dependents))))
		for _, d := range data.Dependents {
			depType := string(d.DependencyType)
			statusIcon := statusIconStr(string(d.Status))
			lines = append(lines, fmt.Sprintf("  %s %s [%s] %s", statusIcon, d.ID, depType, Truncate(d.Title, contentWidth-35)))
		}
	}

	// Comments.
	if len(data.Comments) > 0 {
		lines = append(lines, "")
		lines = append(lines, sectionHeader(fmt.Sprintf("Comments (%d)", len(data.Comments))))
		for i, c := range data.Comments {
			author := c.Author
			if author == "" {
				author = "unknown"
			}
			ts := ""
			if !c.CreatedAt.IsZero() {
				ts = " " + dimStyle.Render(TimeAgo(c.CreatedAt.Format(time.RFC3339)))
			}
			lines = append(lines, fmt.Sprintf("  %s%s:", lipgloss.NewStyle().Bold(true).Render(author), ts))
			for _, tl := range wrapText(c.Text, contentWidth-4) {
				lines = append(lines, "    "+tl)
			}
			if i < len(data.Comments)-1 {
				lines = append(lines, "")
			}
		}
	}

	// Apply scroll offset.
	if scrollOffset > len(lines)-1 {
		scrollOffset = len(lines) - 1
	}
	if scrollOffset < 0 {
		scrollOffset = 0
	}
	visibleLines := lines[scrollOffset:]

	// Cap to terminal height (leave room for scroll indicator).
	maxVisible := height - 2
	if maxVisible < 5 {
		maxVisible = 5
	}
	if len(visibleLines) > maxVisible {
		visibleLines = visibleLines[:maxVisible]
	}

	// Add scroll indicator.
	result := strings.Join(visibleLines, "\n")
	totalLines := len(lines)
	if totalLines > maxVisible {
		scrollInfo := dimStyle.Render(fmt.Sprintf("  [%d/%d] j/k to scroll", scrollOffset+1, totalLines))
		result += "\n" + scrollInfo
	}

	return result
}

// InspectorLineCount returns the total number of lines the inspector would render.
//
// Deprecated: uses RenderInspector which makes DB calls. Use inspectorLineCountSnap instead.
func InspectorLineCount(data InspectorData, width int) int {
	// Render and count — simpler than duplicating line-counting logic.
	full := RenderInspector(data, width, 10000, 0)
	return strings.Count(full, "\n") + 1
}

// inspectorLineCountSnap counts inspector lines using the pure renderInspectorSnap function.
func inspectorLineCountSnap(data *InspectorData, dag *DAGProgress, width, tab int) int {
	if data == nil {
		return 3
	}
	full := renderInspectorSnap(data.Bead, data, dag, width, 10000, 0, tab)
	return strings.Count(full, "\n") + 1
}

func sectionHeader(title string) string {
	style := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
	return style.Render("── " + title + " ──")
}

func renderStatus(status string) string {
	switch status {
	case "open":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Render("open")
	case "in_progress":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Render("in_progress")
	case "closed":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render("closed")
	default:
		return status
	}
}

func statusIconStr(status string) string {
	switch status {
	case "closed":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Render("✓")
	case "in_progress":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Render("▶")
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render("○")
	}
}

func formatInspectorTime(ts string) string {
	t, ok := ParseBoardTime(ts)
	if !ok {
		return ts
	}
	return t.Format("2006-01-02 15:04") + " (" + TimeAgo(ts) + ")"
}

// wrapText splits text into lines that fit within maxWidth.
func wrapText(text string, maxWidth int) []string {
	if maxWidth < 20 {
		maxWidth = 20
	}
	var result []string
	for _, paragraph := range strings.Split(text, "\n") {
		if paragraph == "" {
			result = append(result, "")
			continue
		}
		words := strings.Fields(paragraph)
		if len(words) == 0 {
			result = append(result, "")
			continue
		}
		line := words[0]
		for _, w := range words[1:] {
			if len(line)+1+len(w) > maxWidth {
				result = append(result, line)
				line = w
			} else {
				line += " " + w
			}
		}
		result = append(result, line)
	}
	return result
}

// renderGroupedDeps renders dependencies grouped by type: blocked-by and blocks.
// Discovered-from deps are omitted (shown in Design Context section).
// Parent-child deps are omitted (shown in header).
func renderGroupedDeps(data *InspectorData, contentWidth int) []string {
	// Filter deps: exclude discovered-from and parent-child.
	var blockedBy []*beads.IssueWithDependencyMetadata
	for _, d := range data.Deps {
		dt := string(d.DependencyType)
		if dt == "discovered-from" || dt == "parent-child" {
			continue
		}
		blockedBy = append(blockedBy, d)
	}

	// Filter dependents: exclude parent-child.
	var blocks []*beads.IssueWithDependencyMetadata
	for _, d := range data.Dependents {
		if string(d.DependencyType) == "parent-child" {
			continue
		}
		blocks = append(blocks, d)
	}

	total := len(blockedBy) + len(blocks)
	if total == 0 {
		return nil
	}

	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	var lines []string
	lines = append(lines, "")
	lines = append(lines, sectionHeader(fmt.Sprintf("Dependencies (%d)", total)))

	if len(blockedBy) > 0 {
		lines = append(lines, "  "+dimStyle.Render("Blocked by:"))
		for _, d := range blockedBy {
			lines = append(lines, "    "+depStatusIcon(string(d.Status))+" "+d.ID+" "+Truncate(d.Title, contentWidth-30))
		}
	}
	if len(blocks) > 0 {
		lines = append(lines, "  "+dimStyle.Render("Blocks:"))
		for _, d := range blocks {
			lines = append(lines, "    "+depStatusIcon(string(d.Status))+" "+d.ID+" "+Truncate(d.Title, contentWidth-30))
		}
	}

	return lines
}

// depStatusIcon returns a colored status icon for dependencies.
// Uses yellow for in_progress to distinguish from the cyan used elsewhere.
func depStatusIcon(status string) string {
	switch status {
	case "closed":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Render("✓")
	case "in_progress":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Render("▶")
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render("○")
	}
}

// extractFromLabel extracts the sender from a message bead's "from:" label.
func extractFromLabel(b BoardBead) string {
	for _, l := range b.Labels {
		if strings.HasPrefix(l, "from:") {
			return l[5:]
		}
	}
	return "system"
}

// inspectorDataMsg carries the result of an async inspector data fetch.
type inspectorDataMsg struct {
	Data *InspectorData
	Err  error
}

// fetchInspectorCmd returns a tea.Cmd that fetches inspector data in a goroutine.
func fetchInspectorCmd(b BoardBead) tea.Cmd {
	return func() tea.Msg {
		data := FetchInspectorData(b)
		return inspectorDataMsg{Data: &data}
	}
}

// renderInspectorSnap renders the inspector using pre-fetched data and DAG progress.
// It produces the same visual output as RenderInspector but reads from params
// instead of calling the DB, making it safe to call from View().
// If data is nil, returns a "Loading..." placeholder.
// The tab parameter selects the active tab (0=details, 1=logs).
func renderInspectorSnap(b BoardBead, data *InspectorData, dag *DAGProgress, width, height, scrollOffset, tab int) string {
	if data == nil {
		headerStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
		dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
		return headerStyle.Render("INSPECTOR") + "  " + dimStyle.Render("Esc to close") + "\n\nLoading..."
	}

	if width < 40 {
		width = 40
	}
	contentWidth := width - 4
	if contentWidth > 100 {
		contentWidth = 100
	}

	var lines []string

	// Header bar.
	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	headerHint := "Esc to close  Tab to switch"
	if b.Type == "design" && b.HasLabel("needs-human") {
		headerHint = "y Approve  n Reject  Esc close  Tab switch"
	}
	lines = append(lines, headerStyle.Render("INSPECTOR")+"  "+dimStyle.Render(headerHint))
	lines = append(lines, strings.Repeat("─", Min(contentWidth, 60)))

	// Tab bar.
	activeTabStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6")).Underline(true)
	inactiveTabStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	tabs := []string{"Details", "Logs"}
	var tabParts []string
	for i, t := range tabs {
		if i == tab {
			tabParts = append(tabParts, activeTabStyle.Render("["+t+"]"))
		} else {
			tabParts = append(tabParts, inactiveTabStyle.Render("["+t+"]"))
		}
	}
	lines = append(lines, strings.Join(tabParts, "  "))
	lines = append(lines, "")

	bb := data.Bead

	if tab == InspectorTabDetails {
		// Title + ID.
		titleStyle := lipgloss.NewStyle().Bold(true)
		lines = append(lines, titleStyle.Render(bb.ID)+"  "+PriStr(bb.Priority)+"  "+ShortType(bb.Type))
		lines = append(lines, titleStyle.Render(bb.Title))
		lines = append(lines, "")

		// Status / Phase / Owner.
		statusLine := "Status: " + renderStatus(bb.Status)
		if data.Phase != "" {
			statusLine += "  Phase: " + lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Render(data.Phase)
		}
		lines = append(lines, statusLine)

		owner := BeadOwnerLabel(bb)
		if owner != "" {
			lines = append(lines, "Owner: "+lipgloss.NewStyle().Foreground(lipgloss.Color("5")).Render(owner))
		}
		if bb.Parent != "" {
			lines = append(lines, "Parent: "+dimStyle.Render(bb.Parent))
		}
		timeParts := []string{}
		if bb.CreatedAt != "" {
			timeParts = append(timeParts, "Created: "+formatInspectorTime(bb.CreatedAt))
		}
		if bb.UpdatedAt != "" {
			timeParts = append(timeParts, "Updated: "+formatInspectorTime(bb.UpdatedAt))
		}
		if len(timeParts) > 0 {
			lines = append(lines, dimStyle.Render(strings.Join(timeParts, "  ")))
		}

		// DAG pipeline — use pre-fetched dag param instead of calling DB.
		if dag != nil && len(dag.Steps) > 0 {
			lines = append(lines, "")
			lines = append(lines, sectionHeader("Pipeline"))
			lines = append(lines, "  "+RenderPipelineFromDAG(dag))
			if dag.Attempt != nil {
				a := dag.Attempt
				parts := []string{lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Render(a.Agent)}
				if a.Model != "" {
					parts = append(parts, dimStyle.Render(a.Model))
				}
				if a.Branch != "" {
					parts = append(parts, dimStyle.Render(a.Branch))
				}
				lines = append(lines, "  Attempt: "+strings.Join(parts, " "))
			}
		}

		// Description.
		if bb.Description != "" {
			lines = append(lines, "")
			lines = append(lines, sectionHeader("Description"))
			for _, dl := range wrapText(bb.Description, contentWidth-2) {
				lines = append(lines, "  "+dl)
			}
		}

		// Labels.
		if len(bb.Labels) > 0 {
			lines = append(lines, "")
			lines = append(lines, sectionHeader("Labels"))
			labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("4"))
			var labelParts []string
			for _, l := range bb.Labels {
				labelParts = append(labelParts, labelStyle.Render(l))
			}
			lines = append(lines, "  "+strings.Join(labelParts, "  "))
		}

		// Design context.
		if len(data.DesignBeads) > 0 {
			lines = append(lines, "")
			lines = append(lines, sectionHeader("Design Context"))
			for _, db := range data.DesignBeads {
				statusIcon := statusIconStr(db.Status)
				lines = append(lines, fmt.Sprintf("  %s %s  %s", statusIcon, db.ID, Truncate(db.Title, contentWidth-20)))
				if db.Description != "" {
					descLines := wrapText(db.Description, contentWidth-4)
					maxPreview := 5
					for i, dl := range descLines {
						if i >= maxPreview {
							lines = append(lines, "    "+dimStyle.Render("..."))
							break
						}
						lines = append(lines, "    "+dl)
					}
				}
			}
		}

		// Children (sorted: open/in_progress first, then closed).
		if len(data.Children) > 0 {
			sorted := make([]Bead, len(data.Children))
			copy(sorted, data.Children)
			sort.SliceStable(sorted, func(i, j int) bool {
				iClosed := sorted[i].Status == "closed"
				jClosed := sorted[j].Status == "closed"
				if iClosed != jClosed {
					return !iClosed
				}
				return false
			})
			lines = append(lines, "")
			lines = append(lines, sectionHeader(fmt.Sprintf("Children (%d)", len(sorted))))
			for _, c := range sorted {
				statusIcon := statusIconStr(c.Status)
				phase := ""
				if data.ChildPhases != nil {
					if p, ok := data.ChildPhases[c.ID]; ok && p != "" {
						phase = " " + lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Render("["+p+"]")
					}
				}
				lines = append(lines, fmt.Sprintf("  %s %s %s%s %s", statusIcon, c.ID, ShortType(c.Type), phase, Truncate(c.Title, contentWidth-35)))
			}
		}

		// Dependencies (grouped by type).
		lines = append(lines, renderGroupedDeps(data, contentWidth)...)

		// Messages.
		if len(data.Messages) > 0 {
			lines = append(lines, "")
			lines = append(lines, sectionHeader(fmt.Sprintf("Messages (%d)", len(data.Messages))))
			for _, msg := range data.Messages {
				from := extractFromLabel(msg)
				ts := ""
				if msg.CreatedAt != "" {
					ts = " " + dimStyle.Render(TimeAgo(msg.CreatedAt))
				}
				lines = append(lines, fmt.Sprintf("  %s%s: %s", lipgloss.NewStyle().Bold(true).Render(from), ts, Truncate(msg.Title, contentWidth-30)))
			}
		}

		// Comments.
		if len(data.Comments) > 0 {
			lines = append(lines, "")
			lines = append(lines, sectionHeader(fmt.Sprintf("Comments (%d)", len(data.Comments))))
			for i, c := range data.Comments {
				author := c.Author
				if author == "" {
					author = "unknown"
				}
				ts := ""
				if !c.CreatedAt.IsZero() {
					ts = " " + dimStyle.Render(TimeAgo(c.CreatedAt.Format(time.RFC3339)))
				}
				lines = append(lines, fmt.Sprintf("  %s%s:", lipgloss.NewStyle().Bold(true).Render(author), ts))
				for _, tl := range wrapText(c.Text, contentWidth-4) {
					lines = append(lines, "    "+tl)
				}
				if i < len(data.Comments)-1 {
					lines = append(lines, "")
				}
			}
		}
	}

	if tab == InspectorTabLogs {
		lines = append(lines, sectionHeader("Wizard Logs"))
		lines = append(lines, "")
		if data.LogContent != "" {
			logLines := strings.Split(data.LogContent, "\n")
			start := 0
			if len(logLines) > 50 {
				start = len(logLines) - 50
				lines = append(lines, dimStyle.Render(fmt.Sprintf("  ... showing last 50 of %d lines", len(logLines))))
			}
			for _, ll := range logLines[start:] {
				lines = append(lines, "  "+ll)
			}
		} else {
			lines = append(lines, dimStyle.Render("  No active wizard for "+bb.ID))
		}
	}

	// Apply scroll offset.
	if scrollOffset > len(lines)-1 {
		scrollOffset = len(lines) - 1
	}
	if scrollOffset < 0 {
		scrollOffset = 0
	}
	visibleLines := lines[scrollOffset:]

	// Cap to terminal height (leave room for scroll indicator).
	maxVisible := height - 2
	if maxVisible < 5 {
		maxVisible = 5
	}
	if len(visibleLines) > maxVisible {
		visibleLines = visibleLines[:maxVisible]
	}

	// Add scroll indicator.
	result := strings.Join(visibleLines, "\n")
	totalLines := len(lines)
	if totalLines > maxVisible {
		scrollInfo := dimStyle.Render(fmt.Sprintf("  [%d/%d] j/k to scroll", scrollOffset+1, totalLines))
		result += "\n" + scrollInfo
	}

	return result
}

// RenderFeedbackInput renders the feedback text input bar for design bead rejection.
func RenderFeedbackInput(input string, width int) string {
	promptStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
	inputStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("7"))
	cursorStyle := lipgloss.NewStyle().Background(lipgloss.Color("7")).Foreground(lipgloss.Color("0"))
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	prompt := promptStyle.Render("Feedback: ")
	cursor := cursorStyle.Render(" ")
	hint := dimStyle.Render("  Enter submit • Esc cancel")

	maxInput := width - 30
	if maxInput < 20 {
		maxInput = 20
	}
	displayInput := input
	if len(displayInput) > maxInput {
		displayInput = displayInput[len(displayInput)-maxInput:]
	}

	return prompt + inputStyle.Render(displayInput) + cursor + hint
}
