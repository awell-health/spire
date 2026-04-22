package controllers

// TRANSITIONAL — LEGACY OPERATOR SCHEDULER (spi-njzmg).
//
// BeadWatcher is part of the pre-spi-sj18k operator scheduler loop. The
// canonical cluster-native path no longer routes through it: pkg/steward
// emits WorkloadIntent values and the operator's IntentWorkloadReconciler
// reconciles them into apprentice pods via pkg/agent.BuildApprenticePod.
//
// This file must only run when the OperatorEnableLegacyScheduler gate is
// true (see pkg/config/deployment_mode.go for the deployment-mode contract
// and operator/api/v1alpha1.SpireConfigSpec.EnableLegacyScheduler for the
// CR-level toggle). Default is false. Do not reintroduce a start path for
// this loop outside the gate — the operator is a reconciler, not a
// scheduler.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/steveyegge/beads"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spirev1 "github.com/awell-health/spire/operator/api/v1alpha1"
	"github.com/awell-health/spire/pkg/store"
)

// BeadWatcher reads beads from the shared dolt server and reconciles SpireWorkload CRs.
// DoltHub remote sync is handled by the dedicated spire-syncer pod.
type BeadWatcher struct {
	Client    client.Client
	Log       logr.Logger
	Namespace string
	Interval  time.Duration
	BeadsDir  string // path to .beads directory for store initialization
}

// Start implements controller-runtime's Runnable interface.
func (w *BeadWatcher) Start(ctx context.Context) error {
	w.Run(ctx)
	return nil
}

// Run is the main loop — call from the operator's main.go in a goroutine.
func (w *BeadWatcher) Run(ctx context.Context) {
	w.Log.Info("bead watcher starting", "interval", w.Interval, "backend", "operator-k8s")
	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()

	// Run immediately on start
	w.cycle(ctx)

	for {
		select {
		case <-ctx.Done():
			w.Log.Info("bead watcher stopping", "backend", "operator-k8s")
			return
		case <-ticker.C:
			w.cycle(ctx)
		}
	}
}

func (w *BeadWatcher) cycle(ctx context.Context) {
	w.Log.V(1).Info("bead watcher cycle start", "backend", "operator-k8s")

	// Ensure store is initialized
	if _, err := store.Ensure(w.BeadsDir); err != nil {
		w.Log.Error(err, "failed to initialize bead store", "beadsDir", w.BeadsDir, "backend", "operator-k8s")
		return
	}

	// DoltHub remote sync (pull/push) is handled by the dedicated spire-syncer pod.
	// The bead watcher reads directly from the shared dolt server.

	// 1. Get schedulable beads — GetSchedulableWork handles readiness
	// (no open blockers, excludes deferred/design/attempt/step beads) plus
	// scheduling policy (excludes msg/template beads, active-attempt beads).
	schedResult, err := store.GetSchedulableWork(beads.WorkFilter{})
	if err != nil {
		w.Log.Error(err, "store.GetSchedulableWork failed", "backend", "operator-k8s")
		return
	}

	// Surface quarantined beads (invariant violations) at Error level.
	for _, q := range schedResult.Quarantined {
		w.Log.Error(q.Error, "quarantined bead (multiple open attempts)", "bead_id", q.ID, "backend", "operator-k8s")
	}

	readyBeads := schedResult.Schedulable

	// 2. Get existing workloads
	var existing spirev1.SpireWorkloadList
	if err := w.Client.List(ctx, &existing, client.InNamespace(w.Namespace)); err != nil {
		w.Log.Error(err, "failed to list SpireWorkloads", "backend", "operator-k8s")
		return
	}

	existingMap := make(map[string]*spirev1.SpireWorkload)
	for i := range existing.Items {
		existingMap[existing.Items[i].Spec.BeadID] = &existing.Items[i]
	}

	// 3. Create SpireWorkloads for new ready beads
	created := 0
	for _, bead := range readyBeads {
		if _, exists := existingMap[bead.ID]; exists {
			continue // already tracked
		}

		// Extract prefix from bead ID (everything before the last hyphen-segment)
		prefix := extractPrefix(bead.ID)

		workload := &spirev1.SpireWorkload{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sanitizeName(bead.ID),
				Namespace: w.Namespace,
				Labels: map[string]string{
					"spire.awell.io/bead-id": bead.ID,
					"spire.awell.io/prefix":  prefix,
				},
			},
			Spec: spirev1.SpireWorkloadSpec{
				BeadID:   bead.ID,
				Title:    bead.Title,
				Priority: bead.Priority,
				Type:     bead.Type,
				Prefixes: []string{prefix},
			},
		}

		if err := w.Client.Create(ctx, workload); err != nil {
			w.Log.Error(err, "failed to create SpireWorkload", "bead_id", bead.ID, "backend", "operator-k8s")
			continue
		}

		// Set initial status
		workload.Status.Phase = "Pending"
		workload.Status.Message = "Waiting for agent assignment"
		if err := w.Client.Status().Update(ctx, workload); err != nil {
			w.Log.Error(err, "failed to set initial status", "bead_id", bead.ID, "backend", "operator-k8s")
		}

		created++
	}

	// 4. Check for completed beads — update workloads that are done
	closedStatus := beads.StatusClosed
	closedBeads, err := store.ListBeads(beads.IssueFilter{
		Status: &closedStatus,
	})
	if err == nil {
		for _, cb := range closedBeads {
			if wl, exists := existingMap[cb.ID]; exists {
				if wl.Status.Phase != "Done" {
					wl.Status.Phase = "Done"
					wl.Status.CompletedAt = time.Now().UTC().Format(time.RFC3339)
					wl.Status.Message = "Bead closed"
					w.Client.Status().Update(ctx, wl) //nolint
				}
			}
		}
	} else {
		w.Log.Error(err, "store.ListBeads (closed) failed", "backend", "operator-k8s")
	}

	if created > 0 {
		w.Log.Info("bead watcher cycle complete", "newWorkloads", created, "totalReady", len(readyBeads), "backend", "operator-k8s")
	} else {
		w.Log.V(1).Info("bead watcher cycle complete", "totalReady", len(readyBeads), "backend", "operator-k8s")
	}
}

func extractPrefix(beadID string) string {
	parts := strings.Split(beadID, "-")
	if len(parts) >= 2 {
		return parts[0] + "-" // include trailing hyphen to match agent prefixes like "spi-"
	}
	return beadID
}

func sanitizeName(beadID string) string {
	// k8s names must be lowercase, alphanumeric, hyphens, dots
	name := strings.ToLower(beadID)
	name = strings.ReplaceAll(name, ".", "-")
	return fmt.Sprintf("bead-%s", name)
}
