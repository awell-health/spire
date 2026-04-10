package controllers

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"time"

	"github.com/go-logr/logr"
	"github.com/steveyegge/beads"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spirev1 "github.com/awell-health/spire/operator/api/v1alpha1"
	"github.com/awell-health/spire/pkg/store"
)

// WorkloadAssigner matches pending SpireWorkloads to available SpireAgents.
type WorkloadAssigner struct {
	Client             client.Client
	Log                logr.Logger
	Namespace          string
	Interval           time.Duration
	StaleThreshold     time.Duration
	ReassignThreshold  time.Duration
	BeadsDir           string // path to .beads directory for store validation
}

// Start implements controller-runtime's Runnable interface.
func (a *WorkloadAssigner) Start(ctx context.Context) error {
	a.Run(ctx)
	return nil
}

func (a *WorkloadAssigner) Run(ctx context.Context) {
	a.Log.Info("workload assigner starting", "interval", a.Interval)
	ticker := time.NewTicker(a.Interval)
	defer ticker.Stop()

	a.cycle(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.cycle(ctx)
		}
	}
}

func (a *WorkloadAssigner) cycle(ctx context.Context) {
	// 1. Get pending workloads
	var workloads spirev1.SpireWorkloadList
	if err := a.Client.List(ctx, &workloads, client.InNamespace(a.Namespace)); err != nil {
		a.Log.Error(err, "failed to list workloads")
		return
	}

	// 2. Get agents
	var agents spirev1.SpireAgentList
	if err := a.Client.List(ctx, &agents, client.InNamespace(a.Namespace)); err != nil {
		a.Log.Error(err, "failed to list agents")
		return
	}

	// Build agent availability map
	agentMap := make(map[string]*spirev1.SpireAgent)
	for i := range agents.Items {
		agent := &agents.Items[i]
		agentMap[agent.Name] = agent
	}

	// 3. Validate pending workloads against the shared scheduling policy.
	// Uses store.GetSchedulableWork to ensure the same eligibility rules
	// (msg/template/active-attempt filtering) are applied here, preventing
	// drift between the bead watcher, steward, and this assigner.
	schedulable := a.getSchedulableSet()

	// 4. Assign pending workloads
	// Sort by priority (lower = more urgent)
	var pending []*spirev1.SpireWorkload
	for i := range workloads.Items {
		wl := &workloads.Items[i]
		switch wl.Status.Phase {
		case "Pending", "":
			// If the store is available, validate the bead is still schedulable.
			// If the store isn't available (schedulable == nil), fall through and
			// assign based on CRD state alone (graceful degradation).
			if schedulable != nil && !schedulable[wl.Spec.BeadID] {
				a.Log.Info("workload bead no longer schedulable, cancelling",
					"bead", wl.Spec.BeadID)
				wl.Status.Phase = "Cancelled"
				wl.Status.Message = "Bead no longer schedulable (scheduling policy)"
				a.Client.Status().Update(ctx, wl) //nolint
				continue
			}
			pending = append(pending, wl)
		case "Assigned", "InProgress", "Stale":
			a.checkStale(ctx, wl)
		}
	}

	sort.Slice(pending, func(i, j int) bool {
		return pending[i].Spec.Priority < pending[j].Spec.Priority
	})

	for _, wl := range pending {
		agent := a.selectAgent(agents.Items, wl)
		if agent == nil {
			continue // no available agent
		}

		a.assign(ctx, wl, agent)
	}
}

// getSchedulableSet returns a set of bead IDs that are currently schedulable
// according to the shared scheduling policy in store.GetSchedulableWork.
// Returns nil if the store is not available (graceful degradation).
func (a *WorkloadAssigner) getSchedulableSet() map[string]bool {
	if a.BeadsDir != "" {
		if _, err := store.Ensure(a.BeadsDir); err != nil {
			a.Log.Error(err, "failed to initialize bead store for scheduling validation")
			return nil
		}
	}

	result, err := store.GetSchedulableWork(beads.WorkFilter{})
	if err != nil {
		a.Log.Error(err, "store.GetSchedulableWork failed, skipping validation")
		return nil
	}

	// Log quarantined beads at Error level.
	for _, q := range result.Quarantined {
		a.Log.Error(q.Error, "quarantined bead (multiple open attempts)", "beadId", q.ID)
	}

	set := make(map[string]bool, len(result.Schedulable))
	for _, b := range result.Schedulable {
		set[b.ID] = true
	}
	return set
}

