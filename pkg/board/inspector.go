package board

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"charm.land/lipgloss/v2"
	"github.com/steveyegge/beads"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/dolt"
	"github.com/awell-health/spire/pkg/executor"
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
)

// Inspector tab constants.
const (
	InspectorTabDetails = 0
	InspectorTabLogs    = 1
)

// HookedStepInfo describes a hooked (parked) step for the inspector display.
type HookedStepInfo struct {
	StepName   string // e.g. "review", "design-check"
	WaitingFor string // human-readable description of what the step is waiting for
}

// LogView represents a single log source displayed in the inspector Logs tab.
type LogView struct {
	Name    string // e.g. "wizard", "epic-plan (13:09)", "recovery-decide (14:02)"
	Path    string // absolute path to the log file on disk
	Content string // full file contents
}

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
	Messages    []BoardBead      // messages referencing this bead
	DesignBeads []BoardBead      // design beads linked via discovered-from deps
	HookedStep  *HookedStepInfo  // hooked step details (nil when bead is not hooked)
	Logs        []LogView        // ordered: wizard top-level first, then claude logs newest-first
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

	// Hooked step details.
	if b.Status == "hooked" {
		data.HookedStep = findHookedStepInfo(b.ID, data.DAG)
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
			data.Logs = append(data.Logs, LogView{
				Name:    "wizard",
				Path:    path,
				Content: string(content),
			})
			break
		}
	}

	// Per-invocation claude subprocess logs.
	claudeDir := filepath.Join(logDir, wizardName, "claude")
	if matches, err := filepath.Glob(filepath.Join(claudeDir, "*.log")); err == nil && len(matches) > 0 {
		// Sort descending (newest first, since timestamps sort lexicographically).
		sort.Sort(sort.Reverse(sort.StringSlice(matches)))
		for _, path := range matches {
			content, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			data.Logs = append(data.Logs, LogView{
				Name:    claudeLogName(filepath.Base(path)),
				Path:    path,
				Content: string(content),
			})
		}
	}

	// Sibling spawn logs: apprentices, sages, clerics. Each spawn is named
	// wizard-<beadID>-<suffix> (e.g. -impl, -sage-review, -w0-0, -seq-1),
	// with its own claude/ subdir for any claude subprocesses it invokes.
	knownNames := map[string]bool{}
	for _, lv := range data.Logs {
		knownNames[filepath.Base(lv.Path)] = true
	}
	if siblings, err := filepath.Glob(filepath.Join(logDir, wizardName+"-*.log")); err == nil {
		sort.Strings(siblings)
		for _, path := range siblings {
			if knownNames[filepath.Base(path)] {
				continue
			}
			content, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			stem := strings.TrimSuffix(filepath.Base(path), ".log")
			name := strings.TrimPrefix(stem, wizardName+"-")
			data.Logs = append(data.Logs, LogView{
				Name:    name,
				Path:    path,
				Content: string(content),
			})
			sibClaudeDir := filepath.Join(logDir, stem, "claude")
			if sibMatches, err := filepath.Glob(filepath.Join(sibClaudeDir, "*.log")); err == nil && len(sibMatches) > 0 {
				sort.Sort(sort.Reverse(sort.StringSlice(sibMatches)))
				for _, sp := range sibMatches {
					sc, err := os.ReadFile(sp)
					if err != nil {
						continue
					}
					data.Logs = append(data.Logs, LogView{
						Name:    name + "/" + claudeLogName(filepath.Base(sp)),
						Path:    sp,
						Content: string(sc),
					})
				}
			}
		}
	}

	return data
}

// claudeLogName derives a display name from a claude log filename.
// Input: "<label>-<YYYYMMDD-HHMMSS>.log" (e.g. "epic-plan-20260417-173412.log").
// Output: "<label> (HH:MM)" (e.g. "epic-plan (17:34)").
// If the filename does not match the expected pattern, returns the filename
// without its .log extension as a fallback.
func claudeLogName(filename string) string {
	base := strings.TrimSuffix(filename, ".log")
	// Find the last occurrence of "-YYYYMMDD-HHMMSS" suffix.
	// Scan from the right for a suffix of the form -DDDDDDDD-DDDDDD (8+6 digits).
	if len(base) < 16 {
		return base
	}
	tsStart := len(base) - 16 // position of the leading '-'
	if base[tsStart] != '-' {
		return base
	}
	tsPart := base[tsStart+1:] // "YYYYMMDD-HHMMSS"
	if len(tsPart) != 15 || tsPart[8] != '-' {
		return base
	}
	datePart := tsPart[:8]
	timePart := tsPart[9:]
	for i := 0; i < 8; i++ {
		if datePart[i] < '0' || datePart[i] > '9' {
			return base
		}
	}
	for i := 0; i < 6; i++ {
		if timePart[i] < '0' || timePart[i] > '9' {
			return base
		}
	}
	label := base[:tsStart]
	if label == "" {
		return base
	}
	return fmt.Sprintf("%s (%s:%s)", label, timePart[:2], timePart[2:4])
}

