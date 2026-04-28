// Package main — reset.go implements `spire reset`.
//
// # Reset model
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
//   - `--force` drops the "target must be reached" precondition. Pending
//     steps outside the rewind set are force-advanced to completed with
//     empty outputs, so the graph can route forward from target on resume.
//
//   - `--set <step>.outputs.<key>=<value>` writes output overrides onto
//     any step in the graph (repeatable; value may contain `=`). Overrides
//     apply regardless of whether the step is inside the rewind set. When
//     an override is applied, any completed step downstream of the
//     overridden step is added to the rewind set so its when-clause can
//     re-evaluate on next summon.
//
//     Canonical spi-cwgiy9 replay (epic stuck in implement-failed terminal
//     with the apprentice work actually good):
//
//     spire reset spi-cwgiy9 --to review --force \
//     --set implement.outputs.outcome=verified
//
//     This force-advances past the missing precondition (review was never
//     reached), overrides implement's output so review's when-clause fires
//     and implement-failed's doesn't, and rewinds implement-failed so it
//     re-evaluates instead of routing to bead.finish on resume.
//
// # Child-bead categorization
//
// Reset treats the children of a bead in three groups:
//
//  1. Internal DAG beads (workflow-step, attempt, review-round):
//     - soft reset → step beads CLOSED, attempt/review beads CLOSED and
//     stamped with reset-cycle:<N>
//     - hard reset → step beads DELETED (including nested descendants);
//     attempt/review beads CLOSED (NOT deleted — spi-cjotlm) and stamped
//     with reset-cycle:<N> so the round counter and on-disk log filenames
//     stay monotonic across reset cycles
//     - `--to <step>` → step beads in the rewind set are REOPENED (not closed)
//     so the graph re-enters those steps on the next summon. Everything
//     outside the rewind set is left alone.
//
//     After cleanup, both soft and hard reset bump the parent bead's
//     reset-cycle:<N> label (default 1 → 2, etc.) so post-resummon work
//     groups under a fresh cycle on the board.
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
// # Label stripping
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
// # Step beads vs wave-apprentice beads
//
// The reopen-on-rewind logic in `--to` applies only to TOP-LEVEL step beads
// (the ones tracked in GraphState.StepBeadIDs). Subgraphs dispatch wave
// apprentices as separate beads with their own lifecycle — reset deletes
// their state files but does NOT reopen closed wave-apprentice beads; the
// epic re-dispatches them on resummon.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/executor"
	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/gatewayclient"
	spgit "github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/recovery"
	resetpkg "github.com/awell-health/spire/pkg/reset"
	"github.com/awell-health/spire/pkg/store"
	"github.com/spf13/cobra"
)

// ErrNoGraphState is the local alias for resetpkg.ErrNoGraphState. Kept as
// a package-level var so existing call sites (errors.Is(err, ErrNoGraphState))
// keep working without churn; the real sentinel lives in pkg/reset so
// gateway callers can map it to 409 via errors.Is without depending on
// cmd/spire.
var ErrNoGraphState = resetpkg.ErrNoGraphState

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

// storeReopenStepBeadFunc is a test-replaceable hook for reopening a closed
// step bead during rewind reconciliation. Production wiring delegates to
// store.ReopenStepBead, which transitions the bead to "open" — NOT
// "in_progress". The actually-active step picks up in_progress through the
// normal dispatch path; routing reset rewinds through ActivateStepBead would
// surface every rewound parent step bead as active simultaneously (spi-ogo3wv).
var storeReopenStepBeadFunc = storeReopenStepBead

// terminateBeadFunc is the seam reset uses to reap every runtime worker the
// backend spawned for a bead — the parent wizard plus any nested
// apprentice / sage / cleric workers AND any provider subprocess (claude,
// codex) descended from them. Default impl resolves the backend for the
// bead and dispatches to Backend.TerminateBead so reset gets PGID-scoped
// process-group reaping (spi-w65pr1) instead of the legacy per-PID
// signal that left detached children alive.
//
// Tests swap this var to record calls and skip real signalling — see
// reset_test.go's withFakeTerminateBead helper.
var terminateBeadFunc = func(ctx context.Context, beadID string) error {
	backend := resolveBackendForBead(beadID)
	if backend == nil {
		return nil
	}
	err := backend.TerminateBead(ctx, beadID)
	// ErrTerminateBeadNotImplemented is a no-op for backends that have
	// not yet wired bead-scoped termination (docker, k8s pre-spd-1lu5).
	// The local CLI runs against the process backend, so the production
	// reset path always exercises the real PGID reap.
	if errors.Is(err, agent.ErrTerminateBeadNotImplemented) {
		return nil
	}
	return err
}

