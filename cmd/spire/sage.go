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
verdict is recorded on the current open review-round child bead using the
canonical verdict contract shared with the wizard review loop and steward
routing:

  spire sage accept <bead> [comment]   → review_verdict=approve
  spire sage reject <bead> --feedback  → review_verdict=request_changes

The user-facing verbs remain accept/reject; the CLI translates them to the
canonical approve/request_changes verdict stored on the review-round bead.
Verdict writes funnel through the single review-round store helper, so
steward routing, wizard review re-dispatch, and review history all pick up
sage-CLI verdicts the same way they pick up wizard-driven ones.`,
}

var sageAcceptCmd = &cobra.Command{
	Use:   "accept <bead> [comment]",
	Short: "Record an approve verdict on the current open review round",
	Args:  cobra.RangeArgs(1, 2),
	RunE:  runSageAccept,
}

var sageRejectCmd = &cobra.Command{
	Use:   "reject <bead>",
	Short: "Record a request_changes verdict with feedback on the current open review round",
	Args:  cobra.ExactArgs(1),
	RunE:  runSageReject,
}

// --- Test-replaceable seams (mirror the storeGetBeadFunc pattern) ---

var sageGetChildrenFunc = storeGetChildren
var sageCloseReviewFunc = store.CloseReviewBead
var sageAddLabelFunc = store.AddLabel
var sageAddCommentFunc = store.AddComment

func init() {
	sageRejectCmd.Flags().String("feedback", "", "Feedback text explaining the request_changes verdict (required)")
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

// cmdSageAccept translates the user-facing "accept" verb to the canonical
// review-round verdict "approve" and writes it through CloseReviewBead.
// It also applies the review-approved label on the parent task so the merge
// queue (DetectMergeReady) picks the bead up, matching wizard verdict-only
// approval. No parallel verdict is written to the parent bead — the
// review-round bead is the single authoritative source.
func cmdSageAccept(beadID, comment string) error {
	review, err := findOpenReviewRound(beadID)
	if err != nil {
		return err
	}
	round := reviewRoundNumber(*review)
	summary := comment
	if summary == "" {
		summary = "sage accepted via CLI"
	}
	if err := sageCloseReviewFunc(review.ID, "approve", summary, 0, 0, round, nil); err != nil {
		return fmt.Errorf("close review-round %s: %w", review.ID, err)
	}
	if err := sageAddLabelFunc(beadID, "review-approved"); err != nil {
		return fmt.Errorf("add review-approved label to %s: %w", beadID, err)
	}
	parentComment := "Review approved via sage accept"
	if comment != "" {
		parentComment = fmt.Sprintf("%s — %s", parentComment, comment)
	}
	if err := sageAddCommentFunc(beadID, parentComment); err != nil {
		return fmt.Errorf("add approval comment to %s: %w", beadID, err)
	}
	fmt.Printf("sage accept recorded on %s (review %s closed with verdict=approve)\n", beadID, review.ID)
	return nil
}

// cmdSageReject translates the user-facing "reject" verb to the canonical
// review-round verdict "request_changes" and writes it through
// CloseReviewBead with the feedback as the summary. DetectReviewFeedback
// picks up the request_changes verdict and re-dispatches the apprentice,
// same as when the wizard review loop sets the verdict itself.
func cmdSageReject(beadID, feedback string) error {
	review, err := findOpenReviewRound(beadID)
	if err != nil {
		return err
	}
	round := reviewRoundNumber(*review)
	if err := sageCloseReviewFunc(review.ID, "request_changes", feedback, 0, 0, round, nil); err != nil {
		return fmt.Errorf("close review-round %s: %w", review.ID, err)
	}
	parentComment := fmt.Sprintf("Review round %d: request_changes via sage reject — %s", round, feedback)
	if err := sageAddCommentFunc(beadID, parentComment); err != nil {
		return fmt.Errorf("add request_changes comment to %s: %w", beadID, err)
	}
	fmt.Printf("sage reject recorded on %s (review %s closed with verdict=request_changes)\n", beadID, review.ID)
	return nil
}
