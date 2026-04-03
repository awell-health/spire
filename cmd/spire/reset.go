package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/awell-health/spire/pkg/executor"
	"github.com/awell-health/spire/pkg/formula"
	spgit "github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/recovery"
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
				return fmt.Errorf("--to requires a step/phase name")
			}
			i++
			toPhase = args[i]
		default:
			if strings.HasPrefix(args[i], "-") {
				return fmt.Errorf("unknown flag %q\nusage: spire reset <bead-id> [--to <phase>] [--hard]", args[i])
			}
			beadID = args[i]
		}
	}

	if beadID == "" {
		return fmt.Errorf("usage: spire reset <bead-id> [--to <step>] [--hard]")
	}

	if hard && toPhase != "" {
		return fmt.Errorf("cannot use --hard and --to together: --hard deletes all state, --to rewinds to a specific step")
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

	// All formulas are v3 step graphs.
	if toPhase != "" {
		return softResetV3(beadID, toPhase, wizardName)
	}
	return resetV3(beadID, hard, wizardName, worktreePath)
}

// TODO(spi-tph8j): audit resummon, recover, close-advance, and summon resume
// detection for the same v2 phase assumptions that were fixed here. See spi-xig2d
// for the full audit of every v2 assumption in those files.

// resetV3 performs a full reset for v3 (step-graph) formulas.
// Mirrors the manual reset procedure: clear graph state, clean git artifacts,
// remove internal execution artifacts on hard reset, reopen subtask children,
// and reset the epic bead.
func resetV3(beadID string, hard bool, wizardName, worktreePath string) error {
	// --- 1. Remove v3 graph state files (parent + nested) ---
	removeGraphStateFiles(wizardName)

	// Also remove v2 state.json if it exists (belt and suspenders).
	statePath := executorStatePath(wizardName)
	if err := os.Remove(statePath); err == nil {
		fmt.Printf("  %s✗ v2 state file also removed%s\n", dim, reset)
	}

	// --- 2. Process children: close internal DAG beads, reopen subtask children ---
	children, err := storeGetChildren(beadID)
	if err != nil {
		return fmt.Errorf("get children for %s: %w", beadID, err)
	}

	// Build set of protected bead IDs — never touch these during reset.
	// Includes design beads (discovered-from), recovery beads, and alert beads.
	protectedIDs := buildProtectedBeadIDs(beadID, children)

	// Filter out protected children before processing.
	var processable []Bead
	for _, child := range children {
		if !protectedIDs[child.ID] {
			processable = append(processable, child)
		}
	}

	var counts internalDAGCleanupCounts
	if hard {
		counts = deleteInternalDAGBeadsRecursive(processable)
	} else {
		counts = cleanupInternalDAGChildren(processable, false)
	}
	logInternalDAGCleanup(counts)
	reopenedChildren := 0

	for _, child := range processable {
		if isInternalDAGBead(child) {
			continue
		}

		// Subtask children (task/feature/etc.): reopen them so the epic can re-dispatch.
		if child.Status != "open" {
			if err := storeUpdateBead(child.ID, map[string]interface{}{"status": "open", "assignee": ""}); err != nil {
				fmt.Printf("  %s(note: could not reopen subtask %s: %s)%s\n", dim, child.ID, err, reset)
			} else {
				reopenedChildren++
			}
		}
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

	// --- 5. Close related recovery beads ---
	if err := recovery.CloseRelatedRecoveryBeads(storeBridgeOps{}, beadID, "reset (v3)"); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not close recovery beads: %v\n", err)
	}

	// --- 6. Git cleanup (hard reset: worktrees + branches) ---
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

// softResetV3 rewinds a v3 bead's graph state to a specific step and everything
// after it, preserving completed steps before the target. Closes step beads for
// reset steps (preserving audit trail), deletes nested graph state for those
// steps, and cleans up step-scoped workspaces.
func softResetV3(beadID, targetStep, wizardName string) error {
	// --- 1. Load and validate formula ---
	bead, err := storeGetBead(beadID)
	if err != nil {
		return fmt.Errorf("get bead %s: %w", beadID, err)
	}

	anyFormula, version, err := ResolveFormulaAny(bead)
	if err != nil {
		return fmt.Errorf("resolve formula for %s: %w", beadID, err)
	}
	if version != 3 {
		return fmt.Errorf("--to with v3 logic called on v%d formula", version)
	}
	graph := anyFormula.(*formula.FormulaStepGraph)

	// Validate target step exists in the formula.
	if _, ok := graph.Steps[targetStep]; !ok {
		var validSteps []string
		for name := range graph.Steps {
			validSteps = append(validSteps, name)
		}
		return fmt.Errorf("step %q not found in formula %s (valid steps: %s)",
			targetStep, graph.Name, strings.Join(validSteps, ", "))
	}

	// --- 2. Compute steps to reset (target + all transitive dependents) ---
	stepsToReset := computeStepsToReset(graph, targetStep)

	fmt.Printf("  %sresetting steps: %s%s\n", dim, strings.Join(mapKeys(stepsToReset), ", "), reset)

	// --- 3. Load graph state ---
	gs, err := executor.LoadGraphState(wizardName, configDir)
	if err != nil {
		return fmt.Errorf("load graph state for %s: %w", wizardName, err)
	}
	if gs == nil {
		fmt.Printf("  %s(no graph state found — nothing to rewind)%s\n", dim, reset)
		goto resummon
	}

	{
		// --- 4. Rewind step states ---
		resetCount := 0
		for stepName := range stepsToReset {
			ss, ok := gs.Steps[stepName]
			if !ok {
				continue
			}
			ss.Status = "pending"
			ss.Outputs = nil
			ss.StartedAt = ""
			ss.CompletedAt = ""
			// Preserve CompletedCount — it's mechanical and never reset.
			gs.Steps[stepName] = ss
			resetCount++
		}
		if resetCount > 0 {
			fmt.Printf("  %s↺ rewound %d step(s) to pending%s\n", yellow, resetCount, reset)
		}

		// Clear active step if it's in the reset set.
		if stepsToReset[gs.ActiveStep] {
			gs.ActiveStep = ""
		}

		// --- 5. Workspace cleanup (step-scoped only) ---
		cwd, _ := os.Getwd()
		rc := &spgit.RepoContext{Dir: cwd}
		for stepName := range stepsToReset {
			stepCfg, ok := graph.Steps[stepName]
			if !ok || stepCfg.Workspace == "" {
				continue
			}
			ws, ok := gs.Workspaces[stepCfg.Workspace]
			if !ok {
				continue
			}
			// Only clean step-scoped workspaces — run-scoped ones are shared.
			if ws.Scope != formula.WorkspaceScopeStep {
				continue
			}
			if ws.Dir != "" {
				rc.ForceRemoveWorktree(ws.Dir)
				rc.PruneWorktrees()
				fmt.Printf("  %s✗ removed worktree: %s%s\n", dim, ws.Dir, reset)
			}
			if ws.Branch != "" {
				if err := rc.ForceDeleteBranch(ws.Branch); err == nil {
					fmt.Printf("  %s✗ deleted branch: %s%s\n", dim, ws.Branch, reset)
				}
			}
			delete(gs.Workspaces, stepCfg.Workspace)
		}

		// --- 6. Delete nested graph state for reset steps ---
		dir, _ := configDir()
		if dir != "" {
			runtimeDir := filepath.Join(dir, "runtime")
			for stepName := range stepsToReset {
				nestedName := wizardName + "-" + stepName
				nestedPath := filepath.Join(runtimeDir, nestedName, "graph_state.json")
				if err := os.Remove(nestedPath); err == nil {
					fmt.Printf("  %s✗ nested graph state removed: %s%s\n", dim, nestedName, reset)
				}
				// Also remove nested v2 state.json.
				nestedV2Path := filepath.Join(runtimeDir, nestedName, "state.json")
				os.Remove(nestedV2Path)
			}
		}

		// --- 7. Close step beads for reset steps (preserves audit trail) ---
		closedSteps := 0
		for stepName := range stepsToReset {
			stepBeadID := gs.StepBeadIDs[stepName]
			if stepBeadID == "" {
				continue
			}
			// Annotate the closure with a reset reason before closing.
			_ = storeAddComment(stepBeadID, fmt.Sprintf("Closed by soft-reset --to %s (step rewound to pending)", targetStep))
			if err := storeCloseBead(stepBeadID); err != nil {
				fmt.Printf("  %s(note: could not close step bead %s for %s: %s)%s\n", dim, stepBeadID, stepName, err, reset)
			} else {
				closedSteps++
			}
		}
		if closedSteps > 0 {
			fmt.Printf("  %s✗ closed %d step beads%s\n", dim, closedSteps, reset)
		}

		// --- 7b. Close related recovery beads ---
		if err := recovery.CloseRelatedRecoveryBeads(storeBridgeOps{}, beadID, "reset --to (v3)"); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not close recovery beads: %v\n", err)
		}

		// --- 8. Save updated graph state ---
		if err := gs.Save(wizardName, configDir); err != nil {
			return fmt.Errorf("save graph state: %w", err)
		}
		fmt.Printf("  %s✓ graph state saved%s\n", green, reset)
	}

	// --- 9. Set bead to in_progress (it was interrupted, summon will resume) ---
	if err := storeUpdateBead(beadID, map[string]interface{}{"status": "in_progress"}); err != nil {
		fmt.Printf("  %s(note: could not set %s to in_progress: %s)%s\n", dim, beadID, err, reset)
	} else {
		fmt.Printf("  %s↺ %s set to in_progress%s\n", dim, beadID, reset)
	}

resummon:
	fmt.Printf("%s soft-reset to step %q (v3)\n", beadID, targetStep)

	// --- 10. Re-summon ---
	fmt.Printf("  %s↑ re-summoning wizard for %s%s\n", cyan, beadID, reset)
	if err := cmdSummon([]string{"1", "--targets", beadID}); err != nil {
		return fmt.Errorf("re-summon %s: %w", beadID, err)
	}

	return nil
}

