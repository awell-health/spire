// Package main — reset.go implements `spire reset`.
//
// Reset model
//
// `spire reset <bead>` does exactly one thing: it returns a bead to a clean
// "ready to be summoned again" state. It does NOT re-summon. Callers who
// want to resume (recover, board action, scripts) must call `spire summon`
// explicitly as a separate step.
//
// Two variants:
//
//   - Plain reset (`spire reset <bead>` and `--hard`): wipes all graph state
//     for the bead and (in hard mode) removes worktrees/branches. After this
//     the bead is set to "open".
//
//   - Soft reset (`--to <step>`): rewinds the bead to a specific earlier step.
//     Steps before the target stay completed; the target step + everything
//     that transitively depends on it is rewound to "pending" and their step
//     beads are re-opened. Rewind only — the target must have already been
//     reached (Status != "pending"); a pending target is rejected. Two flags
//     compose onto `--to` for manual operator override:
//
//     * `--force` drops the "target must be reached" precondition. Pending
//       steps outside the rewind set are force-advanced to completed with
//       empty outputs, so the graph can route forward from target on resume.
//
//     * `--set <step>.outputs.<key>=<value>` writes output overrides onto
//       any step in the graph (repeatable; value may contain `=`). Overrides
//       apply regardless of whether the step is inside the rewind set. When
//       an override is applied, any completed step downstream of the
//       overridden step is added to the rewind set so its when-clause can
//       re-evaluate on next summon.
//
//     Canonical spi-cwgiy9 replay (epic stuck in implement-failed terminal
//     with the apprentice work actually good):
//
//         spire reset spi-cwgiy9 --to review --force \
//             --set implement.outputs.outcome=verified
//
//     This force-advances past the missing precondition (review was never
//     reached), overrides implement's output so review's when-clause fires
//     and implement-failed's doesn't, and rewinds implement-failed so it
//     re-evaluates instead of routing to bead.finish on resume.
//
// Child-bead categorization
//
// Reset treats the children of a bead in three groups:
//
//  1. Internal DAG beads (workflow-step, attempt, review-round):
//     - soft reset → CLOSED (audit trail preserved)
//     - hard reset → DELETED (including nested descendants like attempt→step)
//     - `--to <step>` → step beads in the rewind set are REOPENED (not closed)
//     so the graph re-enters those steps on the next summon. Everything
//     outside the rewind set is left alone.
//
//  2. Real subtask children (type=task/bug/feature/etc. under an epic):
//     - plain/hard reset → reopened if currently open/in_progress, leaving
//     closed subtasks alone. They get re-dispatched on resummon.
//     - `--to <step>` → unchanged (reset scoped to the rewound steps only).
//
//  3. Protected beads:
//     - design beads (linked via discovered-from dep)
//     - recovery beads (dep type recovery-for or caused-by, or labeled
//     `recovery-bead`)
//     - alert beads (labeled `alert:*` or linked via caused-by)
//     These are NEVER touched by reset. `recovery.CloseRelatedRecoveryBeads`
//     runs separately to close recovery-for dependents that are no longer
//     applicable.
//
// Label stripping
//
// Reset removes the "stuck state" labels from the bead itself:
//   - `interrupted:*` (executor-exit, build-failure, etc.)
//   - `needs-human`
//   - `feat-branch:*` (cleared only on hard reset and on plain reset)
//
// Unrelated labels (type tags, topic labels, etc.) are preserved.
//
// Graph state, worktrees, and branches
//
//   - Plain/hard reset: deletes the top-level graph_state.json plus any
//     nested sub-executor state matching `<wizardName>-*/graph_state.json`.
//     Hard reset also removes the wizard's worktree directory and the
//     feat/<bead>, epic/<bead>, staging/<bead> branches.
//
//   - `--to <step>` soft reset: rewinds state for the rewound steps and
//     recursively deletes every `graph_state.json` under the wizard's
//     runtime dir — subgraph-dispatched wave apprentices live at arbitrary
//     nesting depth and their state must be torn down before resummon can
//     rebuild it cleanly. Wave-apprentice beads themselves are left alone;
//     they get re-dispatched on the next summon.
//
// Step beads vs wave-apprentice beads
//
// The reopen-on-rewind logic in `--to` applies only to TOP-LEVEL step beads
// (the ones tracked in GraphState.StepBeadIDs). Subgraphs dispatch wave
// apprentices as separate beads with their own lifecycle — reset deletes
// their state files but does NOT reopen closed wave-apprentice beads; the
// epic re-dispatches them on resummon.
package main

