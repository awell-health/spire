package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Test-replaceable vars (follow claim.go pattern).
var approveGetBeadFunc = storeGetBead
var approveGetStepBeadsFunc = storeGetStepBeads
var approveUnhookStepBeadFunc = storeUnhookStepBead
var approveUpdateBeadFunc = storeUpdateBead
var approveAddCommentFunc = storeAddComment
var approveIdentityFunc = func() (string, error) { return detectIdentity("") }
var approveSummonFunc = func(beadID string) error {
	return cmdSummon([]string{"1", "--targets", beadID})
}

var approveCmd = &cobra.Command{
	Use:   "approve <bead-id> [comment]",
	Short: "Approve a hooked approval step and resume the wizard",
	Long: `Approve a bead that has a hooked approval step (status=hooked on a step bead).
Unhooks the approval step, adds an approval comment, and re-summons the wizard
to advance past the gate.

Unlike 'spire resolve', this is for beads where the agent succeeded and the human
gives the go-ahead — no recovery learning is recorded.`,
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		comment := ""
		if len(args) > 1 {
			comment = args[1]
		}
		return cmdApprove(args[0], comment)
	},
}

func cmdApprove(beadID, comment string) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	bead, err := approveGetBeadFunc(beadID)
	if err != nil {
		return fmt.Errorf("get bead %s: %w", beadID, err)
	}

	if bead.Status == "closed" {
		return fmt.Errorf("bead %s is already closed", beadID)
	}

	// Find hooked step beads under this parent.
	stepBeads, err := approveGetStepBeadsFunc(beadID)
	if err != nil {
		return fmt.Errorf("get step beads for %s: %w", beadID, err)
	}

	// Find the hooked approval step: prefer step:human.approve, fall back to any hooked step.
	var approvalStep *Bead
	var firstHooked *Bead
	for i, sb := range stepBeads {
		if sb.Status != "hooked" {
			continue
		}
		if firstHooked == nil {
			firstHooked = &stepBeads[i]
		}
		if containsLabel(sb, "step:human.approve") {
			approvalStep = &stepBeads[i]
			break
		}
	}
	if approvalStep == nil {
		approvalStep = firstHooked
	}
	if approvalStep == nil {
		return fmt.Errorf("bead %s has no hooked approval step", beadID)
	}

	// Unhook the approval step (sets it back to 'open').
	if err := approveUnhookStepBeadFunc(approvalStep.ID); err != nil {
		return fmt.Errorf("unhook approval step %s: %w", approvalStep.ID, err)
	}

	// Resolve identity and add approval comment.
	identity, _ := approveIdentityFunc()
	if identity == "" {
		identity = "human"
	}
	approvalMsg := fmt.Sprintf("Approved by %s", identity)
	if comment != "" {
		approvalMsg += ": " + comment
	}
	if err := approveAddCommentFunc(beadID, approvalMsg); err != nil {
		fmt.Fprintf(os.Stderr, "  (note: could not add approval comment to %s: %s)\n", beadID, err)
	}

	// Check if any other step beads are still hooked; if not, set parent to in_progress.
	anyHooked := false
	for _, sb := range stepBeads {
		if sb.ID == approvalStep.ID {
			continue
		}
		if sb.Status == "hooked" {
			anyHooked = true
			break
		}
	}
	if !anyHooked {
		if err := approveUpdateBeadFunc(beadID, map[string]interface{}{"status": "in_progress"}); err != nil {
			fmt.Fprintf(os.Stderr, "  (note: could not set %s to in_progress: %s)\n", beadID, err)
		}
	}

	fmt.Printf("%sApproved %s%s\n", bold, beadID, reset)
	if comment != "" {
		fmt.Printf("  comment: %q\n", comment)
	}

	// Re-summon wizard to pick up the approved step.
	fmt.Printf("  %s↑ re-summoning wizard for %s%s\n", cyan, beadID, reset)
	if err := approveSummonFunc(beadID); err != nil {
		return fmt.Errorf("re-summon %s: %w", beadID, err)
	}

	return nil
}
