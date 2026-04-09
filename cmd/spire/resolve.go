package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/executor"
	"github.com/awell-health/spire/pkg/store"
	"github.com/spf13/cobra"
	"github.com/steveyegge/beads"
)

// Test-replaceable vars (follow claim.go pattern).
var resolveGetBeadFunc = storeGetBead
var resolveGetDependentsFunc = storeGetDependentsWithMeta
var resolveCloseBeadFunc = storeCloseBead

var resolveCmd = &cobra.Command{
	Use:   "resolve <bead-id> <comment>",
	Short: "Record recovery learning and unblock a stuck bead",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		closeFlag, _ := cmd.Flags().GetBool("close")
		return cmdResolve(args[0], args[1], closeFlag)
	},
}

func init() {
	resolveCmd.Flags().Bool("close", false, "Close the source bead instead of re-summoning")
}

func cmdResolve(beadID, comment string, closeSource bool) error {
	return resolveSourceBead(beadID, comment, closeSource)
}

// resolveSourceBead records recovery learnings and unblocks a stuck source bead.
// Exported as an unexported function so the board can call it directly.
func resolveSourceBead(beadID, comment string, closeSource bool) error {
	// Step 1: Setup
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	bead, err := resolveGetBeadFunc(beadID)
	if err != nil {
		return fmt.Errorf("get bead %s: %w", beadID, err)
	}
	if !containsLabel(bead, "needs-human") {
		return fmt.Errorf("%s does not have needs-human label — nothing to resolve", beadID)
	}

	// Step 2: Find recovery beads
	dependents, err := resolveGetDependentsFunc(beadID)
	if err != nil {
		return fmt.Errorf("get dependents of %s: %w", beadID, err)
	}

	var recoveryBeadIDs []string
	for _, dep := range dependents {
		if dep.DependencyType != "caused-by" {
			continue
		}
		if dep.Status == beads.StatusClosed {
			continue
		}
		hasRecoveryLabel := false
		for _, l := range dep.Labels {
			if l == "recovery-bead" {
				hasRecoveryLabel = true
				break
			}
		}
		if !hasRecoveryLabel {
			continue
		}
		recoveryBeadIDs = append(recoveryBeadIDs, dep.ID)
	}

	// Step 3: Write recovery learnings
	for _, recoveryID := range recoveryBeadIDs {
		recoveryBead, err := resolveGetBeadFunc(recoveryID)
		if err != nil {
			fmt.Printf("  %s(note: could not load recovery bead %s: %s)%s\n", dim, recoveryID, err, reset)
			continue
		}

		learningID := generateResolveLearningID()
		row := store.RecoveryLearningRow{
			ID:              learningID,
			RecoveryBead:    recoveryBead.ID,
			SourceBead:      beadID,
			FailureClass:    recoveryBead.Meta("failure_class"),
			FailureSig:      recoveryBead.Meta("failure_signature"),
			ResolutionKind:  "human_resolve",
			Outcome:         "clean",
			LearningSummary: comment,
			Reusable:        true,
			ResolvedAt:      time.Now(),
			ExpectedOutcome: recoveryBead.Meta("expected_outcome"),
		}
		if err := store.WriteRecoveryLearningAuto(row); err != nil {
			fmt.Printf("  %s(note: could not write learning for %s: %s)%s\n", dim, recoveryID, err, reset)
		} else {
			fmt.Printf("  %s✓ recovery learning recorded (%s)%s\n", green, learningID, reset)
		}

		if err := storeAddComment(recoveryBead.ID, "Resolved by human: "+comment); err != nil {
			fmt.Printf("  %s(note: could not add comment to %s: %s)%s\n", dim, recoveryID, err, reset)
		}

		if err := resolveCloseBeadFunc(recoveryBead.ID); err != nil {
			fmt.Printf("  %s(note: could not close recovery bead %s: %s)%s\n", dim, recoveryID, err, reset)
		} else {
			fmt.Printf("  %s✓ closed recovery bead %s%s\n", green, recoveryID, reset)
		}
	}

	// Step 4: Unblock source bead
	if err := storeRemoveLabel(beadID, "needs-human"); err != nil {
		fmt.Printf("  %s(note: could not remove needs-human from %s: %s)%s\n", dim, beadID, err, reset)
	}
	for _, l := range bead.Labels {
		if strings.HasPrefix(l, "interrupted:") {
			if err := storeRemoveLabel(beadID, l); err != nil {
				fmt.Printf("  %s(note: could not remove %s from %s: %s)%s\n", dim, l, beadID, err, reset)
			}
		}
	}

	// Step 5: Handle --close vs default
	if closeSource {
		if err := resolveCloseBeadFunc(beadID); err != nil {
			return fmt.Errorf("close source bead %s: %w", beadID, err)
		}
		resolveCleanWorktrees(beadID)
		fmt.Printf("  %s✓ Source bead closed. No re-summon needed.%s\n", green, reset)
	} else {
		resolveResetHookedSteps(beadID)
	}

	// Step 6: Summary
	fmt.Println()
	fmt.Printf("%sResolved %s%s\n", bold, beadID, reset)
	if len(recoveryBeadIDs) > 0 {
		fmt.Printf("  recovery beads closed: %s\n", strings.Join(recoveryBeadIDs, ", "))
	} else {
		fmt.Printf("  %s(no open recovery beads found)%s\n", dim, reset)
	}
	fmt.Printf("  learning recorded: %q\n", comment)
	if closeSource {
		fmt.Printf("  source bead: closed\n")
	} else {
		fmt.Printf("  source bead: unblocked\n")
	}

	return nil
}