import (
	"errors"
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

// ErrNoGraphState is returned by softResetV3 when the bead has no graph
// state file on disk — there is nothing to rewind. Plain reset also exits
// early on this condition, but returns nil because plain reset has useful
// work to do (strip labels, reopen subtasks) even without graph state.
var ErrNoGraphState = errors.New("no graph state to rewind")

// cmdSummonFunc is the function reset callers (recover, board) reach for when
// they chain a resummon after reset. Tests swap it to a no-op recorder to
// assert that reset itself does NOT invoke summon.
var cmdSummonFunc = cmdSummon

// cmdResummonFunc is the function recover/board use to chain a resummon after
// reset. Held in a var so tests can observe the call and verify the chain.
var cmdResummonFunc = func(beadID string) error {
	return cmdResummon([]string{beadID})
}

// cmdResetFunc indirection lets recover tests stub cmdReset without
// triggering real store mutations.
var cmdResetFunc = cmdReset

// storeActivateStepBeadFunc is a test-replaceable hook for reopening a closed
// step bead. Production wiring delegates to store.ActivateStepBead.
var storeActivateStepBeadFunc = storeActivateStepBead

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
		if force, _ := cmd.Flags().GetBool("force"); force {
			fullArgs = append(fullArgs, "--force")
		}
		if sets, _ := cmd.Flags().GetStringArray("set"); len(sets) > 0 {
			for _, s := range sets {
				fullArgs = append(fullArgs, "--set", s)
			}
		}
		return cmdReset(fullArgs)
	},
}

func init() {
	resetCmd.Flags().Bool("hard", false, "Hard reset (delete worktree and state)")
	resetCmd.Flags().String("to", "", "Reset to a specific step")
	resetCmd.Flags().Bool("force", false, "With --to, bypass the 'target must have been reached' precondition; pending steps outside the rewind set are advanced to completed. Example: --to review --force --set implement.outputs.outcome=verified")
	resetCmd.Flags().StringArray("set", nil, "With --to, override a step's outputs (format: <step>.outputs.<key>=<value>; repeatable). Rejects '<step>.status=...'. Example: --set review.outputs.outcome=merge")
}

func cmdReset(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: spire reset <bead-id> [--to <step> [--force] [--set <step>.outputs.<key>=<value>]...] [--hard]")
	}

	var beadID string
	var toPhase string
	var hard bool
	var force bool
	var setArgs []string
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
		case "--force":
			force = true
		case "--set":
			if i+1 >= len(args) {
				return fmt.Errorf("--set requires <step>.outputs.<key>=<value>")
			}
			i++
			setArgs = append(setArgs, args[i])
		default:
			if strings.HasPrefix(args[i], "-") {
				return fmt.Errorf("unknown flag %q\nusage: spire reset <bead-id> [--to <step> [--force] [--set <step>.outputs.<key>=<value>]...] [--hard]", args[i])
			}
			beadID = args[i]
		}
	}

	if beadID == "" {
		return fmt.Errorf("usage: spire reset <bead-id> [--to <step> [--force] [--set <step>.outputs.<key>=<value>]...] [--hard]")
	}

	if hard && toPhase != "" {
		return fmt.Errorf("cannot use --hard and --to together: --hard deletes all state, --to rewinds to a specific step")
	}
	if (force || len(setArgs) > 0) && toPhase == "" {
		return fmt.Errorf("--force and --set require --to <step>")
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

	// Unhook any hooked child step beads before reset.
	if children, err := storeGetChildren(beadID); err == nil {
		for _, child := range children {
			if isStepBead(child) && child.Status == "hooked" {
				if err := storeUnhookStepBead(child.ID); err != nil {
					fmt.Printf("  %s(note: could not unhook step %s: %s)%s\n", dim, child.ID, err, reset)
				} else {
					fmt.Printf("  %s✓ unhooked step %s%s\n", green, child.ID, reset)
				}
			}
		}
	}

	// All formulas are v3 step graphs.
	if toPhase != "" {
		return softResetV3(beadID, toPhase, wizardName, force, setArgs)
	}
	return resetV3(beadID, hard, wizardName, worktreePath)
}

// TODO(spi-tph8j): audit resummon, recover, close-advance, and summon resume
// detection for the same v2 phase assumptions that were fixed here. See spi-xig2d
// for the full audit of every v2 assumption in those files.

