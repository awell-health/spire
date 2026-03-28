package board

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// RosterAgent represents an agent registered in the tower.
type RosterAgent struct {
	Name         string        `json:"name"`
	Role         string        `json:"role"`
	Status       string        `json:"status"`
	BeadID       string        `json:"bead_id"`
	BeadTitle    string        `json:"bead_title"`
	EpicID       string        `json:"epic_id,omitempty"`
	EpicTitle    string        `json:"epic_title,omitempty"`
	Phase        string        `json:"phase"`
	Elapsed      time.Duration `json:"elapsed"`
	PhaseElapsed time.Duration `json:"phase_elapsed"`
	Timeout      time.Duration `json:"timeout"`
	Remaining    time.Duration `json:"remaining"`
	RegisteredAt string        `json:"registered_at"`
}

// RosterSummary is the JSON output for roster.
type RosterSummary struct {
	Agents  []RosterAgent `json:"agents"`
	Wizards int           `json:"wizards"`
	Busy    int           `json:"busy"`
	Idle    int           `json:"idle"`
	Offline int           `json:"offline"`
	Timeout time.Duration `json:"timeout"`
}

type rosterBeadContext struct {
	BeadTitle string
	EpicID    string
	EpicTitle string
}

// RosterWorkItem represents a collapsed view of one bead being worked on.
type RosterWorkItem struct {
	BeadID     string
	BeadTitle  string
	EpicID     string
	EpicTitle  string
	Status     string
	Phase      string
	Elapsed    time.Duration
	Timeout    time.Duration
	AgentNames []string
}

// RosterEpicGroup groups work items by epic.
type RosterEpicGroup struct {
	ID    string
	Title string
	Items []RosterWorkItem
}

// k8sPod is the subset of pod JSON we need.
type k8sPod struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Status struct {
		Phase     string `json:"phase"`
		StartTime string `json:"startTime"`
	} `json:"status"`
}

// RosterDeps holds external dependencies needed by roster.
type RosterDeps struct {
	LoadWizardRegistry func() []LocalAgent
	SaveWizardRegistry func([]LocalAgent)
	CleanDeadWizards   func([]LocalAgent) []LocalAgent
	ProcessAlive       func(pid int) bool
}

// RosterFromK8s queries k8s for wizard pods and their start times.
func RosterFromK8s(timeout time.Duration) ([]RosterAgent, error) {
	cmd := exec.Command("kubectl", "get", "pods", "-n", "spire",
		"-l", "spire.awell.io/managed=true",
		"-o", "json")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var podList struct {
		Items []k8sPod `json:"items"`
	}
	if err := json.Unmarshal(out, &podList); err != nil {
		return nil, err
	}

	var agentNames []string
	if names, err := exec.Command("kubectl", "get", "spireagent", "-n", "spire",
		"-o", "jsonpath={.items[*].metadata.name}").Output(); err == nil {
		for _, n := range strings.Fields(strings.TrimSpace(string(names))) {
			if strings.HasPrefix(n, "wizard-") || strings.HasPrefix(n, "artificer") {
				agentNames = append(agentNames, n)
			}
		}
	}

	podAgents := make(map[string]RosterAgent)
	for _, pod := range podList.Items {
		agentName := pod.Metadata.Labels["spire.awell.io/agent"]
		beadID := pod.Metadata.Labels["spire.awell.io/bead"]
		role := pod.Metadata.Labels["spire.awell.io/role"]
		if role == "" {
			role = "wizard"
		}

		agent := RosterAgent{
			Name:    agentName,
			Role:    role,
			Status:  "working",
			BeadID:  beadID,
			Timeout: timeout,
		}

		if pod.Status.StartTime != "" {
			if t, err := time.Parse(time.RFC3339, pod.Status.StartTime); err == nil {
				agent.Elapsed = time.Since(t).Round(time.Second)
				agent.Remaining = timeout - agent.Elapsed
				if agent.Remaining < 0 {
					agent.Remaining = 0
				}
			}
		}

		switch pod.Status.Phase {
		case "Succeeded", "Failed":
			agent.Status = "done"
		case "Pending":
			agent.Status = "provisioning"
		}

		podAgents[agentName] = agent
	}

	var agents []RosterAgent
	for _, name := range agentNames {
		if a, ok := podAgents[name]; ok {
			agents = append(agents, a)
		} else {
			agents = append(agents, RosterAgent{
				Name:    name,
				Role:    "wizard",
				Status:  "idle",
				Timeout: timeout,
			})
		}
	}

	return agents, nil
}