func (a *WorkloadAssigner) selectAgent(agents []spirev1.SpireAgent, wl *spirev1.SpireWorkload) *spirev1.SpireAgent {
	for i := range agents {
		agent := &agents[i]

		// Skip offline agents
		if agent.Status.Phase == "Offline" {
			continue
		}

		// Skip busy agents (at max concurrent)
		maxConcurrent := agent.Spec.MaxConcurrent
		if maxConcurrent == 0 {
			maxConcurrent = 1
		}
		if len(agent.Status.CurrentWork) >= maxConcurrent {
			continue
		}

		// Check prefix match (if agent has prefix restrictions)
		if len(agent.Spec.Prefixes) > 0 && len(wl.Spec.Prefixes) > 0 {
			if !prefixMatch(agent.Spec.Prefixes, wl.Spec.Prefixes) {
				continue
			}
		}

		return agent
	}
	return nil
}

func (a *WorkloadAssigner) assign(ctx context.Context, wl *spirev1.SpireWorkload, agent *spirev1.SpireAgent) {
	now := time.Now().UTC().Format(time.RFC3339)

	// Update workload status (agent-monitor will create the pod)
	wl.Status.Phase = "Assigned"
	wl.Status.AssignedTo = agent.Name
	wl.Status.AssignedAt = now
	wl.Status.Attempts++
	wl.Status.Message = fmt.Sprintf("Assigned to %s", agent.Name)
	if err := a.Client.Status().Update(ctx, wl); err != nil {
		a.Log.Error(err, "failed to update workload status")
	}

	// Update agent status
	agent.Status.Phase = "Working"
	agent.Status.CurrentWork = append(agent.Status.CurrentWork, wl.Spec.BeadID)
	if err := a.Client.Status().Update(ctx, agent); err != nil {
		a.Log.Error(err, "failed to update agent status")
	}

	a.Log.Info("assigned workload", "bead", wl.Spec.BeadID, "agent", agent.Name, "priority", wl.Spec.Priority)
}

func (a *WorkloadAssigner) checkStale(ctx context.Context, wl *spirev1.SpireWorkload) {
	if wl.Status.AssignedAt == "" {
		return
	}

	assignedAt, err := time.Parse(time.RFC3339, wl.Status.AssignedAt)
	if err != nil {
		return
	}

	age := time.Since(assignedAt)

	if age > a.ReassignThreshold {
		// Unassign and return to pending for re-matching
		a.Log.Info("workload stale, unassigning",
			"bead", wl.Spec.BeadID, "agent", wl.Status.AssignedTo, "age", age)

		oldAgent := wl.Status.AssignedTo
		wl.Status.Phase = "Pending"
		wl.Status.AssignedTo = ""
		wl.Status.Message = fmt.Sprintf("Reassigned after %s (was: %s)", age.Round(time.Minute), oldAgent)
		a.Client.Status().Update(ctx, wl) //nolint

	} else if age > a.StaleThreshold && wl.Status.Phase != "Stale" {
		// Send a reminder
		a.Log.Info("workload stale, sending reminder",
			"bead", wl.Spec.BeadID, "agent", wl.Status.AssignedTo, "age", age)

		wl.Status.Phase = "Stale"
		wl.Status.Message = fmt.Sprintf("No progress for %s", age.Round(time.Minute))
		a.Client.Status().Update(ctx, wl) //nolint

		msg := fmt.Sprintf("Reminder: %s (%s) has been in progress for %s. Still working on it?",
			wl.Spec.BeadID, wl.Spec.Title, age.Round(time.Minute))
		exec.CommandContext(ctx, "spire", "send", wl.Status.AssignedTo, msg,
			"--ref", wl.Spec.BeadID).Run() //nolint
	}
}

func prefixMatch(agentPrefixes, workloadPrefixes []string) bool {
	for _, ap := range agentPrefixes {
		for _, wp := range workloadPrefixes {
			if ap == wp {
				return true
			}
		}
	}
	return false
}
