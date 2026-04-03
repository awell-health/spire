package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/awell-health/spire/pkg/executor"
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

	// Strip interrupted:* and needs-human labels — reset fully clears stuck state.
	for _, l := range bead.Labels {
		if strings.HasPrefix(l, "interrupted:") || l == "needs-human" {
			if err := storeRemoveLabel(beadID, l); err != nil {
				fmt.Printf("  %s(note: could not remove %s from %s: %s)%s\n", dim, l, beadID, err, reset)
			} else {
				fmt.Printf("  %s✓ cleared %s from %s%s\n", green, l, beadID, reset)
			}
		}
	}

	// Detect formula version: v2 or v3.
	anyFormula, version, err := ResolveFormulaAny(bead)
	if err != nil {
		return fmt.Errorf("resolve formula for %s: %w", beadID, err)
	}

	if version == 3 {
		if toPhase != "" {
			return fmt.Errorf("--to is not supported for v3 formulas; use --hard to fully reset")
		}
		return resetV3(beadID, hard, wizardName, worktreePath)
	}

	// --- v2 path (unchanged) ---

	f := anyFormula.(*FormulaV2)

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
		resetCleanWorktreesAndBranches(beadID, worktreePath, wizardName)
	}

	fmt.Printf("%s reset to %s\n", beadID, toPhase)

	// --- 7. Re-summon ---

	fmt.Printf("  %s↑ re-summoning wizard for %s%s\n", cyan, beadID, reset)
	if err := cmdSummon([]string{"1", "--targets", beadID}); err != nil {
		return fmt.Errorf("re-summon %s: %w", beadID, err)
	}

	return nil
}

// resetV3 performs a full reset for v3 (step-graph) formulas.
// Mirrors the manual reset procedure: clear graph state, clean git artifacts,
// close step/attempt beads, reopen subtask children, reset the epic bead.
func resetV3(beadID string, hard bool, wizardName, worktreePath string) error {
	// --- 1. Remove v3 graph state files (parent + nested) ---
	removeGraphStateFiles(wizardName)

	// Also remove v2 state.json if it exists (belt and suspenders).
	statePath := executorStatePath(wizardName)
	if err := os.Remove(statePath); err == nil {
		fmt.Printf("  %s✗ v2 state file also removed%s\n", dim, reset)
	}

	// --- 2. Process children: close step/attempt beads, reopen subtask children ---
	children, err := storeGetChildren(beadID)
	if err != nil {
		return fmt.Errorf("get children for %s: %w", beadID, err)
	}

	closedSteps := 0
	closedAttempts := 0
	reopenedChildren := 0

	for _, child := range children {
		if isStepBead(child) || isAttemptBead(child) {
			// Step and attempt beads: close them. They'll be recreated on re-summon.
			if child.Status != "closed" {
				if err := storeCloseBead(child.ID); err != nil {
					fmt.Printf("  %s(note: could not close %s: %s)%s\n", dim, child.ID, err, reset)
				} else if isStepBead(child) {
					closedSteps++
				} else {
					closedAttempts++
				}
			}
			continue
		}

		// Subtask children (task/feature/etc.): reopen them so the epic can re-dispatch.
		if child.Status != "open" && child.Status != "closed" {
			// Only reopen in_progress children — leave closed ones alone (they completed successfully).
			if err := storeUpdateBead(child.ID, map[string]interface{}{"status": "open", "assignee": ""}); err != nil {
				fmt.Printf("  %s(note: could not reopen subtask %s: %s)%s\n", dim, child.ID, err, reset)
			} else {
				reopenedChildren++
			}
		}
	}

	if closedSteps > 0 {
		fmt.Printf("  %s✗ closed %d step beads%s\n", dim, closedSteps, reset)
	}
	if closedAttempts > 0 {
		fmt.Printf("  %s✗ closed %d attempt beads%s\n", dim, closedAttempts, reset)
	}
	if reopenedChildren > 0 {
		fmt.Printf("  %s↺ reopened %d subtask children%s\n", yellow, reopenedChildren, reset)
	}

	// --- 3. Strip labels from the bead ---
	bead, err := storeGetBead(beadID)
	if err != nil {
		return fmt.Errorf("get bead %s: %w", beadID, err)
	}
	for _, l := range bead.Labels {
		if strings.HasPrefix(l, "feat-branch:") ||
			strings.HasPrefix(l, "interrupted:") ||
			l == "needs-human" {
			if err := storeRemoveLabel(beadID, l); err != nil {
				fmt.Printf("  %s(note: could not remove %s: %s)%s\n", dim, l, err, reset)
			} else {
				fmt.Printf("  %s✓ cleared %s%s\n", green, l, reset)
			}
		}
	}

	// --- 4. Reset bead status to open (not in_progress — summon will claim it) ---
	if err := storeUpdateBead(beadID, map[string]interface{}{"status": "open", "assignee": ""}); err != nil {
		fmt.Printf("  %s(note: could not set %s to open: %s)%s\n", dim, beadID, err, reset)
	} else {
		fmt.Printf("  %s↺ %s set to open%s\n", yellow, beadID, reset)
	}

	// --- 5. Git cleanup (hard reset: worktrees + branches) ---
	if hard {
		resetCleanWorktreesAndBranches(beadID, worktreePath, wizardName)
	}

	fmt.Printf("%s reset (v3)\n", beadID)

	// --- 6. Re-summon ---
	fmt.Printf("  %s↑ re-summoning wizard for %s%s\n", cyan, beadID, reset)
	if err := cmdSummon([]string{"1", "--targets", beadID}); err != nil {
		return fmt.Errorf("re-summon %s: %w", beadID, err)
	}

	return nil
}