// findLiveWizardForBeadFunc reports whether a wizard with a live OS process
// owns the given bead. Returns the registry entry when alive, nil otherwise.
//
// "Live" means: a registry entry exists for the bead AND its PID is non-zero
// AND the PID resolves to a running process. A registry entry alone is not
// sufficient — stale entries (process gone, registry not yet swept) read as
// not-live so reset's normalization path can reclaim abandoned active steps.
//
// Tests swap this var to inject live/dead wizards without touching the
// on-disk registry or spawning real processes.
var findLiveWizardForBeadFunc = func(beadID string) *localWizard {
	reg := loadWizardRegistry()
	wiz := findLiveWizardForBead(reg, beadID)
	if wiz == nil {
		return nil
	}
	if wiz.PID > 0 && processAlive(wiz.PID) {
		return wiz
	}
	return nil
}

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
	resetCmd.Flags().Bool("hard", false, "Hard reset (delete worktree and graph state; attempt/review beads are closed and tagged with reset-cycle, not deleted, so logs survive)")
	resetCmd.Flags().String("to", "", "Reset to a specific step")
	resetCmd.Flags().Bool("force", false, "With --to, bypass the 'target must have been reached' precondition; pending steps outside the rewind set are advanced to completed. Example: --to review --force --set implement.outputs.outcome=verified")
	resetCmd.Flags().StringArray("set", nil, "With --to, override a step's outputs (format: <step>.outputs.<key>=<value>; repeatable). Rejects '<step>.status=...'. Example: --set review.outputs.outcome=merge")

	// Wire pkg/reset.RunFunc so the gateway (and any other in-process
	// caller) can drive the soft-reset path through the same code that
	// `spire reset` runs from the CLI. The CLI uses runResetSoft directly;
	// the gateway dispatches via reset.ResetBead → RunFunc.
	resetpkg.RunFunc = runResetSoft
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

	setMap := resetSetArgsToMap(setArgs)

	// Gateway-mode dispatch: tunnel to POST /api/v1/beads/{id}/reset so
	// the same `spire reset` invocation works against an attached cluster
	// tower. Mirrors the v0.48 hardening pattern (spi-zz2ve9 / spi-i7k1ag.4)
	// — local-mode towers stay on the in-process path so on-disk worktrees
	// and graph state are reset alongside the bead.
	//
	// `--hard` routes to the dedicated POST /api/v1/beads/{id}/reset_hard
	// endpoint (spi-wrjiw6) so the manifest's one-verb-per-endpoint shape
	// holds for the destructive variant. The soft / `--to` paths continue
	// through the existing /reset endpoint.
	if t, terr := activeTowerConfigFunc(); terr == nil && t != nil && t.IsGateway() {
		if hard && toPhase == "" {
			return resetHardBeadViaGatewayFunc(context.Background(), beadID)
		}
		return gatewayResetBeadFunc(context.Background(), beadID, resetpkg.Opts{
			BeadID: beadID,
			To:     toPhase,
			Force:  force,
			Set:    setMap,
			Hard:   hard,
		})
	}

	_, err := runResetCore(context.Background(), resetpkg.Opts{
		BeadID: beadID,
		To:     toPhase,
		Force:  force,
		Set:    setMap,
	}, hard)
	return err
}

// gatewayResetBeadFunc is the gateway-mode dispatch seam. cmdReset calls
// it when the active tower is gateway-mode so the reset runs against the
// gateway endpoint instead of the local Dolt store. Tests swap this to
// verify routing without standing up a real gateway. Default impl uses
// store.NewGatewayClientForTower + gatewayclient.ResetBead so the wiring
// matches the close/summon gateway-mode dispatchers.
var gatewayResetBeadFunc = resetBeadViaGateway

// resetHardBeadViaGatewayFunc is the gateway-mode dispatch seam for the
// `--hard` variant. Routes to the dedicated /api/v1/beads/{id}/reset_hard
// endpoint (spi-wrjiw6) so the manifest's one-verb-per-endpoint shape
// holds for the destructive case. Tests swap this to verify routing
// without standing up a real gateway.
var resetHardBeadViaGatewayFunc = resetHardBeadViaGateway

// resetHardBeadViaGateway tunnels a `reset --hard` call through the
// active tower's gatewayclient. Renders the post-reset bead's status to
// stdout to match the local-mode CLI output shape so a gateway-mode
// invocation looks identical to a local one at the terminal.
func resetHardBeadViaGateway(ctx context.Context, id string) error {
	t, err := activeTowerConfigFunc()
	if err != nil {
		return fmt.Errorf("reset %s: resolve tower: %w", id, err)
	}
	if t == nil {
		return fmt.Errorf("reset %s: no active tower", id)
	}
	c, err := store.NewGatewayClientForTower(t)
	if err != nil {
		return fmt.Errorf("reset %s: %w", id, err)
	}
	bead, err := c.ResetHardBead(ctx, id)
	if err != nil {
		return err
	}
	fmt.Printf("%s reset --hard (gateway: status=%s)\n", id, bead.Status)
	return nil
}