// RosterFromBeads builds a roster from bead state (no k8s).
func RosterFromBeads(timeout time.Duration) []RosterAgent {
	agentBeads, err := store.ListBoardBeads(beads.IssueFilter{
		Labels: []string{"agent"},
		Status: store.StatusPtr(beads.StatusOpen),
	})
	if err != nil {
		allOpen, _ := store.ListBoardBeads(beads.IssueFilter{
			Status: store.StatusPtr(beads.StatusOpen),
		})
		var filtered []BoardBead
		for _, b := range allOpen {
			for _, l := range b.Labels {
				if l == "agent" {
					filtered = append(filtered, b)
					break
				}
			}
		}
		agentBeads = filtered
	}

	inProgress, _ := store.ListBoardBeads(beads.IssueFilter{
		Status: store.StatusPtr(beads.StatusInProgress),
	})

	ownerWork := make(map[string]BoardBead)
	for _, b := range inProgress {
		owner := BeadOwnerLabel(b)
		if owner != "" {
			ownerWork[owner] = b
		}
	}

	attemptWork, attemptUpdatedAt := BuildAttemptWorkMap(inProgress, ownerWork)

	exclude := map[string]bool{
		"steward": true, "mayor": true,
		"spi": true, "awell": true,
	}

	var agents []RosterAgent
	for _, ab := range agentBeads {
		name := ""
		for _, l := range ab.Labels {
			if strings.HasPrefix(l, "name:") {
				name = l[5:]
				break
			}
		}
		if name == "" || exclude[name] {
			continue
		}

		role := "wizard"
		for _, l := range ab.Labels {
			if strings.Contains(l, "artificer") {
				role = "artificer"
			}
		}

		agent := RosterAgent{
			Name:         name,
			Role:         role,
			Timeout:      timeout,
			RegisteredAt: ab.CreatedAt,
		}

		if work, ok := ownerWork[name]; ok {
			agent.Status = "working"
			agent.BeadID = work.ID
			agent.BeadTitle = work.Title
			if t, err := time.Parse(time.RFC3339, work.UpdatedAt); err == nil {
				agent.Elapsed = time.Since(t).Round(time.Second)
				agent.Remaining = timeout - agent.Elapsed
				if agent.Remaining < 0 {
					agent.Remaining = 0
				}
			}
		} else if work, ok := attemptWork[name]; ok {
			agent.Status = "working"
			agent.BeadID = work.ID
			agent.BeadTitle = work.Title
			if t, err := time.Parse(time.RFC3339, attemptUpdatedAt[name]); err == nil {
				agent.Elapsed = time.Since(t).Round(time.Second)
				agent.Remaining = timeout - agent.Elapsed
				if agent.Remaining < 0 {
					agent.Remaining = 0
				}
			}
		} else {
			agent.Status = "idle"
		}

		agents = append(agents, agent)
	}

	return agents
}

// BuildAttemptWorkMap scans inProgress beads for attempt beads and returns two maps:
// attemptWork[agentName] = work item BoardBead (the attempt's parent)
// attemptUpdatedAt[agentName] = attempt bead UpdatedAt (for elapsed time calculation)
func BuildAttemptWorkMap(inProgress []BoardBead, ownerWork map[string]BoardBead) (map[string]BoardBead, map[string]string) {
	byID := make(map[string]BoardBead, len(inProgress))
	for _, b := range inProgress {
		byID[b.ID] = b
	}

	attemptWork := make(map[string]BoardBead)
	attemptUpdatedAt := make(map[string]string)
	for _, b := range inProgress {
		if !store.IsAttemptBoardBead(b) {
			continue
		}
		agentName := ""
		for _, l := range b.Labels {
			if strings.HasPrefix(l, "agent:") {
				agentName = l[6:]
				break
			}
		}
		if agentName == "" || b.Parent == "" {
			continue
		}
		if _, covered := ownerWork[agentName]; covered {
			continue
		}
		workItem, ok := byID[b.Parent]
		if !ok {
			continue
		}
		if _, already := attemptWork[agentName]; already {
			continue
		}
		attemptWork[agentName] = workItem
		attemptUpdatedAt[agentName] = b.UpdatedAt
	}
	return attemptWork, attemptUpdatedAt
}