// graphStatePath returns the v3 graph_state.json path for a given agent name.
func graphStatePath(agentName string) string {
	return executor.GraphStatePath(agentName, configDir)
}

// removeGraphStateFiles removes the v3 graph state for a wizard and all its
// nested sub-executors (apprentice, sage, etc.). Prints progress to stdout.
func removeGraphStateFiles(wizardName string) {
	doRemoveGraphStateFiles(wizardName, false)
}

// removeGraphStateFilesQuiet is like removeGraphStateFiles but suppresses output.
// Returns true if any graph state file was removed.
func removeGraphStateFilesQuiet(wizardName string) bool {
	return doRemoveGraphStateFiles(wizardName, true)
}

// doRemoveGraphStateFiles removes v3 graph state (parent + nested) and optionally
// nested v2 state files. Returns true if any file was removed.
func doRemoveGraphStateFiles(wizardName string, quiet bool) bool {
	removed := false

	// Remove parent graph state.
	gsPath := graphStatePath(wizardName)
	if err := os.Remove(gsPath); err == nil {
		removed = true
		if !quiet {
			fmt.Printf("  %s✗ graph state removed%s\n", dim, reset)
		}
	}

	// Remove nested graph states: wizard-<name>-* directories.
	dir, err := configDir()
	if err != nil {
		return removed
	}
	runtimeDir := filepath.Join(dir, "runtime")
	pattern := filepath.Join(runtimeDir, wizardName+"-*", "graph_state.json")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return removed
	}
	for _, m := range matches {
		if err := os.Remove(m); err == nil {
			removed = true
			if !quiet {
				fmt.Printf("  %s✗ nested graph state removed: %s%s\n", dim, filepath.Base(filepath.Dir(m)), reset)
			}
		}
	}

	// Also glob nested state.json (v2 nested executors).
	v2Pattern := filepath.Join(runtimeDir, wizardName+"-*", "state.json")
	v2Matches, _ := filepath.Glob(v2Pattern)
	for _, m := range v2Matches {
		if err := os.Remove(m); err == nil {
			removed = true
			if !quiet {
				fmt.Printf("  %s✗ nested v2 state removed: %s%s\n", dim, filepath.Base(filepath.Dir(m)), reset)
			}
		}
	}

	return removed
}

// resetCleanWorktreesAndBranches removes worktrees and branches for a bead.
// Shared between v2 hard-reset and v3 hard-reset paths.
func resetCleanWorktreesAndBranches(beadID, worktreePath, wizardName string) {
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

	// Also remove .worktrees/<bead-id>-* (feature worktrees for subtasks).
	wtMatches, _ := filepath.Glob(filepath.Join(cwd, ".worktrees", beadID+"-*"))
	for _, m := range wtMatches {
		if err := os.RemoveAll(m); err == nil {
			fmt.Printf("  %s✗ subtask worktree removed: %s%s\n", dim, filepath.Base(m), reset)
		}
	}

	// Delete matching branches: epic/<bead-id>, feat/<bead-id>, feat/<bead-id>.*, staging/<bead-id>
	rc := &spgit.RepoContext{Dir: cwd}
	// Prune stale worktree refs so branch deletion succeeds even if the
	// worktree directory was already removed above.
	rc.PruneWorktrees()
	branchPatterns := []string{
		"epic/" + beadID,
		"feat/" + beadID,
		"feat/" + beadID + ".*",
		"staging/" + beadID,
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
