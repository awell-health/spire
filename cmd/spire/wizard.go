// wizard.go defines the `spire wizard` cobra parent and its role-scoped
// subcommands (claim, seal). Wizard agents orchestrate work on a task bead:
// they create attempt beads, dispatch apprentices, coordinate reviews, and
// seal the work when a merge lands.
package main

import (
	"fmt"
	"os"
	"time"

	spgit "github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/store"
	"github.com/spf13/cobra"
)

var wizardCmd = &cobra.Command{
	Use:   "wizard",
	Short: "Wizard-side commands (claim, seal, ...)",
	Long: `Wizard-side commands.

A wizard is the per-bead orchestrator that drives the formula lifecycle:
it creates an attempt bead, dispatches apprentices, consults sages, and
seals the work once a merge commit is in hand.

"spire wizard claim <bead>" atomically creates an attempt bead and marks
the task bead in_progress. "spire wizard seal <bead>" writes the final
merge commit + seal timestamp and closes the open attempt.`,
}

// --- Test-replaceable seams ---
//
// These function variables let tests swap the store/git calls made by the
// wizard subcommands without touching cmd/spire/store_bridge.go.

var wizardClaimCreateAttempt = storeCreateAttemptBeadAtomic
var wizardClaimUpdateBead = storeUpdateBead
var wizardClaimIdentity = func(asFlag string) (string, error) { return detectIdentity(asFlag) }

var wizardSealGetActiveAttempt = storeGetActiveAttempt
var wizardSealSetBeadMetadata = store.SetBeadMetadataMap
var wizardSealCloseAttempt = storeCloseAttemptBead
var wizardSealNow = func() time.Time { return time.Now().UTC() }

// wizardSealResolveHead returns the current git HEAD SHA of the working
// directory via pkg/git. Overridable for tests that can't reach a real repo.
var wizardSealResolveHead = func() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve cwd: %w", err)
	}
	rc := &spgit.RepoContext{Dir: wd}
	sha := rc.HeadSHA()
	if sha == "" {
		return "", fmt.Errorf("could not resolve current git HEAD in %s", wd)
	}
	return sha, nil
}

// --- Subcommands ---

var wizardClaimCmd = &cobra.Command{
	Use:   "claim <bead-id>",
	Short: "Atomically create an attempt bead and mark the task in_progress",
	Long: `Atomically claim a task bead for a wizard agent.

Creates a child attempt bead under <bead-id> and flips the task bead's
status to in_progress. If an open attempt bead already exists under the
task, the command fails with a message pointing at the existing attempt.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmdWizardClaim(args[0])
	},
}

var wizardSealCmd = &cobra.Command{
	Use:   "seal <bead-id>",
	Short: "Seal a task bead with its merge commit and close the open attempt",
	Long: `Seal the task bead after its feature branch has merged.

Writes merge_commit + sealed_at fields to the task bead's metadata and
closes the bead's current open attempt. The merge SHA comes from
--merge-commit when set; otherwise the current git HEAD is used.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		mergeCommit, _ := cmd.Flags().GetString("merge-commit")
		return cmdWizardSeal(args[0], mergeCommit)
	},
}

func init() {
	wizardSealCmd.Flags().String("merge-commit", "", "Merge commit SHA (defaults to current git HEAD)")
	wizardCmd.AddCommand(wizardClaimCmd, wizardSealCmd)
	rootCmd.AddCommand(wizardCmd)
}

// cmdWizardClaim is the shared entry point invoked by wizardClaimCmd.RunE.
// Kept as a free function so tests can call it directly without constructing
// a cobra.Command.
func cmdWizardClaim(beadID string) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	agent, err := wizardClaimIdentity("")
	if err != nil {
		return fmt.Errorf("detect identity: %w", err)
	}

	branch := resolveClaimBranch(beadID)

	// CreateAttemptBeadAtomic collapses the active-attempt check and the
	// create into a single call: same-agent reclaim returns the existing
	// attempt ID with nil error; foreign-agent conflict returns an error
	// that names the conflicting attempt and agent. This closes the TOCTOU
	// window the previous two-step path opened.
	attemptID, err := wizardClaimCreateAttempt(beadID, agent, "", branch)
	if err != nil {
		return fmt.Errorf("claim %s: %w", beadID, err)
	}

	if err := wizardClaimUpdateBead(beadID, map[string]interface{}{
		"status": "in_progress",
	}); err != nil {
		return fmt.Errorf("set %s in_progress: %w", beadID, err)
	}

	fmt.Printf("claimed %s (attempt %s)\n", beadID, attemptID)
	return nil
}

// cmdWizardSeal is the shared entry point invoked by wizardSealCmd.RunE.
func cmdWizardSeal(beadID, mergeCommit string) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	attempt, err := wizardSealGetActiveAttempt(beadID)
	if err != nil {
		return fmt.Errorf("check active attempt for %s: %w", beadID, err)
	}
	if attempt == nil {
		return fmt.Errorf("bead %s has no open attempt to seal", beadID)
	}

	if mergeCommit == "" {
		resolved, err := wizardSealResolveHead()
		if err != nil {
			return fmt.Errorf("resolve merge commit: %w", err)
		}
		mergeCommit = resolved
	}

	sealedAt := wizardSealNow().Format(time.RFC3339)
	meta := map[string]string{
		"merge_commit": mergeCommit,
		"sealed_at":    sealedAt,
	}
	if err := wizardSealSetBeadMetadata(beadID, meta); err != nil {
		return fmt.Errorf("write seal metadata on %s: %w", beadID, err)
	}

	if err := wizardSealCloseAttempt(attempt.ID, "success: sealed"); err != nil {
		return fmt.Errorf("close attempt %s: %w", attempt.ID, err)
	}

	fmt.Printf("sealed %s (attempt %s, merge %s)\n", beadID, attempt.ID, mergeCommit)
	return nil
}