// RosterFromLocalWizards builds a roster from the local wizard registry (process mode).
func RosterFromLocalWizards(timeout time.Duration, deps RosterDeps) []RosterAgent {
	allAgents := deps.LoadWizardRegistry()
	before := len(allAgents)
	allAgents = deps.CleanDeadWizards(allAgents)
	if len(allAgents) < before && deps.SaveWizardRegistry != nil {
		deps.SaveWizardRegistry(allAgents)
	}

	if len(allAgents) == 0 {
		return nil
	}

	var agents []RosterAgent
	for _, w := range allAgents {
		role := "wizard"
		if strings.Contains(w.Name, "-review") || w.Phase == "review" {
			role = "reviewer"
		}

		agentTimeout := timeout
		if w.BeadID != "" {
			if bead, berr := store.GetBead(w.BeadID); berr == nil && bead.Type == "epic" {
				agentTimeout = 0
			}
		}

		agent := RosterAgent{
			Name:    w.Name,
			Role:    role,
			BeadID:  w.BeadID,
			Timeout: agentTimeout,
			Phase:   w.Phase,
		}

		isAlive := w.PID > 0 && deps.ProcessAlive(w.PID)

		if isAlive {
			agent.Status = "working"
			if t, err := time.Parse(time.RFC3339, w.StartedAt); err == nil {
				agent.Elapsed = time.Since(t).Round(time.Second)
				agent.Remaining = timeout - agent.Elapsed
				if agent.Remaining < 0 {
					agent.Remaining = 0
				}
			}
			if w.PhaseStartedAt != "" {
				if t, err := time.Parse(time.RFC3339, w.PhaseStartedAt); err == nil {
					agent.PhaseElapsed = time.Since(t).Round(time.Second)
				}
			}
		} else {
			agent.Status = "done"
		}

		agents = append(agents, agent)
	}
	return agents
}

// EnrichRosterAgents fills in missing bead metadata from the store.
func EnrichRosterAgents(agents []RosterAgent) []RosterAgent {
	if len(agents) == 0 {
		return agents
	}

	beadCache := make(map[string]Bead)
	contextCache := make(map[string]rosterBeadContext)

	for i := range agents {
		if agents[i].BeadID == "" {
			continue
		}

		if attempt, err := store.GetActiveAttempt(agents[i].BeadID); err == nil && attempt != nil {
			attemptAgent := store.HasLabel(*attempt, "agent:")
			if attemptAgent != "" && agents[i].Name == "" {
				agents[i].Name = attemptAgent
			}
		}

		ctx := resolveRosterBeadContext(agents[i].BeadID, beadCache, contextCache)
		if agents[i].BeadTitle == "" {
			agents[i].BeadTitle = ctx.BeadTitle
		}
		if agents[i].EpicID == "" {
			agents[i].EpicID = ctx.EpicID
		}
		if agents[i].EpicTitle == "" {
			agents[i].EpicTitle = ctx.EpicTitle
		}
		if bead, ok := beadCache[agents[i].BeadID]; ok {
			if phase := GetPhase(bead); phase != "" {
				agents[i].Phase = phase
			}
		}
	}

	return agents
}

func resolveRosterBeadContext(id string, beadCache map[string]Bead, contextCache map[string]rosterBeadContext) rosterBeadContext {
	if ctx, ok := contextCache[id]; ok {
		return ctx
	}

	bead, ok := loadRosterBead(id, beadCache)
	if !ok {
		contextCache[id] = rosterBeadContext{}
		return rosterBeadContext{}
	}

	ctx := rosterBeadContext{
		BeadTitle: bead.Title,
	}

	if bead.Type == "epic" {
		ctx.EpicID = bead.ID
		ctx.EpicTitle = bead.Title
	} else if bead.Parent != "" {
		parentCtx := resolveRosterBeadContext(bead.Parent, beadCache, contextCache)
		ctx.EpicID = parentCtx.EpicID
		ctx.EpicTitle = parentCtx.EpicTitle
	}

	contextCache[id] = ctx
	return ctx
}

func loadRosterBead(id string, beadCache map[string]Bead) (Bead, bool) {
	if bead, ok := beadCache[id]; ok {
		return bead, true
	}

	bead, err := store.GetBead(id)
	if err != nil {
		return Bead{}, false
	}
	beadCache[id] = bead
	return bead, true
}

