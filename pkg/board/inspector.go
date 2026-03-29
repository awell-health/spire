package board

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/steveyegge/beads"

	"github.com/awell-health/spire/pkg/store"
)

// InspectorData holds the fetched detail data for the inspector pane.
type InspectorData struct {
	Bead       BoardBead
	Phase      string
	Comments   []*beads.Comment
	Children   []Bead
	Deps       []*beads.IssueWithDependencyMetadata // what this bead depends on
	Dependents []*beads.IssueWithDependencyMetadata // what depends on this bead
	DAG        *DAGProgress
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
	}
	if deps, err := store.GetDepsWithMeta(b.ID); err == nil {
		data.Deps = deps
	}
	if dependents, err := store.GetDependentsWithMeta(b.ID); err == nil {
		data.Dependents = dependents
	}
	data.DAG = FetchDAGProgress(b.ID)

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
func InspectorLineCount(data InspectorData, width int) int {
	// Render and count — simpler than duplicating line-counting logic.
	full := RenderInspector(data, width, 10000, 0)
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

// RenderPipelineFromDAG renders a compact step pipeline from pre-fetched DAG data.
// This is the pure equivalent of RenderPipelineLipgloss — no DB calls.
// NOTE: The parallel render task (spi-ff00t) also defines this in render.go;
// the merger should deduplicate after both branches land.
func RenderPipelineFromDAG(dag *DAGProgress) string {
	if dag == nil || len(dag.Steps) == 0 {
		return ""
	}

	greenStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	cyanStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	dimLGStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	var parts []string
	for _, step := range dag.Steps {
		switch step.Status {
		case "closed":
			parts = append(parts, greenStyle.Render("✓"))
		case "in_progress":
			parts = append(parts, cyanStyle.Render("▶"))
		default:
			parts = append(parts, dimLGStyle.Render("○"))
		}
	}
	return strings.Join(parts, " ")
}

// renderInspectorSnap renders the inspector using pre-fetched data and DAG progress.
// It produces the same visual output as RenderInspector but reads from params
// instead of calling the DB, making it safe to call from View().
// If data is nil, returns a "Loading..." placeholder.
func renderInspectorSnap(b BoardBead, data *InspectorData, dag *DAGProgress, width, height, scrollOffset int) string {
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
	lines = append(lines, headerStyle.Render("INSPECTOR")+"  "+dimStyle.Render("Esc to close"))
	lines = append(lines, strings.Repeat("─", Min(contentWidth, 60)))

	bb := data.Bead

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
