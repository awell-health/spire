package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/awell-health/spire/pkg/executor"
	"github.com/awell-health/spire/pkg/gatewayclient"
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
	"github.com/spf13/cobra"
	"github.com/steveyegge/beads"
)

var resummonCmd = &cobra.Command{
	Use:   "resummon <bead-id>",
	Short: "Clear timer + needs-human, re-summon wizard",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmdResummon(args)
	},
}

func cmdResummon(args []string) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: spire resummon <bead-id>")
	}

	beadID := args[0]

	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	// Gateway-mode dispatch: tunnel to POST /api/v1/beads/{id}/resummon so
	// the same `spire resummon` invocation works against an attached cluster
	// tower. Same shape as the v0.48 hardening pattern (spi-zz2ve9 /
	// spi-i7k1ag.4) — local-mode towers stay on the in-process path so
	// on-disk worktrees and graph state are exercised alongside the bead.
	if t, terr := activeTowerConfigFunc(); terr == nil && t != nil && t.IsGateway() {
		return resummonViaGatewayFunc(context.Background(), beadID)
	}

	// 1. Look up the bead and verify it's in a resummonable state.
	bead, err := storeGetBead(beadID)
	if err != nil {
		return fmt.Errorf("get bead %s: %w", beadID, err)
	}
	reg := loadWizardRegistry()
	_, hasLiveOwner := resummonLocalOwnerState(reg, beadID)
	hasGraphState := resummonHasGraphState(beadID)

	if err := validateResummonTarget(bead, resummonEvidence{
		HasGraphState: hasGraphState,
		HasLiveOwner:  hasLiveOwner,
	}); err != nil {
		return err
	}

	// 2. Kill the old wizard process and remove its registry entry (clears timer).
	//
	// Why EndWork is NOT called here:
	// EndWork(interrupted, ReopenTask=true) would close the active attempt bead and
	// remove the registry entry. resummon handles registry removal directly (below),
	// and the attempt bead is closed by beadlifecycle.OrphanSweep when the subsequent
	// cmdSummon → BeginWork runs. Scan B in OrphanSweep finds in_progress attempt
	// beads with no live registry entry and closes them automatically. This produces
	// equivalent state to EndWork without introducing an EndWork dependency here.
	removeResummonRegistryEntries(reg, beadID)

	// 3. Preserve graph state — the new wizard should resume where the old one left off.
	// Previously this deleted graph state, forcing a full restart. That wastes all
	// completed work (plan, dispatch, implement) when only a later step failed.

	// 4. Strip needs-human label.
	if containsLabel(bead, "needs-human") {
		if err := storeRemoveLabel(beadID, "needs-human"); err != nil {
			return fmt.Errorf("remove needs-human label from %s: %w", beadID, err)
		}
		fmt.Printf("  %s✓ stripped needs-human from %s%s\n", green, beadID, reset)
	}

	// 4b. Strip any interrupted:* labels so stale failure state doesn't linger.
	for _, l := range bead.Labels {
		if strings.HasPrefix(l, "interrupted:") {
			if err := storeRemoveLabel(beadID, l); err != nil {
				fmt.Printf("  %s(note: could not remove %s from %s: %s)%s\n", dim, l, beadID, err, reset)
			} else {
				fmt.Printf("  %s✓ cleared %s from %s%s\n", green, l, beadID, reset)
			}
		}
	}

	// 4c. Strip any dispatch:* override labels so resummon uses formula defaults
	// unless the user explicitly passes --dispatch on the next summon.
	for _, l := range bead.Labels {
		if strings.HasPrefix(l, "dispatch:") {
			if err := storeRemoveLabel(beadID, l); err != nil {
				fmt.Printf("  %s(note: could not remove %s from %s: %s)%s\n", dim, l, beadID, err, reset)
			} else {
				fmt.Printf("  %s✓ cleared %s from %s%s\n", green, l, beadID, reset)
			}
		}
	}

	// 5. Close any open alert beads that reference this bead (merge-failure, etc.).
	closeRelatedAlerts(beadID)

	// 5b. Close any open recovery + alert beads linked to this bead. The
	// per-caller alert close in step 5 (closeRelatedAlerts) is kept as a
	// belt-and-suspenders helper; CloseRelatedDependents is the canonical
	// path.
	if err := recovery.CloseRelatedDependents(storeBridgeOps{}, beadID, []string{recovery.KindRecovery, recovery.KindAlert}, []string{"caused-by", "recovery-for"}, "resummon"); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not close dependents: %v\n", err)
	}

	// 6. Warn that no recovery learning is recorded.
	fmt.Fprintf(os.Stderr, "\n⚠ No recovery learning recorded. To capture what you did for future recoveries, use:\n  spire resolve %s \"what you did\"\n\n", beadID)

	// 7. Re-summon: spire summon 1 --targets <bead-id>
	fmt.Printf("  re-summoning wizard for %s...\n", beadID)
	return cmdSummon([]string{"1", "--targets", beadID})
}