// BuildRosterWorkItems collapses agents by bead into work items.
func BuildRosterWorkItems(agents []RosterAgent) []RosterWorkItem {
	type workItemAccumulator struct {
		item    RosterWorkItem
		primary RosterAgent
	}

	byBead := make(map[string]*workItemAccumulator)

	for _, agent := range agents {
		if agent.BeadID == "" {
			continue
		}

		entry, ok := byBead[agent.BeadID]
		if !ok {
			entry = &workItemAccumulator{
				item: RosterWorkItem{
					BeadID:    agent.BeadID,
					BeadTitle: agent.BeadTitle,
					EpicID:    agent.EpicID,
					EpicTitle: agent.EpicTitle,
					Status:    agent.Status,
				},
				primary: agent,
			}
			byBead[agent.BeadID] = entry
		}

		if entry.item.BeadTitle == "" && agent.BeadTitle != "" {
			entry.item.BeadTitle = agent.BeadTitle
		}
		if entry.item.EpicID == "" && agent.EpicID != "" {
			entry.item.EpicID = agent.EpicID
		}
		if entry.item.EpicTitle == "" && agent.EpicTitle != "" {
			entry.item.EpicTitle = agent.EpicTitle
		}
		if RosterStatusRank(agent.Status) > RosterStatusRank(entry.item.Status) {
			entry.item.Status = agent.Status
		}
		if PreferRosterPrimary(agent, entry.primary) {
			entry.primary = agent
		}
		entry.item.AgentNames = append(entry.item.AgentNames, agent.Name)
	}

	items := make([]RosterWorkItem, 0, len(byBead))
	for _, entry := range byBead {
		sort.Strings(entry.item.AgentNames)
		entry.item.Phase, entry.item.Elapsed, entry.item.Timeout = RosterAgentCountdown(entry.primary)
		items = append(items, entry.item)
	}

	sort.Slice(items, func(i, j int) bool {
		if RosterStatusRank(items[i].Status) != RosterStatusRank(items[j].Status) {
			return RosterStatusRank(items[i].Status) > RosterStatusRank(items[j].Status)
		}
		if items[i].EpicID != items[j].EpicID {
			return items[i].EpicID < items[j].EpicID
		}
		if items[i].BeadID != items[j].BeadID {
			return items[i].BeadID < items[j].BeadID
		}
		return items[i].BeadTitle < items[j].BeadTitle
	})

	return items
}

// GroupRosterWorkItemsByEpic groups work items by their epic.
func GroupRosterWorkItemsByEpic(items []RosterWorkItem) []RosterEpicGroup {
	groupsByKey := make(map[string]*RosterEpicGroup)
	var order []string

	for _, item := range items {
		key := item.EpicID
		title := item.EpicTitle
		if key == "" {
			key = "__standalone__"
			title = "Standalone Work"
		}

		group, ok := groupsByKey[key]
		if !ok {
			group = &RosterEpicGroup{
				ID:    item.EpicID,
				Title: title,
			}
			groupsByKey[key] = group
			order = append(order, key)
		}
		group.Items = append(group.Items, item)
	}

	sort.Slice(order, func(i, j int) bool {
		if order[i] == "__standalone__" {
			return false
		}
		if order[j] == "__standalone__" {
			return true
		}
		return order[i] < order[j]
	})

	groups := make([]RosterEpicGroup, 0, len(order))
	for _, key := range order {
		group := groupsByKey[key]
		sort.Slice(group.Items, func(i, j int) bool {
			if RosterStatusRank(group.Items[i].Status) != RosterStatusRank(group.Items[j].Status) {
				return RosterStatusRank(group.Items[i].Status) > RosterStatusRank(group.Items[j].Status)
			}
			if group.Items[i].BeadID != group.Items[j].BeadID {
				return group.Items[i].BeadID < group.Items[j].BeadID
			}
			return group.Items[i].BeadTitle < group.Items[j].BeadTitle
		})
		groups = append(groups, *group)
	}

	return groups
}

// RosterUnassignedAgents returns agents with no bead assignment.
func RosterUnassignedAgents(agents []RosterAgent) []RosterAgent {
	var unassigned []RosterAgent
	for _, agent := range agents {
		if agent.BeadID == "" {
			unassigned = append(unassigned, agent)
		}
	}
	sort.Slice(unassigned, func(i, j int) bool {
		if RosterStatusRank(unassigned[i].Status) != RosterStatusRank(unassigned[j].Status) {
			return RosterStatusRank(unassigned[i].Status) > RosterStatusRank(unassigned[j].Status)
		}
		return unassigned[i].Name < unassigned[j].Name
	})
	return unassigned
}