// resetBeadViaGateway tunnels a reset call through the active tower's
// gatewayclient. Renders the post-reset bead's status to stdout to match
// the local-mode CLI output shape ("<bead> reset (v3)" / soft-reset
// preamble) so a gateway-mode invocation looks identical to a local one
// at the terminal.
func resetBeadViaGateway(ctx context.Context, id string, opts resetpkg.Opts) error {
	t, err := activeTowerConfigFunc()
	if err != nil {
		return fmt.Errorf("reset %s: resolve tower: %w", id, err)
	}
	if t == nil {
		return fmt.Errorf("reset %s: no active tower", id)
	}
	c, err := store.NewGatewayClientForTower(t)
	if err != nil {
		return fmt.Errorf("reset %s: %w", id, err)
	}
	bead, err := c.ResetBead(ctx, id, gatewayclient.ResetBeadOpts{
		To:    opts.To,
		Force: opts.Force,
		Set:   opts.Set,
		Hard:  opts.Hard,
	})
	if err != nil {
		return err
	}
	if opts.To != "" {
		fmt.Printf("%s soft-reset to step %q (gateway: status=%s)\n", id, opts.To, bead.Status)
	} else {
		fmt.Printf("%s reset (gateway: status=%s)\n", id, bead.Status)
	}
	return nil
}

// runResetSoft is the in-process entry point for both the soft and hard
// reset paths. It is wired to resetpkg.RunFunc in init() so the gateway
// and any other pkg/reset.ResetBead caller share the exact same kill-
// wizard → strip-labels → unhook → walk-back (or destructive worktree
// teardown) sequence the CLI runs. The historical name "soft" is kept
// for compatibility with existing call sites; opts.Hard switches the
// internal dispatch to the destructive path.
func runResetSoft(ctx context.Context, opts resetpkg.Opts) (*store.Bead, error) {
	return runResetCore(ctx, opts, opts.Hard)
}

// lookupWizardForBead returns a copy of the first registry entry whose
// BeadID matches the given bead, or nil. Reset paths use this to capture
// wizardName and worktreePath BEFORE TerminateBead clears the registry.
// Returning a copy (not a pointer into reg.Wizards) keeps callers safe
// against later registry mutations.
func lookupWizardForBead(beadID string) *localWizard {
	reg := loadWizardRegistry()
	for i := range reg.Wizards {
		if reg.Wizards[i].BeadID == beadID {
			w := reg.Wizards[i]
			return &w
		}
	}
	return nil
}