// resolveCleanWorktrees removes worktree directories and branches for a bead.
func resolveCleanWorktrees(beadID string) {
	// Remove temp worktree paths
	wtPath := filepath.Join(os.TempDir(), "spire-wizard", "*", beadID)
	matches, _ := filepath.Glob(wtPath)
	for _, m := range matches {
		if err := os.RemoveAll(m); err == nil {
			fmt.Printf("  %s✗ worktree removed: %s%s\n", dim, m, reset)
		}
	}

	// Remove in-repo worktree
	cwd, _ := os.Getwd()
	inRepoWt := filepath.Join(cwd, ".worktrees", beadID)
	if err := os.RemoveAll(inRepoWt); err == nil {
		fmt.Printf("  %s✗ in-repo worktree removed: %s%s\n", dim, inRepoWt, reset)
	}

	// Remove subtask worktrees
	wtMatches, _ := filepath.Glob(filepath.Join(cwd, ".worktrees", beadID+"-*"))
	for _, m := range wtMatches {
		if err := os.RemoveAll(m); err == nil {
			fmt.Printf("  %s✗ subtask worktree removed: %s%s\n", dim, filepath.Base(m), reset)
		}
	}
}

// resolveResetHookedSteps finds the graph state for a bead and resets any hooked steps to pending.
func resolveResetHookedSteps(beadID string) {
	dir, err := config.Dir()
	if err != nil {
		fmt.Printf("  %s⚠ could not find config dir: %s%s\n", yellow, err, reset)
		fmt.Printf("  %sSource bead unblocked. Manual re-summon may be needed.%s\n", yellow, reset)
		return
	}

	runtimeGlob := filepath.Join(dir, "runtime", "*", "graph_state.json")
	stateFiles, err := filepath.Glob(runtimeGlob)
	if err != nil || len(stateFiles) == 0 {
		fmt.Printf("  %s⚠ no graph state found for %s — manual re-summon may be needed%s\n", yellow, beadID, reset)
		fmt.Printf("  %s✓ Source bead unblocked. Steward will re-summon wizard.%s\n", green, reset)
		return
	}

	found := false
	for _, sf := range stateFiles {
		data, err := os.ReadFile(sf)
		if err != nil {
			continue
		}
		var gs executor.GraphState
		if err := json.Unmarshal(data, &gs); err != nil {
			continue
		}
		if gs.BeadID != beadID {
			continue
		}
		found = true

		// Reset hooked steps to pending
		modified := false
		for name, step := range gs.Steps {
			if step.Status == "hooked" {
				step.Status = "pending"
				step.CompletedAt = ""
				step.Outputs = nil
				step.StartedAt = ""
				gs.Steps[name] = step
				modified = true
				fmt.Printf("  %s✓ reset step %q from hooked → pending%s\n", green, name, reset)
			}
		}

		if modified {
			// Extract agent name from path: .../runtime/<agent-name>/graph_state.json
			agentName := filepath.Base(filepath.Dir(sf))
			if err := gs.Save(agentName, config.Dir); err != nil {
				fmt.Printf("  %s(note: could not save graph state: %s)%s\n", dim, err, reset)
			}
		}

		fmt.Printf("  %s✓ Source bead unblocked. Steward will re-summon wizard.%s\n", green, reset)
		break
	}

	if !found {
		fmt.Printf("  %s⚠ no graph state found for %s — manual re-summon may be needed%s\n", yellow, beadID, reset)
		fmt.Printf("  %s✓ Source bead unblocked. Steward will re-summon wizard.%s\n", green, reset)
	}
}

func generateResolveLearningID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("rl-%d", time.Now().UnixNano())
	}
	return "rl-" + hex.EncodeToString(b)
}
