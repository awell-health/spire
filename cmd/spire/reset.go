package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/awell-health/spire/pkg/formula"
	spgit "github.com/awell-health/spire/pkg/git"
	"github.com/spf13/cobra"
)

var resetCmd = &cobra.Command{
	Use:   "reset <bead-id> [flags]",
	Short: "Reset a bead's wizard state",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var fullArgs []string
		fullArgs = append(fullArgs, args...)
		if hard, _ := cmd.Flags().GetBool("hard"); hard {
			fullArgs = append(fullArgs, "--hard")
		}
		if v, _ := cmd.Flags().GetString("to"); v != "" {
			fullArgs = append(fullArgs, "--to", v)
		}
		return cmdReset(fullArgs)
	},
}

func init() {
	resetCmd.Flags().Bool("hard", false, "Hard reset (delete worktree and state)")
	resetCmd.Flags().String("to", "", "Reset to a specific phase")
}

func cmdReset(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: spire reset <bead-id> [--to <phase>] [--hard]")
	}

	var beadID string
	var toPhase string
	var hard bool
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--hard":
			hard = true
		case "--to":
			if i+1 >= len(args) {
				return fmt.Errorf("--to requires a phase name")
			}
			i++
			toPhase = args[i]
			if !formula.IsValidPhase(toPhase) {
				return fmt.Errorf("invalid phase %q (valid: %s)", toPhase, strings.Join(formula.ValidPhases, ", "))
			}
		default:
			if strings.HasPrefix(args[i], "-") {
				return fmt.Errorf("unknown flag %q\nusage: spire reset <bead-id> [--to <phase>] [--hard]", args[i])
			}
			beadID = args[i]
		}
	}

	if beadID == "" {
		return fmt.Errorf("usage: spire reset <bead-id> [--to <phase>] [--hard]")
	}

	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	// --- 1. Kill wizard process if alive ---

	reg := loadWizardRegistry()
	var wizard *localWizard
	for i := range reg.Wizards {
		if reg.Wizards[i].BeadID == beadID {
			wizard = &reg.Wizards[i]
			break
		}
	}

	var wizardName string
	var worktreePath string

	if wizard != nil {
		wizardName = wizard.Name
		worktreePath = wizard.Worktree

		if wizard.PID > 0 && processAlive(wizard.PID) {
			if proc, err := os.FindProcess(wizard.PID); err == nil {
				proc.Signal(syscall.SIGTERM)
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					time.Sleep(200 * time.Millisecond)
					if !processAlive(wizard.PID) {
						break
					}
				}
				if processAlive(wizard.PID) {
					proc.Signal(syscall.SIGKILL)
				}
			}
			fmt.Printf("  %s↓ %s killed (pid %d)%s\n", dim, wizardName, wizard.PID, reset)
		}

		// Remove registry entry.
		var remaining []localWizard
		for _, w := range reg.Wizards {
			if w.BeadID != beadID {
				remaining = append(remaining, w)
			}
		}
		reg.Wizards = remaining
		saveWizardRegistry(reg)
	} else {
		wizardName = "wizard-" + beadID
	}

	// --- 2. Delete executor state.json ---

	statePath := executorStatePath(wizardName)
	if err := os.Remove(statePath); err == nil {
		fmt.Printf("  %s✗ state file removed%s\n", dim, reset)
	}

	// --- Resolve formula to determine enabled phases and default --to ---

	bead, err := storeGetBead(beadID)
	if err != nil {
		return fmt.Errorf("get bead %s: %w", beadID, err)
	}

	f, err := ResolveFormula(bead)
	if err != nil {
		return fmt.Errorf("resolve formula for %s: %w", beadID, err)
	}

	enabled := f.EnabledPhases()
	if len(enabled) == 0 {
		return fmt.Errorf("formula has no enabled phases")
	}

	// Default --to is the first enabled phase.
	if toPhase == "" {
		toPhase = enabled[0]
	}

	// Validate that the target phase is in the formula's enabled phases.
	phaseIdx := -1
	for i, p := range enabled {
		if p == toPhase {
			phaseIdx = i
			break
		}
	}
	if phaseIdx < 0 {
		return fmt.Errorf("phase %q is not enabled in formula %s (enabled: %s)",
			toPhase, f.Name, strings.Join(enabled, ", "))
	}

	// --- 3. Close all subtask children (plan output) ---
	// Subtask children are non-step, non-attempt, non-review children.

	children, err := storeGetChildren(beadID)
	if err != nil {
		return fmt.Errorf("get children for %s: %w", beadID, err)
	}

	closedCount := 0
	for _, child := range children {
		if isStepBead(child) || isAttemptBead(child) || isReviewRoundBead(child) {
			continue
		}
		if child.Status == "closed" {
			continue
		}
		if err := storeCloseBead(child.ID); err != nil {
			fmt.Printf("  %s(note: could not close subtask %s: %s)%s\n", dim, child.ID, err, reset)
		} else {
			closedCount++
		}
	}
	if closedCount > 0 {
		fmt.Printf("  %s✗ closed %d subtask children%s\n", dim, closedCount, reset)
	}

	// --- 4. Rewind step beads ---
	// Target step → in_progress, after target → open, before target → leave closed.

	// Build ordered map: phase name → index in enabled phases.
	phaseOrder := make(map[string]int, len(enabled))
	for i, p := range enabled {
		phaseOrder[p] = i
	}

	steps, err := storeGetStepBeads(beadID)
	if err != nil {
		return fmt.Errorf("get step beads for %s: %w", beadID, err)
	}

	for _, step := range steps {
		phaseName := stepBeadPhaseName(step)
		if phaseName == "" {
			continue
		}
		idx, ok := phaseOrder[phaseName]
		if !ok {
			continue
		}

		if idx == phaseIdx {
			// Target phase → in_progress
			if step.Status != "in_progress" {
				if err := storeActivateStepBead(step.ID); err != nil {
					fmt.Printf("  %s(note: could not activate step %s: %s)%s\n", dim, phaseName, err, reset)
				}
			}
			fmt.Printf("  %s→ step:%s set to in_progress%s\n", yellow, phaseName, reset)
		} else if idx > phaseIdx {
			// After target → open
			if step.Status != "open" {
				if err := storeUpdateBead(step.ID, map[string]interface{}{"status": "open"}); err != nil {
					fmt.Printf("  %s(note: could not reopen step %s: %s)%s\n", dim, phaseName, err, reset)
				}
			}
			fmt.Printf("  %s  step:%s set to open%s\n", dim, phaseName, reset)
		}
		// Before target → leave as-is (closed).
	}

	// --- 5. Set bead status to in_progress ---

	if err := storeUpdateBead(beadID, map[string]interface{}{"status": "in_progress"}); err != nil {
		fmt.Printf("  %s(note: could not set %s to in_progress: %s)%s\n", dim, beadID, err, reset)
	} else {
		fmt.Printf("  %s↺ %s set to in_progress%s\n", dim, beadID, reset)
	}

	// --- 6. --hard: remove worktrees and delete branches ---

	if hard {
		// Remove worktree directory.
		if worktreePath == "" {
			worktreePath = filepath.Join(os.TempDir(), "spire-wizard", wizardName, beadID)
		}
		if err := os.RemoveAll(worktreePath); err == nil {
			fmt.Printf("  %s✗ worktree removed: %s%s\n", dim, worktreePath, reset)
		} else if !os.IsNotExist(err) {
			fmt.Printf("  %s(note: could not remove worktree %s: %s)%s\n", dim, worktreePath, err, reset)
		}

		// Also remove .worktrees/<bead-id> if it exists (in-repo worktree).
		cwd, _ := os.Getwd()
		inRepoWt := filepath.Join(cwd, ".worktrees", beadID)
		if err := os.RemoveAll(inRepoWt); err == nil {
			fmt.Printf("  %s✗ in-repo worktree removed: %s%s\n", dim, inRepoWt, reset)
		}

		// Delete matching branches: epic/<bead-id>, feat/<bead-id>, feat/<bead-id>.*
		rc := &spgit.RepoContext{Dir: cwd}
		branchPatterns := []string{
			"epic/" + beadID,
			"feat/" + beadID,
			"feat/" + beadID + ".*",
		}
		for _, pattern := range branchPatterns {
			branches := rc.ListBranches(pattern)
			for _, branch := range branches {
				if err := rc.ForceDeleteBranch(branch); err != nil {
					fmt.Printf("  %s(note: could not delete branch %s: %s)%s\n", dim, branch, err, reset)
				} else {
					fmt.Printf("  %s✗ branch deleted: %s%s\n", dim, branch, reset)
				}
			}
		}
	}

	fmt.Printf("%s reset to %s\n", beadID, toPhase)

	// --- 7. Re-summon ---

	fmt.Printf("  %s↑ re-summoning wizard for %s%s\n", cyan, beadID, reset)
	if err := cmdSummon([]string{"1", "--targets", beadID}); err != nil {
		return fmt.Errorf("re-summon %s: %w", beadID, err)
	}

	return nil
}
