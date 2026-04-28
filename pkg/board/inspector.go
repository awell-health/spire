package board

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"charm.land/lipgloss/v2"
	"github.com/steveyegge/beads"

	"github.com/awell-health/spire/pkg/board/logstream"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/dolt"
	"github.com/awell-health/spire/pkg/executor"
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
)

// logSourceFactory returns the logstream.Source used to populate the
// Logs tab for a bead. Defaults to the local-filesystem source rooted
// at dolt.GlobalDir()+"/wizards" so existing local-native flows render
// the same logs they did before the refactor. cmd/spire's board entry
// point overrides this in cluster-attach mode (via SetLogSourceFactory)
// with a gateway-backed source so desktop / CLI never read pod logs
// directly.
var logSourceFactory = func() logstream.Source {
	return logstream.NewLocalSource(filepath.Join(dolt.GlobalDir(), "wizards"))
}

// SetLogSourceFactory installs the package-wide log source constructor.
// cmd/spire calls this once at board startup to switch the inspector
// onto a gateway-backed source when the active tower is in gateway
// (cluster-attach) mode. Passing nil restores the default local-filesystem
// source so tests can reset between runs.
func SetLogSourceFactory(f func() logstream.Source) {
	if f == nil {
		f = func() logstream.Source {
			return logstream.NewLocalSource(filepath.Join(dolt.GlobalDir(), "wizards"))
		}
	}
	logSourceFactory = f
}

// Inspector tab constants.
const (
	InspectorTabDetails = 0
	InspectorTabLogs    = 1
)

// LogMode selects how the Logs tab renders the active log: LogModePretty
// drives event rendering through the provider adapter, LogModeRaw prints
// the raw transcript bytes.
type LogMode int

const (
	LogModePretty LogMode = iota
	LogModeRaw
)

// HookedStepInfo describes a hooked (parked) step for the inspector display.
type HookedStepInfo struct {
	StepName   string // e.g. "review", "design-check"
	WaitingFor string // human-readable description of what the step is waiting for
}