// runResetCore is the shared prelude + dispatch used by both the CLI's
// `spire reset` (with or without `--hard`) and the gateway's POST /reset
// endpoint. It owns the registry-PID lookup, the bead-scoped TerminateBead
// reap, label strip, step-child unhook, and the dispatch into softResetV3
// (when opts.To is set) or resetV3 (otherwise).
//
// When hard is true, opts.To/Force/Set are ignored — the hard path always
// goes through resetV3(beadID, true, ...).
//
// Termination goes through Backend.TerminateBead (spi-w65pr1) instead of
// signalling the registered parent PID. The backend reaps every entry
// registered for the bead — parent wizard, nested apprentice / sage /
// cleric workers, and any provider subprocess (claude, codex) descended
// from them — by signalling -PGID. Detached children that reparent to
// PID 1 retain their original PGID, so the kill still reaches them.
// Errors from TerminateBead surface as a fail-closed reset error so the
// operator gets the manual-cleanup PID list instead of a silent half-reset.
//
// Returns the post-reset bead so callers (gateway, future tooling) can
// re-render without a follow-up GET.
func runResetCore(ctx context.Context, opts resetpkg.Opts, hard bool) (*store.Bead, error) {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	beadID := opts.BeadID
	if beadID == "" {
		return nil, fmt.Errorf("reset: bead ID required")
	}

	// --- 1. Reap every bead-scoped runtime worker (spi-w65pr1) ---
	//
	// Capture wizardName/worktreePath BEFORE TerminateBead because the
	// terminate primitive clears the registry entries on success and the
	// downstream graph cleanup still needs both fields.
	wizard := lookupWizardForBead(beadID)
	var wizardName string
	var worktreePath string
	if wizard != nil {
		wizardName = wizard.Name
		worktreePath = wizard.Worktree
	} else {
		wizardName = "wizard-" + beadID
	}

	if err := terminateBeadFunc(ctx, beadID); err != nil {
		return nil, fmt.Errorf("reset %s: terminate bead-scoped processes: %w", beadID, err)
	}
	if wizard != nil && wizard.PID > 0 {
		fmt.Printf("  %s↓ %s reaped (pid %d, pgid %d)%s\n", dim, wizardName, wizard.PID, wizard.PGID, reset)
	}

	// --- Resolve formula to determine enabled phases and default --to ---

	bead, err := storeGetBead(beadID)
	if err != nil {
		return nil, fmt.Errorf("get bead %s: %w", beadID, err)
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
	if !hard && opts.To != "" {
		if err := softResetV3(beadID, opts.To, wizardName, opts.Force, resetMapToSetArgs(opts.Set)); err != nil {
			return nil, err
		}
	} else {
		if err := resetV3(beadID, hard, wizardName, worktreePath); err != nil {
			return nil, err
		}
	}

	// Re-fetch the post-reset bead so callers (the gateway, in particular)
	// can return the updated state without a follow-up GET. The CLI ignores
	// the return value; printing already happened inside softResetV3 / resetV3.
	post, gerr := storeGetBead(beadID)
	if gerr != nil {
		return nil, fmt.Errorf("get bead %s after reset: %w", beadID, gerr)
	}
	return &post, nil
}

// resetSetArgsToMap converts the CLI's repeated `--set <step>.outputs.<key>=<value>`
// tokens into the map[string]string shape Opts.Set expects.
//
// Each token is split at the FIRST '=' so values may legally contain '='
// (matching parseSetFlag's behaviour). Tokens with no '=' map the whole
// token to "" — parseSetFlag rejects them downstream with a clear error.
func resetSetArgsToMap(setArgs []string) map[string]string {
	if len(setArgs) == 0 {
		return nil
	}
	out := make(map[string]string, len(setArgs))
	for _, s := range setArgs {
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			out[s] = ""
			continue
		}
		out[s[:eq]] = s[eq+1:]
	}
	return out
}