type resummonEvidence struct {
	HasGraphState bool
	HasLiveOwner  bool
}

func validateResummonTarget(bead Bead, evidence resummonEvidence) error {
	switch bead.Status {
	case "closed", "done":
		return fmt.Errorf("%s is closed — nothing to resummon", bead.ID)
	}

	isHooked := bead.Status == "hooked"
	hasLegacyLabel := containsLabel(bead, "needs-human") || hasLabelPrefix(bead, "interrupted:")
	if isHooked || hasLegacyLabel {
		return nil
	}

	if evidence.HasLiveOwner {
		return fmt.Errorf("%s has a live wizard owner — dismiss or wait before resummoning", bead.ID)
	}

	if evidence.HasGraphState {
		return nil
	}

	switch bead.Status {
	case "in_progress", "dispatched":
		return nil
	}

	return fmt.Errorf("%s is not hooked/interrupted and has no resumable executor state — use summon/ready instead", bead.ID)
}

func resummonLocalOwnerState(reg wizardRegistry, beadID string) (hasEntry bool, hasLiveOwner bool) {
	for _, w := range reg.Wizards {
		if w.BeadID != beadID {
			continue
		}
		hasEntry = true
		if w.PID > 0 && processAlive(w.PID) {
			hasLiveOwner = true
		}
	}
	return hasEntry, hasLiveOwner
}

func resummonHasGraphState(beadID string) bool {
	gs, err := executor.LoadGraphState("wizard-"+beadID, configDir)
	return err == nil && gs != nil
}

func removeResummonRegistryEntries(reg wizardRegistry, beadID string) {
	removed := false
	remaining := make([]localWizard, 0, len(reg.Wizards))
	for _, w := range reg.Wizards {
		if w.BeadID != beadID {
			remaining = append(remaining, w)
			continue
		}
		removed = true
		if w.PID > 0 && processAlive(w.PID) {
			if proc, err := os.FindProcess(w.PID); err == nil {
				proc.Signal(syscall.SIGTERM)
				deadline := time.Now().Add(3 * time.Second)
				for time.Now().Before(deadline) {
					time.Sleep(200 * time.Millisecond)
					if !processAlive(w.PID) {
						break
					}
				}
				if processAlive(w.PID) {
					proc.Signal(syscall.SIGKILL)
				}
			}
			fmt.Printf("  %skilled old wizard %s (pid %d)%s\n", dim, w.Name, w.PID, reset)
		}
	}
	if removed {
		reg.Wizards = remaining
		saveWizardRegistry(reg)
	}
}

// resummonViaGatewayFunc is the gateway-mode dispatch seam. cmdResummon
// calls it when the active tower is gateway-mode so the resummon runs
// against the gateway endpoint instead of the local Dolt store. Tests
// swap this to verify routing without standing up a real gateway.
var resummonViaGatewayFunc = resummonViaGateway