// hardResetBeadCore performs the destructive core of a hard reset: kills wizard,
// removes graph state, deletes internal DAG beads, strips labels, sets bead to
// open, and cleans up worktrees/branches. Does NOT close recovery beads (the
// caller may itself be a recovery bead) and does NOT re-summon (the caller
// controls what happens after reset).
//
// This function is exposed as the executor.Deps.HardResetBead callback so the
// recovery executor can invoke a full hard reset from within a recovery formula.
func hardResetBeadCore(beadID string) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	// --- 1. Kill wizard process if alive + remove registry entry ---
	reg := loadWizardRegistry()
	var wizardName string
	var worktreePath string
	for i := range reg.Wizards {
		if reg.Wizards[i].BeadID == beadID {
			wiz := &reg.Wizards[i]
			wizardName = wiz.Name
			worktreePath = wiz.Worktree

			if wiz.PID > 0 && processAlive(wiz.PID) {
				if proc, err := os.FindProcess(wiz.PID); err == nil {
					proc.Signal(syscall.SIGTERM)
					deadline := time.Now().Add(5 * time.Second)
					for time.Now().Before(deadline) {
						time.Sleep(200 * time.Millisecond)
						if !processAlive(wiz.PID) {
							break
						}
					}
					if processAlive(wiz.PID) {
						proc.Signal(syscall.SIGKILL)
					}
				}
			}

			var remaining []localWizard
			for _, w := range reg.Wizards {
				if w.BeadID != beadID {
					remaining = append(remaining, w)
				}
			}
			reg.Wizards = remaining
			saveWizardRegistry(reg)
			break
		}
	}
	if wizardName == "" {
		wizardName = "wizard-" + beadID
	}

	// --- 2. Remove graph state files (parent + nested) ---
	removeGraphStateFiles(wizardName)

	// --- 3. Delete internal DAG beads (with protected-set filtering) ---
	children, err := storeGetChildren(beadID)
	if err != nil {
		return fmt.Errorf("get children for %s: %w", beadID, err)
	}

	protectedIDs := buildProtectedBeadIDs(beadID, children)

	var processable []Bead
	for _, child := range children {
		if !protectedIDs[child.ID] {
			processable = append(processable, child)
		}
	}

	counts := deleteInternalDAGBeadsRecursive(processable)
	logInternalDAGCleanup(counts)

	// Reopen subtask children so the epic can re-dispatch.
	reopenedChildren := 0
	for _, child := range processable {
		if isInternalDAGBead(child) {
			continue
		}
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

	// --- 4. Strip labels ---
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

	// --- 5. Set bead status to open, clear assignee ---
	if err := storeUpdateBead(beadID, map[string]interface{}{"status": "open", "assignee": ""}); err != nil {
		fmt.Printf("  %s(note: could not set %s to open: %s)%s\n", dim, beadID, err, reset)
	} else {
		fmt.Printf("  %s↺ %s set to open%s\n", yellow, beadID, reset)
	}

	// --- 6. Git cleanup: worktrees + branches ---
	resetCleanWorktreesAndBranches(beadID, worktreePath, wizardName)

	return nil
}

// resetV3 performs a full reset for v3 (step-graph) formulas.
// Delegates destructive work to hardResetBeadCore when hard=true, then
// handles recovery-bead closure and re-summoning.
func resetV3(beadID string, hard bool, wizardName, worktreePath string) error {
	if hard {
		if err := hardResetBeadCore(beadID); err != nil {
			return err
		}
	} else {
		// --- Soft reset path (no worktree/branch deletion) ---

		// 1. Remove graph state files (parent + nested).
		removeGraphStateFiles(wizardName)

		// 2. Process children: close internal DAG beads, reopen subtask children.
		children, err := storeGetChildren(beadID)
		if err != nil {
			return fmt.Errorf("get children for %s: %w", beadID, err)
		}

		protectedIDs := buildProtectedBeadIDs(beadID, children)
		var processable []Bead
		for _, child := range children {
			if !protectedIDs[child.ID] {
				processable = append(processable, child)
			}
		}

		counts := cleanupInternalDAGChildren(processable, false)
		logInternalDAGCleanup(counts)
		reopenedChildren := 0

		for _, child := range processable {
			if isInternalDAGBead(child) {
				continue
			}
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

		// 3. Strip labels from the bead.
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

		// 4. Reset bead status to open.
		if err := storeUpdateBead(beadID, map[string]interface{}{"status": "open", "assignee": ""}); err != nil {
			fmt.Printf("  %s(note: could not set %s to open: %s)%s\n", dim, beadID, err, reset)
		} else {
			fmt.Printf("  %s↺ %s set to open%s\n", yellow, beadID, reset)
		}
	}

	// --- Close related recovery beads (both soft and hard paths) ---
	if err := recovery.CloseRelatedRecoveryBeads(storeBridgeOps{}, beadID, "reset (v3)"); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not close recovery beads: %v\n", err)
	}

	fmt.Printf("%s reset (v3)\n", beadID)
	// Reset is intentionally decoupled from summon — callers who want to
	// resume must invoke `spire summon` (or cmdResummon) themselves.
	return nil
}