// resetMapToSetArgs reverses resetSetArgsToMap so runResetCore can hand
// Opts.Set back to softResetV3 in the []string shape it already expects.
func resetMapToSetArgs(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	// Sort so test output is deterministic; parseSetFlag is order-invariant.
	sort.Strings(out)
	return out
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

	// --- 1. Reap every bead-scoped runtime worker (spi-w65pr1) ---
	//
	// Capture wizardName/worktreePath BEFORE TerminateBead because the
	// terminate primitive clears the registry on success and the
	// downstream worktree/branch cleanup still needs both fields.
	var wizardName string
	var worktreePath string
	if wiz := lookupWizardForBead(beadID); wiz != nil {
		wizardName = wiz.Name
		worktreePath = wiz.Worktree
	}
	if wizardName == "" {
		wizardName = "wizard-" + beadID
	}

	if err := terminateBeadFunc(context.Background(), beadID); err != nil {
		return fmt.Errorf("hard reset %s: terminate bead-scoped processes: %w", beadID, err)
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

	// Read the parent's CURRENT cycle so we stamp closing children with the
	// cycle they belong to (the cycle that just ended), then bump the parent
	// for the next batch of work after resummon.
	currentCycle := readParentResetCycle(beadID)
	counts := deleteInternalDAGBeadsRecursive(processable, currentCycle)
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

	// --- 7. Bump parent reset-cycle so the next attempt/review batch lands
	// in a new cycle group on the board.
	bumpParentResetCycle(beadID)

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

		// Read the parent's CURRENT cycle so we stamp closing children with
		// the cycle they belong to (the cycle that just ended), then bump
		// the parent for the next batch of work after resummon.
		currentCycle := readParentResetCycle(beadID)
		counts := cleanupInternalDAGChildren(processable, false, currentCycle)
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

		// 5. Bump parent reset-cycle so post-resummon work groups under a
		// fresh cycle on the board (parity with the hard-reset path).
		bumpParentResetCycle(beadID)
	}

	// --- Close related recovery + alert beads (both soft and hard paths) ---
	// Alerts are included in the cascade so stale alerts (merge-failure,
	// dispatch-failure, ...) don't linger on the board referencing a
	// deleted worktree after reset. readParentResetCycle returns the
	// cycle that was already in effect on this bead (pre-bump), so the
	// stamp points at the cycle the alerts *belonged* to — the one that
	// just ended. See spi-pwdhs5 Bug B.
	cascadeReason := fmt.Sprintf("reset-cycle:%d", readParentResetCycle(beadID))
	if err := recovery.CloseRelatedDependents(storeBridgeOps{}, beadID, []string{recovery.KindRecovery, recovery.KindAlert}, []string{"caused-by", "recovery-for"}, cascadeReason); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not close dependents: %v\n", err)
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
			return nil, fmt.Errorf("%w: --set %q: expected <step>.outputs.<key>=<value>", resetpkg.ErrSetSyntax, tok)
		}
		lhs, value := tok[:eq], tok[eq+1:]
		segs := strings.Split(lhs, ".")
		if len(segs) != 3 {
			return nil, fmt.Errorf("%w: --set %q: expected <step>.outputs.<key>, got %d path segment(s)", resetpkg.ErrSetSyntax, tok, len(segs))
		}
		step, kind, key := segs[0], segs[1], segs[2]
		if kind == "status" {
			return nil, fmt.Errorf("%w: --set %q: setting status is not allowed — --set is scoped to outputs", resetpkg.ErrSetSyntax, tok)
		}
		if kind != "outputs" {
			return nil, fmt.Errorf("%w: --set %q: only <step>.outputs.<key> is supported (got middle segment %q)", resetpkg.ErrSetSyntax, tok, kind)
		}
		if step == "" || key == "" {
			return nil, fmt.Errorf("%w: --set %q: step and key must be non-empty", resetpkg.ErrSetSyntax, tok)
		}
		if _, ok := graph.Steps[step]; !ok {
			var valid []string
			for name := range graph.Steps {
				valid = append(valid, name)
			}
			sort.Strings(valid)
			return nil, fmt.Errorf("%w: --set %q: step %q not found in formula %s (valid steps: %s)",
				resetpkg.ErrSetSyntax, tok, step, graph.Name, strings.Join(valid, ", "))
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
		sort.Strings(validSteps)
		return fmt.Errorf("%w: step %q not found in formula %s (valid steps: %s)",
			resetpkg.ErrInvalidStep, targetStep, graph.Name, strings.Join(validSteps, ", "))
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
			return fmt.Errorf("%w: cannot rewind %s to %q: step has not been reached yet (pass --force to advance anyway)", resetpkg.ErrTargetNotReached, beadID, targetStep)
		}
	}

	// --- 4b. Pre-flight: identify active predecessor steps and gate on wizard liveness ---
	// `--force` normalizes pre-target steps so the graph can route forward
	// from the target on resume. Without this pass, an `active` predecessor
	// (e.g. an abandoned implement step) would be skipped by the pending-only
	// force-advance below, leaving the graph self-contradictory: the target
	// is runnable but `active_step` still points at the abandoned predecessor,
	// so resummon resumes from the predecessor instead of the target.
	//
	// Liveness gate: if a wizard with a live OS process still owns this bead,
	// fail closed BEFORE any mutation — `--force` is the operator escape hatch
	// for orphaned work, not a hammer for stomping on a running wizard. The
	// CLI reset path kills the wizard process before reaching softResetV3, so
	// in production this guard mostly catches direct callers (gateway, tests)
	// where the kill step was bypassed.
	var activePredecessors []string
	if forceAdvance {
		predecessors := computeStepPredecessors(graph, targetStep)
		for stepName := range predecessors {
			if baseRewind[stepName] {
				continue
			}
			if ss, ok := gs.Steps[stepName]; ok && ss.Status == "active" {
				activePredecessors = append(activePredecessors, stepName)
			}
		}
		if len(activePredecessors) > 0 {
			if wiz := findLiveWizardForBeadFunc(beadID); wiz != nil {
				sort.Strings(activePredecessors)
				return fmt.Errorf("%w: cannot --force past active step(s) %s: live wizard %s (pid %d) still owns %s; release it or kill the wizard first",
					resetpkg.ErrConflict, strings.Join(activePredecessors, ", "), wiz.Name, wiz.PID, beadID)
			}
		}
	}

	// --- 4c. Expand rewind set to include stale-completed downstream-of-override steps ---
	stepsToReset := expandRewindSetForOverrides(baseRewind, graph, gs, overrides)

	fmt.Printf("  %sresetting steps: %s%s\n", dim, strings.Join(mapKeys(stepsToReset), ", "), reset)

	// Under --force, reset may skip an implementation step and resume at a
	// later step that expects the run-scoped workspace to already exist. Rebind
	// any canonical on-disk worktrees before mutating graph state so a corrupt
	// workspace fails closed without a partial reset.
	if forceAdvance {
		rebound, err := executor.RebindRunScopedWorkspacesFromDisk(gs, graph)
		if err != nil {
			return fmt.Errorf("rebind run-scoped workspaces: %w", err)
		}
		for _, name := range rebound {
			fmt.Printf("  %srebound run-scoped workspace %q to existing worktree%s\n", yellow, name, reset)
		}
	}

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
	//
	// Active predecessors of the target (collected in step 4b) are also
	// normalized to completed with audit metadata. This is the orphan-recovery
	// path: implementation crashed mid-flight, no live wizard remains, and the
	// operator wants reset to clear the abandoned predecessor so resummon
	// resumes at the target. The liveness gate in 4b ensures we only reach
	// this path when no wizard is actively working the bead.
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

		normalized := 0
		nowStamp := time.Now().UTC().Format(time.RFC3339)
		for _, stepName := range activePredecessors {
			ss := gs.Steps[stepName]
			ss.Status = "completed"
			if ss.Outputs == nil {
				ss.Outputs = map[string]string{}
			}
			if ss.CompletedAt == "" {
				ss.CompletedAt = nowStamp
			}
			gs.Steps[stepName] = ss
			normalized++

			// Audit trail: comment on the step bead, then close it to match
			// the new graph state. A step bead left in_progress after the
			// graph says completed would deadlock the next summon (the
			// transition path treats already-active step beads as still
			// owned). The audit comment fires before close so the close
			// hook itself never strips it.
			if stepBeadID := gs.StepBeadIDs[stepName]; stepBeadID != "" {
				_ = storeAddComment(stepBeadID,
					fmt.Sprintf("force-normalized from active during reset --to %s (no live wizard)", targetStep))
				if err := storeCloseStepBead(stepBeadID); err != nil {
					fmt.Printf("  %s(note: could not close step bead %s for %s: %s)%s\n", dim, stepBeadID, stepName, err, reset)
				}
			}
		}
		if normalized > 0 {
			fmt.Printf("  %s↟ force-normalized %d abandoned active predecessor(s) to completed%s\n", yellow, normalized, reset)
		}

		// Clear active_step if it pointed at one of the normalized
		// predecessors. Without this, the graph stays internally pointed at
		// an abandoned step and resummon resumes from there instead of the
		// target.
		for _, stepName := range activePredecessors {
			if gs.ActiveStep == stepName {
				gs.ActiveStep = ""
				fmt.Printf("  %s✗ cleared active_step (pointed at normalized predecessor %s)%s\n", dim, stepName, reset)
				break
			}
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
			if err := storeReopenStepBeadFunc(stepBeadID); err != nil {
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

	// --- 8b. Close related recovery + alert beads ---
	cascadeReason := fmt.Sprintf("reset-cycle:%d", readParentResetCycle(beadID))
	if err := recovery.CloseRelatedDependents(storeBridgeOps{}, beadID, []string{recovery.KindRecovery, recovery.KindAlert}, []string{"caused-by", "recovery-for"}, cascadeReason); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not close dependents: %v\n", err)
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

// computeStepPredecessors builds the set of steps that are transitive
// predecessors of targetStep (backward reachability via the Needs edges).
// The target step itself is NOT included.
//
// Used by softResetV3's --force normalization to scope the abandoned-active
// step rule to predecessors only — non-predecessor active steps (parallel
// branches that happen to be running, post-target activity) are left alone.
func computeStepPredecessors(graph *formula.FormulaStepGraph, targetStep string) map[string]bool {
	result := map[string]bool{}
	queue := []string{targetStep}
	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]
		step, ok := graph.Steps[curr]
		if !ok {
			continue
		}
		for _, need := range step.Needs {
			if !result[need] {
				result[need] = true
				queue = append(queue, need)
			}
		}
	}
	return result
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

// hasResetCycleLabel reports whether the bead already carries any
// reset-cycle:<N> label. Used to make stamping idempotent so repeated resets
// don't pile on duplicate labels.
func hasResetCycleLabel(b Bead) bool {
	for _, l := range b.Labels {
		if strings.HasPrefix(l, "reset-cycle:") {
			return true
		}
	}
	return false
}

// readParentResetCycle returns the parent bead's current reset cycle,
// defaulting to 1 when the bead is missing or carries no reset-cycle:<N>
// label. This is the cycle that already-existing children belong to and
// that any in-flight stamping should use.
func readParentResetCycle(parentID string) int {
	parent, err := storeGetBead(parentID)
	if err != nil {
		return 1
	}
	return store.ResetCycleNumber(parent)
}

// bumpParentResetCycle replaces the parent's reset-cycle:<N> label with
// reset-cycle:<N+1>. New attempt/review children created after the next
// summon will inherit the bumped cycle (via store.ParentResetCycle), so
// the board can group them under their own cycle header.
//
// Returns the new cycle, or the prior cycle on failure (best-effort).
func bumpParentResetCycle(parentID string) int {
	current := readParentResetCycle(parentID)
	if err := storeRemoveLabel(parentID, fmt.Sprintf("reset-cycle:%d", current)); err != nil {
		// Tolerated: the label may not have existed yet (pre-feature beads
		// default to cycle 1 with no label). The AddLabel below still
		// installs the new cycle.
	}
	next := current + 1
	if err := storeAddLabel(parentID, fmt.Sprintf("reset-cycle:%d", next)); err != nil {
		fmt.Printf("  %s(note: could not bump reset-cycle on %s: %s)%s\n", dim, parentID, err, reset)
		return current
	}
	fmt.Printf("  %s↻ %s reset-cycle → %d%s\n", yellow, parentID, next, reset)
	return next
}

// stampResetCycleIfMissing adds reset-cycle:<cycle> to the child if it does
// not already carry such a label. cycle <= 0 disables stamping.
func stampResetCycleIfMissing(child Bead, cycle int) {
	if cycle <= 0 || hasResetCycleLabel(child) {
		return
	}
	if err := storeAddLabelFunc(child.ID, fmt.Sprintf("reset-cycle:%d", cycle)); err != nil {
		fmt.Printf("  %s(note: could not stamp reset-cycle on %s: %s)%s\n", dim, child.ID, err, reset)
	}
}

// cleanupInternalDAGChildren disposes of attempt/review/step children during
// soft and hard reset.
//
// Attempt and review-round children are always CLOSED (never deleted) so the
// audit trail — including the round:<N> label that drives the monotonic
// counter and the on-disk wizard log filename — survives across reset cycles.
// hard mode used to delete these; that lost history, including the very logs
// the board was supposed to surface.
//
// Step children are still deleted on hard reset (steps are regenerated by
// resummon and have no historical content worth preserving) and closed on
// soft reset.
//
// cycle is the reset cycle to stamp on attempt/review children that lack a
// reset-cycle:<N> label (idempotent — already-stamped children are skipped).
// cycle <= 0 disables stamping.
func cleanupInternalDAGChildren(children []Bead, hard bool, cycle int) internalDAGCleanupCounts {
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

		// Hard reset still deletes step beads — they're regenerated on
		// resummon and carry no audit history beyond the workflow-step label.
		if hard && kind == "step" {
			if err := storeDeleteBeadFunc(child.ID); err != nil {
				fmt.Printf("  %s(note: could not delete %s: %s)%s\n", dim, child.ID, err, reset)
				continue
			}
			counts.DeletedSteps++
			continue
		}

		// Attempt/review beads are preserved (closed, not deleted) so their
		// labels and the matching on-disk log files keep working across
		// reset cycles. Stamp the current cycle before closing so the board
		// can later group the round under its cycle.
		if kind == "attempt" || kind == "review" {
			stampResetCycleIfMissing(child, cycle)
		}

		if child.Status == "closed" {
			continue
		}
		// NOTE: EndWork is NOT appropriate here. EndWork operates on the parent
		// task bead's attempt bead (the one created by BeginWork). These child
		// beads are internal DAG sub-beads (step attempts, review rounds) nested
		// UNDER the parent task's attempt — closing them directly with
		// storeCloseBeadFunc is correct. The parent task bead's attempt is
		// handled separately by the reset flow.
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
//   - Recovery beads (linked via recovery-for or caused-by deps, or with
//     recovery-bead label)
//
// Alert beads are intentionally NOT protected — they flow through the reset
// cascade via CloseRelatedDependents with kinds=[recovery, alert] and are
// stamped reset-cycle:<N> for audit. Prior behavior kept them alive across
// reset, which left stale alerts (merge-failure, dispatch-failure) referencing
// deleted worktrees on the board after a reset. See spi-pwdhs5 Bug B.
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

	// Recovery beads ONLY (not alerts): linked as dependents via recovery-for
	// or caused-by AND carrying the recovery-bead label. Alert beads on the
	// same edge types are NOT protected — the reset cascade closes them.
	if dependents, err := storeGetDependentsWithMeta(beadID); err == nil {
		for _, dep := range dependents {
			if dep.DependencyType != "recovery-for" && dep.DependencyType != "caused-by" {
				continue
			}
			// Narrowed: only recovery-labeled beads are protected.
			isRecovery := false
			for _, l := range dep.Labels {
				if l == "recovery-bead" {
					isRecovery = true
					break
				}
			}
			if isRecovery {
				protectedIDs[dep.ID] = true
			}
		}
	}

	// Belt-and-suspenders: protect children with recovery-bead labels
	// even if they weren't found via dependency edges. Alert-labeled
	// children are NOT protected (fall through to the reset cascade).
	for _, child := range children {
		if isProtectedByLabel(child) {
			protectedIDs[child.ID] = true
		}
	}

	return protectedIDs
}

// isProtectedByLabel returns true if a bead should be protected from reset
// based on its labels. Narrowed to strict recovery-bead labels — alert:*
// labels are no longer protected (see spi-pwdhs5 Bug B).
func isProtectedByLabel(b Bead) bool {
	for _, l := range b.Labels {
		if l == "recovery-bead" {
			return true
		}
	}
	return false
}

// deleteInternalDAGBeadsRecursive disposes of internal DAG beads and their
// descendants during hard reset.
//
// Step beads are deleted (along with their descendants — bottom-up to avoid
// orphans). Attempt and review-round beads are CLOSED (not deleted) and
// stamped with reset-cycle:<cycle> so the on-disk wizard log filename and
// the round:<N> label survive across the reset boundary. The board then
// surfaces them grouped by cycle and the next sage-review picks up the
// monotonic counter from max(round)+1 instead of restarting at 1.
//
// cycle is the reset cycle to stamp on attempt/review children that lack a
// reset-cycle:<N> label. cycle <= 0 disables stamping.
func deleteInternalDAGBeadsRecursive(children []Bead, cycle int) internalDAGCleanupCounts {
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

		// Attempt/review beads are preserved across hard reset for log
		// continuity. Close them in place (idempotent for already-closed
		// children) and stamp the cycle.
		//
		// NOTE: EndWork is NOT appropriate here. EndWork operates on the parent
		// task bead's attempt bead (the one created by BeginWork). These children
		// are internal DAG sub-beads (step attempts, review rounds) nested UNDER
		// the parent task's attempt — closing them directly with storeCloseBeadFunc
		// is correct. The parent task bead's attempt is handled separately.
		if kind == "attempt" || kind == "review" {
			stampResetCycleIfMissing(child, cycle)
			if child.Status == "closed" {
				continue
			}
			if err := storeCloseBeadFunc(child.ID); err != nil {
				fmt.Printf("  %s(note: could not close %s: %s)%s\n", dim, child.ID, err, reset)
				continue
			}
			switch kind {
			case "review":
				counts.ClosedReviewRounds++
			case "attempt":
				counts.ClosedAttempts++
			}
			continue
		}

		// Step beads still get the historical bottom-up delete behavior.
		deleteBeadDescendants(child.ID)

		if err := storeDeleteBeadFunc(child.ID); err != nil {
			fmt.Printf("  %s(note: could not delete %s: %s)%s\n", dim, child.ID, err, reset)
			continue
		}
		counts.DeletedSteps++
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
//
// In addition to the primary worktree and the `.worktrees/<bead>*` set, this
// also sweeps `$TMPDIR/spire-review/<name>/<bead>*` and
// `$TMPDIR/spire-wizard/<name>/<bead>*` (see spi-pwdhs5 Bug B). A sage or
// wizard that died mid-run can leave a worktree under one of those paths
// that holds `feat/<bead>` open; next summon + merge would collide. Globs
// are per-bead (not `*` wholesale) so concurrent wizards on other beads are
// never disturbed. The cwgiy9 in-wizard recovery design guarantees there is
// no parallel cleric pod whose worktree could be wrongly wiped — if a
// future cleric-pod reintroduction breaks that invariant, this sweep must
// be tightened to check ownership before removal.
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

	// Sweep $TMPDIR/spire-review/<name>/<bead>* and
	// $TMPDIR/spire-wizard/<name>/<bead>* for stale sage/wizard worktrees
	// left holding feat/<bead> open. Per-bead scoping only — each glob
	// anchors on the specific bead ID, never a wildcard.
	tmpRoots := []string{
		filepath.Join(os.TempDir(), "spire-review"),
		filepath.Join(os.TempDir(), "spire-wizard"),
	}
	for _, root := range tmpRoots {
		// Glob match: <root>/<anyname>/<bead>*. Each <anyname> subdir belongs
		// to a sage or wizard identity; we match only per-bead children.
		tmpMatches, _ := filepath.Glob(filepath.Join(root, "*", beadID+"*"))
		for _, m := range tmpMatches {
			// Extra safety: ensure the last path segment looks like a
			// same-bead worktree. filepath.Glob already ensures this by
			// virtue of the literal beadID+"*" suffix, but the base check
			// guards against accidental shell-glob escapes.
			base := filepath.Base(m)
			if base != beadID && !strings.HasPrefix(base, beadID+"-") && !strings.HasPrefix(base, beadID+".") {
				continue
			}
			if err := os.RemoveAll(m); err == nil {
				fmt.Printf("  %s✗ temp worktree removed: %s%s\n", dim, m, reset)
			} else if !os.IsNotExist(err) {
				fmt.Printf("  %s(note: could not remove temp worktree %s: %s)%s\n", dim, m, err, reset)
			}
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
