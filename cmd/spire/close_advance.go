package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads"
)

var closeCmd = &cobra.Command{
	Use:   "close <bead-id>",
	Short: "Force-close a bead (remove phase labels, close molecule steps)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmdClose(args)
	},
}

// cmdClose implements `spire close <bead-id>`.
// Force-closes a bead: removes phase labels, closes open molecule children, closes the bead.
func cmdClose(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: spire close <bead-id>")
	}
	id := args[0]

	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	bead, err := storeGetBeadFunc(id)
	if err != nil {
		return fmt.Errorf("bead %s not found: %w", id, err)
	}

	alreadyClosed := bead.Status == string(beads.StatusClosed)
	if err := closeBeadLifecycle(id, bead); err != nil {
		return err
	}

	if alreadyClosed {
		fmt.Printf("%s is already closed\n", id)
	} else {
		fmt.Printf("closed %s\n", id)
	}
	return nil
}

func storeCloseBeadLifecycle(id string) error {
	bead, err := storeGetBeadFunc(id)
	if err != nil {
		return err
	}
	return closeBeadLifecycle(id, bead)
}

// closeBeadLifecycle performs the Spire-level close cycle for one bead. It is
// intentionally used for direct workflow-step children too; callers should not
// bypass this with a raw bd close/store close when cleaning Spire-owned work.
func closeBeadLifecycle(id string, bead Bead) error {
	// Traverse workflow children before closing the parent. This is best-effort
	// to preserve the historical force-close behavior, and intentionally visits
	// already-closed containers so stale descendants can still be repaired.
	closeWorkflowChildren(id)

	// Remove phase: and interrupted: labels from the bead.
	for _, l := range bead.Labels {
		if strings.HasPrefix(l, "phase:") || strings.HasPrefix(l, "interrupted:") {
			if err := storeRemoveLabelFunc(id, l); err != nil {
				fmt.Fprintf(os.Stderr, "warning: remove label %s from %s: %s\n", l, id, err)
			}
		}
	}

	if bead.Status == "closed" {
		closeCausedByAlerts(id)
		return nil
	}

	// Close the bead.
	if err := storeCloseBeadFunc(id); err != nil {
		return fmt.Errorf("close %s: %w", id, err)
	}

	// Cascade-close: close any open alert beads linked via caused-by dep.
	closeCausedByAlerts(id)
	return nil
}

// closeCausedByAlerts closes open alert beads that have a caused-by dep on the
// given bead. This ensures alert beads are automatically cleaned up when the
// source bead they were triggered by is closed. Only cascades one level.
func closeCausedByAlerts(beadID string) {
	dependents, err := storeGetDependentsWithMetaFunc(beadID)
	if err != nil {
		return
	}

	for _, dep := range dependents {
		if dep.DependencyType != "caused-by" {
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
			fmt.Fprintf(os.Stderr, "warning: cascade-close alert %s: %s\n", dep.ID, err)
			continue
		}
		fmt.Printf("  auto-closed alert %s\n", dep.ID)
	}
}

// closeWorkflowChildren closes both direct workflow-step children created by
// the executor and legacy workflow molecule children.
func closeWorkflowChildren(beadID string) {
	closeDirectWorkflowStepChildren(beadID)
	closeMoleculeChildren(beadID)
}

// closeDirectWorkflowStepChildren closes direct step children using the same
// lifecycle close path as top-level `spire close`.
func closeDirectWorkflowStepChildren(beadID string) {
	children, err := storeGetChildrenFunc(beadID)
	if err != nil {
		return
	}

	for _, child := range children {
		if !isStepBead(child) {
			continue
		}
		if err := closeBeadLifecycle(child.ID, child); err != nil {
			fmt.Fprintf(os.Stderr, "warning: close workflow step %s: %s\n", child.ID, err)
		}
	}
}

// closeMoleculeChildren finds the legacy workflow molecule for a bead (if any)
// and closes it through the same lifecycle path.
func closeMoleculeChildren(beadID string) {
	mols, err := storeListBeadsFunc(beads.IssueFilter{
		IDPrefix: "spi-",
		Labels:   []string{"workflow:" + beadID},
	})
	if err != nil || len(mols) == 0 {
		return
	}

	for _, mol := range mols {
		if err := closeBeadLifecycle(mol.ID, mol); err != nil {
			fmt.Fprintf(os.Stderr, "warning: close molecule %s: %s\n", mol.ID, err)
		}
	}
}