// computeStepsToReset builds the set of steps that need resetting: the target
// step plus all steps that transitively depend on it (forward reachability in
// the dependency graph).
func computeStepsToReset(graph *formula.FormulaStepGraph, targetStep string) map[string]bool {
	// Build forward adjacency: step → steps that need it.
	forward := make(map[string][]string)
	for name, step := range graph.Steps {
		for _, need := range step.Needs {
			forward[need] = append(forward[need], name)
		}
	}

	// BFS from target step.
	result := map[string]bool{targetStep: true}
	queue := []string{targetStep}
	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]
		for _, dep := range forward[curr] {
			if !result[dep] {
				result[dep] = true
				queue = append(queue, dep)
			}
		}
	}
	return result
}

type internalDAGCleanupCounts struct {
	DeletedSteps        int
	DeletedAttempts     int
	DeletedReviewRounds int
	ClosedSteps         int
	ClosedAttempts      int
	ClosedReviewRounds  int
}

func isInternalDAGBead(b Bead) bool {
	return isStepBead(b) || isAttemptBead(b) || isReviewRoundBead(b)
}

func cleanupInternalDAGChildren(children []Bead, hard bool) internalDAGCleanupCounts {
	var counts internalDAGCleanupCounts

	for _, child := range children {
		kind := ""
		switch {
		case isStepBead(child):
			kind = "step"
		case isReviewRoundBead(child):
			kind = "review"
		case isAttemptBead(child):
			kind = "attempt"
		default:
			continue
		}

		if hard {
			if err := storeDeleteBeadFunc(child.ID); err != nil {
				fmt.Printf("  %s(note: could not delete %s: %s)%s\n", dim, child.ID, err, reset)
				continue
			}
			switch kind {
			case "step":
				counts.DeletedSteps++
			case "review":
				counts.DeletedReviewRounds++
			case "attempt":
				counts.DeletedAttempts++
			}
			continue
		}

		if child.Status == "closed" {
			continue
		}
		if err := storeCloseBeadFunc(child.ID); err != nil {
			fmt.Printf("  %s(note: could not close %s: %s)%s\n", dim, child.ID, err, reset)
			continue
		}
		switch kind {
		case "step":
			counts.ClosedSteps++
		case "review":
			counts.ClosedReviewRounds++
		case "attempt":
			counts.ClosedAttempts++
		}
	}

	return counts
}

