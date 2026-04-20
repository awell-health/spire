package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/awell-health/spire/pkg/store"
	"github.com/spf13/cobra"
)

var arbiterCmd = &cobra.Command{
	Use:   "arbiter",
	Short: "Arbiter lifecycle commands (dispute resolution)",
	Long: `Arbiter-side commands.

An arbiter resolves disputes between sages and apprentices by issuing a
binding verdict. "spire arbiter decide" records that verdict on the most
recent review-round child of the task — the same storage boundary the
sage writes to — with a source="arbiter" marker, then closes the round.
Once recorded the verdict is binding: subsequent sage verdicts on that
same review round are rejected by the sage CLI and downstream readers
prefer the arbiter verdict.`,
}

var arbiterDecideCmd = &cobra.Command{
	Use:   "decide <bead>",
	Short: "Record a binding arbiter verdict on a bead",
	Long: `Record a binding arbiter verdict on <bead>.

The verdict is written to the most recent review-round child of <bead>
under the "arbiter_verdict" metadata key as a JSON payload that carries
source="arbiter". The matching plain "review_verdict" key is mirrored on
the same review-round so existing readers see the arbiter's call. If the
review-round is still open, it is closed with the arbiter's verdict.

If the task has an active attempt bead (the current dispute round), the
attempt is closed with result "arbiter-resolved". Downstream sage verdict
writers check the arbiter marker and refuse to overwrite it.`,
	Args: cobra.ExactArgs(1),
	RunE: runArbiterDecide,
}

func init() {
	arbiterDecideCmd.Flags().String("verdict", "", "Arbiter verdict: accept | reject | custom")
	arbiterDecideCmd.Flags().String("note", "", "Optional verdict reasoning/note")
	_ = arbiterDecideCmd.MarkFlagRequired("verdict")
	arbiterCmd.AddCommand(arbiterDecideCmd)
	rootCmd.AddCommand(arbiterCmd)
}

// --- Test-replaceable seams ---

var arbiterGetBeadFunc = storeGetBead
var arbiterSetMetadataMapFunc = store.SetBeadMetadataMap
var arbiterAddCommentFunc = storeAddComment
var arbiterGetActiveAttemptFunc = storeGetActiveAttempt
var arbiterCloseAttemptBeadFunc = storeCloseAttemptBead
var arbiterMostRecentReviewFunc = store.MostRecentReviewRound
var arbiterCloseBeadFunc = store.CloseBead
var arbiterNowFunc = func() time.Time { return time.Now().UTC() }

// arbiterVerdictPayload is the JSON structure written to the review-round
// bead under the "arbiter_verdict" metadata key. The Source field is the
// load-bearing marker that distinguishes arbiter verdicts from sage
// verdicts; readers treat an arbiter-source verdict as binding and the sage
// CLI refuses to overwrite it.
type arbiterVerdictPayload struct {
	Source    string `json:"source"`
	Verdict   string `json:"verdict"`
	Note      string `json:"note,omitempty"`
	DecidedAt string `json:"decided_at"`
}

const (
	arbiterVerdictMetaKey = "arbiter_verdict"
	arbiterVerdictSource  = "arbiter"
	arbiterAttemptResult  = "arbiter-resolved"
)

var allowedArbiterVerdicts = map[string]bool{
	"accept": true,
	"reject": true,
	"custom": true,
}

func runArbiterDecide(cmd *cobra.Command, args []string) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	beadID := args[0]
	verdict, _ := cmd.Flags().GetString("verdict")
	note, _ := cmd.Flags().GetString("note")

	if !allowedArbiterVerdicts[verdict] {
		return fmt.Errorf("invalid verdict %q: must be one of accept, reject, custom", verdict)
	}

	if _, err := arbiterGetBeadFunc(beadID); err != nil {
		return fmt.Errorf("get bead %s: %w", beadID, err)
	}

	// Locate the review-round the arbiter is deciding. The arbiter only
	// settles disputes that began with a sage review, so an absent round is
	// a hard error — there is no review to bind.
	review, err := arbiterMostRecentReviewFunc(beadID)
	if err != nil {
		return fmt.Errorf("look up review-round for %s: %w", beadID, err)
	}
	if review == nil {
		return fmt.Errorf("bead %s has no review-round to decide; arbiter only resolves sage-initiated review rounds", beadID)
	}

	payload := arbiterVerdictPayload{
		Source:    arbiterVerdictSource,
		Verdict:   verdict,
		Note:      note,
		DecidedAt: arbiterNowFunc().Format(time.RFC3339),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal arbiter verdict: %w", err)
	}
	// Write the binding verdict on the review-round bead — the same storage
	// boundary sage writes to. The arbiter_verdict JSON carries the source
	// marker; the mirrored review_verdict keeps existing readers compatible
	// without parsing the JSON payload.
	meta := map[string]string{
		arbiterVerdictMetaKey: string(raw),
		"review_verdict":      verdict,
	}
	if err := arbiterSetMetadataMapFunc(review.ID, meta); err != nil {
		return fmt.Errorf("write arbiter verdict metadata on review-round %s: %w", review.ID, err)
	}

	// Close the review-round if it is still open. A round that was already
	// closed (e.g. by a sage racing the arbiter) stays closed — the arbiter
	// verdict on the same bead now overrides whatever the sage wrote.
	if review.Status == "open" || review.Status == "in_progress" {
		if cerr := arbiterCloseBeadFunc(review.ID); cerr != nil {
			fmt.Fprintf(os.Stderr, "  (note: could not close review-round %s: %s)\n", review.ID, cerr)
		}
	}

	// Close the current dispute round attempt if one is open. A failure to
	// close is surfaced to stderr but does not unwind the verdict write —
	// the verdict is already durable and binding.
	if attempt, err := arbiterGetActiveAttemptFunc(beadID); err == nil && attempt != nil {
		if cerr := arbiterCloseAttemptBeadFunc(attempt.ID, arbiterAttemptResult); cerr != nil {
			fmt.Fprintf(os.Stderr, "  (note: could not close active attempt %s: %s)\n", attempt.ID, cerr)
		}
	}

	summary := fmt.Sprintf("arbiter verdict: %s (review-round %s)", verdict, review.ID)
	if note != "" {
		summary += " — " + note
	}
	if err := arbiterAddCommentFunc(beadID, summary); err != nil {
		fmt.Fprintf(os.Stderr, "  (note: could not add arbiter comment to %s: %s)\n", beadID, err)
	}

	fmt.Printf("arbiter verdict %q recorded on %s (review-round %s)\n", verdict, beadID, review.ID)
	return nil
}