// resummonViaGateway tunnels a resummon call through the active tower's
// gatewayclient. Renders the post-resummon bead's status to stdout so a
// gateway-mode invocation looks identical to a local one at the terminal.
func resummonViaGateway(ctx context.Context, id string) error {
	t, err := activeTowerConfigFunc()
	if err != nil {
		return fmt.Errorf("resummon %s: resolve tower: %w", id, err)
	}
	if t == nil {
		return fmt.Errorf("resummon %s: no active tower", id)
	}
	c, err := store.NewGatewayClientForTower(t)
	if err != nil {
		return fmt.Errorf("resummon %s: %w", id, err)
	}
	bead, err := c.ResummonBead(ctx, id)
	if err != nil {
		return err
	}
	fmt.Printf("%s resummoned (gateway: status=%s)\n", id, bead.Status)
	return nil
}

// dismissBeadViaGatewayFunc is the gateway-mode dispatch seam used by the
// per-bead dismiss verb in dismiss_bead.go. Tests swap this to verify
// routing without standing up a real gateway.
var dismissBeadViaGatewayFunc = dismissBeadViaGateway

// dismissBeadViaGateway tunnels a dismiss call through the active tower's
// gatewayclient.
func dismissBeadViaGateway(ctx context.Context, id string) error {
	t, err := activeTowerConfigFunc()
	if err != nil {
		return fmt.Errorf("dismiss %s: resolve tower: %w", id, err)
	}
	if t == nil {
		return fmt.Errorf("dismiss %s: no active tower", id)
	}
	c, err := store.NewGatewayClientForTower(t)
	if err != nil {
		return fmt.Errorf("dismiss %s: %w", id, err)
	}
	bead, err := c.DismissBead(ctx, id)
	if err != nil {
		return err
	}
	fmt.Printf("%s dismissed (gateway: status=%s)\n", id, bead.Status)
	return nil
}

// updateStatusViaGatewayFunc is the gateway-mode dispatch seam used by
// `spire update --status` against an attached cluster tower.
var updateStatusViaGatewayFunc = updateStatusViaGateway

// updateStatusViaGateway tunnels an update --status call through the
// active tower's gatewayclient.
func updateStatusViaGateway(ctx context.Context, id, to string) error {
	t, err := activeTowerConfigFunc()
	if err != nil {
		return fmt.Errorf("update %s: resolve tower: %w", id, err)
	}
	if t == nil {
		return fmt.Errorf("update %s: no active tower", id)
	}
	c, err := store.NewGatewayClientForTower(t)
	if err != nil {
		return fmt.Errorf("update %s: %w", id, err)
	}
	bead, err := c.UpdateBeadStatus(ctx, id, gatewayclient.UpdateBeadStatusOpts{To: to})
	if err != nil {
		return err
	}
	fmt.Printf("%s updated (gateway: status=%s)\n", id, bead.Status)
	return nil
}

// hasLabelPrefix returns true if any label on the bead starts with the given prefix.
func hasLabelPrefix(b Bead, prefix string) bool {
	for _, l := range b.Labels {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

// closeRelatedAlerts closes all open alert beads that reference the given bead ID
// via a related or caused-by dep. This prevents stale alerts (merge-failure, etc.)
// from lingering on the board after a successful re-summon.
func closeRelatedAlerts(beadID string) {
	dependents, err := storeGetDependentsWithMetaFunc(beadID)
	if err != nil {
		return
	}

	for _, dep := range dependents {
		if dep.DependencyType != beads.DepRelated && dep.DependencyType != "caused-by" {
			continue
		}
		if dep.Status == beads.StatusClosed {
			continue
		}
		// Only close beads that have an alert label.
		isAlert := false
		for _, l := range dep.Labels {
			if l == "alert" || strings.HasPrefix(l, "alert:") {
				isAlert = true
				break
			}
		}
		if !isAlert {
			continue
		}
		if err := storeCloseBeadFunc(dep.ID); err != nil {
			fmt.Printf("  %s(note: could not close alert %s: %s)%s\n", dim, dep.ID, err, reset)
			continue
		}
		fmt.Printf("  %s✓ closed alert %s%s\n", green, dep.ID, reset)
	}
}