func logInternalDAGCleanup(counts internalDAGCleanupCounts) {
	if counts.DeletedSteps > 0 {
		fmt.Printf("  %s✗ deleted %d step beads%s\n", dim, counts.DeletedSteps, reset)
	}
	if counts.DeletedAttempts > 0 {
		fmt.Printf("  %s✗ deleted %d attempt beads%s\n", dim, counts.DeletedAttempts, reset)
	}
	if counts.DeletedReviewRounds > 0 {
		fmt.Printf("  %s✗ deleted %d review-round beads%s\n", dim, counts.DeletedReviewRounds, reset)
	}
	if counts.ClosedSteps > 0 {
		fmt.Printf("  %s✗ closed %d step beads%s\n", dim, counts.ClosedSteps, reset)
	}
	if counts.ClosedAttempts > 0 {
		fmt.Printf("  %s✗ closed %d attempt beads%s\n", dim, counts.ClosedAttempts, reset)
	}
	if counts.ClosedReviewRounds > 0 {
		fmt.Printf("  %s✗ closed %d review-round beads%s\n", dim, counts.ClosedReviewRounds, reset)
	}
}

// mapKeys returns the keys of a map as a sorted slice.
func mapKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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

// buildProtectedBeadIDs builds a set of bead IDs that should never be touched
// during reset. This includes:
//   - Design beads (linked via discovered-from deps)
//   - Recovery beads (linked via recovery-for or caused-by deps, or with recovery-bead label)
//   - Alert beads (linked via caused-by deps with alert:* label, or children with alert:* label)
func buildProtectedBeadIDs(beadID string, children []Bead) map[string]bool {
	protectedIDs := make(map[string]bool)

	// Design beads: linked as dependencies via discovered-from.
	if deps, err := storeGetDepsWithMeta(beadID); err == nil {
		for _, dep := range deps {
			if dep.DependencyType == "discovered-from" {
				protectedIDs[dep.ID] = true
			}
		}
	}

	// Recovery and alert beads: linked as dependents via recovery-for or caused-by.
	if dependents, err := storeGetDependentsWithMeta(beadID); err == nil {
		for _, dep := range dependents {
			if dep.DependencyType == "recovery-for" || dep.DependencyType == "caused-by" {
				protectedIDs[dep.ID] = true
			}
		}
	}

	// Belt-and-suspenders: protect children with recovery-bead or alert:* labels
	// even if they weren't found via dependency edges.
	for _, child := range children {
		if isProtectedByLabel(child) {
			protectedIDs[child.ID] = true
		}
	}

	return protectedIDs
}

