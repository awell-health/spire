package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/steveyegge/beads"
)

// RosterAgent represents an agent registered in the tower.
type RosterAgent struct {
	Name         string        `json:"name"`
	Role         string        `json:"role"`       // "wizard", "artificer", "steward", "reviewer"
	Status       string        `json:"status"`     // "idle", "working", "offline"
	BeadID       string        `json:"bead_id"`    // current work (if working)
	BeadTitle    string        `json:"bead_title"` // current work title
	EpicID       string        `json:"epic_id,omitempty"`
	EpicTitle    string        `json:"epic_title,omitempty"`
	Phase        string        `json:"phase"`         // current molecule phase (e.g. "implement", "review")
	Elapsed      time.Duration `json:"elapsed"`       // time on current work
	PhaseElapsed time.Duration `json:"phase_elapsed"` // time in current phase
	Timeout      time.Duration `json:"timeout"`       // configured wizard timeout
	Remaining    time.Duration `json:"remaining"`     // time left (timeout - elapsed)
	RegisteredAt string        `json:"registered_at"`
}

// RosterSummary is the JSON output.
type RosterSummary struct {
	Agents  []RosterAgent `json:"agents"`
	Wizards int           `json:"wizards"`
	Busy    int           `json:"busy"`
	Idle    int           `json:"idle"`
	Offline int           `json:"offline"`
	Timeout time.Duration `json:"timeout"` // configured timeout from spire.yaml
}

type rosterBeadContext struct {
	BeadTitle string
	EpicID    string
	EpicTitle string
}