// parseSetFlag parses repeated --set tokens into a nested map of step → key → value.
// Each token must match <step>.outputs.<key>=<value>. Values may contain '=' —
// only the first '=' is treated as the delimiter. Rejects status-writes
// (<step>.status=...), nested paths (outputs.foo.bar), empty segments, and
// references to steps not declared in the formula. Any error is reported
// with the offending token and no partial map is returned.
func parseSetFlag(tokens []string, graph *formula.FormulaStepGraph) (map[string]map[string]string, error) {
	if len(tokens) == 0 {
		return nil, nil
	}
	result := make(map[string]map[string]string)
	for _, tok := range tokens {
		eq := strings.IndexByte(tok, '=')
		if eq < 0 {
			return nil, fmt.Errorf("--set %q: expected <step>.outputs.<key>=<value>", tok)
		}
		lhs, value := tok[:eq], tok[eq+1:]
		segs := strings.Split(lhs, ".")
		if len(segs) != 3 {
			return nil, fmt.Errorf("--set %q: expected <step>.outputs.<key>, got %d path segment(s)", tok, len(segs))
		}
		step, kind, key := segs[0], segs[1], segs[2]
		if kind == "status" {
			return nil, fmt.Errorf("--set %q: setting status is not allowed — --set is scoped to outputs", tok)
		}
		if kind != "outputs" {
			return nil, fmt.Errorf("--set %q: only <step>.outputs.<key> is supported (got middle segment %q)", tok, kind)
		}
		if step == "" || key == "" {
			return nil, fmt.Errorf("--set %q: step and key must be non-empty", tok)
		}
		if _, ok := graph.Steps[step]; !ok {
			var valid []string
			for name := range graph.Steps {
				valid = append(valid, name)
			}
			sort.Strings(valid)
			return nil, fmt.Errorf("--set %q: step %q not found in formula %s (valid steps: %s)",
				tok, step, graph.Name, strings.Join(valid, ", "))
		}
		m, ok := result[step]
		if !ok {
			m = make(map[string]string)
			result[step] = m
		}
		m[key] = value
	}
	return result, nil
}

// expandRewindSetForOverrides extends the base rewind set with any step that
// is a forward dependent of an overridden step AND is currently completed.
// Rationale: when an overridden output would change how a downstream step's
// when-clause routes, the downstream step must be rewound so its condition
// re-evaluates on the next summon. Pending downstream steps are left for
// the standard rewind/force-advance pass to handle.
func expandRewindSetForOverrides(base map[string]bool, graph *formula.FormulaStepGraph, gs *executor.GraphState, overrides map[string]map[string]string) map[string]bool {
	if len(overrides) == 0 {
		return base
	}
	out := make(map[string]bool, len(base))
	for k, v := range base {
		out[k] = v
	}
	for step := range overrides {
		for downstream := range computeStepsToReset(graph, step) {
			if downstream == step {
				continue // overridden step itself is not rewound by override propagation
			}
			if ss, ok := gs.Steps[downstream]; ok && ss.Status == "completed" {
				out[downstream] = true
			}
		}
	}
	return out
}

