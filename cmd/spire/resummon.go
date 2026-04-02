package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

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

	// 1. Look up the bead and verify it has needs-human.
	bead, err := storeGetBead(beadID)
	if err != nil {
		return fmt.Errorf("get bead %s: %w", beadID, err)
	}

	if !containsLabel(bead, "needs-human") {
		return fmt.Errorf("%s does not have needs-human label — nothing to resummon", beadID)
	}

	// 2. Kill the old wizard process and remove its registry entry (clears timer).
	reg := loadWizardRegistry()
	wizardName := "wizard-" + beadID

	for i := range reg.Wizards {
		if reg.Wizards[i].BeadID == beadID {
			w := reg.Wizards[i]
			wizardName = w.Name

			// Kill process if still alive.
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
				fmt.Printf("  %skilled old wizard (pid %d)%s\n", dim, w.PID, reset)
			}

			// Remove from registry.
			reg.Wizards = append(reg.Wizards[:i], reg.Wizards[i+1:]...)
			saveWizardRegistry(reg)
			break
		}
	}

	// 3. Remove executor state file so summon starts fresh.
	statePath := executorStatePath(wizardName)
	if err := os.Remove(statePath); err == nil {
		fmt.Printf("  %scleared executor state%s\n", dim, reset)
	}

	// 4. Strip needs-human label.
	if err := storeRemoveLabel(beadID, "needs-human"); err != nil {
		return fmt.Errorf("remove needs-human label from %s: %w", beadID, err)
	}
	fmt.Printf("  %s✓ stripped needs-human from %s%s\n", green, beadID, reset)

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

	// 6. Re-summon: spire summon 1 --targets <bead-id>
	fmt.Printf("  re-summoning wizard for %s...\n", beadID)
	return cmdSummon([]string{"1", "--targets", beadID})
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