// LogView represents a single log source displayed in the inspector Logs tab.
type LogView struct {
	Name          string // e.g. "wizard", "epic-plan (13:09)", "recovery-decide (14:02)"
	Path          string // absolute path to the log file on disk
	Content       string // full stdout file contents

	// Provider is the transcript's source adapter: "claude", "codex", or
	// "" for raw fallback (operational logs, unknown formats).
	Provider string

	// StderrPath is the sidecar "<base>.stderr.log" path; empty when no
	// sidecar file exists on disk.
	StderrPath string

	// StderrContent is the full sidecar contents, preloaded at LogView
	// construction. Empty when StderrPath is empty.
	StderrContent string

	// Events holds the adapter-parsed stdout events plus one KindStderr
	// event per non-empty sidecar line. Nil for raw fallback — renderer
	// then prints Content verbatim regardless of mode.
	Events []logstream.LogEvent

	// Cycle is the reset cycle this log was produced in (matched via the
	// review/attempt bead's reset-cycle:<N> label). 0 means uncategorized
	// (e.g. the top-level wizard log or logs whose owning bead can't be
	// matched). The Logs tab groups views with non-zero Cycle under a
	// "Reset cycle N" header.
	Cycle int
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
	// Fetch ALL children once (open + closed). Used both to populate the
	// visible Children list (real subtasks) and to build the cycle-lookup
	// map for log grouping below.
	allChildren, _ := store.GetChildren(b.ID)
	if allChildren != nil {
		// Filter out internal beads (step, attempt, review-round).
		var visible []Bead
		for _, c := range allChildren {
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

	// Logs tab: assemble through a logstream.Source so local-native and
	// cluster-attach modes share one read path. The default factory
	// returns a local filesystem source rooted at dolt.GlobalDir()+
	// "/wizards"; cmd/spire swaps in a gateway-backed source for
	// gateway-mode towers (see SetLogSourceFactory).
	src := logSourceFactory()
	if src != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		artifacts, err := src.List(ctx, b.ID)
		cancel()
		if err == nil {
			data.Logs = artifactsToLogViews(artifacts)
		}
	}

	// Tag each LogView with the reset cycle of its owning attempt/review
	// bead so the renderer can group them under cycle headers. Logs that
	// don't match any bead (top-level wizard log, legacy logs from before
	// reset-cycle stamping) keep Cycle=0 and render outside any group.
	cycleByLogName := buildLogCycleMap(allChildren)
	if len(cycleByLogName) > 0 {
		for i := range data.Logs {
			if c, ok := cycleByLogName[data.Logs[i].Name]; ok {
				data.Logs[i].Cycle = c
				continue
			}
			// Provider sub-logs are named "<spawn>/...". Match on the
			// spawn-prefix so claude transcripts inherit the spawn's cycle.
			if slash := strings.IndexByte(data.Logs[i].Name, '/'); slash > 0 {
				if c, ok := cycleByLogName[data.Logs[i].Name[:slash]]; ok {
					data.Logs[i].Cycle = c
				}
			}
		}
	}

	return data
}

// Step-name lists used by buildLogCycleMap to pair log filenames with the
// bead whose counter named them. The lists mirror the monotonic-naming
// overrides in pkg/executor/graph_actions.go: any flow that reuses round:N
// for its spawn name must appear in reviewPairedStepNames, any flow that
// reuses attempt:N must appear in attemptPairedStepNames. Future steps that
// start paying the monotonic counter (e.g. validate, deploy) just append
// their name to the appropriate list — no logic change required.
var (
	reviewPairedStepNames  = []string{"sage-review", "fix"}
	attemptPairedStepNames = []string{"implement"}
)

// buildLogCycleMap maps a wizard log's display name (the suffix after
// "wizard-<beadID>-") to its reset cycle.
//
// The mapping relies on the spawn-name convention from
// pkg/executor/graph_actions.go: spawnName = "<agentName>-<step>-<N>", which
// produces a log file "wizard-<beadID>-<step>-<N>.log" and a stripped name
// "<step>-<N>". The monotonic override there reuses round:N or attempt:N
// for the N — so the log name can be joined back to the originating bead's
// reset-cycle by looking up N in the appropriate counter map.
//
//   - "sage-review-<N>" / "fix-<N>" → paired with review-round bead
//     round:<N> (fix runs after the sage-review round that requested
//     changes and inherits its N; see wizardRunSpawnWithHandoff).
//   - "implement-<N>" → paired with attempt bead attempt:<N> (created by
//     the executor at run start; the implement spawn reuses its N).
//
// The returned cycle is the bead's reset-cycle:<N> label value (defaults
// to 1 when missing). Unmatched names are absent from the map.
func buildLogCycleMap(children []Bead) map[string]int {
	if len(children) == 0 {
		return nil
	}
	roundCycle := map[int]int{}   // round number → cycle (review rounds)
	attemptCycle := map[int]int{} // attempt number → cycle (attempt beads)
	for _, c := range children {
		switch {
		case store.IsReviewRoundBead(c):
			n := store.ReviewRoundNumber(c)
			if n > 0 {
				roundCycle[n] = store.ResetCycleNumber(c)
			}
		case store.IsAttemptBead(c):
			n := store.AttemptNumber(c)
			if n > 0 {
				attemptCycle[n] = store.ResetCycleNumber(c)
			}
		}
	}
	if len(roundCycle) == 0 && len(attemptCycle) == 0 {
		return nil
	}
	out := make(map[string]int,
		len(roundCycle)*len(reviewPairedStepNames)+
			len(attemptCycle)*len(attemptPairedStepNames))
	for n, cycle := range roundCycle {
		for _, step := range reviewPairedStepNames {
			out[fmt.Sprintf("%s-%d", step, n)] = cycle
		}
	}
	for n, cycle := range attemptCycle {
		for _, step := range attemptPairedStepNames {
			out[fmt.Sprintf("%s-%d", step, n)] = cycle
		}
	}
	return out
}

// loadProviderLogViews walks wizardDir/<provider>/ subdirectories and
// returns a LogView for every transcript file it finds. Thin wrapper
// around the logstream helper that produces Artifacts; this layer adds
// adapter event parsing and the stderr-line synthesis the inspector
// depends on. Kept as a pkg/board-level helper so tests in this package
// (transcript_e2e_test.go in particular) can probe the LogView shape
// directly without going through the Source.
func loadProviderLogViews(wizardDir, namePrefix string) []LogView {
	return artifactsToLogViews(logstream.LoadProviderArtifacts(wizardDir, namePrefix))
}

// artifactsToLogViews converts Source-produced Artifacts to LogViews,
// running each artifact's bytes through the registered provider
// adapter and synthesising a KindStderr event per non-empty sidecar
// line. Provider-empty artifacts (operational stdout) get no events
// because no adapter exists; the renderer falls back to printing
// Content verbatim. Cycle remains unset here — inspector callers that
// want grouping must layer it on after this conversion (see
// buildLogCycleMap and the cycle-tagging block in FetchInspectorData).
func artifactsToLogViews(artifacts []logstream.Artifact) []LogView {
	if len(artifacts) == 0 {
		return nil
	}
	views := make([]LogView, 0, len(artifacts))
	for _, a := range artifacts {
		var events []logstream.LogEvent
		if a.Provider != "" {
			adapter := logstream.Get(a.Provider)
			parsed, ok := adapter.Parse(a.Content)
			if ok {
				events = parsed
			}
		}
		if a.StderrContent != "" {
			for _, line := range strings.Split(a.StderrContent, "\n") {
				if line == "" {
					continue
				}
				events = append(events, logstream.LogEvent{
					Kind:  logstream.KindStderr,
					Body:  line,
					Raw:   line,
					Error: true,
				})
			}
		}
		views = append(views, LogView{
			Name:          a.Name,
			Path:          a.Path,
			Content:       a.Content,
			Provider:      a.Provider,
			StderrPath:    a.StderrPath,
			StderrContent: a.StderrContent,
			Events:        events,
		})
	}
	return views
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
			ts := ""
			if !c.CreatedAt.IsZero() {
				ts = " " + dimStyle.Render(TimeAgo(c.CreatedAt.Format(time.RFC3339)))
			}
			lines = append(lines, fmt.Sprintf("  %s%s:", renderCommentAuthor(c.Author), ts))
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
func inspectorLineCountSnap(data *InspectorData, dag *DAGProgress, width, tab, logIdx int, logMode LogMode, errorsOnly, expandAll bool) int {
	if data == nil {
		return 3
	}
	full := renderInspectorSnap(data.Bead, data, dag, width, 10000, 0, tab, logIdx, logMode, errorsOnly, expandAll)
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
// logMode/errorsOnly/expandAll drive the Logs tab rendering (ignored on
// the Details tab).
func renderInspectorSnap(b BoardBead, data *InspectorData, dag *DAGProgress, width, height, scrollOffset, tab, logIdx int, logMode LogMode, errorsOnly, expandAll bool) string {
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
		headerHint = "Esc close  Tab switch  j/k scroll  ←/→ log  p raw  x expand  f errors"
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
				ts := ""
				if !c.CreatedAt.IsZero() {
					ts = " " + dimStyle.Render(TimeAgo(c.CreatedAt.Format(time.RFC3339)))
				}
				lines = append(lines, fmt.Sprintf("  %s%s:", renderCommentAuthor(c.Author), ts))
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
			// Distinct empty-state copy: not a load error, just a bead
			// that hasn't produced any artifacts yet (fresh bead, or a
			// cluster-attach gateway that returned an empty manifest).
			// The single dim line below is the only thing the panel
			// renders so the inspector reads as "intentionally blank"
			// rather than "broken".
			lines = append(lines, dimStyle.Render("  No log artifacts yet for "+bb.ID))
			lines = append(lines, dimStyle.Render("  (logs appear here once an agent has produced output)"))
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
			//
			// Logs whose owning attempt/review bead carries a reset-cycle
			// label render under "Reset cycle N" headers so cycles read as
			// distinct groups; unmatched logs (top-level wizard log, legacy
			// pre-feature spawns) render plainly outside any group.
			activeLogStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6")).Underline(true)
			inactiveLogStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
			cycleHeaderStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
			counter := dimStyle.Render(fmt.Sprintf("%d/%d", idx+1, len(data.Logs))) + " " + dimStyle.Render("← h/l →")
			header := dimStyle.Render("Logs:") + " "
			headerW := lipgloss.Width(header)
			sep := "  "
			sepW := lipgloss.Width(sep)
			var stripLines []string
			cur := header
			curW := headerW
			lastCycle := -1 // -1 sentinel so we always emit a header on first cycle change
			for i, lv := range data.Logs {
				if lv.Cycle != lastCycle {
					// Flush the in-progress strip line before emitting a
					// header so the header sits on its own line.
					if curW > headerW {
						stripLines = append(stripLines, cur)
						cur = header
						curW = headerW
					}
					if lv.Cycle > 0 {
						stripLines = append(stripLines, cycleHeaderStyle.Render(fmt.Sprintf("── Reset cycle %d ──", lv.Cycle)))
					}
					lastCycle = lv.Cycle
				}
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
			pane := renderLogPane(&active, width, logMode, errorsOnly, expandAll)
			if pane != "" {
				lines = append(lines, strings.Split(pane, "\n")...)
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

// renderLogPane renders the body of the Logs tab for the active LogView.
//
// LogModeRaw and LogViews with no parsed events (adapter returned nil or
// the provider is unknown) print active.Content verbatim, wrapped to the
// inspector width. No truncation — the inspector's own scroll region
// handles overflow.
//
// LogModePretty walks active.Events through the provider adapter, applying
// the errorsOnly/expandAll filters. Events that would otherwise be hidden
// (stderr in default pretty view) are dropped via shouldShowEvent.
//
// A nil active or one with no stdout and no stderr content returns the
// dim "(empty log)" placeholder. ErrorsOnly with no matching events
// returns "(no errors)" so the pane is never blank.
func renderLogPane(active *LogView, width int, mode LogMode, errorsOnly, expandAll bool) string {
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	if active == nil || (active.Content == "" && active.StderrContent == "") {
		return "  " + dimStyle.Render("(empty log)")
	}

	logWidth := width - 4
	if logWidth < 40 {
		logWidth = 40
	}

	if mode == LogModeRaw || len(active.Events) == 0 {
		var out []string
		for _, ll := range strings.Split(active.Content, "\n") {
			for _, wl := range wrapText(ll, logWidth-2) {
				out = append(out, "  "+wl)
			}
		}
		return strings.Join(out, "\n")
	}

	adapter := logstream.Get(active.Provider)
	var out []string
	for _, ev := range active.Events {
		if !shouldShowEvent(ev, errorsOnly) {
			continue
		}
		for _, line := range adapter.Render(ev, logWidth-2, expandAll) {
			out = append(out, "  "+line)
		}
	}
	if errorsOnly && len(out) == 0 {
		return "  " + dimStyle.Render("(no errors)")
	}
	return strings.Join(out, "\n")
}

// shouldShowEvent is the per-event filter predicate for renderLogPane.
// Default pretty view hides stderr events (they are noisy output from
// subprocess wrappers). Errors-only keeps anything flagged with
// ev.Error, any stderr line, and any KindUnknown event (which we can't
// classify — surfacing it is safer than silently dropping).
func shouldShowEvent(ev logstream.LogEvent, errorsOnly bool) bool {
	if errorsOnly {
		return ev.Error || ev.Kind == logstream.KindStderr || ev.Kind == logstream.KindUnknown
	}
	return ev.Kind != logstream.KindStderr
}