// softResetV3 rewinds a v3 bead's graph state to a specific step and everything
// that transitively depends on it. Steps before the target are preserved
// untouched. The target and its dependents are set back to "pending" and
// their step beads are re-opened (not closed) so the graph re-enters those
// steps on the next summon.
//
// Rewind semantics (strict):
//   - Target step must have been reached (Status != "pending") unless
//     forceAdvance is true. Fast-forward attempts without --force are
//     rejected with an error and no state is mutated.
//   - Nested graph-state files for subgraph-dispatched wave apprentices are
//     deleted recursively — they live at arbitrary depth under the wizard's
//     runtime dir and re-materialize on resummon.
//   - When graph_state.json is missing, returns ErrNoGraphState. A missing
//     file means there's nothing to rewind, which is distinct from a
//     successful rewind.
//   - After a successful rewind the parent bead is set to "open" (same as
//     plain reset). Soft reset does NOT auto-summon — callers resume
//     explicitly.
//
// Composed flags:
//   - forceAdvance: skip the 'target must be reached' precondition. Pending
//     steps outside the rewind set are promoted to completed with empty
//     outputs so the next summon can route forward from target without the
//     graph stalling on pre-target not-taken branches.
//   - setArgs: repeated "<step>.outputs.<key>=<value>" tokens. Overrides are
//     parsed and validated BEFORE any state mutation (invalid tokens fail
//     early with no partial writes). Any completed step downstream of an
//     overridden step is added to the rewind set so its when-clause
//     re-evaluates on the next summon.
func softResetV3(beadID, targetStep, wizardName string, forceAdvance bool, setArgs []string) error {
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

	// Parse and validate --set tokens BEFORE any state mutation. A typo in
	// --set must not produce a partial write.
	overrides, err := parseSetFlag(setArgs, graph)
	if err != nil {
		return err
	}

	// --- 2. Compute base rewind set (target + all transitive dependents) ---
	baseRewind := computeStepsToReset(graph, targetStep)

	// --- 3. Load graph state (required — no silent resummon) ---
	gs, err := executor.LoadGraphState(wizardName, configDir)
	if err != nil {
		return fmt.Errorf("load graph state for %s: %w", wizardName, err)
	}
	if gs == nil {
		return fmt.Errorf("cannot rewind %s to %q: %w (wizard %s)",
			beadID, targetStep, ErrNoGraphState, wizardName)
	}

	// --- 4. Pre-flight: reject if target step has not been reached (unless --force) ---
	// Rewind-only semantics by default: --to must not be usable to
	// fast-forward. --force drops this check. The check runs BEFORE any
	// mutation so the operation is all-or-nothing.
	if !forceAdvance {
		if ts, ok := gs.Steps[targetStep]; ok && ts.Status == "pending" {
			return fmt.Errorf("cannot rewind %s to %q: step has not been reached yet (pass --force to advance anyway)", beadID, targetStep)
		}
	}

	// --- 4b. Expand rewind set to include stale-completed downstream-of-override steps ---
	stepsToReset := expandRewindSetForOverrides(baseRewind, graph, gs, overrides)

	fmt.Printf("  %sresetting steps: %s%s\n", dim, strings.Join(mapKeys(stepsToReset), ", "), reset)

	// --- 5. Rewind step states ---
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

	// --- 5b. Under --force, promote pending steps outside the rewind set ---
	// to completed with empty outputs. This unblocks forward routing: on the
	// next summon the interpreter sees skipped not-taken branches as already
	// resolved and dispatches from target. Without this, a stuck graph where
	// the target was never reached would still stall on a pending predecessor
	// or sibling branch.
	if forceAdvance {
		advanced := 0
		for stepName, ss := range gs.Steps {
			if stepsToReset[stepName] {
				continue
			}
			if ss.Status != "pending" {
				continue
			}
			ss.Status = "completed"
			ss.Outputs = map[string]string{}
			gs.Steps[stepName] = ss
			advanced++
		}
		if advanced > 0 {
			fmt.Printf("  %s↟ force-advanced %d pending step(s) to completed (outputs={})%s\n", yellow, advanced, reset)
		}
	}

	// --- 5c. Apply --set output overrides AFTER rewind and force-advance ---
	// so override values take precedence over both wiped (nil) and force-
	// advanced (empty-map) outputs. Targets in the rewind set end up as
	// pending with the overridden outputs attached; overrides on steps
	// outside the rewind set update their Outputs map while leaving status
	// unchanged.
	if len(overrides) > 0 {
		overrideCount := 0
		for stepName, kv := range overrides {
			ss, ok := gs.Steps[stepName]
			if !ok {
				continue
			}
			if ss.Outputs == nil {
				ss.Outputs = make(map[string]string, len(kv))
			}
			for k, v := range kv {
				ss.Outputs[k] = v
				overrideCount++
			}
			gs.Steps[stepName] = ss
		}
		if overrideCount > 0 {
			fmt.Printf("  %s✎ applied %d output override(s) across %d step(s)%s\n", yellow, overrideCount, len(overrides), reset)
		}
	}

	// --- 6. Workspace cleanup (step-scoped only) ---
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

	// --- 7. Recursively delete nested graph state under rewound subgraphs ---
	// Subgraph-dispatched wave apprentices live at arbitrary nesting depth,
	// so a literal `<wizardName>-<stepName>` glob misses them. Walk every
	// `graph_state.json` under `runtime/<wizardName>-*/` and delete it —
	// subgraphs re-materialize on resummon. Unrelated wizard trees are
	// naturally excluded because we only walk children of our wizard dir.
	removeNestedGraphStateRecursive(wizardName)

	// --- 8. Reopen step beads in the rewind set (audit trail preserved) ---
	// Rewinding graph state to "pending" while the step bead stays "closed"
	// would deadlock the next summon — the executor's transition path skips
	// activating an already-closed step bead. So we reopen closed step beads
	// here and add an audit comment explaining why. Open/in_progress beads
	// are left as-is.
	reopenedSteps := 0
	leftSteps := 0
	for stepName := range stepsToReset {
		stepBeadID := gs.StepBeadIDs[stepName]
		if stepBeadID == "" {
			continue
		}
		b, err := storeGetBead(stepBeadID)
		if err != nil {
			fmt.Printf("  %s(note: could not read step bead %s for %s: %s)%s\n", dim, stepBeadID, stepName, err, reset)
			continue
		}
		if b.Status == "closed" {
			_ = storeAddComment(stepBeadID, fmt.Sprintf("Reopened by soft-reset --to %s (step rewound to pending)", targetStep))
			if err := storeActivateStepBeadFunc(stepBeadID); err != nil {
				fmt.Printf("  %s(note: could not reopen step bead %s for %s: %s)%s\n", dim, stepBeadID, stepName, err, reset)
				continue
			}
			reopenedSteps++
		} else {
			leftSteps++
		}
	}
	if reopenedSteps > 0 {
		fmt.Printf("  %s↺ reopened %d step bead(s)%s\n", yellow, reopenedSteps, reset)
	}
	if leftSteps > 0 {
		fmt.Printf("  %s(left %d step bead(s) unchanged — already open/in_progress)%s\n", dim, leftSteps, reset)
	}

	// --- 8b. Close related recovery beads ---
	if err := recovery.CloseRelatedRecoveryBeads(storeBridgeOps{}, beadID, "reset --to (v3)"); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not close recovery beads: %v\n", err)
	}

	// --- 9. Save updated graph state ---
	if err := gs.Save(wizardName, configDir); err != nil {
		return fmt.Errorf("save graph state: %w", err)
	}
	fmt.Printf("  %s✓ graph state saved%s\n", green, reset)

	// --- 10. Set bead status to "open" (matches plain reset) ---
	if err := storeUpdateBead(beadID, map[string]interface{}{"status": "open", "assignee": ""}); err != nil {
		fmt.Printf("  %s(note: could not set %s to open: %s)%s\n", dim, beadID, err, reset)
	} else {
		fmt.Printf("  %s↺ %s set to open%s\n", yellow, beadID, reset)
	}

	fmt.Printf("%s soft-reset to step %q (v3)\n", beadID, targetStep)
	// Reset is intentionally decoupled from summon — callers who want to
	// resume must invoke `spire summon` (or cmdResummon) themselves.
	return nil
}

// removeNestedGraphStateRecursive deletes every graph_state.json nested under
// the wizard's runtime tree. Used by softResetV3 to tear down subgraph-
// dispatched wave apprentice state — they can nest arbitrarily deep and a
// literal glob at the first level misses them.
//
// The top-level graph_state.json (runtime/<wizardName>/graph_state.json) is
// NOT touched — it's rewritten in place by softResetV3's gs.Save() call.
func removeNestedGraphStateRecursive(wizardName string) {
	dir, err := configDir()
	if err != nil || dir == "" {
		return
	}
	runtimeDir := filepath.Join(dir, "runtime")

	entries, err := os.ReadDir(runtimeDir)
	if err != nil {
		return
	}
	prefix := wizardName + "-"
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		subtree := filepath.Join(runtimeDir, name)
		_ = filepath.WalkDir(subtree, func(path string, d os.DirEntry, werr error) error {
			if werr != nil {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if filepath.Base(path) != "graph_state.json" {
				return nil
			}
			if err := os.Remove(path); err == nil {
				rel, _ := filepath.Rel(runtimeDir, path)
				fmt.Printf("  %s✗ nested graph state removed: %s%s\n", dim, rel, reset)
			}
			return nil
		})
	}
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
