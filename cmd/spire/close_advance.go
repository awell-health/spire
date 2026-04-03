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

	bead, err := storeGetBead(id)
	if err != nil {
		return fmt.Errorf("bead %s not found: %w", id, err)
	}

	if bead.Status == "closed" {
		fmt.Printf("%s is already closed\n", id)
		return nil
	}

	// Close open molecule children (workflow steps).
	closeMoleculeChildren(id)

	// Remove phase: and interrupted: labels from the bead.
	for _, l := range bead.Labels {
		if strings.HasPrefix(l, "phase:") || strings.HasPrefix(l, "interrupted:") {
			if err := storeRemoveLabel(id, l); err != nil {
				fmt.Fprintf(os.Stderr, "warning: remove label %s from %s: %s\n", l, id, err)
			}
		}
	}

	// Close the bead.
	if err := storeCloseBead(id); err != nil {
		return fmt.Errorf("close %s: %w", id, err)
	}

	// Cascade-close: close any open alert beads linked via caused-by dep.
	closeCausedByAlerts(id)

	fmt.Printf("closed %s\n", id)
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


// closeMoleculeChildren finds the workflow molecule for a bead (if any) and
// closes all open step children, then closes the molecule itself.
func closeMoleculeChildren(beadID string) {
	mols, err := storeListBeads(beads.IssueFilter{
		IDPrefix: "spi-",
		Labels:   []string{"workflow:" + beadID},
	})
	if err != nil || len(mols) == 0 {
		return
	}

	for _, mol := range mols {
		// Close each open child step.
		children, err := storeGetChildren(mol.ID)
		if err == nil {
			for _, child := range children {
				if child.Status != "closed" {
					if err := storeCloseBead(child.ID); err != nil {
						fmt.Fprintf(os.Stderr, "warning: close molecule step %s: %s\n", child.ID, err)
					}
				}
			}
		}
		// Close the molecule itself.
		if err := storeCloseBead(mol.ID); err != nil {
			fmt.Fprintf(os.Stderr, "warning: close molecule %s: %s\n", mol.ID, err)
		}
	}
}