type rosterWorkItem struct {
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

type rosterEpicGroup struct {
	ID    string
	Title string
	Items []rosterWorkItem
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

func cmdRoster(args []string) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	flagJSON := false
	for _, arg := range args {
		switch arg {
		case "--json":
			flagJSON = true
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire roster [--json]", arg)
		}
	}

	// Load stale + timeout from repo config.
	cwd, _ := os.Getwd()
	cfg, _ := repoconfig.Load(cwd)
	stale := 10 * time.Minute   // guideline — when we flag it
	timeout := 15 * time.Minute // kill — when we shut it down
	if cfg != nil {
		if cfg.Agent.Stale != "" {
			if d, err := time.ParseDuration(cfg.Agent.Stale); err == nil {
				stale = d
			}
		}
		if cfg.Agent.Timeout != "" {
			if d, err := time.ParseDuration(cfg.Agent.Timeout); err == nil {
				timeout = d
			}
		}
	}
	_ = stale // used in future enrichment for bar color thresholds

	// Try k8s first — it has real pod start times.
	if agents, err := rosterFromK8s(timeout); err == nil && len(agents) > 0 {
		agents = enrichRosterAgents(agents)
		summary := buildSummary(agents, timeout)
		if flagJSON {
			return jsonOut(summary)
		}
		printRoster(summary)
		return nil
	}

	// Local wizards from wizard registry (process mode).
	if localAgents := rosterFromLocalWizards(timeout); len(localAgents) > 0 {
		localAgents = enrichRosterAgents(localAgents)
		summary := buildSummary(localAgents, timeout)
		if flagJSON {
			return jsonOut(summary)
		}
		printRoster(summary)
		return nil
	}

	// Fallback: beads-based roster (no countdown, no pod times).
	agents := rosterFromBeads(timeout)
	agents = enrichRosterAgents(agents)
	summary := buildSummary(agents, timeout)
	if flagJSON {
		return jsonOut(summary)
	}
	printRoster(summary)
	return nil
}

// rosterFromK8s queries k8s for wizard pods and their start times.
func rosterFromK8s(timeout time.Duration) ([]RosterAgent, error) {
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

	// Also get SpireAgent CRs for idle agents (those without pods).
	var agentNames []string
	if names, err := exec.Command("kubectl", "get", "spireagent", "-n", "spire",
		"-o", "jsonpath={.items[*].metadata.name}").Output(); err == nil {
		for _, n := range strings.Fields(strings.TrimSpace(string(names))) {
			if strings.HasPrefix(n, "wizard-") || strings.HasPrefix(n, "artificer") {
				agentNames = append(agentNames, n)
			}
		}
	}

	// Build agents from pods (working agents).
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

		// Parse pod start time for countdown.
		if pod.Status.StartTime != "" {
			if t, err := time.Parse(time.RFC3339, pod.Status.StartTime); err == nil {
				agent.Elapsed = time.Since(t).Round(time.Second)
				agent.Remaining = timeout - agent.Elapsed
				if agent.Remaining < 0 {
					agent.Remaining = 0
				}
			}
		}

		// Pod phase.
		switch pod.Status.Phase {
		case "Succeeded", "Failed":
			agent.Status = "done"
		case "Pending":
			agent.Status = "provisioning"
		}

		podAgents[agentName] = agent
	}

	// Add idle agents (CRs without pods).
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

// rosterFromBeads builds a roster from bead state (no k8s).
func rosterFromBeads(timeout time.Duration) []RosterAgent {
	agentBeads, err := storeListBoardBeads(beads.IssueFilter{
		Labels: []string{"agent"},
		Status: statusPtr(beads.StatusOpen),
	})
	if err != nil {
		allOpen, _ := storeListBoardBeads(beads.IssueFilter{
			Status: statusPtr(beads.StatusOpen),
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

	inProgress, _ := storeListBoardBeads(beads.IssueFilter{
		Status: statusPtr(beads.StatusInProgress),
	})

	ownerWork := make(map[string]BoardBead)
	for _, b := range inProgress {
		owner := beadOwnerLabel(b)
		if owner != "" {
			ownerWork[owner] = b
		}
	}

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
			// Best effort elapsed from bead updated_at.
			if t, err := time.Parse(time.RFC3339, work.UpdatedAt); err == nil {
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

// rosterFromLocalWizards builds a roster from the local wizard registry (process mode).
func rosterFromLocalWizards(timeout time.Duration) []RosterAgent {
	reg := loadWizardRegistry()
	reg = cleanDeadWizards(reg)

	if len(reg.Wizards) == 0 {
		return nil
	}

	var agents []RosterAgent
	for _, w := range reg.Wizards {
		// Determine role from wizard name or phase.
		role := "wizard"
		if strings.Contains(w.Name, "-review") || w.Phase == "review" {
			role = "reviewer"
		}

		agent := RosterAgent{
			Name:    w.Name,
			Role:    role,
			BeadID:  w.BeadID,
			Timeout: timeout,
			Phase:   w.Phase, // empty string if old registry format
		}

		// Check if process is alive.
		isAlive := w.PID > 0 && processAlive(w.PID)

		if isAlive {
			agent.Status = "working"
			if t, err := time.Parse(time.RFC3339, w.StartedAt); err == nil {
				agent.Elapsed = time.Since(t).Round(time.Second)
				agent.Remaining = timeout - agent.Elapsed
				if agent.Remaining < 0 {
					agent.Remaining = 0
				}
			}
			// Phase elapsed time.
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

func enrichRosterAgents(agents []RosterAgent) []RosterAgent {
	if len(agents) == 0 {
		return agents
	}

	beadCache := make(map[string]Bead)
	contextCache := make(map[string]rosterBeadContext)

	for i := range agents {
		if agents[i].BeadID == "" {
			continue
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
		// Phase: always read from bead label (source of truth).
		// The registry entry's Phase field is stale once the executor advances phases
		// via setPhase() without updating wizards.json.
		if bead, ok := beadCache[agents[i].BeadID]; ok {
			if phase := getPhase(bead); phase != "" {
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

	bead, err := storeGetBead(id)
	if err != nil {
		return Bead{}, false
	}
	beadCache[id] = bead
	return bead, true
}

func buildRosterWorkItems(agents []RosterAgent) []rosterWorkItem {
	type workItemAccumulator struct {
		item    rosterWorkItem
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
				item: rosterWorkItem{
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
		if rosterStatusRank(agent.Status) > rosterStatusRank(entry.item.Status) {
			entry.item.Status = agent.Status
		}
		if preferRosterPrimary(agent, entry.primary) {
			entry.primary = agent
		}
		entry.item.AgentNames = append(entry.item.AgentNames, agent.Name)
	}

	items := make([]rosterWorkItem, 0, len(byBead))
	for _, entry := range byBead {
		sort.Strings(entry.item.AgentNames)
		entry.item.Phase, entry.item.Elapsed, entry.item.Timeout = rosterAgentCountdown(entry.primary)
		items = append(items, entry.item)
	}

	sort.Slice(items, func(i, j int) bool {
		if rosterStatusRank(items[i].Status) != rosterStatusRank(items[j].Status) {
			return rosterStatusRank(items[i].Status) > rosterStatusRank(items[j].Status)
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

func groupRosterWorkItemsByEpic(items []rosterWorkItem) []rosterEpicGroup {
	groupsByKey := make(map[string]*rosterEpicGroup)
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
			group = &rosterEpicGroup{
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

	groups := make([]rosterEpicGroup, 0, len(order))
	for _, key := range order {
		group := groupsByKey[key]
		sort.Slice(group.Items, func(i, j int) bool {
			if rosterStatusRank(group.Items[i].Status) != rosterStatusRank(group.Items[j].Status) {
				return rosterStatusRank(group.Items[i].Status) > rosterStatusRank(group.Items[j].Status)
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

func rosterUnassignedAgents(agents []RosterAgent) []RosterAgent {
	var unassigned []RosterAgent
	for _, agent := range agents {
		if agent.BeadID == "" {
			unassigned = append(unassigned, agent)
		}
	}
	sort.Slice(unassigned, func(i, j int) bool {
		if rosterStatusRank(unassigned[i].Status) != rosterStatusRank(unassigned[j].Status) {
			return rosterStatusRank(unassigned[i].Status) > rosterStatusRank(unassigned[j].Status)
		}
		return unassigned[i].Name < unassigned[j].Name
	})
	return unassigned
}

func countActiveRosterWorkItems(items []rosterWorkItem) int {
	active := 0
	for _, item := range items {
		if item.Status == "working" || item.Status == "provisioning" {
			active++
		}
	}
	return active
}

func rosterStatusRank(status string) int {
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

func preferRosterPrimary(candidate, current RosterAgent) bool {
	if rosterStatusRank(candidate.Status) != rosterStatusRank(current.Status) {
		return rosterStatusRank(candidate.Status) > rosterStatusRank(current.Status)
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

func rosterAgentCountdown(agent RosterAgent) (string, time.Duration, time.Duration) {
	elapsed := agent.Elapsed
	timeout := agent.Timeout
	if agent.Phase != "" && agent.PhaseElapsed > 0 {
		elapsed = agent.PhaseElapsed
		timeout = rosterPhaseTimeout(agent.Phase, agent.Timeout)
	}
	return agent.Phase, elapsed, timeout
}

func buildSummary(agents []RosterAgent, timeout time.Duration) RosterSummary {
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

func jsonOut(s RosterSummary) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

func printRoster(s RosterSummary) {
	if len(s.Agents) == 0 {
		fmt.Printf("%sTOWER ROSTER — empty%s\n", bold, reset)
		fmt.Printf("\n%sNo wizards summoned. Use %sspire summon N%s to conjure capacity.%s\n", dim, reset+bold, reset+dim, reset)
		return
	}

	workItems := buildRosterWorkItems(s.Agents)
	epicGroups := groupRosterWorkItemsByEpic(workItems)
	unassigned := rosterUnassignedAgents(s.Agents)
	activeWorkItems := countActiveRosterWorkItems(workItems)

	fmt.Printf("%sTOWER ROSTER%s — %d active work item(s), %d agent process(es), timeout %s\n", bold, reset, activeWorkItems, s.Wizards, s.Timeout)
	fmt.Println()

	if len(epicGroups) == 0 {
		fmt.Printf("%sNo active bead assignments.%s\n", dim, reset)
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
				fmt.Printf("%sEPIC %s%s%s — %s\n", bold, cyan, group.ID, reset+bold, title)
			} else {
				fmt.Printf("%sSTANDALONE WORK%s\n", bold, reset)
			}

			for _, item := range group.Items {
				icon, _ := rosterStatusDisplay(item.Status)
				phaseStr := ""
				if item.Phase != "" {
					phaseStr = fmt.Sprintf("%s[%s]%s ", yellow, item.Phase, reset)
				}
				title := item.BeadTitle
				if title == "" {
					title = "Untitled bead"
				}
				fmt.Printf("  %s %-12s %s%s", icon, item.BeadID, phaseStr, truncate(title, 52))
				if item.Timeout > 0 && item.Elapsed > 0 {
					fmt.Printf("  %s", renderCountdown(item.Elapsed, item.Timeout))
				}
				fmt.Println()
				fmt.Printf("      %sagents:%s %s\n", dim, reset, strings.Join(item.AgentNames, ", "))
			}
		}
	}

	if len(unassigned) > 0 {
		fmt.Println()
		fmt.Printf("%sUNASSIGNED AGENTS%s\n", bold, reset)
		for _, agent := range unassigned {
			icon, statusStr := rosterStatusDisplay(agent.Status)
			fmt.Printf("  %s %-18s %s\n", icon, agent.Name, statusStr)
		}
	}

	fmt.Println()
	fmt.Printf("Agent processes: %d/%d busy", s.Busy, s.Wizards)
	if s.Idle > 0 {
		fmt.Printf(" (%s%d idle%s)", green, s.Idle, reset)
	}
	fmt.Println()
}

func rosterStatusDisplay(status string) (string, string) {
	switch status {
	case "working":
		return cyan + "◐" + reset, fmt.Sprintf("%sworking%s", cyan, reset)
	case "provisioning":
		return yellow + "◔" + reset, fmt.Sprintf("%sprovisioning%s", yellow, reset)
	case "idle":
		return green + "○" + reset, fmt.Sprintf("%sidle%s", green, reset)
	case "done":
		return dim + "✓" + reset, fmt.Sprintf("%sdone%s", dim, reset)
	case "offline":
		return dim + "×" + reset, fmt.Sprintf("%soffline%s", dim, reset)
	default:
		return dim + "?" + reset, status
	}
}

// rosterPhaseTimeout returns the expected timeout for a given molecule phase.
// Design and review-fix get 10m; implement gets the global timeout (15m default);
// review gets 10m (Opus call is fast, but leave room for tests).
func rosterPhaseTimeout(phase string, globalTimeout time.Duration) time.Duration {
	switch phase {
	case "design", "review-fix", "review":
		return 10 * time.Minute
	case "implement":
		return globalTimeout
	default:
		return globalTimeout
	}
}

// renderCountdown renders an elapsed/timeout bar like: 7m12s / 10m  ███████░░░
func renderCountdown(elapsed, timeout time.Duration) string {
	barWidth := 10
	ratio := float64(elapsed) / float64(timeout)
	if ratio > 1 {
		ratio = 1
	}
	filled := int(ratio * float64(barWidth))
	if filled > barWidth {
		filled = barWidth
	}

	// Color: green < 70%, yellow 70-90%, red > 90%.
	barColor := green
	if ratio > 0.9 {
		barColor = red
	} else if ratio > 0.7 {
		barColor = yellow
	}

	remaining := timeout - elapsed
	if remaining < 0 {
		remaining = 0
	}

	elapsedStr := formatDuration(elapsed)
	timeoutStr := formatDuration(timeout)

	bar := fmt.Sprintf("%s%s%s%s",
		barColor, strings.Repeat("█", filled), reset,
		strings.Repeat("░", barWidth-filled))

	if remaining == 0 {
		return fmt.Sprintf("%s%s / %s%s  %s  %sOVERTIME%s", red, elapsedStr, timeoutStr, reset, bar, bold+red, reset)
	}

	return fmt.Sprintf("%s / %s  %s", elapsedStr, timeoutStr, bar)
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}
