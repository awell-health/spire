package main

import (
	"fmt"
	"os"

	"github.com/awell-health/spire/pkg/config"
	"github.com/spf13/cobra"
)

// readyGetBeadFunc is a test-replaceable wrapper around storeGetBead.
var readyGetBeadFunc = storeGetBead

// readyUpdateBeadFunc is a test-replaceable wrapper around storeUpdateBead.
var readyUpdateBeadFunc = func(id string, updates map[string]interface{}) error {
	return storeUpdateBead(id, updates)
}

// readyGetChildrenFunc is a test-replaceable wrapper around storeGetChildren
// used by readyGuardActiveWork to detect non-terminal attempt children.
var readyGetChildrenFunc = storeGetChildren

// readyFindLiveWizardFunc reports a wizard in the local registry that is
// bound to beadID and has a live process, or nil when none exists. It is a
// test seam so cluster-attached / cluster-native ready calls in tests don't
// read the developer's real ~/.config/spire/wizards.json. (spi-v1hcrs)
var readyFindLiveWizardFunc = func(beadID string) *localWizard {
	reg := loadWizardRegistry()
	for i := range reg.Wizards {
		w := &reg.Wizards[i]
		if w.BeadID != beadID {
			continue
		}
		if w.PID > 0 && processAlive(w.PID) {
			return w
		}
	}
	return nil
}

// readyActiveTowerFunc resolves the active tower for mode-aware messaging.
// Test seam so ready_test.go can drive cluster-native / cluster-attached /
// local-native branches without a real config file.
var readyActiveTowerFunc = activeTowerConfig

// readyStewardRunningFunc reports whether the local steward process is
// alive. Test seam so the local-native "no steward" guard is exercisable
// without spawning a real steward.
var readyStewardRunningFunc = func() bool {
	pid := readPID(stewardPIDPath())
	return pid > 0 && processAlive(pid)
}

var readyCmd = &cobra.Command{
	Use:   "ready <bead-id> [bead-id...]",
	Short: "Mark beads as ready for agent pickup",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runReady,
}

// readyModeInfo captures the dispatch context ready needs for messaging:
// the tower's deployment mode (local-native / cluster-native / attached-
// reserved), whether the tower is reached via the gateway (cluster-attached
// from the laptop's perspective), and whether the local steward is alive.
type readyModeInfo struct {
	mode              config.DeploymentMode
	isClusterAttached bool
	hasLocalSteward   bool
	towerName         string
}

// readyDescribeMode collects mode + steward state for messaging. Tower
// resolution failures degrade gracefully (mode == "") rather than blocking
// ready — the user may have ready'd into a not-yet-configured tower.
func readyDescribeMode() readyModeInfo {
	info := readyModeInfo{}
	if t, err := readyActiveTowerFunc(); err == nil && t != nil {
		info.mode = t.EffectiveDeploymentMode()
		info.isClusterAttached = t.IsGateway()
		info.towerName = t.Name
	}
	info.hasLocalSteward = readyStewardRunningFunc()
	return info
}

// readyGuardActiveWork rejects ready transitions when the bead already has
// work in flight: status in {in_progress, dispatched, hooked}, a non-
// terminal attempt child, or a live local wizard bound to the bead. The
// status case fires before the store/registry checks so a clear "status="
// message wins when present.
func readyGuardActiveWork(beadID, status string) error {
	switch status {
	case "in_progress", "dispatched", "hooked":
		return fmt.Errorf(
			"cannot ready %s: status=%q — bead already in progress (work in flight); "+
				"use spire roster / bd show %s to inspect, or spire resummon if stuck",
			beadID, status, beadID,
		)
	}

	// Active attempt: any non-terminal attempt child means a wizard or
	// pod is already executing the formula for this bead. The store
	// query is best-effort — if we can't read children we don't block,
	// because the status + registry checks still cover the hot paths.
	if children, err := readyGetChildrenFunc(beadID); err == nil {
		for _, c := range children {
			if c.Type != "attempt" {
				continue
			}
			switch c.Status {
			case "closed", "merged", "done":
				continue
			}
			return fmt.Errorf(
				"cannot ready %s: active attempt %s (status=%s) already exists; "+
					"use spire roster / bd show %s to inspect, or spire resummon if stuck",
				beadID, c.ID, c.Status, beadID,
			)
		}
	}

	// Live wizard binding: a local wizard process is already claimed to
	// this bead. Cluster-side guild capacity is not consulted here —
	// the cluster-mode path goes through the steward, which owns its
	// own deduplication. (spi-v1hcrs)
	if w := readyFindLiveWizardFunc(beadID); w != nil {
		return fmt.Errorf(
			"cannot ready %s: wizard %q (pid %d) is already bound to this bead; "+
				"use spire roster to inspect, or spire dismiss --targets %s",
			beadID, w.Name, w.PID, beadID,
		)
	}

	return nil
}

func runReady(cmd *cobra.Command, args []string) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	for _, id := range args {
		bead, err := readyGetBeadFunc(id)
		if err != nil {
			return fmt.Errorf("bead %s not found", id)
		}

		switch bead.Status {
		case "open":
			// valid transition — proceed to active-work guard below.
		case "ready":
			fmt.Fprintf(os.Stderr, "bead %s is already ready\n", id)
			continue
		case "in_progress", "dispatched", "hooked":
			// Hand off to readyGuardActiveWork for the unified
			// "work in flight" message rather than returning a
			// short status-only error here. Doing it this way keeps
			// the dispatched/hooked CLI guidance (resummon / roster)
			// in one place.
		case "closed":
			return fmt.Errorf("bead %s is closed", id)
		case "deferred":
			return fmt.Errorf("bead %s is deferred — undefer it first", id)
		default:
			return fmt.Errorf("bead %s has unexpected status %q", id, bead.Status)
		}

		if err := readyGuardActiveWork(id, bead.Status); err != nil {
			return err
		}

		info := readyDescribeMode()

		// Local-native + cluster-attached are exclusive: cluster-attached
		// means the laptop talks to a remote steward via the gateway, so
		// the local steward is irrelevant. The "needs a local steward"
		// guard fires only for genuine local-native towers.
		if info.mode == config.DeploymentModeLocalNative && !info.isClusterAttached && !info.hasLocalSteward {
			return fmt.Errorf(
				"cannot ready %s: local-native mode requires a running steward; "+
					"without one, ready will sit unconsumed.\n"+
					"Run: spire up",
				id,
			)
		}

		if err := readyUpdateBeadFunc(id, map[string]interface{}{
			"status": "ready",
		}); err != nil {
			return fmt.Errorf("ready %s: %w", id, err)
		}

		switch {
		case info.isClusterAttached:
			fmt.Printf("ready: %s (queued for cluster steward/operator dispatch via gateway tower %q)\n", id, info.towerName)
		case info.mode == config.DeploymentModeClusterNative:
			fmt.Printf("ready: %s (queued for cluster steward/operator dispatch on tower %q)\n", id, info.towerName)
		case info.mode == config.DeploymentModeLocalNative:
			fmt.Printf("ready: %s (mode=local-native, local steward will pick up)\n", id)
		default:
			fmt.Printf("ready: %s\n", id)
		}
	}

	return nil
}
