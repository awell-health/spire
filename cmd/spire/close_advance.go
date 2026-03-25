package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/steveyegge/beads"
)

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

	// Remove any phase: label from the bead.
	for _, l := range bead.Labels {
		if strings.HasPrefix(l, "phase:") {
			if err := storeRemoveLabel(id, l); err != nil {
				fmt.Fprintf(os.Stderr, "warning: remove label %s from %s: %s\n", l, id, err)
			}
		}
	}

	// Close the bead.
	if err := storeCloseBead(id); err != nil {
		return fmt.Errorf("close %s: %w", id, err)
	}

	fmt.Printf("closed %s\n", id)
	return nil
}

// cmdAdvance implements `spire advance <bead-id>`.
// Advances the bead to the next phase in its formula's enabled phases.
// If already at the last enabled phase, closes the bead.
// If the bead has no phase label, advances to the first enabled phase.
func cmdAdvance(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: spire advance <bead-id>")
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
		return fmt.Errorf("bead %s is already closed", id)
	}

	// Resolve the formula for this bead.
	formula, err := ResolveFormula(bead)
	if err != nil {
		return fmt.Errorf("resolve formula for %s: %w", id, err)
	}

	enabled := formula.EnabledPhases()
	if len(enabled) == 0 {
		return fmt.Errorf("formula has no enabled phases")
	}

	currentPhase := getPhase(bead)

	// Find the next phase.
	nextPhase := ""
	if currentPhase == "" {
		// No current phase — advance to first enabled phase.
		nextPhase = enabled[0]
	} else {
		for i, p := range enabled {
			if p == currentPhase {
				if i+1 < len(enabled) {
					nextPhase = enabled[i+1]
				}
				// else: already at last phase — nextPhase stays ""
				break
			}
		}
		if nextPhase == "" && currentPhase != "" {
			// Either already at last phase or phase not in formula — close.
			fmt.Printf("%s: at last phase (%s), closing\n", id, currentPhase)
			return cmdClose([]string{id})
		}
	}

	// Transition to next phase.
	if err := setPhase(id, nextPhase); err != nil {
		return fmt.Errorf("advance %s to %s: %w", id, nextPhase, err)
	}

	fmt.Printf("%s: advanced to phase %s\n", id, nextPhase)
	return nil
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
