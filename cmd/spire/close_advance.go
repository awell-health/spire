package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	closepkg "github.com/awell-health/spire/pkg/close"
	"github.com/awell-health/spire/pkg/store"
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

// gatewayCloseBeadFunc is the gateway-mode dispatch seam. cmdClose calls
// it when the active tower is gateway-mode so the close lifecycle runs
// server-side (where direct Dolt access makes the workflow-step traversal
// possible) instead of failing closed on the local store. Tests swap this
// to verify routing without standing up a real gateway.
var gatewayCloseBeadFunc = closeBeadViaGateway

// cmdClose implements `spire close <bead-id>`.
// Force-closes a bead: removes phase labels, closes open molecule children, closes the bead.
//
// In gateway mode the call is tunneled to POST /api/v1/beads/{id}/close so
// the lifecycle runs server-side against direct Dolt — local close cannot
// discover workflow-step children because GetChildren is unsupported on
// the gateway client.
func cmdClose(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: spire close <bead-id>")
	}
	id := args[0]

	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	// gateway-mode: tunnel close lifecycle to the server. Local pkg/store
	// cannot discover workflow-step children (GetChildren is gateway-
	// unsupported); running the lifecycle locally would silently leave
	// step beads open after closing the parent.
	if t, err := activeTowerConfigFunc(); err == nil && t != nil && t.IsGateway() {
		if err := gatewayCloseBeadFunc(context.Background(), id); err != nil {
			return err
		}
		fmt.Printf("closed %s\n", id)
		return nil
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

// closeBeadViaGateway tunnels a close call through the active tower's
// gatewayclient. Real implementation; tests swap the gatewayCloseBeadFunc
// seam directly. Errors with messages containing "not found" propagate to
// the CLI verbatim so the user sees the same shape as direct mode.
func closeBeadViaGateway(ctx context.Context, id string) error {
	t, err := activeTowerConfigFunc()
	if err != nil {
		return fmt.Errorf("close %s: resolve tower: %w", id, err)
	}
	if t == nil {
		return fmt.Errorf("close %s: no active tower", id)
	}
	c, err := store.NewGatewayClientForTower(t)
	if err != nil {
		return fmt.Errorf("close %s: %w", id, err)
	}
	return c.CloseBead(ctx, id)
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
//
// Fails closed when workflow-child discovery returns a gateway-unsupported
// error: never close the parent on a swallowed read failure. In gateway mode
// the CLI short-circuits to the server-side endpoint before reaching this
// code, so this fail-closed guard is defense-in-depth — it ensures any future
// caller that routes here directly also surfaces the violation rather than
// silently leaving children open.
func closeBeadLifecycle(id string, bead Bead) error {
	// Traverse workflow children before closing the parent. Best-effort by
	// design (preserves historical force-close behavior; intentionally visits
	// already-closed containers so stale descendants can still be repaired)
	// EXCEPT when child discovery returns ErrGatewayUnsupported — that's a
	// strong "I cannot see children" signal and the parent must not close.
	if err := closeWorkflowChildren(id); err != nil {
		if errors.Is(err, store.ErrGatewayUnsupported) {
			return fmt.Errorf("close %s: workflow child discovery unsupported on this tower; route close through the gateway endpoint: %w", id, err)
		}
		// Other errors keep the historical "log warning, continue" behavior:
		// closeDirectWorkflowStepChildren and closeMoleculeChildren swallow
		// individual close failures and only return propagatable errors for
		// the gateway-unsupported sentinel above.
	}

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
// the executor and legacy workflow molecule children. Returns the first
// gateway-unsupported error so the parent close can fail closed; other
// errors are logged and swallowed (preserves force-close ergonomics).
func closeWorkflowChildren(beadID string) error {
	if err := closeDirectWorkflowStepChildren(beadID); err != nil {
		return err
	}
	return closeMoleculeChildren(beadID)
}

// closeDirectWorkflowStepChildren closes direct step children using the same
// lifecycle close path as top-level `spire close`. A gateway-unsupported
// error from child discovery propagates so the parent close can fail closed
// — never silently skip child cleanup on a tower where GetChildren is not
// implemented.
func closeDirectWorkflowStepChildren(beadID string) error {
	children, err := storeGetChildrenFunc(beadID)
	if err != nil {
		if errors.Is(err, store.ErrGatewayUnsupported) {
			return err
		}
		return nil
	}

	for _, child := range children {
		if !isStepBead(child) {
			continue
		}
		if err := closeBeadLifecycle(child.ID, child); err != nil {
			fmt.Fprintf(os.Stderr, "warning: close workflow step %s: %s\n", child.ID, err)
		}
	}
	return nil
}

// closeMoleculeChildren finds the legacy workflow molecule for a bead (if any)
// and closes it through the same lifecycle path.
func closeMoleculeChildren(beadID string) error {
	mols, err := storeListBeadsFunc(beads.IssueFilter{
		IDPrefix: "spi-",
		Labels:   []string{"workflow:" + beadID},
	})
	if err != nil {
		if errors.Is(err, store.ErrGatewayUnsupported) {
			return err
		}
		return nil
	}
	if len(mols) == 0 {
		return nil
	}

	for _, mol := range mols {
		if err := closeBeadLifecycle(mol.ID, mol); err != nil {
			fmt.Fprintf(os.Stderr, "warning: close molecule %s: %s\n", mol.ID, err)
		}
	}
	return nil
}

func init() {
	// Wire pkg/close.RunFunc so the gateway (and any other in-process
	// caller) can drive the close lifecycle through the same code that
	// `spire close` runs from the CLI. The CLI uses closeBeadLifecycle
	// directly; the gateway dispatches via close.RunLifecycle → RunFunc.
	closepkg.RunFunc = storeCloseBeadLifecycle
}