// CountActiveRosterWorkItems returns the count of working/provisioning items.
func CountActiveRosterWorkItems(items []RosterWorkItem) int {
	active := 0
	for _, item := range items {
		if item.Status == "working" || item.Status == "provisioning" {
			active++
		}
	}
	return active
}

// RosterStatusRank returns a priority ranking for agent statuses.
func RosterStatusRank(status string) int {
	switch status {
	case "working":
		return 5
	case "provisioning":
		return 4
	case "idle":
		return 3
	case "done":
		return 2
	case "offline":
		return 1
	default:
		return 0
	}
}

// PreferRosterPrimary decides whether a candidate should replace the current primary agent.
func PreferRosterPrimary(candidate, current RosterAgent) bool {
	if RosterStatusRank(candidate.Status) != RosterStatusRank(current.Status) {
		return RosterStatusRank(candidate.Status) > RosterStatusRank(current.Status)
	}
	if candidate.Phase != "" && current.Phase == "" {
		return true
	}
	if candidate.Phase == "" && current.Phase != "" {
		return false
	}
	if rosterNameRank(candidate.Name) != rosterNameRank(current.Name) {
		return rosterNameRank(candidate.Name) > rosterNameRank(current.Name)
	}
	if len(candidate.Name) != len(current.Name) {
		return len(candidate.Name) < len(current.Name)
	}
	return candidate.Name < current.Name
}

func rosterNameRank(name string) int {
	switch {
	case strings.Contains(name, "-impl"),
		strings.Contains(name, "-review"),
		strings.Contains(name, "-fix"),
		strings.Contains(name, "-design"):
		return 1
	default:
		return 2
	}
}

// RosterAgentCountdown returns phase, elapsed, and timeout for an agent.
func RosterAgentCountdown(agent RosterAgent) (string, time.Duration, time.Duration) {
	elapsed := agent.Elapsed
	timeout := agent.Timeout
	if agent.Phase != "" && agent.PhaseElapsed > 0 {
		elapsed = agent.PhaseElapsed
		timeout = RosterPhaseTimeout(agent.Phase, agent.Timeout)
	}
	return agent.Phase, elapsed, timeout
}

// BuildSummary builds a RosterSummary from agents.
func BuildSummary(agents []RosterAgent, timeout time.Duration) RosterSummary {
	s := RosterSummary{Agents: agents, Timeout: timeout}
	for _, a := range agents {
		s.Wizards++
		switch a.Status {
		case "working", "provisioning":
			s.Busy++
		case "idle":
			s.Idle++
		case "offline":
			s.Offline++
		}
	}
	return s
}

// JSONOut writes a RosterSummary as JSON to stdout.
func JSONOut(s RosterSummary) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

