package board

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// ErrAttachedRosterNotImplemented signals that the active tower is in
// attached-reserved mode, for which no live-roster source is wired yet.
// Callers (gateway, CLI) translate this into a typed not-implemented
// response rather than silently falling back to a different mode's
// source. Mirrors the typed-not-implemented shape used by spi-jsxa3v
// for summon/dismiss.
var ErrAttachedRosterNotImplemented = errors.New("live roster: attached-reserved mode is not yet supported")

// RosterAgent represents an agent registered in the tower.
//
// Archmage records which archmage spawned/owns this row so the gateway can
// surface per-archmage origin when multiple desktops attach through the
// same cluster tower. Empty when the source-of-truth (local registry entry
// or operator pod label) carries no attribution.
type RosterAgent struct {
	Name         string        `json:"name"`
	Role         string        `json:"role"`
	Status       string        `json:"status"`
	BeadID       string        `json:"bead_id"`
	BeadTitle    string        `json:"bead_title"`
	EpicID       string        `json:"epic_id,omitempty"`
	EpicTitle    string        `json:"epic_title,omitempty"`
	Phase   string        `json:"phase"`
	Elapsed time.Duration `json:"elapsed"`
	Timeout time.Duration `json:"timeout"`
	Remaining    time.Duration `json:"remaining"`
	RegisteredAt string        `json:"registered_at"`
	Archmage     string        `json:"archmage,omitempty"`
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
	DAG        *DAGProgress      // executor DAG: steps, attempt, reviews
	EpicSub    *EpicChildSummary // subtask progress for epics
}

// RosterEpicGroup groups work items by epic.
type RosterEpicGroup struct {
	ID    string
	Title string
	Items []RosterWorkItem
}

// rosterClusterClient is the swappable seam RosterFromClusterRegistry
// uses to query Kubernetes. The production implementation hits the
// in-cluster API server via client-go; tests substitute a fake.
//
// Two narrow methods are enough for the roster query: pod listing for
// live wizard state, WizardGuild name listing for idle slots. Keeping
// the surface small so the fake in tests doesn't drag in the whole
// kubernetes.Interface.
type rosterClusterClient interface {
	ListWizardPods(ctx context.Context, namespace string) ([]corev1.Pod, error)
	ListWizardGuildNames(ctx context.Context, namespace string) ([]string, error)
}

// newRosterClusterClient is the package-level factory swapped in tests.
// Production callers leave this alone — it dials the API server via
// in-cluster service-account creds and falls back to a kubeconfig file
// for local-development runs of the gateway.
var newRosterClusterClient = newRealRosterClusterClient

func newRealRosterClusterClient() (rosterClusterClient, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			rules, &clientcmd.ConfigOverrides{},
		).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("k8s config: %w", err)
		}
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("k8s clientset: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("k8s dynamic client: %w", err)
	}
	return &realRosterClusterClient{cs: cs, dyn: dyn}, nil
}

type realRosterClusterClient struct {
	cs  kubernetes.Interface
	dyn dynamic.Interface
}

func (r *realRosterClusterClient) ListWizardPods(ctx context.Context, namespace string) ([]corev1.Pod, error) {
	list, err := r.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "spire.awell.io/managed=true",
	})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// wizardGuildGVR is the GroupVersionResource for the WizardGuild CRD.
// Hardcoded here rather than imported from the operator module to keep
// pkg/board independent of the operator's CRD types — we only need the
// guild name, which the unstructured/dynamic client returns.
var wizardGuildGVR = schema.GroupVersionResource{
	Group:    "spire.awell.io",
	Version:  "v1alpha1",
	Resource: "wizardguilds",
}

func (r *realRosterClusterClient) ListWizardGuildNames(ctx context.Context, namespace string) ([]string, error) {
	list, err := r.dyn.Resource(wizardGuildGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		names = append(names, item.GetName())
	}
	return names, nil
}

// RosterDeps holds external dependencies needed by the local-native
// roster path. LoadWizardRegistry surfaces registry read errors so
// transient JSON parse failures do not silently masquerade as an empty
// registry — see spi-rx6bf6 for the gateway-vs-CLI divergence that
// motivated the migration off agent.LoadRegistry's error-swallowing
// variant.
type RosterDeps struct {
	LoadWizardRegistry func() ([]LocalAgent, error)
	SaveWizardRegistry func([]LocalAgent)
	CleanDeadWizards   func([]LocalAgent) []LocalAgent
	ProcessAlive       func(pid int) bool

	// ResolveArchmage returns the archmage attribution for a single
	// registry entry — typically the tower's archmage name. Nil leaves
	// RosterAgent.Archmage empty so the gateway falls back to the cluster
	// tower's static archmage in the JSON response. Provided as a dep
	// (rather than read directly from config inside RosterFromLocalWizards)
	// so unit tests can inject deterministic attribution without touching
	// the real tower config.
	ResolveArchmage func(LocalAgent) string
}

