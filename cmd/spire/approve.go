package main

import (
	"fmt"
	"os"

	"github.com/awell-health/spire/pkg/config"
	"github.com/spf13/cobra"
)

// Test-replaceable vars (follow claim.go pattern).
var approveGetBeadFunc = storeGetBead
var approveRemoveLabelFunc = storeRemoveLabel
var approveAddCommentFunc = storeAddComment
var approveIdentityFunc = func() (string, error) { return detectIdentity("") }

var approveCmd = &cobra.Command{
	Use:   "approve <bead-id> [comment]",
	Short: "Approve a human gate and advance the graph",
	Long: `Approve a bead that is waiting for human approval (awaiting-approval label).
Removes the awaiting-approval and needs-human labels, adds an approval comment,
and the steward will re-summon the wizard to advance past the gate.

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

	if !containsLabel(bead, "awaiting-approval") {
		return fmt.Errorf("bead %s is not awaiting approval", beadID)
	}

	// Remove both gate labels.
	if err := approveRemoveLabelFunc(beadID, "awaiting-approval"); err != nil {
		fmt.Fprintf(os.Stderr, "  (note: could not remove awaiting-approval from %s: %s)\n", beadID, err)
	}
	if err := approveRemoveLabelFunc(beadID, "needs-human"); err != nil {
		fmt.Fprintf(os.Stderr, "  (note: could not remove needs-human from %s: %s)\n", beadID, err)
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

	// Reset hooked steps so the steward can re-summon.
	towerName := ""
	if tc, err := config.ActiveTowerConfig(); err == nil && tc != nil {
		towerName = tc.Name
	}
	resolveResetHookedSteps(beadID, towerName)

	fmt.Printf("%sApproved %s%s\n", bold, beadID, reset)
	if comment != "" {
		fmt.Printf("  comment: %q\n", comment)
	}

	return nil
}