// PrintRoster renders a roster summary to stdout with ANSI formatting.
func PrintRoster(s RosterSummary) {
	if len(s.Agents) == 0 {
		fmt.Printf("%sTOWER ROSTER — empty%s\n", Bold, Reset)
		fmt.Printf("\n%sNo wizards summoned. Use %sspire summon N%s to conjure capacity.%s\n", Dim, Reset+Bold, Reset+Dim, Reset)
		return
	}

	workItems := BuildRosterWorkItems(s.Agents)
	epicGroups := GroupRosterWorkItemsByEpic(workItems)
	unassigned := RosterUnassignedAgents(s.Agents)
	activeWorkItems := CountActiveRosterWorkItems(workItems)

	fmt.Printf("%sTOWER ROSTER%s — %d active work item(s), %d agent process(es), timeout %s\n", Bold, Reset, activeWorkItems, s.Wizards, s.Timeout)
	fmt.Println()

	if len(epicGroups) == 0 {
		fmt.Printf("%sNo active bead assignments.%s\n", Dim, Reset)
	} else {
		for i, group := range epicGroups {
			if i > 0 {
				fmt.Println()
			}
			if group.ID != "" {
				title := group.Title
				if title == "" {
					title = "Untitled epic"
				}
				fmt.Printf("%sEPIC %s%s%s — %s\n", Bold, Cyan, group.ID, Reset+Bold, title)
			} else {
				fmt.Printf("%sSTANDALONE WORK%s\n", Bold, Reset)
			}

			for _, item := range group.Items {
				icon, _ := RosterStatusDisplay(item.Status)
				phaseStr := ""
				if item.Phase != "" {
					phaseStr = fmt.Sprintf("%s[%s]%s ", Yellow, item.Phase, Reset)
				}
				title := item.BeadTitle
				if title == "" {
					title = "Untitled bead"
				}
				fmt.Printf("  %s %-12s %s%s", icon, item.BeadID, phaseStr, Truncate(title, 52))
				if item.Elapsed > 0 {
					if item.Timeout > 0 {
						fmt.Printf("  %s", RenderCountdown(item.Elapsed, item.Timeout))
					} else {
						fmt.Printf("  %s", item.Elapsed.Round(time.Second))
					}
				}
				fmt.Println()
				if len(item.AgentNames) == 1 {
					fmt.Printf("      %sagent:%s %s\n", Dim, Reset, item.AgentNames[0])
				} else {
					fmt.Printf("      %sagent:%s %s %s(+%d helpers)%s\n", Dim, Reset, item.AgentNames[0], Dim, len(item.AgentNames)-1, Reset)
				}
			}
		}
	}

	if len(unassigned) > 0 {
		fmt.Println()
		fmt.Printf("%sUNASSIGNED AGENTS%s\n", Bold, Reset)
		for _, agent := range unassigned {
			icon, statusStr := RosterStatusDisplay(agent.Status)
			fmt.Printf("  %s %-18s %s\n", icon, agent.Name, statusStr)
		}
	}

	fmt.Println()
	fmt.Printf("Agent processes: %d/%d busy", s.Busy, s.Wizards)
	if s.Idle > 0 {
		fmt.Printf(" (%s%d idle%s)", Green, s.Idle, Reset)
	}
	fmt.Println()
}

// RosterStatusDisplay returns an icon and label for an agent status.
func RosterStatusDisplay(status string) (string, string) {
	switch status {
	case "working":
		return Cyan + "◐" + Reset, fmt.Sprintf("%sworking%s", Cyan, Reset)
	case "provisioning":
		return Yellow + "◔" + Reset, fmt.Sprintf("%sprovisioning%s", Yellow, Reset)
	case "idle":
		return Green + "○" + Reset, fmt.Sprintf("%sidle%s", Green, Reset)
	case "done":
		return Dim + "✓" + Reset, fmt.Sprintf("%sdone%s", Dim, Reset)
	case "offline":
		return Dim + "×" + Reset, fmt.Sprintf("%soffline%s", Dim, Reset)
	default:
		return Dim + "?" + Reset, status
	}
}

// RosterPhaseTimeout returns the expected timeout for a given molecule phase.
func RosterPhaseTimeout(phase string, globalTimeout time.Duration) time.Duration {
	switch phase {
	case "design", "review-fix", "review":
		return 10 * time.Minute
	case "implement":
		return globalTimeout
	default:
		return globalTimeout
	}
}

// RenderCountdown renders an elapsed/timeout bar.
func RenderCountdown(elapsed, timeout time.Duration) string {
	barWidth := 10
	ratio := float64(elapsed) / float64(timeout)
	if ratio > 1 {
		ratio = 1
	}
	filled := int(ratio * float64(barWidth))
	if filled > barWidth {
		filled = barWidth
	}

	barColor := Green
	if ratio > 0.9 {
		barColor = Red
	} else if ratio > 0.7 {
		barColor = Yellow
	}

	remaining := timeout - elapsed
	if remaining < 0 {
		remaining = 0
	}

	elapsedStr := FormatDuration(elapsed)
	timeoutStr := FormatDuration(timeout)

	bar := fmt.Sprintf("%s%s%s%s",
		barColor, strings.Repeat("█", filled), Reset,
		strings.Repeat("░", barWidth-filled))

	if remaining == 0 {
		return fmt.Sprintf("%s%s / %s%s  %s  %sOVERTIME%s", Red, elapsedStr, timeoutStr, Reset, bar, Bold+Red, Reset)
	}

	return fmt.Sprintf("%s / %s  %s", elapsedStr, timeoutStr, bar)
}

// FormatDuration formats a duration as "Xm YYs" or "Xs".
func FormatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}