// RosterFromClusterRegistry queries the cluster's operator-side wizard
// surface — the wizard pods labeled by the operator and the
// WizardGuild custom resources — and returns one RosterAgent per
// registered wizard. This is the cluster-native equivalent of reading
// wizards.json: the operator owns writes via reconciliation, clients
// query liveness but do not mutate.
//
// Used only by the cluster-native branch of LiveRoster; never by the
// local-native branch, which would otherwise observe pods from an
// unrelated cluster a developer's kubectl happens to point at
// (spi-rx6bf6).
//
// Uses an in-cluster k8s client (with kubeconfig fallback for local
// development) rather than shelling out to kubectl. The previous
// kubectl implementation broke when the gateway pod ran inside the
// cluster — gateway images don't ship a kubectl binary, so every
// /api/v1/roster call returned HTTP 500 "exit status 1". The k8s
// client uses the pod's service account when InClusterConfig
// succeeds; the gateway's KSA needs list permissions on pods and
// wizardguilds for this to work, which the chart's RBAC already
// grants to the operator KSA the gateway shares.
func RosterFromClusterRegistry(ctx context.Context, timeout time.Duration) ([]RosterAgent, error) {
	cli, err := newRosterClusterClient()
	if err != nil {
		return nil, fmt.Errorf("roster: build cluster client: %w", err)
	}

	pods, err := cli.ListWizardPods(ctx, "spire")
	if err != nil {
		return nil, fmt.Errorf("roster: list wizard pods: %w", err)
	}

	// Guild names provide the "idle slots" the roster surfaces when
	// no wizard is currently running for a given guild. A list error
	// here is non-fatal — we still return live pods, just without idle
	// rows. Matches the previous kubectl shell-out's behaviour: it
	// logged-and-swallowed the error from the second kubectl call.
	agentNames, _ := cli.ListWizardGuildNames(ctx, "spire")

	podAgents := make(map[string]RosterAgent)
	for _, pod := range pods {
		agentName := pod.Labels["spire.awell.io/agent"]
		beadID := pod.Labels["spire.awell.io/bead"]
		role := pod.Labels["spire.awell.io/role"]
		if role == "" {
			role = "wizard"
		}
		// Cluster pods that the operator stamps with the requesting
		// archmage's name flow per-archmage origin into the roster row.
		// Older pods without this label leave Archmage empty — that maps
		// to "tower default" on the gateway side.
		archmage := pod.Labels["spire.awell.io/archmage"]

		agent := RosterAgent{
			Name:     agentName,
			Role:     role,
			Status:   "working",
			BeadID:   beadID,
			Timeout:  timeout,
			Archmage: archmage,
		}

		if pod.Status.StartTime != nil {
			elapsed := time.Since(pod.Status.StartTime.Time).Round(time.Second)
			agent.Elapsed = elapsed
			agent.Remaining = timeout - elapsed
			if agent.Remaining < 0 {
				agent.Remaining = 0
			}
		}

		switch pod.Status.Phase {
		case corev1.PodSucceeded, corev1.PodFailed:
			agent.Status = "done"
		case corev1.PodPending:
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

// LegacyAgentRegistrationBeads scans the bead store for the
// agent-labeled registration beads created at desktop/agent startup
// (e.g. spi-jpurm, spi-nbwrdw, spi-nw5d95, spi-omuxk).
//
// These beads are NOT a live-roster signal: they reflect who once
// registered, never who is running right now. Treating them as a
// fallback once produced the gateway-vs-CLI divergence reported in
// spi-rx6bf6, where /api/v1/roster returned three idle desktop ghosts
// while `spire roster` correctly showed the live wizards from
// wizards.json.
//
// Live-roster callers (gateway handleRoster, cmd/spire/roster) MUST
// NOT consult this function. It is preserved for diagnostic and
// migration tooling — and so the four legacy registration beads remain
// readable while the cleanup decision (spi-rx6bf6 "Out of scope")
// lands in a follow-up.
//
// Deprecated: not a live signal. Prefer LiveRoster for the live
// wizard population.
func LegacyAgentRegistrationBeads(timeout time.Duration) []RosterAgent {
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

// RosterFromLocalWizards builds a roster from the local wizard
// registry (process mode). Read errors from LoadWizardRegistry are
// surfaced to the caller so a transient JSON parse / FS error does
// not silently report "no wizards" to a desktop or terminal client
// (spi-rx6bf6).
func RosterFromLocalWizards(timeout time.Duration, deps RosterDeps) ([]RosterAgent, error) {
	allAgents, err := deps.LoadWizardRegistry()
	if err != nil {
		return nil, err
	}
	before := len(allAgents)
	allAgents = deps.CleanDeadWizards(allAgents)
	if len(allAgents) < before && deps.SaveWizardRegistry != nil {
		deps.SaveWizardRegistry(allAgents)
	}

	if len(allAgents) == 0 {
		return nil, nil
	}

	var agents []RosterAgent
	for _, w := range allAgents {
		role := "wizard"
		if strings.Contains(w.Name, "-review") {
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
		}
		if deps.ResolveArchmage != nil {
			agent.Archmage = deps.ResolveArchmage(w)
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
		} else {
			agent.Status = "done"
		}

		agents = append(agents, agent)
	}
	return agents, nil
}

// LiveRoster dispatches to the correct roster source for the active
// tower's deployment mode and returns the live wizard population.
//
// Replaces the legacy hardcoded source-priority cascade
// (RosterFromK8s → RosterFromLocalWizards → RosterFromBeads) that
// used kubectl reachability and registration-bead presence as
// fallbacks. Each gateway/CLI surface (handleRoster, cmdRoster) calls
// this helper so the two surfaces never disagree on what "who is
// running" means (spi-rx6bf6).
//
// Mode contract:
//
//   - DeploymentModeLocalNative reads from the local wizard registry
//     via deps; an empty registry returns (nil, nil). It does NOT
//     fall through to LegacyAgentRegistrationBeads — stale
//     agent-labeled beads are not a live signal.
//   - DeploymentModeClusterNative reads from the operator-side
//     wizard registry via RosterFromClusterRegistry; an unreachable
//     cluster surfaces the error rather than silently falling back to
//     the local file. The deps are not consulted in this branch.
//   - DeploymentModeAttachedReserved returns
//     ErrAttachedRosterNotImplemented; gateway/CLI translate this
//     into a typed not-implemented response.
//   - Any other mode returns an error naming the mode.
func LiveRoster(ctx context.Context, mode config.DeploymentMode, timeout time.Duration, deps RosterDeps) ([]RosterAgent, error) {
	switch mode {
	case config.DeploymentModeLocalNative:
		return RosterFromLocalWizards(timeout, deps)
	case config.DeploymentModeClusterNative:
		return RosterFromClusterRegistry(ctx, timeout)
	case config.DeploymentModeAttachedReserved:
		return nil, ErrAttachedRosterNotImplemented
	case config.DeploymentModeUnknown:
		// spi-eep81n: an in-memory TowerConfig{} bypassing LoadTowerConfig
		// surfaces Unknown here. Refusing the call keeps roster from
		// silently reading the local wizard registry on behalf of a tower
		// whose topology was never declared.
		return nil, fmt.Errorf("live roster: tower has no DeploymentMode set; configure deployment_mode in the tower JSON")
	default:
		return nil, fmt.Errorf("live roster: unsupported deployment mode %q", mode)
	}
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
		// Fetch DAG progress for working items.
		if entry.item.Status == "working" || entry.item.Status == "provisioning" {
			entry.item.DAG = FetchDAGProgress(entry.item.BeadID)
			if entry.item.EpicID == entry.item.BeadID {
				entry.item.EpicSub = FetchEpicChildSummary(entry.item.BeadID)
			}
		}
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
	return agent.Phase, agent.Elapsed, agent.Timeout
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
				// Epic subtask progress inline.
				epicProgress := ""
				if item.EpicSub != nil {
					epicProgress = " " + Dim + RenderEpicProgressANSI(item.EpicSub) + Reset
				}
				fmt.Printf("  %s %-12s %s%s%s", icon, item.BeadID, phaseStr, Truncate(title, 48), epicProgress)
				if item.Elapsed > 0 {
					if item.Timeout > 0 {
						fmt.Printf("  %s", RenderCountdown(item.Elapsed, item.Timeout))
					} else {
						fmt.Printf("  %s", item.Elapsed.Round(time.Second))
					}
				}
				fmt.Println()

				// DAG: step pipeline.
				if item.DAG != nil && len(item.DAG.Steps) > 0 {
					fmt.Printf("      %s\n", RenderPipelineCompactANSI(item.DAG.Steps))
				}

				// DAG: active attempt.
				if item.DAG != nil && item.DAG.Attempt != nil {
					fmt.Printf("      %sattempt:%s %s\n", Dim, Reset, RenderAttemptANSI(item.DAG.Attempt))
				} else if len(item.AgentNames) == 1 {
					fmt.Printf("      %sagent:%s %s\n", Dim, Reset, item.AgentNames[0])
				} else if len(item.AgentNames) > 1 {
					fmt.Printf("      %sagent:%s %s %s(+%d helpers)%s\n", Dim, Reset, item.AgentNames[0], Dim, len(item.AgentNames)-1, Reset)
				}

				// DAG: review history.
				if item.DAG != nil && len(item.DAG.Reviews) > 0 {
					fmt.Printf("      %sreview:%s %s\n", Dim, Reset, RenderReviewSummaryANSI(item.DAG.Reviews))
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