// isProtectedByLabel returns true if a bead should be protected from reset
// based on its labels (recovery-bead or alert:* labels).
func isProtectedByLabel(b Bead) bool {
	for _, l := range b.Labels {
		if l == "recovery-bead" {
			return true
		}
		if strings.HasPrefix(l, "alert:") {
			return true
		}
	}
	return false
}

// deleteInternalDAGBeadsRecursive deletes all internal DAG beads and their
// descendants. For nested internal beads (e.g., attempt children under step beads),
// it deletes bottom-up (leaves first) to avoid orphaned children.
func deleteInternalDAGBeadsRecursive(children []Bead) internalDAGCleanupCounts {
	var counts internalDAGCleanupCounts

	for _, child := range children {
		kind := ""
		switch {
		case isStepBead(child):
			kind = "step"
		case isReviewRoundBead(child):
			kind = "review"
		case isAttemptBead(child):
			kind = "attempt"
		default:
			continue
		}

		// Recursively delete descendants first (bottom-up).
		deleteBeadDescendants(child.ID)

		if err := storeDeleteBeadFunc(child.ID); err != nil {
			fmt.Printf("  %s(note: could not delete %s: %s)%s\n", dim, child.ID, err, reset)
			continue
		}
		switch kind {
		case "step":
			counts.DeletedSteps++
		case "review":
			counts.DeletedReviewRounds++
		case "attempt":
			counts.DeletedAttempts++
		}
	}

	return counts
}

// deleteBeadDescendants recursively deletes all children of a bead.
// Used to clean up nested internal DAG beads before deleting their parent.
// Uses storeGetChildrenFunc for testability.
func deleteBeadDescendants(parentID string) {
	children, err := storeGetChildrenFunc(parentID)
	if err != nil || len(children) == 0 {
		return
	}
	for _, child := range children {
		deleteBeadDescendants(child.ID)
		_ = storeDeleteBeadFunc(child.ID)
	}
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