// findHookedStepInfo determines which step is hooked and what it's waiting for.
// It uses DAG steps (already loaded) to find the hooked step name, then loads
// the executor graph state to read step outputs for the "waiting for" detail.
func findHookedStepInfo(beadID string, dag *DAGProgress) *HookedStepInfo {
	// Find hooked step name from DAG.
	hookedName := ""
	if dag != nil {
		for _, s := range dag.Steps {
			if s.Status == "hooked" {
				hookedName = s.Name
				break
			}
		}
	}
	if hookedName == "" {
		return nil
	}

	info := &HookedStepInfo{StepName: hookedName}

	// Try to load graph state to get step outputs.
	wizardName := "wizard-" + beadID
	gs, err := executor.LoadGraphState(wizardName, config.Dir)
	if err == nil && gs != nil {
		if ss, ok := gs.Steps[hookedName]; ok {
			info.WaitingFor = hookedWaitingFor(hookedName, ss.Outputs)
			return info
		}
	}

	// Fallback: infer from step name alone.
	info.WaitingFor = hookedWaitingFor(hookedName, nil)
	return info
}

// hookedWaitingFor determines a human-readable "waiting for" string from
// the step name and its graph state outputs.
func hookedWaitingFor(stepName string, outputs map[string]string) string {
	// Check outputs for design_ref.
	if ref, ok := outputs["design_ref"]; ok && ref != "" {
		return "design bead " + ref
	}

	// Check if step name suggests human approval.
	if stepName == "review" || strings.Contains(stepName, "review") {
		return "human approval"
	}

	// Check outputs for explicit hook reason.
	if reason, ok := outputs["hook_reason"]; ok && reason != "" {
		return reason
	}

	// If outputs exist, show them as raw condition.
	if len(outputs) > 0 {
		var parts []string
		for k, v := range outputs {
			parts = append(parts, k+"="+v)
		}
		sort.Strings(parts)
		return strings.Join(parts, ", ")
	}

	return "external condition"
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

	// Hooked step details — shown prominently when bead is hooked.
	if data.HookedStep != nil {
		hookedStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
		lines = append(lines, hookedStyle.Render("⏸ Hooked at: step:"+data.HookedStep.StepName))
		lines = append(lines, "Waiting for: "+hookedStyle.Render(data.HookedStep.WaitingFor))
	}

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
func inspectorLineCountSnap(data *InspectorData, dag *DAGProgress, width, tab, logIdx int) int {
	if data == nil {
		return 3
	}
	full := renderInspectorSnap(data.Bead, data, dag, width, 10000, 0, tab, logIdx)
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
	case "hooked":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Render("hooked")
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
	case "hooked":
		return lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Render("⏸")
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
// The logIdx parameter selects which log is shown within the Logs tab.
func renderInspectorSnap(b BoardBead, data *InspectorData, dag *DAGProgress, width, height, scrollOffset, tab, logIdx int) string {
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
	headerHint := "Esc close  Tab switch  j/k scroll  J/K ×5"
	if tab == InspectorTabLogs {
		headerHint = "Esc close  Tab switch  j/k scroll  ←/→ or h/l log"
	}
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

		// Hooked step details — shown prominently when bead is hooked.
		if data.HookedStep != nil {
			hookedStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
			lines = append(lines, hookedStyle.Render("⏸ Hooked at: step:"+data.HookedStep.StepName))
			lines = append(lines, "Waiting for: "+hookedStyle.Render(data.HookedStep.WaitingFor))
		}

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

		// Recovery metadata — only for recovery beads.
		if bb.Type == "recovery" {
			lines = append(lines, "")
			lines = append(lines, sectionHeader("Recovery"))

			sourceBead := bb.Meta(recovery.KeySourceBead)
			if sourceBead != "" {
				sourceTitle := ""
				if sb, err := store.GetBead(sourceBead); err == nil {
					sourceTitle = sb.Title
				}
				lines = append(lines, fmt.Sprintf("  Source: %s  %s", sourceBead, dimStyle.Render(Truncate(sourceTitle, contentWidth-30))))
			}

			if fc := bb.Meta(recovery.KeyFailureClass); fc != "" {
				lines = append(lines, "  Failure: "+lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Render(fc))
			}
			if step := bb.Meta(recovery.KeySourceStep); step != "" {
				lines = append(lines, "  Step: "+step)
			}
			if res := bb.Meta(recovery.KeyResolutionKind); res != "" {
				lines = append(lines, "  Resolution: "+lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Render(res))
			}
			if vs := bb.Meta(recovery.KeyVerificationStatus); vs != "" {
				color := "2" // green
				if vs != "clean" {
					color = "1" // red
				}
				lines = append(lines, "  Verification: "+lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Render(vs))
			}
			if summary := bb.Meta(recovery.KeyLearningSummary); summary != "" {
				lines = append(lines, "  Learning: "+dimStyle.Render(Truncate(summary, contentWidth-12)))
			}

			// Prior recoveries for the same source bead.
			if sourceBead != "" {
				if learnings, err := recovery.GetRecoveryLearnings(sourceBead); err == nil && len(learnings) > 0 {
					// Don't count self.
					var prior []store.Bead
					for _, l := range learnings {
						if l.ID != bb.ID {
							prior = append(prior, l)
						}
					}
					if len(prior) > 0 {
						lines = append(lines, fmt.Sprintf("  Prior: %d recovery bead(s) for %s", len(prior), sourceBead))
						for _, p := range prior {
							rm := recovery.RecoveryMetadataFromBead(p)
							detail := rm.FailureClass
							if rm.ResolutionKind != "" {
								detail += " → " + rm.ResolutionKind
							}
							if rm.VerificationStatus != "" {
								detail += " (" + rm.VerificationStatus + ")"
							}
							lines = append(lines, "    "+dimStyle.Render(p.ID+"  "+detail))
						}
					}
				}
			}
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
		if len(data.Logs) == 0 {
			lines = append(lines, dimStyle.Render("  No logs for "+bb.ID))
		} else {
			// Clamp logIdx into range.
			idx := logIdx
			if idx < 0 {
				idx = 0
			}
			if idx >= len(data.Logs) {
				idx = len(data.Logs) - 1
			}

			// Sub-tab strip listing the available logs. May wrap across
			// multiple lines when there are many spawns (epic with N
			// apprentices + sage + their claude logs can easily exceed
			// terminal width). Lines pack greedily by visible width.
			activeLogStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6")).Underline(true)
			inactiveLogStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
			counter := dimStyle.Render(fmt.Sprintf("%d/%d", idx+1, len(data.Logs))) + " " + dimStyle.Render("← h/l →")
			header := dimStyle.Render("Logs:") + " "
			headerW := lipgloss.Width(header)
			sep := "  "
			sepW := lipgloss.Width(sep)
			var stripLines []string
			cur := header
			curW := headerW
			for i, lv := range data.Logs {
				var part string
				if i == idx {
					part = activeLogStyle.Render("[" + lv.Name + "]")
				} else {
					part = inactiveLogStyle.Render(lv.Name)
				}
				partW := lipgloss.Width(part)
				if curW > headerW && curW+sepW+partW > contentWidth {
					stripLines = append(stripLines, cur)
					cur = strings.Repeat(" ", headerW) + part
					curW = headerW + partW
					continue
				}
				if curW > headerW {
					cur += sep
					curW += sepW
				}
				cur += part
				curW += partW
			}
			stripLines = append(stripLines, cur)
			lines = append(lines, stripLines...)
			lines = append(lines, strings.Repeat(" ", headerW)+counter)
			lines = append(lines, "")

			// Render active log. Use the full inspector width (not the
			// 100-col prose cap): logs have long lines (args arrays, JSON
			// stream) and event logs that benefit from room.
			active := data.Logs[idx]
			if active.Content != "" {
				logLines := strings.Split(active.Content, "\n")
				start := 0
				if len(logLines) > 50 {
					start = len(logLines) - 50
					lines = append(lines, dimStyle.Render(fmt.Sprintf("  ... showing last 50 of %d lines", len(logLines))))
				}
				logWidth := width - 4
				if logWidth < 40 {
					logWidth = 40
				}
				for _, ll := range logLines[start:] {
					for _, wl := range wrapText(ll, logWidth-2) {
						lines = append(lines, "  "+wl)
					}
				}
			} else {
				lines = append(lines, dimStyle.Render("  (empty log)"))
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

// RenderResolveInput renders the resolve text input bar for needs-human bead resolution.
func RenderResolveInput(input string, width int) string {
	promptStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
	inputStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("7"))
	cursorStyle := lipgloss.NewStyle().Background(lipgloss.Color("7")).Foreground(lipgloss.Color("0"))
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	prompt := promptStyle.Render("Resolve: ")
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
