package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/awell-health/spire/pkg/store"
	"github.com/spf13/cobra"
)

// --- Commands ---

var sageCmd = &cobra.Command{
	Use:   "sage",
	Short: "Sage lifecycle commands (review verdicts)",
	Long: `Sage-side commands.

A sage is the agent dispatched to review an apprentice's work. The sage's
verdict on the current open review round is recorded with:

  spire sage accept <bead> [comment]
  spire sage reject <bead> --feedback <text>

Both verdicts write review metadata to the task bead and close the open
review-round child bead for that task.`,
}

var sageAcceptCmd = &cobra.Command{
	Use:   "accept <bead> [comment]",
	Short: "Record an accept verdict on the current open review round",
	Args:  cobra.RangeArgs(1, 2),
	RunE:  runSageAccept,
}

var sageRejectCmd = &cobra.Command{
	Use:   "reject <bead>",
	Short: "Record a reject verdict with feedback on the current open review round",
	Args:  cobra.ExactArgs(1),
	RunE:  runSageReject,
}

// --- Test-replaceable seams (mirror the storeGetBeadFunc pattern) ---

var sageGetChildrenFunc = storeGetChildren
var sageSetMetadataMapFunc = store.SetBeadMetadataMap
var sageCloseAttemptFunc = store.CloseAttemptBead

func init() {
	sageRejectCmd.Flags().String("feedback", "", "Feedback text explaining the reject verdict (required)")
	_ = sageRejectCmd.MarkFlagRequired("feedback")
	sageCmd.AddCommand(sageAcceptCmd, sageRejectCmd)
	rootCmd.AddCommand(sageCmd)
}

func runSageAccept(cmd *cobra.Command, args []string) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}
	beadID := args[0]
	comment := ""
	if len(args) > 1 {
		comment = args[1]
	}
	return cmdSageAccept(beadID, comment)
}

func runSageReject(cmd *cobra.Command, args []string) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}
	beadID := args[0]
	feedback, _ := cmd.Flags().GetString("feedback")
	return cmdSageReject(beadID, feedback)
}

// findOpenReviewRound returns the single open or in-progress review-round
// child of beadID. It returns a clear error when there is no open round or
// more than one — both states would make a verdict ambiguous.
func findOpenReviewRound(beadID string) (*Bead, error) {
	children, err := sageGetChildrenFunc(beadID)
	if err != nil {
		return nil, fmt.Errorf("get children of %s: %w", beadID, err)
	}
	var open []Bead
	for _, c := range children {
		if !isReviewRoundBead(c) {
			continue
		}
		if c.Status != "open" && c.Status != "in_progress" {
			continue
		}
		open = append(open, c)
	}
	switch len(open) {
	case 0:
		return nil, fmt.Errorf("bead %s has no open review-round attempt", beadID)
	case 1:
		return &open[0], nil
	default:
		ids := make([]string, len(open))
		for i, r := range open {
			ids[i] = r.ID
		}
		return nil, fmt.Errorf("bead %s has %d open review-round attempts: %s",
			beadID, len(open), strings.Join(ids, ", "))
	}
}

func cmdSageAccept(beadID, comment string) error {
	review, err := findOpenReviewRound(beadID)
	if err != nil {
		return err
	}
	meta := map[string]string{"review_verdict": "accept"}
	if comment != "" {
		meta["review_comment"] = comment
	}
	if err := sageSetMetadataMapFunc(beadID, meta); err != nil {
		return fmt.Errorf("set verdict metadata on %s: %w", beadID, err)
	}
	if err := sageCloseAttemptFunc(review.ID, "accept"); err != nil {
		return fmt.Errorf("close review-round %s: %w", review.ID, err)
	}
	fmt.Printf("sage accept recorded on %s (review %s closed)\n", beadID, review.ID)
	return nil
}

func cmdSageReject(beadID, feedback string) error {
	review, err := findOpenReviewRound(beadID)
	if err != nil {
		return err
	}
	meta := map[string]string{
		"review_verdict":  "reject",
		"review_feedback": feedback,
	}
	if err := sageSetMetadataMapFunc(beadID, meta); err != nil {
		return fmt.Errorf("set verdict metadata on %s: %w", beadID, err)
	}
	if err := sageCloseAttemptFunc(review.ID, "reject"); err != nil {
		return fmt.Errorf("close review-round %s: %w", review.ID, err)
	}
	fmt.Printf("sage reject recorded on %s (review %s closed)\n", beadID, review.ID)
	return nil
}
