package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spirev1 "github.com/awell-health/spire/operator/api/v1alpha1"
)

// BeadWatcher syncs beads from DoltHub into SpireWorkload CRs.
type BeadWatcher struct {
	Client    client.Client
	Log       logr.Logger
	Namespace string
	Interval  time.Duration
}

type beadJSON struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Priority int    `json:"priority"`
	Type     string `json:"type"`
}

// Run is the main loop — call from the operator's main.go in a goroutine.
func (w *BeadWatcher) Run(ctx context.Context) {
	w.Log.Info("bead watcher starting", "interval", w.Interval)
	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()

	// Run immediately on start
	w.cycle(ctx)

	for {
		select {
		case <-ctx.Done():
			w.Log.Info("bead watcher stopping")
			return
		case <-ticker.C:
			w.cycle(ctx)
		}
	}
}

func (w *BeadWatcher) cycle(ctx context.Context) {
	w.Log.V(1).Info("bead watcher cycle start")

	// 1. Sync from DoltHub
	if out, err := exec.CommandContext(ctx, "bd", "dolt", "pull").CombinedOutput(); err != nil {
		w.Log.Error(err, "bd dolt pull failed", "output", string(out))
		// Continue anyway — we might have stale-but-useful local state
	}

	// 2. Get ready beads
	out, err := exec.CommandContext(ctx, "bd", "ready", "--json").Output()
	if err != nil {
		w.Log.Error(err, "bd ready --json failed")
		return
	}

	var beads []beadJSON
	if err := json.Unmarshal(out, &beads); err != nil {
		w.Log.Error(err, "failed to parse bd ready output")
		return
	}

	// 3. Get existing workloads
	var existing spirev1.SpireWorkloadList
	if err := w.Client.List(ctx, &existing, client.InNamespace(w.Namespace)); err != nil {
		w.Log.Error(err, "failed to list SpireWorkloads")
		return
	}

	existingMap := make(map[string]*spirev1.SpireWorkload)
	for i := range existing.Items {
		existingMap[existing.Items[i].Spec.BeadID] = &existing.Items[i]
	}

	// 4. Create SpireWorkloads for new ready beads
	created := 0
	for _, bead := range beads {
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
			w.Log.Error(err, "failed to create SpireWorkload", "beadId", bead.ID)
			continue
		}

		// Set initial status
		workload.Status.Phase = "Pending"
		workload.Status.Message = "Waiting for agent assignment"
		if err := w.Client.Status().Update(ctx, workload); err != nil {
			w.Log.Error(err, "failed to set initial status", "beadId", bead.ID)
		}

		created++
	}

	// 5. Check for completed beads — update workloads that are done
	allOut, err := exec.CommandContext(ctx, "bd", "list", "--status=closed", "--json").Output()
	if err == nil {
		var closedBeads []beadJSON
		if json.Unmarshal(allOut, &closedBeads) == nil {
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
		}
	}

	// 6. Push state
	exec.CommandContext(ctx, "bd", "dolt", "push").Run() //nolint

	if created > 0 {
		w.Log.Info("bead watcher cycle complete", "newWorkloads", created, "totalReady", len(beads))
	} else {
		w.Log.V(1).Info("bead watcher cycle complete", "totalReady", len(beads))
	}
}

func extractPrefix(beadID string) string {
	parts := strings.Split(beadID, "-")
	if len(parts) >= 2 {
		return parts[0]
	}
	return beadID
}

func sanitizeName(beadID string) string {
	// k8s names must be lowercase, alphanumeric, hyphens, dots
	name := strings.ToLower(beadID)
	name = strings.ReplaceAll(name, ".", "-")
	return fmt.Sprintf("bead-%s", name)
}
